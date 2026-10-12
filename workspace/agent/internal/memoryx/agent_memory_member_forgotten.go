package memoryx

// The member's list of forgotten memories (#2003). The changes list is a window of recent
// changes, so a memory forgotten before that window could not be found to restore. The store
// keeps a tombstone per forgotten name under <scope dir>/.forgotten/, and this lists those
// directly. Like the rest of the member routes it is read-only, counts no use and is not gated by
// the switch; a restore still goes through the revert route with its own checks.

import (
	"errors"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/httpx"
)

const (
	// agentMemForgottenMax bounds one answer (and the git work behind it): each row costs a few
	// git calls.
	agentMemForgottenMax = 300
	// agentMemTombMaxSize: a tombstone holds a revision number. Anything bigger is not one.
	agentMemTombMaxSize = 64
)

// agentMemForgottenWire is the forgotten list. Rows are the same view the changes list gives its
// newest row, so Revertible says whether the commit can be restored. Withheld counts names left
// out because the name failed the scan or the newest change of the memory did; they are never
// listed. Truncated says more tombstones exist than one answer holds: narrow the scope.
type agentMemForgottenWire struct {
	Forgotten []agentMemChangeView `json:"forgotten"`
	Withheld  int                  `json:"withheld,omitempty"`
	Truncated bool                 `json:"truncated,omitempty"`
}

// agentMemTombNames lists the names with a tombstone under one scope directory. The directory is
// untrusted: a symlink anywhere on the way, a non-regular or oversized entry, or a name that is
// not a memory name is not a tombstone. bad counts entries refused for their name.
func agentMemTombNames(scopeRel string) (names []string, bad int) {
	dir, err := agentMemCheckDir(scopeRel+"/"+agentMemTombDir, false)
	if err != nil {
		return nil, 0
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0
	}
	for _, d := range ents {
		info, err := d.Info()
		if err != nil || !d.Type().IsRegular() || info.Size() > agentMemTombMaxSize {
			continue
		}
		// The raw name is judged before it is used for anything: it may be a token.
		if agentMemValidName(d.Name()) && agentMemCleanText(d.Name()) {
			names = append(names, d.Name())
		} else {
			bad++
		}
	}
	sort.Strings(names)
	return names, bad
}

// agentMemMemberForgotten lists the forgotten memories of the user scope and every project, or of
// the one scope named. A tombstone counts only while the memory is absent and has a published
// change; the newest change is judged by agentMemMemberLatest's scan, raw and decoded, so a name
// or text that would be withheld there is counted here, never listed.
func agentMemMemberForgotten(scope, project string) (agentMemForgottenWire, error) {
	out := agentMemForgottenWire{Forgotten: []agentMemChangeView{}}
	type src struct{ scope, project, rel string }
	var srcs []src
	switch scope {
	case "":
		srcs = append(srcs, src{agentMemScopeUser, "", "user"})
		if dir, err := agentMemCheckDir("projects", false); err == nil {
			ents, _ := os.ReadDir(dir)
			for _, d := range ents {
				if d.IsDir() && agentMemValidProjectID(d.Name()) {
					srcs = append(srcs, src{agentMemScopeProject, d.Name(), "projects/" + d.Name()})
				}
			}
		}
	default:
		if _, err := agentMemMemberCaller(scope, project); err != nil {
			return out, err
		}
		rel := "user"
		if scope == agentMemScopeProject {
			rel = "projects/" + project
		}
		srcs = append(srcs, src{scope, project, rel})
	}

	agentMemMu.RLock()
	defer agentMemMu.RUnlock()
	if !memoryHasCommits() {
		return out, nil
	}
	for _, s := range srcs {
		names, bad := agentMemTombNames(s.rel)
		out.Withheld += bad
		for _, name := range names {
			if len(out.Forgotten) >= agentMemForgottenMax {
				out.Truncated = true
				break
			}
			v, err := agentMemMemberLatestLocked(s.scope, name, s.rel+"/"+name+".md")
			var ue *memoryUserErr
			switch {
			case err == nil:
			case errors.As(err, &ue) && ue.Status == http.StatusNotFound:
				continue
			case errors.As(err, &ue) && ue.Code == errCodeMemorySecretDetected:
				out.Withheld++
				continue
			default:
				return out, err
			}
			if v.Live {
				continue
			}
			out.Forgotten = append(out.Forgotten, v)
		}
	}
	sort.SliceStable(out.Forgotten, func(i, j int) bool {
		return strings.Compare(out.Forgotten[i].At, out.Forgotten[j].At) > 0
	})
	return out, nil
}

// HandleAgentMemoryMemberForgotten lists the forgotten memories to the Console. Read-only.
func HandleAgentMemoryMemberForgotten(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	out, err := agentMemMemberForgotten(q.Get("scope"), q.Get("project"))
	if err != nil {
		agentMemWriteErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
