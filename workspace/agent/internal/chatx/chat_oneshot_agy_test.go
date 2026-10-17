package chatx

import (
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/usagex"
)

// agy exits 0 with nothing on stdout when the model declines a prompt (#2061); that must not be
// ledgered as a successful call.
func TestAgyOneShotReplyEmptyStdoutIsNotOK(t *testing.T) {
	for _, out := range []string{"", "  \n\n"} {
		var call usagex.Call
		if got := agyOneShotReply(&call, []byte(out)); got != "" || call.OK {
			t.Errorf("stdout %q: reply=%q OK=%v, want empty and OK=false", out, got, call.OK)
		}
	}
	var call usagex.Call
	if got := agyOneShotReply(&call, []byte(" A title \n")); got != "A title" || !call.OK {
		t.Errorf("reply=%q OK=%v, want %q and OK=true", got, call.OK, "A title")
	}
}
