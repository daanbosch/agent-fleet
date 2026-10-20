package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/team-telnyx/fast-vm-spinner/internal/enroller"
	"github.com/team-telnyx/fast-vm-spinner/internal/fastvm"
)

type fakeBackend struct{}

func (fakeBackend) CreateVMs(user string, count int, _ fastvm.CreateOpts) ([]fastvm.VMInfo, error) {
	return []fastvm.VMInfo{{
		VMID:         "60000",
		Name:         "fastvm-" + user + "-1",
		User:         user,
		TemplateName: "plain-ubuntu",
		Node:         "atl1-pc1-027",
		SSHPort:      60000,
		Backend:      "firecracker",
		SSHUser:      "root",
	}}, nil
}
func (fakeBackend) ListVMs(string) ([]fastvm.VMInfo, error) { return nil, nil }
func (fakeBackend) GetVM(string) (*fastvm.VMInfo, error)    { return nil, errors.New("not found") }
func (fakeBackend) GetVMByName(string) (*fastvm.VMInfo, error) {
	return nil, errors.New("not found")
}
func (fakeBackend) ResolveVMID(string) (string, error)  { return "", errors.New("not found") }
func (fakeBackend) DestroyVM(string) error              { return nil }
func (fakeBackend) DestroyUserVMs(string) error         { return nil }
func (fakeBackend) Converge(string, string) error       { return nil }
func (fakeBackend) SnapshotVM(string) error             { return nil }
func (fakeBackend) Status() (*fastvm.StatusInfo, error) { return &fastvm.StatusInfo{}, nil }
func (fakeBackend) SSHHost() string                     { return "atl1-pc1-027.hosts.telnyx.io" }

type channelEnroller struct {
	ch chan enroller.EnrollRequest
}

func (e channelEnroller) Enroll(_ context.Context, req enroller.EnrollRequest) error {
	e.ch <- req
	return nil
}

func TestCreateVMsQueuesAsyncEnrollment(t *testing.T) {
	ch := make(chan enroller.EnrollRequest, 1)
	router := NewRouterWithEnrollment(
		fakeBackend{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"test",
		EnrollmentConfig{Mode: "async", Client: channelEnroller{ch: ch}, Features: []string{"host-identity"}, Timeout: time.Second},
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/vms", bytes.NewReader([]byte(`{"user":"daan","count":1}`)))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var decoded struct {
		Data struct {
			Enrollment EnrollmentSummary `json:"enrollment"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if decoded.Data.Enrollment.Status != "queued" {
		t.Fatalf("enrollment status = %q, want queued", decoded.Data.Enrollment.Status)
	}

	select {
	case got := <-ch:
		if got.SSHHost != "atl1-pc1-027.hosts.telnyx.io" || got.SSHUser != "root" || got.Backend != "firecracker" {
			t.Fatalf("unexpected enrollment request: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for async enrollment request")
	}
}

func (f fakeBackend) ListWarmTemplates() ([]fastvm.WarmTemplate, error)         { return nil, nil }
func (f fakeBackend) WarmSnapshot(string, string) (*fastvm.WarmTemplate, error) { return nil, nil }
func (f fakeBackend) WarmLaunch(string, string, string, string) (fastvm.VMInfo, error) {
	return fastvm.VMInfo{}, nil
}
func (f fakeBackend) ListWarmTemplatesForUser(string) ([]fastvm.WarmTemplate, error) { return nil, nil }
func (f fakeBackend) UpdateWarmTemplateACL(string, string, bool, []string) (*fastvm.WarmTemplate, error) {
	return nil, nil
}
func (f fakeBackend) PauseVM(string) error  { return nil }
func (f fakeBackend) ResumeVM(string) error { return nil }
func (f fakeBackend) ExecVM(string, string, int) (*fastvm.ExecResult, error) {
	return nil, nil
}
func (f fakeBackend) ExposePort(string, int) (*fastvm.ExposedPortInfo, error) {
	return nil, nil
}
func (f fakeBackend) UnexposePort(string, int) error { return nil }
