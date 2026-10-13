package sessionx

// The idempotency ledger for create_session. It de-duplicates POST /sessions by idempotency
// key, killing the "the client timed out but the backend did create the session" race - an LLM
// reads that as a failure, retries, and ends up with a second independent session (the double
// launch in docs/log/36). Same idea as the ClientMessageID MsgLedger (agents.MsgLedger,
// docs/log/27 §4/§9.5), in a create-only lightweight form.
//
// A duplicate retry arrives seconds later, in the same process, so a key is normally kept in
// memory only, and a finished create is forgotten after createLedgerTTL. A create still
// running is never forgotten by time: answering "no such create" while it runs would let the
// caller start over beside it.
//
// A create that asks for a durable key (idempotency_durable; the Control Plane's launch from
// a handoff offer, issue #2040) also leaves a record on disk (createOutcomeDir), because its
// outcome may be asked about days later and after a restart: inflight while it runs, done
// with the answer once it succeeds, removed when it fails. POST /sessions-idempotency/{key}/
// abandon turns "no record" into an abandoned record, atomically with begin, so a create
// under that key that arrives later is refused rather than run behind a caller who has
// already started over. A record that cannot be read, or an inflight one left by an earlier
// process, is "unknown" — never "not created".

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/paths"
)

// createLedgerTTL is how long a FINISHED record is kept in memory. It must comfortably exceed
// the slowest launch's retry (worktree fetch plus CLI boot), or a retried create never
// converges onto the first one and a duplicate session appears. Short enough, though, that
// deliberately launching the same thing again later is not blocked. A durable record is not
// subject to it.
const createLedgerTTL = 3 * time.Minute

type createLedgerState int

const (
	createInflight  createLedgerState = iota // being created (the first request is doing the work)
	createDone                               // finished (body holds the wireSession JSON)
	createAbandoned                          // durable only: the caller gave the key up; a create under it is refused
	createUnknown                            // durable only: the record cannot be read, or an earlier process died mid-create
)

type createLedgerEntry struct {
	state createLedgerState
	body  []byte // the wireSession JSON replayed for idempotency (when state==createDone)
	at    time.Time
	// durable: the key also has a record on disk, which complete and fail keep in step.
	durable bool
}

// createSessionLedger is the in-memory ledger de-duplicating POST /sessions by idempotency key.
type createSessionLedger struct {
	mu sync.Mutex
	m  map[string]*createLedgerEntry
}

var createLedger = &createSessionLedger{m: map[string]*createLedgerEntry{}}

// begin claims the right to create under key. It returns (nil, false) when the caller owns
// this create and should carry on. When a record already exists it returns a snapshot of it
// and true, so the caller can replay a done one and refuse the others. A durable key that
// cannot be recorded is refused as unknown: running it unrecorded would break the promise
// the record makes.
func (l *createSessionLedger) begin(key string, durable bool) (createLedgerEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gcLocked()
	if e := l.m[key]; e != nil {
		return *e, true
	}
	if durable {
		if e, found := readCreateOutcome(key); found {
			return e, true
		}
		if err := writeCreateOutcome(key, createOutcomeRecord{State: outcomeInflight}); err != nil {
			log.Printf("create %s: recording the key failed: %v", key, err)
			return createLedgerEntry{state: createUnknown}, true
		}
	}
	l.m[key] = &createLedgerEntry{state: createInflight, at: time.Now(), durable: durable}
	return createLedgerEntry{}, false
}

// complete moves an inflight record to done and stores the body used for replays.
func (l *createSessionLedger) complete(key string, body []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.m[key]; e != nil {
		e.state = createDone
		e.body = body
		e.at = time.Now()
		if e.durable {
			// On failure the disk keeps "inflight" from this process, which a later process
			// reads as unknown: safe, if unhelpful.
			if err := writeCreateOutcome(key, createOutcomeRecord{State: outcomeDone, Body: body}); err != nil {
				log.Printf("create %s: recording the outcome failed: %v", key, err)
			}
		}
	}
}

// fail drops a record that ended while still inflight, letting a genuine retry through. done
// records are kept for replay.
func (l *createSessionLedger) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.m[key]; e != nil && e.state == createInflight {
		delete(l.m, key)
		if e.durable {
			if err := os.Remove(createOutcomePath(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Printf("create %s: removing the record of a failed create: %v", key, err)
			}
		}
	}
}

// lookup returns a snapshot of the current record (for GET /sessions-idempotency/{key}):
// memory first, then the disk. ok=false means neither knows the key.
func (l *createSessionLedger) lookup(key string) (createLedgerEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gcLocked()
	if e := l.m[key]; e != nil {
		return *e, true
	}
	return readCreateOutcome(key)
}

// abandon is lookup that, when nothing is known about key, records it as abandoned so no
// create under it can begin later. It holds the same lock as begin, so a create either began
// before (and is reported) or is refused after.
func (l *createSessionLedger) abandon(key string) createLedgerEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gcLocked()
	if e := l.m[key]; e != nil {
		return *e
	}
	if e, found := readCreateOutcome(key); found {
		return e
	}
	if err := writeCreateOutcome(key, createOutcomeRecord{State: outcomeAbandoned}); err != nil {
		log.Printf("create %s: recording the abandon failed: %v", key, err)
		return createLedgerEntry{state: createUnknown}
	}
	return createLedgerEntry{state: createAbandoned}
}

// gcLocked forgets finished in-memory records past the TTL. An inflight record stays until
// its create ends (complete or fail): a create that outlives the TTL is still running.
func (l *createSessionLedger) gcLocked() {
	cutoff := time.Now().Add(-createLedgerTTL)
	for k, e := range l.m {
		if e.state != createInflight && e.at.Before(cutoff) {
			delete(l.m, k)
		}
	}
}

const (
	outcomeInflight  = "inflight"
	outcomeDone      = "done"
	outcomeAbandoned = "abandoned"
)

// createOutcomeRecord is one durable key's record on disk.
type createOutcomeRecord struct {
	Key   string `json:"key"`
	State string `json:"state"`
	// Proc names the process that wrote an inflight record; another process reading it knows
	// that create was cut off, and cannot tell whether it made a session.
	Proc string          `json:"proc,omitempty"`
	Body json.RawMessage `json:"body,omitempty"`
	At   string          `json:"at"`
}

// createOutcomeProc tells this process's inflight records from an earlier one's.
var createOutcomeProc = strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(os.Getpid())

// createOutcomeDir holds the durable records. A variable so tests can point it elsewhere.
var createOutcomeDir = func() string { return filepath.Join(paths.AgentStateDir(), "session-create-outcomes") }

// createOutcomePath names a key's record by its hash: keys come from clients.
func createOutcomePath(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(createOutcomeDir(), hex.EncodeToString(sum[:])+".json")
}

// readCreateOutcome reads a key's durable record. found=false only when there is certainly
// none; a record that cannot be read or decoded is found, as unknown.
func readCreateOutcome(key string) (createLedgerEntry, bool) {
	b, err := os.ReadFile(createOutcomePath(key))
	if errors.Is(err, os.ErrNotExist) {
		return createLedgerEntry{}, false
	}
	var rec createOutcomeRecord
	if err != nil || json.Unmarshal(b, &rec) != nil || rec.Key != key {
		return createLedgerEntry{state: createUnknown}, true
	}
	switch {
	case rec.State == outcomeDone && len(rec.Body) > 0:
		return createLedgerEntry{state: createDone, body: rec.Body, durable: true}, true
	case rec.State == outcomeAbandoned:
		return createLedgerEntry{state: createAbandoned, durable: true}, true
	case rec.State == outcomeInflight && rec.Proc == createOutcomeProc:
		return createLedgerEntry{state: createInflight, durable: true}, true
	default:
		return createLedgerEntry{state: createUnknown, durable: true}, true
	}
}

// writeCreateOutcome replaces a key's record atomically (write, sync, rename), so a reader
// never sees half of one.
func writeCreateOutcome(key string, rec createOutcomeRecord) error {
	rec.Key, rec.At = key, time.Now().UTC().Format(time.RFC3339)
	if rec.State == outcomeInflight {
		rec.Proc = createOutcomeProc
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	path := createOutcomePath(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".outcome-*")
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), path)
}

// createIdempotencyKey decides the de-duplication key for a create request.
//   - An explicit key sent by the client (create_session over stdio MCP) is used as is. The
//     tool derives it deterministically from the conversation id plus the arguments, so an LLM
//     retrying with the same arguments reproduces the key and a timed-out retry converges onto
//     the first session.
//   - Without an explicit key it falls back to an intent fingerprint scoped by report_to (the
//     originating conversation), so a client that sends no key (CP MCP) cannot double-create on
//     a retry either. With no conversation scope (an interactive launch from the Console)
//     nothing is de-duplicated - a person is free to launch the same thing twice on purpose.
func createIdempotencyKey(r *CreateReq) string {
	if k := strings.TrimSpace(r.IdempotencyKey); k != "" {
		return k
	}
	if strings.TrimSpace(r.ReportTo) == "" {
		return ""
	}
	h := sha256.New()
	for _, f := range []string{
		r.ReportTo, r.Dir, r.Subdir, r.Kind, r.Model, r.Effort, r.Mode, r.Driver,
		r.InitialPrompt, r.Branch, r.NewBranch, r.RemoteURL, r.RepoName, r.Folder, r.Title,
		strconv.FormatBool(r.Worktree), strconv.FormatBool(r.UseExisting),
		strconv.FormatBool(r.AllowSharedWorkingCopy),
	} {
		h.Write([]byte(f))
		h.Write([]byte{0})
	}
	return "fp_" + hex.EncodeToString(h.Sum(nil))
}
