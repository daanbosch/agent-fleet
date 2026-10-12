// Package limitepoch keeps the private login epoch of ADR 0111 decision 6: an opaque value
// per agent kind that changes when the member signs in to a different provider account, and
// the time this Agent first saw that account.
//
// A quota reading captured under the previous account must never be shown as the new
// account's. The kinds' collectors do not record which account a reading came from, so the
// rule is by time instead: a reading observed before the current account was first seen is
// dropped. That is conservative in one direction only — a reading the new account made
// before the Agent noticed the switch is dropped too, and comes back with the next one.
//
// The epoch is an HMAC of the account identity under a key that never leaves this home, so
// the CP can compare epochs without being able to tell whose account it is.
package limitepoch

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/paths"
)

type entry struct {
	// ID is the full HMAC of the identity. Only its prefix is ever sent.
	ID string `json:"id"`
	// Since is the admission bound: readings observed before it are dropped. Zero for the first
	// account a ledger ever records.
	Since time.Time `json:"since"`
	// Seen is when this ledger entry was made, always a real time. It orders epochs for the CP
	// (a later Seen is a later login), so a late post from before a re-login can be refused.
	// Unlike Since it is never zero: a ledger rebuilt after a home is recreated must still
	// outrank whatever the CP holds from before.
	Seen time.Time `json:"seen"`
}

// Stamp is the current account's epoch as the snapshot and the chip endpoints report it.
type Stamp struct {
	Epoch string
	Since time.Time
	Seen  time.Time
}

// SeenLayout is how Seen travels: UTC, fixed-width nanoseconds, so the CP can order two
// values by comparing the strings and no precision is lost on the way.
const SeenLayout = "2006-01-02T15:04:05.000000000Z"

// SeenText is Seen as the wire carries it.
func (s Stamp) SeenText() string { return s.Seen.UTC().Format(SeenLayout) }

type ledger struct {
	Key   []byte           `json:"key"`
	Kinds map[string]entry `json:"kinds"`
	// Last is the latest Seen this ledger ever issued, across kinds. A new Seen is always
	// strictly after it, so two logins recognised in the same instant, or after the clock went
	// back, are still ordered.
	Last time.Time `json:"last"`
}

var (
	mu       sync.Mutex
	loadedAt string // the path the in-memory ledger was read from
	cur      ledger
)

func ledgerPath() string { return filepath.Join(paths.AgentStateDir(), "limit-epochs.json") }

// load reads the ledger once per path. The path is part of the key because tests (and only
// tests) move HOME, and a ledger read under one home must not answer for another.
// Caller holds mu.
func load() {
	p := ledgerPath()
	if loadedAt == p {
		return
	}
	loadedAt, cur = p, ledger{}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &cur)
	}
	if len(cur.Key) != 32 {
		cur.Key = make([]byte, 32)
		if _, err := rand.Read(cur.Key); err != nil {
			// Without randomness the epoch would be a plain hash of the account id, which the
			// CP could reverse by guessing. Leave the key empty: Current then reports nothing.
			cur.Key = nil
		}
		cur.Kinds = nil
	}
	if cur.Kinds == nil {
		cur.Kinds = map[string]entry{}
	}
}

// save writes the ledger atomically. A failed write keeps the in-memory ledger, which is
// still right for this process; the next Agent start sees the account as new and sets Since
// then, which only drops more.
func save() {
	b, err := json.Marshal(cur)
	if err != nil {
		return
	}
	p := ledgerPath()
	if os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

// Current returns the epoch for kind's current account identity ("" = no identity could be
// read, which is an account of its own) and since, the time this Agent first saw it. ok is
// false when no key could be made, and the caller then reports nothing for the kind.
//
// The first account a ledger ever records gets a zero Since: nothing says the readings already
// on disk belong to anybody else.
func Current(kind, identity string, now time.Time) (Stamp, bool) {
	mu.Lock()
	defer mu.Unlock()
	load()
	if len(cur.Key) == 0 {
		return Stamp{}, false
	}
	m := hmac.New(sha256.New, cur.Key)
	m.Write([]byte(kind + "\x00" + identity))
	id := hex.EncodeToString(m.Sum(nil))
	e, known := cur.Kinds[kind]
	if !known || e.ID != id || e.Seen.IsZero() {
		seen := now.UTC()
		if !seen.After(cur.Last) {
			seen = cur.Last.Add(time.Nanosecond)
		}
		cur.Last = seen
		since := seen
		switch {
		case !known:
			since = time.Time{}
		case e.ID == id:
			// A ledger written before Seen existed: same account, keep its bound.
			since = e.Since
		}
		e = entry{ID: id, Since: since, Seen: seen}
		cur.Kinds[kind] = e
		save()
	}
	return Stamp{Epoch: id[:16], Since: e.Since, Seen: e.Seen}, true
}

// Admit reports whether a reading observed at observedAt may be shown under the account first
// seen at since. A reading with no observation time is never admitted: its age is unknown.
//
// Strictly after since: collectors stamp whole seconds, so a reading of the previous account
// taken in the same second as the switch would otherwise pass. The cost is that a reading of
// the new account in that second is dropped too, and comes back with the next one.
func Admit(since, observedAt time.Time) bool {
	if observedAt.IsZero() {
		return false
	}
	return since.IsZero() || observedAt.After(since)
}

// Annotate adds to a usage-chip response what the Console's usage-observation post needs to
// honour the observation contract: when the reading was really observed, and the opaque
// epoch of the account it belongs to. Both are left out when the reading predates the account
// now signed in (or has no time), so the browser never posts the previous account's numbers
// as this one's, and never has to make a time up.
func Annotate(out map[string]any, kind, identity string, observedAt time.Time) {
	st, ok := Current(kind, identity, time.Now())
	if !ok || !Admit(st.Since, observedAt) {
		return
	}
	out["observedAt"] = observedAt.UTC().Format(time.RFC3339)
	out["limitEpoch"] = st.Epoch
	out["limitEpochSince"] = st.SeenText()
}
