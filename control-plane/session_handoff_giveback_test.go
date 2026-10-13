package main

// The recipient's give-back of a launch whose outcome stays unknown (issue #2058), through the
// CP's real routes on SQLite and, with AF_TEST_DATABASE_URL set, Postgres: only the recipient
// can give it back, only a take in the unknown state is released, and neither the sender's
// withdraw nor the recipient's own reconcile can win alongside it. A session the recipient's
// Agent reports after the give-back is shown to the recipient and moves nothing.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/control-plane/internal/pgtest"
	"github.com/k-k1/agent-fleet/control-plane/internal/store"
)

// handoffDialects runs f on a fresh recovery fixture per dialect.
func handoffDialects(t *testing.T, f func(t *testing.T, h *handoffRecovery)) {
	t.Run("sqlite", func(t *testing.T) { f(t, newHandoffRecovery(t)) })
	t.Run("postgres", func(t *testing.T) {
		url, ok := pgtest.Schema(t)
		if !ok {
			t.Skip("set AF_TEST_DATABASE_URL to run the Postgres half")
		}
		st, err := store.OpenPostgres(url)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		f(t, newHandoffRecoveryIn(t, newInboxEnvOn(t, st)))
	})
}

func (h *handoffRecovery) giveBack(who string, body map[string]any) *httptest.ResponseRecorder {
	return h.do(who, http.MethodPost, "/api/session-handoff-offers/"+h.offer+"/give-back", body)
}

var confirmedGiveBack = map[string]any{"confirmed": true}

// ageTake moves the take's confirm past the take window, which is what makes its outcome
// unknown rather than in progress.
func (h *handoffRecovery) ageTake() {
	h.t.Helper()
	old := time.Now().UTC().Add(-store.HandoffTakeWindow - time.Minute).Format(time.RFC3339)
	if _, err := h.st.DB().ExecContext(context.Background(), `UPDATE session_handoff_offer SET taken_at=?, take_confirmed_at=? WHERE id=? AND take_confirmed_at<>''`, old, old, h.offer); err != nil {
		h.t.Fatal(err)
	}
}

// requireTakeState asserts a 409 whose detail names the take's state.
func (h *handoffRecovery) requireTakeState(rec *httptest.ResponseRecorder, code, state string) {
	h.t.Helper()
	var out struct {
		Error struct {
			Code      string  `json:"code"`
			TakeState *string `json:"takeState"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusConflict || out.Error.Code != code || out.Error.TakeState == nil || *out.Error.TakeState != state {
		h.t.Fatalf("answer = %d %s, want 409 %s with takeState %q", rec.Code, rec.Body.String(), code, state)
	}
}

func (h *handoffRecovery) notices(who, kind string) []store.Notification {
	h.t.Helper()
	ns, err := h.st.ListNotifications(context.Background(), h.members[who], "", 100)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []store.Notification
	for _, n := range ns {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

// listedTakeState reads the offer's takeState as the sender's ledger and the recipient's inbox
// show it.
func (h *handoffRecovery) listedTakeState(who, path string) string {
	h.t.Helper()
	rec := h.do(who, http.MethodGet, path, nil)
	var out struct {
		Offers []map[string]any `json:"offers"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		h.t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body.String())
	}
	for _, o := range out.Offers {
		if o["id"] == h.offer {
			s, _ := o["takeState"].(string)
			return s
		}
	}
	h.t.Fatalf("GET %s does not list the offer: %s", path, rec.Body.String())
	return ""
}

func TestHandoffGiveBackOnlyByTheRecipientOfAnUnknownTake(t *testing.T) {
	handoffDialects(t, func(t *testing.T, h *handoffRecovery) {
		h.agent.set("drop", "old") // nothing created, and an Agent that cannot say so
		token := h.mustTake()
		h.requireTakeState(h.giveBack("b", confirmedGiveBack), "handoff_not_unknown", "starting")
		h.requireTakeState(h.withdraw(), "handoff_being_taken", "starting")
		_ = h.create(token)
		rec, _ := h.take()
		h.requireCode(rec, http.StatusConflict, "handoff_launch_unknown")

		// Within the window the create may still be on its way: not given back.
		h.requireTakeState(h.giveBack("b", confirmedGiveBack), "handoff_not_unknown", "confirming")
		h.requireTakeState(h.withdraw(), "handoff_being_taken", "confirming")
		if s := h.listedTakeState("a", "/api/session-handoff-offers"); s != "confirming" {
			t.Fatalf("the sender's ledger says takeState %q, want confirming", s)
		}

		h.ageTake()
		if s := h.listedTakeState("a", "/api/session-handoff-offers"); s != "unknown" {
			t.Fatalf("the sender's ledger says takeState %q, want unknown", s)
		}
		if s := h.listedTakeState("b", "/api/session-handoff-offers/received"); s != "unknown" {
			t.Fatalf("the recipient's inbox says takeState %q, want unknown", s)
		}
		h.requireTakeState(h.withdraw(), "handoff_being_taken", "unknown")
		for _, who := range []string{"a", "c", "x"} { // the sender, a tenant peer, another tenant
			h.requireCode(h.giveBack(who, confirmedGiveBack), http.StatusNotFound, "not_found")
		}
		h.requireCode(h.giveBack("b", map[string]any{}), http.StatusBadRequest, "handoff_give_back_unconfirmed")
		if o := h.row(); o.TakeConfirmedAt == "" {
			t.Fatalf("a refused give-back moved the take: %+v", o)
		}

		rec = h.giveBack("b", confirmedGiveBack)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"pending"`) {
			t.Fatalf("give-back = %d %s, want 200 pending", rec.Code, rec.Body.String())
		}
		if o := h.row(); o.Status != "pending" || o.TakenAt != "" || o.TakeConfirmedAt != "" {
			t.Fatalf("after the give-back: %+v, want pending and untaken", o)
		}
		audits, err := h.st.ListAuditByTenant(context.Background(), h.t1.ID, 100)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, a := range audits {
			if a.Action == "session.handoff.give_back" {
				n++
				if a.Detail != "" || a.Target != "s1" {
					t.Fatalf("give-back audit %+v, want target s1 and no detail", a)
				}
			}
		}
		if n != 1 {
			t.Fatalf("%d give-back audit rows, want 1", n)
		}
		// The same take's create is fenced, and the offer is the sender's to withdraw again.
		h.agent.set("ok", "agent")
		h.requireCode(h.create(token), http.StatusConflict, "handoff_take_lost")
		h.requireCode(h.withdraw(), http.StatusOK, "")
		h.requireCode(h.giveBack("b", confirmedGiveBack), http.StatusConflict, "handoff_already_decided")
		if h.agent.count() != 0 {
			t.Fatalf("sessions=%d, want none", h.agent.count())
		}
	})
}

// TestHandoffGiveBackRacesTheWithdraw fires the sender's withdraw and the recipient's give-back
// at once on an unknown take, round after round: the withdraw may land only after the
// give-back, never past the take.
func TestHandoffGiveBackRacesTheWithdraw(t *testing.T) {
	handoffDialects(t, func(t *testing.T, h *handoffRecovery) {
		ctx := context.Background()
		base := h.row()
		old := time.Now().UTC().Add(-store.HandoffTakeWindow - time.Minute).Format(time.RFC3339)
		gaveBack, withdrew := 0, 0
		for i := 0; i < 20; i++ {
			if i > 0 {
				o := base
				o.ID, o.Status, o.CreatedAt = store.NewID(), "pending", store.NowTS()
				if created, err := h.st.CreateSessionHandoffOffer(ctx, o); err != nil || !created {
					t.Fatalf("round %d: offer created=%v err=%v", i, created, err)
				}
				h.offer = o.ID
			}
			if ok, err := h.st.TakeSessionHandoffOffer(ctx, h.offer, h.members["b"], "tok", old); err != nil || !ok {
				t.Fatalf("round %d: take=%v err=%v", i, ok, err)
			}
			if ok, err := h.st.ConfirmSessionHandoffTake(ctx, h.offer, h.members["b"], "tok", old); err != nil || !ok {
				t.Fatalf("round %d: confirm=%v err=%v", i, ok, err)
			}
			var gave, wd *httptest.ResponseRecorder
			var wg sync.WaitGroup
			start := make(chan struct{})
			wg.Add(2)
			go func() { defer wg.Done(); <-start; gave = h.giveBack("b", confirmedGiveBack) }()
			go func() { defer wg.Done(); <-start; wd = h.withdraw() }()
			close(start)
			wg.Wait()
			if gave.Code != http.StatusOK {
				t.Fatalf("round %d: give-back = %d %s; nothing else could have settled the take", i, gave.Code, gave.Body.String())
			}
			gaveBack++
			switch wd.Code {
			case http.StatusOK:
				withdrew++
			case http.StatusConflict:
				// Refused while the take held; the answer reads the row after the give-back may
				// have landed, so its takeState is "unknown" or "".
				h.requireCode(wd, http.StatusConflict, "handoff_being_taken")
				h.requireCode(h.withdraw(), http.StatusOK, "") // the sender's next try lands
			default:
				t.Fatalf("round %d: withdraw = %d %s", i, wd.Code, wd.Body.String())
			}
			if o := h.row(); o.Status != "withdrawn" {
				t.Fatalf("round %d: %+v, want withdrawn", i, o)
			}
		}
		t.Logf("%d give-backs, %d withdraws landed in the same round", gaveBack, withdrew)
	})
}

// holdAbandon makes the fake Agent's abandon wait until unblock is called, signalling arrived.
func (h *handoffRecovery) holdAbandon() (arrived chan struct{}, unblock func()) {
	inner := h.agent.srv.Config.Handler
	arrived, release := make(chan struct{}), make(chan struct{})
	var once, freed sync.Once
	unblock = func() { freed.Do(func() { close(release) }) }
	h.t.Cleanup(unblock)
	h.agent.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/abandon") {
			once.Do(func() { close(arrived) })
			<-release
		}
		inner.ServeHTTP(w, r)
	})
	return arrived, unblock
}

// TestHandoffGiveBackRacesTheReconcile: the recipient's next press asks their Agent about the
// lost create, and the give-back lands while it asks. The Agent then reports the session: the
// offer is not flipped to accepted behind the give-back, the press says so with the session's
// name, and the recipient is notified.
func TestHandoffGiveBackRacesTheReconcile(t *testing.T) {
	handoffDialects(t, func(t *testing.T, h *handoffRecovery) {
		h.agent.set("lost", "agent")
		_ = h.create(h.mustTake())
		h.ageTake()
		arrived, unblock := h.holdAbandon()
		press := make(chan *httptest.ResponseRecorder, 1)
		go func() { rec, _ := h.take(); press <- rec }()
		<-arrived
		gave := h.giveBack("b", confirmedGiveBack)
		unblock()
		rec := <-press
		h.requireCode(gave, http.StatusOK, "")
		var out struct {
			Error struct {
				Code, SessionName, OfferStatus string
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusConflict || out.Error.Code != "handoff_session_after_give_back" || out.Error.SessionName != "s-1" || out.Error.OfferStatus != "pending" {
			t.Fatalf("the press = %d %s, want 409 handoff_session_after_give_back naming s-1", rec.Code, rec.Body.String())
		}
		if o := h.row(); o.Status != "pending" || o.TakenAt != "" {
			t.Fatalf("the late report moved the offer: %+v", o)
		}
		if n := h.notices("b", "handoff-late-session"); len(n) != 1 || n[0].TargetID != "s-1" || n[0].TargetType != "session" {
			t.Fatalf("the recipient's late-session notices: %+v, want one for s-1", n)
		}
		if h.acceptedNotices() != 0 {
			t.Fatal("the sender was told the offer was accepted")
		}
	})
}

// TestHandoffGiveBackAfterTheReconcileAccepted is the other order: the press found the session
// and accepted the offer, so the give-back finds it decided and changes nothing.
func TestHandoffGiveBackAfterTheReconcileAccepted(t *testing.T) {
	handoffDialects(t, func(t *testing.T, h *handoffRecovery) {
		h.agent.set("lost", "agent")
		_ = h.create(h.mustTake())
		h.ageTake()
		if rec, got := h.take(); rec.Code != http.StatusOK || got.Status != "accepted" {
			t.Fatalf("reconcile = %d %s", rec.Code, rec.Body.String())
		}
		h.requireCode(h.giveBack("b", confirmedGiveBack), http.StatusConflict, "handoff_already_decided")
		if o := h.row(); o.Status != "accepted" || o.AcceptedSessionName != "s-1" {
			t.Fatalf("the give-back moved an accepted offer: %+v", o)
		}
		if n := h.notices("b", "handoff-late-session"); len(n) != 0 {
			t.Fatalf("late-session notices after an ordinary reconcile: %+v", n)
		}
	})
}

// TestHandoffLateCreateAfterGiveBack: the create itself answers only after the recipient gave
// the take back and the sender withdrew. The CP does not accept the withdrawn offer behind the
// sender's back; the recipient is told a session started from it.
func TestHandoffLateCreateAfterGiveBack(t *testing.T) {
	handoffDialects(t, func(t *testing.T, h *handoffRecovery) {
		inner := h.agent.srv.Config.Handler
		arrived, release := make(chan struct{}), make(chan struct{})
		h.agent.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/sessions" {
				close(arrived)
				<-release
			}
			inner.ServeHTTP(w, r)
		})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		t.Cleanup(unblock) // a failed assertion must not leave the create holding the server open
		token := h.mustTake()
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- h.create(token) }()
		<-arrived
		h.ageTake() // the create has been running for longer than the window
		h.requireCode(h.giveBack("b", confirmedGiveBack), http.StatusOK, "")
		h.requireCode(h.withdraw(), http.StatusOK, "")
		unblock()
		if late := <-done; late.Code != http.StatusCreated {
			t.Fatalf("the late create = %d %s", late.Code, late.Body.String())
		}
		if o := h.row(); o.Status != "withdrawn" || o.AcceptedSessionName != "" {
			t.Fatalf("the late create settled the offer: %+v, want withdrawn", o)
		}
		n := h.notices("b", "handoff-late-session")
		if len(n) != 1 || n[0].TargetID != "s-1" || !strings.Contains(n[0].Payload, `"offerStatus":"withdrawn"`) {
			t.Fatalf("the recipient's late-session notices: %+v, want one for s-1 naming the withdrawn offer", n)
		}
		if h.acceptedNotices() != 0 {
			t.Fatal("the sender was told the offer was accepted")
		}
	})
}
