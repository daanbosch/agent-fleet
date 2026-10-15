package opencode

import (
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMigration answers the migration status the way opencode 2.0.29 does ("running" while the
// import goes, then "completed") and counts the throwaway servers EnsureMigrated starts.
type fakeMigration struct {
	running  atomic.Int32 // polls still answered "running"
	started  atomic.Int32
	stopped  atomic.Int32
	finished atomic.Bool
}

func useFakeMigration(t *testing.T, running int32) *fakeMigration {
	t.Helper()
	f := &fakeMigration{}
	f.running.Store(running)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pw, ok := r.BasicAuth(); !ok || pw != "pw" || r.URL.Path != "/api/experimental/migration/v1" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if f.running.Add(-1) >= 0 {
			_, _ = w.Write([]byte(`{"status":"running","progress":{"label":"Migrating sessions","numerator":1,"denominator":50}}`))
			return
		}
		f.finished.Store(true)
		_, _ = w.Write([]byte(`{"status":"completed"}`))
	}))
	t.Cleanup(srv.Close)
	old := startMigrationServe
	startMigrationServe = func(LaunchSpec) (string, string, <-chan struct{}, func(), error) {
		f.started.Add(1)
		return srv.URL, "pw", nil, func() { f.stopped.Add(1) }, nil
	}
	t.Cleanup(func() { startMigrationServe = old })
	return f
}

// The defect: a resume asked an opencode 2 whose import was still running, got a 404 for a
// conversation that exists and started a new one. EnsureMigrated must not return before the
// status says the import is over, and must not ask again once it has seen that.
func TestEnsureMigratedWaitsForTheImportOnce(t *testing.T) {
	optInHome(t)
	f := useFakeMigration(t, 3)
	spec := LaunchSpec{Maj: 2, Bin: "opencode"}

	if err := EnsureMigrated(spec); err != nil {
		t.Fatal(err)
	}
	if !f.finished.Load() {
		t.Fatal("returned while the import was still running")
	}
	if f.started.Load() != 1 || f.stopped.Load() != 1 {
		t.Fatalf("started %d stopped %d, want one throwaway server, stopped", f.started.Load(), f.stopped.Load())
	}
	if err := EnsureMigrated(spec); err != nil || f.started.Load() != 1 {
		t.Fatalf("second call: err %v, started %d; the marker should have answered", err, f.started.Load())
	}
}

func TestEnsureMigratedSkipsOpencode1(t *testing.T) {
	optInHome(t)
	f := useFakeMigration(t, 0)
	if err := EnsureMigrated(LaunchSpec{Maj: 1, Bin: "opencode"}); err != nil || f.started.Load() != 0 {
		t.Fatalf("err %v, started %d: a 1.x opencode does not migrate", err, f.started.Load())
	}
}

// Concurrent first starts (the serve, a chat run, a models listing) share one import.
func TestEnsureMigratedSerialisesConcurrentCallers(t *testing.T) {
	optInHome(t)
	f := useFakeMigration(t, 4)
	spec := LaunchSpec{Maj: 2, Bin: "opencode"}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := EnsureMigrated(spec); err != nil {
				t.Error(err)
			} else if !f.finished.Load() {
				t.Error("a caller returned before the import finished")
			}
		}()
	}
	wg.Wait()
	if f.started.Load() != 1 {
		t.Fatalf("%d throwaway servers started for five callers, want 1", f.started.Load())
	}
}

func TestEnsureMigratedTimeoutLeavesNoMarker(t *testing.T) {
	optInHome(t)
	useFakeMigration(t, 1<<20)
	old := migrationWait
	migrationWait = 1500 * time.Millisecond
	t.Cleanup(func() { migrationWait = old })
	if err := EnsureMigrated(LaunchSpec{Maj: 2, Bin: "opencode"}); err == nil {
		t.Fatal("a migration that never completes returned nil")
	}
	if _, err := os.Stat(migrationMarker()); err == nil {
		t.Fatal("a marker vouches for a database that was never migrated")
	}
}

// A throwaway server that dies (a broken install, a port taken) fails the caller at once instead
// of holding it for the whole wait.
func TestEnsureMigratedFailsFastWhenTheServerExits(t *testing.T) {
	optInHome(t)
	old := startMigrationServe
	t.Cleanup(func() { startMigrationServe = old })
	startMigrationServe = func(LaunchSpec) (string, string, <-chan struct{}, func(), error) {
		c := make(chan struct{})
		close(c)
		return "http://127.0.0.1:1", "pw", c, func() {}, nil
	}
	start := time.Now()
	if err := EnsureMigrated(LaunchSpec{Maj: 2, Bin: "opencode"}); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err %v after %v, want an immediate failure", err, time.Since(start))
	}
}
