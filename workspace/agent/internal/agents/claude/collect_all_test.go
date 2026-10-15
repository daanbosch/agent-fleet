package claude

import (
	"strings"
	"testing"
)

// The export reads the whole file: the mirror's 1 MiB newest-first cap must not drop the
// opening request, and an ordinary tool call must carry its result.
func TestCollectAllTurnsKeepsEverythingAndToolResults(t *testing.T) {
	big := strings.Repeat("x", 600_000)
	lines := [][]byte{
		[]byte(`{"type":"user","message":{"role":"user","content":"FIRST_REQUEST"}}`),
		[]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"` + big + `"}]}}`),
		[]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"` + big + `"},{"type":"tool_use","id":"b1","name":"Bash","input":{"command":"ls"}}]}}`),
		[]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"b1","content":"OUTPUT_SENTINEL"}]}}`),
	}
	if got := CollectTurns(lines, 0, len(lines)); len(got) > 0 && strings.Contains(got[0].Text, "FIRST_REQUEST") {
		t.Fatalf("precondition: the mirror's collector was expected to cap this transcript")
	}
	all := CollectAllTurns(lines)
	if len(all) == 0 || !strings.Contains(all[0].Text, "FIRST_REQUEST") {
		t.Fatalf("first turn lost: %d turns", len(all))
	}
	var out string
	for _, tr := range all {
		for _, p := range tr.Parts {
			if p.Kind == "tool" && p.Tool == "Bash" {
				out = p.Output
			}
		}
	}
	if out != "OUTPUT_SENTINEL" {
		t.Fatalf("Bash output = %q", out)
	}
	for _, tr := range CollectTurns(lines, 2, len(lines)) {
		for _, p := range tr.Parts {
			if p.Kind == "tool" && p.Output != "" {
				t.Fatalf("the mirror's collector must keep leaving ordinary tool output out")
			}
		}
	}
}
