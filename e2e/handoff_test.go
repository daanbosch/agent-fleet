//go:build e2e

// Member handoff (ADR 0057, docs/log/77 §77.5–§77.12) between two real workspaces: the
// automated part of #1960's two-account run sheet (H-1..H-16).
//
// The session handed over is kind=claude, because only kinds with a transcript enter the
// shared-session catalog, and a share or an offer needs a catalog row (a shell session is
// refused with share_target_not_found). It is never driven, so no LLM credential is
// needed: the commits are made in a shell session in the same working copy, the push is the
// offer's own "push, then send", and the handoff proposal is posted to the session the way
// propose_session_handoff does.
package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestMemberHandoff(t *testing.T) {
	f := startMemberFleet(t)
	remote := newGitRemote(t, dockerBridgeGateway(t))
	repoURL := remote.newRepo(t, "handoff")
	const repoDir = "repos/handoff" // where clone-then-start puts it (derived from the URL)
	const branch = "e2e/handoff"

	a, b := f.addMember("alice"), f.addMember("bob")

	// --- B has a working copy of the repository from before the branch existed (H-9) ---
	b.startWorkspace()
	bGit := b.createSession(map[string]any{"kind": "shell", "title": "b-git", "remote_url": repoURL})
	if got := b.shell(bGit, "git branch --show-current"); got != "main" {
		t.Fatalf("B's clone is on %q, want main", got)
	}
	b.stopWorkspace()

	// --- A: a clone, a session in it, a commit on a new branch, a handoff proposal (H-1) ---
	a.startWorkspace()
	aGit := a.createSession(map[string]any{"kind": "shell", "title": "a-git", "remote_url": repoURL})
	a.shell(aGit, fmt.Sprintf(`git config user.name alice && git config user.email %s && `+
		`git switch -q -c %s && echo one > work.txt && git add work.txt && git commit -q -m one`, a.email, branch))
	src := a.createSession(map[string]any{"kind": "claude", "title": "handoff source", "dir": repoDir})
	const title, prompt = "Carry on the e2e handoff", "Continue the work on e2e/handoff."
	a.postJSON("/api/sessions/"+src+"/handoff-proposal", map[string]any{"title": title, "prompt": prompt}, http.StatusOK)
	if ps, _ := a.getJSON("/api/sessions/" + src + "/handoff-proposal")["proposals"].([]any); len(ps) != 1 {
		t.Fatalf("source session holds %d handoff proposals, want 1", len(ps))
	}

	// Not shared with anyone yet: an empty candidate list, not an error (H-2).
	if ms := handoffRecipients(t, a, src).members; len(ms) != 0 {
		t.Fatalf("unshared session offers candidates %v", ms)
	}
	share := a.postJSON("/api/session-shares", map[string]any{
		"recipientUserKey": b.key, "scope": map[string]any{"type": "session", "key": src}, "permission": "ro",
	}, http.StatusCreated)
	shareID, _ := share["id"].(string)

	// --- the push gate and "push, then send" (H-3, H-4, #2072) ---
	// The owner never pushes from a shell: the offer's pushFirst has the owner's Agent push
	// the branch and the gate is asked again before the offer is stored.
	offerBody := map[string]any{"sessionName": src, "recipientUserKey": b.key, "title": title, "prompt": prompt}
	pushFirst := withKey(offerBody, "pushFirst", true)
	rc := handoffRecipients(t, a, src)
	if len(rc.members) != 1 || rc.members[0] != b.key {
		t.Fatalf("handoff candidates = %v, want [%s]", rc.members, b.key)
	}
	requireGate(t, rc, "no_upstream", "") // a never-pushed branch has no upstream and ahead 0
	requirePushOffer(t, rc, "origin", 1)
	requireOfferRefused(t, a, offerBody, "handoff_blocked", "no_upstream") // the push is opt-in
	remoteHead := func() string { return a.shell(aGit, "git ls-remote origin refs/heads/"+branch+" | cut -f1") }

	// An offer the warning would refuse anyway publishes nothing.
	a.shell(aGit, "echo dirty > scratch.txt")
	requireOfferRefused(t, a, pushFirst, "handoff_warning", "uncommitted_changes")
	if got := remoteHead(); got != "" {
		t.Fatalf("origin has %s at %s after a refused offer; nothing may be pushed", branch, got)
	}

	// From a never-pushed branch: pushed, upstream recorded, offer carries the pushed head.
	first := createOffer(t, a, pushFirst)
	head := a.shell(aGit, "git rev-parse HEAD")
	if first.HeadSha != head || remoteHead() != head {
		t.Fatalf("first offer head %s, origin %s, want both at %s", first.HeadSha, remoteHead(), head)
	}
	if got := a.shell(aGit, "git rev-parse --abbrev-ref @{u}"); got != "origin/"+branch {
		t.Fatalf("upstream after the push = %q, want origin/%s", got, branch)
	}
	a.call(http.MethodDelete, "/api/session-handoff-offers/"+first.ID, nil, http.StatusOK) // offer again below

	// With unpushed commits on a tracked branch.
	a.shell(aGit, "echo two >> work.txt && git commit -q -am two")
	rc = handoffRecipients(t, a, src)
	requireGate(t, rc, "unpushed_commits", "uncommitted_changes")
	requirePushOffer(t, rc, "origin", 1)
	requireOfferRefused(t, a, withKey(offerBody, "ackWarning", true), "handoff_blocked", "unpushed_commits")
	head = a.shell(aGit, "git rev-parse HEAD")

	// --- the offer (H-5) ---
	offer := createOffer(t, a, pushFirst)
	if offer.Status != "pending" || offer.Branch != branch || offer.HeadSha != head || offer.RepoRemote != repoURL {
		t.Fatalf("offer = %+v, want pending %s@%s from %s", offer, branch, head, repoURL)
	}
	if got := remoteHead(); got != head {
		t.Fatalf("origin %s = %s after push-then-send, want %s", branch, got, head)
	}
	requireGate(t, handoffRecipients(t, a, src), "", "uncommitted_changes")
	a.stopWorkspace()

	// --- B, workspace stopped, sees the offer and the notification (H-6, H-7) ---
	b.requireStopped("before reading the inbox")
	in := receivedOffers(t, b)
	if len(in) != 1 || in[0].ID != offer.ID || in[0].Prompt != prompt || in[0].OwnerUserKey != a.key || in[0].HeadSha != head {
		t.Fatalf("B's inbox = %+v, want the one offer %s from %s", in, offer.ID, a.key)
	}
	requireNotification(t, b, "handoff-offer", "handoff-offer-"+offer.ID+"-", "offerId", offer.ID)
	if code, body := b.do(http.MethodGet, "/api/shared-sessions", nil); code != 200 || !strings.Contains(string(body), offer.SessionID) {
		t.Fatalf("B's shared sessions (%d) do not list %s: %s", code, offer.SessionID, truncate(body))
	}
	b.requireStopped("after reading the inbox and notifications")

	// --- B starts the workspace and launches from the offer (H-7b, H-8, H-10b) ---
	// The launch takes the existing-branch path (use_existing): it fetches a branch the
	// working copy has never seen (gitx.EnsureBranchRef), and the offered branch was pushed
	// after B cloned. The Console's default new-branch path fetches a missing base the same
	// way (gitx.NewWorktreeStartPoint); its own tests cover it in workspace/agent.
	// The launched session is a shell so its HEAD can be read from inside it; the kind is
	// the recipient's choice in the launch dialog and the offer does not depend on it.
	b.startWorkspace()
	// The Console takes the offer before it creates the session (#2020). While the take
	// holds it the sender cannot withdraw it, so the same work cannot start on both sides.
	token := takeOffer(t, b, offer.ID)
	a.postJSON("/api/session-handoff-offers/"+offer.ID+"/take", map[string]any{}, http.StatusNotFound) // only the recipient takes
	if got := a.call(http.MethodDelete, "/api/session-handoff-offers/"+offer.ID, nil, http.StatusConflict); takeStateOf(got) != "starting" {
		t.Fatalf("withdraw during the recipient's take refused with %v, want handoff_being_taken while starting", got)
	} else if c, _ := apiErr(got); c != "handoff_being_taken" {
		t.Fatalf("withdraw during the recipient's take refused with %q, want handoff_being_taken", c)
	}
	// The give-back (#2058) is the recipient's alone, and only for a launch whose outcome stayed
	// unknown past the take window: a take still starting is not given back.
	a.postJSON("/api/session-handoff-offers/"+offer.ID+"/give-back", map[string]any{"confirmed": true}, http.StatusNotFound)
	if got := b.postJSON("/api/session-handoff-offers/"+offer.ID+"/give-back", map[string]any{"confirmed": true}, http.StatusConflict); takeStateOf(got) != "starting" {
		t.Fatalf("give-back of a starting take answered %v, want handoff_not_unknown while starting", got)
	} else if c, _ := apiErr(got); c != "handoff_not_unknown" {
		t.Fatalf("give-back of a starting take refused with %q, want handoff_not_unknown", c)
	}
	if o := ownedOffer(t, a, offer.ID); o.Status != "pending" || o.TakeState != "starting" {
		t.Fatalf("A's ledger during the take: %s/%q, want pending/starting", o.Status, o.TakeState)
	}
	launched := b.createSession(map[string]any{
		"kind": "shell", "title": in[0].Title, "dir": repoDir,
		"worktree": true, "use_existing": true, "branch": in[0].Branch,
		"handoff_take": map[string]any{"offerId": offer.ID, "token": token}, // the CP confirms it before the Agent is asked
	})
	if got := b.shell(launched, "git rev-parse HEAD"); got != in[0].HeadSha {
		t.Fatalf("launched session HEAD = %s, want the offer's head_sha %s", got, in[0].HeadSha)
	}
	if got := b.shell(launched, "git branch --show-current"); got != branch {
		t.Fatalf("launched session is on %q, want %s", got, branch)
	}
	// The CP saw the Agent create the session, so it has already accepted the offer: a
	// Console that lost that answer does not leave the offer to its next press (#2040). The
	// Console's own accept afterwards is answered as done.
	if o := ownedOffer(t, a, offer.ID); o.Status != "accepted" || o.AcceptedSessionName != launched {
		t.Fatalf("after the create: offer %s is %s/%q, want accepted/%q by the CP", o.ID, o.Status, o.AcceptedSessionName, launched)
	}
	acceptOffer(t, b, offer.ID, launched, token)

	// --- A, workspace stopped, is told; the offer is accepted (H-11) ---
	a.requireStopped("before reading the accept")
	requireNotification(t, a, "handoff-accepted", "handoff-accepted-"+offer.ID+"-", "acceptedSessionName", launched)
	if o := ownedOffer(t, a, offer.ID); o.Status != "accepted" || o.AcceptedSessionName != launched {
		t.Fatalf("A's ledger: offer %s is %s/%q, want accepted/%q", o.ID, o.Status, o.AcceptedSessionName, launched)
	}
	if in := receivedOffers(t, b); len(in) != 0 {
		t.Fatalf("B's inbox still holds %+v after the accept", in)
	}
	a.requireStopped("after reading the accept")

	// --- decline: no notification to A, the offer reads declined (H-12) ---
	a.startWorkspace()
	declined := createOffer(t, a, offerBody)
	if in := receivedOffers(t, b); len(in) != 1 || in[0].ID != declined.ID {
		t.Fatalf("B's inbox = %+v, want offer %s", in, declined.ID)
	}
	if got := b.postJSON("/api/session-handoff-offers/"+declined.ID+"/decline", map[string]any{}, 200); got["status"] != "declined" {
		t.Fatalf("decline answered %v", got)
	}
	if o := ownedOffer(t, a, declined.ID); o.Status != "declined" {
		t.Fatalf("A's ledger: declined offer reads %s", o.Status)
	}
	// A decided offer can no longer be taken, and the refusal says why (#2020).
	if c, _ := apiErr(b.postJSON("/api/session-handoff-offers/"+declined.ID+"/take", map[string]any{}, http.StatusConflict)); c != "handoff_already_decided" {
		t.Fatalf("take of a declined offer refused with %q, want handoff_already_decided", c)
	}
	for _, n := range a.notifications() {
		if p, _ := n["payload"].(map[string]any); p["offerId"] == declined.ID {
			t.Fatalf("A was notified about the declined offer: %v", n)
		}
	}
	if in := receivedOffers(t, b); len(in) != 0 {
		t.Fatalf("B's inbox still holds %+v after the decline", in)
	}

	// --- one pending offer per session (H-15), then unshare expires it (H-16) ---
	unshared := createOffer(t, a, offerBody)
	requireOfferRefused(t, a, map[string]any{
		"sessionName": src, "recipientUserKey": b.key, "title": title, "prompt": prompt, "ackWarning": true,
	}, "handoff_already_pending", "")
	a.call(http.MethodDelete, "/api/session-shares/"+shareID, nil, 200)
	if o := ownedOffer(t, a, unshared.ID); o.Status != "expired" {
		t.Fatalf("A's ledger: offer after unshare reads %s, want expired", o.Status)
	}
	if in := receivedOffers(t, b); len(in) != 0 {
		t.Fatalf("B's inbox still holds %+v after the unshare", in)
	}
}

// --- handoff API helpers ---------------------------------------------------

// handoffOffer is the subset of the offer wire (handoffOfferInboxWire) the scenario reads.
type handoffOffer struct {
	ID                  string `json:"id"`
	SessionID           string `json:"sessionId"`
	SessionName         string `json:"sessionName"`
	Title               string `json:"title"`
	Status              string `json:"status"`
	Branch              string `json:"branch"`
	RepoRemote          string `json:"repoRemote"`
	HeadSha             string `json:"headSha"`
	AcceptedSessionName string `json:"acceptedSessionName"`
	TakeState           string `json:"takeState"`
	OwnerUserKey        string `json:"ownerUserKey"`
	Prompt              string `json:"prompt"`
}

// takeStateOf is the takeState an error answer names ("" when absent).
func takeStateOf(v map[string]any) string {
	e, _ := v["error"].(map[string]any)
	s, _ := e["takeState"].(string)
	return s
}

// recipientsView is GET /api/sessions/{name}/handoff-recipients reduced to the candidates'
// user keys and the push gate's verdict.
type recipientsView struct {
	members          []string
	blocked, warning string
	pushRemote       string
	pushCommits      int
}

func handoffRecipients(t *testing.T, m *member, session string) recipientsView {
	t.Helper()
	var out struct {
		Members []struct {
			UserKey string `json:"userKey"`
		} `json:"members"`
		Context struct {
			Blocked     string `json:"blocked"`
			Warning     string `json:"warning"`
			PushRemote  string `json:"pushRemote"`
			PushCommits int    `json:"pushCommits"`
		} `json:"context"`
	}
	decode(t, m, http.MethodGet, "/api/sessions/"+session+"/handoff-recipients", nil, 200, &out)
	v := recipientsView{blocked: out.Context.Blocked, warning: out.Context.Warning,
		pushRemote: out.Context.PushRemote, pushCommits: out.Context.PushCommits}
	for _, c := range out.Members {
		v.members = append(v.members, c.UserKey)
	}
	return v
}

func requireGate(t *testing.T, v recipientsView, blocked, warning string) {
	t.Helper()
	if v.blocked != blocked || v.warning != warning {
		t.Fatalf("push gate = blocked %q warning %q, want blocked %q warning %q", v.blocked, v.warning, blocked, warning)
	}
}

// requirePushOffer: what the modal's "push, then send" says it will publish.
func requirePushOffer(t *testing.T, v recipientsView, remote string, commits int) {
	t.Helper()
	if v.pushRemote != remote || v.pushCommits != commits {
		t.Fatalf("push offer = %q/%d, want %q/%d", v.pushRemote, v.pushCommits, remote, commits)
	}
}

// withKey is body with one more key, leaving body as it was.
func withKey(body map[string]any, k string, v any) map[string]any {
	out := map[string]any{}
	for bk, bv := range body {
		out[bk] = bv
	}
	out[k] = v
	return out
}

// requireOfferRefused posts an offer and expects a 409 with code (and reason, when set).
func requireOfferRefused(t *testing.T, m *member, body map[string]any, code, reason string) {
	t.Helper()
	got := m.postJSON("/api/session-handoff-offers", body, http.StatusConflict)
	if c, r := apiErr(got); c != code || (reason != "" && r != reason) {
		t.Fatalf("offer refused with %q/%q, want %q/%q", c, r, code, reason)
	}
}

// createOffer sends the offer acknowledging the uncommitted-changes warning, as the
// Console's "send anyway" box does.
func createOffer(t *testing.T, m *member, body map[string]any) handoffOffer {
	t.Helper()
	b := map[string]any{"ackWarning": true}
	for k, v := range body {
		b[k] = v
	}
	var o handoffOffer
	decode(t, m, http.MethodPost, "/api/session-handoff-offers", b, http.StatusCreated, &o)
	if o.ID == "" {
		t.Fatalf("offer created without an id")
	}
	return o
}

// takeOffer and acceptOffer are what the Console does around a launch from an offer
// (console/src/features/sharing/acceptHandoff.ts): take it before the session is created,
// accept it with the take's token once the session exists. They are the one place the
// scenario accepts, so a change to that contract lands here.
func takeOffer(t *testing.T, m *member, offerID string) string {
	t.Helper()
	got := m.postJSON("/api/session-handoff-offers/"+offerID+"/take", map[string]any{}, 200)
	token, _ := got["token"].(string)
	if token == "" {
		t.Fatalf("take answered %v", got)
	}
	return token
}

func acceptOffer(t *testing.T, m *member, offerID, sessionName, token string) {
	t.Helper()
	got := m.postJSON("/api/session-handoff-offers/"+offerID+"/accept", map[string]any{"sessionName": sessionName, "token": token}, 200)
	if got["status"] != "accepted" {
		t.Fatalf("accept answered %v", got)
	}
}

func receivedOffers(t *testing.T, m *member) []handoffOffer {
	t.Helper()
	var out struct {
		Offers []handoffOffer `json:"offers"`
	}
	decode(t, m, http.MethodGet, "/api/session-handoff-offers/received", nil, 200, &out)
	return out.Offers
}

func ownedOffer(t *testing.T, m *member, id string) handoffOffer {
	t.Helper()
	var out struct {
		Offers []handoffOffer `json:"offers"`
	}
	decode(t, m, http.MethodGet, "/api/session-handoff-offers", nil, 200, &out)
	for _, o := range out.Offers {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("%s's ledger has no offer %s: %+v", m.name, id, out.Offers)
	return handoffOffer{}
}

// requireNotification finds a notification of kind whose id starts with idPrefix (the
// event id mixes in the recipient's membership id, which the harness does not know) and
// whose payload[key] is want.
func requireNotification(t *testing.T, m *member, kind, idPrefix, key, want string) {
	t.Helper()
	ns := m.notifications()
	for _, n := range ns {
		id, _ := n["id"].(string)
		p, _ := n["payload"].(map[string]any)
		if n["kind"] == kind && strings.HasPrefix(id, idPrefix) && p[key] == want {
			return
		}
	}
	t.Fatalf("%s has no %s notification (%s…, %s=%s); has %v", m.name, kind, idPrefix, key, want, ns)
}
