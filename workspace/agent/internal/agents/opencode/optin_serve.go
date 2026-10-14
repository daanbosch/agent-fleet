package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/paths"
)

// Keeping the supervised serve in step with the opt-in switch (optin.go). A daemon is bound to
// the opencode it was started as: flipping the switch must replace it, and nothing else may
// ever talk to the old one as if it were the new.

// serveRecord says which opencode the daemon af started is. It exists only while the opt-in is
// the way the daemon was started, so a workspace that never opts in never gets the file.
type serveRecord struct {
	PID int    `json:"pid"`
	Key string `json:"key"`
}

func serveRecordFile() string { return filepath.Join(paths.AgentStateDir(), "opencode-serve.json") }

// noteServeStart records the daemon just started, or removes the record when it was started the
// default way.
func noteServeStart(pid int, key string) {
	f := serveRecordFile()
	if key == "" {
		_ = os.Remove(f)
		return
	}
	b, err := json.Marshal(serveRecord{PID: pid, Key: key})
	if err == nil {
		err = writeAtomic(f, b, 0o600)
	}
	if err != nil {
		log.Printf("opencode serve: could not record how the daemon was started, a restarted Agent cannot tell it apart: %v", err)
	}
}

// recordedServe is the record, and false when there is none or its process is not an
// `opencode serve` any more (a recycled pid must never be signalled).
func recordedServe() (serveRecord, bool) {
	b, err := os.ReadFile(serveRecordFile())
	if err != nil {
		return serveRecord{}, false
	}
	var r serveRecord
	if json.Unmarshal(b, &r) != nil || r.PID <= 0 || r.Key == "" {
		return serveRecord{}, false
	}
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(r.PID) + "/cmdline")
	if err != nil || !strings.Contains(string(cmdline), "serve") {
		return serveRecord{}, false
	}
	return r, true
}

// adoptedKey is how a daemon found already running was started: by the record when there is
// one, else the default way (an Agent without the opt-in could only start it that way).
func adoptedKey() string {
	if r, ok := recordedServe(); ok {
		return r.Key
	}
	return ""
}

// retireStale replaces a daemon that was started under the other setting of the switch. It
// runs before ensure takes the lock because the replacement drains (up to the drain timeout),
// which must not hold the lock the Console's polling needs. Restart is the same path a key
// change takes: running turns finish or are aborted, handles are re-resumed.
func (s *Supervisor) retireStale() error {
	want := launchKey()
	s.mu.Lock()
	up, have, owned := s.up, s.key, s.cmd != nil
	s.mu.Unlock()
	if !up {
		// Not supervised, but one may still be listening from before an Agent restart.
		if _, k := readRecordKey(); k == want || !healthy(serveAddr()) {
			return nil
		}
		have = adoptedKey()
		if have == want {
			return nil
		}
	} else if have == want {
		return nil
	}
	log.Printf("opencode serve: started as %q, now %q: replacing it", have, want)
	if owned {
		s.Restart("opencode major switched")
		return nil
	}
	return s.retireAdopted(serveAddr(), up)
}

// readRecordKey reads the record's key without the liveness check, for the cheap "is there
// anything to compare" test on every ensure.
func readRecordKey() (serveRecord, string) {
	b, err := os.ReadFile(serveRecordFile())
	if err != nil {
		return serveRecord{}, ""
	}
	var r serveRecord
	if json.Unmarshal(b, &r) != nil {
		return serveRecord{}, ""
	}
	return r, r.Key
}

// retireAdopted ends a daemon this Agent did not start itself, by the pid it recorded. Without a
// record the daemon cannot be signalled, and the caller says so rather than attach to the wrong
// opencode.
func (s *Supervisor) retireAdopted(addr string, supervised bool) error {
	r, ok := recordedServe()
	if !ok {
		return fmt.Errorf("opencode serve が %s で別のメジャー版のまま動いています。止めてからやり直してください", addr)
	}
	s.mu.Lock()
	s.stopping, s.up = true, false
	s.mu.Unlock()
	if supervised {
		s.drain(addr)
	}
	_ = syscall.Kill(r.PID, syscall.SIGTERM)
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) && healthy(addr) {
		time.Sleep(100 * time.Millisecond)
	}
	if healthy(addr) {
		_ = syscall.Kill(r.PID, syscall.SIGKILL)
		time.Sleep(500 * time.Millisecond)
	}
	if healthy(addr) {
		return errors.New("opencode serve を止められませんでした")
	}
	s.mu.Lock()
	s.stopping = false
	s.mu.Unlock()
	recordLifecycle("stopped", s.Generation(), "opencode major switched")
	go reconcileAll("opencode major switched")
	return nil
}
