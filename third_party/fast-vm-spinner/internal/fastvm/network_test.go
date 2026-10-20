package fastvm

import (
	"testing"
)

func TestBridgeName(t *testing.T) {
	// Verify bridge names are deterministic and <= 15 chars
	users := []string{"daan", "agent007", "test", "a-very-long-username-here"}

	seen := make(map[string]string)
	for _, user := range users {
		got := bridgeName(user)

		if len(got) > 15 {
			t.Errorf("bridgeName(%q) = %q, exceeds 15 char Linux limit (len=%d)", user, got, len(got))
		}

		// Must start with "fvm-"
		if got[:4] != "fvm-" {
			t.Errorf("bridgeName(%q) = %q, doesn't start with fvm-", user, got)
		}

		// Must be deterministic
		if bridgeName(user) != got {
			t.Errorf("bridgeName(%q) not deterministic", user)
		}

		// Must be unique per user
		if prev, ok := seen[got]; ok {
			t.Errorf("bridgeName collision: %q and %q both produce %q", user, prev, got)
		}
		seen[got] = user
	}
}

func TestBridgeNameMaxLength(t *testing.T) {
	// Even very long usernames should produce bridge names <= 15 chars
	longUser := "this-is-a-very-long-username-that-should-not-break-things"
	got := bridgeName(longUser)
	if len(got) > 15 {
		t.Errorf("bridgeName(%q) length = %d, want <= 15", longUser, len(got))
	}
}

func TestBlockNetwork(t *testing.T) {
	tests := []struct {
		block int
		want  string
	}{
		{1, "192.168.0.64"},
		{2, "192.168.0.128"},
		{3, "192.168.0.192"},
		{4, "192.168.1.0"},
		{5, "192.168.1.64"},
		{8, "192.168.2.0"},
		{100, "192.168.25.0"},
		{1023, "192.168.255.192"},
	}

	for _, tt := range tests {
		got := blockNetwork(tt.block)
		if got != tt.want {
			t.Errorf("blockNetwork(%d) = %q, want %q", tt.block, got, tt.want)
		}
	}
}

func TestBlockGateway(t *testing.T) {
	tests := []struct {
		block int
		want  string
	}{
		{1, "192.168.0.65"},
		{4, "192.168.1.1"},
		{5, "192.168.1.65"},
		{100, "192.168.25.1"},
	}

	for _, tt := range tests {
		got := blockGateway(tt.block)
		if got != tt.want {
			t.Errorf("blockGateway(%d) = %q, want %q", tt.block, got, tt.want)
		}
	}
}

func TestBlockVMIP(t *testing.T) {
	tests := []struct {
		block  int
		offset int
		want   string
	}{
		{1, 1, "192.168.0.66"},     // first VM in block 1
		{1, 2, "192.168.0.67"},     // second VM
		{1, 50, "192.168.0.115"},   // 50th VM
		{4, 1, "192.168.1.2"},      // first VM in block 4
		{100, 3, "192.168.25.4"},   // 3rd VM in block 100
	}

	for _, tt := range tests {
		got := blockVMIP(tt.block, tt.offset)
		if got != tt.want {
			t.Errorf("blockVMIP(%d, %d) = %q, want %q", tt.block, tt.offset, got, tt.want)
		}
	}
}

func TestBlockCIDR(t *testing.T) {
	tests := []struct {
		block int
		want  string
	}{
		{1, "192.168.0.64/26"},
		{4, "192.168.1.0/26"},
		{100, "192.168.25.0/26"},
	}

	for _, tt := range tests {
		got := blockCIDR(tt.block)
		if got != tt.want {
			t.Errorf("blockCIDR(%d) = %q, want %q", tt.block, got, tt.want)
		}
	}
}
