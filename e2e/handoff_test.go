//go:build e2e

// Member handoff (ADR 0057, docs/log/77 §77.5–§77.12) between two real workspaces: the
// automated part of #1960's two-account run sheet (H-1..H-16).
//
// The session handed over is kind=claude, because only kinds with a transcript enter the
// shared-session catalog, and a share or an offer needs a catalog row (a shell session is
// refused with share_target_not_found). It is never driven, so no LLM credential is
// needed: the git work happens in a shell session in the same working copy, and the
// handoff proposal is posted to the session the way propose_session_handoff does.
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

	// --- the push gate (H-3, H-4) ---
	offerBody := map[string]any{"sessionName": src, "recipientUserKey": b.key, "title": title, "prompt": prompt}
	rc := handoffRecipients(t, a, src)
	if len(rc.members) != 1 || rc.members[0] != b.key {
		t.Fatalf("handoff candidates = %v, want [%s]", rc.members, b.key)
	}
	requireGate(t, rc, "no_upstream", "") // a never-pushed branch has no upstream and ahead 0
	requireOfferRefused(t, a, offerBody, "handoff_blocked", "no_upstream")

	a.shell(aGit, "git push -q -u origin HEAD")
	requireGate(t, handoffRecipients(t, a, src), "", "")
	a.shell(aGit, "echo two >> work.txt && git commit -q -am two")
	requireGate(t, handoffRecipients(t, a, src), "unpushed_commits", "")
	requireOfferRefused(t, a, offerBody, "handoff_blocked", "unpushed_commits")
	a.shell(aGit, "git push -q")
	head := a.shell(aGit, "git rev-parse HEAD")
	a.shell(aGit, "echo dirty >> work.txt")
	requireGate(t, handoffRecipients(t, a, src), "", "uncommitted_changes")
	requireOfferRefused(t, a, offerBody, "handoff_warning", "uncommitted_changes")

	// --- the offer (H-5) ---
	offer := createOffer(t, a, offerBody)
	if offer.Status != "pending" || offer.Branch != branch || offer.HeadSha != head || offer.RepoRemote != repoURL {
		t.Fatalf("offer = %+v, want pending %s@%s from %s", offer, branch, head, repoURL)
	}
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
	// after B cloned. The Console's default new-branch-off-the-offered-branch path does not
	// fetch a missing base yet (#2019), so this scenario does not use it.
	// The launched session is a shell so its HEAD can be read from inside it; the kind is
	// the recipient's choice in the launch dialog and the offer does not depend on it.
	b.startWorkspace()
	launched := b.createSession(map[string]any{
		"kind": "shell", "title": in[0].Title, "dir": repoDir,
		"worktree": true, "use_existing": true, "branch": in[0].Branch,
	})
	if got := b.shell(launched, "git rev-parse HEAD"); got != in[0].HeadSha {
		t.Fatalf("launched session HEAD = %s, want the offer's head_sha %s", got, in[0].HeadSha)
	}
	if got := b.shell(launched, "git branch --show-current"); got != branch {
		t.Fatalf("launched session is on %q, want %s", got, branch)
	}
	acceptOffer(t, b, offer.ID, launched)

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
	OwnerUserKey        string `json:"ownerUserKey"`
	Prompt              string `json:"prompt"`
}

// recipientsView is GET /api/sessions/{name}/handoff-recipients reduced to the candidates'
// user keys and the push gate's verdict.
type recipientsView struct {
	members          []string
	blocked, warning string
}

func handoffRecipients(t *testing.T, m *member, session string) recipientsView {
	t.Helper()
	var out struct {
		Members []struct {
			UserKey string `json:"userKey"`
		} `json:"members"`
		Context struct {
			Blocked string `json:"blocked"`
			Warning string `json:"warning"`
		} `json:"context"`
	}
	decode(t, m, http.MethodGet, "/api/sessions/"+session+"/handoff-recipients", nil, 200, &out)
	v := recipientsView{blocked: out.Context.Blocked, warning: out.Context.Warning}
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

// acceptOffer reports the launch to the CP, which is what the Console does once the
// launch from the offer succeeded (console/src/features/sharing/acceptHandoff.ts). It is
// the one place the scenario accepts, so a change to that contract (#2020) lands here.
func acceptOffer(t *testing.T, m *member, offerID, sessionName string) {
	t.Helper()
	got := m.postJSON("/api/session-handoff-offers/"+offerID+"/accept", map[string]any{"sessionName": sessionName}, 200)
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
