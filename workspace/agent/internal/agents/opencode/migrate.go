package opencode

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/paths"
)

// The first opencode 2 process to open a database that still holds 1.x data migrates it in place
// and imports the 1.x sessions. /api/info answers within a second, but the import keeps running
// (measured on 2.0.29: 50 conversations x 1000 messages, 253 MB, ~25 s): until it completes,
// GET /api/session/{id} answers 404 for a conversation that exists, and a create succeeds. A
// resume that trusts that 404 starts a fresh conversation and overwrites the slot's recorded
// session id. So no af-started opencode 2 process touches a database before EnsureMigrated has
// seen the migration complete.

// migrationWait bounds the wait; a var so tests can shorten it.
var migrationWait = 30 * time.Minute

// startMigrationServe starts a throwaway `opencode serve` on a free loopback port against the
// database spec launches with, and returns its address, the basic-auth password, a channel that
// closes when the process exits and a stop func. A var so tests can stand in for the binary.
var startMigrationServe = func(spec LaunchSpec) (addr, password string, exited <-chan struct{}, stop func(), err error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", nil, nil, err
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	_ = l.Close()
	password, err = newServePassword()
	if err != nil {
		return "", "", nil, nil, err
	}
	cmd := exec.Command(spec.Bin, "serve", "--hostname", "127.0.0.1", "--port", port)
	cmd.Env = append(append(withoutEnv(os.Environ(), "AF_SESSION_SID", "AF_SESSION_NAME"), spec.processEnv()...),
		"OPENCODE_SERVER_USERNAME="+serveUser, "OPENCODE_SERVER_PASSWORD="+password)
	cmd.Dir = paths.HomeDir()
	if err := cmd.Start(); err != nil {
		return "", "", nil, nil, err
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	stop = func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	return "http://127.0.0.1:" + port, password, done, stop, nil
}

func migrationMarker() string { return filepath.Join(paths.AgentStateDir(), "opencode-v2-migrated") }

// migrationKey names what the marker vouches for: this binary version on this database.
func migrationKey(spec LaunchSpec) string {
	db := filepath.Join(paths.HomeDir(), ".local", "share", "opencode", "opencode.db")
	if spec.V2 {
		db = V2DBPath()
	}
	return spec.version() + "\n" + db + "\n"
}

// EnsureMigrated returns once the database spec's opencode will open has been migrated by an
// opencode 2 (immediately for 1.x, and for a database already vouched for). Concurrent callers,
// across processes, queue on one lock, so the import runs once and nobody opens the database
// while it does. A failure leaves no marker and is retried by the next caller.
func EnsureMigrated(spec LaunchSpec) error {
	if spec.Maj < 2 {
		return nil
	}
	key := migrationKey(spec)
	done := func() bool { b, err := os.ReadFile(migrationMarker()); return err == nil && string(b) == key }
	if done() {
		return nil
	}
	if err := os.MkdirAll(paths.AgentStateDir(), 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(migrationMarker()+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	deadline := time.Now().Add(migrationWait)
	for {
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("opencode 2 is still migrating the database (another process holds %s)", migrationMarker()+".lock")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if done() {
		return nil
	}
	addr, password, exited, stop, err := startMigrationServe(spec)
	if err != nil {
		return fmt.Errorf("opencode 2 database migration: %w", err)
	}
	defer stop()
	if err := waitMigrated(addr, password, exited, deadline); err != nil {
		return err
	}
	return MarkMigrated(spec)
}

// MarkMigrated records that the database spec's opencode opens has been migrated. Tests with a
// stand-in binary use it to skip the gate.
func MarkMigrated(spec LaunchSpec) error {
	return writeAtomic(migrationMarker(), []byte(migrationKey(spec)), 0o644)
}

// waitMigrated polls GET /api/experimental/migration/v1 until it stops answering "running".
// Measured shapes: {"status":"running","progress":{…}} and {"status":"completed"}. Anything
// that is not "running" ends the wait: a status this code does not know must not hold every
// launch for half an hour.
func waitMigrated(addr, password string, exited <-chan struct{}, deadline time.Time) error {
	c := &http.Client{Timeout: 5 * time.Second}
	var last string
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return fmt.Errorf("opencode 2 exited during its database migration (last status %q)", last)
		default:
		}
		req, err := http.NewRequest("GET", addr+"/api/experimental/migration/v1", nil)
		if err != nil {
			return err
		}
		req.SetBasicAuth(serveUser, password)
		if res, err := c.Do(req); err == nil {
			var st struct {
				Status string `json:"status"`
			}
			ok := res.StatusCode == 200 && json.NewDecoder(res.Body).Decode(&st) == nil
			res.Body.Close()
			if ok {
				last = st.Status
				if st.Status != "running" {
					return nil
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("opencode 2 database migration did not finish in time (last status %q)", last)
}
