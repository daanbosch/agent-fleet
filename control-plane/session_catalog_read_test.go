package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/k-k1/agent-fleet/control-plane/internal/store"
)

// bigCatalog returns oneSession plus enough archived rows, each carrying the bulky fields an
// older Agent still sends, to push the catalog past agentText's 1 MiB cap. Measured on a
// real workspace: 1,174 sessions came to 1,089,492 bytes.
func bigCatalog(t *testing.T) []map[string]any {
	t.Helper()
	out := append([]map[string]any{}, oneSession...)
	say := strings.Repeat("x", 1024)
	for i := 0; i < 1200; i++ {
		out = append(out, map[string]any{
			"name": fmt.Sprintf("old-%04d", i), "kind": "claude", "dir": "/home/dev/repos/private", "repo": "private",
			"workingCopyId": "wc-1", "archived": true, "lastSay": say,
		})
	}
	raw, _ := json.Marshal(map[string]any{"sessions": out})
	if len(raw) <= 1<<20 {
		t.Fatalf("fixture catalog is %d bytes; it must exceed 1 MiB to mean anything", len(raw))
	}
	return out
}

// TestHandoffRecipientsLoadWithCatalogOverOneMiB: a member with a long history must still be
// able to hand off. The sync used to read the catalog through agentText, which cuts at 1 MiB;
// the cut JSON failed to decode and the modal said the running workspace was not running.
func TestHandoffRecipientsLoadWithCatalogOverOneMiB(t *testing.T) {
	ctx := context.Background()
	f := newHandoffAPIFixture(t, bigCatalog(t))
	if err := f.st.PutSessionShare(ctx, store.SessionShare{ID: "share-1", TenantID: f.tenant.ID,
		OwnerMembershipID: f.owner.ID, RecipientMembershipID: f.recipient.ID, ScopeType: "session",
		ScopeKey: "session-1", Permission: "ro", CreatedAt: store.NowTS(), UpdatedAt: store.NowTS()}); err != nil {
		t.Fatal(err)
	}
	rec := f.getRecipients(t, "session-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "recipient@example.com") {
		t.Fatalf("the RO share recipient is missing from the candidate list: %s", rec.Body.String())
	}
	rows, err := f.st.ListSharedSessionCatalogByOwner(ctx, f.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1201 {
		t.Fatalf("catalog rows=%d, want every session synced (1201)", len(rows))
	}
}

// TestHandoffRecipientsCatalogSyncFailureHasOwnCode: when the workspace is running but its
// catalog cannot be read, the answer names that, not workspace_not_running.
func TestHandoffRecipientsCatalogSyncFailureHasOwnCode(t *testing.T) {
	f := newHandoffAPIFixture(t, oneSession)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	t.Cleanup(broken.Close)
	f.res.rt = stubRuntime{endpoint: broken.URL, token: "tok"}
	rec := f.getRecipients(t, "session-1")
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), `"catalog_sync_failed"`) {
		t.Fatalf("status=%d body=%s, want 502 catalog_sync_failed", rec.Code, rec.Body.String())
	}
	// Positive control: a workspace that really is stopped keeps its own code.
	f.res.rt = stubRuntime{endpoint: broken.URL, token: "tok", state: "stopped"}
	rec = f.getRecipients(t, "session-1")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"workspace_not_running"`) {
		t.Fatalf("status=%d body=%s, want 409 workspace_not_running", rec.Code, rec.Body.String())
	}
}

// TestReadAgentCatalogDetectsTruncation: a catalog past the limit is reported as too large,
// not as the JSON syntax error a cut body would otherwise produce.
func TestReadAgentCatalogDetectsTruncation(t *testing.T) {
	serve := func(size int) stubRuntime {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A valid document of exactly `size` bytes: {"sessions":[],"pad":"xxx…"}
			head, tail := `{"sessions":[],"pad":"`, `"}`
			_, _ = w.Write([]byte(head + strings.Repeat("x", size-len(head)-len(tail)) + tail))
		}))
		t.Cleanup(srv.Close)
		return stubRuntime{endpoint: srv.URL, token: "tok"}
	}
	if _, err := readAgentCatalog(context.Background(), serve(agentCatalogMaxBytes), ""); err != nil {
		t.Fatalf("a catalog of exactly the limit must be read: %v", err)
	}
	_, err := readAgentCatalog(context.Background(), serve(agentCatalogMaxBytes+1), "")
	if !errors.Is(err, errCatalogTooLarge) {
		t.Fatalf("err=%v, want errCatalogTooLarge", err)
	}
	// A permanent oversize and a transient failure must reach the user as different codes,
	// or the Console's translation advises retrying something no retry fixes.
	if e := catalogSyncAPIError(err); e.code != errCodeCatalogTooLarge {
		t.Fatalf("api error=%+v, want %s", e, errCodeCatalogTooLarge)
	}
	if e := catalogSyncAPIError(&catalogSyncError{errors.New("connection refused")}); e.code != errCodeCatalogSyncFailed {
		t.Fatalf("transient failure api error=%+v, want %s", e, errCodeCatalogSyncFailed)
	}
}
