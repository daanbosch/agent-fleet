package sessionx

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

func catalogGet(t *testing.T, query string) (session.CatalogResponse, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	HandleSessionCatalog(rec, httptest.NewRequest(http.MethodGet, "/sessions/catalog"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got session.CatalogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got, rec.Body.String()
}

func names(rows []session.CatalogEntry) string {
	var n []string
	for _, r := range rows {
		n = append(n, r.Name)
	}
	return strings.Join(n, ",")
}

func catalogFixture(t *testing.T, n int) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AF_SESSIONS_DIR", t.TempDir())
	old := catalogChanges
	catalogChanges = newCatalogTracker()
	t.Cleanup(func() { catalogChanges = old })
	for i := 0; i < n; i++ {
		session.WriteMeta(session.Meta{Name: fmt.Sprintf("cat-%04d", i), Kind: "claude", Dir: "/nonexistent/repo",
			Repo: "repo", Title: "t", CreatedAt: fmt.Sprintf("2026-10-01T00:%02d:00Z", i%60)})
	}
}

func since(c string) string { return "?since=" + url.QueryEscape(c) }

// Without ?since the body is what an older Control Plane expects: no cursor, no delta fields.
func TestCatalogWithoutSinceIsUnchanged(t *testing.T) {
	catalogFixture(t, 3)
	_, raw := catalogGet(t, "")
	for _, k := range []string{"cursor", "delta", "deleted"} {
		if strings.Contains(raw, `"`+k+`"`) {
			t.Fatalf("a request without since got %q: %s", k, raw)
		}
	}
}

func TestCatalogSinceSendsOnlyWhatChanged(t *testing.T) {
	catalogFixture(t, 5)
	full, _ := catalogGet(t, since(""))
	if full.Delta || len(full.Sessions) != 5 || full.Cursor == "" {
		t.Fatalf("first call = %+v, want the whole catalog and a cursor", full)
	}
	same, _ := catalogGet(t, since(full.Cursor))
	if !same.Delta || len(same.Sessions) != 0 || len(same.Deleted) != 0 || same.Cursor != full.Cursor {
		t.Fatalf("nothing changed, got %+v", same)
	}

	m, _ := session.ReadMeta("cat-0002")
	m.Archived = true
	session.WriteMeta(m)
	one, _ := catalogGet(t, since(full.Cursor))
	if !one.Delta || names(one.Sessions) != "cat-0002" || !one.Sessions[0].Archived {
		t.Fatalf("after archiving one, got %+v", one)
	}
	// The cursor moves on: asking again with it yields nothing.
	if again, _ := catalogGet(t, since(one.Cursor)); len(again.Sessions) != 0 {
		t.Fatalf("a row was sent twice: %+v", again)
	}

	// A deletion is reported by name, from any older cursor too.
	if err := os.Remove(filepath.Join(os.Getenv("AF_SESSIONS_DIR"), "cat-0004.json")); err != nil {
		matches, _ := filepath.Glob(filepath.Join(os.Getenv("AF_SESSIONS_DIR"), "*cat-0004*"))
		t.Fatalf("could not remove the meta: %v (candidates %v)", err, matches)
	}
	gone, _ := catalogGet(t, since(full.Cursor))
	if !gone.Delta || strings.Join(gone.Deleted, ",") != "cat-0004" || names(gone.Sessions) != "cat-0002" {
		t.Fatalf("after a deletion, got %+v", gone)
	}
}

// A cursor from another Agent process, a counter ahead of ours, or garbage never matches: the
// reader gets the whole catalog and replaces its copy.
func TestCatalogUnknownCursorGetsTheWholeCatalog(t *testing.T) {
	catalogFixture(t, 4)
	first, _ := catalogGet(t, since(""))
	_, rev, _ := strings.Cut(first.Cursor, ".")
	for name, c := range map[string]string{
		"another epoch (restart)": "0000000000000000." + rev,
		"ahead of the counter":    strings.Split(first.Cursor, ".")[0] + ".999999",
		"garbage":                 "zzz",
		"no revision":             strings.Split(first.Cursor, ".")[0] + ".",
		"negative":                strings.Split(first.Cursor, ".")[0] + ".-1",
	} {
		got, _ := catalogGet(t, since(c))
		if got.Delta || len(got.Sessions) != 4 {
			t.Errorf("%s: got %+v, want the whole catalog", name, got)
		}
	}
	// A restarted Agent (fresh tracker, counter back at zero) must not honour the old cursor
	// even though its revision number would look plausible.
	catalogChanges = newCatalogTracker()
	got, _ := catalogGet(t, since(first.Cursor))
	if got.Delta || len(got.Sessions) != 4 {
		t.Fatalf("after a restart got %+v", got)
	}
}

// Forgetting a tombstone must push older cursors to a full catalog, never to a delta that
// silently misses the deletion.
func TestCatalogForgottenTombstoneForcesFullCatalog(t *testing.T) {
	tr := newCatalogTracker()
	row := func(n string) session.CatalogEntry { return session.CatalogEntry{Name: n, Kind: "claude"} }
	var all []session.CatalogEntry
	for i := 0; i < maxCatalogTombstones+10; i++ {
		all = append(all, row(fmt.Sprintf("s%05d", i)))
	}
	start := tr.respond(all, "")
	tr.respond(nil, start.Cursor) // everything is deleted: more tombstones than kept
	got := tr.respond(nil, start.Cursor)
	if got.Delta {
		t.Fatalf("a cursor older than the forgotten tombstones got a delta: %d deleted", len(got.Deleted))
	}
	fresh := tr.respond(nil, got.Cursor)
	if !fresh.Delta {
		t.Fatalf("a current cursor should get a delta, got %+v", fresh)
	}
}
