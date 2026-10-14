package opencode

import (
	"os"
	"testing"
)

// skipUnlessV2Required skips a test whose prerequisite (the opencode 2 binary, tmux, node) is
// missing, except when AF_REQUIRE_OPENCODE2=1: the opencode 2 job in opencode-contract.yml sets
// it so a missing prerequisite fails the job instead of reading as a green run of nothing.
func skipUnlessV2Required(t *testing.T, reason string) {
	t.Helper()
	if os.Getenv("AF_REQUIRE_OPENCODE2") == "1" {
		t.Fatalf("%s, and AF_REQUIRE_OPENCODE2=1 forbids skipping", reason)
	}
	t.Skip(reason)
}
