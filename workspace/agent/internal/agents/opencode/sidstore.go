package opencode

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/paths"
)

// slotSids is the slot -> opencode session id store. Plugins that predate the state-directory
// move kept writing <AgentConfigDir>/opencode-sid, and statemig copies that directory only once
// at boot, so a record captured afterwards lives only there. Read returns whichever of the two
// records is newer (the state directory wins a tie). It never moves, copies or deletes: nothing
// here can race a plugin's write, and a stale legacy record simply loses to any newer state
// write. Cost: one extra stat of the old directory per Read.
type slotSids struct{ agents.SidStore }

var sids = slotSids{agents.NewSidStore("opencode-sid")}

func legacySidPath(sid string) string {
	return filepath.Join(paths.AgentConfigDir(), "opencode-sid", sid)
}

// clearedPath is the tombstone Remove leaves when it cannot delete the old record: a legacy
// record that is not newer than it is ignored.
func (s slotSids) clearedPath(sid string) string { return s.SidStore.Path(sid + ".cleared") }

func (s slotSids) Read(sid string) string {
	if sid == "" {
		return ""
	}
	lp := legacySidPath(sid)
	lfi, err := os.Stat(lp)
	if err != nil || !lfi.Mode().IsRegular() {
		return s.SidStore.Read(sid)
	}
	if sfi, err := os.Stat(s.SidStore.Path(sid)); err == nil && !lfi.ModTime().After(sfi.ModTime()) {
		return s.SidStore.Read(sid)
	}
	if cfi, err := os.Stat(s.clearedPath(sid)); err == nil && !lfi.ModTime().After(cfi.ModTime()) {
		return s.SidStore.Read(sid)
	}
	if b, err := os.ReadFile(lp); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			return v
		}
	}
	return s.SidStore.Read(sid)
}

// Remove drops both copies. When the old one cannot be deleted (permissions, a flaky mount;
// no stat first, since "cannot examine" must not be read as "absent") a tombstone in the state
// directory keeps it from coming back until a plugin writes it again.
func (s slotSids) Remove(sid string) {
	s.SidStore.Remove(sid)
	if sid == "" {
		return
	}
	if err := os.Remove(legacySidPath(sid)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = s.SidStore.Put(sid+".cleared", "x")
	}
}
