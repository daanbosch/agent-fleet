package chatx

// The deny-all one-shot route (ADR 0117 decision 3): a helper turn that reads a third party's
// text must run with no tool, no MCP server, no file and no network access, and a backend that
// cannot be shown to do that does not run it. Fail closed, never "run with the default".
//
// Only claude qualifies today. The others have no flag that removes their tools (codex keeps
// its read-only shell, opencode's one-shot config denies edit and bash only, cursor's ask mode
// is read-only rather than tool-less, agy and muse have no such switch), and none of them has
// been verified; the table is denyAllKinds.
//
// A flag in the argv is not proof. Measured on claude 2.1.302: --strict-mcp-config with an
// empty config still leaves Claude in Chrome connected (22 browser tools) when the inherited
// environment sets CLAUDE_CODE_ENABLE_CFC=1. So the route checks what the run EFFECTIVELY has,
// twice: a probe with the very same argv and environment before the transcript is handed over,
// and the init event of the real run, which discards the reply if it ever lists a tool.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/usagex"
)

// ErrDenyAllUnavailable is returned when no backend that can run deny-all is usable, or the
// one that is could not be verified. Nothing was sent to any model.
var ErrDenyAllUnavailable = errors.New("no backend that can run without tools is available")

// denyAllKinds are the kinds with a deny-all one-shot that is verified at run time.
var denyAllKinds = []string{session.KindClaude}

// DenyAllKind reports whether kind has a verified-at-run-time deny-all one-shot.
func DenyAllKind(kind string) bool {
	for _, k := range denyAllKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// OneShotSourceUnavailable is the resolution source of a deny-all feature whose pinned backend
// cannot run without tools (or none can be reached): the review would fail closed, not fall back.
const OneShotSourceUnavailable = "unavailable"

// ResolveDenyAllCached is ResolveOneShotCached for a deny-all feature: the same precedence as
// resolveDenyAllKind, answered from the availability cache alone so GET /ai-assist/resolution
// never starts a CLI. ok is false while the cache cannot say yet, which includes "nothing
// confirmed reachable"; "unavailable" is the one definite answer: the pin names a backend that
// cannot qualify.
func ResolveDenyAllCached(feature string) (kind, source string, ok bool) {
	if pin := aiFeatureAgentPref(feature); oneShotRunnable(pin) {
		if !DenyAllKind(pin) {
			return "", OneShotSourceUnavailable, true
		}
		avail, known := headlessAvailableCached(pin)
		if !known {
			return "", "", false
		}
		if avail {
			return pin, OneShotSourcePin, true
		}
	}
	for _, k := range oneShotOrder() {
		if !DenyAllKind(k) {
			continue
		}
		if avail, known := headlessAvailableCached(k); known && avail {
			return k, OneShotSourceDefault, true
		}
	}
	return "", "", false // nothing confirmed reachable: unknown, as oneShotKindCached says
}

// WarmDenyAllKind fills the availability cache for the kinds a deny-all feature can run on, the
// counterpart of WarmOneShotKind: that one warms the shared order, which may never reach claude.
func WarmDenyAllKind() {
	for _, k := range denyAllKinds {
		warmWG.Add(1)
		go func() {
			defer warmWG.Done()
			headlessAgentAvailable(k)
		}()
	}
}

// denyAllProbeBudget bounds the probe: it starts a CLI process and talks to no model.
const denyAllProbeBudget = 45 * time.Second

// denyAllProbePrompt is a built-in slash command, answered locally by the CLI: measured on
// 2.1.302 as num_turns 0, zero tokens, zero cost, with the init event printed first. Were a
// CLI to send it to the model instead, the cost is one tiny prompt.
const denyAllProbePrompt = "/help"

// resolveDenyAllKind picks the backend for feature. A feature pin names a vendor the member
// chose: if that vendor cannot run deny-all the call fails rather than moving the transcript
// to another one. Without a usable pin the shared order is walked and kinds that cannot run
// deny-all are skipped.
func resolveDenyAllKind(feature string) (string, error) {
	if pin := aiFeatureAgentPref(feature); oneShotRunnable(pin) {
		if !DenyAllKind(pin) {
			return "", fmt.Errorf("%w: the pinned backend %q cannot run without tools", ErrDenyAllUnavailable, pin)
		}
		if headlessAgentAvailable(pin) {
			return pin, nil
		}
	}
	for _, k := range oneShotOrder() {
		if DenyAllKind(k) && headlessAgentAvailable(k) {
			return k, nil
		}
	}
	return "", ErrDenyAllUnavailable
}

// OneShotDenyAll runs one prompt on a backend that is verified to have no tool and no MCP
// server, and returns the reply. feature picks the pin as for OneShotHeadless; the ledger row is
// written from here under the caller's usagex tag. It makes ONE model call: retry policy
// belongs to the caller, which must count attempts.
//
// Errors wrap ErrDenyAllUnavailable when nothing was sent to a model.
func OneShotDenyAll(ctx context.Context, feature string, tier OneShotTier, persona, prompt string) (reply string, err error) {
	kind, err := resolveDenyAllKind(feature)
	if err != nil {
		return "", err
	}
	selected, configured := resolveOneShotModel(feature, tier, kind)
	if !configured || selected == AssistantRecommendedModel {
		selected = recommendedOneShotModel(kind, tier)
	}
	selected = strings.TrimSpace(selected)

	// Same argv, same environment, a prompt that is not the transcript.
	if err := verifyClaudeDenyAll(ctx, persona, selected); err != nil {
		return "", err
	}

	call := usagex.Call{Kind: session.KindClaude, ModelReq: selected}
	defer usagex.RecordCall(ctx, &call, time.Now())
	cmd := claudeOneShotCmd(ctx, claudeOneShotArgsFormat(persona, selected, true))
	cmd.Stdin = strings.NewReader(prompt)
	out, runErr := cmd.Output()
	ev := parseClaudeStream(out)
	r := ev.result
	call.Models, call.CostUSD = UsageModelRows(r.ModelUsage), r.TotalCostUSD
	call.FallbackTotals(r.Usage.LedgerTokens(), "")
	if !ev.initSeen {
		// Without the init event nothing says what the run had, so the reply is not trusted.
		return "", errors.New("claude deny-all run: no init event, the reply was discarded")
	}
	if why := ev.violation(); why != "" {
		return "", fmt.Errorf("claude deny-all run had %s: the reply was discarded", why)
	}
	if runErr != nil {
		// Not cliErr: stderr of a run that was given the transcript is not something to put in an
		// error that reaches logs and run records.
		return "", fmt.Errorf("claude execution failed (%s)", exitSummary(runErr))
	}
	if !ev.resultSeen || r.IsError {
		return "", errors.New("claude returned an invalid response/error")
	}
	call.OK = true
	return strings.TrimRight(r.Result, "\n"), nil
}

// verifyClaudeDenyAll runs the one-shot argv for model with a local slash command and checks the
// init event. Any doubt is a failure.
func verifyClaudeDenyAll(ctx context.Context, persona, model string) error {
	ctx, cancel := context.WithTimeout(ctx, denyAllProbeBudget)
	defer cancel()
	cmd := claudeOneShotCmd(ctx, claudeOneShotArgsFormat(persona, model, true))
	cmd.Stdin = strings.NewReader(denyAllProbePrompt)
	out, err := cmd.Output()
	ev := parseClaudeStream(out)
	if !ev.initSeen {
		if err != nil {
			return fmt.Errorf("%w: the probe did not run: %s", ErrDenyAllUnavailable, cliErr(err))
		}
		return fmt.Errorf("%w: the probe printed no init event", ErrDenyAllUnavailable)
	}
	if why := ev.violation(); why != "" {
		return fmt.Errorf("%w: claude would run with %s", ErrDenyAllUnavailable, why)
	}
	return nil
}

// claudeOneShotCmd is the claude command every one-shot starts: the chat command prefix and the
// one-shot environment.
func claudeOneShotCmd(ctx context.Context, args []string) *exec.Cmd {
	cmd := chatClaudeCmd(ctx, args...)
	cmd.Env = append(cmd.Env, claudeOneShotEnv...)
	return cmd
}

// claudeStream is what the deny-all route reads off a stream-json run.
type claudeStream struct {
	initSeen   bool
	toolsKnown bool
	mcpKnown   bool
	tools      int
	mcp        int
	resultSeen bool
	result     claudeResult
}

// violation says what the init event listed that deny-all forbids, "" when it lists nothing. A
// field the event does not carry counts as a violation: absence of evidence is not evidence.
func (s claudeStream) violation() string {
	switch {
	case !s.toolsKnown:
		return "an unreadable tool list"
	case s.tools > 0:
		return fmt.Sprintf("%d tools", s.tools)
	case !s.mcpKnown:
		return "an unreadable MCP server list"
	case s.mcp > 0:
		return fmt.Sprintf("%d MCP servers", s.mcp)
	}
	return ""
}

// parseClaudeStream reads claude's stream-json output. It keeps only counts and the result
// object: names of tools and servers are not needed and the text of other events is never kept.
func parseClaudeStream(out []byte) claudeStream {
	var s claudeStream
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var head struct {
			Type       string             `json:"type"`
			Subtype    string             `json:"subtype"`
			Tools      *[]json.RawMessage `json:"tools"`
			MCPServers *[]json.RawMessage `json:"mcp_servers"`
		}
		if json.Unmarshal(line, &head) != nil {
			continue
		}
		switch {
		case head.Type == "system" && head.Subtype == "init":
			// A second init (a resumed run prints one per session) must not clear the first's
			// verdict: counts only ever add up.
			s.initSeen = true
			if head.Tools != nil {
				s.toolsKnown = true
				s.tools += len(*head.Tools)
			}
			if head.MCPServers != nil {
				s.mcpKnown = true
				s.mcp += len(*head.MCPServers)
			}
		case head.Type == "result":
			var r claudeResult
			if json.Unmarshal(line, &r) == nil {
				s.result, s.resultSeen = r, true
			}
		}
	}
	return s
}

// exitSummary says how a process ended without quoting anything it printed.
func exitSummary(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Sprintf("exit status %d", ee.ExitCode())
	}
	return "could not run"
}
