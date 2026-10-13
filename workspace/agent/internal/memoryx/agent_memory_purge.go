package memoryx

// AF-owned agent memory (ADR 0108, open question 3) — the member's purge of one memory's past
// revisions from the 0022 history (#1982).
//
// memory_forget only hides a memory; its text stays in af-memory.git and in every bundle
// exported from it. The purge rewrites the history so it does not:
//
//   - The file af/<scope dir>/<name>.md is removed from every commit on every ref (main, the
//     refs/imports lineages, the refs/premigrate rescue refs). The one exception is the current
//     revision: the commits at the tip of main that already hold the tip's text keep it, so the
//     published state and the history still agree. A leak in the current text is purged by
//     saving the memory again first, which makes the leaking revision a past one.
//   - The memory's own change commits (AF-Memory: <path>) go with it, so the changes list has no
//     row for a revision that no longer exists. The newest kept change is marked
//     AF-Purged-Before, and a revert of it is refused: what it would restore is gone.
//   - Commits that become empty are dropped, the others re-linked; the refs move in one
//     transaction. Then the index and the reflogs are reset and the repo is gc'd with
//     --prune=now, and each purged blob is checked to be gone from disk.
//
// A bundle exported earlier is a copy outside this repo and cannot be recalled; the Console and
// the guide say so. The value is never read into a response or a log: the preview reports counts,
// commit ids and the scanner's masked findings (rule, path, line, hint) only.
//
// It holds agentMemMu and memorySnapshotMu for its whole run, like a write. A crash before the
// refs move leaves unreachable objects and nothing else; after, the marker file makes the next
// purge (or the next start of the snapshot loop) finish the gc.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// agentMemPurgedTrailer marks the first kept change of a memory whose earlier revisions were
// purged. agentMemRevert refuses an undo of it.
const agentMemPurgedTrailer = "AF-Purged-Before"

const (
	agentMemPurgeListMax     = 200
	agentMemPurgeFindingsMax = 20
)

// agentMemPurgeFault lets a test stop the purge between its steps and see what the repo looks
// like. Always nil outside tests.
var agentMemPurgeFault func(stage string) error

func agentMemPurgeStage(stage string) error {
	if agentMemPurgeFault != nil {
		return agentMemPurgeFault(stage)
	}
	return nil
}

func agentMemPurgePendingPath() string { return filepath.Join(memoryWorkDir(), "purge-gc.pending") }

type agentMemPurgeRef struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"` // main | branch | import | rescue
	Affected int    `json:"affected"`
	sha      string
}

type agentMemPurgeChange struct {
	Commit     string `json:"commit"`
	At         string `json:"at"`
	Op         string `json:"op"`
	AuthorKind string `json:"authorKind"`
}

// agentMemPurgePreviewWire is what the confirmation shows. It holds no memory text.
type agentMemPurgePreviewWire struct {
	Scope   string           `json:"scope"`
	Project *agentMemProject `json:"project,omitempty"`
	Name    string           `json:"name"`
	Live    bool             `json:"live"`
	// KeepsCurrent: the current revision stays in the history (the memory is live).
	KeepsCurrent bool `json:"keepsCurrent"`
	// Nothing: no past revision exists, so there is nothing to purge.
	Nothing bool `json:"nothing"`
	// Changes are the memory's change commits that will be removed, newest first (capped);
	// ChangesTotal counts them all.
	Changes      []agentMemPurgeChange `json:"changes"`
	ChangesTotal int                   `json:"changesTotal"`
	// Snapshots counts the other commits whose tree carries a past revision and will be rewritten.
	Snapshots int                `json:"snapshots"`
	Refs      []agentMemPurgeRef `json:"refs"`
	// Findings are what the secret scanner reports in the past revisions: rule, path, line and a
	// masked hint, never the value.
	Findings []memorySecretFinding `json:"findings"`
	// Plan binds a purge to this preview: the history must be as it was when the member looked.
	Plan string `json:"plan"`
}

type agentMemPurgeReq struct {
	Scope   string `json:"scope"`
	Project string `json:"project"`
	Name    string `json:"name"`
	Plan    string `json:"plan"`
	// Confirm is the memory's name typed again.
	Confirm string `json:"confirm"`
}

type agentMemPurgeResult struct {
	Scope            string `json:"scope"`
	Name             string `json:"name"`
	KeptCurrent      bool   `json:"keptCurrent"`
	CommitsDropped   int    `json:"commitsDropped"`
	CommitsRewritten int    `json:"commitsRewritten"`
	RefsRewritten    int    `json:"refsRewritten"`
	RefsDeleted      int    `json:"refsDeleted"`
	BlobsRemoved     int    `json:"blobsRemoved"`
	// BlobsShared counts purged texts that are still in the repo because another path holds the
	// same bytes; they are not this memory's history.
	BlobsShared int `json:"blobsShared"`
}

// agentMemPurgeScan is the history as the purge sees it.
type agentMemPurgeScan struct {
	path     string // af/<scope dir>/<name>.md
	comps    []string
	refs     []agentMemPurgeRef
	order    []string // every commit, parents first
	parents  map[string][]string
	blob     map[string]string // commit -> blob id at path ("" = absent)
	own      map[string]agentMemPurgeChange
	keep     map[string]bool
	keepBlob string
	purged   []string // blob ids of past revisions, deduplicated
	// markPurged is the kept change that gets AF-Purged-Before when something is removed.
	markPurged string
	plan       string
}

func agentMemPurgeRel(scope, project, name string) (rel string, err error) {
	if err := agentMemCheckName(name); err != nil {
		return "", err
	}
	switch scope {
	case agentMemScopeUser:
		return "user/" + name + ".md", nil
	case agentMemScopeProject:
		if !agentMemValidProjectID(project) {
			return "", memoryErrf(http.StatusBadRequest, errCodeMemoryBadRequest, "project is not a project id")
		}
		return "projects/" + project + "/" + name + ".md", nil
	}
	return "", memoryErrf(http.StatusBadRequest, errCodeMemoryBadRequest, "scope must be %q or %q", agentMemScopeUser, agentMemScopeProject)
}

func agentMemPurgeRefKind(name string) string {
	switch {
	case name == "refs/heads/"+memoryBranch:
		return "main"
	case strings.HasPrefix(name, "refs/heads/"):
		return "branch"
	case strings.HasPrefix(name, "refs/imports/"):
		return "import"
	case strings.HasPrefix(name, "refs/premigrate/"):
		return "rescue"
	}
	return ""
}

// agentMemPurgeScanHistory reads the refs, the commit graph and the memory's file at every commit.
// Callers hold agentMemMu.
func agentMemPurgeScanHistory(rel string) (*agentMemPurgeScan, error) {
	s := &agentMemPurgeScan{
		path: agentMemRepoPrefix + "/" + rel, parents: map[string][]string{}, blob: map[string]string{},
		own: map[string]agentMemPurgeChange{}, keep: map[string]bool{},
	}
	s.comps = strings.Split(s.path, "/")
	out, err := memoryGitRun("for-each-ref", "--format=%(objectname) %(objecttype) %(refname)")
	if err != nil {
		return nil, err
	}
	var planLines []string
	mainTip := ""
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		kind := agentMemPurgeRefKind(f[2])
		if kind == "" || f[1] != "commit" {
			// Agent Fleet makes no other refs. Leaving one the purge cannot rewrite would keep
			// the text reachable, so it is refused rather than skipped.
			return nil, memoryErrf(http.StatusConflict, errCodeMemoryConflict,
				"the memory repo holds a ref this purge does not rewrite; it was not changed")
		}
		s.refs = append(s.refs, agentMemPurgeRef{Name: f[2], Kind: kind, sha: f[0]})
		planLines = append(planLines, f[0]+" "+f[2])
		if kind == "main" {
			mainTip = f[0]
		}
	}
	sort.Strings(planLines)
	sum := sha256.Sum256([]byte(s.path + "\n" + strings.Join(planLines, "\n")))
	s.plan = hex.EncodeToString(sum[:8])
	if len(s.refs) == 0 {
		return s, nil
	}

	revs, err := memoryGitRun("rev-list", "--topo-order", "--reverse", "--parents", "--all")
	if err != nil {
		return nil, err
	}
	var lookup strings.Builder
	for _, line := range strings.Split(revs, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		s.order = append(s.order, f[0])
		s.parents[f[0]] = f[1:]
		lookup.WriteString(f[0] + ":" + s.path + "\n")
	}
	cmd := memoryGit("cat-file", "--batch-check")
	cmd.Stdin = strings.NewReader(lookup.String())
	res, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read the memory's revisions: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(res), "\n"), "\n")
	if len(lines) != len(s.order) {
		return nil, fmt.Errorf("read the memory's revisions: unexpected answer")
	}
	for i, c := range s.order {
		if f := strings.Fields(lines[i]); len(f) == 3 && f[1] == "blob" {
			s.blob[c] = f[0]
		}
	}

	hist, err := memoryGitRun("log", "--all", "-F", "--grep=AF-Memory: "+s.path,
		"--format=%H"+memoryFldSep+"%aI"+memoryFldSep+"%B"+memoryRecSep)
	if err != nil {
		return nil, err
	}
	for _, rec := range strings.Split(hist, memoryRecSep) {
		f := strings.SplitN(strings.TrimSpace(rec), memoryFldSep, 3)
		if len(f) < 3 {
			continue
		}
		tr := agentMemTrailers(f[2])
		if tr["AF-Trigger"] != memoryTriggerAgentMemory || tr["AF-Memory"] != s.path {
			continue
		}
		ch := agentMemPurgeChange{Commit: f[0], At: f[1], Op: "?", AuthorKind: "?"}
		if agentMemOps[tr["AF-Op"]] {
			ch.Op = tr["AF-Op"]
		}
		if agentMemAuthorOK(tr["AF-Author-Kind"], "") {
			ch.AuthorKind = tr["AF-Author-Kind"]
		}
		s.own[f[0]] = ch
	}

	// The current revision: the run of commits at the tip of main that already hold the tip's text.
	if mainTip != "" && s.blob[mainTip] != "" {
		s.keepBlob = s.blob[mainTip]
		for c := mainTip; c != "" && s.blob[c] == s.keepBlob; {
			s.keep[c] = true
			if p := s.parents[c]; len(p) > 0 {
				c = p[0]
			} else {
				c = ""
			}
		}
	}
	for _, c := range s.order {
		if _, own := s.own[c]; own && s.keep[c] {
			s.markPurged = c // the first change of the kept run
			break
		}
	}
	seen := map[string]bool{}
	for _, c := range s.order {
		if b := s.blob[c]; b != "" && !s.keep[c] && b != s.keepBlob && !seen[b] {
			seen[b] = true
			s.purged = append(s.purged, b)
		}
	}
	return s, nil
}

// removes says whether the purge takes the memory's file out of commit c (or the commit itself).
func (s *agentMemPurgeScan) removes(c string) bool {
	if s.keep[c] {
		return false
	}
	_, own := s.own[c]
	return own || s.blob[c] != ""
}

func (s *agentMemPurgeScan) nothing() bool {
	for _, c := range s.order {
		if s.removes(c) {
			return false
		}
	}
	return true
}

func (s *agentMemPurgeScan) preview(scope, project, name string) (agentMemPurgePreviewWire, error) {
	out := agentMemPurgePreviewWire{
		Scope: scope, Name: name, Changes: []agentMemPurgeChange{}, Refs: []agentMemPurgeRef{},
		Findings: []memorySecretFinding{}, Plan: s.plan, KeepsCurrent: len(s.keep) > 0,
	}
	if project != "" {
		out.Project = agentMemProjectInfo(project)
	}
	_, out.Live, _ = agentMemReadFile(filepath.Join(agentMemDir(), filepath.FromSlash(strings.TrimPrefix(s.path, agentMemRepoPrefix+"/"))))
	for i := len(s.order) - 1; i >= 0; i-- {
		c := s.order[i]
		if !s.removes(c) {
			continue
		}
		if ch, ok := s.own[c]; ok {
			out.ChangesTotal++
			if len(out.Changes) < agentMemPurgeListMax {
				ch.Commit = ch.Commit[:min(12, len(ch.Commit))]
				out.Changes = append(out.Changes, ch)
			}
		} else {
			out.Snapshots++
		}
	}
	out.Nothing = s.nothing()
	for _, r := range s.refs {
		list, err := memoryGitRun("rev-list", r.sha)
		if err != nil {
			return out, err
		}
		for _, c := range strings.Fields(list) {
			if s.removes(c) {
				r.Affected++
			}
		}
		out.Refs = append(out.Refs, r)
	}
	if len(s.purged) > 0 {
		refs := make([]memoryObjRef, len(s.purged))
		for i, b := range s.purged {
			refs[i] = memoryObjRef{SHA: b, Path: s.path}
		}
		f, err := memoryScanObjects(refs, nil)
		if err != nil {
			return out, err
		}
		out.Findings = f[:min(len(f), agentMemPurgeFindingsMax)]
		for i := range out.Findings {
			out.Findings[i].History = true
		}
	}
	return out, nil
}

// agentMemPurgePreview says what a purge of one memory would rewrite. It changes nothing.
func agentMemPurgePreview(scope, project, name string) (agentMemPurgePreviewWire, error) {
	rel, err := agentMemPurgeRel(scope, project, name)
	if err != nil {
		return agentMemPurgePreviewWire{}, err
	}
	agentMemMu.RLock()
	defer agentMemMu.RUnlock()
	if !memoryHasCommits() {
		return agentMemPurgePreviewWire{Scope: scope, Name: name, Nothing: true, Changes: []agentMemPurgeChange{},
			Refs: []agentMemPurgeRef{}, Findings: []memorySecretFinding{}}, nil
	}
	s, err := agentMemPurgeScanHistory(rel)
	if err != nil {
		return agentMemPurgePreviewWire{}, err
	}
	return s.preview(scope, project, name)
}

type purgeRawCommit struct {
	tree    string
	parents []string
	extra   []string // header lines other than tree, parent and the signature
	msg     string
}

func parsePurgeCommit(data []byte) purgeRawCommit {
	var c purgeRawCommit
	head, msg, _ := bytes.Cut(data, []byte("\n\n"))
	c.msg = string(msg)
	skip := false
	for _, line := range strings.Split(string(head), "\n") {
		if strings.HasPrefix(line, " ") {
			if !skip {
				c.extra = append(c.extra, line)
			}
			continue
		}
		skip = false
		switch {
		case strings.HasPrefix(line, "tree "):
			c.tree = strings.TrimPrefix(line, "tree ")
		case strings.HasPrefix(line, "parent "):
			c.parents = append(c.parents, strings.TrimPrefix(line, "parent "))
		case strings.HasPrefix(line, "gpgsig"), strings.HasPrefix(line, "mergetag"):
			skip = true // a signature does not survive a rewrite
		default:
			c.extra = append(c.extra, line)
		}
	}
	return c
}

func (c purgeRawCommit) render() []byte {
	var b strings.Builder
	b.WriteString("tree " + c.tree + "\n")
	for _, p := range c.parents {
		b.WriteString("parent " + p + "\n")
	}
	for _, l := range c.extra {
		b.WriteString(l + "\n")
	}
	b.WriteString("\n" + c.msg)
	return []byte(b.String())
}

// agentMemPurgeResume finishes a gc that a crash cut short. Callers hold the locks.
func agentMemPurgeResume() error {
	if _, err := os.Stat(agentMemPurgePendingPath()); err != nil {
		return nil
	}
	if err := agentMemPurgeCollect(); err != nil {
		return err
	}
	_ = os.Remove(agentMemPurgePendingPath())
	return nil
}

// ResumeMemoryPurge runs the pending gc of an interrupted purge, if there is one.
func ResumeMemoryPurge() {
	agentMemMu.Lock()
	defer agentMemMu.Unlock()
	memorySnapshotMu.Lock()
	defer memorySnapshotMu.Unlock()
	if err := agentMemPurgeResume(); err != nil {
		log.Printf("agent memory: finishing an interrupted purge: %s", agentMemErrKind(err))
	}
}

// agentMemPurgeCollect drops what the old history left on disk: reflogs, then every unreachable
// object, loose or packed.
func agentMemPurgeCollect() error {
	if _, err := memoryGitRun("reflog", "expire", "--expire=now", "--expire-unreachable=now", "--all"); err != nil {
		return fmt.Errorf("expire reflogs: %w", err)
	}
	if _, err := memoryGitRun("-c", "gc.autoDetach=false", "gc", "--prune=now", "--quiet"); err != nil {
		return fmt.Errorf("gc: %w", err)
	}
	return nil
}

// agentMemPurge removes the past revisions of one memory from the history, behind the preview's
// plan and the memory's name typed again.
func agentMemPurge(req agentMemPurgeReq) (agentMemPurgeResult, error) {
	rel, err := agentMemPurgeRel(req.Scope, req.Project, req.Name)
	if err != nil {
		return agentMemPurgeResult{}, err
	}
	if req.Confirm != req.Name {
		return agentMemPurgeResult{}, memoryErrf(http.StatusBadRequest, errCodeMemoryBadRequest, "confirm must be the memory's name")
	}
	agentMemMu.Lock()
	defer agentMemMu.Unlock()
	memorySnapshotMu.Lock()
	defer memorySnapshotMu.Unlock()
	if err := memoryEnsureRepo(); err != nil {
		return agentMemPurgeResult{}, err
	}
	if err := agentMemPurgeResume(); err != nil {
		return agentMemPurgeResult{}, err
	}
	res := agentMemPurgeResult{Scope: req.Scope, Name: req.Name}
	if !memoryHasCommits() {
		return res, memoryErrf(http.StatusConflict, errCodeMemoryConflict, "there is no history to purge")
	}
	s, err := agentMemPurgeScanHistory(rel)
	if err != nil {
		return res, err
	}
	if req.Plan == "" || req.Plan != s.plan {
		return res, memoryErrf(http.StatusConflict, errCodeMemoryConflict,
			"the history changed since the preview; review it again")
	}
	if s.nothing() {
		return res, memoryErrf(http.StatusConflict, errCodeMemoryConflict, "this memory has no past revision to purge")
	}
	res.KeptCurrent = len(s.keep) > 0

	// Every new object first; nothing is reachable from them yet, so a crash here costs garbage.
	cat, err := startPurgeCatFile()
	if err != nil {
		return res, err
	}
	defer cat.close()
	w, err := newPurgeWriter(cat)
	if err != nil {
		return res, err
	}
	mapped := map[string]string{} // old commit -> new commit; "" = dropped
	treeOf := map[string]string{} // new commit -> its tree
	emptyTree := ""
	for _, c := range s.order {
		typ, data, err := cat.get(c)
		if err != nil || typ != "commit" {
			return res, fmt.Errorf("read a commit of the history: %v", err)
		}
		raw := parsePurgeCommit(data)
		var parents []string
		for _, p := range raw.parents {
			if np := mapped[p]; np != "" && !purgeContains(parents, np) {
				parents = append(parents, np)
			}
		}
		tree := raw.tree
		_, isOwn := s.own[c]
		removes := s.removes(c)
		if s.blob[c] != "" && removes {
			if tree, err = w.removePath(raw.tree, s.comps); err != nil {
				return res, err
			}
			if tree == "" {
				if emptyTree == "" {
					if emptyTree, err = w.hashObject("tree", nil); err != nil {
						return res, err
					}
				}
				tree = emptyTree
			}
		}
		msg := raw.msg
		empty := (len(parents) == 1 && tree == treeOf[parents[0]]) || (len(parents) == 0 && tree == emptyTreeID(w, &emptyTree))
		switch {
		case removes && (isOwn || s.blob[c] != ""):
			// The change record of a past revision goes with it, and so does a commit that
			// carried nothing else. One that also carries other work stays, no longer pointing
			// at the memory.
			if empty {
				mapped[c] = firstOr(parents)
				res.CommitsDropped++
				continue
			}
			if isOwn {
				msg = dropTrailerLines(msg, "AF-Memory: ")
			}
		case c == s.markPurged && !strings.Contains(msg, agentMemPurgedTrailer+":"):
			msg = strings.TrimRight(msg, "\n") + "\n" + agentMemPurgedTrailer + ": 1\n"
		}
		if tree == raw.tree && msg == raw.msg && equalStrings(parents, raw.parents) {
			mapped[c], treeOf[c] = c, tree
			continue
		}
		nc := purgeRawCommit{tree: tree, parents: parents, extra: raw.extra, msg: msg}
		nsha, err := w.hashObject("commit", nc.render())
		if err != nil {
			return res, err
		}
		mapped[c], treeOf[nsha] = nsha, tree
		res.CommitsRewritten++
	}
	if err := agentMemPurgeStage("objects"); err != nil {
		return res, err
	}

	// The marker first: once a ref moves, the old objects are garbage that must be collected even
	// if this process dies before it does.
	if err := os.MkdirAll(memoryWorkDir(), 0o700); err != nil {
		return res, err
	}
	if err := os.WriteFile(agentMemPurgePendingPath(), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return res, err
	}
	var tx bytes.Buffer
	tx.WriteString("start\n")
	for _, r := range s.refs {
		switch nt := mapped[r.sha]; {
		case nt == r.sha:
		case nt == "":
			fmt.Fprintf(&tx, "delete %s %s\n", r.Name, r.sha)
			res.RefsDeleted++
		default:
			fmt.Fprintf(&tx, "update %s %s %s\n", r.Name, nt, r.sha)
			res.RefsRewritten++
		}
	}
	tx.WriteString("prepare\ncommit\n")
	cmd := memoryGit("update-ref", "--stdin")
	cmd.Stdin = &tx
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(agentMemPurgePendingPath())
		return res, fmt.Errorf("move the refs: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if err := agentMemPurgeStage("refs"); err != nil {
		return res, err
	}

	// The index still names the old blobs; rebuild it from the new tip before the gc looks.
	if memoryHasCommits() {
		_, err = memoryGitRun("read-tree", memoryBranch)
	} else {
		_, err = memoryGitRun("read-tree", "--empty")
	}
	if err != nil {
		return res, fmt.Errorf("reset the index: %w", err)
	}
	if err := agentMemPurgeCollect(); err != nil {
		return res, err
	}
	if err := agentMemPurgeStage("gc"); err != nil {
		return res, err
	}
	_ = os.Remove(agentMemPurgePendingPath())

	// Say it only if it is true: look for each purged blob.
	var shared map[string]bool
	for _, b := range s.purged {
		if _, err := memoryGitRun("cat-file", "-e", b); err != nil {
			res.BlobsRemoved++
			continue
		}
		if shared == nil {
			shared = map[string]bool{}
			out, err := memoryGitRun("rev-list", "--objects", "--all")
			if err != nil {
				return res, err
			}
			for _, l := range strings.Split(out, "\n") {
				sha, _, _ := strings.Cut(l, " ")
				shared[sha] = true
			}
		}
		if !shared[b] {
			return res, memoryErrf(http.StatusInternalServerError, errCodeMemorySnapshotFailed,
				"the history was rewritten but a purged revision is still on disk; run the purge again")
		}
		res.BlobsShared++
	}
	log.Printf("agent memory: purge: %d commit(s) dropped, %d rewritten, %d ref(s) moved", res.CommitsDropped, res.CommitsRewritten, res.RefsRewritten+res.RefsDeleted)
	return res, nil
}

func emptyTreeID(w *purgeWriter, cached *string) string {
	if *cached == "" {
		if id, err := w.hashObject("tree", nil); err == nil {
			*cached = id
		}
	}
	return *cached
}

func firstOr(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

func purgeContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func dropTrailerLines(msg, prefix string) string {
	var keep []string
	sc := bufio.NewScanner(strings.NewReader(msg))
	for sc.Scan() {
		if !strings.HasPrefix(sc.Text(), prefix) {
			keep = append(keep, sc.Text())
		}
	}
	return strings.Join(keep, "\n") + "\n"
}
