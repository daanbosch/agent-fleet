package sessionx

import (
	"reflect"
	"runtime"
	"slices"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents/codex"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents/copilot"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents/cursor"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents/kiro"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents/lcpp"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents/muse"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents/opencode"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

// wantRuntime is the dispatch the per-kind switches in session_handlers.go made before
// agents.ManagedRuntime replaced them: kind -> the package functions ManagedAlive / managedBusy /
// managedInFlight / dropManagedRuntime / removeManagedLedger called. A nil row is "no runtime":
// false, false, false, no-op, no-op. Every registered kind has a row, and so do spellings that
// are not kinds, which the switches matched exactly and so answered as no runtime.
var wantRuntime = map[string]*agents.ManagedRuntimeOps{
	session.KindOpencode: {Alive: opencode.ManagedAlive, Busy: opencode.ManagedBusy, InFlight: opencode.ManagedInFlight, Drop: opencode.DropHandle, RemoveLedger: opencode.RemoveLedger},
	session.KindCodex:    {Alive: codex.ManagedAlive, Busy: codex.ManagedBusy, InFlight: codex.ManagedInFlight, Drop: codex.DropHandle, RemoveLedger: codex.RemoveLedger},
	session.KindCopilot:  {Alive: copilot.ManagedAlive, Busy: copilot.ManagedBusy, InFlight: copilot.ManagedInFlight, Drop: copilot.DropHandle, RemoveLedger: copilot.RemoveLedger},
	session.KindCursor:   {Alive: cursor.ManagedAlive, Busy: cursor.ManagedBusy, InFlight: cursor.ManagedInFlight, Drop: cursor.DropHandle, RemoveLedger: cursor.RemoveLedger},
	session.KindKiro:     {Alive: kiro.ManagedAlive, Busy: kiro.ManagedBusy, InFlight: kiro.ManagedInFlight, Drop: kiro.DropHandle, RemoveLedger: kiro.RemoveLedger},
	session.KindLcpp:     {Alive: lcpp.ManagedAlive, Busy: lcpp.ManagedBusy, InFlight: lcpp.ManagedInFlight, Drop: lcpp.DropHandle, RemoveLedger: lcpp.RemoveLedger},
	session.KindMuse:     {Alive: muse.ManagedAlive, Busy: muse.ManagedBusy, InFlight: muse.ManagedInFlight, Drop: muse.DropHandle, RemoveLedger: muse.RemoveLedger},
	session.KindClaude:   nil,
	session.KindAgy:      nil,
	session.KindShell:    nil,
	session.KindSSM:      nil,
	// Not kinds: no NormalizeKind, no AgentOf claude fallback.
	"":         nil,
	"Codex":    nil,
	"kiro-cli": nil,
	"OPENCODE": nil,
	"unknown":  nil,
}

func funcName(f any) string {
	if reflect.ValueOf(f).IsNil() {
		return "<nil>"
	}
	return runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
}

func sameFunc(a, b any) bool {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if va.IsNil() || vb.IsNil() {
		return va.IsNil() && vb.IsNil()
	}
	return va.Pointer() == vb.Pointer()
}

// Each kind dispatches to exactly the package functions the switches called, so a live handle
// gets the same answer it did; the table also has to name every registered kind.
func TestManagedRuntimeDispatchPinned(t *testing.T) {
	for _, k := range RegisteredKinds() {
		if _, ok := wantRuntime[k]; !ok {
			t.Errorf("registered kind %q has no row in wantRuntime; decide whether it has a Managed runtime", k)
		}
	}
	for kind, want := range wantRuntime {
		got, ok := managedRuntimeOf(kind)
		if ok != (want != nil) {
			t.Errorf("%q: has runtime = %v, want %v", kind, ok, want != nil)
			continue
		}
		if !ok {
			continue
		}
		for _, f := range []struct {
			op        string
			got, want any
		}{
			{"Alive", got.Alive, want.Alive},
			{"Busy", got.Busy, want.Busy},
			{"InFlight", got.InFlight, want.InFlight},
			{"Drop", got.Drop, want.Drop},
			{"RemoveLedger", got.RemoveLedger, want.RemoveLedger},
		} {
			if !sameFunc(f.got, f.want) {
				t.Errorf("%q: %s dispatches to %s, want %s", kind, f.op, funcName(f.got), funcName(f.want))
			}
		}
	}
}

// With no handle for the name — every row, runtime or not — the questions answer false and
// drop / ledger removal do nothing (and do not panic).
func TestManagedRuntimeWithoutHandle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for kind := range wantRuntime {
		m := session.Meta{Name: "no-such-managed-runtime", Kind: kind, Driver: session.DriverManaged}
		if ManagedAlive(m) || managedBusy(m) || managedInFlight(m) {
			t.Errorf("%q: alive/busy/inFlight = %v/%v/%v without a handle, want all false",
				kind, ManagedAlive(m), managedBusy(m), managedInFlight(m))
		}
		dropManagedRuntime(m)
		removeManagedLedger(m)
	}
}

type fakeRuntimeAgent struct {
	agents.Agent
	answer          bool
	dropped, purged []string
}

func (f *fakeRuntimeAgent) ManagedRuntime() agents.ManagedRuntimeOps {
	ask := func(string) bool { return f.answer }
	return agents.ManagedRuntimeOps{
		Alive: ask, Busy: ask, InFlight: ask,
		Drop:         func(n string) { f.dropped = append(f.dropped, n) },
		RemoveLedger: func(n string) { f.purged = append(f.purged, n) },
	}
}

// The sessionx wrappers hand the runtime's answer through untouched and pass the session name.
func TestManagedRuntimeWrappersPassThrough(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const kind = "rt-fake"
	f := &fakeRuntimeAgent{Agent: agentRegistry[session.KindClaude]}
	agentRegistry[kind] = f
	t.Cleanup(func() { delete(agentRegistry, kind) })
	m := session.Meta{Name: "rt-probe", Kind: kind, Driver: session.DriverManaged}
	for _, answer := range []bool{true, false} {
		f.answer = answer
		if ManagedAlive(m) != answer || managedBusy(m) != answer || managedInFlight(m) != answer {
			t.Errorf("answer %v: alive/busy/inFlight = %v/%v/%v", answer, ManagedAlive(m), managedBusy(m), managedInFlight(m))
		}
	}
	dropManagedRuntime(m)
	removeManagedLedger(m)
	if !slices.Equal(f.dropped, []string{"rt-probe"}) || !slices.Equal(f.purged, []string{"rt-probe"}) {
		t.Errorf("dropped %v, purged %v; want each [rt-probe]", f.dropped, f.purged)
	}
}

// A kind that can run Managed — ManagedOnly, ManagedDefault, or a managed driver — must
// implement agents.ManagedRuntime with every op set; a kind with no Managed route must not.
// Without it a new kind's Managed sessions read as stopped and idle, and stop / halt / archive
// leave the runtime running.
func TestManagedRuntimeFollowsCaps(t *testing.T) {
	for kind := range managedDrivers {
		if !slices.Contains(RegisteredKinds(), kind) {
			t.Errorf("managedDrivers has %q, which is not a registered kind", kind)
		}
	}
	for _, kind := range RegisteredKinds() {
		c := agentRegistry[kind].Caps()
		_, driver := managedDrivers[kind]
		canManaged := c.ManagedOnly || c.ManagedDefault || driver
		rt, ok := agentRegistry[kind].(agents.ManagedRuntime)
		if ok != canManaged {
			t.Errorf("%s: implements agents.ManagedRuntime = %v, but ManagedOnly=%v ManagedDefault=%v managed driver=%v",
				kind, ok, c.ManagedOnly, c.ManagedDefault, driver)
			continue
		}
		if !ok {
			continue
		}
		ops := rt.ManagedRuntime()
		if ops.Alive == nil || ops.Busy == nil || ops.InFlight == nil || ops.Drop == nil || ops.RemoveLedger == nil {
			t.Errorf("%s: ManagedRuntimeOps has a nil op: %+v", kind, ops)
		}
	}
}
