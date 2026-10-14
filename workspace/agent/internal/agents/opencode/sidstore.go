package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/paths"
)

// legacyScanEvery bounds how often the old directory is looked at. Plugins that predate the
// state-directory move live as long as the opencode pane they run in and keep writing
// <AgentConfigDir>/opencode-sid, so the check cannot be a one-off at boot; but ~/.config may be
// a network mount (ADR 0087), and Read sits on polled paths. One directory listing per interval,
// whatever the number of readers, is the whole cost. A record from such a plugin is therefore
// seen up to this long after it is written.
const legacyScanEvery = 30 * time.Second

// slotSids is the slot -> opencode session id store. A record a pre-fix plugin left in the old
// directory is adopted (moved into the state directory) when it is newer than the state one or
// that one is missing; otherwise the state directory wins. mu orders adoption against this
// process's Write and Remove, so an adoption never resurrects what Remove just cleared; a
// pre-fix plugin writing in the same instant is outside it (its next event rewrites the record).
type slotSids struct {
	agents.SidStore
	mu       sync.Mutex
	lastScan time.Time
}

var sids = &slotSids{SidStore: agents.NewSidStore("opencode-sid")}

func legacySidDir() string { return filepath.Join(paths.AgentConfigDir(), "opencode-sid") }

// clearedPath is the tombstone Remove leaves when it cannot delete the old record: adoption
// ignores a legacy file that is not newer than it.
func (s *slotSids) clearedPath(sid string) string { return s.SidStore.Path(sid + ".cleared") }

func (s *slotSids) Read(sid string) string {
	if sid != "" {
		s.scanLegacy(false)
	}
	return s.SidStore.Read(sid)
}

func (s *slotSids) Write(sid, val string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SidStore.Write(sid, val)
}

func (s *slotSids) Remove(sid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SidStore.Remove(sid)
	lp := filepath.Join(legacySidDir(), sid)
	if _, err := os.Stat(lp); err != nil {
		return
	}
	if os.Remove(lp) != nil {
		// Could not delete it (ownership, a flaky mount): make the clear durable instead.
		_ = s.SidStore.Put(sid+".cleared", "x")
	}
}

// scanLegacy adopts every eligible record in the old directory, at most once per interval
// unless force is set (tests).
func (s *slotSids) scanLegacy(force bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !force && time.Since(s.lastScan) < legacyScanEvery {
		return
	}
	s.lastScan = time.Now()
	ents, err := os.ReadDir(legacySidDir())
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		s.adopt(e.Name())
	}
}

// adopt runs under mu. The old file is deleted only once the state-directory write has
// succeeded; a failed write leaves it for the next scan.
func (s *slotSids) adopt(sid string) {
	lp := filepath.Join(legacySidDir(), sid)
	lfi, err := os.Stat(lp)
	if err != nil || !lfi.Mode().IsRegular() {
		return
	}
	if cfi, err := os.Stat(s.clearedPath(sid)); err == nil && !lfi.ModTime().After(cfi.ModTime()) {
		return
	}
	b, err := os.ReadFile(lp)
	if err != nil {
		return
	}
	val := strings.TrimSpace(string(b))
	if val == "" {
		return
	}
	if sfi, err := os.Stat(s.SidStore.Path(sid)); err == nil && !lfi.ModTime().After(sfi.ModTime()) {
		return
	}
	if s.SidStore.Put(sid, val) != nil {
		return
	}
	_ = os.Remove(lp)
}
