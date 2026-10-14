package opencode

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/paths"
)

// slotSids is the slot → opencode session id store. Plugins that predate the state-directory
// move kept writing <AgentConfigDir>/opencode-sid, and statemig copies that directory only once
// at boot, so a record captured afterwards lives only there. Read honours such a record when it
// is newer than the state-directory one (or the latter is missing) and moves it across; with
// both present and the state one at least as new, the state directory wins.
type slotSids struct{ agents.SidStore }

var sids = slotSids{agents.NewSidStore("opencode-sid")}

func legacySidPath(sid string) string {
	return filepath.Join(paths.AgentConfigDir(), "opencode-sid", sid)
}

func (s slotSids) Read(sid string) string {
	if sid != "" {
		s.adoptLegacy(sid)
	}
	return s.SidStore.Read(sid)
}

// Remove drops both copies: a surviving legacy record would otherwise be adopted again on the
// next Read and resurrect a conversation the caller just cleared.
func (s slotSids) Remove(sid string) {
	s.SidStore.Remove(sid)
	if sid != "" {
		_ = os.Remove(legacySidPath(sid))
	}
}

func (s slotSids) adoptLegacy(sid string) {
	lp := legacySidPath(sid)
	lfi, err := os.Stat(lp)
	if err != nil || !lfi.Mode().IsRegular() {
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
	s.SidStore.Write(sid, val)
	_ = os.Remove(lp)
}
