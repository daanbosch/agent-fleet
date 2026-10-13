package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/k-k1/agent-fleet/control-plane/internal/store"
)

const (
	// shareReposTTL is how long a /repos read is reused by later catalog syncs. /repos runs
	// git once per working copy (measured: 5.1 s on a member with many worktrees, against
	// 0.6 s for the session catalog), and what the sync takes from it — worktree, parent and
	// branch per working copy — rarely changes. A session in a working copy the cached read
	// does not know forces a live read, so a repo share still reaches a new worktree at once.
	shareReposTTL = 2 * time.Minute
	// catalogSyncTimeout bounds one coalesced sync. It is detached from the request that
	// started it, because other callers wait on the same result.
	catalogSyncTimeout = 60 * time.Second
)

// catalogSyncNeed is what a caller requires of the sync it waits for.
type catalogSyncNeed struct {
	// exact: the Agent must be read after this call began. Paths that act on a session the
	// owner may have created a moment ago (share put, handoff create, proposal approval) set
	// it; throttled reads join whatever sync is already running.
	exact bool
	// liveRepos: /repos must be read now rather than taken from the cache, because the
	// caller confirms a repo or worktree share target against it.
	liveRepos bool
}

type catalogFlight struct {
	res       *resolved
	liveRepos bool
	done      chan struct{}
	repos     map[string]repoInfo
	err       error
}

// ownerCatalogSync is one owner's sync state: at most one flight running and at most one
// queued behind it, so N viewers never mean N syncs, and an exact caller that arrives
// mid-flight gets the next one instead of a result read before it asked.
type ownerCatalogSync struct {
	cur, next *catalogFlight
	repos     map[string]repoInfo
	reposAt   time.Time
}

type catalogSyncer struct {
	mu     sync.Mutex
	owners map[string]*ownerCatalogSync
}

// catalogSyncOwnersMax bounds the per-owner state a long-lived CP keeps. Dropping an idle
// owner only costs that owner's next sync a live /repos read.
const catalogSyncOwnersMax = 10_000

// evictIdle drops owners with no flight whose /repos cache has expired. Call with cs.mu held.
func (cs *catalogSyncer) evictIdle() {
	for k, o := range cs.owners {
		if o.cur == nil && o.next == nil && time.Since(o.reposAt) >= shareReposTTL {
			delete(cs.owners, k)
		}
	}
}

func (m *manager) catalogSyncer() *catalogSyncer {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.catalogSync == nil {
		m.catalogSync = &catalogSyncer{owners: map[string]*ownerCatalogSync{}}
	}
	return m.catalogSync
}

// syncOwnerCatalog reconciles the owner's shared-session catalog with their Agent and
// returns the working-copy inventory it used (nil when unknown; when need.liveRepos is set,
// nil unless /repos answered during this sync).
//
// The Agent round trips run outside the owner's share lock; only the database write and the
// rule pruning take it. Holding it across them made every share create, unshare,
// permission change and handover wait for up to two Agent calls and a 1,200-row rewrite.
// Never call this while holding shareLockFor(owner): the flight needs that lock to finish.
func (m *manager) syncOwnerCatalog(ctx context.Context, res *resolved, need catalogSyncNeed) (map[string]repoInfo, error) {
	cs := m.catalogSyncer()
	owner := res.mv.MembershipID
	cs.mu.Lock()
	o := cs.owners[owner]
	if o == nil {
		if len(cs.owners) >= catalogSyncOwnersMax {
			cs.evictIdle()
		}
		o = &ownerCatalogSync{}
		cs.owners[owner] = o
	}
	var f *catalogFlight
	switch {
	case o.cur == nil:
		f = &catalogFlight{res: res, liveRepos: need.liveRepos, done: make(chan struct{})}
		o.cur = f
		go m.runCatalogFlights(cs, o, f)
	case !need.exact && (!need.liveRepos || o.cur.liveRepos):
		f = o.cur
	default:
		if o.next == nil {
			o.next = &catalogFlight{res: res, done: make(chan struct{})}
		}
		o.next.liveRepos = o.next.liveRepos || need.liveRepos
		f = o.next
	}
	cs.mu.Unlock()
	select {
	case <-f.done:
		return f.repos, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// runCatalogFlights runs f, then whatever queued behind it, one at a time. Running them in
// order is what keeps an older Agent snapshot from being written over a newer one.
func (m *manager) runCatalogFlights(cs *catalogSyncer, o *ownerCatalogSync, f *catalogFlight) {
	for f != nil {
		f.repos, f.err = m.runCatalogFlight(cs, o, f)
		cs.mu.Lock()
		close(f.done)
		o.cur, o.next = o.next, nil
		f = o.cur
		cs.mu.Unlock()
	}
}

// runCatalogFlight runs one flight and turns a panic into its error. Without that, a panic
// would leave done unclosed and the queue stuck, so every later sync for this owner would hang
// on a dead flight — and an unrecovered panic in this goroutine would take the CP down.
func (m *manager) runCatalogFlight(cs *catalogSyncer, o *ownerCatalogSync, f *catalogFlight) (repos map[string]repoInfo, err error) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("catalog sync for %s panicked: %v\n%s", f.res.mv.MembershipID, p, debug.Stack())
			repos, err = nil, &catalogSyncError{fmt.Errorf("catalog sync panicked: %v", p)}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), catalogSyncTimeout)
	defer cancel()
	return m.syncCatalogOnce(ctx, cs, o, f)
}

func (m *manager) syncCatalogOnce(ctx context.Context, cs *catalogSyncer, o *ownerCatalogSync, f *catalogFlight) (map[string]repoInfo, error) {
	res := f.res
	if res.rt.State(ctx) != "running" {
		return nil, nil
	}
	cs.mu.Lock()
	cached, cachedAt := o.repos, o.reposAt
	cs.mu.Unlock()
	readRepos := f.liveRepos || cached == nil || time.Since(cachedAt) >= shareReposTTL
	type reposResult struct{ inv map[string]repoInfo }
	var pending chan reposResult
	startRepos := func() {
		pending = make(chan reposResult, 1)
		go func() { pending <- reposResult{readAgentRepos(ctx, res)} }()
	}
	if readRepos {
		startRepos() // in parallel with the catalog: /repos is the slow one
	}
	sessions, err := readAgentCatalog(ctx, res.rt)
	if err != nil {
		return nil, err
	}
	if !readRepos {
		for _, s := range sessions {
			if _, known := cached[s.WorkingCopyID]; s.WorkingCopyID != "" && !known {
				readRepos = true
				startRepos()
				break
			}
		}
	}
	// live is the inventory read during this sync; nil when it was not read or failed. Only
	// a live read may prune rules (a deleted working copy ends its rule), never a cached
	// or failed one.
	var live map[string]repoInfo
	if readRepos {
		live = (<-pending).inv
	}
	info := live
	if live == nil {
		info = cached
	}
	// A working copy the inventory does not describe keeps what its stored row says, rather
	// than an empty parent: an empty parent_working_copy_id drops a worktree's sessions out
	// of the repo share until the next good read.
	var stored map[string]store.SharedSessionCatalog
	if live == nil {
		for _, s := range sessions {
			if _, known := info[s.WorkingCopyID]; s.WorkingCopyID != "" && !known {
				rows, err := m.store.ListSharedSessionCatalogByOwner(ctx, res.mv.MembershipID)
				if err != nil {
					return nil, err
				}
				stored = make(map[string]store.SharedSessionCatalog, len(rows))
				for _, r := range rows {
					stored[r.Name] = r
				}
				break
			}
		}
	}
	now := store.NowTS()
	rows := make([]store.SharedSessionCatalog, 0, len(sessions))
	for _, s := range sessions {
		// state is liveness (running/stopped), activity is the Agent's live state (working /
		// question / …). Keeping the last activity on a stopped session would read as "it
		// stopped mid-turn", so activity is only carried while the session is alive.
		state, activity := "stopped", ""
		if s.Alive {
			state, activity = "running", s.State
		}
		wc, known := info[s.WorkingCopyID]
		if prev, ok := stored[s.Name]; !known && ok && prev.WorkingCopyID == s.WorkingCopyID {
			wc = repoInfo{worktree: prev.Worktree, parent: prev.Parent, parentWC: prev.ParentWorkingCopyID, branch: prev.Branch}
		}
		rows = append(rows, store.SharedSessionCatalog{ID: store.NewID(), WorkspaceID: res.ws.ID,
			OwnerMembershipID: res.mv.MembershipID, Name: s.Name, Kind: s.Kind, Dir: s.Dir, Repo: s.Repo,
			WorkingCopyID: s.WorkingCopyID, Title: s.Title, Label: s.Label, CreatedAt: s.CreatedAt,
			State: state, Archived: s.Archived, LastSeen: now, Worktree: wc.worktree, Parent: wc.parent,
			ParentWorkingCopyID: wc.parentWC, Branch: wc.branch, Activity: activity})
	}
	result := info
	if f.liveRepos {
		result = live
	}
	lock := m.shareLockFor(res.mv.MembershipID)
	lock.Lock()
	defer lock.Unlock()
	// The cache is published under the share lock, so whoever holds that lock sees the
	// newest live inventory any flight has applied (see appliedRepos).
	if live != nil {
		cs.mu.Lock()
		o.repos, o.reposAt = live, time.Now()
		cs.mu.Unlock()
	}
	if err := m.store.ReplaceSharedSessionCatalog(ctx, res.ws.ID, res.mv.MembershipID, rows); errors.Is(err, store.ErrSessionShareOwnerBusy) {
		return result, nil // another CP replica is applying an already-authorized operation
	} else if err != nil {
		return result, err
	}
	if live == nil {
		return result, nil
	}
	shares, _ := m.store.ListSessionSharesByOwner(ctx, res.mv.MembershipID)
	for _, share := range shares {
		if _, ok := live[share.ScopeKey]; share.ScopeType != "session" && !ok {
			_ = m.store.DeleteSessionSharesByScope(ctx, res.mv.MembershipID, share.ScopeType, share.ScopeKey)
		}
	}
	return result, nil
}

// appliedRepos is the newest live working-copy inventory a sync has applied for this owner.
// Call it holding shareLockFor(owner): flights publish it under that lock and run in order,
// so it is at least as new as any inventory a caller received from syncOwnerCatalog, and a
// later flight that saw a working copy vanish (and pruned its rules) has replaced it.
func (m *manager) appliedRepos(owner string) map[string]repoInfo {
	cs := m.catalogSyncer()
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if o := cs.owners[owner]; o != nil {
		return o.repos
	}
	return nil
}

// readAgentRepos reads the owner's working-copy inventory keyed by workingCopyId, or nil when
// the Agent could not answer.
func readAgentRepos(ctx context.Context, res *resolved) map[string]repoInfo {
	body, err := agentText(ctx, res.rt, http.MethodGet, "/repos", nil)
	if err != nil {
		return nil
	}
	var inventory struct {
		Repos []struct {
			Name          string `json:"name"`
			WorkingCopyID string `json:"workingCopyId"`
			Worktree      bool   `json:"worktree"`
			Parent        string `json:"parent"`
			Branch        string `json:"branch"`
		} `json:"repos"`
	}
	if json.Unmarshal([]byte(body), &inventory) != nil {
		return nil
	}
	out := map[string]repoInfo{}
	// Parent is a folder name, so build a name -> workingCopyId map first.
	wcByName := map[string]string{}
	for _, repo := range inventory.Repos {
		if !repo.Worktree {
			wcByName[repo.Name] = repo.WorkingCopyID
		}
	}
	for _, repo := range inventory.Repos {
		info := repoInfo{worktree: repo.Worktree, parent: repo.Parent, branch: repo.Branch}
		if repo.Worktree {
			info.parentWC = wcByName[repo.Parent]
		}
		out[repo.WorkingCopyID] = info
	}
	return out
}
