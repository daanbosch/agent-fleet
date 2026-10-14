package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents/opencode"
)

// fakeNpm stands in for `npm install --prefix`: it leaves what @opencode/cli leaves (the
// postinstall's copy at bin/opencode.exe, a manifest and the platform packages) with a binary
// that reports version.
func fakeNpm(t *testing.T, reported string, calls *int) func(dir, ver string) error {
	t.Helper()
	return func(dir, ver string) error {
		*calls++
		cli := filepath.Join(dir, "node_modules", "@opencode", "cli")
		for _, d := range []string{filepath.Join(cli, "bin"),
			filepath.Join(dir, "node_modules", "@opencode", "cli-linux-x64", "bin"),
			filepath.Join(dir, "node_modules", "@opencode", "cli-linux-x64-musl", "bin")} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return err
			}
		}
		return os.WriteFile(filepath.Join(cli, "bin", "opencode.exe"), []byte("#!/bin/sh\necho 'opencode v"+reported+"'\n"), 0o755)
	}
}

func TestInstallOpencode2PrunesPlatformPackagesAndStaysOffPATH(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	calls := 0
	if err := installOpencode2("2.0.28", fakeNpm(t, "2.0.28", &calls)); err != nil {
		t.Fatal(err)
	}
	prefix := opencode.V2Prefix()
	if opencode.V2Binary() == "" {
		t.Fatal("the binary is not where the launcher looks for it")
	}
	if ents, _ := filepath.Glob(filepath.Join(prefix, "node_modules", "@opencode", "cli-*")); len(ents) != 0 {
		t.Fatalf("platform packages were kept: %v", ents)
	}
	for _, p := range []string{prefix + ".staging", prefix + ".old"} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s left behind", p)
		}
	}
	// Not on PATH: nothing was installed outside the private prefix.
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".local", "bin", "opencode")); err == nil {
		t.Fatal("the install put an opencode on PATH")
	}
	// The install alone does not switch anything on.
	if opencode.WantsV2() || opencode.UseV2() {
		t.Fatal("install-opencode2 must not switch af to opencode 2")
	}
	if err := installOpencode2("2.0.28", fakeNpm(t, "2.0.28", &calls)); err != nil || calls != 1 {
		t.Fatalf("a second run of the same version must not reinstall (err=%v calls=%d)", err, calls)
	}
}

func TestInstallOpencode2WrongVersionKeepsThePreviousInstall(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	calls := 0
	if err := installOpencode2("2.0.28", fakeNpm(t, "2.0.28", &calls)); err != nil {
		t.Fatal(err)
	}
	err := installOpencode2("2.0.29", fakeNpm(t, "2.0.28", &calls)) // reports the wrong version
	if err == nil || !strings.Contains(err.Error(), "reports") {
		t.Fatalf("err = %v, want a version mismatch", err)
	}
	if v := opencode2Version(opencode.V2Binary()); v != "2.0.28" {
		t.Fatalf("the previous install was damaged: version %q", v)
	}
	boom := errors.New("registry down")
	if err := installOpencode2("2.0.30", func(string, string) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if v := opencode2Version(opencode.V2Binary()); v != "2.0.28" {
		t.Fatalf("a failed npm run damaged the previous install: %q", v)
	}
}

func TestOpencodeMajorStatusNamesTheState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(opencode.MajorEnv, "")
	if got := opencodeMajorStatus(); !strings.Contains(got, "off (pinned opencode 1.x)") {
		t.Fatalf("default status = %q", got)
	}
	_ = opencode.SetWantsV2(true)
	if got := opencodeMajorStatus(); !strings.Contains(got, "not installed") {
		t.Fatalf("requested-without-binary status = %q", got)
	}
}
