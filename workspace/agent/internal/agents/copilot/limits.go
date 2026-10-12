package copilot

import (
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/limitobs"
)

// peekToken is the gh token Token last read, without running gh: the limit snapshot must not
// wait on a subprocess.
func peekToken() string {
	tokenMu.Lock()
	defer tokenMu.Unlock()
	return tokenVal
}

// LimitReading is the copilot credit pools for the limit snapshot (ADR 0111 decision 6), as
// last fetched by the usage chip. Nothing is fetched here. A pool's used share is
// 100 - percent_remaining; unlimited pools have no quota to report and are left out.
//
// The cache is reported only while the token it was fetched with is still the current one.
// Token is refreshed by every chip poll and every copilot launch, so a gh login to another
// account stops the old pools from being reported as soon as either runs.
func LimitReading() limitobs.Reading {
	tok := peekToken()
	usageMu.Lock()
	defer usageMu.Unlock()
	if usageCached == nil || tok == "" || usageToken != tok || usageAt.IsZero() {
		return limitobs.Reading{}
	}
	r := limitobs.Reading{Identity: usageCached.User}
	var reset time.Time
	if t, err := time.Parse(time.RFC3339, usageCached.ResetsAt); err == nil {
		reset = t.UTC()
	}
	for _, q := range usageCached.Quotas {
		if q.Unlimited {
			continue
		}
		r.Windows = append(r.Windows, limitobs.Window{
			Unit: q.ID, UsedPct: 100 - q.RemainingPct, ResetsAt: reset, ObservedAt: usageAt.UTC(),
		})
	}
	return r
}
