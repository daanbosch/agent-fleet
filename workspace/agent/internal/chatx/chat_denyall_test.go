package chatx

// The deny-all one-shot route (chat_denyall.go). A fake `claude` on PATH stands in for the CLI:
// it logs every invocation (argv and the first stdin line), prints an init event whose tool and
// MCP lists the test chooses, and answers with a result event. Nothing here starts a real CLI.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/usagex"
)

const secretPrompt = "TRANSCRIPT-MARKER please summarise"

// fakeDenyAllClaude installs a fake claude. probeInit and runInit are the JSON lists spliced into
// the init event of the first (the probe) and every later invocation, e.g.
// `"tools":[],"mcp_servers":[]`. It returns the directory holding the invocation log.
func fakeDenyAllClaude(t *testing.T, probeInit, runInit string) string {
	t.Helper()
	bin := t.TempDir()
	logDir := t.TempDir()
	script := `#!/bin/sh
LOG="` + logDir + `"
n=$(cat "$LOG/count" 2>/dev/null || echo 0)
n=$((n+1)); echo $n > "$LOG/count"
echo "$*" > "$LOG/argv.$n"
head -n 1 > "$LOG/stdin.$n"
if [ "$n" = 1 ]; then INIT='` + probeInit + `'; else INIT='` + runInit + `'; fi
echo '{"type":"system","subtype":"init",'"$INIT"',"model":"fake"}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"FAKE-REPLY","total_cost_usd":0.001,"usage":{"input_tokens":10,"output_tokens":2}}'
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return logDir
}

func denyAllEnv(t *testing.T) {
	t.Helper()
	withTempHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Cleanup(SetHeadlessAvailableForTest("claude", true))
}

func invocations(t *testing.T, logDir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(logDir, "count"))
	if err != nil {
		return 0
	}
	n := 0
	for _, c := range strings.TrimSpace(string(b)) {
		n = n*10 + int(c-'0')
	}
	return n
}

func readLog(t *testing.T, logDir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(logDir, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return string(b)
}

func TestOneShotDenyAllRunsAfterAProbeThatShowsNothing(t *testing.T) {
	denyAllEnv(t)
	clean := `"tools":[],"mcp_servers":[]`
	logDir := fakeDenyAllClaude(t, clean, clean)

	reply, err := OneShotDenyAll(context.Background(), usagex.FeatureMemoryReview, OneShotShort, "persona", secretPrompt)
	if err != nil {
		t.Fatalf("OneShotDenyAll: %v", err)
	}
	if reply != "FAKE-REPLY" {
		t.Fatalf("reply = %q", reply)
	}
	if n := invocations(t, logDir); n != 2 {
		t.Fatalf("claude ran %d times, want the probe and the call", n)
	}
	// The probe never carries the transcript; the call does.
	if got := strings.TrimSpace(readLog(t, logDir, "stdin.1")); got != "/help" {
		t.Fatalf("the probe stdin = %q", got)
	}
	if got := strings.TrimSpace(readLog(t, logDir, "stdin.2")); got != secretPrompt {
		t.Fatalf("the call stdin = %q", got)
	}
	// Both runs use the pinned deny-all argv, so the probe proves what the call will have.
	for _, f := range []string{"argv.1", "argv.2"} {
		argv := readLog(t, logDir, f)
		for _, want := range []string{"--strict-mcp-config", "--no-chrome", "--output-format stream-json", "--tools "} {
			if !strings.Contains(argv, want) {
				t.Fatalf("%s lacks %q: %s", f, want, argv)
			}
		}
	}
}

// The reviewer's finding on #2335: strict MCP alone leaves Claude in Chrome connected when the
// environment enables it. A flag is not evidence, so a probe that lists tools or servers stops
// the call before the transcript is handed over.
func TestOneShotDenyAllRefusesWhenTheProbeListsToolsOrServers(t *testing.T) {
	cases := map[string]string{
		"tools":            `"tools":["mcp__claude-in-chrome__navigate"],"mcp_servers":[]`,
		"servers":          `"tools":[],"mcp_servers":[{"name":"claude-in-chrome","status":"connected"}]`,
		"no tools field":   `"mcp_servers":[]`,
		"no servers field": `"tools":[]`,
		"empty event":      `"x":1`,
	}
	for name, init := range cases {
		t.Run(name, func(t *testing.T) {
			denyAllEnv(t)
			logDir := fakeDenyAllClaude(t, init, `"tools":[],"mcp_servers":[]`)
			reply, err := OneShotDenyAll(context.Background(), usagex.FeatureMemoryReview, OneShotShort, "persona", secretPrompt)
			if !errors.Is(err, ErrDenyAllUnavailable) {
				t.Fatalf("err = %v, want ErrDenyAllUnavailable", err)
			}
			if reply != "" {
				t.Fatalf("reply = %q", reply)
			}
			if n := invocations(t, logDir); n != 1 {
				t.Fatalf("claude ran %d times: only the probe may run", n)
			}
			if strings.Contains(readLog(t, logDir, "stdin.1"), "TRANSCRIPT-MARKER") {
				t.Fatal("the transcript reached a backend that was not verified")
			}
		})
	}
}

// The probe can pass and the real run still differ (an account connector that appears in between,
// a config written meanwhile): the call's own init event is checked and its reply thrown away.
func TestOneShotDenyAllDiscardsAReplyFromARunThatHadTools(t *testing.T) {
	denyAllEnv(t)
	logDir := fakeDenyAllClaude(t, `"tools":[],"mcp_servers":[]`, `"tools":["Bash"],"mcp_servers":[]`)
	reply, err := OneShotDenyAll(context.Background(), usagex.FeatureMemoryReview, OneShotShort, "persona", secretPrompt)
	if err == nil || reply != "" {
		t.Fatalf("reply = %q, err = %v: a run with tools must not return a reply", reply, err)
	}
	if invocations(t, logDir) != 2 {
		t.Fatal("expected the probe and the call")
	}
}

// A feature pinned to a vendor that cannot run deny-all fails: the transcript does not move to
// another vendor behind the member's back, and nothing is started.
func TestOneShotDenyAllNeverMovesAPinnedFeatureToAnotherVendor(t *testing.T) {
	writeUIPrefs(t, `{"aiFeatureAgents":{"memory.review":"codex"}}`)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Cleanup(SetHeadlessAvailableForTest("claude", true))
	t.Cleanup(SetHeadlessAvailableForTest("codex", true))
	clean := `"tools":[],"mcp_servers":[]`
	logDir := fakeDenyAllClaude(t, clean, clean)

	_, err := OneShotDenyAll(context.Background(), usagex.FeatureMemoryReview, OneShotShort, "persona", secretPrompt)
	if !errors.Is(err, ErrDenyAllUnavailable) {
		t.Fatalf("err = %v, want ErrDenyAllUnavailable", err)
	}
	if invocations(t, logDir) != 0 {
		t.Fatal("a CLI was started for a pin that cannot qualify")
	}
}

// With no pin, a shared order that leads with a kind that cannot qualify skips it.
func TestOneShotDenyAllSkipsKindsThatCannotQualify(t *testing.T) {
	writeUIPrefs(t, `{"aiAssistOrder":["codex","claude"]}`)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Cleanup(SetHeadlessAvailableForTest("claude", true))
	t.Cleanup(SetHeadlessAvailableForTest("codex", true))
	clean := `"tools":[],"mcp_servers":[]`
	logDir := fakeDenyAllClaude(t, clean, clean)

	if _, err := OneShotDenyAll(context.Background(), usagex.FeatureMemoryReview, OneShotShort, "persona", secretPrompt); err != nil {
		t.Fatalf("err = %v", err)
	}
	if invocations(t, logDir) != 2 {
		t.Fatal("claude should have run")
	}
}

func TestOneShotDenyAllWithoutAQualifyingBackend(t *testing.T) {
	writeUIPrefs(t, `{}`)
	t.Cleanup(SetHeadlessAvailableForTest("claude", false))
	for _, k := range []string{"codex", "opencode", "cursor", "agy", "muse"} {
		t.Cleanup(SetHeadlessAvailableForTest(k, true))
	}
	logDir := fakeDenyAllClaude(t, `"tools":[],"mcp_servers":[]`, `"tools":[],"mcp_servers":[]`)
	_, err := OneShotDenyAll(context.Background(), usagex.FeatureMemoryReview, OneShotShort, "persona", secretPrompt)
	if n := invocations(t, logDir); !errors.Is(err, ErrDenyAllUnavailable) || n != 0 {
		t.Fatalf("err = %v, invocations = %d", err, n)
	}
}

func TestParseClaudeStreamCountsOnlyWhatTheInitEventLists(t *testing.T) {
	out := []byte(`not json
{"type":"assistant","message":{"tools":["x"]}}
{"type":"system","subtype":"init","tools":["a","b"],"mcp_servers":[{"name":"s"}]}
{"type":"result","result":"R","is_error":false}
`)
	s := parseClaudeStream(out)
	if !s.initSeen || s.tools != 2 || s.mcp != 1 || !s.resultSeen || s.result.Result != "R" {
		t.Fatalf("%+v", s)
	}
	if s.violation() == "" {
		t.Fatal("two tools must be a violation")
	}
	if (claudeStream{}).violation() == "" {
		t.Fatal("no evidence must be a violation")
	}
}

func TestDenyAllKindIsClaudeOnly(t *testing.T) {
	for _, k := range []string{"codex", "opencode", "cursor", "agy", "muse", "kiro", "copilot", "lcpp", ""} {
		if DenyAllKind(k) {
			t.Fatalf("%q must not qualify until a deny-all argv is verified for it", k)
		}
	}
	if !DenyAllKind("claude") {
		t.Fatal("claude qualifies")
	}
}

// A failed run that was given the transcript must not put what the process printed into its error.
func TestOneShotDenyAllErrorsDoNotQuoteStderr(t *testing.T) {
	denyAllEnv(t)
	bin := t.TempDir()
	init := `echo '{"type":"system","subtype":"init","tools":[],"mcp_servers":[]}'`
	script := "#!/bin/sh\nhead -n 1 >/dev/null\n" + init + "\n" +
		"if [ -f \"" + bin + "/ran\" ]; then echo 'STDERR-ECHO transcript text' >&2; exit 3; fi\n" +
		"touch \"" + bin + "/ran\"\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	_, err := OneShotDenyAll(context.Background(), usagex.FeatureMemoryReview, OneShotShort, "persona", secretPrompt)
	if err == nil || strings.Contains(err.Error(), "STDERR-ECHO") || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("err = %v", err)
	}
}
