package mcpx

// opencode 2 delivers the calling session as params._meta["ai.opencode/sessionID"] instead of
// the stamped argument (its client drops the stamp when the tool's schema is closed). These pin
// that the same slot → session resolution accepts it, and where it differs in trust.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/mcpreg"
)

// metaCallHandoffSession makes one request_browser_action call carrying args and meta, with two
// live Managed opencode sessions in the folder, and returns the session the handoff was filed
// under ("" when the call could not tell its caller apart).
func metaCallHandoffSession(t *testing.T, args, meta map[string]any) string {
	t.Helper()
	t.Setenv("AF_SESSIONS_DIR", t.TempDir())
	t.Chdir(t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	withMCPFlags(t, false, true, true)
	oldSource := mcpSourceSession
	t.Cleanup(func() { mcpSourceSession = oldSource })
	mcpSourceSession = ""
	writeOpencodeSlot(t, "ocfirst", cwd, "ses_first")
	writeOpencodeSlot(t, "ocsecond", cwd, "ses_second")

	var filedFor string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			_, _ = fmt.Fprintf(w, `{"alive":true,"ready":true,"name":%q}`, path.Base(path.Dir(r.URL.Path)))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		filedFor, _ = body["sessionName"].(string)
		_, _ = w.Write([]byte(`{"id":"ba_1","state":"attached","openUrl":"/open/browser-attachment/ba_1","viewer":false,"controlMode":"user-control","handoff":{"message":"m","completionLabel":"c","allowCancel":false,"controlMode":"user-control","result":"pending"}}`))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	t.Setenv("AGENT_ADDR", u.Host)

	args["attachment_id"], args["message"] = "ba_1", "m"
	params, err := json.Marshal(map[string]any{"name": "request_browser_action", "arguments": args, "_meta": meta})
	if err != nil {
		t.Fatal(err)
	}
	mcpStdioCall(mcpReq{ID: json.RawMessage(`1`), Params: params})
	if mcpCallerSID != "" || mcpCallerFromMeta {
		t.Fatalf("caller state leaked past the call: sid=%q fromMeta=%v", mcpCallerSID, mcpCallerFromMeta)
	}
	return filedFor
}

func TestMCPStdioCallResolvesCallerFromMeta(t *testing.T) {
	metaSecond := map[string]any{mcpCallerSIDMetaKey: "ses_second", "progressToken": 2}

	t.Run("meta alone", func(t *testing.T) {
		callerTrustEnv(t)
		if got := metaCallHandoffSession(t, map[string]any{}, metaSecond); got != "ocsecond" {
			t.Fatalf("handoff filed for %q, want ocsecond from _meta", got)
		}
	})

	t.Run("no plugin needed", func(t *testing.T) {
		callerTrustEnv(t)
		home := os.Getenv("HOME")
		if err := os.Remove(filepath.Join(home, ".config", "opencode", "plugin", "agent-fleet-caller.js")); err != nil {
			t.Fatal(err)
		}
		if got := metaCallHandoffSession(t, map[string]any{}, metaSecond); got != "ocsecond" {
			t.Fatalf("handoff filed for %q, want ocsecond: _meta is opencode's own, no plugin vouches for it", got)
		}
	})

	t.Run("stamp wins", func(t *testing.T) {
		callerTrustEnv(t)
		got := metaCallHandoffSession(t, map[string]any{mcpCallerSIDArg: "ses_first"}, metaSecond)
		if got != "ocfirst" {
			t.Fatalf("handoff filed for %q, want ocfirst: the stamped argument keeps priority on opencode 1", got)
		}
	})

	t.Run("unrotated server name", func(t *testing.T) {
		callerTrustEnv(t)
		t.Setenv(mcpreg.AFServerKeyEnv, "")
		if got := metaCallHandoffSession(t, map[string]any{}, metaSecond); got != "" {
			t.Fatalf("handoff filed for %q, want the cwd fallback's ambiguity refusal", got)
		}
	})

	for name, meta := range map[string]map[string]any{
		"unknown id":     {mcpCallerSIDMetaKey: "ses_forged"},
		"non-string id":  {mcpCallerSIDMetaKey: 7},
		"other meta key": {"io.modelcontextprotocol/clientInfo": "ses_second"},
	} {
		t.Run(name, func(t *testing.T) {
			callerTrustEnv(t)
			if got := metaCallHandoffSession(t, map[string]any{}, meta); got != "" {
				t.Fatalf("handoff filed for %q, want the cwd fallback's ambiguity refusal", got)
			}
		})
	}
}
