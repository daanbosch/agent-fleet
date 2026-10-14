package harness

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

// bridgeRegistry is a registry with one mutating "real" tool and a non-mutating bridge whose
// Target resolves the call's arguments ("real" -> the tool, anything else -> unresolved).
func bridgeRegistry(ran *atomic.Bool) *Registry {
	real := Tool{
		Def: ToolDef{Name: "real"}, Mutates: true,
		Run: func(context.Context, *Runtime, string) (string, error) { ran.Store(true); return "real ran", nil },
	}
	bridge := Tool{
		Def: ToolDef{Name: "bridge"},
		Target: func(args string) (Tool, bool) {
			if strings.Contains(args, `"real"`) {
				return real, true
			}
			return Tool{}, false
		},
		Run: func(ctx context.Context, rt *Runtime, args string) (string, error) {
			if !strings.Contains(args, `"real"`) {
				return "", nil
			}
			return real.Run(ctx, rt, args)
		},
	}
	return NewRegistry(real, bridge)
}

func TestBridgeCallIsPlanGatedByItsTarget(t *testing.T) {
	var ran atomic.Bool
	client := &scriptedClient{turns: []Turn{
		{ToolCalls: []ToolCall{{ID: "1", Name: "bridge", Arguments: `{"name":"real"}`}}},
		{Content: "done"},
	}}
	rt := &Runtime{Cwd: t.TempDir(), Plan: true}
	res, err := Run(context.Background(), client, bridgeRegistry(&ran), rt, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ran.Load() {
		t.Fatal("plan mode did not stop a mutating tool reached through a bridge")
	}
	if got := res.Messages[1].Content; !strings.Contains(got, "real is unavailable in plan mode") {
		t.Fatalf("tool message = %q, want the plan-mode refusal naming the real tool", got)
	}
}

func TestBridgeCallIsApprovalGatedByItsTargetAndNamesIt(t *testing.T) {
	var ran atomic.Bool
	var summaries []string
	rt := &Runtime{Cwd: t.TempDir(), Approve: func(_ context.Context, call ToolCall, tool Tool, summary string) (bool, error) {
		summaries = append(summaries, call.Name+"|"+tool.Def.Name+"|"+summary)
		return false, nil
	}}
	client := &scriptedClient{turns: []Turn{
		{ToolCalls: []ToolCall{{ID: "1", Name: "bridge", Arguments: `{"name":"real"}`}}},
		{Content: "done"},
	}}
	res, err := Run(context.Background(), client, bridgeRegistry(&ran), rt, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ran.Load() {
		t.Fatal("a declined approval still ran the target")
	}
	if len(summaries) != 1 || summaries[0] != "real|real|run real" {
		t.Fatalf("approval asked = %q, want one ask naming the real tool", summaries)
	}
	if !strings.HasPrefix(res.Messages[1].Content, "declined") {
		t.Fatalf("tool message = %q, want a decline", res.Messages[1].Content)
	}
}

func TestBridgeUnresolvedTargetRunsNothingMutating(t *testing.T) {
	var ran atomic.Bool
	client := &scriptedClient{turns: []Turn{
		{ToolCalls: []ToolCall{{ID: "1", Name: "bridge", Arguments: `{"name":"nope"}`}}},
		{Content: "done"},
	}}
	rt := &Runtime{Cwd: t.TempDir()} // Approve nil: any gated call would decline
	if _, err := Run(context.Background(), client, bridgeRegistry(&ran), rt, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ran.Load() {
		t.Fatal("an unresolved bridge call ran the mutating tool")
	}
}

func TestSameNameStreakCountsBridgeTargets(t *testing.T) {
	reg := bridgeRegistry(new(atomic.Bool))
	var tr nameStreakTracker
	var last int
	for _, args := range []string{`{"name":"real"}`, `{"name":"other"}`, `{"name":"real"}`} {
		last = tr.note(judgedCalls(reg, []ToolCall{{Name: "bridge", Arguments: args}}))
	}
	if last != 1 {
		t.Fatalf("streak = %d, want 1: alternating targets are not one tool repeated", last)
	}
}
