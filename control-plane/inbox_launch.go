package main

// Launching a session from a request (ADR 0111 decision 2, docs/log/130 R-6..R-8).
//
// Only the current assignee may launch from a request. The launch is recorded on the item in
// two durable steps:
//
//  1. reserved, before anything else: a non-assignee or a second press is refused here,
//     before the caller's workspace is even resolved, so a refused launch creates neither a
//     session nor a workspace record;
//  2. creating, by a conditional update run after the workspace is ready and the quota has
//     passed, immediately before the Agent is asked (workspaceAPI.sessionCreateThen). This is
//     the commit point: a release or a withdraw that lands first wins and the Agent is never
//     asked, one that lands after it orders after the launch.
//
// The Agent is asked with an idempotency key bound to the item and the reservation, so the
// CP can later ask the Agent what became of that one create (GET /sessions-idempotency/{key})
// instead of creating again. A create whose outcome is unknown (no answer, a 5xx, the CP
// stopped, the record could not be written) is never issued again under a new key: the
// assignee's next press looks the key up and records what it finds, and when the Agent no
// longer knows the key the press answers inbox_launch_unknown. Handing the request back is the
// way out of that, decided by the person who can see their own sessions.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/k-k1/agent-fleet/control-plane/internal/store"
)

// inboxLaunchWindow is how long a reservation holds the request before a later press may
// take it over. It covers a cold workspace start, which the session create waits for.
const inboxLaunchWindow = 10 * time.Minute

// inboxLaunchMaxBody bounds the session-create body relayed from the Console.
const inboxLaunchMaxBody = 256 << 10

// inboxLaunchRecordTries is how often a created session's record is attempted before the
// launch is left creating for the next press to reconcile.
const inboxLaunchRecordTries = 3

// captureWriter buffers a handler's response so the launch can read the created session
// before answering.
type captureWriter struct {
	h      http.Header
	status int
	buf    bytes.Buffer
}

func (c *captureWriter) Header() http.Header { return c.h }
func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.buf.Write(b)
}
func (c *captureWriter) WriteHeader(s int) {
	if c.status == 0 {
		c.status = s
	}
}

// inboxLaunchKey is the Agent idempotency key of one reserved launch.
func inboxLaunchKey(itemID, token string) string { return "inbox-" + itemID + "-" + token }

type inboxCreatedSession struct {
	Name   string `json:"name"`
	Dir    string `json:"dir"`
	Branch string `json:"branch"`
}

// launch (POST /api/inbox/items/{id}/launch). The body is what the Console would send to
// POST /api/sessions.
func (a inboxAPI) launch(w http.ResponseWriter, r *http.Request, ident store.Identity, mv store.MembershipView) {
	ctx := r.Context()
	id := r.PathValue("id")
	raw, err := io.ReadAll(io.LimitReader(r.Body, inboxLaunchMaxBody+1))
	var body map[string]any
	if err != nil || len(raw) > inboxLaunchMaxBody || json.Unmarshal(raw, &body) != nil || body == nil {
		writeAPIErr(w, &apiError{http.StatusBadRequest, errCodeInboxBadRequest, "invalid session request"})
		return
	}
	a.reconcile(ctx, mv.TenantID)
	it, _, ok := a.participant(w, ctx, mv, id)
	if !ok {
		return
	}
	token := store.NewID()
	now := time.Now().UTC()
	began, err := a.mgr.store.BeginInboxLaunch(ctx, id, mv.MembershipID, token, now.Format(time.RFC3339), now.Add(inboxLaunchWindow).Format(time.RFC3339))
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	if !began {
		cur, _, _ := a.mgr.store.GetInboxItem(ctx, id)
		switch {
		case cur.State == "claimed" && cur.AssigneeMembershipID == mv.MembershipID && cur.LaunchState == "creating" && cur.LaunchBy == mv.MembershipID:
			a.reconcileLaunch(w, r, mv, cur)
		case cur.State == "claimed" && cur.AssigneeMembershipID == mv.MembershipID:
			writeInboxErr(w, http.StatusConflict, errCodeInboxLaunchBusy, "a session is already being started or was started from this request",
				inboxErrWire{Session: cur.SessionName})
		default:
			writeInboxErr(w, http.StatusConflict, errCodeInboxStateConflict, "only the current assignee can launch from this request",
				a.inboxConflict(ctx, mv, cur))
		}
		return
	}
	// Until the confirm below, nothing has been asked of the Agent, so every way out before it
	// gives the reservation back. After it, only a known refusal does.
	confirmed := false
	abort := func() { _ = a.mgr.store.AbortInboxLaunch(context.WithoutCancel(ctx), id, token) }
	key := a.mgr.resolveIdentity(r)
	res, aerr := a.mgr.resolveFull(ctx, key.key, key.email, tenantSel(r))
	if aerr != nil {
		abort()
		writeAPIErr(w, aerr)
		return
	}
	if res.mv.MembershipID != mv.MembershipID {
		abort()
		writeAPIErr(w, &apiError{http.StatusConflict, "tenant_selection_required", "the tenant changed during the request"})
		return
	}
	tctx, untrack := a.mgr.memberConns.track(ctx, mv.MembershipID)
	defer untrack()
	body["idempotency_key"] = inboxLaunchKey(id, token)
	raw, _ = json.Marshal(body)
	// The session carries its provenance badge (decision 3): the request and its sender, set
	// here by the CP, never taken from the body.
	origin := inboxOriginRef{Item: id}
	if v, _, err := a.view(ctx, mv); err == nil {
		origin.Sender = v.person(it.SenderMembershipID).UserKey
	}
	sub := r.Clone(context.WithValue(tctx, inboxOriginKey{}, origin))
	sub.URL.Path, sub.URL.RawPath, sub.URL.RawQuery = "/api/sessions", "", ""
	sub.RequestURI = "/api/sessions"
	sub.Body = io.NopCloser(bytes.NewReader(raw))
	sub.ContentLength = int64(len(raw))
	sub.Header.Set("Content-Type", "application/json")
	lost := false
	cw := &captureWriter{h: http.Header{}}
	a.ws.sessionCreateThen(cw, sub, res, func(ctx context.Context) *apiError {
		ok, err := a.mgr.store.ConfirmInboxLaunch(ctx, id, mv.MembershipID, token)
		if err != nil {
			return internalErr(err)
		}
		if !ok {
			lost = true
			return &apiError{http.StatusConflict, errCodeInboxStateConflict, "the request changed before the session was created"}
		}
		confirmed = true
		return nil
	})
	if !confirmed {
		abort()
		if lost {
			cur, _, _ := a.mgr.store.GetInboxItem(ctx, id)
			writeInboxErr(w, http.StatusConflict, errCodeInboxStateConflict, "the request changed before the session was created",
				a.inboxConflict(ctx, mv, cur))
			return
		}
		forwardCaptured(w, cw)
		return
	}
	switch {
	case cw.status >= 200 && cw.status < 300:
		var s inboxCreatedSession
		_ = json.Unmarshal(cw.buf.Bytes(), &s)
		if nb, _ := body["new_branch"].(string); s.Branch == "" && strings.TrimSpace(nb) != "" {
			s.Branch = strings.TrimSpace(nb)
		}
		recorded := a.recordLaunch(ctx, id, mv.MembershipID, token, s)
		detail := "session=" + s.Name
		if !recorded {
			// The session exists, so the answer stays the Agent's 201. The launch stays
			// creating: the next press looks the key up and records it.
			detail += " recorded=false"
		}
		_ = a.mgr.store.InsertAudit(context.WithoutCancel(ctx), store.AuditLog{ID: store.NewID(), TenantID: mv.TenantID,
			ActorKind: "user", ActorID: ident.ID, Action: "inbox.launch", Target: it.ID, Detail: detail,
			HTTPStatus: cw.status, At: store.NowTS()})
	case cw.status >= 400 && cw.status < 500 && !strings.Contains(cw.buf.String(), `"create_in_progress"`):
		// The Agent refused the create, so no session came of it.
		abort()
	default:
		// No answer, a 5xx, or the Agent still creating: the outcome is unknown and the
		// launch stays creating.
		log.Printf("inbox: launch %s left creating after status %d", id, cw.status)
	}
	forwardCaptured(w, cw)
}

// recordLaunch writes the created session onto the request, trying a few times: a session
// that exists but is not on the card would let the assignee start a second one.
func (a inboxAPI) recordLaunch(ctx context.Context, id, mid, token string, s inboxCreatedSession) bool {
	ctx = context.WithoutCancel(ctx)
	if s.Name == "" {
		log.Printf("inbox: launch %s: the Agent answered without a session name", id)
		return false
	}
	for i := 0; i < inboxLaunchRecordTries; i++ {
		ok, err := a.mgr.store.FinishInboxLaunch(ctx, id, mid, token, s.Name, s.Dir, s.Branch, store.NowTS())
		if err == nil {
			if !ok {
				log.Printf("inbox: launch %s created %s after the request was handed back; not recorded", id, s.Name)
			}
			return ok
		}
		log.Printf("inbox: launch %s: recording session %s failed (try %d): %v", id, s.Name, i+1, err)
		time.Sleep(time.Duration(100*(i+1)) * time.Millisecond)
	}
	return false
}

// reconcileLaunch answers a press while this assignee's launch is creating: it asks the Agent
// what became of that create and records a session it finds. It never creates.
func (a inboxAPI) reconcileLaunch(w http.ResponseWriter, r *http.Request, mv store.MembershipView, cur store.InboxItem) {
	ctx := r.Context()
	unknown := func() {
		writeInboxErr(w, http.StatusConflict, errCodeInboxLaunchUnknown,
			"a session may already have been started from this request; check your sessions, or hand the request back to start over",
			inboxErrWire{State: cur.State})
	}
	key := a.mgr.resolveIdentity(r)
	res, aerr := a.mgr.resolveFull(ctx, key.key, key.email, tenantSel(r))
	if aerr != nil || res.mv.MembershipID != mv.MembershipID || res.rt.State(ctx) != "running" {
		unknown()
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		res.rt.Endpoint()+"/sessions-idempotency/"+url.PathEscape(inboxLaunchKey(cur.ID, cur.LaunchToken)), nil)
	if err != nil {
		unknown()
		return
	}
	if res.rt.Token() != "" {
		req.Header.Set("Authorization", "Bearer "+res.rt.Token())
	}
	resp, err := agentRelayClient.Do(req)
	if err != nil {
		unknown()
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
		var s inboxCreatedSession
		_ = json.Unmarshal(b, &s)
		if !a.recordLaunch(ctx, cur.ID, mv.MembershipID, cur.LaunchToken, s) {
			unknown()
			return
		}
		writeInboxErr(w, http.StatusConflict, errCodeInboxLaunchBusy, "a session was already started from this request",
			inboxErrWire{Session: s.Name})
	case http.StatusAccepted:
		writeInboxErr(w, http.StatusConflict, errCodeInboxLaunchBusy, "a session is being started from this request", inboxErrWire{})
	default:
		unknown()
	}
}

func forwardCaptured(w http.ResponseWriter, cw *captureWriter) {
	for k, vs := range cw.h {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if cw.status == 0 {
		cw.status = http.StatusOK
	}
	w.WriteHeader(cw.status)
	_, _ = w.Write(cw.buf.Bytes())
}
