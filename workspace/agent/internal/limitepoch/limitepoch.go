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
	ID    string    `json:"id"`
	Since time.Time `json:"since"`
}

type ledger struct {
	Key   []byte           `json:"key"`
	Kinds map[string]entry `json:"kinds"`
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
func Current(kind, identity string, now time.Time) (epoch string, since time.Time, ok bool) {
	mu.Lock()
	defer mu.Unlock()
	load()
	if len(cur.Key) == 0 {
		return "", time.Time{}, false
	}
	m := hmac.New(sha256.New, cur.Key)
	m.Write([]byte(kind + "\x00" + identity))
	id := hex.EncodeToString(m.Sum(nil))
	e, seen := cur.Kinds[kind]
	if !seen || e.ID != id {
		since := now.UTC()
		if !seen {
			since = time.Time{}
		}
		e = entry{ID: id, Since: since}
		cur.Kinds[kind] = e
		save()
	}
	return id[:16], e.Since, true
}

// Admit reports whether a reading observed at observedAt may be shown under the account first
// seen at since. A reading with no observation time is never admitted: its age is unknown.
func Admit(since, observedAt time.Time) bool {
	return !observedAt.IsZero() && !observedAt.Before(since)
}

// Annotate adds to a usage-chip response what the Console's usage-observation post needs to
// honour the observation contract: when the reading was really observed, and the opaque
// epoch of the account it belongs to. Both are left out when the reading predates the account
// now signed in (or has no time), so the browser never posts the previous account's numbers
// as this one's, and never has to make a time up.
func Annotate(out map[string]any, kind, identity string, observedAt time.Time) {
	epoch, since, ok := Current(kind, identity, time.Now())
	if !ok || !Admit(since, observedAt) {
		return
	}
	out["observedAt"] = observedAt.UTC().Format(time.RFC3339)
	out["limitEpoch"] = epoch
}
