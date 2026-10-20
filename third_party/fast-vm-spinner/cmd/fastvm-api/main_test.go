package main

import "testing"

func TestNewBackendDefaultsToFirecracker(t *testing.T) {
	backend, err := newBackend("", t.TempDir(), "atl1-pc1-027.hosts.telnyx.io", t.TempDir(), "", "plain-ubuntu")
	if err != nil {
		t.Fatalf("newBackend default returned error: %v", err)
	}

	status, err := backend.Status()
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.Backend != "firecracker" {
		t.Fatalf("Backend = %q, want firecracker", status.Backend)
	}
}
