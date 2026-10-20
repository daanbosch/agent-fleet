package fastvm

// Stage profiling for the latency-critical flows (cold create, warm
// snapshot, warm launch). Every flow records a Profile — named stage
// durations plus a detail bag for flow-specific extras (e.g. the
// firecrackerctl TIME_* breakdown, snapshot write throughput). The last
// profiles are kept in an in-memory ring and surfaced through
// GET /api/v1/profile and `fastvm profile`, so a benchmark run can be
// analyzed without scraping timestamps out of journal lines.

import (
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProfileStage is one named segment of a flow.
type ProfileStage struct {
	Name string  `json:"name"`
	MS   float64 `json:"ms"`
}

// Profile is a completed flow trace.
type Profile struct {
	Event    string            `json:"event"`
	VMID     string            `json:"vmid,omitempty"`
	Name     string            `json:"name,omitempty"` // VM / template name, if known
	At       time.Time         `json:"at"`
	TotalMS  float64           `json:"total_ms"`
	Stages   []ProfileStage    `json:"stages"`
	DetailKV map[string]string `json:"detail,omitempty"`
}

// profileRingN bounds memory; warm-launch bursts never approach this.
const profileRingN = 128

var (
	profMu  sync.Mutex
	profRing [profileRingN]*Profile
	profCursor int
)

// RecordProfile appends p to the ring (drops the oldest).
func RecordProfile(p *Profile) {
	if p == nil {
		return
	}
	profMu.Lock()
	defer profMu.Unlock()
	profRing[profCursor] = p
	profCursor = (profCursor + 1) % profileRingN
}

// RecentProfiles returns the stored profiles, newest first.
func RecentProfiles() []*Profile {
	profMu.Lock()
	defer profMu.Unlock()
	out := make([]*Profile, 0, profileRingN)
	for i := 0; i < profileRingN; i++ {
		idx := (profCursor - 1 - i + profileRingN) % profileRingN
		if profRing[idx] != nil {
			out = append(out, profRing[idx])
		}
	}
	return out
}

// profBuilder accumulates stages for a running flow.
type profBuilder struct {
	event      string
	vmid, name string
	started    time.Time
	lastMark   time.Time
	stages     []ProfileStage
	detail     map[string]string
}

// newProf starts profiling flow event for vmid (may be empty ahead of
// allocation).
func newProf(event, vmid string) *profBuilder {
	now := time.Now()
	return &profBuilder{event: event, vmid: vmid, started: now, lastMark: now, detail: map[string]string{}}
}

// stage closes the previous segment, naming it name, and starts the next.
// Call it at each boundary; the final segment is closed by finish().
func (p *profBuilder) stage(name string) {
	if p == nil {
		return
	}
	now := time.Now()
	p.stages = append(p.stages, ProfileStage{Name: name, MS: msBetween(p.lastMark, now)})
	p.lastMark = now
}

// mark records a split without naming a stage yet: useful right before
// a section whose sub-parts were timed elsewhere (logs its own values).
func (p *profBuilder) skipTo(t time.Time) {
	if p == nil {
		return
	}
	p.lastMark = t
}

// addDetail stores a string detail value (e.g. method=reflink, io mbps).
func (p *profBuilder) addDetail(key, value string) {
	if p == nil {
		return
	}
	p.detail[key] = value
}

// addStage adds an externally measured stage (e.g. timing values parsed
// out of firecrackerctl output) without affecting the caller's marks.
func (p *profBuilder) addStage(name string, ms float64) {
	if p == nil {
		return
	}
	p.stages = append(p.stages, ProfileStage{Name: name, MS: ms})
}

// finish closes the running segment as name, logs the breakdown, records
// the profile and returns it. Safe on nil.
func (p *profBuilder) finish(name string) *Profile {
	if p == nil {
		return nil
	}
	p.stage(name)
	total := msBetween(p.started, time.Now())
	prof := &Profile{
		Event:    p.event,
		VMID:     p.vmid,
		Name:     p.name,
		At:       p.started,
		TotalMS:  total,
		Stages:   p.stages,
		DetailKV: p.detail,
	}
	RecordProfile(prof)

	attrs := make([]any, 0, 4+2*len(prof.Stages))
	attrs = append(attrs, "event", prof.Event, "vmid", prof.VMID, "total_ms", int(total))
	for _, s := range prof.Stages {
		attrs = append(attrs, s.Name+"_ms", int(s.MS))
	}
	keys := make([]string, 0, len(prof.DetailKV))
	for k := range prof.DetailKV {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		attrs = append(attrs, k, prof.DetailKV[k])
	}
	slog.Info("profile: "+prof.Event, attrs...)
	return prof
}

func msBetween(from, to time.Time) float64 {
	return float64(to.Sub(from).Microseconds()) / 1000.0
}

// msFromDuration is a convenience for addStage callers.
func msFromDuration(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}

// msFromString parses a millisecond string (firecrackerctl TIME_* values).
func msFromString(raw string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// ProfileSummary renders a compact human line for CLI use.
func (p *Profile) Summary() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-14s vmid=%s total=%dms [", p.Event, p.VMID, int(p.TotalMS))
	for i, s := range p.Stages {
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%s=%dms", s.Name, int(s.MS))
	}
	sb.WriteByte(']')
	if len(p.DetailKV) > 0 {
		keys := make([]string, 0, len(p.DetailKV))
		for k := range p.DetailKV {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sb.WriteString(" {")
		for i, k := range keys {
			if i > 0 {
				sb.WriteByte(' ')
			}
			fmt.Fprintf(&sb, "%s=%s", k, p.DetailKV[k])
		}
		sb.WriteByte('}')
	}
	return sb.String()
}
