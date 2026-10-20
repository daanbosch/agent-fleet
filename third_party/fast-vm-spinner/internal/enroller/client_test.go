package enroller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientPostsEnrollmentWithBearerToken(t *testing.T) {
	var got EnrollRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/enroll" {
			t.Fatalf("path = %s, want /v1/enroll", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		writeJSON(w, http.StatusOK, EnrollResponse{VMID: got.VMID, Status: "enrolled"})
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, Token: "secret"}
	err := client.Enroll(context.Background(), EnrollRequest{
		VMID:    "60000",
		SSHHost: "atl1-pc1-027.hosts.telnyx.io",
		SSHPort: 60000,
		SSHUser: "root",
	})
	if err != nil {
		t.Fatalf("Enroll returned error: %v", err)
	}
	if got.VMID != "60000" {
		t.Fatalf("VMID = %q, want 60000", got.VMID)
	}
}
