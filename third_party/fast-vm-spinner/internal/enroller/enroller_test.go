package enroller

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recordingRunner struct {
	targets []SSHTarget
	scripts []string
}

func (r *recordingRunner) RunScript(_ context.Context, target SSHTarget, script string) (string, error) {
	r.targets = append(r.targets, target)
	r.scripts = append(r.scripts, script)
	return "", nil
}

func TestServiceEnrollsAuthorizedKeysAndRecordsStatus(t *testing.T) {
	secretsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(secretsDir, "authorized_keys"), []byte("ssh-ed25519 AAAATEST fastvm\n"), 0600); err != nil {
		t.Fatalf("write authorized_keys: %v", err)
	}
	runner := &recordingRunner{}
	svc := NewService(Config{
		AuthToken:       "secret",
		SecretsDir:      secretsDir,
		DefaultTimeout:  time.Second,
		EnableSystemctl: false,
	}, runner, slog.New(slog.NewTextHandler(ioDiscard{}, nil)))

	body := []byte(`{
		"vmid":"60000",
		"name":"fastvm-daan-1",
		"user":"daan",
		"backend":"firecracker",
		"template":"plain-ubuntu",
		"node":"atl1-pc1-027",
		"ssh_host":"atl1-pc1-027.hosts.telnyx.io",
		"ssh_port":60000,
		"ssh_user":"ubuntu",
		"features":["authorized-keys"]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/enroll", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(runner.scripts) != 2 {
		t.Fatalf("scripts = %d, want feature script + marker", len(runner.scripts))
	}
	if runner.targets[0].User != "ubuntu" {
		t.Fatalf("target user = %q, want ubuntu", runner.targets[0].User)
	}
	if !strings.Contains(runner.scripts[0], "target_user='ubuntu'") {
		t.Fatalf("authorized keys script did not target ubuntu home:\n%s", runner.scripts[0])
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/v1/enrollments/60000", nil)
	statusReq.Header.Set("Authorization", "Bearer secret")
	statusRec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status lookup code = %d body = %s", statusRec.Code, statusRec.Body.String())
	}
	var decoded struct {
		Data EnrollmentStatus `json:"data"`
	}
	if err := json.NewDecoder(statusRec.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if decoded.Data.Status != "enrolled" {
		t.Fatalf("enrollment status = %q, want enrolled", decoded.Data.Status)
	}
}

func TestServiceRejectsMissingAuth(t *testing.T) {
	svc := NewService(Config{AuthToken: "secret"}, &recordingRunner{}, slog.New(slog.NewTextHandler(ioDiscard{}, nil)))
	req := httptest.NewRequest(http.MethodPost, "/v1/enroll", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	svc.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestServiceReadsSecuritySecretsFromExplicitPaths(t *testing.T) {
	secretsDir := t.TempDir()
	laceworkDir := t.TempDir()
	crowdStrikeDir := t.TempDir()
	laceworkTokenPath := filepath.Join(laceworkDir, "agent_access_token")
	crowdStrikeCIDPath := filepath.Join(crowdStrikeDir, "crowdstrike_customer_id")
	if err := os.WriteFile(laceworkTokenPath, []byte("lacework-token"), 0600); err != nil {
		t.Fatalf("write lacework token: %v", err)
	}
	if err := os.WriteFile(crowdStrikeCIDPath, []byte("falcon-cid"), 0600); err != nil {
		t.Fatalf("write crowdstrike cid: %v", err)
	}

	runner := &recordingRunner{}
	svc := NewService(Config{
		AuthToken:                 "secret",
		SecretsDir:                secretsDir,
		LaceworkAccessTokenPath:   laceworkTokenPath,
		CrowdStrikeCustomerIDPath: crowdStrikeCIDPath,
		DefaultTimeout:            time.Second,
		EnableSystemctl:           false,
	}, runner, slog.New(slog.NewTextHandler(ioDiscard{}, nil)))

	body := []byte(`{
		"vmid":"60000",
		"name":"fastvm-daan-1",
		"user":"daan",
		"backend":"firecracker",
		"template":"plain-ubuntu",
		"node":"atl1-pc1-027",
		"ssh_host":"atl1-pc1-027.hosts.telnyx.io",
		"ssh_port":60000,
		"ssh_user":"ubuntu",
		"features":["lacework","crowdstrike"]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/enroll", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(runner.scripts) != 3 {
		t.Fatalf("scripts = %d, want two feature scripts + marker", len(runner.scripts))
	}
	if !strings.Contains(runner.scripts[0], "/etc/lacework/config.json") {
		t.Fatalf("lacework script did not use explicit token path:\n%s", runner.scripts[0])
	}
	if !strings.Contains(runner.scripts[1], "--cid='falcon-cid'") {
		t.Fatalf("crowdstrike script did not use explicit cid path:\n%s", runner.scripts[1])
	}
}

func TestValidateRequestRejectsUnexpectedSSHHost(t *testing.T) {
	err := validateRequest(EnrollRequest{
		VMID:    "60000",
		SSHHost: "example.com",
		SSHPort: 60000,
		SSHUser: "root",
	}, ".hosts.telnyx.io")
	if err == nil {
		t.Fatal("expected validation error")
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) {
	return len(p), nil
}
