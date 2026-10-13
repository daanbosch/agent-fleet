package sessionx

// "Push, then send" for a member handoff (ADR 0057, issue #2072).
//
// When the push gate blocks because the branch is unpushed or was never pushed, the owner
// can ask for the push from the offer modal instead of leaving it to push by hand. The CP
// calls this endpoint on the owner's own click, inside the offer request, and then asks
// handoff-context again: whether the offer may go stays this Agent's verdict.
//
// What is pushed is deliberately narrow: the current branch only, by its full ref name,
// never with force, never tags, never submodules. Uncommitted changes are not touched. The
// workspace's own git setup supplies the credentials (the global credential helper), and the
// repository's hooks run — a pre-push hook that refuses is reported as git printed it.

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/bridge"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/gitx"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/httpx"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

// handoffPushTimeout bounds the push. GIT_TERMINAL_PROMPT=0 stops git asking for a password
// but not a slow host or a slow pre-push hook, and the CP's request to this Agent waits on
// it (agentHTTPClient gives up after two minutes).
const handoffPushTimeout = 60 * time.Second

// handoffPushOutputMax caps the git output relayed to the Console; a chatty pre-push hook
// keeps its last lines, which is where the reason for a refusal is.
const handoffPushOutputMax = 4 << 10

// handoffPushClears reports whether a push can clear this gate reason. A detached HEAD and
// an undecidable state cannot be fixed by pushing and stay blocked.
func handoffPushClears(blocked string) bool {
	return blocked == handoffBlockUnpushed || blocked == handoffBlockNoUpstream
}

// handoffPushTarget is where the current branch would be pushed.
type handoffPushTarget struct {
	branch string
	remote string // a remote name, or a URL when branch.<b>.remote holds one
	dst    string // the full ref on the remote
	// setUpstream: the branch has no upstream configured, so the push records one (-u).
	setUpstream bool
}

var errHandoffNoRemote = errors.New("the branch has no upstream and the working copy has no origin remote")

// handoffPushTargetOf resolves the push target of branch: its configured upstream when it has
// one, otherwise origin under the same name. The upstream is read from config rather than
// @{u} because @{u} fails once the remote-tracking ref is gone, and pushing back to the
// configured branch is exactly what that branch needs.
func handoffPushTargetOf(dir, branch string) (handoffPushTarget, error) {
	if branch == "" || branch == "(detached)" {
		return handoffPushTarget{}, errors.New("HEAD is detached")
	}
	t := handoffPushTarget{branch: branch}
	remote, _ := gitx.Run(dir, "config", "--get", "branch."+branch+".remote")
	merge, _ := gitx.Run(dir, "config", "--get", "branch."+branch+".merge")
	if remote != "" && merge != "" {
		if remote == "." {
			// The upstream is another local branch: a push would move a local ref, which
			// publishes nothing.
			return handoffPushTarget{}, errors.New("the branch's upstream is a local branch, not a remote")
		}
		if !strings.HasPrefix(merge, "refs/heads/") {
			return handoffPushTarget{}, errors.New("the branch's upstream is not a branch: " + merge)
		}
		t.remote, t.dst = remote, merge
		return t, nil
	}
	if _, ok := gitx.GitOriginURL(dir); !ok {
		return handoffPushTarget{}, errHandoffNoRemote
	}
	t.remote, t.dst, t.setUpstream = "origin", "refs/heads/"+branch, true
	return t, nil
}

// commits is how many commits the push would publish. With an upstream that is git's own
// ahead count; without one, the commits no ref of that remote has.
func (t handoffPushTarget) commits(dir string, ahead int) int {
	if !t.setUpstream && ahead > 0 {
		return ahead
	}
	out, err := gitx.Run(dir, "rev-list", "--count", "refs/heads/"+t.branch, "--not", "--remotes="+t.remote)
	if err != nil {
		return ahead
	}
	n, _ := strconv.Atoi(out)
	return n
}

// args is the push command. The refspec names the branch by its full ref and carries no
// '+', so nothing else moves and nothing is forced whatever push.default, remote.*.push or
// push.followTags say.
func (t handoffPushTarget) args() []string {
	a := []string{"push", "--no-follow-tags", "--recurse-submodules=no"}
	if t.setUpstream {
		a = append(a, "--set-upstream")
	}
	return append(a, t.remote, "refs/heads/"+t.branch+":"+t.dst)
}

// urlUserinfoRe matches the userinfo of a URL in free text, so "https://user:token@host/…"
// in git's or a hook's output loses the credential before the Console shows it.
var urlUserinfoRe = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://)[^/\s@]+@`)

// scrubPushOutput makes git's output safe to show: URL credentials go first, then the token
// shapes the chat bridges already scrub. The tail is kept when it is long.
func scrubPushOutput(s string) string {
	s = bridge.ScrubSecrets(urlUserinfoRe.ReplaceAllString(strings.TrimSpace(s), "$1"))
	if len(s) > handoffPushOutputMax {
		s = "…" + s[len(s)-handoffPushOutputMax:]
	}
	return s
}

// handoffPushResult is the answer of a push. Context is handoff-context rebuilt after the
// push, so the caller judges on the state the push left behind.
type handoffPushResult struct {
	Pushed  bool           `json:"pushed"`
	Remote  string         `json:"remote,omitempty"`
	Branch  string         `json:"branch,omitempty"`
	Output  string         `json:"output,omitempty"`
	Context handoffContext `json:"context"`
}

// handoffPushError is a refusal: code is the machine token, message the reason a person
// reads (for a failed push, git's own output).
type handoffPushError struct {
	status        int
	code, message string
}

// pushForHandoff pushes dir's current branch when the gate says a push can clear it. A gate
// that is already open pushes nothing and answers Pushed=false.
func pushForHandoff(ctx context.Context, dir string) (handoffPushResult, *handoffPushError) {
	before := buildHandoffContext(dir)
	switch {
	case before.Vcs != "git":
		return handoffPushResult{}, &handoffPushError{http.StatusConflict, "not_git", "the working copy is not a git repository"}
	case before.Blocked == "":
		return handoffPushResult{Context: before}, nil
	case before.Blocked == handoffBlockDetached:
		return handoffPushResult{}, &handoffPushError{http.StatusConflict, handoffBlockDetached, "HEAD is detached; there is no branch to push"}
	case !handoffPushClears(before.Blocked):
		return handoffPushResult{}, &handoffPushError{http.StatusConflict, before.Blocked, "the working copy is in a state a push cannot fix"}
	}
	t, err := handoffPushTargetOf(dir, before.Branch)
	if err != nil {
		return handoffPushResult{}, &handoffPushError{http.StatusConflict, "no_push_remote", err.Error()}
	}
	pctx, cancel := context.WithTimeout(ctx, handoffPushTimeout)
	defer cancel()
	raw, err := gitx.CmdContext(pctx, dir, t.args()...).CombinedOutput()
	out := scrubPushOutput(string(raw))
	if err != nil {
		if pctx.Err() != nil {
			return handoffPushResult{}, &handoffPushError{http.StatusGatewayTimeout, "push_timeout",
				"the push did not finish within " + handoffPushTimeout.String()}
		}
		if out == "" {
			out = err.Error()
		}
		return handoffPushResult{}, &handoffPushError{http.StatusConflict, "push_rejected", out}
	}
	return handoffPushResult{Pushed: true, Remote: sanitizeRemoteURL(t.remote), Branch: t.branch, Output: out,
		Context: buildHandoffContext(dir)}, nil
}

// HandleSessionHandoffPush — POST /sessions/{name}/handoff-push. Called by the CP only, from
// the owner's offer request; no Console route leads here.
func HandleSessionHandoffPush(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !session.ValidName(name) {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_name", "invalid session name")
		return
	}
	m, ok := session.ReadMeta(name)
	if !ok {
		httpx.WriteErr(w, http.StatusNotFound, "not_found", "no such session: "+name)
		return
	}
	if strings.TrimSpace(m.Dir) == "" {
		httpx.WriteErr(w, http.StatusConflict, "no_working_copy", "session has no working copy")
		return
	}
	res, perr := pushForHandoff(r.Context(), m.Dir)
	if perr != nil {
		httpx.WriteErr(w, perr.status, perr.code, perr.message)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}
