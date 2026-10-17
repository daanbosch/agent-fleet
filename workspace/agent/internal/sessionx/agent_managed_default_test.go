package sessionx

import (
	"slices"
	"testing"
)

// The kinds that start managed by default are the hand list the callers kept before they were
// derived (codex opencode copilot cursor kiro) plus lcpp and muse, which have no Terminal route.
// The literal is the point: a kind registered with ManagedDefault, or one dropped, changes the
// set and has to be decided here.
func TestManagedDefaultKindFollowsTheRegistry(t *testing.T) {
	want := []string{"codex", "copilot", "cursor", "kiro", "lcpp", "muse", "opencode"}
	var got []string
	for _, k := range RegisteredKinds() {
		if ManagedDefaultKind(k) {
			got = append(got, k)
		}
		c := AgentOf(k).Caps()
		if c.ManagedOnly && !ManagedDefaultKind(k) {
			t.Errorf("%s: managed-only but not a managed default", k)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("managed-default kinds = %v, want %v", got, want)
	}
}

// An unregistered or alternate spelling gets no managed driver. NormalizeKind would fold these
// onto claude, which is not a managed default either — but "codex" spelled "Codex" must not
// reach the Agent as a managed request for a kind it will launch as claude.
func TestManagedDefaultKindRefusesAlternateSpellings(t *testing.T) {
	for _, k := range []string{"", "claude", "agy", "shell", "ssm", "Codex", "CODEX", " codex", "codex ",
		"kiro-cli", "cursor-agent", "github-copilot", "llamacpp", "llama.cpp", "Muse", "open-code", "antigravity"} {
		if ManagedDefaultKind(k) {
			t.Errorf("ManagedDefaultKind(%q) = true, want false", k)
		}
	}
}

// A CP newer than its Agent may name a kind the Agent has not registered yet. The Agent folds
// it to claude (NormalizeKind), which has no managed driver: an empty driver would start a
// claude TUI under the wrong kind, while driver=managed is refused with driver_unsupported
// (HandleCreateSession). Pinned so the managed request stays the fail-closed answer.
func TestUnregisteredKindFoldsToClaudeWhichHasNoManagedDriver(t *testing.T) {
	const unknown = "kind-this-agent-has-not-registered"
	if got := NormalizeKind(unknown); got != "claude" {
		t.Fatalf("NormalizeKind(%q) = %q, want claude", unknown, got)
	}
	if _, ok := managedDrivers[NormalizeKind(unknown)]; ok {
		t.Fatalf("claude has a managed driver; an unregistered kind named with driver=managed would no longer be refused")
	}
	if AgentOf(unknown).Caps().ManagedOnly {
		t.Fatalf("an unregistered kind must not inherit ManagedOnly from the claude fallback")
	}
}
