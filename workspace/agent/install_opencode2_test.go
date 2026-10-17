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

// opencodeOnPath puts a fake opencode of the given package and version first on PATH, the shape
// `npm install -g` leaves behind.
func opencodeOnPath(t *testing.T, pkg, version string) {
	t.Helper()
	root := t.TempDir()
	pkgDir := filepath.Join(root, "lib", "node_modules", filepath.FromSlash(pkg))
	if err := os.MkdirAll(filepath.Join(pkgDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(`{"name":"`+pkg+`","version":"`+version+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(pkgDir, "bin", "opencode.exe")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, filepath.Join(root, "bin", "opencode")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestOpencodeMajorStatusNamesTheState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(opencode.MajorEnv, "")
	opencodeOnPath(t, "opencode-ai", "1.19.0") // a rollback pin: the opt-in is live only next to 1.x
	if got := opencodeMajorStatus(); !strings.Contains(got, "off (pinned opencode 1.x)") {
		t.Fatalf("default status = %q", got)
	}
	_ = opencode.SetWantsV2(true)
	if got := opencodeMajorStatus(); !strings.Contains(got, "not installed") {
		t.Fatalf("requested-without-binary status = %q", got)
	}
}

// With the pinned opencode on 2.x, a workspace that had the opt-in on (switch file, private
// install) is told the leftovers are ignored and where they are; one that never opted in is not
// told about leftovers it does not have.
func TestOpencodeMajorStatusWhenThePinIsV2(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(opencode.MajorEnv, "")
	opencodeOnPath(t, "@opencode/cli", "2.0.29")

	clean := opencodeMajorStatus()
	if !strings.Contains(clean, "already 2.x") || strings.Contains(clean, "Leftovers") {
		t.Fatalf("status for a workspace that never opted in = %q", clean)
	}

	if err := installOpencode2("2.0.28", fakeNpm(t, "2.0.28", new(int))); err != nil {
		t.Fatal(err)
	}
	if err := opencode.SetWantsV2(true); err != nil {
		t.Fatal(err)
	}
	if opencode.UseV2() {
		t.Fatal("the private opt-in is used although the pinned opencode is 2.x")
	}
	got := opencodeMajorStatus()
	for _, want := range []string{"already 2.x", "Leftovers", opencode.V2Prefix(), "opencode2-data", "not merged"} {
		if !strings.Contains(got, want) {
			t.Errorf("status lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "on (opencode 2:") {
		t.Fatalf("status claims the opt-in is on: %s", got)
	}
}
