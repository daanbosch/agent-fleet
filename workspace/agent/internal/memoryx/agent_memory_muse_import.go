package memoryx

// One-time import of muse's project-scope native memory into AF memory (ADR 0108 decision 6,
// #1991). It runs the claude import's evaluation (agent_memory_claude_import.go): preview first,
// secret hits skipped, the forgotten never brought back, the member as the commit's author and
// the author of the memory unknown.
//
// Only the `project` scope is read: <working copy>/.agents/memory, and it lands in that working
// copy's AF project. The `personal` and `personal_project` stores are not read because their
// location is not established (ADR 0108, note of 2026-10-12). No scope is ever written into AF's
// user scope: a muse note is a project note, and a user-scope memory reaches every project.
//
// .agents/memory is inside the repository and a team may commit it. Every item says whether git
// tracks it, so the member sees that the text may not be theirs alone before it is copied.
// MEMORY.md is muse's index and is not a memory. Subdirectories are not entered.

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/gitx"
)

const (
	// agentMemMuseSourcePrefix marks a memory imported from muse; a muse update only replaces a
	// copy that carries it.
	agentMemMuseSourcePrefix = "muse:"
	// agentMemMuseSubdir is muse's project-scope store, relative to a working copy.
	agentMemMuseSubdir = ".agents/memory"

	// museImportDiverged: AF has its own version (Reason says why) and muse's file is not applied.
	museImportDiverged = "diverged"
)

// agentMemMuseSource is one working copy that has muse project memory.
type agentMemMuseSource struct {
	Dir   string `json:"dir"`
	Count int    `json:"count"`
	// Shared counts the files git tracks in that working copy.
	Shared int `json:"shared"`
	// Project is where the files would go; nil when Reason says why not.
	Project *agentMemProject `json:"project,omitempty"`
	Reason  string           `json:"reason,omitempty"`
}

type agentMemMuseSources struct {
	Sources  []agentMemMuseSource `json:"sources"`
	Withheld int                  `json:"withheld,omitempty"`
}

type agentMemMusePreview struct {
	Dir      string               `json:"dir"`
	Project  *agentMemProject     `json:"project,omitempty"`
	Reason   string               `json:"reason,omitempty"`
	Items    []agentMemImportItem `json:"items"`
	Counts   map[string]int       `json:"counts"`
	Withheld int                  `json:"withheld,omitempty"`
	Truncate bool                 `json:"truncated,omitempty"`
	byName   map[string]*agentMemImportItem
}

// agentMemMuseDirOK says whether a name may be taken for a working copy under ~/repos.
func agentMemMuseDirOK(dir string) bool {
	return dir != "" && dir != "." && dir != ".." && !strings.ContainsAny(dir, "/\\\x00") &&
		agentMemImportShowable(dir)
}

// agentMemMuseOpen opens <repos>/<dir>/.agents/memory. The working copy itself may be a symlink
// (the member's own layout); `.agents` and `memory` may not, and the caller reads every file
// relative to the returned handle so nothing is swapped between the check and the read.
func agentMemMuseOpen(dir string) (*os.File, bool) {
	flags := syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_CLOEXEC
	fd, err := syscall.Open(filepath.Join(gitx.ReposRoot(), dir), flags, 0)
	if err != nil {
		return nil, false
	}
	for _, seg := range strings.Split(agentMemMuseSubdir, "/") {
		next, err := syscall.Openat(fd, seg, flags|syscall.O_NOFOLLOW, 0)
		_ = syscall.Close(fd)
		if err != nil {
			return nil, false
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), "muse-memory"), true
}

// agentMemMuseTracked returns the file names git tracks under .agents/memory of a working copy.
// A directory that is not a git repository tracks nothing. When git cannot say, every file counts as tracked: the warning errs toward "shared".
func agentMemMuseTracked(dir string, files []string) map[string]bool {
	out := map[string]bool{}
	if !gitx.IsGitRepo(filepath.Join(gitx.ReposRoot(), dir)) {
		return out
	}
	raw, err := gitx.Run(filepath.Join(gitx.ReposRoot(), dir), "ls-files", "-z", "--", agentMemMuseSubdir)
	if err != nil {
		for _, f := range files {
			out[f] = true
		}
		return out
	}
	for _, p := range strings.Split(raw, "\x00") {
		if n, ok := strings.CutPrefix(p, agentMemMuseSubdir+"/"); ok {
			out[n] = true
		}
	}
	return out
}

// agentMemMuseProject maps a working copy to its AF project; reason says why it has none.
func agentMemMuseProject(dir string) (*agentMemProject, string) {
	p := agentMemProjectFor(filepath.Join(gitx.ReposRoot(), dir))
	switch {
	case p == nil:
		return nil, claudeImportNoProject
	case !agentMemCleanText(p.Root + "\n" + p.Display + "\n" + p.ID):
		return nil, claudeImportNoProject
	}
	return p, ""
}

// agentMemMuseList returns the working copies under ~/repos that hold muse project memory.
func agentMemMuseList() agentMemMuseSources {
	out := agentMemMuseSources{Sources: []agentMemMuseSource{}}
	ents, err := os.ReadDir(gitx.ReposRoot())
	if err != nil {
		return out
	}
	for _, d := range ents {
		mem, ok := agentMemMuseOpen(d.Name())
		if !ok {
			continue
		}
		files, _ := agentMemImportFiles(mem)
		mem.Close()
		if len(files) == 0 {
			continue
		}
		if !agentMemMuseDirOK(d.Name()) {
			out.Withheld++
			continue
		}
		s := agentMemMuseSource{Dir: d.Name(), Count: len(files)}
		for f := range agentMemMuseTracked(d.Name(), files) {
			if strings.HasSuffix(f, ".md") && f != agentMemImportIndex && !strings.Contains(f, "/") {
				s.Shared++
			}
		}
		s.Project, s.Reason = agentMemMuseProject(d.Name())
		out.Sources = append(out.Sources, s)
	}
	sort.Slice(out.Sources, func(i, j int) bool { return out.Sources[i].Dir < out.Sources[j].Dir })
	return out
}

// agentMemMuseBuild evaluates every file of one working copy's muse memory. Callers hold agentMemMu.
func agentMemMuseBuild(dir string) (*agentMemMusePreview, error) {
	// The name is never echoed: a directory name can be a token, and the list withholds those.
	if !agentMemMuseDirOK(dir) {
		return nil, memoryErrf(http.StatusBadRequest, errCodeMemoryBadRequest, "dir is not a working copy name")
	}
	mem, ok := agentMemMuseOpen(dir)
	if !ok {
		return nil, memoryErrf(http.StatusNotFound, errCodeMemoryNotFound, "no muse project memory in that working copy")
	}
	defer mem.Close()
	pv := &agentMemMusePreview{Dir: dir, Items: []agentMemImportItem{}, Counts: map[string]int{},
		byName: map[string]*agentMemImportItem{}}
	pv.Project, pv.Reason = agentMemMuseProject(dir)
	if pv.Project == nil {
		return pv, nil
	}
	rel := "projects/" + pv.Project.ID
	history, err := agentMemImportHistory(rel)
	if err != nil {
		return nil, err
	}
	files, trunc := agentMemImportFiles(mem)
	pv.Truncate = trunc
	tracked := agentMemMuseTracked(dir, files)
	for _, f := range files {
		src := agentMemImportSrc{Source: agentMemMuseSourcePrefix + "project/" + agentMemMuseSubdir + "/" + f,
			Muse: true, Shared: tracked[f]}
		it, withheld := agentMemImportEvaluateSrc(mem, f, src, rel, history)
		if withheld {
			pv.Withheld++
			continue
		}
		pv.Counts[it.Status]++
		pv.Items = append(pv.Items, it)
	}
	sort.SliceStable(pv.Items, func(i, j int) bool {
		a, b := pv.Items[i], pv.Items[j]
		if claudeImportOrder[a.Status] != claudeImportOrder[b.Status] {
			return claudeImportOrder[a.Status] < claudeImportOrder[b.Status]
		}
		return a.Name < b.Name
	})
	for i := range pv.Items {
		pv.byName[pv.Items[i].Name] = &pv.Items[i]
	}
	return pv, nil
}

// agentMemMusePreviewFor is the read-only preview; it works with the switch off.
func agentMemMusePreviewFor(dir string) (*agentMemMusePreview, error) {
	agentMemMu.RLock()
	defer agentMemMu.RUnlock()
	return agentMemMuseBuild(dir)
}

type agentMemMuseReq struct {
	Project string                  `json:"project"`
	Dir     string                  `json:"dir"`
	Items   []agentMemImportReqItem `json:"items"`
}

// agentMemMuseApply imports the listed items, one commit each. The caller has checked the switch.
func agentMemMuseApply(req agentMemMuseReq, now time.Time) (agentMemImportApplied, error) {
	if len(req.Items) == 0 || len(req.Items) > agentMemImportMaxItems {
		return agentMemImportApplied{}, memoryErrf(http.StatusBadRequest, errCodeMemoryBadRequest,
			"send between 1 and %d items per request", agentMemImportMaxItems)
	}
	agentMemMu.Lock()
	defer agentMemMu.Unlock()
	memorySnapshotMu.Lock()
	defer memorySnapshotMu.Unlock()

	pv, err := agentMemMuseBuild(req.Dir)
	if err != nil {
		return agentMemImportApplied{}, err
	}
	if pv.Project == nil {
		return agentMemImportApplied{}, memoryErrf(http.StatusConflict, errCodeMemoryConflict,
			"this working copy's muse memory cannot be imported (%s)", pv.Reason)
	}
	if pv.Project.ID != req.Project {
		return agentMemImportApplied{}, memoryErrf(http.StatusConflict, errCodeMemoryConflict,
			"the project this working copy maps to has changed; preview it again")
	}
	return agentMemImportApplyItems(pv.Project, pv.byName, req.Items, now), nil
}
