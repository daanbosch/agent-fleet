// Package limitsnap builds the limit snapshot the Agent adds to GET /sessions for the CP's
// limit board (ADR 0111 decision 6).
//
// Building it only reads what the kinds' collectors have already captured: a statusLine
// capture, a rollout, an app-server push, a cache the usage chip filled. Nothing here fetches,
// execs or asks a host, because the reaper's sweep is serial and waits on this response.
//
// Coverage is decision 6's P1 table: claude, codex and muse windows, copilot credit pools.
// agy is deliberately absent (its refresh scrapes the TUI), and so is every other kind. A kind
// absent from the snapshot is unknown to the CP, never "available".
package limitsnap

import (
	"sort"
	"sync"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents/claude"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents/codex"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents/copilot"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents/muse"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/limitepoch"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/limitobs"
)

// Version is the snapshot's wire version. The CP treats any other value as "every kind
// unknown" and still reads the rest of the envelope.
const Version = 1

// Snapshot is the "limits" member of the GET /sessions envelope.
type Snapshot struct {
	V     int             `json:"v"`
	Kinds map[string]Kind `json:"kinds"`
}

// Kind is one agent kind's quota. Epoch is the opaque login epoch (limitepoch); the CP drops
// what it holds for the kind when the epoch changes. EpochSince orders epochs (a later value is
// a later login), so the CP can refuse a late report from before a re-login.
//
// Units may be empty: a kind whose account is known but has nothing admitted yet still reports
// its epoch, which is how a re-login clears the previous account's values on the CP before the
// new account has been observed at all.
type Kind struct {
	Epoch      string `json:"epoch"`
	EpochSince string `json:"epochSince"`
	Units      []Unit `json:"units"`
}

// Unit is one quota unit as last observed.
type Unit struct {
	Unit       string  `json:"unit"`
	UsedPct    float64 `json:"usedPct"`
	ResetsAt   string  `json:"resetsAt,omitempty"`
	WindowMins int     `json:"windowMins,omitempty"`
	ObservedAt string  `json:"observedAt"`
	// Derived marks a value worked out rather than observed (a past window rolled to 0%).
	// This Agent never sends one; the field is part of the contract the browser post shares,
	// and the CP ignores a derived unit from either source.
	Derived bool `json:"derived,omitempty"`
}

// sources is decision 6's P1 coverage. A var so a test can stand in for the collectors.
var sources = map[string]func() limitobs.Reading{
	"claude":  claude.LimitReading,
	"codex":   codex.LimitReading,
	"muse":    muse.LimitReading,
	"copilot": copilot.LimitReading,
}

// Build reads every covered kind once.
//
// Every kind whose account can be named is reported, with or without units: an epoch-only
// entry is what tells the CP that the account changed (or signed out) before anything of the
// new one was observed. Only a kind that cannot name its account at all is left out, without
// touching its epoch ledger. Units observed before the current account was first seen are
// dropped (limitepoch).
func Build(now time.Time) Snapshot {
	s := Snapshot{V: Version, Kinds: map[string]Kind{}}
	for kind, read := range sources {
		r := read()
		if r.NoIdentity {
			continue
		}
		st, ok := limitepoch.Current(kind, r.Identity, now)
		if !ok {
			continue
		}
		units := []Unit{}
		for _, w := range r.Windows {
			if !limitepoch.Admit(st.Since, w.ObservedAt) {
				continue
			}
			u := Unit{Unit: w.Unit, UsedPct: w.UsedPct, WindowMins: w.WindowMins,
				ObservedAt: w.ObservedAt.UTC().Format(time.RFC3339)}
			if !w.ResetsAt.IsZero() {
				u.ResetsAt = w.ResetsAt.UTC().Format(time.RFC3339)
			}
			units = append(units, u)
		}
		sort.Slice(units, func(i, j int) bool { return units[i].Unit < units[j].Unit })
		s.Kinds[kind] = Kind{Epoch: st.Epoch, EpochSince: st.SeenText(), Units: units}
	}
	return s
}

// cacheTTL bounds how often Build runs. The codex rollout scan reads files, and the reaper is
// not the only caller that might ask; 20 s is well under the reaper's one-minute sweep.
const cacheTTL = 20 * time.Second

var cache struct {
	sync.Mutex
	at   time.Time
	snap Snapshot
}

// Cached is Build at most once per cacheTTL.
func Cached(now time.Time) Snapshot {
	cache.Lock()
	defer cache.Unlock()
	if cache.at.IsZero() || now.Sub(cache.at) >= cacheTTL || now.Before(cache.at) {
		cache.snap, cache.at = Build(now), now
	}
	return cache.snap
}
