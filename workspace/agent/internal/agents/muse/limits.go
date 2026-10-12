package muse

import (
	"errors"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/limitobs"
)

// LimitReading is the muse quota for the limit snapshot (ADR 0111 decision 6): the newest
// observation any host delivered, with the host's own observation time. It never asks a host
// (readQuota does an RPC), so a workspace whose muse sessions have not completed a turn yet
// reports nothing, which reads as unknown, not as 0%.
func LimitReading() limitobs.Reading {
	r := limitobs.Reading{Identity: accountIdentity()}
	u := lastQuota()
	if u == nil || u.ObservedAtMs <= 0 {
		return r
	}
	at := time.UnixMilli(u.ObservedAtMs).UTC()
	if u.Window.ResetsAtMs > 0 {
		r.Windows = append(r.Windows, limitobs.Window{
			Unit: "window", UsedPct: float64(u.Window.UsedPercent), ResetsAt: time.UnixMilli(u.Window.ResetsAtMs).UTC(),
			WindowMins: int(u.Window.WindowDurationMins), ObservedAt: at,
		})
	}
	if u.Weekly.ResetsAtMs > 0 {
		r.Windows = append(r.Windows, limitobs.Window{
			Unit: "weekly", UsedPct: float64(u.Weekly.UsedPercent), ResetsAt: time.UnixMilli(u.Weekly.ResetsAtMs).UTC(),
			WindowMins: 7 * 24 * 60, ObservedAt: at,
		})
	}
	return r
}

var (
	identityMu   sync.Mutex
	identityLast string
)

// accountIdentity is the signed-in Meta account's e-mail from auth.json. A file that exists
// but does not parse keeps the last answer, so one torn read is not taken for a sign-in to
// another account.
func accountIdentity() string {
	identityMu.Lock()
	defer identityMu.Unlock()
	if _, err := os.Stat(authPath()); errors.Is(err, fs.ErrNotExist) {
		identityLast = ""
		return ""
	}
	c := readCredential()
	if !c.Present {
		return identityLast
	}
	identityLast = c.Email
	return identityLast
}
