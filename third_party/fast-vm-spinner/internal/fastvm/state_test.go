package fastvm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateManagerAllocateBlock(t *testing.T) {
	dir := t.TempDir()

	sm := NewStateManager(dir)

	// First allocation for a user
	block1, err := sm.AllocateBlock("daan")
	if err != nil {
		t.Fatalf("AllocateBlock(daan): %v", err)
	}
	if block1 < 1 || block1 > 1023 {
		t.Errorf("block = %d, want 1-1023", block1)
	}

	// Same user should get same block
	block1b, err := sm.AllocateBlock("daan")
	if err != nil {
		t.Fatalf("AllocateBlock(daan) again: %v", err)
	}
	if block1b != block1 {
		t.Errorf("second call returned %d, want %d", block1b, block1)
	}

	// Different user should get different block
	block2, err := sm.AllocateBlock("agent007")
	if err != nil {
		t.Fatalf("AllocateBlock(agent007): %v", err)
	}
	if block2 == block1 {
		t.Errorf("two users got same block: %d", block2)
	}

	// Verify persisted to disk
	data, err := os.ReadFile(filepath.Join(dir, "users.json"))
	if err != nil {
		t.Fatalf("read users.json: %v", err)
	}
	if len(data) < 10 {
		t.Errorf("users.json too small: %s", string(data))
	}
}

func TestStateManagerRegisterUnregisterVM(t *testing.T) {
	dir := t.TempDir()
	sm := NewStateManager(dir)

	// Register a VM
	err := sm.RegisterVM("60001", VMState{
		User:     "testuser",
		Name:     "fastvm-testuser-1",
		Template: 9012,
		Node:     "node1",
		Created:  "2024-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("RegisterVM: %v", err)
	}

	// Read it back
	vm, ok := sm.GetVM("60001")
	if !ok {
		t.Fatal("GetVM returned false")
	}
	if vm.User != "testuser" {
		t.Errorf("vm.User = %q, want testuser", vm.User)
	}
	if vm.Name != "fastvm-testuser-1" {
		t.Errorf("vm.Name = %q, want fastvm-testuser-1", vm.Name)
	}

	// Check UserVMIDs
	ids := sm.UserVMIDs("testuser")
	if len(ids) != 1 || ids[0] != "60001" {
		t.Errorf("UserVMIDs = %v, want [60001]", ids)
	}

	// UserHasVMs
	if !sm.UserHasVMs("testuser") {
		t.Error("UserHasVMs returned false")
	}
	if sm.UserHasVMs("nobody") {
		t.Error("UserHasVMs(nobody) returned true")
	}

	// Unregister
	err = sm.UnregisterVM("60001")
	if err != nil {
		t.Fatalf("UnregisterVM: %v", err)
	}

	_, ok = sm.GetVM("60001")
	if ok {
		t.Error("GetVM returned true after unregister")
	}
}

func TestStateManagerNextVMID(t *testing.T) {
	dir := t.TempDir()
	sm := NewStateManager(dir)

	// Empty state — should get 60000
	id, err := sm.NextVMID()
	if err != nil {
		t.Fatalf("NextVMID: %v", err)
	}
	if id != 60000 {
		t.Errorf("NextVMID = %d, want 60000", id)
	}

	// Register 60000 and try again
	sm.RegisterVM("60000", VMState{User: "x", Name: "x", Template: 1, Node: "n", Created: "t"})
	id, err = sm.NextVMID()
	if err != nil {
		t.Fatalf("NextVMID: %v", err)
	}
	if id != 60001 {
		t.Errorf("NextVMID = %d, want 60001", id)
	}
}

func TestStateManagerNextVMIDExcluding(t *testing.T) {
	dir := t.TempDir()
	sm := NewStateManager(dir)

	id, err := sm.NextVMIDExcluding(map[int]bool{
		60000: true,
		60001: true,
	})
	if err != nil {
		t.Fatalf("NextVMIDExcluding: %v", err)
	}
	if id != 60002 {
		t.Errorf("NextVMIDExcluding = %d, want 60002", id)
	}
}

func TestStateManagerNameExists(t *testing.T) {
	dir := t.TempDir()
	sm := NewStateManager(dir)

	sm.RegisterVM("60000", VMState{User: "u", Name: "fastvm-u-1", Template: 1, Node: "n", Created: "t"})

	if !sm.NameExists("fastvm-u-1") {
		t.Error("NameExists returned false for existing name")
	}
	if sm.NameExists("fastvm-u-99") {
		t.Error("NameExists returned true for non-existing name")
	}
}

func TestStateManagerFindVMByName(t *testing.T) {
	dir := t.TempDir()
	sm := NewStateManager(dir)

	sm.RegisterVM("60000", VMState{User: "u", Name: "my-custom-vm", Template: 1, Node: "n", Created: "t"})
	sm.RegisterVM("60001", VMState{User: "u", Name: "fastvm-u-1", Template: 1, Node: "n", Created: "t"})

	// Found by custom name
	vmid, vm, ok := sm.FindVMByName("my-custom-vm")
	if !ok {
		t.Fatal("FindVMByName returned false for existing name")
	}
	if vmid != "60000" {
		t.Errorf("vmid = %q, want 60000", vmid)
	}
	if vm.Name != "my-custom-vm" {
		t.Errorf("vm.Name = %q, want my-custom-vm", vm.Name)
	}

	// Found by auto name
	vmid2, _, ok2 := sm.FindVMByName("fastvm-u-1")
	if !ok2 {
		t.Fatal("FindVMByName returned false for auto name")
	}
	if vmid2 != "60001" {
		t.Errorf("vmid = %q, want 60001", vmid2)
	}

	// Not found
	_, _, ok3 := sm.FindVMByName("nonexistent")
	if ok3 {
		t.Error("FindVMByName returned true for nonexistent name")
	}
}

func TestValidVMName(t *testing.T) {
	tests := []struct {
		name  string
		valid bool
	}{
		{"my-vm", true},
		{"my_vm_123", true},
		{"a", true},
		{"clawhouse-pr92-test", true},
		{"A-B-C", true},
		{"vm" + string(make([]byte, 62)), false}, // too long (>63)
		{"-starts-with-dash", false},
		{"_starts-with-underscore", false},
		{"has spaces", false},
		{"has.dots", false},
		{"", false},
	}

	for _, tt := range tests {
		got := validVMName.MatchString(tt.name)
		if got != tt.valid {
			t.Errorf("validVMName(%q) = %v, want %v", tt.name, got, tt.valid)
		}
	}
}

func TestCalcVMIPFromName(t *testing.T) {
	tests := []struct {
		vmid  string
		name  string
		user  string
		block int
		want  string
	}{
		{"60001", "fastvm-daan-1", "daan", 100, "192.168.25.2"},
		{"60002", "fastvm-daan-3", "daan", 100, "192.168.25.4"},
		{"60003", "fastvm-agent-1", "agent", 1, "192.168.0.66"},
		{"60004", "unknown-format", "user", 4, "192.168.1.2"}, // fallback to suffix=1
	}

	for _, tt := range tests {
		got := calcVMIPFromName(tt.vmid, tt.name, tt.user, tt.block)
		if got != tt.want {
			t.Errorf("calcVMIPFromName(%q, %q, %q, %d) = %q, want %q",
				tt.vmid, tt.name, tt.user, tt.block, got, tt.want)
		}
	}
}
