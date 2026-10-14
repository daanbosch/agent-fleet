package opencode

// The question tool as a form on opencode 2 (v2form.go). The fixtures testdata/v2/form-* are
// captured from a real 2.0.28 serve; the fake daemon only replays them.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

func fixtureForms(t *testing.T, name string) []v2Form {
	t.Helper()
	var w struct {
		Data []v2Form `json:"data"`
	}
	if err := json.Unmarshal(v2Fixture(t, name), &w); err != nil {
		t.Fatal(err)
	}
	return w.Data
}

// fixtureEvents returns the recorded events of one file, one raw SSE payload per line, with the
// recorded session id replaced by ses.
func fixtureEvents(t *testing.T, name, ses string) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(v2Fixture(t, name)))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var e struct {
			Data struct {
				SessionID string `json:"sessionID"`
				Form      struct {
					SessionID string `json:"sessionID"`
				} `json:"form"`
			} `json:"data"`
		}
		line := sc.Text()
		if json.Unmarshal([]byte(line), &e) != nil {
			t.Fatal("bad fixture line")
		}
		rec := e.Data.SessionID
		if rec == "" {
			rec = e.Data.Form.SessionID
		}
		out = append(out, strings.ReplaceAll(line, rec, ses))
	}
	return out
}

func eventOfType(t *testing.T, lines []string, typ string) string {
	t.Helper()
	for _, l := range lines {
		if strings.Contains(l, `"type":"`+typ+`"`) {
			return l
		}
	}
	t.Fatalf("no %s event in the fixture", typ)
	return ""
}

// formDaemon is a v2 daemon that also answers the form routes from fixtures.
type formDaemon struct {
	*v2Daemon
	fmu     sync.Mutex
	list    map[string]string // session → GET …/form body
	detail  map[string]string // form id → GET …/form/{id} body
	replies []string          // "POST path body"
	deletes []string          // "DELETE uri"
}

func newFormDaemon(t *testing.T) *formDaemon {
	d := newV2Daemon(t)
	fd := &formDaemon{v2Daemon: d, list: map[string]string{}, detail: map[string]string{}}
	inner := d.f.srv.Config.Handler
	d.f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if !strings.HasPrefix(p, "/api/session/") || !strings.Contains(p, "/form") {
			inner.ServeHTTP(w, r)
			return
		}
		rest := strings.TrimPrefix(p, "/api/session/")
		ses, tail, _ := strings.Cut(rest, "/form")
		id := strings.TrimPrefix(tail, "/")
		fd.fmu.Lock()
		defer fd.fmu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && id == "":
			if b, ok := fd.list[ses]; ok {
				_, _ = io.WriteString(w, b)
			} else {
				_, _ = io.WriteString(w, `{"data":[]}`)
			}
		case r.Method == "GET":
			if b, ok := fd.detail[id]; ok {
				_, _ = io.WriteString(w, b)
				return
			}
			http.Error(w, `{"_tag":"FormNotFoundError"}`, http.StatusNotFound)
		case r.Method == "POST" && strings.HasSuffix(id, "/reply"):
			b, _ := io.ReadAll(r.Body)
			fd.replies = append(fd.replies, p+" "+string(b))
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "DELETE":
			fd.deletes = append(fd.deletes, r.URL.RequestURI())
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	})
	return fd
}

// --- mapping ----------------------------------------------------------------------

func TestV2FormMapsToQuestions(t *testing.T) {
	f := fixtureForms(t, "form-multi.pending.json")[0]
	in := f.interaction()
	if in.Kind != agents.InteractionQuestion || in.ID != f.ID || len(in.Questions) != 2 {
		t.Fatalf("interaction = %+v", in)
	}
	q0, q1 := in.Questions[0], in.Questions[1]
	if q0.Header != "Color" || q0.Question != "Which color?" || q0.MultiSelect || len(q0.Options) != 2 || q0.Options[1].Label != "Blue" || q0.Options[1].Description != "b" {
		t.Errorf("single choice = %+v", q0)
	}
	if q1.Header != "Toppings" || !q1.MultiSelect || len(q1.Options) != 3 || q1.Options[2].Label != "Egg" {
		t.Errorf("multiple choice = %+v", q1)
	}
	for _, q := range in.Questions {
		if q.ID != f.ID {
			t.Errorf("question id = %q, want the form id", q.ID)
		}
	}
}

func TestV2FormAnswerFollowsTheFieldTypes(t *testing.T) {
	f := fixtureForms(t, "form-multi.pending.json")[0]
	got, err := f.answer([]agents.InteractionAnswer{{Options: []int{1}}, {Options: []int{0, 2}, Text: " Pineapple "}})
	want := map[string]any{"q0": "Blue", "q1": []string{"Cheese", "Egg", "Pineapple"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("answer = %v, %v; want %v", got, err, want)
	}
	// Free text answers a single choice (custom:true), and a multiselect may select nothing.
	got, err = f.answer([]agents.InteractionAnswer{{Text: "Green"}, {}})
	want = map[string]any{"q0": "Green", "q1": []string{}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("free text = %v, %v; want %v", got, err, want)
	}
	for name, bad := range map[string][]agents.InteractionAnswer{
		"one answer for two questions": {{Options: []int{0}}},
		"option out of range":          {{Options: []int{2}}, {}},
	} {
		if _, err := f.answer(bad); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// An optional field left blank is omitted, not an error: the form decides what it needs.
	if got, err := f.answer([]agents.InteractionAnswer{{}, {Options: []int{0}}}); err != nil || got["q0"] != nil || got["q1"] == nil {
		t.Errorf("blank optional field: %v, %v", got, err)
	}
}

func TestV2FormAnswerOtherFieldTypes(t *testing.T) {
	f := v2Form{Fields: []v2FormField{
		{Key: "n", Type: "number", Required: true},
		{Key: "i", Type: "integer"},
		{Key: "b", Type: "boolean"},
		{Key: "s", Type: "string", Required: true},
		{Key: "x", Type: "external", URL: "https://example.invalid"},
		{Key: "h", Type: "string", Hidden: true},
	}}
	got, err := f.answer([]agents.InteractionAnswer{{Text: "1.5"}, {Text: "7"}, {Options: []int{1}}, {Text: "hi"}, {}})
	want := map[string]any{"n": 1.5, "i": int64(7), "b": false, "s": "hi"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("answer = %v, %v; want %v", got, err, want)
	}
	if _, err := f.answer([]agents.InteractionAnswer{{Text: "x"}, {}, {}, {Text: "hi"}, {}}); err == nil {
		t.Error("a non-number was accepted for a number field")
	}
	if _, err := f.answer([]agents.InteractionAnswer{{}, {}, {}, {Text: "hi"}, {}}); err == nil {
		t.Error("a required field left blank was accepted")
	}
}

// --- the mirror -------------------------------------------------------------------

// A turn waiting on a form is active as far as the daemon is concerned (measured), so it read as
// working with no question: the form list is what makes it a question, the way v1's running
// question tool part did.
func TestV2PendingFormReadsAsQuestion(t *testing.T) {
	fd := newFormDaemon(t)
	fd.addSession(t, "ses_a", "", "1791931000000", "form-waiting.messages.json")
	fd.active = []string{"ses_a"}
	m := v2Meta(t, "ses_a")

	if got := LiveState(m); got != "working" {
		t.Fatalf("no form: LiveState = %q, want working", got)
	}
	fd.list["ses_a"] = string(v2Fixture(t, "form-multi.pending.json"))
	if got := LiveState(m); got != "question" {
		t.Fatalf("form pending: LiveState = %q, want question", got)
	}
	td, _ := readTranscript(m)
	if len(td.Pending) != 2 || td.Pending[0].Header != "Color" || !td.Pending[1].MultiSelect {
		t.Fatalf("pending = %+v", td.Pending)
	}
	if id := fixtureForms(t, "form-multi.pending.json")[0].ID; td.Pending[0].ID != id || td.Pending[1].ID != id {
		t.Errorf("a Managed question must carry the form id /respond takes: %+v", td.Pending)
	}
	// The running question tool part is the question itself, not a turn part.
	for _, tn := range td.Turns {
		for _, p := range tn.Parts {
			if p.Kind == "question" || p.Tool == "question" {
				t.Errorf("an unanswered question tool shows as a part: %+v", p)
			}
		}
	}
	if pm, ok := (agentImpl{}).PendingModal(m); !ok || pm.Kind != "question" || len(pm.Questions) != 2 {
		t.Errorf("PendingModal = %+v, %v", pm, ok)
	}

	// Nothing running: a form the daemon no longer holds, or a stale "running" tool part left by
	// a killed daemon, is not a question.
	fd.active = nil
	if got := LiveState(m); got != "idle" {
		t.Errorf("idle session: LiveState = %q", got)
	}
	if td, _ := readTranscript(m); len(td.Pending) != 0 {
		t.Errorf("idle session still has a pending question: %+v", td.Pending)
	}
}

// A Terminal slot on the shared serve shows the pending form (so the question chip and the card
// light up) but without the form id: its TUI answers the form itself, and the card keeps driving
// that TUI with keys. af never posts a form reply for it, so there is nothing to double-answer.
func TestV2TerminalShowsTheFormWithoutAnAddress(t *testing.T) {
	fd := newFormDaemon(t)
	fd.addSession(t, "ses_a", "", "1791931000000", "form-waiting.messages.json")
	fd.active = []string{"ses_a"}
	fd.list["ses_a"] = string(v2Fixture(t, "form-single.pending.json"))
	prev := v2TargetFor
	v2TargetFor = func(session.Meta) (apiTarget, bool) {
		return apiTarget{Addr: fd.f.srv.URL, Client: serveClient}, true
	}
	t.Cleanup(func() { v2TargetFor = prev })
	m := session.Meta{Name: "v2term", Dir: "/work", Kind: session.KindOpencode, Driver: session.DriverTUI}
	sids.Write(session.UUID(m.Dir, m.Name), "ses_a")
	t.Cleanup(func() { sids.Remove(session.UUID(m.Dir, m.Name)) })

	if got := LiveState(m); got != "question" {
		t.Fatalf("LiveState = %q, want question", got)
	}
	td, _ := readTranscript(m)
	if len(td.Pending) != 1 || td.Pending[0].Question != "Pick one" || td.Pending[0].ID != "" {
		t.Fatalf("pending = %+v", td.Pending)
	}
	fd.fmu.Lock()
	defer fd.fmu.Unlock()
	if len(fd.replies)+len(fd.deletes) != 0 {
		t.Errorf("a read answered or cancelled a form: %v %v", fd.replies, fd.deletes)
	}
}

// --- the driver -------------------------------------------------------------------

func formHandle(t *testing.T, fd *formDaemon) *threadHandle {
	h := newTestHandle(t, fd.f.srv)
	registerTestHandle(t, h)
	// The tests mark the handle running without a pump behind it; the cleanup newTestHandle
	// registered waits for the pump to be idle, so put the flag back first (cleanups run LIFO).
	t.Cleanup(func() {
		h.mu.Lock()
		h.running = false
		h.mu.Unlock()
	})
	return h
}

func TestV2FormEventsMoveTheHandle(t *testing.T) {
	fd := newFormDaemon(t)
	h := formHandle(t, fd)
	h.mu.Lock()
	h.state, h.running = agents.TurnRunning, true
	h.mu.Unlock()
	evs := fixtureEvents(t, "form-multi-reply.events.jsonl", h.ses)

	handleServeEvent([]byte(eventOfType(t, evs, "form.created")))
	snap, _ := h.Snapshot()
	if snap.TurnState != agents.TurnWaitingInteraction || snap.Interaction == nil || len(snap.Interaction.Questions) != 2 {
		t.Fatalf("after form.created: %+v", snap)
	}
	id := snap.Interaction.ID
	if err := h.Send(agents.TurnInput{Prompt: "stray", ClientMessageID: "msg_stray"}); !ErrQuestionPending(err) {
		t.Errorf("free text while a form waits = %v, want question_pending", err)
	}

	// Answered elsewhere (an attached TUI): cleared, and the turn is running again.
	handleServeEvent([]byte(eventOfType(t, evs, "form.replied")))
	snap, _ = h.Snapshot()
	if snap.TurnState != agents.TurnRunning || snap.Interaction != nil {
		t.Fatalf("after form.replied: %+v", snap)
	}

	// Another session's form, and another form's settlement, leave the handle alone.
	handleServeEvent([]byte(strings.ReplaceAll(eventOfType(t, evs, "form.created"), h.ses, "ses_child")))
	handleServeEvent([]byte(eventOfType(t, evs, "form.created")))
	handleServeEvent([]byte(strings.ReplaceAll(eventOfType(t, evs, "form.replied"), id, "frm_other")))
	if snap, _ = h.Snapshot(); snap.Interaction == nil || snap.TurnState != agents.TurnWaitingInteraction {
		t.Fatalf("an unrelated form.replied cleared the question: %+v", snap)
	}

	// An interrupt cancels the form while the handle is already interrupting: the state stays.
	h.mu.Lock()
	h.state = agents.TurnInterrupting
	h.mu.Unlock()
	cancelled := eventOfType(t, fixtureEvents(t, "form-interrupt.events.jsonl", h.ses), "form.cancelled")
	var rec struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(cancelled), &rec)
	handleServeEvent([]byte(strings.ReplaceAll(cancelled, rec.Data.ID, id)))
	snap, _ = h.Snapshot()
	if snap.TurnState != agents.TurnInterrupting || snap.Interaction != nil {
		t.Fatalf("after the interrupt's form.cancelled: %+v", snap)
	}
}

// A form that was already waiting when the Agent (re)attached is found by reconcile, not by an event.
func TestV2ReconcileFindsAWaitingForm(t *testing.T) {
	fd := newFormDaemon(t)
	h := formHandle(t, fd)
	fd.active = []string{h.ses}
	fd.list[h.ses] = string(v2Fixture(t, "form-single.pending.json"))
	h.reconcile()
	snap, _ := h.Snapshot()
	if snap.TurnState != agents.TurnWaitingInteraction || snap.Interaction == nil || len(snap.Interaction.Questions) != 1 {
		t.Fatalf("reconcile = %+v", snap)
	}
}

func TestV2RespondAnswersTheForm(t *testing.T) {
	fd := newFormDaemon(t)
	h := formHandle(t, fd)
	f := fixtureForms(t, "form-single.pending.json")[0]
	fd.detail[f.ID] = string(v2Fixture(t, "form-detail.pending.json"))
	h.mu.Lock()
	h.state, h.running, h.inter = agents.TurnWaitingInteraction, true, f.interaction()
	h.mu.Unlock()

	if err := h.Respond(agents.InteractionReply{ID: "frm_stale", Decision: agents.DecisionAnswer}); err == nil {
		t.Fatal("a reply to an id that is not waiting was accepted")
	}
	if err := h.Respond(agents.InteractionReply{ID: f.ID, Decision: agents.DecisionAnswer,
		Answers: []agents.InteractionAnswer{{Options: []int{1}}}}); err != nil {
		t.Fatal(err)
	}
	fd.fmu.Lock()
	replies := append([]string(nil), fd.replies...)
	fd.fmu.Unlock()
	if len(replies) != 1 || !strings.HasSuffix(replies[0], "/form/"+f.ID+"/reply "+`{"answer":{"q0":"B"}}`) {
		t.Fatalf("replies = %q", replies)
	}
	snap, _ := h.Snapshot()
	if snap.Interaction != nil || snap.TurnState != agents.TurnRunning {
		t.Errorf("after the reply: %+v", snap)
	}
}

// Dismissing maps to v1's reject: the form is cancelled, with a reason the model reads.
func TestV2RespondCancelDismissesTheForm(t *testing.T) {
	fd := newFormDaemon(t)
	h := formHandle(t, fd)
	f := fixtureForms(t, "form-single.pending.json")[0]
	h.mu.Lock()
	h.state, h.running, h.inter = agents.TurnWaitingInteraction, true, f.interaction()
	h.mu.Unlock()
	if err := h.Respond(agents.InteractionReply{ID: f.ID, Decision: agents.DecisionCancel}); err != nil {
		t.Fatal(err)
	}
	fd.fmu.Lock()
	defer fd.fmu.Unlock()
	if len(fd.deletes) != 1 || !strings.Contains(fd.deletes[0], "/form/"+f.ID+"?message=") {
		t.Fatalf("deletes = %q", fd.deletes)
	}
	if len(fd.replies) != 0 {
		t.Errorf("a dismissal posted a reply: %q", fd.replies)
	}
}

// Answering a form somebody else settled first (the TUI) must not post a second answer.
func TestV2RespondDoesNotDoubleAnswer(t *testing.T) {
	fd := newFormDaemon(t)
	h := formHandle(t, fd)
	f := fixtureForms(t, "form-single.pending.json")[0]
	fd.detail[f.ID] = string(v2Fixture(t, "form-detail.answered.json"))
	h.mu.Lock()
	h.state, h.running, h.inter = agents.TurnWaitingInteraction, true, f.interaction()
	h.mu.Unlock()
	err := h.Respond(agents.InteractionReply{ID: f.ID, Decision: agents.DecisionAnswer, Answers: []agents.InteractionAnswer{{Options: []int{0}}}})
	if err == nil || !strings.Contains(err.Error(), "answered") {
		t.Fatalf("Respond on a settled form = %v", err)
	}
	fd.fmu.Lock()
	defer fd.fmu.Unlock()
	if len(fd.replies) != 0 {
		t.Errorf("a settled form was answered again: %q", fd.replies)
	}
}
