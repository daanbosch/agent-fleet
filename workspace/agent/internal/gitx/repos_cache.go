package gitx

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// GET /repos used to fork git five to eight times per working copy on every call (measured:
// 7-9 s for 74 copies, warm or cold). This file caches the per-copy answers behind a stat-based fingerprint, so a
// call that finds nothing changed costs a handful of stat/read calls per copy and no git.
//
// What each field of Repo comes from, and what invalidates it:
//
//	branch, unborn, ahead, behind  git status --branch: HEAD, the current branch's ref, its
//	                               upstream ref, packed-refs, config      -> refsFP; maxStatusAge
//	dirty                          git status: the INDEX and the TREE      -> refsFP (index) and
//	                               maxStatusAge, because a file edited in the tree changes no
//	                               git metadata at all
//	provider, remote, remotePath   origin URL after url.*.insteadOf: the repo config and the
//	                               global git configs                      -> identFP
//	worktree, parent               where the .git gitfile points           -> identFP
//	createdAt                      mtime of the .git gitfile               -> identFP (it stats it)
//	integration                    worktree HEAD vs the parent's upstream  -> refsFP of BOTH
//	                               copies and the parent's branch label; maxIntegrationAge
//	workingCopyId, locked, path    read live on every call (a file read / the lock ledger)
//
// A fingerprint is taken BEFORE git runs and stored with the result, so a change that lands
// while git is running makes the next call recompute instead of pinning an old answer.
// Anything the fingerprint cannot model (config includes, a non-default fetch refspec, the
// reftable ref backend) makes the copy uncacheable: it is computed live every time. When in
// doubt, answer "recompute".
const (
	// maxIntegrationAge is a belt for the integration result: ref changes are fingerprinted,
	// so this only bounds a fingerprint blind spot.
	maxIntegrationAge = 5 * time.Minute
	// maxIdentAge likewise bounds the identity result (origin URL, worktree parent).
	maxIdentAge = 5 * time.Minute

	// maxCacheEntries bounds the cache; listRepos also prunes entries whose folder is
	// gone, so the cap only matters for an absurd ~/repos.
	maxCacheEntries = 1024
	// gitParallelism caps the git processes the cache starts at once, across all requests:
	// the host is memory-constrained and N concurrent /repos calls must not multiply it.
	gitParallelism = 4
)

// maxStatusAge bounds how long a status result is served when no git metadata moved. It is
// the staleness a plain file edit (dirty flag) can have; the CP caches /repos for two
// minutes already, so the dialogs tolerate far more than this. A variable only so tests can
// force the age out.
var maxStatusAge = 10 * time.Second

// fileSig is what we compare to decide a file changed: identity (a ref or the index is
// replaced by rename, which gives a new inode), mtime and size. A missing file is the zero
// value with exists=false, so "created" and "removed" are changes too.
type fileSig struct {
	fi os.FileInfo
}

func sigOf(path string) fileSig {
	fi, err := os.Stat(path)
	if err != nil {
		return fileSig{}
	}
	return fileSig{fi: fi}
}

func (a fileSig) equal(b fileSig) bool {
	if a.fi == nil || b.fi == nil {
		return a.fi == nil && b.fi == nil
	}
	return os.SameFile(a.fi, b.fi) && a.fi.ModTime().Equal(b.fi.ModTime()) && a.fi.Size() == b.fi.Size()
}

// fingerprint is the list of files (paths) and their signatures. Paths are part of it: the
// current branch or its upstream changing names the file to watch differently.
type fingerprint struct {
	paths []string
	sigs  []fileSig
}

func (f *fingerprint) add(path string) {
	f.paths = append(f.paths, path)
	f.sigs = append(f.sigs, sigOf(path))
}

func (a fingerprint) equal(b fingerprint) bool {
	if len(a.paths) != len(b.paths) {
		return false
	}
	for i := range a.paths {
		if a.paths[i] != b.paths[i] || !a.sigs[i].equal(b.sigs[i]) {
			return false
		}
	}
	return true
}

// globalConfigPaths are the git configs outside the repository that can change what a
// repository reports (url.<base>.insteadOf rewrites `remote get-url`).
func globalConfigPaths() []string {
	home := homeDir()
	paths := []string{"/etc/gitconfig", filepath.Join(home, ".gitconfig")}
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		xdg = filepath.Join(home, ".config")
	}
	return append(paths, filepath.Join(xdg, "git", "config"))
}

// gitLocation resolves where a working copy keeps its git data. gitDir holds HEAD and the
// index (per worktree); commonDir holds refs, packed-refs and config (shared by linked
// worktrees). ok=false when dir has no readable .git.
func gitLocation(dir string) (gitDir, commonDir string, ok bool) {
	marker := filepath.Join(dir, ".git")
	fi, err := os.Stat(marker)
	if err != nil {
		return "", "", false
	}
	if fi.IsDir() {
		return marker, marker, true
	}
	body, err := os.ReadFile(marker)
	if err != nil {
		return "", "", false
	}
	gitDir = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(body)), "gitdir:"))
	if gitDir == "" {
		return "", "", false
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}
	gitDir = filepath.Clean(gitDir)
	commonDir = gitDir
	if b, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		c := strings.TrimSpace(string(b))
		if c != "" {
			if !filepath.IsAbs(c) {
				c = filepath.Join(gitDir, c)
			}
			commonDir = filepath.Clean(c)
		}
	}
	return gitDir, commonDir, true
}

// upstreamRef names the ref file a branch's ahead/behind is measured against, from the
// repository config text. "" means "no upstream" (nothing to watch). ok=false means the
// config uses something the fingerprint cannot follow.
func upstreamRef(config, branch string) (ref string, ok bool) {
	lower := strings.ToLower(config)
	if strings.Contains(lower, "[include") || strings.Contains(lower, "refstorage") {
		return "", false
	}
	var section, remote, merge string
	fetchOK := true
	for _, raw := range strings.Split(config, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") {
			section = strings.ToLower(line)
			continue
		}
		k, v, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.Trim(strings.TrimSpace(v), `"`)
		switch {
		case section == `[branch "`+strings.ToLower(branch)+`"]` && k == "remote":
			remote = v
		case section == `[branch "`+strings.ToLower(branch)+`"]` && k == "merge":
			merge = v
		case strings.HasPrefix(section, "[remote ") && k == "fetch":
			// Only the default mapping puts refs/heads/x of remote r at refs/remotes/r/x.
			name := strings.TrimSuffix(strings.TrimPrefix(section, `[remote "`), `"]`)
			if v != "+refs/heads/*:refs/remotes/"+name+"/*" {
				fetchOK = false
			}
		}
	}
	if remote == "" || merge == "" {
		return "", true
	}
	if !fetchOK || !strings.HasPrefix(merge, "refs/heads/") {
		return "", false
	}
	if remote == "." {
		return merge, true
	}
	return "refs/remotes/" + remote + "/" + strings.TrimPrefix(merge, "refs/heads/"), true
}

// refsFingerprint covers everything `git status --branch` reads from the repository's
// metadata. ok=false when the copy cannot be fingerprinted precisely.
func refsFingerprint(dir string, global []string) (fp fingerprint, ok bool) {
	gitDir, commonDir, found := gitLocation(dir)
	if !found {
		return fp, false
	}
	fp.add(filepath.Join(dir, ".git"))
	head := filepath.Join(gitDir, "HEAD")
	fp.add(head)
	fp.add(filepath.Join(gitDir, "index"))
	fp.add(filepath.Join(gitDir, "config.worktree"))
	fp.add(filepath.Join(commonDir, "packed-refs"))
	cfgPath := filepath.Join(commonDir, "config")
	fp.add(cfgPath)
	for _, g := range global {
		fp.add(g)
	}
	headBody, err := os.ReadFile(head)
	if err != nil {
		return fp, false
	}
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		return fp, false
	}
	refName, symbolic := strings.CutPrefix(strings.TrimSpace(string(headBody)), "ref: ")
	if !symbolic { // detached: HEAD itself carries the commit
		if _, up := upstreamRef(string(cfg), ""); !up {
			return fp, false
		}
		return fp, true
	}
	fp.add(filepath.Join(commonDir, filepath.FromSlash(refName)))
	if branch, isBranch := strings.CutPrefix(refName, "refs/heads/"); isBranch {
		up, good := upstreamRef(string(cfg), branch)
		if !good {
			return fp, false
		}
		if up != "" {
			fp.add(filepath.Join(commonDir, filepath.FromSlash(up)))
		}
	}
	return fp, true
}

// identFingerprint covers what the identity result reads: the .git marker (its mtime is
// createdAt), the repository config and the global configs.
func identFingerprint(dir string, global []string) (fp fingerprint, ok bool) {
	_, commonDir, found := gitLocation(dir)
	if !found {
		return fp, false
	}
	fp.add(filepath.Join(dir, ".git"))
	fp.add(filepath.Join(commonDir, "config"))
	for _, g := range global {
		fp.add(g)
	}
	return fp, true
}

// repoIdent is the slow-moving part of a Repo.
type repoIdent struct {
	provider, host, remotePath string
	worktree                   bool
	parent                     string // folder name of the main working copy
	parentDir                  string
	createdAt                  string
}

type slot[T any] struct {
	valid bool
	fp    fingerprint
	at    time.Time
	val   T
}

// fresh reports whether the slot answers for a request whose fingerprint is fp.
func (s *slot[T]) fresh(fp fingerprint, maxAge time.Duration) bool {
	return s.valid && s.fp.equal(fp) && time.Since(s.at) < maxAge
}

func (s *slot[T]) set(fp fingerprint, v T) {
	s.valid, s.fp, s.at, s.val = true, fp, time.Now(), v
}

// copyEntry is one working copy's cache. mu is held while git runs for it, which is the
// single-flight: concurrent callers for the same copy queue on it and then find the slot
// fresh, so git never runs twice for one copy at once.
type copyEntry struct {
	mu       sync.Mutex
	lastUsed time.Time
	ident    slot[repoIdent]
	status   slot[RepoStatus]
	integ    slot[RepoIntegration]
	integKey string // parent dir + target label the integration slot was computed for
}

type repoCache struct {
	mu      sync.Mutex
	entries map[string]*copyEntry
	sem     chan struct{}
}

var listCache = &repoCache{entries: map[string]*copyEntry{}, sem: make(chan struct{}, gitParallelism)}

func (c *repoCache) entry(dir string) *copyEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[dir]
	if e == nil {
		if len(c.entries) >= maxCacheEntries {
			c.evictOldestLocked()
		}
		e = &copyEntry{}
		c.entries[dir] = e
	}
	e.lastUsed = time.Now()
	return e
}

func (c *repoCache) evictOldestLocked() {
	var oldest string
	var at time.Time
	for k, e := range c.entries {
		if oldest == "" || e.lastUsed.Before(at) {
			oldest, at = k, e.lastUsed
		}
	}
	delete(c.entries, oldest)
}

// prune drops entries whose folder is no longer listed.
func (c *repoCache) prune(live map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if !live[k] {
			delete(c.entries, k)
		}
	}
}

// gitSlot runs fn under the process-wide git concurrency cap.
func (c *repoCache) gitSlot() func() {
	c.sem <- struct{}{}
	return func() { <-c.sem }
}

// inspect returns the status and identity of the git working copy at dir.
func (c *repoCache) inspect(dir string, global []string) (RepoStatus, repoIdent) {
	e := c.entry(dir)
	e.mu.Lock()
	defer e.mu.Unlock()

	identFP, identOK := identFingerprint(dir, global)
	if !identOK || !e.ident.fresh(identFP, maxIdentAge) {
		release := c.gitSlot()
		id := computeIdent(dir)
		release()
		if identOK {
			e.ident.set(identFP, id)
		} else {
			e.ident = slot[repoIdent]{val: id}
		}
	}
	statusFP, statusOK := refsFingerprint(dir, global)
	if !statusOK || !e.status.fresh(statusFP, maxStatusAge) {
		release := c.gitSlot()
		st, _ := GitStatus(dir)
		release()
		if statusOK {
			e.status.set(statusFP, st)
		} else {
			e.status = slot[RepoStatus]{val: st}
		}
	}
	return e.status.val, e.ident.val
}

func computeIdent(dir string) repoIdent {
	var id repoIdent
	if origin, ok := GitOriginURL(dir); ok {
		id.provider, id.host = gitProviderHost(origin)
		id.remotePath = gitRemotePath(origin)
	}
	if IsLinkedWorktree(dir) {
		id.worktree = true
		id.parentDir = WorktreeParent(dir)
		id.parent = filepath.Base(id.parentDir)
		id.createdAt = worktreeCreatedAt(dir)
	}
	return id
}

// integration returns the linked worktree's comparison with its parent's upstream.
func (c *repoCache) integration(dir, parentDir, targetBranch string, global []string) RepoIntegration {
	e := c.entry(dir)
	e.mu.Lock()
	defer e.mu.Unlock()

	var fp fingerprint
	wtFP, wtOK := refsFingerprint(dir, global)
	parFP, parOK := refsFingerprint(parentDir, global)
	ok := wtOK && parOK
	if ok {
		fp = fingerprint{
			paths: append(append([]string{}, wtFP.paths...), parFP.paths...),
			sigs:  append(append([]fileSig{}, wtFP.sigs...), parFP.sigs...),
		}
	}
	key := parentDir + "\x00" + targetBranch
	if ok && e.integKey == key && e.integ.fresh(fp, maxIntegrationAge) {
		return e.integ.val
	}
	release := c.gitSlot()
	r := GitWorktreeIntegration(parentDir, dir, targetBranch)
	release()
	if ok {
		e.integ.set(fp, r)
		e.integKey = key
	} else {
		e.integ = slot[RepoIntegration]{val: r}
	}
	return r
}

// parentDir is the main working copy of the linked worktree at dir, from the identity slot
// inspect just filled.
func (c *repoCache) parentDir(dir string) string {
	e := c.entry(dir)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ident.val.parentDir
}

// parallel runs fn for every index with at most gitParallelism calls in flight. The cap
// that bounds git processes is the cache's semaphore; this one only bounds goroutines.
func parallel(idx []int, fn func(i int)) {
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < gitParallelism*2 && w < len(idx); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				fn(i)
			}
		}()
	}
	for _, i := range idx {
		next <- i
	}
	close(next)
	wg.Wait()
}
