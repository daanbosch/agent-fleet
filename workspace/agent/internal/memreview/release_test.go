package memreview

import (
	"os"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/memoryx"
)

// rejectedAbout queues, rejects and returns a proposal about the user-scope memory name.
func rejectedAbout(t *testing.T, n int, name string, at time.Time) Proposal {
	t.Helper()
	p := uprop(n, apRun1, OpUpdate, name, "an idea nobody wanted "+name, 1)
	queue(t, p)
	if err := Reject(p.ID, at); err != nil {
		t.Fatal(err)
	}
	return p
}

func forgetThroughApproval(t *testing.T, n int, name string) {
	t.Helper()
	f := uprop(n, apRun2, OpForget, name, "", 1)
	queue(t, f)
	if _, err := Approve(f.ID, apNow); err != nil {
		t.Fatal(err)
	}
}

func seedMemory(t *testing.T, n int, name string) {
	t.Helper()
	s := uprop(n, apRun1, OpCreate, name, "v1", 0)
	queue(t, s)
	if _, err := Approve(s.ID, apNow); err != nil {
		t.Fatal(err)
	}
}

// A release that cannot be done when the memory is forgotten is not lost: the forget still
// succeeds, the failure is visible, and the next wake-up finishes it. A rejection made after the
// forget is about the memory as it is then and stays.
func TestAFailedReleaseIsRetriedAndKeepsLaterRejections(t *testing.T) {
	approvalEnv(t)
	memoryx.SetForgetHooks(ForgetHooks())
	seedMemory(t, 1, "topic")
	old := rejectedAbout(t, 2, "topic", time.Now().Add(-time.Hour))

	// The rejected directory cannot be written to.
	if err := os.Chmod(rejectedStore.Dir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(rejectedStore.Dir(), 0o700) })
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode")
	}
	forgetThroughApproval(t, 3, "topic")
	if !Rejected(old.Hash) {
		t.Fatal("the release should have failed in this setup (the control did not hold)")
	}
	if ReleasePending() != 1 {
		t.Fatalf("the failure is not visible: pending releases = %d", ReleasePending())
	}

	if err := os.Chmod(rejectedStore.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	// Someone rejects an idea about the forgotten memory after the forget.
	later := uprop(4, apRun2, OpCreate, "topic", "proposed again", 0)
	queue(t, later)
	if err := Reject(later.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	ReconcileReleases()
	if Rejected(old.Hash) {
		t.Fatal("the owed release was not finished")
	}
	if !Rejected(later.Hash) {
		t.Fatal("a rejection made after the forget was released with the old ones")
	}
	if ReleasePending() != 0 {
		t.Fatal("a finished release is still owed")
	}
}

// A stop between the forget's commit and the "done" call leaves the intent; the reconcile finishes
// it when the memory is gone, and drops it when the forget never committed.
func TestReconcileTellsACommittedForgetFromAFailedOne(t *testing.T) {
	approvalEnv(t)
	seedMemory(t, 1, "kept")
	seedMemory(t, 2, "gone")
	keptRej := rejectedAbout(t, 3, "kept", apNow)
	goneRej := rejectedAbout(t, 4, "gone", apNow)

	// Intents only (the commit-time call never came), as after a stop. "gone" is then forgotten
	// without the hooks, as the stopped process would have committed it.
	noteForgetIntent("user", "", "kept")
	noteForgetIntent("user", "", "gone")
	if ReleasePending() != 2 {
		t.Fatalf("intents = %d", ReleasePending())
	}
	forgetThroughApproval(t, 5, "gone") // no hooks installed: the approval alone commits it

	ReconcileReleases()
	if Rejected(goneRej.Hash) {
		t.Fatal("the hash of a memory that was forgotten was not released")
	}
	if !Rejected(keptRej.Hash) {
		t.Fatal("the hash of a memory that still exists was released")
	}
	if ReleasePending() != 0 {
		t.Fatalf("intents left = %d", ReleasePending())
	}
}

// With nothing remembered about a memory a forget writes nothing here: it does not depend on this
// package's directory.
func TestNoIntentWithoutRejections(t *testing.T) {
	approvalEnv(t)
	noteForgetIntent("user", "", "nothing-known")
	if ReleasePending() != 0 {
		t.Fatal("an intent was written for a memory nothing was rejected about")
	}
}
