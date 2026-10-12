// Package limitobs is the shape in which an agent kind hands its last captured quota reading
// to the limit snapshot (ADR 0111 decision 6). It is a leaf package so the kind packages can
// return it without importing the snapshot builder, which imports them.
package limitobs

import "time"

// Window is one quota unit as the provider last reported it: a rate-limit window, or a
// credit pool. It is the raw reading. A kind never rolls a past window forward here: what
// happens past ResetsAt is the reader's call ("reset not yet observed"), and a value made
// up on the Agent would look like an observation on every screen downstream.
type Window struct {
	// Unit names the quota within the kind: "5h" / "7d" for claude and codex, "window" /
	// "weekly" for muse, the pool id for copilot.
	Unit    string
	UsedPct float64
	// ResetsAt is zero when the provider named no reset.
	ResetsAt time.Time
	// WindowMins is the window's length when the provider states it; 0 = not stated.
	WindowMins int
	// ObservedAt is when the provider last really reported this value. A failed refresh, a
	// cache read or a re-render of an unchanged reading must never move it: a reading that
	// carries a younger time than it has is exactly the lie the board exists to avoid.
	ObservedAt time.Time
}

// Reading is one kind's captured state. Identity names the provider account now signed in
// ("" = signed out, which is an account state of its own). It never leaves the Agent: the
// snapshot turns it into an opaque epoch.
//
// NoIdentity says the account cannot be named at the moment at all (copilot before gh was
// ever asked for a token). The kind is then left out, and its epoch ledger is not touched.
type Reading struct {
	Identity   string
	NoIdentity bool
	Windows    []Window
}
