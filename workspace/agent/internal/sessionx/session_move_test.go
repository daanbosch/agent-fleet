package sessionx

// The move endpoint (ADR 0115 P2a): the end-to-end order of begin, snapshot, file, create,
// commit and stop, and every way it can stop short. The source is the managed codex fake of
// session_move_gate_test.go; the snapshot and, unless a test says otherwise, the target's create
// are stand-ins, so the file's content and the create's outcome are the test's to choose.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/lifegate"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/paths"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/transcript"
)

type moveRun struct {
	d   *moveFakeDriver
	src session.Meta
	// turns is what the stand-in snapshot reads as the conversation.
	turns   []transcript.Turn
	queued  []agents.QueueItem
	creates []CreateReq
	stops   []string
	// create, when set, answers the target's create instead of the default success.
	create func(req CreateReq) (int, []byte)
}

func moveRunEnv(t *testing.T, name string) *moveRun {
	t.Helper()
	d, m := moveEnv(t, name)
	r := &moveRun{d: d, src: m, turns: []transcript.Turn{
		{Role: "user", Text: "fix the flaky login test", Parts: []transcript.Part{{Kind: "text", Text: "fix the flaky login test"}}},
		{Role: "assistant", Parts: []transcript.Part{{Kind: "text", Text: "Found the race in the fixture."}}},
	}}
	prevRead, prevCreate, prevStop, prevWrite := readMoveSource, createMoveTarget, stopMoveSource, writeMoveHandover
	readMoveSource = func(m session.Meta) HandoverInput {
		return HandoverInput{Source: m, Turns: r.turns, QueuedItems: r.queued, PeerMessages: agents.PendingPeers(m.Name)}
	}
	createMoveTarget = func(_ context.Context, req CreateReq) (int, []byte) {
		r.creates = append(r.creates, req)
		if r.create != nil {
			return r.create(req)
		}
		return createFakeTarget(req)
	}
	stopMoveSource = func(m session.Meta) error {
		r.stops = append(r.stops, m.Name)
		return nil
	}
	t.Cleanup(func() {
		readMoveSource, createMoveTarget, stopMoveSource, writeMoveHandover = prevRead, prevCreate, prevStop, prevWrite
	})
	return r
}

// createFakeTarget is a create that succeeds: the target's meta is written, as the real create
// does before it answers.
func createFakeTarget(req CreateReq) (int, []byte) {
	m := session.Meta{Name: req.name, Dir: req.Dir, Kind: req.Kind, Driver: req.Driver, Title: req.Title}
	session.WriteMeta(m)
	b, _ := json.Marshal(map[string]string{"name": req.name, "kind": req.Kind})
	return http.StatusCreated, b
}

func postMove(t *testing.T, name, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+name+"/move"+path, strings.NewReader(body))
	req.SetPathValue("name", name)
	rec := httptest.NewRecorder()
	switch path {
	case "":
		HandleSessionMove(rec, req)
	case "/retry":
		HandleSessionMoveRetry(rec, req)
	case "/cancel":
		HandleSessionMoveCancel(rec, req)
	case "/reopen":
		HandleSessionMoveReopen(rec, req)
	default:
		t.Fatalf("no handler for %s", path)
	}
	return rec
}

type moveAnswer struct {
	Source        string `json:"source"`
	File          string `json:"file"`
	SourceStopped bool   `json:"sourceStopped"`
	Earlier       []string
	Target        struct {
		Name string `json:"name"`
	} `json:"target"`
	Error moveErrBody `json:"error"`
}

func decodeMove(t *testing.T, rec *httptest.ResponseRecorder) moveAnswer {
	t.Helper()
	var a moveAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatalf("answer %d %q: %v", rec.Code, rec.Body.String(), err)
	}
	return a
}

func mustMove(t *testing.T, name, kind string) moveAnswer {
	t.Helper()
	rec := postMove(t, name, "", `{"kind":"`+kind+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("move %s → %s: %d %s", name, kind, rec.Code, rec.Body.String())
	}
	return decodeMove(t, rec)
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMoveWritesTheFileCreatesTheTargetAndStopsTheSource(t *testing.T) {
	r := moveRunEnv(t, "mvok")
	if err := os.WriteFile(filepath.Join(r.src.Dir, "AGENTS.md"), []byte("rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := mustMove(t, "mvok", session.KindClaude)
	target := a.Target.Name
	if len(r.creates) != 1 || r.creates[0].name != target || r.creates[0].Kind != session.KindClaude || r.creates[0].Dir != r.src.Dir {
		t.Fatalf("creates = %+v, answer target %q", r.creates, target)
	}
	want := filepath.Join(paths.SessionWorkDir(target), "handover-mvok.md")
	if a.File != want {
		t.Fatalf("file = %q, want %q (the target's own directory)", a.File, want)
	}
	md := readFile(t, a.File)
	for _, s := range []string{"# Where it stopped", "fix the flaky login test", "Found the race in the fixture.", "AGENTS.md"} {
		if !strings.Contains(md, s) {
			t.Errorf("the file lacks %q:\n%s", s, md)
		}
	}
	prompt := r.creates[0].InitialPrompt
	for _, s := range []string{a.File, "read the head", "Never open the source session's own transcript"} {
		if !strings.Contains(prompt, s) {
			t.Errorf("the first prompt lacks %q:\n%s", s, prompt)
		}
	}
	cur, _ := session.ReadMeta("mvok")
	if cur.MovedTo != target || cur.Moving != "" {
		t.Fatalf("source meta = moving %q movedTo %q, want movedTo %s", cur.Moving, cur.MovedTo, target)
	}
	if len(r.stops) != 1 || r.stops[0] != "mvok" || !a.SourceStopped {
		t.Errorf("stops = %v (sourceStopped %v), want the source stopped once", r.stops, a.SourceStopped)
	}
	if _, ok := moveRecords.Read("mvok"); ok {
		t.Error("the move's record outlived its commit")
	}
	assertMovedRefusal(t, postInput(t, "mvok", `{"prompt":"one more thing"}`), target)
	assertNothingSent(t, r.d)
}

// Every failure before a target can exist leaves the source as it was: input open, nothing
// stopped, nothing sent, its spool and its queue untouched, and no file in a directory of a
// session that does not exist.
func TestMoveFailuresBeforeTheTargetRollBack(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(r *moveRun)
		status   int
		wantCode string
	}{
		{"empty transcript", func(r *moveRun) { r.turns = nil }, http.StatusConflict, "move_empty_transcript"},
		{"write error", func(r *moveRun) {
			writeMoveHandover = func(string, string, string) (string, error) { return "", errors.New("disk full") }
		}, http.StatusInternalServerError, "move_write_failed"},
		{"refused create", func(r *moveRun) {
			r.create = func(CreateReq) (int, []byte) {
				return http.StatusBadRequest, []byte(`{"error":{"code":"bad_model","message":"no such model"}}`)
			}
		}, http.StatusBadRequest, "bad_model"},
		{"create failed with nothing started", func(r *moveRun) {
			r.create = func(CreateReq) (int, []byte) {
				return http.StatusInternalServerError, []byte(`{"error":{"code":"runtime_failed","message":"boom"}}`)
			}
		}, http.StatusInternalServerError, "runtime_failed"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name := "mvrb" + string(rune('a'+i))
			r := moveRunEnv(t, name)
			// An ordinary user input still in the driver's queue, and a peer message waiting in
			// the spool: neither may be lost by a move that did not happen.
			r.queued = []agents.QueueItem{{ID: "q1", Text: "also update the changelog"}}
			if err := agents.PutPendingPeer(name, agents.PendingPeer{ID: "pp1", From: "peer", Intent: "question",
				Message: "is the fixture shared?", QueuedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			c.setup(r)
			rec := postMove(t, name, "", `{"kind":"claude"}`)
			if a := decodeMove(t, rec); rec.Code != c.status || a.Error.Code != c.wantCode {
				t.Fatalf("got %d %s, want %d %s", rec.Code, rec.Body.String(), c.status, c.wantCode)
			}
			cur, _ := session.ReadMeta(name)
			if cur.Moving != "" || cur.MovedTo != "" {
				t.Fatalf("source meta = moving %q movedTo %q after a rollback", cur.Moving, cur.MovedTo)
			}
			if len(r.stops) != 0 {
				t.Errorf("the source was stopped (%v): a stop drops a cursor/kiro queue", r.stops)
			}
			if p := agents.PendingPeers(name); len(p) != 1 || p[0].ID != "pp1" {
				t.Errorf("pending peers = %+v, want pp1 still spooled", p)
			}
			if _, ok := moveRecords.Read(name); ok {
				t.Error("a rolled-back move left its record")
			}
			for _, cr := range r.creates {
				if _, err := os.Stat(paths.SessionWorkDir(cr.name)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("the target's directory survived the rollback: %v", err)
				}
			}
			assertNothingSent(t, r.d)
			// Input is open again, and the queue's input reaches the runtime as it would have.
			if rec := postInput(t, name, `{"prompt":"also update the changelog"}`); rec.Code != http.StatusOK {
				t.Fatalf("input after the rollback: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// The usage-limit booking fires while the move holds input closed, the move then rolls back:
// the spent booking is booked again rather than left as a dead id in the episode.
func TestBookingThatFiredDuringARolledBackMoveIsBookedAgain(t *testing.T) {
	f := newRateLimitFixture(t)
	now := time.Now()
	r := moveRunEnv(t, "mvbook")
	setRateLimitPref(t, true)
	if err := RateLimitStates.Write("mvbook", rateLimitState{At: now.Add(-time.Hour).Format(time.RFC3339),
		ResumeAt: now.Add(-time.Minute).Format(time.RFC3339), ScheduleID: "sch_spent", ScheduleTries: 1}); err != nil {
		t.Fatal(err)
	}
	r.create = func(req CreateReq) (int, []byte) {
		assertMovedRefusal(t, postInput(t, "mvbook",
			`{"prompt":"`+rateLimitResumePrompt()+`","source":"schedule","schedule_id":"sch_spent"}`), req.name)
		return http.StatusBadRequest, []byte(`{"error":{"code":"bad_model","message":"no"}}`)
	}
	if rec := postMove(t, "mvbook", "", `{"kind":"claude"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("move: %d %s", rec.Code, rec.Body.String())
	}
	st := stateOf(t, "mvbook")
	if f.scheduled != 1 || st.ScheduleID != "sch_test" || st.FiredDuringMove != "" {
		t.Fatalf("after the rollback: booked %d, episode %+v; want the resume booked again", f.scheduled, st)
	}
}

// A peer message waiting in the source's spool is carried into the file exactly once and taken
// out of the spool, so it is neither lost nor delivered to the stopped source later.
func TestSpooledPeerMessagesEnterTheFileOnce(t *testing.T) {
	moveRunEnv(t, "mvpeer")
	if err := agents.PutPendingPeer("mvpeer", agents.PendingPeer{ID: "pp1", From: "reviewer", Intent: "question",
		Message: "did you rerun the suite with -race?", QueuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	a := mustMove(t, "mvpeer", session.KindClaude)
	if n := strings.Count(readFile(t, a.File), "did you rerun the suite with -race?"); n != 1 {
		t.Fatalf("the peer message appears %d times in the file, want once", n)
	}
	if p := agents.PendingPeers("mvpeer"); len(p) != 0 {
		t.Fatalf("pending peers after the move = %+v, want the spool emptied", p)
	}
}

// A stopped Managed cursor source has no conversation left anywhere; the move refuses it with a
// reason instead of writing an empty file.
func TestMoveRefusesAStoppedManagedCursorSource(t *testing.T) {
	moveRunEnv(t, "mvcur-unused")
	session.WriteMeta(session.Meta{Name: "mvcur", Dir: t.TempDir(), Kind: session.KindCursor, Driver: session.DriverManaged})
	t.Cleanup(func() { lifegate.Reopen("mvcur") })
	rec := postMove(t, "mvcur", "", `{"kind":"claude"}`)
	if a := decodeMove(t, rec); rec.Code != http.StatusConflict || a.Error.Code != "move_cursor_stopped" {
		t.Fatalf("got %d %s, want 409 move_cursor_stopped", rec.Code, rec.Body.String())
	}
	if cur, _ := session.ReadMeta("mvcur"); cur.Moving != "" {
		t.Fatalf("moving = %q after the refusal", cur.Moving)
	}
}

// Only another kind that can read the file is a target (decisions 5 and 9).
func TestMoveRefusesTargetsThatCannotTakeIt(t *testing.T) {
	r := moveRunEnv(t, "mvkind")
	cases := []struct{ kind, code string }{
		{session.KindCodex, "move_same_kind"},
		{session.KindLcpp, "move_target_unsupported"},
		{session.KindCursor, "move_target_unsupported"},
		{session.KindKiro, "move_target_unsupported"},
		{session.KindShell, "move_target_unsupported"},
		{"nosuchkind", "bad_kind"},
	}
	for _, c := range cases {
		rec := postMove(t, "mvkind", "", `{"kind":"`+c.kind+`"}`)
		a := decodeMove(t, rec)
		if rec.Code != http.StatusBadRequest || a.Error.Code != c.code {
			t.Errorf("%s: got %d %s, want 400 %s", c.kind, rec.Code, rec.Body.String(), c.code)
		}
		if c.kind == session.KindCodex && a.Error.CanFork != moveCanFork(session.KindCodex) {
			t.Errorf("same kind: canFork = %v, want %v", a.Error.CanFork, moveCanFork(session.KindCodex))
		}
	}
	if len(r.creates) != 0 {
		t.Fatalf("a refused target was created: %+v", r.creates)
	}
	if cur, _ := session.ReadMeta("mvkind"); cur.Moving != "" {
		t.Fatalf("moving = %q after refusals", cur.Moving)
	}
}

// A → B → C: C owns copies of every earlier record, so deleting A and B leaves its files whole,
// and B's file names A's as earlier history.
func TestMoveChainCopiesEarlierFilesForward(t *testing.T) {
	r := moveRunEnv(t, "mva")
	ab := mustMove(t, "mva", session.KindClaude)
	b := ab.Target.Name
	t.Cleanup(func() { lifegate.Reopen(b) })
	bc := mustMove(t, b, session.KindCodex)
	c := bc.Target.Name
	if len(r.stops) != 2 {
		t.Fatalf("stops = %v, want A and B", r.stops)
	}
	if len(bc.Earlier) != 1 || bc.Earlier[0] != "handover-mva.md" {
		t.Fatalf("earlier = %v, want handover-mva.md", bc.Earlier)
	}
	if !strings.Contains(readFile(t, bc.File), "handover-mva.md") {
		t.Error("B's file does not name A's as earlier history")
	}
	for _, n := range []string{"mva", b} {
		if err := os.RemoveAll(paths.SessionWorkDir(n)); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"handover-mva.md", HandoverFileName(b)} {
		if md := readFile(t, filepath.Join(paths.SessionWorkDir(c), f)); !strings.Contains(md, "# Where it stopped") {
			t.Errorf("%s in C's directory is not a handover file:\n%s", f, md)
		}
	}
}

// A create that may have started a target is not guessed at: the source stays closed with its
// file until the user retries (same file, same name) or cancels.
func TestUncertainCreateWaitsForRetryOrCancel(t *testing.T) {
	t.Run("retry creates it again", func(t *testing.T) {
		r := moveRunEnv(t, "mvunc1")
		r.create = func(CreateReq) (int, []byte) { return 0, nil } // the create's outcome is unknown
		a := decodeMove(t, postMove(t, "mvunc1", "", `{"kind":"claude"}`))
		if a.Error.Code != "move_create_uncertain" || a.Error.Target == "" || a.Error.File == "" {
			t.Fatalf("answer = %+v, want move_create_uncertain naming the target and file", a.Error)
		}
		target, file := a.Error.Target, a.Error.File
		assertMovedRefusal(t, postInput(t, "mvunc1", `{"prompt":"hello?"}`), target)
		p := buildMovePreview(mustMeta(t, "mvunc1"), time.Now())
		if p.Pending == nil || p.Pending.Target != target || p.Pending.State != moveUncertain || p.Pending.TargetExists {
			t.Fatalf("preview pending = %+v", p.Pending)
		}
		r.create = nil
		rec := postMove(t, "mvunc1", "/retry", "")
		if rec.Code != http.StatusCreated {
			t.Fatalf("retry: %d %s", rec.Code, rec.Body.String())
		}
		if n := len(r.creates); n != 2 || r.creates[1].name != target || !strings.Contains(r.creates[1].InitialPrompt, file) {
			t.Fatalf("creates = %+v, want the second under the same name and file", r.creates)
		}
		if cur := mustMeta(t, "mvunc1"); cur.MovedTo != target {
			t.Fatalf("movedTo = %q after retry", cur.MovedTo)
		}
	})
	t.Run("cancel reopens the source", func(t *testing.T) {
		r := moveRunEnv(t, "mvunc2")
		r.create = func(CreateReq) (int, []byte) { return 0, nil }
		a := decodeMove(t, postMove(t, "mvunc2", "", `{"kind":"claude"}`))
		if rec := postMove(t, "mvunc2", "/cancel", ""); rec.Code != http.StatusOK {
			t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
		}
		if _, err := os.Stat(a.Error.File); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the file survived the cancel: %v", err)
		}
		if cur := mustMeta(t, "mvunc2"); cur.Moving != "" || cur.MovedTo != "" {
			t.Fatalf("meta after cancel = moving %q movedTo %q", cur.Moving, cur.MovedTo)
		}
		if rec := postInput(t, "mvunc2", `{"prompt":"back here"}`); rec.Code != http.StatusOK {
			t.Fatalf("input after cancel: %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("a target that exists is committed, never cancelled", func(t *testing.T) {
		r := moveRunEnv(t, "mvunc3")
		r.create = func(req CreateReq) (int, []byte) {
			createFakeTarget(req) // written, then the answer was lost
			return http.StatusGatewayTimeout, nil
		}
		a := decodeMove(t, postMove(t, "mvunc3", "", `{"kind":"claude"}`))
		if a.Error.Code != "move_create_uncertain" {
			t.Fatalf("answer = %+v", a.Error)
		}
		if rec := postMove(t, "mvunc3", "/cancel", ""); rec.Code != http.StatusConflict || decodeMove(t, rec).Error.Code != "move_target_exists" {
			t.Fatalf("cancel with a live target: %d %s", rec.Code, rec.Body.String())
		}
		if rec := postMove(t, "mvunc3", "/retry", ""); rec.Code != http.StatusOK {
			t.Fatalf("retry: %d %s", rec.Code, rec.Body.String())
		}
		if len(r.creates) != 1 {
			t.Errorf("retry created the existing target again: %d creates", len(r.creates))
		}
		if cur := mustMeta(t, "mvunc3"); cur.MovedTo != a.Error.Target {
			t.Fatalf("movedTo = %q", cur.MovedTo)
		}
	})
}

func mustMeta(t *testing.T, name string) session.Meta {
	t.Helper()
	m, ok := session.ReadMeta(name)
	if !ok {
		t.Fatalf("no meta for %s", name)
	}
	return m
}

// "Resume here anyway" is the user's way back into a moved source.
func TestMoveReopenOpensAMovedSource(t *testing.T) {
	moveRunEnv(t, "mvre")
	if rec := postMove(t, "mvre", "/reopen", ""); rec.Code != http.StatusConflict {
		t.Fatalf("reopen of a session never moved: %d %s", rec.Code, rec.Body.String())
	}
	mustMove(t, "mvre", session.KindClaude)
	if rec := postMove(t, "mvre", "/reopen", ""); rec.Code != http.StatusOK {
		t.Fatalf("reopen: %d %s", rec.Code, rec.Body.String())
	}
	if rec := postInput(t, "mvre", `{"prompt":"continue here"}`); rec.Code != http.StatusOK {
		t.Fatalf("input after reopen: %d %s", rec.Code, rec.Body.String())
	}
}

// The real create, in-process: the target gets the name the move booked (the file is already in
// its directory) and its first prompt reaches its runtime.
func TestMoveCreatesTheTargetUnderTheBookedName(t *testing.T) {
	r := moveRunEnv(t, "mvreal-unused")
	createMoveTarget = func(ctx context.Context, req CreateReq) (int, []byte) {
		r.creates = append(r.creates, req)
		r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/sessions", nil)
		c := &captureWriter{header: http.Header{}}
		createSession(c, r, req)
		return c.status, c.body.Bytes()
	}
	// A Terminal claude source moving to the managed codex fake.
	session.WriteMeta(session.Meta{Name: "mvreal", Dir: t.TempDir(), Kind: session.KindClaude})
	t.Cleanup(func() { lifegate.Reopen("mvreal") })
	rec := postMove(t, "mvreal", "", `{"kind":"codex","driver":"managed"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("move: %d %s", rec.Code, rec.Body.String())
	}
	a := decodeMove(t, rec)
	if a.Target.Name != r.creates[0].name {
		t.Fatalf("created %q, booked %q", a.Target.Name, r.creates[0].name)
	}
	tm := mustMeta(t, a.Target.Name)
	if tm.Kind != session.KindCodex || tm.Dir != mustMeta(t, "mvreal").Dir {
		t.Fatalf("target meta = %+v", tm)
	}
	select {
	case in := <-r.d.h.sent:
		if !strings.Contains(in.Prompt, a.File) {
			t.Fatalf("first prompt %q does not name %s", in.Prompt, a.File)
		}
	case <-time.After(moveGuard):
		t.Fatal("the first prompt never reached the target")
	}
}

func TestMovePreviewTellsTheDialogWhatMoves(t *testing.T) {
	r := moveRunEnv(t, "mvpre")
	r.queued = []agents.QueueItem{{ID: "q1", Text: "then bump the version", Origin: agents.Origin{Kind: "user"}}}
	if err := agents.PutPendingPeer("mvpre", agents.PendingPeer{ID: "pp1", From: "peer", Message: "ping", QueuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	p := buildMovePreview(mustMeta(t, "mvpre"), time.Now())
	if p.Refusal != nil {
		t.Fatalf("refusal = %+v", p.Refusal)
	}
	reasons := map[string]string{}
	for _, tg := range p.Targets {
		reasons[tg.Kind] = tg.Reason
	}
	want := map[string]string{session.KindClaude: "", session.KindCodex: moveTargetSameKind,
		session.KindLcpp: moveTargetCannotRead, session.KindCursor: moveTargetNotMeasured, session.KindKiro: moveTargetNotMeasured}
	for k, v := range want {
		if got, ok := reasons[k]; !ok || got != v {
			t.Errorf("target %s: reason %q (listed %v), want %q", k, got, ok, v)
		}
	}
	if len(p.Queued) != 1 || p.Queued[0].Text != "then bump the version" {
		t.Errorf("queued = %+v", p.Queued)
	}
	if len(p.Peers) != 1 || p.Peers[0].Message != "ping" {
		t.Errorf("peers = %+v", p.Peers)
	}
	if p.SizeBytes == 0 || p.Git == nil {
		t.Errorf("size %d, git %+v", p.SizeBytes, p.Git)
	}
	r.turns = nil
	if p := buildMovePreview(mustMeta(t, "mvpre"), time.Now()); p.Refusal == nil || p.Refusal.Code != "move_empty_transcript" {
		t.Errorf("empty transcript: refusal = %+v", p.Refusal)
	}
	// A managed cursor source is warned about: its mirror is empty once stopped (decision 11.7).
	cur := session.Meta{Name: "mvpre-cur", Dir: t.TempDir(), Kind: session.KindCursor, Driver: session.DriverManaged}
	if p := buildMovePreview(cur, time.Now()); !contains(p.Warnings, moveWarnCursorMirror) || p.Refusal == nil {
		t.Errorf("stopped managed cursor: warnings %v refusal %+v", p.Warnings, p.Refusal)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestInstructionGapNamesWhatTheTargetDoesNotLoad(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"CLAUDE.md", "AGENTS.md"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		from, to string
		want     string
	}{
		{session.KindClaude, session.KindCodex, "CLAUDE.md"},
		{session.KindCodex, session.KindClaude, "AGENTS.md"},
		{session.KindCodex, session.KindOpencode, ""},
		{session.KindCopilot, session.KindCodex, ""}, // .github/copilot-instructions.md is absent
	}
	for _, c := range cases {
		if got := strings.Join(instructionGap(dir, c.from, c.to), ","); got != c.want {
			t.Errorf("%s → %s: %q, want %q", c.from, c.to, got, c.want)
		}
	}
}

func TestRemoveHandoverFilesTouchesNothingElse(t *testing.T) {
	withTempHome(t)
	dir := paths.SessionWorkDir("rmtarget")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"handover-a.md", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveHandoverFiles("rmtarget", []string{"handover-a.md", "notes.txt", "handover-gone.md"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatalf("a file that is not a handover was touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "handover-a.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("handover-a.md survived: %v", err)
	}
	if err := RemoveHandoverFiles("rmtarget", nil); err != nil || os.Remove(filepath.Join(dir, "notes.txt")) != nil {
		t.Fatal("cleanup")
	}
	if err := RemoveHandoverFiles("rmtarget", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the emptied directory stayed: %v", err)
	}
}

// git status --porcelain is read untrimmed: its first line starts with a status column that can
// be a space (" M": modified, not staged).
func TestReadMoveGitKeepsThePorcelainColumns(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-qm", "one")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := readMoveGit(dir)
	if !g.Available || g.Status != " M a.txt" {
		t.Fatalf("status = %q (available %v), want \" M a.txt\"", g.Status, g.Available)
	}
	if !strings.Contains(g.DiffStat, "a.txt") || g.Ahead != "(the branch has no upstream)" {
		t.Fatalf("diff stat %q, ahead %q", g.DiffStat, g.Ahead)
	}
	if g := readMoveGit(t.TempDir()); g.Available {
		t.Fatal("a folder that is not a working copy reads as one")
	}
}
