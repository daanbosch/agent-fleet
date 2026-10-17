package agentkinds

import (
	"slices"
	"testing"
)

// The snapshot's equality with the Agent's registry is held on the Agent side
// (TestKindCatalogSnapshot); this pins what the CP relies on when it reads it.
func TestSnapshotShape(t *testing.T) {
	all := All()
	var kinds []string
	for _, e := range all {
		kinds = append(kinds, e.Kind)
		for field, v := range map[string]string{"mcpClient": e.MCPClient, "instructions": e.Instructions} {
			if v != "yes" && v != "no" && v != "unverified" {
				t.Errorf("%s.%s = %q, want yes / no / unverified", e.Kind, field, v)
			}
		}
	}
	if !slices.IsSorted(kinds) || len(slices.Compact(slices.Clone(kinds))) != len(kinds) {
		t.Errorf("kinds = %v, want sorted and unique", kinds)
	}
	for _, k := range []string{"claude", "codex", "lcpp", "muse", "shell"} {
		if !slices.Contains(kinds, k) {
			t.Errorf("snapshot lacks %q", k)
		}
	}
	mcp := MCPClientKinds()
	if !mcp["claude"] || !mcp["muse"] || !mcp["lcpp"] || mcp["shell"] || mcp["ssm"] {
		t.Errorf("MCPClientKinds() = %v", mcp)
	}
}
