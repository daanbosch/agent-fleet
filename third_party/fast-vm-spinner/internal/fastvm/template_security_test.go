package fastvm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSecurityTemplateIDsDefaultAndOverride(t *testing.T) {
	t.Setenv("FASTVM_SECURITY_TEMPLATE_IDS", "")
	if !isSecurityTemplateID(defaultSecurityTemplateID) {
		t.Fatalf("default security template %d was not accepted", defaultSecurityTemplateID)
	}
	if isSecurityTemplateID(9012) {
		t.Fatal("non-security template 9012 was accepted by default")
	}

	t.Setenv("FASTVM_SECURITY_TEMPLATE_IDS", "9020, 9021, invalid, -1")
	if !isSecurityTemplateID(9020) || !isSecurityTemplateID(9021) {
		t.Fatal("configured security template IDs were not accepted")
	}
	if isSecurityTemplateID(defaultSecurityTemplateID) {
		t.Fatalf("default security template %d should not be accepted after override", defaultSecurityTemplateID)
	}
}

func TestProxmoxTemplateSecurityBaselineUsesRegistry(t *testing.T) {
	state := NewStateManager(t.TempDir())

	if !proxmoxTemplateHasSecurityBaseline(state, defaultSecurityTemplateID) {
		t.Fatalf("template %d should be secure by default", defaultSecurityTemplateID)
	}
	if proxmoxTemplateHasSecurityBaseline(state, 60000) {
		t.Fatal("unregistered custom template should not be secure")
	}

	if err := state.RegisterTemplate("60000", TemplateState{SecurityTemplate: true}); err != nil {
		t.Fatalf("RegisterTemplate: %v", err)
	}
	if !proxmoxTemplateHasSecurityBaseline(state, 60000) {
		t.Fatal("registered custom template should be secure")
	}
}

func TestRequireProxmoxSnapshotSecurityBaseline(t *testing.T) {
	state := NewStateManager(t.TempDir())

	if err := requireProxmoxSnapshotSecurityBaseline(state, "60000", &VMState{Template: defaultSecurityTemplateID}); err != nil {
		t.Fatalf("secure source template rejected: %v", err)
	}
	if err := requireProxmoxSnapshotSecurityBaseline(state, "60001", &VMState{Template: 9012}); err == nil {
		t.Fatal("insecure source template accepted")
	}
	if err := requireProxmoxSnapshotSecurityBaseline(state, "60002", &VMState{Template: 9012, SecurityTemplate: true}); err != nil {
		t.Fatalf("VM security marker rejected: %v", err)
	}
}

func TestFirecrackerTemplateSecurityMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0755); err != nil {
		t.Fatalf("mkdir templates: %v", err)
	}

	sourcePath := filepath.Join(dir, "templates", "9013.env")
	if err := os.WriteFile(sourcePath, []byte("TEMPLATE=9013\nSECURITY_TEMPLATE=1\nSECURITY_FEATURES=falcon,lacework\n"), 0644); err != nil {
		t.Fatalf("write source env: %v", err)
	}
	targetPath := filepath.Join(dir, "templates", "60000.env")
	if err := os.WriteFile(targetPath, []byte("TEMPLATE=60000\n"), 0644); err != nil {
		t.Fatalf("write target env: %v", err)
	}

	if !firecrackerTemplateHasSecurityBaseline(dir, "9013") {
		t.Fatal("source template metadata was not recognized")
	}
	if err := inheritFirecrackerSecurityMetadata(dir, "9013", "60000", "60000"); err != nil {
		t.Fatalf("inheritFirecrackerSecurityMetadata: %v", err)
	}

	values, err := readTemplateEnv(targetPath)
	if err != nil {
		t.Fatalf("read target env: %v", err)
	}
	if values["SECURITY_TEMPLATE"] != "1" {
		t.Fatalf("SECURITY_TEMPLATE = %q, want 1", values["SECURITY_TEMPLATE"])
	}
	if values["SECURITY_FEATURES"] != "falcon,lacework" {
		t.Fatalf("SECURITY_FEATURES = %q, want falcon,lacework", values["SECURITY_FEATURES"])
	}
	if values["SECURITY_INHERITED_FROM"] != "9013" {
		t.Fatalf("SECURITY_INHERITED_FROM = %q, want 9013", values["SECURITY_INHERITED_FROM"])
	}
}

func TestFirecrackerTemplateSecurityRejectsUnsafeName(t *testing.T) {
	if _, err := firecrackerTemplateEnvPath("/tmp/fastvm", "../bad"); err == nil {
		t.Fatal("unsafe template name was accepted")
	}
}
