package fastvm

import (
	"testing"
)

// The embedded warmnat object must expose the per-clone constants fastvm
// rewrites (cilium Variables) so each clone's programs carry their own params.
func TestWarmnatSpecVariables(t *testing.T) {
	spec, err := loadWarmSpec()
	if err != nil {
		t.Fatalf("load warmnat spec: %v", err)
	}
	for _, name := range []string{"CFG_GUEST_IP", "CFG_GW_IP", "CFG_CLONE_IP", "CFG_GW_MAC"} {
		if spec.Variables[name] == nil {
			t.Fatalf("variable %s missing from warmnat.o (did you rebuild bpf/warmnat.o after editing warmnat.bpf.c?)", name)
		}
	}
	if spec.Programs["tc_to_guest"] == nil || spec.Programs["tc_from_guest"] == nil {
		t.Fatal("warmnat.o is missing the tc_to_guest/tc_from_guest programs")
	}
}

func TestWarmCloneNetDistinct(t *testing.T) {
	seen := map[string]bool{}
	for vmid := 60000; vmid < 60500; vmid++ {
		gw, clone := warmCloneNet(vmid)
		if seen[gw] {
			t.Fatalf("subnet collision at vmid %d (%s)", vmid, gw)
		}
		seen[gw] = true
		if clone == "" {
			t.Fatalf("empty clone IP at vmid %d", vmid)
		}
	}
}

func TestIppacketReg(t *testing.T) {
	got, err := ippacketReg("192.168.99.2")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0x0263a8c0 {
		t.Fatalf("ippacketReg(192.168.99.2) = %#x, want 0x0263a8c0", got)
	}
}
