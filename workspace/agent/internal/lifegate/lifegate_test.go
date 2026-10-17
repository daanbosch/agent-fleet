package lifegate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

const guard = 10 * time.Second

func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AF_SESSIONS_DIR", filepath.Join(t.TempDir(), "sessions"))
	ResetForTest()
	t.Cleanup(ResetForTest)
}

func mustEnter(t *testing.T, ctx context.Context, name string) *Lease {
	t.Helper()
	l, err := Enter(ctx, name)
	if err != nil {
		t.Fatalf("Enter(%s): %v", name, err)
	}
	return l
}

func waitWaiting(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(guard)
	for WritersWaiting(name) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no writer started waiting")
		}
		time.Sleep(time.Millisecond)
	}
}

// The exclusive side waits for every lease, and no lease is admitted from the moment it waits:
// otherwise a steady trickle of inputs could hold a move off forever.
func TestLockWaitsForLeasesAndHoldsNewOnesBack(t *testing.T) {
	isolate(t)
	l := mustEnter(t, context.Background(), "s")
	locked := make(chan *Exclusive, 1)
	go func() {
		x, _ := Lock(context.Background(), "s")
		locked <- x
	}()
	waitWaiting(t, "s")
	entered := make(chan *Lease, 1)
	go func() {
		l2, _ := Enter(context.Background(), "s")
		entered <- l2
	}()
	select {
	case <-locked:
		t.Fatal("Lock returned while a lease was held")
	case <-entered:
		t.Fatal("a new lease was admitted while a writer waited")
	case <-time.After(30 * time.Millisecond):
	}
	l.Release()
	l.Release() // idempotent: must not admit the writer twice or go negative
	x := <-locked
	select {
	case <-entered:
		t.Fatal("a lease was admitted while the writer held the gate")
	case <-time.After(30 * time.Millisecond):
	}
	x.Unlock()
	select {
	case l2 := <-entered:
		l2.Release()
	case <-time.After(guard):
		t.Fatal("the held-back lease was never admitted")
	}
}

// A writer that gives up lets the readers it held back through.
func TestLockGivingUpReleasesWaitingReaders(t *testing.T) {
	isolate(t)
	l := mustEnter(t, context.Background(), "s")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Lock(ctx, "s")
		done <- err
	}()
	waitWaiting(t, "s")
	entered := make(chan *Lease, 1)
	go func() {
		l2, _ := Enter(context.Background(), "s")
		entered <- l2
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Lock = %v, want canceled", err)
	}
	select {
	case l2 := <-entered:
		l2.Release()
	case <-time.After(guard):
		t.Fatal("a reader stayed blocked behind a writer that gave up")
	}
	l.Release()
}

// A lease on the context is borrowed, not taken again: a second take would queue behind the
// waiting writer, which waits for the first — the deadlock a Go RWMutex has with a recursive
// RLock.
func TestEnterBorrowsTheContextLease(t *testing.T) {
	isolate(t)
	outer := mustEnter(t, context.Background(), "s")
	go func() { x, _ := Lock(context.Background(), "s"); x.Unlock() }()
	waitWaiting(t, "s")
	inner := make(chan *Lease, 1)
	go func() {
		l, _ := Enter(WithLease(context.Background(), outer), "s")
		inner <- l
	}()
	select {
	case l := <-inner:
		if !l.Holds("s") {
			t.Fatal("the borrowed lease does not hold the session")
		}
		l.Release() // releases nothing: the writer must still wait for outer
	case <-time.After(guard):
		t.Fatal("the inner take deadlocked behind the waiting writer")
	}
	if WritersWaiting("s") != 1 {
		t.Fatal("releasing a borrowed lease let the writer in")
	}
	outer.Release()
	// A lease for another session on the context is not borrowed.
	if _, err := Enter(WithLease(context.Background(), outer), "other"); err != nil {
		t.Fatal(err)
	}
}

func TestEnterRefusesAClosedSession(t *testing.T) {
	isolate(t)
	session.WriteMeta(session.Meta{Name: "src", Moving: "dst"})
	var moved *MovedError
	if _, err := Enter(context.Background(), "src"); !errors.As(err, &moved) || moved.Target != "dst" || moved.Done {
		t.Fatalf("moving: %v", err)
	}
	session.WriteMeta(session.Meta{Name: "src", MovedTo: "dst"})
	if _, err := Enter(context.Background(), "src"); !errors.As(err, &moved) || !moved.Done {
		t.Fatalf("movedTo: %v", err)
	}
	if !Closed("src") {
		t.Fatal("Closed = false for a moved session")
	}
	// The in-memory state closes a session whose meta does not say so (yet, or unreadably).
	session.WriteMeta(session.Meta{Name: "src2"})
	x, err := Lock(context.Background(), "src2")
	if err != nil {
		t.Fatal(err)
	}
	x.SetClosed("dst2", false)
	x.Unlock()
	if _, err := Enter(context.Background(), "src2"); !errors.As(err, &moved) || moved.Target != "dst2" {
		t.Fatalf("closed in memory: %v", err)
	}
	ran := 0
	WhenReopened("src2", func() { ran++ })
	Reopen("src2")
	Reopen("src2")
	if ran != 1 {
		t.Fatalf("reopen callback ran %d times, want once", ran)
	}
	mustEnter(t, context.Background(), "src2").Release()
}

func TestCheckNeedsALiveLeaseOnTheSession(t *testing.T) {
	isolate(t)
	var none *Lease
	if !errors.Is(none.Check("s"), ErrNotHeld) {
		t.Fatal("a nil lease passed Check")
	}
	l := mustEnter(t, context.Background(), "s")
	if l.Check("other") == nil {
		t.Fatal("a lease on s passed Check for other")
	}
	l.Release()
	if l.Check("s") == nil {
		t.Fatal("a released lease passed Check")
	}
}

// After a restart the meta is the only record of a move, so one that exists but cannot be read
// refuses rather than admits.
func TestEnterRefusesAnUnreadableMeta(t *testing.T) {
	isolate(t)
	session.WriteMeta(session.Meta{Name: "torn"})
	if err := os.WriteFile(session.MetaPath("torn"), []byte(`{"name":"torn","mov`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Enter(context.Background(), "torn"); !errors.Is(err, ErrMetaUnreadable) {
		t.Fatalf("Enter = %v, want ErrMetaUnreadable", err)
	}
	if !Closed("torn") {
		t.Fatal("Closed = false for an unreadable meta")
	}
	// No meta at all is "no session": the caller answers that itself.
	mustEnter(t, context.Background(), "absent").Release()
}
