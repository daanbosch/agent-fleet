package cursor

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/procx"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

// The shape measured on cursor-agent 2026.10.16 (managed, `sleep 120`): the ACP child leads its
// own group, the shell tool under it is a session leader, and the child sends no message for
// the whole sleep.
func cursorTable(withTool bool) map[int]procx.Info {
	tab := map[int]procx.Info{
		100: {PPID: 1, State: 'S', Comm: "cursor-agent", Pgrp: 100, Sid: 50},
		101: {PPID: 100, State: 'S', Comm: "node", Pgrp: 100, Sid: 50},
	}
	if withTool {
		tab[200] = procx.Info{PPID: 101, State: 'S', Comm: "bash", Pgrp: 200, Sid: 200}
		tab[201] = procx.Info{PPID: 200, State: 'S', Comm: "sleep", Pgrp: 200, Sid: 200}
	}
	return tab
}

func progressHandle() *threadHandle {
	return &threadHandle{name: "prog", cmd: &exec.Cmd{Process: &os.Process{Pid: 100}}}
}

func TestManagedProgressSilentToolKeepsHolding(t *testing.T) {
	h := progressHandle()
	h.act.Touch()
	at, _ := h.act.At()
	now := at.Add(2 * time.Hour) // two hours of silence from the child

	got, ok := h.progress(now, func() map[int]procx.Info { return cursorTable(true) })
	if !ok || !got.Equal(now) {
		t.Fatalf("a live tool under the child: got %v, %v; want now", got, ok)
	}
	got, ok = h.progress(now, func() map[int]procx.Info { return cursorTable(false) })
	if !ok || !got.Equal(at) {
		t.Fatalf("no tool: got %v, %v; want the last message, so the bound can lapse", got, ok)
	}
}

func TestManagedProgressNeverHeardAndNoToolCannotTell(t *testing.T) {
	h := progressHandle()
	if _, ok := h.progress(time.Now(), func() map[int]procx.Info { return cursorTable(false) }); ok {
		t.Fatal("answered for a child never heard from")
	}
}

func TestManagedProgressNeedsALiveHandle(t *testing.T) {
	if _, ok := (managedDriver{}).ManagedProgress(session.Meta{Name: "no-such-session"}); ok {
		t.Fatal("answered without a handle")
	}
}

func TestReadLoopMessagesStampTheHandle(t *testing.T) {
	h := progressHandle()
	h.onNotify("session/update", nil)
	if _, ok := h.act.At(); !ok {
		t.Fatal("a notification did not stamp the handle")
	}
}
