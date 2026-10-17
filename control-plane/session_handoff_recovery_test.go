package main

// A launch from a handoff offer whose outcome the recipient's Console never learnt (issue
// #2040), through the CP's real routes, with the recipient's Agent replaced by a stub that
// keeps the idempotency ledger the way the Agent does. Releasing on an unknown outcome
// would hand the offer back to the sender while the session may exist (ADR 0057 decision 6).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/control-plane/internal/store"
)

// handoffFakeAgent is the recipient's Agent, keeping its contract (workspace/agent sessionx
// session_idempotency.go): reserve records a key; only the create that owns a key reports an
// outcome in the create headers; a refused key stays refused; abandon settles only a reserved
// key and answers unknown for one it has no record of. create decides what a POST /sessions
// does: "ok" creates and answers 201; "lost" creates and answers a bare 502 (the answer is
// lost on the way back); "proxy" creates and something in between answers 504 with a JSON
// error of its own; "drop" answers a bare 502 without creating; "fail" (500 tmux_failed) and
// "refuse" (400 bad_title) are the Agent's own refusals. abandon decides POST
// /sessions-idempotency/{key}/abandon: "agent" answers like today's Agent, "old" like an
// Agent without the route, "creating" with 202, "unknown" with an unreadable record. "old"
// also answers reserve like an Agent without that route.
type handoffFakeAgent struct {
	mu        sync.Mutex
	create    string
	abandon   string
	creates   int
	keys      map[string]string
	reserved  map[string]bool
	refused   map[string]bool
	abandoned map[string]bool
	// reserveFails: the Agent cannot record a key (503 naming it).
	reserveFails bool
	bodies       []map[string]any
	srv          *httptest.Server
}

func newHandoffFakeAgent(t *testing.T) *handoffFakeAgent {
	f := &handoffFakeAgent{create: "ok", abandon: "agent", keys: map[string]string{},
		reserved: map[string]bool{}, refused: map[string]bool{}, abandoned: map[string]bool{}}
	keyOf := func(path, verb string) string {
		return strings.TrimSuffix(strings.TrimPrefix(path, "/sessions-idempotency/"), "/"+verb)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sessions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.bodies = append(f.bodies, body)
			key, _ := body["idempotency_key"].(string)
			owned := func(outcome string) {
				w.Header().Set(createKeyHeader, key)
				w.Header().Set(createOutcomeHeader, outcome)
			}
			switch {
			case f.abandoned[key]:
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"error":{"code":"create_abandoned"}}`)
				return
			case f.refused[key]:
				owned("refused")
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"error":{"code":"create_refused"}}`)
				return
			case key != "" && f.keys[key] != "":
				owned("created")
				_, _ = io.WriteString(w, `{"name":"`+f.keys[key]+`"}`)
				return
			}
			switch f.create {
			case "drop":
				w.WriteHeader(http.StatusBadGateway)
				return
			case "fail", "refuse":
				f.refused[key] = true
				owned("refused")
				if f.create == "fail" {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = io.WriteString(w, `{"error":{"code":"tmux_failed"}}`)
				} else {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"error":{"code":"bad_title"}}`)
				}
				return
			}
			f.creates++
			name := "s-" + string(rune('0'+f.creates))
			f.keys[key] = name
			switch f.create {
			case "lost":
				w.WriteHeader(http.StatusBadGateway)
			case "proxy":
				w.WriteHeader(http.StatusGatewayTimeout)
				_, _ = io.WriteString(w, `{"error":{"code":"upstream_timeout"}}`)
			default:
				owned("created")
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, `{"name":"`+name+`"}`)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reserve"):
			if f.abandon == "old" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			key := keyOf(r.URL.Path, "reserve")
			if f.reserveFails {
				w.Header().Set(createKeyHeader, key)
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":{"code":"create_outcome_unknown"}}`)
				return
			}
			f.reserved[key] = true
			w.Header().Set(createKeyHeader, key)
			_, _ = io.WriteString(w, `{"status":"reserved"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/abandon"):
			key := keyOf(r.URL.Path, "abandon")
			if f.abandon == "old" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set(createKeyHeader, key)
			name, ok := f.keys[key]
			switch {
			case f.abandon == "creating":
				w.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(w, `{"status":"creating"}`)
			case f.abandon == "unknown" || !(ok || f.refused[key] || f.reserved[key] || f.abandoned[key]):
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":{"code":"create_outcome_unknown"}}`)
			case ok:
				_, _ = io.WriteString(w, `{"name":"`+name+`"}`)
			default:
				f.abandoned[key] = !f.refused[key]
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":{"code":"not_created"}}`)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *handoffFakeAgent) set(create, abandon string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.create, f.abandon = create, abandon
}

func (f *handoffFakeAgent) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates
}

type handoffRecovery struct {
	*inboxEnv
	agent *handoffFakeAgent
	offer string
}

// newHandoffRecovery has a offer b one session; b's workspace runs on the fake Agent.
func newHandoffRecovery(t *testing.T) *handoffRecovery {
	t.Helper()
	return newHandoffRecoveryIn(t, newInboxEnv(t))
}

func newHandoffRecoveryIn(t *testing.T, e *inboxEnv) *handoffRecovery {
	t.Helper()
	ctx := context.Background()
	agent := newHandoffFakeAgent(t)
	e.mgr.rtFactory = inboxPauseFactory{&sync.Once{}, make(chan struct{}), inboxClosedChan(), agent.srv.URL}
	owner, recipient := e.members["a"], e.members["b"]
	ws := store.Workspace{ID: "ws-a", TenantID: e.t1.ID, MembershipID: owner, ContainerName: "a", Network: "n",
		DataDir: "/data/a", AgentPort: "1", AgentToken: "t", State: "stopped", CreatedAt: store.NowTS()}
	if err := e.st.CreateWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := e.st.PutSessionShare(ctx, store.SessionShare{ID: "share-1", TenantID: e.t1.ID, OwnerMembershipID: owner,
		RecipientMembershipID: recipient, ScopeType: "session", ScopeKey: "s1", Permission: "ro",
		CreatedAt: store.NowTS(), UpdatedAt: store.NowTS()}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.ReplaceSharedSessionCatalog(ctx, ws.ID, owner, []store.SharedSessionCatalog{{ID: "cat-1", WorkspaceID: ws.ID,
		OwnerMembershipID: owner, Name: "s1", Kind: "claude", Dir: "/repos/app", Repo: "app", WorkingCopyID: "wc_1",
		CreatedAt: store.NowTS(), State: "running", LastSeen: store.NowTS()}}); err != nil {
		t.Fatal(err)
	}
	cats, err := e.st.ListSharedSessionCatalogByOwner(ctx, owner)
	if err != nil || len(cats) != 1 {
		t.Fatalf("catalog = %v err=%v", cats, err)
	}
	o := store.SessionHandoffOffer{ID: store.NewID(), TenantID: e.t1.ID, CatalogID: cats[0].ID,
		OwnerMembershipID: owner, RecipientMembershipID: recipient, Title: "carry on", Ciphertext: "Ym9keQ==",
		SourceSessionName: "s1", SourceSessionKind: "claude", Status: "pending",
		CreatedAt: store.NowTS(), ExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339)}
	if created, err := e.st.CreateSessionHandoffOffer(ctx, o); err != nil || !created {
		t.Fatalf("offer: created=%v err=%v", created, err)
	}
	return &handoffRecovery{inboxEnv: e, agent: agent, offer: o.ID}
}

type handoffTakeAnswer struct {
	Status      string `json:"status"`
	Token       string `json:"token"`
	SessionName string `json:"sessionName"`
}

func (h *handoffRecovery) take(prev ...string) (*httptest.ResponseRecorder, handoffTakeAnswer) {
	h.t.Helper()
	body := map[string]any{}
	if len(prev) > 0 {
		body["token"] = prev[0]
	}
	rec := h.do("b", http.MethodPost, "/api/session-handoff-offers/"+h.offer+"/take", body)
	var out handoffTakeAnswer
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func (h *handoffRecovery) mustTake() string {
	h.t.Helper()
	rec, out := h.take()
	if rec.Code != http.StatusOK || out.Token == "" {
		h.t.Fatalf("take = %d %s, want 200 with a token", rec.Code, rec.Body.String())
	}
	return out.Token
}

func (h *handoffRecovery) create(token string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do("b", http.MethodPost, "/api/sessions", map[string]any{"kind": "claude", "dir": "/repos/app",
		"handoff_take": map[string]any{"offerId": h.offer, "token": token}})
}

func (h *handoffRecovery) row() store.SessionHandoffOffer {
	h.t.Helper()
	o, ok, err := h.st.GetSessionHandoffOffer(context.Background(), h.offer)
	if err != nil || !ok {
		h.t.Fatalf("offer: ok=%v err=%v", ok, err)
	}
	return o
}

func (h *handoffRecovery) withdraw() *httptest.ResponseRecorder {
	return h.do("a", http.MethodDelete, "/api/session-handoff-offers/"+h.offer, nil)
}

func (h *handoffRecovery) acceptedNotices() int {
	h.t.Helper()
	ns, err := h.st.ListNotifications(context.Background(), h.members["a"], "", 100)
	if err != nil {
		h.t.Fatal(err)
	}
	n := 0
	for _, x := range ns {
		if x.Kind == "handoff-accepted" {
			n++
		}
	}
	return n
}

func (h *handoffRecovery) requireCode(rec *httptest.ResponseRecorder, status int, code string) {
	h.t.Helper()
	if rec.Code != status || (code != "" && decode[errResp](h.t, rec).Error.Code != code) {
		h.t.Fatalf("answer = %d %s, want %d %s", rec.Code, rec.Body.String(), status, code)
	}
}

// TestHandoffCreateSettlesOnTheAgentsAnswer: the CP settles the take from the Agent's answer
// to the create, whether or not the Console hears it. A created session accepts the offer
// (once: the Console's own accept afterwards is answered as done), and the create carries an
// idempotency key bound to the take.
func TestHandoffCreateSettlesOnTheAgentsAnswer(t *testing.T) {
	h := newHandoffRecovery(t)
	token := h.mustTake()
	if rec := h.create(token); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	if got := h.agent.bodies[0]["idempotency_key"]; got != handoffCreateKey(h.offer, token) || h.agent.bodies[0]["handoff_take"] != nil {
		t.Fatalf("the Agent received %v, want the take's idempotency key and no handoff_take", h.agent.bodies[0])
	}
	if o := h.row(); o.Status != "accepted" || o.AcceptedSessionName != "s-1" {
		t.Fatalf("after the create: %+v, want accepted by s-1", o)
	}
	rec := h.do("b", http.MethodPost, "/api/session-handoff-offers/"+h.offer+"/accept", map[string]any{"token": token, "sessionName": "s-1"})
	h.requireCode(rec, http.StatusOK, "")
	if n := h.acceptedNotices(); n != 1 {
		t.Fatalf("the owner was told %d times, want once", n)
	}
	// A different session name is not this launch's accept.
	rec = h.do("b", http.MethodPost, "/api/session-handoff-offers/"+h.offer+"/accept", map[string]any{"token": token, "sessionName": "s-9"})
	h.requireCode(rec, http.StatusConflict, "handoff_not_taken")
}

// TestHandoffUnknownOutcomeKeepsTheTake is #2040: the create's outcome is unknown to the CP, so
// the take stays and the sender cannot withdraw under it; the recipient's next press asks
// their Agent under the take's key and never creates a second session.
func TestHandoffUnknownOutcomeKeepsTheTake(t *testing.T) {
	t.Run("the session exists", func(t *testing.T) {
		h := newHandoffRecovery(t)
		h.agent.set("lost", "agent")
		token := h.mustTake()
		if rec := h.create(token); rec.Code != http.StatusBadGateway {
			t.Fatalf("create = %d %s, want the lost answer's 502", rec.Code, rec.Body.String())
		}
		if o := h.row(); o.Status != "pending" || o.TakeConfirmedAt == "" {
			t.Fatalf("after an unknown outcome: %+v, want pending under the confirmed take", o)
		}
		h.requireCode(h.withdraw(), http.StatusConflict, "handoff_being_taken")
		// The Console's release is what it did before #2040; it must not be what decides.
		rec, out := h.take()
		if rec.Code != http.StatusOK || out.Status != "accepted" || out.SessionName != "s-1" || out.Token != "" {
			t.Fatalf("next press = %d %s, want 200 accepted by s-1", rec.Code, rec.Body.String())
		}
		if o := h.row(); o.Status != "accepted" || o.AcceptedSessionName != "s-1" {
			t.Fatalf("after reconciling: %+v", o)
		}
		if h.agent.count() != 1 || h.acceptedNotices() != 1 {
			t.Fatalf("sessions=%d notices=%d, want 1 and 1", h.agent.count(), h.acceptedNotices())
		}
		h.requireCode(h.withdraw(), http.StatusConflict, "handoff_already_decided")
	})

	t.Run("nothing was created", func(t *testing.T) {
		h := newHandoffRecovery(t)
		h.agent.set("drop", "agent")
		token := h.mustTake()
		if rec := h.create(token); rec.Code != http.StatusBadGateway {
			t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
		}
		h.requireCode(h.withdraw(), http.StatusConflict, "handoff_being_taken")
		h.agent.set("ok", "agent")
		fresh := h.mustTake()
		if fresh == token {
			t.Fatal("the fresh take reused the stale token")
		}
		if rec := h.create(fresh); rec.Code != http.StatusCreated {
			t.Fatalf("create under the fresh take = %d %s", rec.Code, rec.Body.String())
		}
		if o := h.row(); o.Status != "accepted" || h.agent.count() != 1 {
			t.Fatalf("after starting over: %+v sessions=%d", o, h.agent.count())
		}
	})

	t.Run("the Agent cannot say", func(t *testing.T) {
		h := newHandoffRecovery(t)
		h.agent.set("drop", "old")
		token := h.mustTake()
		_ = h.create(token)
		rec, _ := h.take()
		h.requireCode(rec, http.StatusConflict, "handoff_launch_unknown")
		h.requireCode(h.withdraw(), http.StatusConflict, "handoff_being_taken")
		h.agent.set("drop", "creating")
		rec, _ = h.take()
		h.requireCode(rec, http.StatusConflict, "handoff_being_taken")
		h.agent.set("drop", "unknown")
		rec, _ = h.take()
		h.requireCode(rec, http.StatusConflict, "handoff_launch_unknown")
		if o := h.row(); o.Status != "pending" || o.TakeConfirmedAt == "" || h.agent.count() != 0 {
			t.Fatalf("the take moved without an answer: %+v sessions=%d", o, h.agent.count())
		}
	})
}

// TestHandoffRefusedCreateDoesNotPinTheOffer: a create the Agent refused made no session and
// never will under its key, so the CP gives the take back at once: the sender may withdraw,
// and a retry under the same take is refused before it reaches the Agent (the Console starts
// over under a new take).
func TestHandoffRefusedCreateDoesNotPinTheOffer(t *testing.T) {
	for _, mode := range []string{"refuse", "fail"} { // the Agent's own 400 and 500
		t.Run(mode, func(t *testing.T) {
			h := newHandoffRecovery(t)
			h.agent.set(mode, "agent")
			token := h.mustTake()
			if rec := h.create(token); rec.Code < 400 {
				t.Fatalf("create = %d", rec.Code)
			}
			if o := h.row(); o.Status != "pending" || o.TakenAt != "" {
				t.Fatalf("after a refused create: %+v, want no take", o)
			}
			h.agent.set("ok", "agent")
			h.requireCode(h.create(token), http.StatusConflict, "handoff_take_lost")
			if len(h.agent.bodies) != 1 {
				t.Fatalf("the Agent was asked %d times, want once", len(h.agent.bodies))
			}
			if rec := h.create(h.mustTake()); rec.Code != http.StatusCreated || h.agent.count() != 1 {
				t.Fatalf("create under a new take = %d, sessions=%d", rec.Code, h.agent.count())
			}
		})
	}
	h := newHandoffRecovery(t)
	h.agent.set("refuse", "agent")
	_ = h.create(h.mustTake())
	h.requireCode(h.withdraw(), http.StatusOK, "")
}

// TestHandoffOneCreatePerTake is review round 2's order: a take's create is held at the Agent,
// a second create under the same take is sent meanwhile, and the first one's refusal arrives
// last. The second is refused before it reaches the Agent, so the take's key has one create,
// whose refusal is true however late it comes: the take goes back, the sender may withdraw,
// and no session exists.
func TestHandoffOneCreatePerTake(t *testing.T) {
	h := newHandoffRecovery(t)
	h.agent.set("refuse", "agent")
	inner := h.agent.srv.Config.Handler
	arrived, answer := make(chan struct{}), make(chan struct{})
	var posts atomic.Int32
	h.agent.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/sessions" && posts.Add(1) == 1 {
			close(arrived)
			<-answer
		}
		inner.ServeHTTP(w, r)
	})
	token := h.mustTake()
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- h.create(token) }()
	<-arrived
	second := h.create(token)
	if second.Code != http.StatusConflict || decode[errResp](t, second).Error.Code != "handoff_take_used" {
		close(answer)
		<-first
		t.Fatalf("a second create under the take = %d %s, want it refused before the Agent", second.Code, second.Body.String())
	}
	close(answer)
	h.requireCode(<-first, http.StatusBadRequest, "bad_title")
	h.requireCode(h.withdraw(), http.StatusOK, "")
	if h.agent.count() != 0 || len(h.agent.bodies) != 1 {
		t.Fatalf("sessions=%d creates received=%d, want 0 and 1", h.agent.count(), len(h.agent.bodies))
	}
}

// TestHandoffUnreservedKeyStaysUnknown: on an Agent without reserve the create still goes
// ahead, but its key has no record there, so even after that Agent is updated an abandon
// cannot answer "not created": the take stays.
func TestHandoffUnreservedKeyStaysUnknown(t *testing.T) {
	h := newHandoffRecovery(t)
	h.agent.set("lost", "old")
	token := h.mustTake()
	_ = h.create(token)
	h.agent.mu.Lock()
	reserved := h.agent.reserved[handoffCreateKey(h.offer, token)]
	h.agent.keys = map[string]string{} // updated and restarted: no memory of the create
	h.agent.mu.Unlock()
	if reserved {
		t.Fatal("an Agent without reserve recorded the key")
	}
	h.agent.set("ok", "agent")
	// A resend under the used take (an older tab, a retry) is refused before the Agent, and
	// in particular reserves nothing there: a reservation would vouch for the key.
	h.requireCode(h.create(token), http.StatusConflict, "handoff_take_used")
	h.agent.mu.Lock()
	reserved = h.agent.reserved[handoffCreateKey(h.offer, token)]
	h.agent.mu.Unlock()
	if reserved {
		t.Fatal("a resend under a used take reserved its key on the updated Agent")
	}
	rec, _ := h.take()
	h.requireCode(rec, http.StatusConflict, "handoff_launch_unknown")
	h.requireCode(h.withdraw(), http.StatusConflict, "handoff_being_taken")
}

// TestHandoffFailedReserveSendsNothing: when the recipient's Agent cannot record the key, the
// create is not sent, and the take, confirmed a moment before, goes back.
func TestHandoffFailedReserveSendsNothing(t *testing.T) {
	h := newHandoffRecovery(t)
	h.agent.mu.Lock()
	h.agent.reserveFails = true
	h.agent.mu.Unlock()
	h.requireCode(h.create(h.mustTake()), http.StatusBadGateway, "handoff_reserve_failed")
	if len(h.agent.bodies) != 0 {
		t.Fatalf("the create was sent: %v", h.agent.bodies)
	}
	if o := h.row(); o.Status != "pending" || o.TakenAt != "" {
		t.Fatalf("after a failed reserve: %+v, want no take", o)
	}
	h.requireCode(h.withdraw(), http.StatusOK, "")
}

// TestHandoffAgentErrorIsARefusal: an answer carrying the Agent's create headers is the
// Agent's word whatever its status (tmux_failed is a 500), so the take goes back. A press that
// sends the token of a launch whose create never reached the Agent gives that unconfirmed take
// back and takes afresh; a confirmed take is not given back by its token.
func TestHandoffAgentErrorIsARefusal(t *testing.T) {
	h := newHandoffRecovery(t)
	h.agent.set("fail", "agent")
	h.requireCode(h.create(h.mustTake()), http.StatusInternalServerError, "tmux_failed")
	if o := h.row(); o.TakenAt != "" {
		t.Fatalf("after the Agent's own error: %+v, want the take given back", o)
	}

	// The create never reached the CP's confirm (say the workspace start failed on the way):
	// the take is unconfirmed, and only its own token frees it before the window.
	h2 := newHandoffRecovery(t)
	token := h2.mustTake()
	rec, _ := h2.take()
	h2.requireCode(rec, http.StatusConflict, "handoff_being_taken")
	rec, _ = h2.take("not-mine")
	h2.requireCode(rec, http.StatusConflict, "handoff_being_taken")
	rec, out := h2.take(token)
	if rec.Code != http.StatusOK || out.Token == "" || out.Token == token {
		t.Fatalf("press with the lost launch's token = %d %s, want a fresh take", rec.Code, rec.Body.String())
	}
	h2.requireCode(h2.create(token), http.StatusConflict, "handoff_take_lost")
	if rec := h2.create(out.Token); rec.Code != http.StatusCreated || h2.agent.count() != 1 {
		t.Fatalf("create under the fresh take = %d, sessions=%d", rec.Code, h2.agent.count())
	}

	// A confirmed take is not released by its token: its session may exist.
	h3 := newHandoffRecovery(t)
	h3.agent.set("lost", "old")
	token = h3.mustTake()
	_ = h3.create(token)
	rec, _ = h3.take(token)
	h3.requireCode(rec, http.StatusConflict, "handoff_launch_unknown")
}

// TestHandoffConfirmedTakeIsNotReleasedOnTheConsolesWord: a Console's release, from this
// version or an older one that releases on any failure, cannot give back a take whose create
// reached the Agent, since its session may exist. The sender stays refused, and the next press
// reconciles to that session.
func TestHandoffConfirmedTakeIsNotReleasedOnTheConsolesWord(t *testing.T) {
	h := newHandoffRecovery(t)
	h.agent.set("lost", "agent")
	token := h.mustTake()
	_ = h.create(token)
	rec := h.do("b", http.MethodPost, "/api/session-handoff-offers/"+h.offer+"/release", map[string]any{"token": token})
	h.requireCode(rec, http.StatusConflict, "handoff_launch_unknown")
	h.requireCode(h.withdraw(), http.StatusConflict, "handoff_being_taken")
	if rec, out := h.take(); rec.Code != http.StatusOK || out.Status != "accepted" || out.SessionName != "s-1" {
		t.Fatalf("next press = %d %s", rec.Code, rec.Body.String())
	}
}

// TestHandoffProxyErrorIsNotARefusal: an error answered by something between the CP and the
// Agent is not the Agent's word, JSON or not, so it settles nothing: the take stays confirmed
// past the window, and the next press finds the session the Agent did create.
func TestHandoffProxyErrorIsNotARefusal(t *testing.T) {
	h := newHandoffRecovery(t)
	h.agent.set("proxy", "agent")
	token := h.mustTake()
	h.requireCode(h.create(token), http.StatusGatewayTimeout, "upstream_timeout")
	if o := h.row(); o.TakeConfirmedAt == "" {
		t.Fatalf("a proxy's error unconfirmed the take: %+v", o)
	}
	if _, err := h.st.DB().Exec(`UPDATE session_handoff_offer SET taken_at='2000-01-01T00:00:00Z' WHERE id=?`, h.offer); err != nil {
		t.Fatal(err)
	}
	h.requireCode(h.withdraw(), http.StatusConflict, "handoff_being_taken")
	if rec, out := h.take(); rec.Code != http.StatusOK || out.Status != "accepted" || out.SessionName != "s-1" || h.agent.count() != 1 {
		t.Fatalf("next press = %d %s, sessions=%d", rec.Code, rec.Body.String(), h.agent.count())
	}
}

// TestHandoffLateCreateAfterAbandonIsRefused: the confirmed create is still on its way to the
// Agent when the next press reconciles. The Agent records the key as abandoned before answering
// not_created, so when the create finally arrives it is refused: the fresh take may be given
// back and the sender may withdraw without the same work starting on both sides.
func TestHandoffLateCreateAfterAbandonIsRefused(t *testing.T) {
	h := newHandoffRecovery(t)
	inner := h.agent.srv.Config.Handler
	arrived, release := make(chan struct{}), make(chan struct{})
	h.agent.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/sessions" {
			close(arrived)
			<-release
		}
		inner.ServeHTTP(w, r)
	})
	token := h.mustTake()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- h.create(token) }()
	<-arrived
	rec, out := h.take()
	if rec.Code != http.StatusOK || out.Token == "" || out.Token == token {
		close(release)
		<-done
		t.Fatalf("press while the create is held = %d %s, want a fresh take", rec.Code, rec.Body.String())
	}
	h.requireCode(h.do("b", http.MethodPost, "/api/session-handoff-offers/"+h.offer+"/release", map[string]any{"token": out.Token}), http.StatusOK, "")
	h.requireCode(h.withdraw(), http.StatusOK, "")
	close(release)
	late := <-done
	if late.Code == http.StatusCreated || h.agent.count() != 0 {
		t.Fatalf("the late create = %d, sessions=%d: the work started after the sender took it back", late.Code, h.agent.count())
	}
}
