package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/team-telnyx/fast-vm-spinner/internal/fastvm"
)

func TestCreateVMsReturnsBadRequestForBackendValidation(t *testing.T) {
	engine := fastvm.NewFirecrackerEngine(fastvm.FirecrackerConfig{
		StateDir:        t.TempDir(),
		InstallDir:      t.TempDir(),
		DefaultTemplate: "9013",
		Node:            "atl1-pc1-027",
		SSHHost:         "atl1-pc1-027.hosts.telnyx.io",
	})
	router := NewRouter(engine, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/vms", strings.NewReader(`{"user":"daan","count":1,"cores":33}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Firecracker supports at most 32 vCPUs") {
		t.Fatalf("response body = %s", rec.Body.String())
	}
}
