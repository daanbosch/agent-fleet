package claude

// The token-refresh lock claude takes in $CLAUDE_CONFIG_DIR, and what to record when a turn dies on
// it. Every claude in a workspace shares one config dir, so when the access token (about 8h)
// runs out they all go for one lock, and a process that cannot get it within ~7.5s fails the turn
// with "Could not refresh your login because another Claude Code process is refreshing it".
//
// Measured on a production deployment: the failing turn was followed 19s later by a successful
// refresh written by the lock's holder, so the holder was alive but slow, and no process had died.
// claude does not log why the lock was contended, hence the snapshot below.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// refreshStuckStem is the stable part of claude's message for a refresh lock it could not get.
// The classifiers in abort.go and errors.go key on the same stem.
const refreshStuckStem = "refresh your login"

// RefreshStuck reports whether msg is claude giving up on the token-refresh lock. Unlike a
// revoked login, this one ends by itself as soon as the holder finishes the refresh (or its lock
// goes stale after 60s), so the resume that follows can be much quicker than a person's sign-in.
func RefreshStuck(msg string) bool {
	return strings.Contains(strings.ToLower(msg), refreshStuckStem)
}

// RefreshLockSnapshot is one line describing the state around the refresh lock at now: lock ages
// (new and legacy), the credentials file's age and expiry, and how many claude processes run.
// It reads metadata only, never the contents of the credentials.
func RefreshLockSnapshot(now time.Time) string {
	dir := ConfigDir()
	parts := []string{
		"lock=" + ageOf(filepath.Join(dir, ".oauth_refresh.lock"), now),
		"legacyLock=" + ageOf(legacyLockPath(dir), now),
		"creds=" + ageOf(credsPath(), now),
	}
	if e := CredentialExpiry(); e.Known {
		parts = append(parts, "accessExpires="+e.Access.UTC().Format(time.RFC3339))
	}
	parts = append(parts, fmt.Sprintf("claudeProcs=%d", countClaudeProcs()))
	return strings.Join(parts, " ")
}

// legacyLockPath is the second lock older claude versions take: the config dir's real path
// with ".lock" appended, a sibling of the dir rather than a file in it.
func legacyLockPath(dir string) string {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return dir + ".lock"
}

// ageOf renders how long ago path was last modified, or "none".
func ageOf(path string, now time.Time) string {
	st, err := os.Stat(path)
	if err != nil {
		return "none"
	}
	return now.Sub(st.ModTime()).Round(time.Second).String() + " old"
}

// countClaudeProcs counts processes named claude through /proc, which is local to the container
// (unlike the config dir) and costs no EFS round trip.
func countClaudeProcs() int {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return -1
	}
	n := 0
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		if c := strings.TrimSpace(string(b)); c == "claude" || c == "claude.exe" {
			n++
		}
	}
	return n
}
