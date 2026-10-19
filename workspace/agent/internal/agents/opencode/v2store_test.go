package opencode

// A stopped opencode 2 session has no daemon to ask: its history is read from opencode.db
// (v2store.go). The store here is built from the same recorded API answers the daemon tests
// replay, so "the store gives the same turns as the API" is checked, not assumed.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

// openTestStore opens (creating it) the workspace's opencode.db with the v2 tables.
func openTestStore(t *testing.T) *sql.DB {
	t.Helper()
	path := specDBPath(CurrentLaunch())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS session_v2 (id TEXT PRIMARY KEY, parent_id TEXT, agent TEXT, model TEXT,
			directory TEXT, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS session_message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, type TEXT NOT NULL,
			seq INTEGER NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// addStoreSession inserts one conversation record.
func addStoreSession(t *testing.T, db *sql.DB, id, parent string, created, updated int64) {
	t.Helper()
	var p any
	if parent != "" {
		p = parent
	}
	if _, err := db.Exec(`INSERT INTO session_v2 VALUES (?, ?, 'build', '{"id":"m","providerID":"stub"}', '/work', ?, ?)`, id, p, created, updated); err != nil {
		t.Fatal(err)
	}
}

// addStoreMessages stores API-shaped messages (JSON with id and type) as opencode does: id and
// type are columns, the rest is the data blob.
func addStoreMessages(t *testing.T, db *sql.DB, ses string, msgs []json.RawMessage) {
	t.Helper()
	for i, raw := range msgs {
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatal(err)
		}
		var id, typ string
		_ = json.Unmarshal(msg["id"], &id)
		_ = json.Unmarshal(msg["type"], &typ)
		delete(msg, "id")
		delete(msg, "type")
		data, _ := json.Marshal(msg)
		if _, err := db.Exec(`INSERT INTO session_message VALUES (?, ?, ?, ?, 0, 0, ?)`, id, ses, typ, i+1, string(data)); err != nil {
			t.Fatal(err)
		}
	}
}

// writeV2Store fills conversation ses with the messages of a recorded API answer.
func writeV2Store(t *testing.T, ses, msgFile string) {
	t.Helper()
	db := openTestStore(t)
	addStoreSession(t, db, ses, "", 1791931000000, 1791931000000)
	var w struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(v2Fixture(t, msgFile), &w); err != nil {
		t.Fatal(err)
	}
	addStoreMessages(t, db, ses, w.Data)
}

// deadDaemon is the address of a daemon that was there and is gone.
func deadDaemon(t *testing.T) apiTarget {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close()
	v2Down.mu.Lock()
	delete(v2Down.until, addr)
	v2Down.mu.Unlock()
	return apiTarget{Addr: addr, Client: http.DefaultClient}
}

func useTarget(t *testing.T, tg apiTarget) {
	t.Helper()
	prev := v2TargetFor
	v2TargetFor = func(session.Meta) (apiTarget, bool) { return tg, true }
	t.Cleanup(func() { v2TargetFor = prev })
}

func turnsJSON(t *testing.T, td agents.TranscriptData) string {
	t.Helper()
	b, err := json.Marshal(td.Turns)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Every recorded conversation reads the same from the store as from the daemon.
func TestV2StoreGivesTheTurnsTheAPIGives(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, f := range []string{"tool", "interrupted", "fail-auth", "fail-server-error", "compaction-ok", "compaction-failed", "form-replied"} {
		t.Run(f, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			d, m := v2Slot(t, f+".messages.json")
			_ = d
			want, _ := readTranscript(m)
			if len(want.Turns) == 0 {
				t.Fatalf("the API gave no turns for %s", f)
			}
			writeV2Store(t, "ses_a", f+".messages.json")
			useTarget(t, deadDaemon(t))
			got, _ := readTranscript(m)
			if turnsJSON(t, got) != turnsJSON(t, want) {
				t.Errorf("store turns differ from the API's\n got: %s\nwant: %s", turnsJSON(t, got), turnsJSON(t, want))
			}
			if got.Mode != want.Mode {
				t.Errorf("mode = %q, want %q", got.Mode, want.Mode)
			}
		})
	}
}

// The stopped session's cost and usage are read from the store as well.
func TestV2StoreServesUsageOfAStoppedSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, m := v2Slot(t, "compaction-ok.messages.json")
	want := usageTurnsV2(mustTarget(t, m), m)
	writeV2Store(t, "ses_a", "compaction-ok.messages.json")
	useTarget(t, deadDaemon(t))
	tg, _ := v2TargetFor(m)
	got := usageTurnsV2(tg, m)
	if len(want) == 0 || len(got) != len(want) {
		t.Fatalf("usage turns = %d, want %d", len(got), len(want))
	}
}

func mustTarget(t *testing.T, m session.Meta) apiTarget {
	t.Helper()
	tg, ok := v2TargetFor(m)
	if !ok {
		t.Fatal("no v2 target")
	}
	return tg
}

// A live daemon is always asked first: the store is only for when nobody answers.
func TestV2LiveDaemonWinsOverTheStore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, m := v2Slot(t, "tool.messages.json")
	writeV2Store(t, "ses_a", "fail-auth.messages.json")
	got, _ := readTranscript(m)
	if len(got.Turns) != 2 || got.Turns[1].Text != "working tool done" {
		t.Fatalf("the live daemon's conversation was replaced by the store's: %+v", got.Turns)
	}
}

// An answer from the daemon, even a refusal, is not "gone": the store would show a conversation
// the daemon says it does not have.
func TestV2StoreIsNotUsedForAnAnsweringDaemon(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d := newV2Daemon(t)
	d.f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	})
	m := v2Meta(t, "ses_a")
	writeV2Store(t, "ses_a", "tool.messages.json")
	got, _ := readTranscript(m)
	if len(got.Turns) != 0 {
		t.Fatalf("turns from the store although the daemon answered: %d", len(got.Turns))
	}
}

// Without a mapping the store finds the conversation the way the API listing does.
func TestV2StoreFindsTheConversationWithoutAMapping(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := session.Meta{Name: "v2unmapped", Dir: "/work", Kind: session.KindOpencode, Driver: session.DriverManaged}
	writeV2Store(t, "ses_a", "tool.messages.json")
	useTarget(t, deadDaemon(t))
	got, _ := readTranscript(m)
	if len(got.Turns) != 2 {
		t.Fatalf("turns = %d, want the directory's conversation", len(got.Turns))
	}
	other := session.Meta{Name: "v2other", Dir: "/elsewhere", Kind: session.KindOpencode, Driver: session.DriverManaged}
	if got, _ := readTranscript(other); len(got.Turns) != 0 {
		t.Fatalf("another directory's slot read %d turns", len(got.Turns))
	}
}

// No store, or one without the v2 tables, is an empty chat as before — not a failure.
func TestV2StoreMissingIsAnEmptyChat(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := v2Meta(t, "ses_a")
	useTarget(t, deadDaemon(t))
	if got, ok := readTranscript(m); !ok || len(got.Turns) != 0 {
		t.Fatalf("no store: %+v %v", got, ok)
	}
	path := specDBPath(CurrentLaunch())
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = db.Exec(`CREATE TABLE message (id TEXT)`)
	db.Close()
	if got, ok := readTranscript(m); !ok || len(got.Turns) != 0 {
		t.Fatalf("v1-only store: %+v %v", got, ok)
	}
}

// Before anything has probed a daemon (the Agent just restarted) the installed opencode and the
// database say which store holds the conversations; defaulting to v1 read the tables v2 never fills.
func TestV2ServedBeforeAnyProbeFollowsTheInstalledMajor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	prev := detectedMajor.Load()
	detectedMajor.Store(0)
	t.Cleanup(func() { detectedMajor.Store(prev) })
	m := session.Meta{Name: "a", Dir: "/d", Kind: session.KindOpencode, Driver: session.DriverManaged}
	openTestStore(t)

	useMajor(t, 1)
	if v2Served() {
		t.Error("v1 installed: served as v2")
	}
	useMajor(t, 2)
	if !v2Served() {
		t.Fatal("v2 installed over a v2 store: not served as v2")
	}
	if tg, ok := v2TargetFor(m); !ok || tg.Addr == "" {
		t.Errorf("a managed session on an unprobed v2 install has no target: %+v %v", tg, ok)
	}
	detectedMajor.Store(int32(apiV1))
	if v2Served() {
		t.Error("a probed v1 daemon was overruled by the installed major")
	}
}

// 2.x installed over a database v2 has not opened yet: the history is still in the v1 tables, and
// only the first start of v2 moves it. Until then the v1 reader has to serve it.
func TestV2NotServedOverAnUnmigratedV1Store(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	prev := detectedMajor.Load()
	detectedMajor.Store(0)
	t.Cleanup(func() { detectedMajor.Store(prev) })
	useMajor(t, 2)
	if v2Served() {
		t.Fatal("no database at all: served as v2")
	}
	path := specDBPath(CurrentLaunch())
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = db.Exec(`CREATE TABLE session (id TEXT)`)
	db.Close()
	if v2Served() {
		t.Fatal("a v1-only database: served as v2")
	}
	m := session.Meta{Name: "a", Dir: "/d", Kind: session.KindOpencode, Driver: session.DriverManaged}
	if _, ok := v2TargetFor(m); ok {
		t.Fatal("the v2 reader was chosen for a database that holds only v1 history")
	}
}

// A page that is not the API's (200 with HTML, or JSON that does not fit) is an answer from a
// daemon that is up: the store would show an older conversation than the one it holds, and hide
// that the contract moved. That holds for the next request too (the daemon is not "down").
func TestV2StoreIsNotUsedForAnUnparseableAnswer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d := newV2Daemon(t)
	d.f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>the web UI</html>"))
	})
	m := v2Meta(t, "ses_a")
	writeV2Store(t, "ses_a", "tool.messages.json")
	for i := 0; i < 2; i++ {
		if got, _ := readTranscript(m); len(got.Turns) != 0 {
			t.Fatalf("read %d: %d turns from the store although the daemon answered", i, len(got.Turns))
		}
	}
}

// The conversation record arrives, then the message read is cut: the whole conversation is read
// again from the store, never the API's half joined to it.
func TestV2StoreTakesOverWhenTheMessageReadIsCut(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d, m := v2Slot(t, "tool.messages.json")
	writeV2Store(t, "ses_a", "tool.messages.json")
	inner := d.f.srv.Config.Handler
	d.f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/message") {
			c, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				c.Close()
			}
			return
		}
		inner.ServeHTTP(w, r)
	})
	got, _ := readTranscript(m)
	if len(got.Turns) != 2 {
		t.Fatalf("turns = %d, want the store's whole conversation", len(got.Turns))
	}
	if usage := usageTurnsV2(mustTarget(t, m), m); len(usage) == 0 {
		t.Fatal("usage turns not taken over from the store")
	}
}

// A subagent's copied messages (older than the child itself) are not billed twice, with the
// daemon and without it: the child's creation time comes from the same place as its messages.
func TestV2StoreDoesNotBillACopiedChildTwice(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d, m := v2Slot(t, "subagent.messages.json")
	var w struct {
		Data []json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(v2Fixture(t, "subagent.messages.json"), &w)
	var child string
	for _, raw := range w.Data {
		var mm v2Msg
		_ = json.Unmarshal(raw, &mm)
		for _, c := range mm.Content {
			if c.State.Metadata.SessionID != "" {
				child = c.State.Metadata.SessionID
			}
		}
	}
	if child == "" {
		t.Fatal("fixture has no subagent session")
	}
	childMsgs := []string{
		`{"id":"msg_c1","type":"user","text":"task","time":{"created":1791931541000}}`,
		`{"id":"msg_c2","type":"assistant","agent":"general","model":{"id":"m","providerID":"stub"},"cost":0.0003,"time":{"created":1791931541001,"completed":1791931541002},"content":[{"type":"text","text":"copied"}]}`,
	}
	d.addSession(t, child, "ses_a", "1791931541411", "")
	d.messages[child] = childMsgs
	want := usageTurnsV2(mustTarget(t, m), m)
	var wantCost float64
	for _, tn := range want {
		if tn.Sidechain {
			wantCost += tn.CostUSD
		}
	}
	if wantCost != 0 {
		t.Fatalf("the API path bills the copied child message: %v", wantCost)
	}

	db := openTestStore(t)
	addStoreSession(t, db, "ses_a", "", 1791931000000, 1791931000000)
	addStoreMessages(t, db, "ses_a", w.Data)
	addStoreSession(t, db, child, "ses_a", 1791931541411, 1791931541411)
	var raws []json.RawMessage
	for _, c := range childMsgs {
		raws = append(raws, json.RawMessage(c))
	}
	addStoreMessages(t, db, child, raws)
	useTarget(t, deadDaemon(t))
	tg, _ := v2TargetFor(m)
	var gotCost float64
	var sides int
	for _, tn := range usageTurnsV2(tg, m) {
		if tn.Sidechain {
			sides++
			gotCost += tn.CostUSD
		}
	}
	if sides == 0 {
		t.Fatal("the child's turns are missing offline")
	}
	if gotCost != 0 {
		t.Fatalf("offline, the copied child message is billed again: $%v", gotCost)
	}
}

// With no mapping and two roots touched at the same instant the store takes the same one the
// API's listing does (updated, then id, newest first): the answer must not depend on the daemon.
func TestV2StoreBreaksTiesLikeTheAPI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db := openTestStore(t)
	addStoreSession(t, db, "ses_a", "", 1000, 2000)
	addStoreSession(t, db, "ses_z", "", 1000, 2000)
	m := session.Meta{Name: "v2tie", Dir: "/work", Kind: session.KindOpencode, Driver: session.DriverManaged}
	info, err := storeSession(m)
	if err != nil || info == nil || info.ID != "ses_z" {
		t.Fatalf("storeSession = %+v, %v; want ses_z", info, err)
	}
}
