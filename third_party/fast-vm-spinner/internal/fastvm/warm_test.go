package fastvm

import (
	"bytes"
	"strings"
	"testing"
)

func TestPatchVMState(t *testing.T) {
	// Build a minimal vmstate-shaped file: data + trailing crc64 LE.
	body := []byte{0x01, 0x02}
	body = append(body, []byte("/opt/fastvm-microvm/instances/60000/rootfs.ext4")...)
	body = append(body, 0x00, 0x03)
	body = append(body, []byte("fc60000514539")...)
	whole := append([]byte{}, body...)
	whole = append(whole, le64(crc64xz(0, body))...)
	if err := validateVMStateCRC(whole); err != nil {
		t.Fatalf("source blob invalid: %v", err)
	}

	patched, err := patchVMState(whole,
		"/opt/fastvm-microvm/instances/60000/rootfs.ext4",
		"/opt/fastvm-microvm/instances/60501/rootfs.ext4")
	if err != nil {
		t.Fatalf("path patch: %v", err)
	}
	if !bytes.Contains(patched, []byte("instances/60501/rootfs.ext4")) {
		t.Fatal("patched blob missing new path")
	}
	if len(patched) != len(whole) {
		t.Fatalf("length changed: %d -> %d", len(whole), len(patched))
	}
	if err := validateVMStateCRC(patched); err != nil {
		t.Fatalf("patched blob invalid after CRC refresh: %v", err)
	}

	// Unequal lengths must be refused.
	if _, err := patchVMState(whole, "short", "much-longer-string"); err == nil {
		t.Fatal("expected length-mismatch error")
	}
	// Missing string must be refused.
	if _, err := patchVMState(whole, "/nonexistent/path/here", "/nonexistent/path/there"); err == nil {
		t.Fatal("expected missing-string error")
	}
}

func TestCloneTapName(t *testing.T) {
	name, err := cloneTapName(60501, 13)
	if err != nil {
		t.Fatalf("cloneTapName: %v", err)
	}
	if len(name) != 13 {
		t.Fatalf("tap name %q has length %d, want 13", name, len(name))
	}
	if !strings.HasPrefix(name, "fc60501") {
		t.Fatalf("unexpected tap name %q", name)
	}
	if name == "fc60000514539" {
		t.Fatal("generated tap collides with the source tap constant")
	}
	if _, err := cloneTapName(60501, 4); err == nil {
		t.Fatal("expected error for impossible length")
	}
}

func TestWarmValidUsername(t *testing.T) {
	for _, ok := range []string{"daan", "revenue-coder", "u_1"} {
		if !warmValidUsername(ok) {
			t.Fatalf("want valid: %q", ok)
		}
	}
	for _, bad := range []string{"", "with space", "with/slash"} {
		if warmValidUsername(bad) {
			t.Fatalf("want invalid: %q", bad)
		}
	}
}

// TestCrc64xzVector checks the Go port against the crc64 crate vectors:
// crc64(0, "123456789") == 0xe9c6d914c4b8d9ca and firecracker's doc example
// crc64(0, [123 u8;16]) == 0x29D5357216326566 (src/vmm/src/snapshot/crc.rs).
// and hold. Firecracker's format uses the `crc64` crate = CRC-64/Jones
// (poly 0xAD93D23594C935A9).
func TestCrc64xzVector(t *testing.T) {
	data := make([]byte, 16)
	for i := range data {
		data[i] = 123
	}
	if got := crc64xz(0, []byte("123456789")); got != 0xe9c6d914c4b8d9ca {
		t.Fatalf("crc64xz(\"123456789\") = %#x, want 0xe9c6d914c4b8d9ca", got)
	}
	if got := crc64xz(0, data); got != 0x29D5357216326566 {
		t.Fatalf("crc64xz = %#x, want 0x29D5357216326566", got)
	}
}

// TestPatchedVMStateZeroResidue proves the trailing-CRC property: after
// patching and re-CRCing the file, crc64(0, wholeFile) == 0, exactly the
// invariant firecracker validates on load.
func TestPatchedVMStateZeroResidue(t *testing.T) {
	header := []byte{0x64, 0x00, 0x00, 0x00, 0x00}
	body := []byte("/opt/fastvm-microvm/instances/60000/rootfs.ext4")
	test := append(append(append([]byte{}, header...), body...), 0xAB, 0xCD)
	whole := append(append([]byte{}, test...), le64(crc64xz(0, test))...)
	if err := validateVMStateCRC(whole); err != nil {
		t.Fatalf("fresh vmstate fails validation: %v", err)
	}
	patched, err := patchVMState(whole,
		"/opt/fastvm-microvm/instances/60000/rootfs.ext4",
		"/opt/fastvm-microvm/instances/66666/rootfs.ext4")
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if err := validateVMStateCRC(patched); err != nil {
		t.Fatalf("patched vmstate fails validation: %v", err)
	}
	if !bytes.Contains(patched, []byte("/instances/66666/rootfs.ext4")) {
		t.Fatal("patch lost the rewritten path")
	}
}
