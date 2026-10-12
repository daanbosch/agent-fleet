package main

// Recipes, take-in and session attachments of the member inbox (ADR 0111 decisions 2-5,
// P1-A second half).
//
// A recipe is a prompt with a suggested kind, model and effort, and optionally a repository
// named by a canonical identity. Receiving one fetches and clones nothing, and nothing of it
// reaches an agent until the recipient takes it in: take-in copies it into the recipient's
// memo queue as their own memo with an origin the CP sets, and from then on the memo queue's
// own paths apply. A session attachment shows a shared session in the message; what each
// reader sees of it is decided on every read from the share ACL.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/k-k1/agent-fleet/control-plane/internal/store"
)

const (
	inboxRecipePromptMax  = 16 << 10
	inboxRecipeFieldMax   = 128
	inboxMaxSessionAttach = 5
)

// inboxRepoHosts are the git providers a recipe may name, the ones a member can connect in
// the Console. A repository elsewhere has no canonical identity here and is refused.
var inboxRepoHosts = map[string]bool{"github.com": true, "bitbucket.org": true}

var (
	inboxRepoSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	inboxRecipeToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$`)
)

// canonicalRepoIdentity turns what a sender typed into "host/owner/repo", or reports false.
// It accepts the identity itself, an https URL and the two ssh forms, and keeps nothing of a
// URL but host and path: userinfo, port, query and fragment are dropped, a trailing ".git"
// too. A remote that does not reduce to exactly owner/repo on a known host is refused rather
// than passed through, so a recipient never sees a link the CP could not vouch for.
func canonicalRepoIdentity(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", true
	}
	var host, path string
	switch {
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "ssh" && u.Scheme != "git") {
			return "", false
		}
		host, path = u.Hostname(), u.Path
	case strings.Contains(raw, "@") && strings.Contains(raw, ":"):
		// scp-like ssh: git@host:owner/repo.git
		at := strings.LastIndex(raw, "@")
		rest := raw[at+1:]
		colon := strings.Index(rest, ":")
		if colon < 0 {
			return "", false
		}
		host, path = rest[:colon], rest[colon+1:]
	default:
		parts := strings.SplitN(raw, "/", 2)
		if len(parts) != 2 {
			return "", false
		}
		host, path = parts[0], parts[1]
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if !inboxRepoHosts[host] {
		return "", false
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	segs := strings.Split(path, "/")
	if len(segs) != 2 || !inboxRepoSegment.MatchString(segs[0]) || !inboxRepoSegment.MatchString(segs[1]) {
		return "", false
	}
	return host + "/" + strings.ToLower(segs[0]) + "/" + strings.ToLower(segs[1]), true
}

// inboxRecipe is a recipe as sent and as sealed.
type inboxRecipe struct {
	Prompt string `json:"prompt"`
	Kind   string `json:"kind"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
	Repo   string `json:"repo"`
}

// inboxShareConfirm is the owner's confirmation of the share a session attachment creates:
// every recipient and the scope, as the owner saw them before sending.
type inboxShareConfirm struct {
	Recipients []string `json:"recipients"`
	Scope      string   `json:"scope"`
}

// inboxAttachmentIn is one attachment of a send. A session is named by sessionName (the
// sender's own session) or catalogId (one shared with the sender).
type inboxAttachmentIn struct {
	Type        string             `json:"type"`
	Recipe      *inboxRecipe       `json:"recipe"`
	SessionName string             `json:"sessionName"`
	CatalogID   string             `json:"catalogId"`
	Share       *inboxShareConfirm `json:"share"`
}

func validRecipeToken(s string) bool { return s == "" || inboxRecipeToken.MatchString(s) }

// prepareAttachments validates a send's attachments into what the store creates with the
// item. ids are the recipients' membership ids, keys their user keys, in the same order.
func (a inboxAPI) prepareAttachments(r *http.Request, mv store.MembershipView, ids, keys []string, in []inboxAttachmentIn) (store.InboxSendExtras, *apiError) {
	ctx := r.Context()
	var out store.InboxSendExtras
	recipes, sessions := 0, 0
	seen := map[string]bool{}
	for _, at := range in {
		switch at.Type {
		case "recipe":
			recipes++
			if recipes > 1 || at.Recipe == nil {
				return out, &apiError{http.StatusBadRequest, errCodeInboxAttachmentInvalid, "at most one recipe, with a prompt"}
			}
			rc := *at.Recipe
			rc.Prompt = strings.TrimSpace(rc.Prompt)
			rc.Kind, rc.Model, rc.Effort = strings.TrimSpace(rc.Kind), strings.TrimSpace(rc.Model), strings.TrimSpace(rc.Effort)
			if rc.Prompt == "" || len(rc.Prompt) > inboxRecipePromptMax {
				return out, &apiError{http.StatusBadRequest, errCodeInboxAttachmentInvalid, "a recipe needs a prompt of at most 16 KiB"}
			}
			if !validRecipeToken(rc.Kind) || !validRecipeToken(rc.Model) || !validRecipeToken(rc.Effort) {
				return out, &apiError{http.StatusBadRequest, errCodeInboxAttachmentInvalid, "the suggested kind, model or effort is not valid"}
			}
			repo, ok := canonicalRepoIdentity(rc.Repo)
			if !ok {
				return out, &apiError{http.StatusBadRequest, errCodeInboxRepoInvalid, "the repository must be github.com or bitbucket.org owner/repo"}
			}
			rc.Repo = repo
			b, _ := json.Marshal(rc)
			ct, kr, err := a.sealAttachment(ctx, mv.TenantID, b)
			if err != nil {
				return out, internalErr(err)
			}
			out.Attachments = append(out.Attachments, store.InboxAttachment{ID: store.NewID(), Kind: "recipe", Ciphertext: ct, KeyRef: kr})
		case "session":
			sessions++
			if sessions > inboxMaxSessionAttach {
				return out, &apiError{http.StatusBadRequest, errCodeInboxAttachmentInvalid, "too many sessions attached"}
			}
			cat, owner, aerr := a.attachableSession(r, mv, at)
			if aerr != nil {
				return out, aerr
			}
			if seen[cat.ID] {
				continue
			}
			seen[cat.ID] = true
			att := store.InboxAttachment{ID: store.NewID(), Kind: "session", CatalogID: cat.ID}
			if at.Share != nil {
				// Only the owner can create a share by attaching (decision 5), and only with
				// every recipient and the scope confirmed as they are.
				if !owner {
					return out, &apiError{http.StatusForbidden, errCodeInboxShareNotOwner, "only the session's owner can share it"}
				}
				if at.Share.Scope != "session" || !sameKeys(at.Share.Recipients, keys) {
					return out, &apiError{http.StatusBadRequest, errCodeInboxShareUnconfirmed, "confirm every recipient and the scope of the share"}
				}
				now := store.NowTS()
				for _, rid := range ids {
					out.Shares = append(out.Shares, store.SessionShare{ID: store.NewID(), TenantID: mv.TenantID,
						OwnerMembershipID: mv.MembershipID, RecipientMembershipID: rid, ScopeType: "session",
						ScopeKey: cat.Name, Permission: "ro", CreatedAt: now, UpdatedAt: now})
				}
				att.Granted = true
			}
			out.Attachments = append(out.Attachments, att)
		default:
			return out, &apiError{http.StatusBadRequest, errCodeInboxAttachmentInvalid, "unknown attachment type"}
		}
	}
	return out, nil
}

// sealAttachment seals a recipe like a body: with the tenant key custodian when there is one,
// base64 otherwise. Its own function so rewrap-keys can tell which table it seals into.
func (a inboxAPI) sealAttachment(ctx context.Context, tenant string, b []byte) (string, string, error) {
	if a.mgr.custodian == nil {
		return base64.StdEncoding.EncodeToString(b), "", nil
	}
	ct, err := a.mgr.custodian.Wrap(ctx, tenant, b)
	return ct, tenant, err
}

func sameKeys(a, b []string) bool {
	x := map[string]bool{}
	for _, k := range a {
		x[strings.TrimSpace(k)] = true
	}
	if len(x) != len(b) {
		return false
	}
	for _, k := range b {
		if !x[k] {
			return false
		}
	}
	return true
}

// attachableSession finds the session an attachment names, and whether the sender owns it.
// The sender's own session is looked up by name in their share catalogue, synced from their
// workspace when the catalogue does not have it yet. A session someone else owns can be
// attached only when it is already shared with the sender. Anything else is refused the same
// way, so the answer says nothing about other members' sessions.
func (a inboxAPI) attachableSession(r *http.Request, mv store.MembershipView, at inboxAttachmentIn) (store.SharedSessionCatalog, bool, *apiError) {
	ctx := r.Context()
	notFound := &apiError{http.StatusNotFound, errCodeInboxAttachmentInvalid, "the session cannot be attached"}
	if name := strings.TrimSpace(at.SessionName); name != "" {
		find := func() (store.SharedSessionCatalog, bool, error) {
			rows, err := a.mgr.store.ListSharedSessionCatalogByOwner(ctx, mv.MembershipID)
			if err != nil {
				return store.SharedSessionCatalog{}, false, err
			}
			for _, c := range rows {
				if c.Name == name && !c.Archived {
					return c, true, nil
				}
			}
			return store.SharedSessionCatalog{}, false, nil
		}
		c, ok, err := find()
		if err != nil {
			return c, false, internalErr(err)
		}
		if !ok {
			// Not in the catalogue yet: ask the owner's own running workspace, as a share
			// does. A member with no workspace has no session to attach and stops here.
			if _, has, err := a.mgr.store.GetWorkspaceByMembership(ctx, mv.MembershipID); err != nil || !has {
				return c, false, notFound
			}
			key := a.mgr.resolveIdentity(r)
			res, aerr := a.mgr.resolveFull(ctx, key.key, key.email, tenantSel(r))
			if aerr != nil || res.mv.MembershipID != mv.MembershipID || res.rt.State(ctx) != "running" {
				return c, false, &apiError{http.StatusConflict, "workspace_not_running", "your workspace must be running to attach your session"}
			}
			share := newSessionShareAPI(a.mgr)
			lock := a.mgr.shareLockFor(mv.MembershipID)
			lock.Lock()
			_, serr := share.syncCatalogLocked(ctx, res)
			lock.Unlock()
			if serr != nil {
				return c, false, &apiError{http.StatusConflict, "workspace_not_running", "your workspace must be running to attach your session"}
			}
			if c, ok, err = find(); err != nil {
				return c, false, internalErr(err)
			} else if !ok {
				return c, false, notFound
			}
		}
		return c, true, nil
	}
	c, ok, err := a.mgr.store.GetSharedSessionCatalog(ctx, strings.TrimSpace(at.CatalogID))
	if err != nil {
		return c, false, internalErr(err)
	}
	if !ok || c.Archived {
		return c, false, notFound
	}
	if c.OwnerMembershipID == mv.MembershipID {
		return c, true, nil
	}
	if !a.canSeeSession(ctx, mv.MembershipID, c) {
		return c, false, notFound
	}
	return c, false, nil
}

// canSeeSession is the share ACL: the owner, or a member a share currently covers it for.
func (a inboxAPI) canSeeSession(ctx context.Context, viewer string, c store.SharedSessionCatalog) bool {
	if c.OwnerMembershipID == viewer {
		return true
	}
	if c.Archived {
		return false
	}
	shares, err := a.mgr.store.ListSessionSharesByRecipient(ctx, viewer)
	if err != nil {
		return false
	}
	return effectivePermission(shares, c) != ""
}

// inboxRecipeWire is a recipe as a participant sees it. Repo is shown apart from the prompt
// so the recipient sees what it points at (decision 4).
type inboxRecipeWire struct {
	Prompt string `json:"prompt"`
	Kind   string `json:"kind"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
	Repo   string `json:"repo"`
	// CanTakeIn: the caller may take it in now. MemoID: the memo they already took it in as.
	CanTakeIn bool   `json:"canTakeIn"`
	MemoID    string `json:"memoId"`
}

// inboxSharedSessionWire is an attached session the reader may see. Mine: the reader owns it,
// so Name is theirs to open; anyone else opens the shared view by CatalogID.
type inboxSharedSessionWire struct {
	CatalogID string          `json:"catalogId"`
	Name      string          `json:"name"`
	Title     string          `json:"title"`
	Kind      string          `json:"kind"`
	Repo      string          `json:"repo"`
	Branch    string          `json:"branch"`
	Owner     inboxPersonWire `json:"owner"`
	Mine      bool            `json:"mine"`
}

// inboxAttachmentWire is one attachment as one reader sees it. A session the reader cannot
// see is Available=false with nothing else: no catalog id, title, repository, branch, owner
// or preview (decision 5). The placeholder is the server's, decided on this read.
type inboxAttachmentWire struct {
	Type      string                  `json:"type"`
	Available bool                    `json:"available"`
	Recipe    *inboxRecipeWire        `json:"recipe,omitempty"`
	Session   *inboxSharedSessionWire `json:"session,omitempty"`
}

func (a inboxAPI) openRecipe(ctx context.Context, at store.InboxAttachment) (inboxRecipe, bool) {
	var rc inboxRecipe
	body := a.open(ctx, store.InboxItem{Ciphertext: at.Ciphertext, KeyRef: at.KeyRef})
	if body == "" || json.Unmarshal([]byte(body), &rc) != nil {
		return rc, false
	}
	return rc, true
}

// mayTakeIn is decision 2's rule for one participant: any recipient of a DM, the current
// assignee of a request.
func mayTakeIn(it store.InboxItem, mine store.InboxParticipant) bool {
	if mine.Role != "recipient" {
		return false
	}
	if it.Kind == "dm" {
		return true
	}
	return (it.State == "claimed" || it.State == "done") && it.AssigneeMembershipID == mine.MembershipID
}

func (v inboxView) attachments(ctx context.Context, it store.InboxItem, mine store.InboxParticipant) ([]inboxAttachmentWire, error) {
	rows, err := v.a.mgr.store.ListInboxAttachments(ctx, it.ID)
	if err != nil {
		return nil, err
	}
	out := []inboxAttachmentWire{}
	for _, at := range rows {
		switch at.Kind {
		case "recipe":
			rc, ok := v.a.openRecipe(ctx, at)
			if !ok {
				out = append(out, inboxAttachmentWire{Type: "recipe"})
				continue
			}
			w := &inboxRecipeWire{Prompt: rc.Prompt, Kind: rc.Kind, Model: rc.Model, Effort: rc.Effort, Repo: rc.Repo,
				CanTakeIn: mayTakeIn(it, mine)}
			if m, found, err := v.a.mgr.store.FindMemoByOrigin(ctx, it.ID, v.me); err == nil && found {
				w.MemoID = m.ID
			}
			out = append(out, inboxAttachmentWire{Type: "recipe", Available: true, Recipe: w})
		case "session":
			c, ok, err := v.a.mgr.store.GetSharedSessionCatalog(ctx, at.CatalogID)
			if err != nil {
				return nil, err
			}
			if !ok || !v.a.canSeeSession(ctx, v.me, c) {
				out = append(out, inboxAttachmentWire{Type: "session"})
				continue
			}
			out = append(out, inboxAttachmentWire{Type: "session", Available: true, Session: &inboxSharedSessionWire{
				CatalogID: c.ID, Name: c.Name, Title: c.Title, Kind: c.Kind, Repo: c.Repo, Branch: c.Branch,
				Owner: v.person(c.OwnerMembershipID), Mine: c.OwnerMembershipID == v.me,
			}})
		}
	}
	return out, nil
}

// inboxTakeInWire is the answer of a take-in.
type inboxTakeInWire struct {
	Memo    memoDTO `json:"memo"`
	Created bool    `json:"created"`
}

// takeIn (POST /api/inbox/items/{id}/take-in) copies the item's recipe into the caller's
// memo queue with the server's origin. It is withMembership like every inbox route: taking
// in touches no workspace (docs/log/130 W-4).
func (a inboxAPI) takeIn(w http.ResponseWriter, r *http.Request, ident store.Identity, mv store.MembershipView) {
	ctx := r.Context()
	id := r.PathValue("id")
	a.reconcile(ctx, mv.TenantID)
	it, mine, ok := a.participant(w, ctx, mv, id)
	if !ok {
		return
	}
	rows, err := a.mgr.store.ListInboxAttachments(ctx, id)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	var rc inboxRecipe
	found := false
	for _, at := range rows {
		if at.Kind == "recipe" {
			rc, found = a.openRecipe(ctx, at)
			break
		}
	}
	if !found {
		writeAPIErr(w, &apiError{http.StatusNotFound, errCodeInboxAttachmentInvalid, "this item has no recipe"})
		return
	}
	if mine.Role != "recipient" {
		writeInboxErr(w, http.StatusConflict, errCodeInboxStateConflict, "only a recipient can take a recipe in", a.inboxConflict(ctx, mv, it))
		return
	}
	v, _, err := a.view(ctx, mv)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	sender := v.person(it.SenderMembershipID)
	m := store.Memo{ID: store.NewID(), Kind: "text", Body: rc.Prompt, CreatedAt: store.NowTS(),
		OriginSenderMembershipID: it.SenderMembershipID, OriginSenderKey: sender.UserKey}
	memo, res, err := a.mgr.store.TakeInInboxRecipe(ctx, id, mv.MembershipID, m)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	if !res.Allowed {
		cur, _, _ := a.mgr.store.GetInboxItem(ctx, id)
		writeInboxErr(w, http.StatusConflict, errCodeInboxStateConflict, "only the current assignee can take this request's recipe in",
			a.inboxConflict(ctx, mv, cur))
		return
	}
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
		_ = a.mgr.store.InsertAudit(context.WithoutCancel(ctx), store.AuditLog{ID: store.NewID(), TenantID: mv.TenantID,
			ActorKind: "user", ActorID: ident.ID, Action: "inbox.take_in", Target: id, Detail: "kind=" + it.Kind,
			HTTPStatus: status, At: store.NowTS()})
	}
	writeJSON(w, status, inboxTakeInWire{Memo: memoToDTO(memo), Created: res.Created})
}

// attachmentAudit names what a send attached, for its audit row: kinds and whether a share
// was granted, never a prompt or a session's name.
func attachmentAudit(x store.InboxSendExtras) string {
	recipes, sessions, granted := 0, 0, 0
	for _, at := range x.Attachments {
		if at.Kind == "recipe" {
			recipes++
		} else {
			sessions++
			if at.Granted {
				granted++
			}
		}
	}
	if recipes+sessions == 0 {
		return ""
	}
	return " recipes=" + itoa(recipes) + " sessions=" + itoa(sessions) + " granted=" + itoa(granted)
}

func itoa(n int) string { return strconv.Itoa(n) }

// inboxOriginKey carries a provenance the CP itself decided (the request launch) into the
// session create, past stampInboxOrigin's rule that a client may not name one.
type inboxOriginKey struct{}

type inboxOriginRef struct {
	Sender string `json:"sender"`
	Item   string `json:"item"`
}

// stampInboxOrigin decides the provenance of a session create (decision 3) and strips every
// provenance key a client wrote. It always parses the body: a key spelled with an escape
// (\u0069nbox_origin) or in another case (INBOX_ORIGIN) is the same key to the Agent's
// decoder, which matches names case-insensitively, so the match here is the same fold. The
// origin comes from the request launch (through the context) or from the memo a launch was
// seeded from (inbox_memo_id, looked up and owned); it is not sent to the Agent at all. The
// caller records it once the create has succeeded, and the badge is drawn from that record.
func (a workspaceAPI) stampInboxOrigin(ctx context.Context, res *resolved, r *http.Request) (*inboxOriginRef, *apiError) {
	body, err := readAllBody(r)
	if err != nil {
		return nil, &apiError{http.StatusBadRequest, "bad_request", "cannot read body"}
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil || m == nil {
		return nil, &apiError{http.StatusBadRequest, "bad_request", "invalid JSON body"}
	}
	memoID := ""
	for k, v := range m {
		switch {
		case strings.EqualFold(k, "inbox_memo_id"):
			var id string
			if json.Unmarshal(v, &id) == nil && id != "" {
				memoID = id
			}
			delete(m, k)
		case strings.EqualFold(k, "inbox_origin"):
			delete(m, k)
		}
	}
	out, _ := json.Marshal(m)
	restoreBody(r, out)
	if o, ok := ctx.Value(inboxOriginKey{}).(inboxOriginRef); ok {
		return &o, nil
	}
	if memoID != "" {
		if memo, found, err := a.mgr.store.GetMemo(ctx, memoID); err == nil && found &&
			memo.MembershipID == res.mv.MembershipID && memo.OriginItemID != "" {
			return &inboxOriginRef{Sender: memo.OriginSenderKey, Item: memo.OriginItemID}, nil
		}
	}
	return nil, nil
}

// teeWriter passes a response through and keeps its status and the start of its body, so a
// create can be recorded once the Agent has answered.
type teeWriter struct {
	http.ResponseWriter
	status int
	buf    []byte
}

func (t *teeWriter) WriteHeader(s int) {
	if t.status == 0 {
		t.status = s
	}
	t.ResponseWriter.WriteHeader(s)
}
func (t *teeWriter) Write(b []byte) (int, error) {
	if t.status == 0 {
		t.status = http.StatusOK
	}
	if len(t.buf) < 64<<10 {
		t.buf = append(t.buf, b...)
	}
	return t.ResponseWriter.Write(b)
}
func (t *teeWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// recordInboxOrigin writes the provenance of the session a create just made.
func (a workspaceAPI) recordInboxOrigin(ctx context.Context, res *resolved, o *inboxOriginRef, t *teeWriter) {
	if t.status < 200 || t.status >= 300 {
		return
	}
	var s struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(t.buf, &s) != nil || s.Name == "" {
		return
	}
	// A memo-seeded create has no idempotency key to recover it by, so this is the one chance
	// to write its badge: tried a few times, then logged. The session itself is unaffected.
	for i := 0; i < inboxLaunchRecordTries; i++ {
		err := a.mgr.store.RecordInboxSessionOrigin(context.WithoutCancel(ctx), res.mv.MembershipID, s.Name, o.Item, o.Sender)
		if err == nil {
			return
		}
		log.Printf("inbox: recording the provenance of session %s failed (try %d): %v", s.Name, i+1, err)
		time.Sleep(time.Duration(100*(i+1)) * time.Millisecond)
	}
}

// applyInboxOrigins sets each session's provenance badge from the CP's own record, and only
// from it: what the workspace reports is dropped, since an agent can call the Agent without
// the CP and give any session any value.
func (a workspaceAPI) applyInboxOrigins(ctx context.Context, mv store.MembershipView, list []sessionWire) []sessionWire {
	rec, err := a.mgr.store.ListInboxSessionOrigins(ctx, mv.MembershipID)
	for i := range list {
		list[i].InboxOrigin = nil
		if err != nil {
			continue
		}
		if o, ok := rec[list[i].Name]; ok {
			list[i].InboxOrigin, _ = json.Marshal(inboxOriginRef{Sender: o.SenderKey, Item: o.ItemID})
		}
	}
	return list
}

// errInboxShareBusy maps the share lease refusal of a send that grants a share.
func isShareBusy(err error) bool { return errors.Is(err, store.ErrSessionShareOwnerBusy) }
