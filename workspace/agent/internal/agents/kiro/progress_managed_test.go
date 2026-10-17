package kiro

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/procx"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

// The shape measured on kiro-cli 2.31.0 (managed, `sleep 120`): the runtime and its
// kiro-cli-chat child lead one group, the shell tool joins that group without a session of its
// own, and the runtime sends no message for 112 s of the sleep.
func kiroTable(withTool bool) map[int]procx.Info {
	tab := map[int]procx.Info{
		100: {PPID: 1, State: 'S', Comm: "kiro-cli", Pgrp: 100, Sid: 50},
		101: {PPID: 100, State: 'S', Comm: "kiro-cli-chat", Pgrp: 100, Sid: 50},
		102: {PPID: 101, State: 'S', Comm: "workspace-agent", Pgrp: 102, Sid: 50},
	}
	if withTool {
		tab[200] = procx.Info{PPID: 101, State: 'S', Comm: "sleep", Pgrp: 100, Sid: 50}
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

	got, ok := h.progress(now, func() map[int]procx.Info { return kiroTable(true) })
	if !ok || !got.Equal(now) {
		t.Fatalf("a live tool under the child: got %v, %v; want now", got, ok)
	}
	got, ok = h.progress(now, func() map[int]procx.Info { return kiroTable(false) })
	if !ok || !got.Equal(at) {
		t.Fatalf("no tool: got %v, %v; want the last message, so the bound can lapse", got, ok)
	}
}

func TestManagedProgressNeverHeardAndNoToolCannotTell(t *testing.T) {
	h := progressHandle()
	if _, ok := h.progress(time.Now(), func() map[int]procx.Info { return kiroTable(false) }); ok {
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
