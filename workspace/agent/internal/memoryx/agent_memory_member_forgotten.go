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
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/httpx"
)

// agentMemForgottenMax bounds the rows of one answer (a variable so a test can use a small one).
var agentMemForgottenMax = 300

const (
	// agentMemTombScanMax bounds how many tombstones one request judges, since each judged
	// memory costs a few git calls. Past it the answer says truncated.
	agentMemTombScanMax = 2000
	// agentMemTombMaxSize: a tombstone holds a revision number. Anything bigger is not one.
	agentMemTombMaxSize = 64
)

// agentMemForgottenWire is the forgotten list, newest forgotten first. Rows are the same view the
// changes list gives its newest row, so Revertible says whether the commit can be restored.
// Withheld counts names left out because the name failed the scan or the newest change of the
// memory did; they are never listed. Truncated says memories that could be listed were left out
// (more than one answer holds, or more tombstones than one request judges): narrow the scope.
// Projects names every project that has a listable forgotten memory, cut or not, so the Console can
// keep offering a scope whose rows did not fit.
type agentMemForgottenWire struct {
	Forgotten []agentMemChangeView `json:"forgotten"`
	Projects  []*agentMemProject   `json:"projects"`
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

// agentMemTombCand is one tombstone with the newest change of its memory, found in a single pass
// over the history rather than one search per memory.
type agentMemTombCand struct {
	scope, project, name, rel string
	commit                    string
	at                        time.Time
}

// agentMemNewestCommits returns, for each wanted "af/..." path, the newest agent-memory commit
// that names it with its author time, deciding "newest" by the trailer parsing of
// agentMemLatestChange. A commit whose time does not parse sorts as the zero time.
func agentMemNewestCommits(want map[string]bool) (map[string]agentMemTombCand, error) {
	out := map[string]agentMemTombCand{}
	if len(want) == 0 {
		return out, nil
	}
	log, err := memoryGitRun("log", memoryBranch, "-F", "--grep=AF-Trigger: "+memoryTriggerAgentMemory,
		"--format=%H"+memoryFldSep+"%aI"+memoryFldSep+"%B"+memoryRecSep)
	if err != nil {
		return nil, err
	}
	for _, rec := range strings.Split(log, memoryRecSep) {
		f := strings.SplitN(strings.TrimSpace(rec), memoryFldSep, 3)
		if len(f) < 3 {
			continue
		}
		tr := agentMemTrailers(f[2])
		path := tr["AF-Memory"]
		if tr["AF-Trigger"] != memoryTriggerAgentMemory || !want[path] {
			continue
		}
		if _, seen := out[path]; seen {
			continue
		}
		at, _ := time.Parse(time.RFC3339, f[1])
		out[path] = agentMemTombCand{commit: f[0], at: at}
	}
	return out, nil
}

// agentMemMemberForgotten lists the forgotten memories of the user scope and every project, or of
// the one scope named, newest forgotten first. Every tombstone is judged before the newest rows
// are chosen, so the cut never drops a newer memory for an older one. A tombstone counts only
// while the memory is absent and has a published change; the newest change is judged by
// agentMemMemberLatest's scan, raw and decoded, so a name or text that would be withheld there is
// counted here, never listed. agentMemMu is taken per memory, not across the request.
func agentMemMemberForgotten(scope, project string) (agentMemForgottenWire, error) {
	out := agentMemForgottenWire{Forgotten: []agentMemChangeView{}, Projects: []*agentMemProject{}}
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

	var cands []agentMemTombCand
	want := map[string]bool{}
	for _, s := range srcs {
		names, bad := agentMemTombNames(s.rel)
		out.Withheld += bad
		for _, name := range names {
			if len(cands) >= agentMemTombScanMax {
				out.Truncated = true
				break
			}
			cands = append(cands, agentMemTombCand{scope: s.scope, project: s.project, name: name, rel: s.rel + "/" + name + ".md"})
			want[agentMemRepoPrefix+"/"+s.rel+"/"+name+".md"] = true
		}
	}
	agentMemMu.RLock()
	var newest map[string]agentMemTombCand
	var err error
	if memoryHasCommits() {
		newest, err = agentMemNewestCommits(want)
	}
	agentMemMu.RUnlock()
	if err != nil {
		return out, err
	}
	found := cands[:0]
	for _, c := range cands {
		if n, ok := newest[agentMemRepoPrefix+"/"+c.rel]; ok {
			c.commit, c.at = n.commit, n.at
			found = append(found, c)
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].at.After(found[j].at) })

	projects := map[string]bool{}
	for _, c := range found {
		agentMemMu.RLock()
		v, err := agentMemMemberChangeAt(c.scope, c.name, c.rel, c.commit)
		agentMemMu.RUnlock()
		var ue *memoryUserErr
		switch {
		case err == nil:
		case errors.As(err, &ue) && ue.Code == errCodeMemorySecretDetected:
			out.Withheld++
			continue
		default:
			return out, err
		}
		if v.Live {
			continue
		}
		if c.project != "" && !projects[c.project] {
			projects[c.project] = true
			out.Projects = append(out.Projects, agentMemProjectInfo(c.project))
		}
		if len(out.Forgotten) >= agentMemForgottenMax {
			out.Truncated = true
			continue
		}
		out.Forgotten = append(out.Forgotten, v)
	}
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
