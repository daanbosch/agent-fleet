package sessionx

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

// The catalog lists every session, archived ones included, so a member with a long history
// sends a thousand rows on every sync of which a handful changed. Rows are derived from the
// session metas and tmux liveness, written in many places, so changes are found by comparing
// each request's rows with the previous ones rather than by hooking the writers: a row whose
// value differs from the last one seen gets the next revision, a name that vanished gets a
// tombstone with the next revision. The reader keeps the revision it last applied (the
// cursor) and asks for everything after it.
//
// The cursor is "<epoch>.<revision>". The epoch is random per process, so after a restart —
// when the counter starts over and a number alone would silently mean something else — no old
// cursor matches and the reader is sent the whole catalog. Nothing here reads a clock.

// maxCatalogTombstones bounds the memory kept for deleted names. Dropping one raises
// tombFloor, and a cursor older than that gets the whole catalog: the answer is still right,
// only larger.
const maxCatalogTombstones = 4096

type catalogRow struct {
	entry session.CatalogEntry
	rev   uint64
}

type catalogTracker struct {
	mu        sync.Mutex
	epoch     string
	rev       uint64
	rows      map[string]catalogRow
	tombs     map[string]uint64 // deleted name -> revision of the deletion
	tombFloor uint64            // every deletion at or below this may have been forgotten
}

var catalogChanges = newCatalogTracker()

func newCatalogTracker() *catalogTracker {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("catalog epoch: " + err.Error()) // a CSPRNG that fails is not a state to limp along in
	}
	return &catalogTracker{epoch: hex.EncodeToString(b[:]), rows: map[string]catalogRow{}, tombs: map[string]uint64{}}
}

// observe folds the current rows into the tracker. Call with t.mu held.
func (t *catalogTracker) observe(items []session.CatalogEntry) {
	seen := make(map[string]bool, len(items))
	for _, e := range items {
		seen[e.Name] = true
		if old, ok := t.rows[e.Name]; !ok || old.entry != e {
			t.rev++
			t.rows[e.Name] = catalogRow{entry: e, rev: t.rev}
			delete(t.tombs, e.Name)
		}
	}
	for name := range t.rows {
		if !seen[name] {
			t.rev++
			delete(t.rows, name)
			t.tombs[name] = t.rev
		}
	}
	for len(t.tombs) > maxCatalogTombstones {
		var oldest string
		var at uint64
		for n, r := range t.tombs {
			if oldest == "" || r < at {
				oldest, at = n, r
			}
		}
		delete(t.tombs, oldest)
		t.tombFloor = max(t.tombFloor, at)
	}
}

// respond answers a request that carried ?since=since.
func (t *catalogTracker) respond(items []session.CatalogEntry, since string) session.CatalogResponse {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.observe(items)
	resp := session.CatalogResponse{Cursor: t.epoch + "." + strconv.FormatUint(t.rev, 10)}
	epoch, revStr, ok := strings.Cut(since, ".")
	rev, err := strconv.ParseUint(revStr, 10, 64)
	if !ok || err != nil || epoch != t.epoch || rev > t.rev || rev < t.tombFloor {
		resp.Sessions = items
		return resp
	}
	resp.Delta = true
	resp.Sessions = []session.CatalogEntry{}
	for _, e := range items { // items is already in the catalog's order
		if t.rows[e.Name].rev > rev {
			resp.Sessions = append(resp.Sessions, e)
		}
	}
	for name, r := range t.tombs {
		if r > rev {
			resp.Deleted = append(resp.Deleted, name)
		}
	}
	sort.Strings(resp.Deleted)
	return resp
}
