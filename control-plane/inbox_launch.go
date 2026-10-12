package main

// Launching a session from a request (ADR 0111 decision 2, docs/log/130 R-6..R-8).
//
// Only the current assignee may launch from a request, and the CP checks that before the
// session exists and before it even resolves the caller's workspace: a refused launch
// creates neither a session nor a workspace record. The launch itself is the ordinary
// session create (workspaceAPI.sessionCreate) with the Console's usual body, so quotas,
// auto-start and the Agent's own checks apply unchanged. The CP then records the session,
// working copy and branch on the request; agents never report links.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/k-k1/agent-fleet/control-plane/internal/store"
)

// inboxLaunchWindow is how long one launch holds the request against a second one. It
// covers a cold workspace start, which the session create waits for.
const inboxLaunchWindow = 10 * time.Minute

// inboxLaunchMaxBody bounds the session-create body relayed from the Console.
const inboxLaunchMaxBody = 256 << 10

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

// launch (POST /api/inbox/items/{id}/launch). The body is what the Console would send to
// POST /api/sessions.
func (a inboxAPI) launch(w http.ResponseWriter, r *http.Request, ident store.Identity, mv store.MembershipView) {
	ctx := r.Context()
	id := r.PathValue("id")
	raw, err := io.ReadAll(io.LimitReader(r.Body, inboxLaunchMaxBody+1))
	var probe map[string]any
	if err != nil || len(raw) > inboxLaunchMaxBody || json.Unmarshal(raw, &probe) != nil {
		writeAPIErr(w, &apiError{http.StatusBadRequest, errCodeInboxBadRequest, "invalid session request"})
		return
	}
	a.reconcile(ctx, mv.TenantID)
	it, _, ok := a.participant(w, ctx, mv, id)
	if !ok {
		return
	}
	now := time.Now().UTC()
	began, err := a.mgr.store.BeginInboxLaunch(ctx, id, mv.MembershipID, now.Format(time.RFC3339), now.Add(inboxLaunchWindow).Format(time.RFC3339))
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	if !began {
		cur, _, _ := a.mgr.store.GetInboxItem(ctx, id)
		if cur.State == "claimed" && cur.AssigneeMembershipID == mv.MembershipID {
			writeInboxErr(w, http.StatusConflict, errCodeInboxLaunchBusy, "a session is already being started or was started from this request",
				inboxErrWire{Session: cur.SessionName})
			return
		}
		writeInboxErr(w, http.StatusConflict, errCodeInboxStateConflict, "only the current assignee can launch from this request",
			a.inboxConflict(ctx, mv, cur))
		return
	}
	created := false
	defer func() {
		if !created {
			_ = a.mgr.store.AbortInboxLaunch(context.WithoutCancel(ctx), id, mv.MembershipID)
		}
	}()
	key := a.mgr.resolveIdentity(r)
	res, aerr := a.mgr.resolveFull(ctx, key.key, key.email, tenantSel(r))
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	if res.mv.MembershipID != mv.MembershipID {
		writeAPIErr(w, &apiError{http.StatusConflict, "tenant_selection_required", "the tenant changed during the request"})
		return
	}
	tctx, untrack := a.mgr.memberConns.track(ctx, mv.MembershipID)
	defer untrack()
	sub := r.Clone(tctx)
	sub.URL.Path, sub.URL.RawPath, sub.URL.RawQuery = "/api/sessions", "", ""
	sub.RequestURI = "/api/sessions"
	sub.Body = io.NopCloser(bytes.NewReader(raw))
	sub.ContentLength = int64(len(raw))
	sub.Header.Set("Content-Type", "application/json")
	cw := &captureWriter{h: http.Header{}}
	a.ws.sessionCreate(cw, sub, res)
	if cw.status >= 200 && cw.status < 300 {
		var s struct {
			Name   string `json:"name"`
			Dir    string `json:"dir"`
			Branch string `json:"branch"`
		}
		_ = json.Unmarshal(cw.buf.Bytes(), &s)
		if s.Name != "" {
			branch := s.Branch
			if nb, _ := probe["new_branch"].(string); branch == "" && strings.TrimSpace(nb) != "" {
				branch = strings.TrimSpace(nb)
			}
			if err := a.mgr.store.FinishInboxLaunch(context.WithoutCancel(ctx), id, mv.MembershipID, s.Name, s.Dir, branch, store.NowTS()); err == nil {
				created = true
			}
			_ = a.mgr.store.InsertAudit(context.WithoutCancel(ctx), store.AuditLog{ID: store.NewID(), TenantID: mv.TenantID,
				ActorKind: "user", ActorID: ident.ID, Action: "inbox.launch", Target: it.ID, Detail: "session=" + s.Name,
				HTTPStatus: cw.status, At: store.NowTS()})
		}
	}
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
