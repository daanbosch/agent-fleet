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
