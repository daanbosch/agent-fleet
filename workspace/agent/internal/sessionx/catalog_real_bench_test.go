package sessionx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"
)

// TestCatalogRealSessions measures GET /sessions/catalog over a real sessions directory
// (AF_BENCH_SESSIONS_DIR; read only). Opt-in: the numbers in the PR that added the delta came
// from it.
func TestCatalogRealSessions(t *testing.T) {
	dir := os.Getenv("AF_BENCH_SESSIONS_DIR")
	if dir == "" {
		t.Skip("AF_BENCH_SESSIONS_DIR not set")
	}
	t.Setenv("AF_SESSIONS_DIR", dir)
	old := catalogChanges
	catalogChanges = newCatalogTracker()
	t.Cleanup(func() { catalogChanges = old })
	get := func(query string) (time.Duration, int, string) {
		start := time.Now()
		rec := httptest.NewRecorder()
		HandleSessionCatalog(rec, httptest.NewRequest(http.MethodGet, "/sessions/catalog"+query, nil))
		var c struct{ Cursor string }
		_ = json.Unmarshal(rec.Body.Bytes(), &c)
		return time.Since(start), rec.Body.Len(), c.Cursor
	}
	d, n, _ := get("")
	t.Logf("whole catalog, no since (old CP): %6.1f ms %8d bytes", float64(d.Microseconds())/1000, n)
	d, n, cur := get("?since=")
	t.Logf("first call with since=          : %6.1f ms %8d bytes", float64(d.Microseconds())/1000, n)
	for i := 0; i < 3; i++ {
		d, n, _ = get("?since=" + url.QueryEscape(cur))
		t.Logf("warm, nothing changed           : %6.1f ms %8d bytes", float64(d.Microseconds())/1000, n)
	}
}
