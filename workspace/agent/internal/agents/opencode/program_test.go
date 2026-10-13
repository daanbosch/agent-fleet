package opencode

import (
	"strings"
	"testing"
)

func TestBuildOpencodeProgramAuto(t *testing.T) {
	useMajor(t, 1)
	// Default launch: unattended --auto bypass, no session/model when unset.
	got := buildProgram("", "", "", false)
	if !strings.Contains(got, "--auto") {
		t.Fatalf("expected --auto (permission bypass) in %q", got)
	}

	// With a captured session id and model, they're passed through alongside --auto.
	got = buildProgram("anthropic/claude-x", "plan", "ses_abc", false)
	for _, want := range []string{"--auto", "--session", "ses_abc", "--model", "anthropic/claude-x"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in %q", want, got)
		}
	}
	if !strings.Contains(got, "--agent plan") {
		t.Fatalf("expected plan agent in %q", got)
	}
}

func TestBuildOpencodeProgramFork(t *testing.T) {
	useMajor(t, 1)
	// Fork launch (first boot of a forked slot): resume the SOURCE session with --fork
	// so opencode copies it into a new conversation and diverges.
	got := buildProgram("", "normal", "ses_src", true)
	if !strings.Contains(got, "--session 'ses_src'") || !strings.Contains(got, "--fork") {
		t.Fatalf("expected --session ses_src --fork in %q", got)
	}
	if !strings.Contains(got, "--agent build") {
		t.Fatalf("expected normal mode to select build agent in %q", got)
	}
	// A plain resume must NOT carry --fork.
	got = buildProgram("", "", "ses_src", false)
	if strings.Contains(got, "--fork") {
		t.Fatalf("plain resume must not fork: %q", got)
	}
}

// v1 keeps its exact launch line even while a v2 exists in the same test binary.
func TestBuildProgramV1Unchanged(t *testing.T) {
	useMajor(t, 1)
	got := buildProgram("anthropic/claude-x", "plan", "ses_abc", true)
	want := "opencode --auto --session 'ses_abc' --fork --model 'anthropic/claude-x' --agent plan"
	if got != want {
		t.Fatalf("v1 launch line changed:\n got %q\nwant %q", got, want)
	}
}

func TestBuildProgramV2FreshSession(t *testing.T) {
	useMajor(t, 2)
	// Nothing chosen: a bare private-server TUI, no config, none of the v1-only flags.
	got := buildProgram("", "", "", false)
	if got != "opencode --standalone --auto" {
		t.Fatalf("plain v2 launch = %q", got)
	}
	// Model and mode of a fresh session ride the inline config; the flags do not exist on the v2 TUI.
	got = buildProgram("stub/stub-2", "plan", "", false)
	for _, bad := range []string{"--model", "--agent", "--fork"} {
		if strings.Contains(got, bad) {
			t.Fatalf("v2 TUI has no %s: %q", bad, got)
		}
	}
	want := `OPENCODE_CONFIG_CONTENT='{"default_agent":"plan","model":"stub/stub-2"}' opencode --standalone --auto`
	if got != want {
		t.Fatalf("fresh v2 launch:\n got %q\nwant %q", got, want)
	}
	if got := buildProgram("", "normal", "", false); !strings.Contains(got, `"default_agent":"build"`) {
		t.Fatalf("normal mode must select the build agent: %q", got)
	}
}

func TestBuildProgramV2Resume(t *testing.T) {
	useMajor(t, 2)
	// Nothing to change: attach straight to the session.
	if got := buildProgram("", "", "ses_abc", false); got != "opencode --standalone --auto --session 'ses_abc'" {
		t.Fatalf("plain resume = %q", got)
	}
	// An existing session ignores the config, so model and agent are set through the API first.
	got := buildProgram("stub/stub-2#high", "plan", "ses_abc", false)
	for _, want := range []string{
		`opencode api --standalone POST /api/session/"$AF_OC_SES"/model -d '{"model":{"id":"stub-2","providerID":"stub","variant":"high"}}'`,
		`opencode api --standalone POST /api/session/"$AF_OC_SES"/agent -d '{"agent":"plan"}'`,
		`AF_OC_SES='ses_abc'`,
		`opencode --standalone --auto --session "$AF_OC_SES"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("resume line lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "OPENCODE_CONFIG_CONTENT") || strings.Contains(got, "--fork") {
		t.Fatalf("resume must not use inline config or --fork: %q", got)
	}
}

func TestBuildProgramV2Fork(t *testing.T) {
	useMajor(t, 2)
	got := buildProgram("", "normal", "ses_src", true)
	if !strings.Contains(got, "POST /api/session/'ses_src'/fork") {
		t.Fatalf("fork must go through the API: %q", got)
	}
	if strings.Contains(got, "--fork") {
		t.Fatalf("the v2 TUI has no --fork: %q", got)
	}
	// The copy is what the TUI resumes, and a failed fork must not fall through to the source.
	if !strings.Contains(got, `[ -z "$AF_OC_SES" ]`) || !strings.Contains(got, `--session "$AF_OC_SES"`) {
		t.Fatalf("fork line must guard and resume the copy: %q", got)
	}
	if strings.Contains(got, "--session 'ses_src'") {
		t.Fatalf("fork line must never attach to the source session: %q", got)
	}
	if !strings.Contains(got, `/agent -d '{"agent":"build"}'`) {
		t.Fatalf("fork line must still apply the mode: %q", got)
	}
}
