package sessionx

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents/claude"
)

// The lost-log refusal reaches the Console as its own code (409 transcript_lost), wrapped or
// not, so it can offer "start a new conversation" instead of a generic resume failure.
func TestWriteCodexReleasingErrMapsTranscriptLost(t *testing.T) {
	for _, err := range []error{claude.ErrTranscriptLost, fmt.Errorf("wrapped: %w", claude.ErrTranscriptLost)} {
		rec := httptest.NewRecorder()
		if !writeCodexReleasingErr(rec, err) {
			t.Fatalf("%v: not handled", err)
		}
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"transcript_lost"`) {
			t.Fatalf("%v: got %d %s", err, rec.Code, rec.Body.String())
		}
	}
}
