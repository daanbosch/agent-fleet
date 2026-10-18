package memreview

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/memberorigin"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/memberorigin/memberorigintest"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/uiprefs"
)

func reviewMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("POST /agents/memory-review/run", memberorigin.Require(RunMaxRequest, http.HandlerFunc(HandleRun)))
	mux.HandleFunc("GET /agents/memory-review", HandleStatus)
	return mux
}

func writePrefsFile(t *testing.T, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(uiprefs.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(uiprefs.Path(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func post(t *testing.T, s *memberorigintest.Signer, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/agents/memory-review/run", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer smoke-token")
	if s != nil {
		s.Sign(r, []byte(body))
	}
	w := httptest.NewRecorder()
	reviewMux().ServeHTTP(w, r)
	return w
}

func code(w *httptest.ResponseRecorder) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if e.Error.Code != "" {
		return e.Error.Code
	}
	return e.Code
}

func withDefaultLoop(t *testing.T, lh *loopHarness) {
	t.Helper()
	old := DefaultLoop
	DefaultLoop = lh.loop
	t.Cleanup(func() { DefaultLoop = old })
}

// "Review now" is a member action: a session's own token is not enough, and nothing starts.
func TestRunRefusesARequestWithoutTheMemberMark(t *testing.T) {
	lh := newLoopHarness(t)
	withDefaultLoop(t, lh)
	lh.add("a", 5, turns(4))
	memberorigintest.Install(t)
	if w := post(t, nil, `{"session":"a"}`); w.Code != http.StatusForbidden {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if lh.calls != 0 || len(runs()) != 0 || lh.loop.Running("a") {
		t.Fatal("an unmarked request started a review")
	}
}

func TestRunStartsAReviewAndStatusReportsIt(t *testing.T) {
	lh := newLoopHarness(t)
	withDefaultLoop(t, lh)
	lh.add("a", 5, turns(4))
	s := memberorigintest.Install(t)
	writePrefsFile(t, `{"agentMemory":true,"memoryReview":"propose"}`)

	w := post(t, s, `{"session":"a"}`)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"started":true`) {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	waitIdle(t, lh.loop, "a")

	r := httptest.NewRequest(http.MethodGet, "/agents/memory-review?session=a", nil)
	rec := httptest.NewRecorder()
	reviewMux().ServeHTTP(rec, r)
	var st StatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if st.Mode != "propose" || !st.Enabled || st.Pending != 1 || st.Session == nil || st.Session.Last == nil ||
		st.Session.Last.State != StateDone || st.Session.Last.Queued != 1 || st.Session.Running {
		t.Fatalf("status = %+v / %+v", st, st.Session)
	}
	// No text of the proposal or of the conversation is in the answer.
	if strings.Contains(rec.Body.String(), "the text") || strings.Contains(rec.Body.String(), "question number") {
		t.Fatalf("status carries text: %s", rec.Body)
	}
}

func TestRunRefusals(t *testing.T) {
	lh := newLoopHarness(t)
	withDefaultLoop(t, lh)
	lh.add("a", 5, turns(4))
	s := memberorigintest.Install(t)
	for name, tc := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"not json":        {`nope`, 400, "bad_request"},
		"no session":      {`{}`, 400, "bad_request"},
		"bad name":        {`{"session":"../x"}`, 400, "bad_request"},
		"unknown session": {`{"session":"nobody"}`, 404, "not_found"},
	} {
		if w := post(t, s, tc.body); w.Code != tc.status || code(w) != tc.code {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	lh.enabled = false
	if w := post(t, s, `{"session":"a"}`); w.Code != 409 || code(w) != "memory_review_off" {
		t.Errorf("switched off: %d %s", w.Code, w.Body)
	}
	if lh.calls != 0 || len(runs()) != 0 {
		t.Fatal("a refused request did something")
	}
	DefaultLoop = nil
	if w := post(t, s, `{"session":"a"}`); w.Code != 503 {
		t.Errorf("no loop: %d", w.Code)
	}
}

func TestStatusIsReadableWithoutTheMarkAndChecksTheName(t *testing.T) {
	lh := newLoopHarness(t)
	withDefaultLoop(t, lh)
	for path, want := range map[string]int{
		"/agents/memory-review":                200,
		"/agents/memory-review?session=a":      200,
		"/agents/memory-review?session=..%2Fx": 400,
	} {
		w := httptest.NewRecorder()
		reviewMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != want {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
}
