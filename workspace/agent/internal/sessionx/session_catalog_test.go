package sessionx

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

// TestSessionCatalogCarriesOnlyWhatTheSyncStores: GET /sessions/catalog lists every session,
// archived ones included, and the Control Plane's sync is its only reader. Sending the full
// Session took a real 1,174-session catalog past the CP's 1 MiB read, and handoff then
// reported the running workspace as not running. The row keys are pinned to what the CP
// stores, and a long history stays far below that size.
func TestSessionCatalogCarriesOnlyWhatTheSyncStores(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AF_SESSIONS_DIR", t.TempDir())
	const n = 1200
	for i := 0; i < n; i++ {
		session.WriteMeta(session.Meta{Name: fmt.Sprintf("cat-%04d", i), Kind: "claude", Dir: "/nonexistent/repo",
			Repo: "repo", Title: "a title", Label: "a label", CreatedAt: "2026-10-01T00:00:00Z", Archived: i%2 == 0})
	}
	rec := httptest.NewRecorder()
	HandleSessionCatalog(rec, httptest.NewRequest(http.MethodGet, "/sessions/catalog", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != n {
		t.Fatalf("rows=%d, want %d", len(got.Sessions), n)
	}
	allowed := map[string]bool{"name": true, "kind": true, "dir": true, "repo": true, "workingCopyId": true,
		"title": true, "label": true, "createdAt": true, "state": true, "alive": true, "archived": true}
	for _, row := range got.Sessions {
		for k := range row {
			if !allowed[k] {
				t.Fatalf("catalog row carries %q, which the CP sync does not store: %v", k, row)
			}
		}
		if row["title"] != "a title" || row["label"] != "a label" || row["repo"] != "repo" || row["createdAt"] == "" {
			t.Fatalf("catalog row lost a field the CP stores: %v", row)
		}
	}
	if size := rec.Body.Len(); size > 256<<10 {
		t.Fatalf("%d sessions make a %d-byte catalog; it should stay well under the CP's limits", n, size)
	}

	// Positive control: the full Session this replaced carries keys the allow-list rejects,
	// so the check above would have caught it.
	full, _ := json.Marshal(wireSession(session.Meta{Name: "cat-0000", Kind: "claude", Dir: "/nonexistent/repo"}, false))
	var fullRow map[string]any
	_ = json.Unmarshal(full, &fullRow)
	var extra []string
	for k := range fullRow {
		if !allowed[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	if !strings.Contains(strings.Join(extra, ","), "display") {
		t.Fatalf("positive control: the full Session should carry keys outside the allow-list, got extra=%v", extra)
	}
}
