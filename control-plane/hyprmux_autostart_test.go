package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/control-plane/internal/store"
)

// startCountingRuntime counts Start calls and fails the first failFirst of them.
type startCountingRuntime struct {
	*deadlineStub
	starts    int
	failFirst int
}

func (r *startCountingRuntime) Start(context.Context) error {
	r.starts++
	if r.starts <= r.failFirst {
		return errors.New("shim did not answer")
	}
	r.mu.Lock()
	r.state = "running"
	r.mu.Unlock()
	return nil
}

func hyprmuxAutostartFixture(t *testing.T, failFirst int) (*manager, *startCountingRuntime) {
	t.Helper()
	ctx := context.Background()
	st, mgr, tn, mv, _ := networkFixture(t)
	ws := store.Workspace{ID: store.NewID(), TenantID: tn.ID, MembershipID: mv.MembershipID,
		ContainerName: "af-yamada", Network: "n", DataDir: "d", AgentPort: "1", AgentToken: "t",
		State: "stopped", CreatedAt: store.NowTS()}
	if err := st.CreateWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	mgr.defaultTenantID = tn.ID
	rt := &startCountingRuntime{deadlineStub: &deadlineStub{state: "none"}, failFirst: failFirst}
	mgr.rtFactory = stubFactory{rt: rt}
	return mgr, rt
}

func TestHyprmuxAutostartStartsWorkspaceOnBoot(t *testing.T) {
	mgr, rt := hyprmuxAutostartFixture(t, 0)
	mgr.autostartHyprmuxWorkspaces(context.Background(), time.Millisecond)
	if rt.starts != 1 {
		t.Fatalf("Start calls = %d, want 1", rt.starts)
	}
}

func TestHyprmuxAutostartRetriesOnceAfterAFailedStart(t *testing.T) {
	mgr, rt := hyprmuxAutostartFixture(t, 1)
	mgr.autostartHyprmuxWorkspaces(context.Background(), time.Millisecond)
	if rt.starts != 2 {
		t.Fatalf("Start calls = %d, want 2 (one failed start, one retry)", rt.starts)
	}
}

func TestHyprmuxAutostartGivesUpAfterOneRetry(t *testing.T) {
	mgr, rt := hyprmuxAutostartFixture(t, 2)
	mgr.autostartHyprmuxWorkspaces(context.Background(), time.Millisecond)
	if rt.starts != 2 {
		t.Fatalf("Start calls = %d, want 2: no third attempt", rt.starts)
	}
}

func TestHyprmuxAutostartIsOptIn(t *testing.T) {
	t.Setenv("AF_HYPRMUX_AUTOSTART", "")
	if hyprmuxAutostartOn("hyprmux-host") {
		t.Fatal("autostart is on without AF_HYPRMUX_AUTOSTART=1")
	}
	t.Setenv("AF_HYPRMUX_AUTOSTART", "1")
	if !hyprmuxAutostartOn("hyprmux-host") {
		t.Fatal("AF_HYPRMUX_AUTOSTART=1 does not enable autostart on hyprmux-host")
	}
	if hyprmuxAutostartOn("native") {
		t.Fatal("autostart applies beyond the hyprmux-host profile")
	}
}
