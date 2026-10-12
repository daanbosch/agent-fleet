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
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/gitx"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/projcfg"
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

// agentMemMuseOpen opens <repos>/<dir>/.agents/memory one component at a time with O_NOFOLLOW,
// the working copy included: a symlink under ~/repos could otherwise point outside it. The caller
// reads every file relative to mem, and runs the project mapping and git through wc, so nothing
// is swapped between a check and a use. Both handles are the caller's to close.
func agentMemMuseOpen(dir string) (mem, wc *os.File, ok bool) {
	flags := syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW
	fd, err := syscall.Open(gitx.ReposRoot(), flags&^syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, false
	}
	for i, seg := range append([]string{dir}, strings.Split(agentMemMuseSubdir, "/")...) {
		next, err := syscall.Openat(fd, seg, flags, 0)
		_ = syscall.Close(fd)
		if err != nil {
			if wc != nil {
				wc.Close()
			}
			return nil, nil, false
		}
		fd = next
		if i == 0 {
			// A second descriptor for the working copy: the walk below consumes fd.
			dup, err := syscall.Dup(fd)
			if err != nil {
				_ = syscall.Close(fd)
				return nil, nil, false
			}
			syscall.CloseOnExec(dup)
			wc = os.NewFile(uintptr(dup), "muse-working-copy")
		}
	}
	return os.NewFile(uintptr(fd), "muse-memory"), wc, true
}

// agentMemMuseFdPath names the opened working copy for a child process (chdir into it) and for
// path checks. It follows the descriptor, not the working copy's current path.
func agentMemMuseFdPath(wc *os.File) string {
	return fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), wc.Fd())
}

func agentMemMuseGit(wc *os.File, args ...string) *exec.Cmd {
	cmd := gitx.Cmd("", args...)
	cmd.Dir = agentMemMuseFdPath(wc)
	return cmd
}

// agentMemMuseTracked returns the file names git tracks under .agents/memory of a working copy.
// A directory that is not a git repository tracks nothing. When git cannot say, every file counts
// as tracked: the warning errs toward "shared".
func agentMemMuseTracked(wc *os.File, files []string) map[string]bool {
	out := map[string]bool{}
	if projcfg.DetectVCS(agentMemMuseFdPath(wc)) != projcfg.VCSGit {
		return out
	}
	// The repository's own config is the member's, but a cloned repo's is not: core.fsmonitor runs
	// an arbitrary command for ls-files, so it is forced off (measured: a marker script ran on
	// every preview without it).
	b, err := agentMemMuseGit(wc, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null",
		"ls-files", "-z", "--", agentMemMuseSubdir).Output()
	if err != nil {
		for _, f := range files {
			out[f] = true
		}
		return out
	}
	for _, p := range strings.Split(string(b), "\x00") {
		if n, ok := strings.CutPrefix(p, agentMemMuseSubdir+"/"); ok {
			out[n] = true
		}
	}
	return out
}

// agentMemMuseProject maps the opened working copy to its AF project; reason says why it has
// none. The directory name only labels it: git answers about the descriptor's own directory.
func agentMemMuseProject(dir string, wc *os.File) (*agentMemProject, string) {
	p := agentMemProjectWith(filepath.Join(gitx.ReposRoot(), dir), agentMemMuseFdPath(wc),
		func(args ...string) (string, error) {
			out, err := agentMemMuseGit(wc, args...).Output()
			return strings.TrimSpace(string(out)), err
		})
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
		mem, wc, ok := agentMemMuseOpen(d.Name())
		if !ok {
			continue
		}
		files, _ := agentMemImportFiles(mem)
		mem.Close()
		if len(files) == 0 || !agentMemMuseDirOK(d.Name()) {
			wc.Close()
			if len(files) > 0 {
				out.Withheld++
			}
			continue
		}
		s := agentMemMuseSource{Dir: d.Name(), Count: len(files)}
		for f := range agentMemMuseTracked(wc, files) {
			if strings.HasSuffix(f, ".md") && f != agentMemImportIndex && !strings.Contains(f, "/") {
				s.Shared++
			}
		}
		s.Project, s.Reason = agentMemMuseProject(d.Name(), wc)
		wc.Close()
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
	mem, wc, ok := agentMemMuseOpen(dir)
	if !ok {
		return nil, memoryErrf(http.StatusNotFound, errCodeMemoryNotFound, "no muse project memory in that working copy")
	}
	defer mem.Close()
	defer wc.Close()
	pv := &agentMemMusePreview{Dir: dir, Items: []agentMemImportItem{}, Counts: map[string]int{},
		byName: map[string]*agentMemImportItem{}}
	pv.Project, pv.Reason = agentMemMuseProject(dir, wc)
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
	tracked := agentMemMuseTracked(wc, files)
	gone, err := agentMemMuseForgottenHashes(rel)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		src := agentMemImportSrc{Source: agentMemMuseSourcePrefix + "project/" + agentMemMuseSubdir + "/" + f,
			Muse: true, Shared: tracked[f], ForgottenHashes: gone}
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

// agentMemMuseForgottenHashes returns the source hashes this scope ever stored for a name that is
// no longer live (forgotten, or its import reverted). A forgotten memory is matched by name in
// agentMemImportHistory; this also catches the same file renamed in muse, whose bytes are
// unchanged. A file edited as well as renamed is a different text and is not caught.
func agentMemMuseForgottenHashes(rel string) (map[string]bool, error) {
	out := map[string]bool{}
	if !memoryHasCommits() {
		return out, nil
	}
	log, err := memoryGitRun("log", memoryBranch, "--no-renames", "-p", "--format=", "-U0",
		"--", agentMemRepoPrefix+"/"+rel)
	if err != nil {
		return nil, err
	}
	prefix := agentMemRepoPrefix + "/" + rel + "/"
	byName := map[string][]string{}
	name := ""
	for _, line := range strings.Split(log, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ b/"), strings.HasPrefix(line, "--- a/"):
			name = ""
			if n, ok := strings.CutPrefix(line[6:], prefix); ok {
				if n, ok = strings.CutSuffix(n, ".md"); ok && agentMemValidName(n) {
					name = n
				}
			}
		case name != "" && (strings.HasPrefix(line, "+source_hash:") || strings.HasPrefix(line, "-source_hash:")):
			h := strings.Trim(strings.TrimSpace(line[len("+source_hash:"):]), `"`)
			byName[name] = append(byName[name], h)
		}
	}
	dir, err := agentMemCheckDir(rel, false)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for n, hs := range byName {
		if err == nil {
			if _, serr := os.Stat(filepath.Join(dir, n+".md")); serr == nil {
				continue
			}
		}
		for _, h := range hs {
			if h != "" {
				out[h] = true
			}
		}
	}
	return out, nil
}
