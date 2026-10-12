package main

// The member request inbox (ADR 0111 decisions 2, 3 and 8-11, P1-A first half).
//
// People send each other DMs and requests through the CP. Nothing here reaches an agent or a
// workspace: every route is withMembership, so using the inbox never creates a workspace
// record or starts one (decision 8), and nothing is exposed under /internal/ or as an agent
// tool (decision 11: an agent drafts through add_memo, a person sends). The one route that
// does touch a workspace, launching a session from a claimed request, checks the claim
// before it resolves the workspace (inbox_launch.go).
//
// A non-participant gets 404 for an item, whether it exists or not and whichever tenant it
// is in. Bodies are sealed with the tenant key custodian when there is one (decision 9) and
// never written to the audit log. There is no admin read API (addendum 1).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/k-k1/agent-fleet/control-plane/internal/store"
)

// Limits and retention from the 2026-10-10 addendum (items 2, 5 and 6). They are fixed, not
// tenant settings: a configurable period widens what the purge has to be tested against.
const (
	inboxMaxRecipients  = 20
	inboxSendsPerHour   = 30
	inboxPendingPerPair = 10
	// inboxBodyMaxBytes is the body cap, in bytes of UTF-8. A request is prose for a person,
	// not a transcript; 16 KiB is several pages.
	inboxBodyMaxBytes   = 16 << 10
	inboxDMRetention    = 90 * 24 * time.Hour
	inboxFinalRetention = 90 * 24 * time.Hour
	inboxLongOpenAfter  = 14 * 24 * time.Hour
	inboxPRFieldMax     = 2048
)

type inboxAPI struct {
	memberAuth
	ws workspaceAPI
	// tick and ping pace the stream. Fields so a test can run the stream fast.
	tick, ping time.Duration
}

func newInboxAPI(m *manager, autostart bool) inboxAPI {
	return inboxAPI{memberAuth{m}, newWorkspaceAPI(m, autostart), inboxStreamTick, inboxStreamPing}
}

// registerInboxRoutes registers every inbox route. All of them are withMembership; the test
// that walks this list (inbox_routes_test.go) fails if one is registered any other way that
// creates a workspace record.
func registerInboxRoutes(mux *http.ServeMux, cfg config) {
	a := newInboxAPI(cfg.mgr, cfg.autostart)
	mux.HandleFunc("GET /api/inbox", a.withMembership(a.list))
	mux.HandleFunc("POST /api/inbox/items", a.withMembership(a.send))
	mux.HandleFunc("GET /api/inbox/items/{id}", a.withMembership(a.detail))
	mux.HandleFunc("POST /api/inbox/items/{id}/read", a.withMembership(a.read))
	mux.HandleFunc("POST /api/inbox/items/{id}/claim", a.withMembership(a.claim))
	mux.HandleFunc("POST /api/inbox/items/{id}/decline", a.withMembership(a.decline))
	mux.HandleFunc("POST /api/inbox/items/{id}/release", a.withMembership(a.release))
	mux.HandleFunc("POST /api/inbox/items/{id}/done", a.withMembership(a.done))
	mux.HandleFunc("POST /api/inbox/items/{id}/withdraw", a.withMembership(a.withdraw))
	mux.HandleFunc("PUT /api/inbox/items/{id}/pr", a.withMembership(a.setPR))
	// Launch resolves the workspace itself, and only after the CP has checked the claim.
	mux.HandleFunc("POST /api/inbox/items/{id}/launch", a.withMembership(a.launch))
	mux.HandleFunc("POST /api/inbox/cursor", a.withMembership(a.cursor))
	mux.HandleFunc("GET /api/inbox/settings", a.withMembership(a.settings))
	mux.HandleFunc("PUT /api/inbox/settings", a.withMembership(a.putSettings))
	mux.HandleFunc("PUT /api/inbox/blocks/{userKey}", a.withMembership(a.block))
	mux.HandleFunc("DELETE /api/inbox/blocks/{userKey}", a.withMembership(a.unblock))
	mux.HandleFunc("GET /api/inbox/stream", a.withMembership(a.stream))
}

func inboxRetentionAt(now time.Time) store.InboxRetention {
	return store.InboxRetention{
		DMBefore:    now.Add(-inboxDMRetention).Format(time.RFC3339),
		FinalBefore: now.Add(-inboxFinalRetention).Format(time.RFC3339),
	}
}

// inboxErrWire is an inbox error: code and message, plus what the screen needs to say why.
// Every extra key is omitted when empty, so a plain refusal is the usual {code, message}.
type inboxErrWire struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// State and Assignee: the item as it is now, on a conflict.
	State    string           `json:"state,omitempty"`
	Assignee *inboxPersonWire `json:"assignee,omitempty"`
	// Recipients: who refused, or whose pair limit is reached.
	Recipients []string `json:"recipients,omitempty"`
	// Session: the session already started from this request.
	Session string `json:"session,omitempty"`
	// The limit that was hit.
	MaxBytes int `json:"maxBytes,omitempty"`
	Max      int `json:"max,omitempty"`
	PerHour  int `json:"perHour,omitempty"`
	PerPair  int `json:"perPair,omitempty"`
}

type inboxErrEnvelope struct {
	Error inboxErrWire `json:"error"`
}

// writeInboxErr writes an error with the extra keys set in e beside code and message.
func writeInboxErr(w http.ResponseWriter, status int, code, message string, e inboxErrWire) {
	e.Code, e.Message = code, message
	writeJSON(w, status, inboxErrEnvelope{Error: e})
}

// inboxConflict describes the item as it is now, for a 409.
func (a inboxAPI) inboxConflict(ctx context.Context, mv store.MembershipView, cur store.InboxItem) inboxErrWire {
	e := inboxErrWire{State: cur.State}
	if cur.AssigneeMembershipID != "" {
		if v, _, err := a.view(ctx, mv); err == nil {
			p := v.person(cur.AssigneeMembershipID)
			e.Assignee = &p
		}
	}
	return e
}

func inboxNotFound(w http.ResponseWriter) {
	writeAPIErr(w, &apiError{http.StatusNotFound, errCodeInboxNotFound, "inbox item not found"})
}

func (a inboxAPI) seal(ctx context.Context, tenant string, body []byte) (string, string, error) {
	if a.mgr.custodian == nil {
		return base64.StdEncoding.EncodeToString(body), "", nil
	}
	ct, err := a.mgr.custodian.Wrap(ctx, tenant, body)
	return ct, tenant, err
}

// open returns "" for a body it cannot open: one unreadable row must not fail the list.
func (a inboxAPI) open(ctx context.Context, it store.InboxItem) string {
	if it.Ciphertext == "" {
		return ""
	}
	if it.KeyRef == "" {
		b, err := base64.StdEncoding.DecodeString(it.Ciphertext)
		if err != nil {
			return ""
		}
		return string(b)
	}
	if a.mgr.custodian == nil {
		return ""
	}
	b, err := a.mgr.custodian.Unwrap(ctx, it.KeyRef, it.Ciphertext)
	if err != nil {
		return ""
	}
	return string(b)
}

// reconcile applies the system rows of the state table (removal, block) to the caller's
// tenant before a read or an action, so what the caller sees and acts on is current. The
// sweep does the same for tenants nobody is reading.
func (a inboxAPI) reconcile(ctx context.Context, tenantID string) {
	if _, err := a.mgr.store.ReconcileInbox(ctx, tenantID, store.NowTS()); err != nil {
		log.Printf("inbox: reconcile %s: %v", tenantID, err)
	}
}

// inboxPersonWire names a member. Removed is true for a membership that is no longer active
// in the tenant, whose key is then not shown.
type inboxPersonWire struct {
	UserKey string `json:"userKey"`
	Email   string `json:"email"`
	Removed bool   `json:"removed"`
}

// inboxRecipientWire is one recipient. Delivery and Eligible are filled for the sender only:
// a recipient sees who else was asked, not what each of them did.
type inboxRecipientWire struct {
	inboxPersonWire
	Delivery string `json:"delivery"`
	Eligible bool   `json:"eligible"`
}

type inboxSessionWire struct {
	Name       string          `json:"name"`
	Dir        string          `json:"dir"`
	Branch     string          `json:"branch"`
	LaunchedAt string          `json:"launchedAt"`
	By         inboxPersonWire `json:"by"`
}

type inboxPRWire struct {
	URL    string `json:"url"`
	Branch string `json:"branch"`
}

// inboxItemWire is an item as one participant sees it.
type inboxItemWire struct {
	ID         string               `json:"id"`
	Kind       string               `json:"kind"`
	State      string               `json:"state"`
	Role       string               `json:"role"`
	Delivery   string               `json:"delivery"`
	Sender     inboxPersonWire      `json:"sender"`
	Recipients []inboxRecipientWire `json:"recipients"`
	Assignee   *inboxPersonWire     `json:"assignee"`
	// AssignedToMe: the caller is the assignee. The Console does not know its own
	// membership, so it cannot tell from Assignee alone.
	AssignedToMe bool   `json:"assignedToMe"`
	Body         string `json:"body"`
	// CanClaim: the caller is a recipient who can take this open request now.
	CanClaim bool `json:"canClaim"`
	// LongOpen: a request still open or claimed 14 days after it was sent (decision 9).
	LongOpen  bool              `json:"longOpen"`
	Unseen    bool              `json:"unseen"`
	Seq       int64             `json:"seq"`
	CreatedAt string            `json:"createdAt"`
	UpdatedAt string            `json:"updatedAt"`
	FinalAt   string            `json:"finalAt"`
	Session   *inboxSessionWire `json:"session"`
	PR        *inboxPRWire      `json:"pr"`
}

type inboxMailboxWire struct {
	LastSeq    int64  `json:"lastSeq"`
	ReadSeq    int64  `json:"readSeq"`
	Unseen     int    `json:"unseen"`
	DND        bool   `json:"dnd"`
	AcceptFrom string `json:"acceptFrom"`
}

// inboxView renders items for one member. It holds the tenant roster for the request so
// names are looked up once.
type inboxView struct {
	a      inboxAPI
	me     string
	byID   map[string]store.MemberInfo
	byKey  map[string]store.MemberInfo
	cursor int64
	now    time.Time
}

func (a inboxAPI) view(ctx context.Context, mv store.MembershipView) (inboxView, store.InboxMailbox, error) {
	rows, err := a.mgr.store.ListMembersByTenant(ctx, mv.TenantID)
	if err != nil {
		return inboxView{}, store.InboxMailbox{}, err
	}
	v := inboxView{a: a, me: mv.MembershipID, byID: map[string]store.MemberInfo{}, byKey: map[string]store.MemberInfo{}, now: time.Now().UTC()}
	for _, m := range rows {
		v.byID[m.MembershipID] = m
		v.byKey[m.UserKey] = m
	}
	mb, err := a.mgr.store.GetInboxMailbox(ctx, mv.MembershipID)
	if err != nil {
		return inboxView{}, store.InboxMailbox{}, err
	}
	v.cursor = mb.ReadSeq
	return v, mb, nil
}

func (v inboxView) person(mid string) inboxPersonWire {
	if m, ok := v.byID[mid]; ok {
		return inboxPersonWire{UserKey: m.UserKey, Email: m.Email}
	}
	return inboxPersonWire{Removed: true}
}

func (v inboxView) item(ctx context.Context, it store.InboxItem, mine store.InboxParticipant) (inboxItemWire, error) {
	parts, err := v.a.mgr.store.ListInboxParticipants(ctx, it.ID)
	if err != nil {
		return inboxItemWire{}, err
	}
	var eligible map[string]bool
	if it.Kind == "request" && it.State == "open" {
		if eligible, err = v.a.mgr.store.InboxEligibleRecipients(ctx, it.ID); err != nil {
			return inboxItemWire{}, err
		}
	}
	out := inboxItemWire{
		ID: it.ID, Kind: it.Kind, State: it.State, Role: mine.Role, Delivery: mine.Delivery,
		Sender: v.person(it.SenderMembershipID), Recipients: []inboxRecipientWire{},
		Body: v.a.open(ctx, it), Seq: mine.Seq,
		CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt, FinalAt: it.FinalAt,
		Unseen:       mine.Seq > v.cursor && it.UpdatedBy != v.me,
		CanClaim:     mine.Role == "recipient" && eligible[v.me],
		AssignedToMe: it.AssigneeMembershipID == v.me,
	}
	for _, p := range parts {
		if p.Role != "recipient" {
			continue
		}
		rw := inboxRecipientWire{inboxPersonWire: v.person(p.MembershipID)}
		if mine.Role == "sender" {
			rw.Delivery, rw.Eligible = p.Delivery, eligible[p.MembershipID]
		}
		out.Recipients = append(out.Recipients, rw)
	}
	if it.AssigneeMembershipID != "" {
		p := v.person(it.AssigneeMembershipID)
		out.Assignee = &p
	}
	if it.Kind == "request" && (it.State == "open" || it.State == "claimed") {
		if t, err := time.Parse(time.RFC3339, it.CreatedAt); err == nil && v.now.Sub(t) >= inboxLongOpenAfter {
			out.LongOpen = true
		}
	}
	if it.SessionName != "" {
		out.Session = &inboxSessionWire{Name: it.SessionName, Dir: it.SessionDir, Branch: it.SessionBranch,
			LaunchedAt: it.LaunchedAt, By: v.person(it.SessionMembershipID)}
		// The session lives in the launcher's workspace, so its name and path are theirs.
		// Others see that a session exists and when it started, not where.
		if it.SessionMembershipID != v.me {
			out.Session.Name, out.Session.Dir = "", ""
		}
	}
	if it.PRURL != "" || it.PRBranch != "" {
		out.PR = &inboxPRWire{URL: it.PRURL, Branch: it.PRBranch}
	}
	return out, nil
}

// inboxListWire is GET /api/inbox.
type inboxListWire struct {
	Items   []inboxItemWire  `json:"items"`
	Mailbox inboxMailboxWire `json:"mailbox"`
}

// inboxItemEnvelope is the answer of every route that changes one item.
type inboxItemEnvelope struct {
	Item inboxItemWire `json:"item"`
}

// inboxMailboxEnvelope is POST /api/inbox/cursor.
type inboxMailboxEnvelope struct {
	Mailbox inboxMailboxWire `json:"mailbox"`
}

func mailboxWire(mb store.InboxMailbox, unseen int) inboxMailboxWire {
	return inboxMailboxWire{LastSeq: mb.LastSeq, ReadSeq: mb.ReadSeq, Unseen: unseen, DND: mb.DND, AcceptFrom: mb.AcceptFrom}
}

// list (GET /api/inbox) returns every item the caller takes part in, sent and received,
// with the mailbox state the badge is drawn from.
func (a inboxAPI) list(w http.ResponseWriter, r *http.Request, _ store.Identity, mv store.MembershipView) {
	ctx := r.Context()
	a.reconcile(ctx, mv.TenantID)
	v, mb, err := a.view(ctx, mv)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	ret := inboxRetentionAt(v.now)
	entries, err := a.mgr.store.ListInboxEntries(ctx, mv.MembershipID, ret)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	items := make([]inboxItemWire, 0, len(entries))
	for _, e := range entries {
		if e.Item.TenantID != mv.TenantID {
			continue
		}
		iw, err := v.item(ctx, e.Item, e.Mine)
		if err != nil {
			writeAPIErr(w, internalErr(err))
			return
		}
		items = append(items, iw)
	}
	unseen, err := a.mgr.store.CountInboxUnseen(ctx, mv.MembershipID, ret)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, inboxListWire{Items: items, Mailbox: mailboxWire(mb, unseen)})
}

// participant loads an item and the caller's row in it. ok=false means 404 has been
// written: an item that does not exist, is expired, belongs to another tenant, or that the
// caller does not take part in all look the same (docs/log/130 A-4, A-5).
func (a inboxAPI) participant(w http.ResponseWriter, ctx context.Context, mv store.MembershipView, id string) (store.InboxItem, store.InboxParticipant, bool) {
	it, ok, err := a.mgr.store.GetInboxItem(ctx, id)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return store.InboxItem{}, store.InboxParticipant{}, false
	}
	if !ok || it.TenantID != mv.TenantID || inboxExpired(it, time.Now().UTC()) {
		inboxNotFound(w)
		return store.InboxItem{}, store.InboxParticipant{}, false
	}
	parts, err := a.mgr.store.ListInboxParticipants(ctx, id)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return store.InboxItem{}, store.InboxParticipant{}, false
	}
	for _, p := range parts {
		if p.MembershipID == mv.MembershipID {
			return it, p, true
		}
	}
	inboxNotFound(w)
	return store.InboxItem{}, store.InboxParticipant{}, false
}

func inboxExpired(it store.InboxItem, now time.Time) bool {
	ret := inboxRetentionAt(now)
	if it.Kind == "dm" {
		return it.CreatedAt < ret.DMBefore
	}
	return it.FinalAt != "" && it.FinalAt < ret.FinalBefore
}

// writeItem answers with the item as the caller sees it now.
func (a inboxAPI) writeItem(w http.ResponseWriter, ctx context.Context, mv store.MembershipView, id string, status int) {
	v, _, err := a.view(ctx, mv)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	it, ok, err := a.mgr.store.GetInboxItem(ctx, id)
	if err != nil || !ok {
		writeAPIErr(w, internalErr(err))
		return
	}
	parts, err := a.mgr.store.ListInboxParticipants(ctx, id)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	var mine store.InboxParticipant
	for _, p := range parts {
		if p.MembershipID == mv.MembershipID {
			mine = p
		}
	}
	iw, err := v.item(ctx, it, mine)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, status, inboxItemEnvelope{Item: iw})
}

func (a inboxAPI) detail(w http.ResponseWriter, r *http.Request, _ store.Identity, mv store.MembershipView) {
	ctx := r.Context()
	a.reconcile(ctx, mv.TenantID)
	if _, _, ok := a.participant(w, ctx, mv, r.PathValue("id")); !ok {
		return
	}
	a.writeItem(w, ctx, mv, r.PathValue("id"), http.StatusOK)
}

type inboxSendBody struct {
	Kind       string   `json:"kind"`
	Recipients []string `json:"recipients"`
	Body       string   `json:"body"`
}

// receiveRefusal reports whether recipient (a membership id) does not accept items from
// sender: blocked, or accepting from chosen members only and the sender is not chosen.
func (a inboxAPI) receiveRefusal(ctx context.Context, recipient, sender string) (bool, error) {
	mb, err := a.mgr.store.GetInboxMailbox(ctx, recipient)
	if err != nil {
		return false, err
	}
	rules, err := a.mgr.store.ListInboxSenderRules(ctx, recipient)
	if err != nil {
		return false, err
	}
	switch rules[sender] {
	case "block":
		return true, nil
	case "allow":
		return false, nil
	}
	return mb.AcceptFrom == "chosen", nil
}

// send (POST /api/inbox/items) sends a DM or a request. Either the whole item is created for
// every recipient or nothing is (docs/log/130 A-2, A-6): an item that reached only some of
// its recipients would leave the sender believing it reached all of them.
func (a inboxAPI) send(w http.ResponseWriter, r *http.Request, ident store.Identity, mv store.MembershipView) {
	ctx := r.Context()
	raw, err := io.ReadAll(io.LimitReader(r.Body, inboxBodyMaxBytes*6+64<<10))
	if err != nil {
		writeAPIErr(w, &apiError{http.StatusBadRequest, errCodeInboxBadRequest, "invalid inbox item"})
		return
	}
	var in inboxSendBody
	if json.Unmarshal(raw, &in) != nil || (in.Kind != "dm" && in.Kind != "request") {
		writeAPIErr(w, &apiError{http.StatusBadRequest, errCodeInboxBadRequest, "kind must be dm or request"})
		return
	}
	if strings.TrimSpace(in.Body) == "" {
		writeAPIErr(w, &apiError{http.StatusBadRequest, errCodeInboxBadRequest, "body is required"})
		return
	}
	if len(in.Body) > inboxBodyMaxBytes {
		writeInboxErr(w, http.StatusRequestEntityTooLarge, errCodeInboxBodyTooLarge, "body is too large",
			inboxErrWire{MaxBytes: inboxBodyMaxBytes})
		return
	}
	keys := map[string]bool{}
	var order []string
	for _, k := range in.Recipients {
		if k = strings.TrimSpace(k); k != "" && !keys[k] {
			keys[k] = true
			order = append(order, k)
		}
	}
	if len(order) == 0 {
		writeAPIErr(w, &apiError{http.StatusBadRequest, errCodeInboxBadRequest, "at least one recipient is required"})
		return
	}
	if len(order) > inboxMaxRecipients {
		writeInboxErr(w, http.StatusBadRequest, errCodeInboxTooManyRecipients, "too many recipients",
			inboxErrWire{Max: inboxMaxRecipients})
		return
	}
	v, _, err := a.view(ctx, mv)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	// Unknown, another tenant's and removed members are refused with one code and one
	// message that names none of them, so the answer is no oracle for who exists
	// (docs/log/130 A-2). The roster holds active members of this tenant only.
	var ids []string
	for _, k := range order {
		m, ok := v.byKey[k]
		if !ok || m.MembershipID == mv.MembershipID {
			writeAPIErr(w, &apiError{http.StatusBadRequest, errCodeInboxInvalidRecipient, "one or more recipients are not valid"})
			return
		}
		ids = append(ids, m.MembershipID)
	}
	// Receive controls (decision 10) are refused explicitly and by name (docs/log/130
	// §130.8 item 1): the sender learns that it will not arrive instead of waiting for an
	// answer that cannot come. Membership is no secret here, the roster already shows it.
	var refused []string
	for i, id := range ids {
		no, err := a.receiveRefusal(ctx, id, mv.MembershipID)
		if err != nil {
			writeAPIErr(w, internalErr(err))
			return
		}
		if no {
			refused = append(refused, order[i])
		}
	}
	if len(refused) > 0 {
		writeInboxErr(w, http.StatusForbidden, errCodeInboxNotAccepting, "some recipients do not accept items from you",
			inboxErrWire{Recipients: refused})
		return
	}
	ct, kr, err := a.seal(ctx, mv.TenantID, []byte(in.Body))
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	now := time.Now().UTC()
	it := store.InboxItem{ID: store.NewID(), TenantID: mv.TenantID, SenderMembershipID: mv.MembershipID,
		Kind: in.Kind, Ciphertext: ct, KeyRef: kr, CreatedAt: now.Format(time.RFC3339)}
	if in.Kind == "request" {
		it.State = "open"
	}
	res, err := a.mgr.store.CreateInboxItem(ctx, it, ids, store.InboxSendLimits{
		PerHour: inboxSendsPerHour, HourStart: now.Add(-time.Hour).Format(time.RFC3339), PendingPerPair: inboxPendingPerPair,
	})
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	switch {
	case res.Invalid:
		// A recipient left between the check above and the transaction: the same answer.
		writeAPIErr(w, &apiError{http.StatusBadRequest, errCodeInboxInvalidRecipient, "one or more recipients are not valid"})
		return
	case len(res.NotAccepting) > 0:
		var names []string
		for _, id := range res.NotAccepting {
			names = append(names, v.byID[id].UserKey)
		}
		writeInboxErr(w, http.StatusForbidden, errCodeInboxNotAccepting, "some recipients do not accept items from you",
			inboxErrWire{Recipients: names})
		return
	case res.RateLimited:
		writeInboxErr(w, http.StatusTooManyRequests, errCodeInboxRateLimited, "too many items sent in the last hour",
			inboxErrWire{PerHour: inboxSendsPerHour})
		return
	case len(res.PendingFull) > 0:
		var names []string
		for _, id := range res.PendingFull {
			names = append(names, v.byID[id].UserKey)
		}
		writeInboxErr(w, http.StatusTooManyRequests, errCodeInboxPendingLimit, "too many pending items for some recipients",
			inboxErrWire{Recipients: names, PerPair: inboxPendingPerPair})
		return
	}
	// Who sent which kind to whom, and when. Never the body (decision 9).
	sort.Strings(order)
	_ = a.mgr.store.InsertAudit(context.WithoutCancel(ctx), store.AuditLog{ID: store.NewID(), TenantID: mv.TenantID,
		ActorKind: "user", ActorID: ident.ID, Action: "inbox.send", Target: it.ID,
		Detail: "kind=" + in.Kind + " recipients=" + strings.Join(order, ","), HTTPStatus: http.StatusCreated, At: store.NowTS()})
	a.writeItem(w, ctx, mv, it.ID, http.StatusCreated)
}

func (a inboxAPI) read(w http.ResponseWriter, r *http.Request, _ store.Identity, mv store.MembershipView) {
	ctx := r.Context()
	id := r.PathValue("id")
	// It answers with the item, so it reconciles first like every other read.
	a.reconcile(ctx, mv.TenantID)
	if _, _, ok := a.participant(w, ctx, mv, id); !ok {
		return
	}
	if _, err := a.mgr.store.MarkInboxRead(ctx, id, mv.MembershipID, store.NowTS()); err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	a.writeItem(w, ctx, mv, id, http.StatusOK)
}

// transition runs one operation of the state table. A refusal is 409 with the state and the
// assignee as they are now, so the screen can show what happened instead of a bare error
// (docs/log/130 R-1: "B claimed it").
func (a inboxAPI) transition(w http.ResponseWriter, r *http.Request, ident store.Identity, mv store.MembershipView,
	op string, fn func(ctx context.Context, id, mid, now string) (bool, error)) {
	ctx := r.Context()
	id := r.PathValue("id")
	a.reconcile(ctx, mv.TenantID)
	it, _, ok := a.participant(w, ctx, mv, id)
	if !ok {
		return
	}
	changed, err := fn(ctx, id, mv.MembershipID, store.NowTS())
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	if !changed {
		cur, _, _ := a.mgr.store.GetInboxItem(ctx, id)
		// A second claim by the assignee (a double click, a retry from another tab) is
		// answered as the success it already was (docs/log/130 R-4).
		if op == "claim" && cur.State == "claimed" && cur.AssigneeMembershipID == mv.MembershipID {
			a.writeItem(w, ctx, mv, id, http.StatusOK)
			return
		}
		writeInboxErr(w, http.StatusConflict, errCodeInboxStateConflict, "the item is not in a state that allows "+op,
			a.inboxConflict(ctx, mv, cur))
		return
	}
	_ = a.mgr.store.InsertAudit(context.WithoutCancel(ctx), store.AuditLog{ID: store.NewID(), TenantID: mv.TenantID,
		ActorKind: "user", ActorID: ident.ID, Action: "inbox." + op, Target: id, Detail: "kind=" + it.Kind,
		HTTPStatus: http.StatusOK, At: store.NowTS()})
	a.writeItem(w, ctx, mv, id, http.StatusOK)
}

func (a inboxAPI) claim(w http.ResponseWriter, r *http.Request, ident store.Identity, mv store.MembershipView) {
	a.transition(w, r, ident, mv, "claim", a.mgr.store.ClaimInboxItem)
}

func (a inboxAPI) decline(w http.ResponseWriter, r *http.Request, ident store.Identity, mv store.MembershipView) {
	a.transition(w, r, ident, mv, "decline", a.mgr.store.DeclineInboxItem)
}

func (a inboxAPI) release(w http.ResponseWriter, r *http.Request, ident store.Identity, mv store.MembershipView) {
	a.transition(w, r, ident, mv, "release", a.mgr.store.ReleaseInboxItem)
}

func (a inboxAPI) done(w http.ResponseWriter, r *http.Request, ident store.Identity, mv store.MembershipView) {
	a.transition(w, r, ident, mv, "done", a.mgr.store.CompleteInboxItem)
}

func (a inboxAPI) withdraw(w http.ResponseWriter, r *http.Request, ident store.Identity, mv store.MembershipView) {
	a.transition(w, r, ident, mv, "withdraw", a.mgr.store.WithdrawInboxItem)
}

// cleanPRURL keeps an http(s) link and drops userinfo, query and fragment, so a pasted URL
// carrying a token does not end up in front of every participant.
func cleanPRURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", true
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", false
	}
	u.User, u.RawQuery, u.Fragment, u.RawFragment = nil, "", "", ""
	return u.String(), true
}

// setPR (PUT /api/inbox/items/{id}/pr) records the PR and the final branch by hand. Only the
// assignee may, and agents never report links (decision 2).
func (a inboxAPI) setPR(w http.ResponseWriter, r *http.Request, ident store.Identity, mv store.MembershipView) {
	var in struct {
		URL    string `json:"url"`
		Branch string `json:"branch"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 3*inboxPRFieldMax)).Decode(&in) != nil {
		writeAPIErr(w, &apiError{http.StatusBadRequest, errCodeInboxBadRequest, "invalid PR link"})
		return
	}
	u, ok := cleanPRURL(in.URL)
	branch := strings.TrimSpace(in.Branch)
	if !ok || len(u) > inboxPRFieldMax || len(branch) > 255 {
		writeAPIErr(w, &apiError{http.StatusBadRequest, errCodeInboxBadRequest, "the PR link must be an http(s) URL"})
		return
	}
	a.transition(w, r, ident, mv, "pr", func(ctx context.Context, id, mid, now string) (bool, error) {
		return a.mgr.store.SetInboxPRLink(ctx, id, mid, u, branch, now)
	})
}

func (a inboxAPI) cursor(w http.ResponseWriter, r *http.Request, _ store.Identity, mv store.MembershipView) {
	var in struct {
		Seq int64 `json:"seq"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in) != nil || in.Seq < 0 {
		writeAPIErr(w, &apiError{http.StatusBadRequest, errCodeInboxBadRequest, "seq is required"})
		return
	}
	mb, err := a.mgr.store.SetInboxReadCursor(r.Context(), mv.MembershipID, in.Seq)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	unseen, err := a.mgr.store.CountInboxUnseen(r.Context(), mv.MembershipID, inboxRetentionAt(time.Now().UTC()))
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	writeJSON(w, http.StatusOK, inboxMailboxEnvelope{Mailbox: mailboxWire(mb, unseen)})
}

type inboxSettingsWire struct {
	AcceptFrom string            `json:"acceptFrom"`
	DND        bool              `json:"dnd"`
	Allowed    []inboxPersonWire `json:"allowed"`
	Blocked    []inboxPersonWire `json:"blocked"`
	// Sealed says whether bodies are encrypted at rest with the tenant key. Settings has to
	// say so when they are not (decision 9).
	Sealed bool `json:"sealed"`
}

func (a inboxAPI) settings(w http.ResponseWriter, r *http.Request, _ store.Identity, mv store.MembershipView) {
	ctx := r.Context()
	v, mb, err := a.view(ctx, mv)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	rules, err := a.mgr.store.ListInboxSenderRules(ctx, mv.MembershipID)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	out := inboxSettingsWire{AcceptFrom: mb.AcceptFrom, DND: mb.DND, Allowed: []inboxPersonWire{},
		Blocked: []inboxPersonWire{}, Sealed: a.mgr.custodian != nil}
	for mid, rule := range rules {
		p := v.person(mid)
		if p.Removed {
			continue
		}
		if rule == "block" {
			out.Blocked = append(out.Blocked, p)
		} else {
			out.Allowed = append(out.Allowed, p)
		}
	}
	sort.Slice(out.Allowed, func(i, j int) bool { return out.Allowed[i].UserKey < out.Allowed[j].UserKey })
	sort.Slice(out.Blocked, func(i, j int) bool { return out.Blocked[i].UserKey < out.Blocked[j].UserKey })
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, out)
}

func (a inboxAPI) putSettings(w http.ResponseWriter, r *http.Request, _ store.Identity, mv store.MembershipView) {
	var in struct {
		AcceptFrom string   `json:"acceptFrom"`
		DND        bool     `json:"dnd"`
		Allowed    []string `json:"allowed"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in) != nil || (in.AcceptFrom != "tenant" && in.AcceptFrom != "chosen") {
		writeAPIErr(w, &apiError{http.StatusBadRequest, errCodeInboxBadRequest, "acceptFrom must be tenant or chosen"})
		return
	}
	ctx := r.Context()
	v, _, err := a.view(ctx, mv)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	var ids []string
	for _, k := range in.Allowed {
		m, ok := v.byKey[strings.TrimSpace(k)]
		if !ok || m.MembershipID == mv.MembershipID {
			writeAPIErr(w, &apiError{http.StatusBadRequest, errCodeInboxInvalidRecipient, "one or more members are not valid"})
			return
		}
		ids = append(ids, m.MembershipID)
	}
	if err := a.mgr.store.PutInboxSettings(ctx, mv.MembershipID, in.AcceptFrom, in.DND, ids); err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	a.settings(w, r, store.Identity{}, mv)
}

func (a inboxAPI) setBlock(w http.ResponseWriter, r *http.Request, ident store.Identity, mv store.MembershipView, blocked bool) {
	ctx := r.Context()
	v, _, err := a.view(ctx, mv)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	m, ok := v.byKey[r.PathValue("userKey")]
	if !ok || m.MembershipID == mv.MembershipID {
		writeAPIErr(w, &apiError{http.StatusBadRequest, errCodeInboxInvalidRecipient, "not a valid member"})
		return
	}
	if err := a.mgr.store.SetInboxBlock(ctx, mv.MembershipID, m.MembershipID, blocked); err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	action := "inbox.unblock"
	if blocked {
		action = "inbox.block"
		// A block takes the blocker off that sender's requests at once: a claim goes back to
		// open, and a request nobody else can take is declined (decision 2, system rows).
		a.reconcile(ctx, mv.TenantID)
	}
	_ = a.mgr.store.InsertAudit(context.WithoutCancel(ctx), store.AuditLog{ID: store.NewID(), TenantID: mv.TenantID,
		ActorKind: "user", ActorID: ident.ID, Action: action, Target: m.UserKey, HTTPStatus: http.StatusOK, At: store.NowTS()})
	a.settings(w, r, ident, mv)
}

func (a inboxAPI) block(w http.ResponseWriter, r *http.Request, ident store.Identity, mv store.MembershipView) {
	a.setBlock(w, r, ident, mv, true)
}

func (a inboxAPI) unblock(w http.ResponseWriter, r *http.Request, ident store.Identity, mv store.MembershipView) {
	a.setBlock(w, r, ident, mv, false)
}
