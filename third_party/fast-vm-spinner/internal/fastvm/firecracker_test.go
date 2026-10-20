package fastvm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFirecrackerKeyValues(t *testing.T) {
	values := parseFirecrackerKeyValues(`ID=60000
PID=1234
GUEST_IP=192.168.10.2
SSH=ssh -i /opt/fastvm-microvm/images/id_rsa root@192.168.10.2
ignored line
`)

	if values["ID"] != "60000" {
		t.Fatalf("ID = %q, want 60000", values["ID"])
	}
	if values["GUEST_IP"] != "192.168.10.2" {
		t.Fatalf("GUEST_IP = %q, want 192.168.10.2", values["GUEST_IP"])
	}
	if values["SSH"] != "ssh -i /opt/fastvm-microvm/images/id_rsa root@192.168.10.2" {
		t.Fatalf("SSH = %q", values["SSH"])
	}
}

func TestFirecrackerTemplateMapping(t *testing.T) {
	e := &FirecrackerEngine{defaultTemplate: "plain-ubuntu"}

	if got := e.templateName(0); got != "plain-ubuntu" {
		t.Fatalf("templateName(0) = %q, want plain-ubuntu", got)
	}
	if got := e.templateName(60000); got != "60000" {
		t.Fatalf("templateName(60000) = %q, want 60000", got)
	}
	if got := e.templateID(0, "plain-ubuntu"); got != 0 {
		t.Fatalf("templateID(default) = %d, want 0", got)
	}
	if got := e.templateID(0, "60001"); got != 60001 {
		t.Fatalf("templateID(numeric name) = %d, want 60001", got)
	}
}

func TestFirecrackerReservedIDsReserveTemplatesAndInstances(t *testing.T) {
	dir := t.TempDir()
	for _, subdir := range []string{"images", "instances", "templates"} {
		if err := os.MkdirAll(filepath.Join(dir, subdir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", subdir, err)
		}
	}
	for _, name := range []string{
		"plain-ubuntu.ext4",
		"60000.ext4",
		"60999.ext4",
		"61000.ext4",
	} {
		path := filepath.Join(dir, "templates", name)
		if err := os.WriteFile(path, nil, 0644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "instances", "60001"), 0755); err != nil {
		t.Fatalf("mkdir instance: %v", err)
	}

	e := &FirecrackerEngine{installDir: dir}
	got := e.reservedFirecrackerIDs()
	if !got[60000] || !got[60001] || !got[60999] {
		t.Fatalf("reservedFirecrackerIDs() = %v, want 60000, 60001, and 60999 reserved", got)
	}
	if got[61000] {
		t.Fatalf("reservedFirecrackerIDs() reserved 61000 outside fastvm range")
	}
}

func TestFirecrackerVMInfoUsesRootSSHCommand(t *testing.T) {
	e := &FirecrackerEngine{
		sshHost: "atl1-pc1-027.hosts.telnyx.io",
		node:    "atl1-pc1-027",
		sshUser: "root",
	}
	info := e.vmInfo("60042", VMState{
		User:         "daan",
		Name:         "fastvm-daan-1",
		TemplateName: "plain-ubuntu",
		Node:         "atl1-pc1-027",
		IP:           "192.168.10.2",
		Created:      "2026-06-01T00:00:00Z",
	})

	if info.Backend != firecrackerBackendName {
		t.Fatalf("Backend = %q, want %s", info.Backend, firecrackerBackendName)
	}
	if info.SSHUser != "root" {
		t.Fatalf("SSHUser = %q, want root", info.SSHUser)
	}
	if info.SSHCommand != "ssh -p 60042 root@atl1-pc1-027.hosts.telnyx.io" {
		t.Fatalf("SSHCommand = %q", info.SSHCommand)
	}
}

func TestFirecrackerCreateRejectsTooManyVCPUsBeforeStateAllocation(t *testing.T) {
	dir := t.TempDir()
	for _, subdir := range []string{"images", "instances", "templates"} {
		if err := os.MkdirAll(filepath.Join(dir, subdir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", subdir, err)
		}
	}
	bin := filepath.Join(dir, "bin", "firecrackerctl")
	if err := os.MkdirAll(filepath.Dir(bin), 0755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatalf("write firecrackerctl stub: %v", err)
	}

	e := NewFirecrackerEngine(FirecrackerConfig{
		InstallDir:      dir,
		Bin:             bin,
		DefaultTemplate: "9013",
		DefaultMemory:   4096,
		DefaultCores:    2,
		Node:            "atl1-pc1-027",
		SSHHost:         "atl1-pc1-027.hosts.telnyx.io",
		SSHUser:         "root",
		StateDir:        filepath.Join(dir, "state"),
	})

	_, err := e.CreateVMs("buildbox", 1, CreateOpts{Cores: firecrackerMaxVCPUs + 1})
	if err == nil {
		t.Fatal("CreateVMs accepted too many Firecracker vCPUs")
	}
	if !strings.Contains(err.Error(), "Firecracker supports at most 32 vCPUs") {
		t.Fatalf("CreateVMs error = %q", err)
	}

	vms, readErr := e.state.ReadVMs()
	if readErr != nil {
		t.Fatalf("ReadVMs: %v", readErr)
	}
	if len(vms) != 0 {
		t.Fatalf("CreateVMs registered state after validation failure: %#v", vms)
	}
}
