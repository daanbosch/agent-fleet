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

// agentMemMuseOpen opens <repos>/<dir>/.agents/memory one component at a time with O_NOFOLLOW,
// the working copy included: a symlink under ~/repos could otherwise point outside it. The caller
// reads every file relative to the returned handle so nothing is swapped between check and read.
// wc is the working copy's identity at open time, for agentMemMuseSame.
func agentMemMuseOpen(dir string) (mem *os.File, wc museWC, ok bool) {
	flags := syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW
	fd, err := syscall.Open(gitx.ReposRoot(), flags&^syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, wc, false
	}
	for i, seg := range append([]string{dir}, strings.Split(agentMemMuseSubdir, "/")...) {
		next, err := syscall.Openat(fd, seg, flags, 0)
		_ = syscall.Close(fd)
		if err != nil {
			return nil, wc, false
		}
		fd = next
		if i == 0 {
			var st syscall.Stat_t
			if syscall.Fstat(fd, &st) != nil {
				_ = syscall.Close(fd)
				return nil, wc, false
			}
			wc = museWC{uint64(st.Dev), uint64(st.Ino)}
		}
	}
	return os.NewFile(uintptr(fd), "muse-memory"), wc, true
}

// museWC is a working copy's device and inode.
type museWC struct{ dev, ino uint64 }

// agentMemMuseSame says whether the path still names the working copy that was opened. The
// project mapping and the git query below work on the path, so a swap in between would pair the
// pinned files with another copy's project.
func agentMemMuseSame(dir string, wc museWC) bool {
	cur, err := os.Lstat(filepath.Join(gitx.ReposRoot(), dir))
	if err != nil {
		return false
	}
	st, ok := cur.Sys().(*syscall.Stat_t)
	return ok && uint64(st.Dev) == wc.dev && uint64(st.Ino) == wc.ino
}

// agentMemMuseTracked returns the file names git tracks under .agents/memory of a working copy.
// A directory that is not a git repository tracks nothing. When git cannot say, every file counts as tracked: the warning errs toward "shared".
func agentMemMuseTracked(dir string, files []string) map[string]bool {
	out := map[string]bool{}
	if !gitx.IsGitRepo(filepath.Join(gitx.ReposRoot(), dir)) {
		return out
	}
	// The repository's own config is the member's, but a cloned repo's is not: core.fsmonitor runs
	// an arbitrary command for ls-files, so it is forced off (measured: a marker script ran on
	// every preview without it).
	b, err := gitx.Cmd(filepath.Join(gitx.ReposRoot(), dir), "-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=/dev/null", "ls-files", "-z", "--", agentMemMuseSubdir).Output()
	raw := string(b)
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
		mem, _, ok := agentMemMuseOpen(d.Name())
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
	mem, wc, ok := agentMemMuseOpen(dir)
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
	gone, err := agentMemMuseForgottenHashes(rel)
	if err != nil {
		return nil, err
	}
	if !agentMemMuseSame(dir, wc) {
		return nil, memoryErrf(http.StatusConflict, errCodeMemoryConflict, "the working copy changed while it was read; preview it again")
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
