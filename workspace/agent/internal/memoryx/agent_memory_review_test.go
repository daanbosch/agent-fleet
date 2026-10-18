package memoryx

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The review judges a string whole: one finding, a NUL byte or the scanner's cap and the string is
// not used. The scanner stops at 20 findings per text, so a text with 25 secrets would otherwise
// have its first 20 reported and the rest passed along.
func TestReviewTextProblem(t *testing.T) {
	key := agentMemFakeAWS()
	many := strings.Repeat("key "+key+"\n", memorySecretMaxPerFile+5)
	cases := []struct {
		name, text, want string
	}{
		{"clean prose", "we decided to keep the cache; see docs/decisions", ""},
		{"placeholder is not a secret", "AKIAEXAMPLEEXAMPLE00 is an example", ""},
		{"one key on one line", "token " + key + " end", "aws-access-key-id"},
		{"a key after padding", strings.Repeat("x", 9000) + " " + key, "aws-access-key-id"},
		{"nul byte", "plain\x00text", "nul-byte"},
		{"past the cap", many, "scan-cap"},
	}
	for _, tc := range cases {
		if got := ReviewTextProblem(tc.text); got != tc.want {
			t.Errorf("%s: problem = %q, want %q", tc.name, got, tc.want)
		}
	}
	// The answer is a rule name or a reason: the value never comes back.
	if p := ReviewTextProblem(key); strings.Contains(p, key[:8]) {
		t.Errorf("the problem text echoes the secret: %q", p)
	}
}

func TestReviewSnapshotReadsWhatTheSessionSeesAndCountsNoUse(t *testing.T) {
	_, _, _ = agentMemTestEnv(t)
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	claudeC := agentMemCallerT(t, "claude-main")
	for _, r := range []agentMemSaveReq{
		{Name: "build-cmd", Description: "how to build", Type: "project", Body: "run make"},
		{Name: "claude-only", Description: "only claude", Type: "project", Kinds: []string{"claude"}, Body: "b"},
		{Scope: agentMemScopeUser, Name: "style", Description: "writing style", Type: "feedback", Body: "terse"},
	} {
		if _, err := agentMemSave(claudeC, r, now); err != nil {
			t.Fatal(err)
		}
	}

	snap, err := ReviewSnapshotFor("codex-wt")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Kind != "codex" || !snap.HasProject {
		t.Fatalf("snapshot = %+v", snap)
	}
	var names []string
	for _, e := range snap.Entries {
		names = append(names, e.Scope+"/"+e.Name)
		if e.Body == "" {
			t.Errorf("%s has no body", e.Name)
		}
	}
	// feedback ranks ahead of project; the claude-only memory is not for a codex session.
	if strings.Join(names, ",") != "user/style,project/build-cmd" {
		t.Fatalf("entries = %v", names)
	}
	if line := snap.Entries[1].IndexLine(); !strings.HasPrefix(line, "- [project] build-cmd — how to build") {
		t.Errorf("index line = %q", line)
	}

	snapClaude, err := ReviewSnapshotFor("claude-main")
	if err != nil || len(snapClaude.Entries) != 3 {
		t.Fatalf("claude sees %d entries, err %v", len(snapClaude.Entries), err)
	}

	// Reading for a review is not using a memory: no usage sidecar is written for either scope.
	for _, scope := range []string{agentMemScopeProject, agentMemScopeUser} {
		rel, err := agentMemScopeDir(scope, claudeC)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := agentMemCheckDir(rel+"/"+agentMemUsageDir, false); !os.IsNotExist(err) {
			t.Errorf("%s: a usage sidecar exists after a review read (err %v)", scope, err)
		}
	}
}

func TestReviewSnapshotRefusesAnUnknownSession(t *testing.T) {
	_, _, _ = agentMemTestEnv(t)
	for _, name := range []string{"", "no-such-session", "../x"} {
		if _, err := ReviewSnapshotFor(name); err == nil {
			t.Errorf("%q: a review of a session the store cannot place must fail", name)
		}
	}
}

func TestValidateReviewMemory(t *testing.T) {
	ok := func(scope, name, desc, typ, body string) error {
		return ValidateReviewMemory(scope, name, desc, typ, nil, body)
	}
	if err := ok("project", "a-name", "one line", "feedback", "body"); err != nil {
		t.Fatalf("a valid memory: %v", err)
	}
	bad := map[string]error{
		"scope":       ok("session", "a", "d", "user", "b"),
		"name":        ok("project", "Bad Name", "d", "user", "b"),
		"description": ok("project", "a", "two\nlines", "user", "b"),
		"empty body":  ok("project", "a", "d", "user", "  "),
		"type":        ok("project", "a", "d", "bogus", "b"),
		"no type":     ok("project", "a", "d", "", "b"),
		"too large":   ok("project", "a", "d", "user", strings.Repeat("x", agentMemMaxBody+1)),
	}
	for name, err := range bad {
		if err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// A refusal names the rule, never the value.
	err := ok("project", "SECRET-"+agentMemFakeAWS(), "d", "user", "b")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("err = %v", err)
	}
}
