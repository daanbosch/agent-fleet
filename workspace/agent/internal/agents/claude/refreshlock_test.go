package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRefreshStuck(t *testing.T) {
	if !RefreshStuck(refreshStuckText) {
		t.Errorf("the measured refresh-lock message is not recognised: %q", refreshStuckText)
	}
	for _, msg := range []string{
		"Please run /login · API Error: 401 OAuth access token has expired. Re-authenticate to continue.",
		"You've hit your session limit",
		"",
	} {
		if RefreshStuck(msg) {
			t.Errorf("RefreshStuck(%q) = true, want false (only the lock wording resumes quickly)", msg)
		}
	}
}

// TestRefreshLockSnapshot checks what the log line carries, and that it reads metadata only: the
// credentials hold a token, which must never reach a log.
func TestRefreshLockSnapshot(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if err := os.MkdirAll(filepath.Join(dir, ".oauth_refresh.lock"), 0o755); err != nil {
		t.Fatal(err)
	}
	creds := `{"claudeAiOauth":{"accessToken":"SECRET-ACCESS","refreshToken":"SECRET-REFRESH","expiresAt":4102444800000,"refreshTokenExpiresAt":4102444800000}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
	resetCredCache()

	got := RefreshLockSnapshot(time.Now().Add(7 * time.Second))
	for _, want := range []string{"lock=7s old", "legacyLock=none", "creds=", "accessExpires=2100-01-01T00:00:00Z", "claudeProcs="} {
		if !strings.Contains(got, want) {
			t.Errorf("snapshot %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "SECRET") {
		t.Errorf("snapshot leaks credential contents: %q", got)
	}

	if err := os.Mkdir(filepath.Join(root, "claude.lock"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := RefreshLockSnapshot(time.Now()); strings.Contains(got, "legacyLock=none") {
		t.Errorf("a legacy lock next to the config dir is not reported: %q", got)
	}
}
