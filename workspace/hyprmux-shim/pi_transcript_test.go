package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// piFixtureLines is a small pi session JSONL: system, user, thinking-bearing
// assistant, toolResult, two-text-part assistant, plus non-message entries.
var piFixtureLines = []string{
	`{"type":"session","version":3,"id":"k-pi-0005","timestamp":"2026-10-05T09:00:00Z","cwd":"/work/epsilon"}`,
	`{"type":"message","timestamp":"2026-10-05T09:00:01Z","message":{"role":"system","content":""}}`,
	`{"type":"message","timestamp":"2026-10-05T09:00:02Z","message":{"role":"user","content":[{"type":"text","text":"update the fleet"}]}}`,
	`{"type":"message","timestamp":"2026-10-05T09:00:03Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"SECRET-THINKING inner reasoning"},{"type":"text","text":"merging now"}]}}`,
	`{"type":"message","timestamp":"2026-10-05T09:00:04Z","message":{"role":"toolResult","content":[{"type":"text","text":"tool output"}]}}`,
	`{"type":"message","timestamp":"2026-10-05T09:00:05Z","message":{"role":"assistant","content":[{"type":"toolCall","name":"bash","arguments":{"command":"ls"}},{"type":"text","text":"done part one"},{"type":"text","text":"second part"}]}}`,
	`{"type":"custom","customType":"rpiv-todo-state","data":{}}`,
}

func writePiFixture(t *testing.T, dir, name string, lines []string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// piTestServer builds a test server over a registry with three rows:
// k-pi-0005 kitty with piSessionFile, k-pi-0006 kitty without, h-pi-0007
// headless with both a piSessionFile and a seeded log.
func piTestServer(t *testing.T, jsonl string) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	reg := filepath.Join(dir, "sessions-v4.json")
	regJSON := fmt.Sprintf(`{"sessions":[
		{"id":"k-pi-0005","agent":"pi","agentVersion":"1.3.0","mode":"normal","terminal":"kitty","title":"epsilon","workdir":"/work/epsilon","status":"running","pid":1,"createdAt":"2026-10-05T09:00:00Z","piSessionFile":%q},
		{"id":"k-pi-0006","agent":"pi","agentVersion":"1.3.0","mode":"normal","terminal":"kitty","title":"zeta","workdir":"/work/zeta","status":"running","pid":1,"createdAt":"2026-10-05T09:00:00Z"},
		{"id":"h-pi-0007","agent":"pi","agentVersion":"1.3.0","mode":"normal","terminal":"headless","title":"eta","workdir":"/work/eta","status":"running","pid":1,"createdAt":"2026-10-07T09:00:00Z","piSessionFile":%q}
	]}`, jsonl, jsonl)
	if err := os.WriteFile(reg, []byte(regJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config{Token: testToken, RegistryPath: reg, LogsDir: dir}
	srv := httptest.NewServer(newHandler(cfg))
	t.Cleanup(srv.Close)
	return srv, dir
}

type mirrorWire struct {
	Name     string           `json:"name"`
	Messages []transcriptTurn `json:"messages"`
	Reset    bool             `json:"reset"`
}

func partsText(parts []transcriptPart) string {
	out := ""
	for _, p := range parts {
		out += p.Text
	}
	return out
}

func TestKittyMessagesServePiTranscript(t *testing.T) {
	dir := t.TempDir()
	transcript := writePiFixture(t, dir, "pi.jsonl", piFixtureLines)
	srv, _ := piTestServer(t, transcript)
	code, body, _ := request(t, srv, "/sessions/k-pi-0005/messages", true)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", code, body)
	}
	var got mirrorWire
	decodeJSON(t, body, &got)
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (user + 2 assistant); body %s", len(got.Messages), body)
	}
	if got.Messages[0].Role != "user" || partsText(got.Messages[0].Parts) != "update the fleet" {
		t.Errorf("turn 0 = %+v, want user 'update the fleet'", got.Messages[0])
	}
	if got.Messages[1].Role != "assistant" || partsText(got.Messages[1].Parts) != "merging now" {
		t.Errorf("turn 1 = %+v, want assistant 'merging now' (thinking must not leak)", got.Messages[1])
	}
	if got.Messages[2].Role != "assistant" || len(got.Messages[2].Parts) != 2 || got.Messages[2].Parts[0].Text != "done part one" {
		t.Errorf("turn 2 = %+v, want assistant with 2 text parts", got.Messages[2])
	}
	for _, m := range got.Messages {
		for _, p := range m.Parts {
			if strings.Contains(p.Text, "SECRET-THINKING") || strings.Contains(p.Text, "tool output") {
				t.Errorf("part leaks a non-text block: %q", p.Text)
			}
		}
	}
	if !got.Reset || got.Name != "k-pi-0005" {
		t.Errorf("wire = %+v, want reset mirror answer for k-pi-0005", got)
	}
}

func TestKittyMessagesWithoutSessionFileStayEmpty(t *testing.T) {
	dir := t.TempDir()
	transcript := writePiFixture(t, dir, "pi.jsonl", piFixtureLines)
	srv, _ := piTestServer(t, transcript)
	code, body, _ := request(t, srv, "/sessions/k-pi-0006/messages", true)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	var got mirrorWire
	decodeJSON(t, body, &got)
	if len(got.Messages) != 0 {
		t.Fatalf("messages = %d, want 0 for a kitty row without piSessionFile", len(got.Messages))
	}
}

func TestKittyMessagesToleratesTruncatedTailLine(t *testing.T) {
	dir := t.TempDir()
	whole := strings.Join(piFixtureLines, "\n")
	p := filepath.Join(dir, "pi-partial.jsonl")
	if err := os.WriteFile(p, []byte(whole[:len(whole)-20]), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, _ := piTestServer(t, p)
	code, body, _ := request(t, srv, "/sessions/k-pi-0005/messages", true)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 despite a truncated tail line; body %s", code, body)
	}
	var got mirrorWire
	decodeJSON(t, body, &got)
	if len(got.Messages) == 0 {
		t.Fatal("messages empty: a truncated tail line must lose only its own turn, not the history")
	}
}

func TestKittyMessagesRefusesRelativeSessionFile(t *testing.T) {
	srv, _ := piTestServer(t, "pi-sessions/pi.jsonl") // relative, must be refused
	code, body, _ := request(t, srv, "/sessions/k-pi-0005/messages", true)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with an empty transcript, not an error; body %s", code, body)
	}
	var got mirrorWire
	decodeJSON(t, body, &got)
	if len(got.Messages) != 0 {
		t.Fatalf("messages = %d, want 0: a relative piSessionFile must be refused", len(got.Messages))
	}
}

func TestKittyMessagesCacheFollowsAppends(t *testing.T) {
	dir := t.TempDir()
	p := writePiFixture(t, dir, "pi.jsonl", piFixtureLines)
	srv, _ := piTestServer(t, p)
	_, body1, _ := request(t, srv, "/sessions/k-pi-0005/messages", true)
	var first mirrorWire
	decodeJSON(t, body1, &first)
	if len(first.Messages) != 3 {
		t.Fatalf("first read = %d turns, want 3", len(first.Messages))
	}
	if err := os.WriteFile(p, []byte(strings.Join(append(piFixtureLines,
		`{"type":"message","timestamp":"2026-10-05T09:00:06Z","message":{"role":"user","content":[{"type":"text","text":"and then it was morning"}]}}`,
	), "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, body2, _ := request(t, srv, "/sessions/k-pi-0005/messages", true)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	var second mirrorWire
	decodeJSON(t, body2, &second)
	if len(second.Messages) != 4 || partsText(second.Messages[3].Parts) != "and then it was morning" {
		t.Fatalf("cached answer ignored a fresh writer append: %d turns", len(second.Messages))
	}
}

func TestHeadlessMessagesPrefersPiTranscriptOverLogTail(t *testing.T) {
	dir := t.TempDir()
	transcript := writePiFixture(t, dir, "pi.jsonl", piFixtureLines)
	srv, logsDir := piTestServer(t, transcript)
	// h-pi-0007 is headless AND has a seeded raw log; the real conversation must win.
	if err := os.WriteFile(filepath.Join(logsDir, "h-pi-0007.log"), []byte("line1\nline2\nline3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, body, _ := request(t, srv, "/sessions/h-pi-0007/messages", true)
	if code != http.StatusOK {
		t.Fatalf("status = %d; body %s", code, body)
	}
	var got mirrorWire
	decodeJSON(t, body, &got)
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 real turns, not the fenced log tail; body %s", len(got.Messages), body)
	}
	if got.Name != "h-pi-0007" || got.Messages[0].Role != "user" {
		t.Errorf("wire = %+v", got)
	}
}

func piSystemOnlyFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return writePiFixture(t, dir, "system-only.jsonl", piFixtureLines[:2]) // session header + system row, no talk
}

func TestHeadlessUnusableFileKeepsLogTailButEmptyFileStaysEmpty(t *testing.T) {
	// A readable-but-conversation-less session file must answer an honest empty
	// mirror; only an unusable (unreadable) file falls back to the log tail.
	emptyTalk := piSystemOnlyFixture(t)
	srv, logsDir := piTestServer(t, emptyTalk)
	if err := os.WriteFile(filepath.Join(logsDir, "h-pi-0007.log"), []byte("line1\nline2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, body, _ := request(t, srv, "/sessions/h-pi-0007/messages", true)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	var got mirrorWire
	decodeJSON(t, body, &got)
	if len(got.Messages) != 0 {
		t.Fatalf("messages = %d, want 0: a parseable empty conversation beats the log tail", len(got.Messages))
	}
	// Now make the file unusable (unlink) and confirm the log-tail fallback returns.
	if err := os.Remove(emptyTalk); err != nil {
		t.Fatal(err)
	}
	code, body, _ = request(t, srv, "/sessions/h-pi-0007/messages", true)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	var fellBack mirrorWire
	decodeJSON(t, body, &fellBack)
	if len(fellBack.Messages) == 0 {
		t.Fatal("unusable session file must fall back to the headless log tail, got none")
	}
}

func TestFatLineLosesOnlyItsOwnTurn(t *testing.T) {
	dir := t.TempDir()
	wide := strings.Repeat("x", maxTranscriptLineBytes+4096)
	fat := `{"type":"message","timestamp":"2026-10-05T09:00:06Z","message":{"role":"user","content":[{"type":"text","text":"` + wide + `"}]}}`
	lines := []string{piFixtureLines[0], fat}
	lines = append(lines, piFixtureLines[1:]...)
	// and add a real turn after the fat one.
	lines = append(lines,
		`{"type":"message","timestamp":"2026-10-05T09:00:07Z","message":{"role":"assistant","content":[{"type":"text","text":"after the fat line"}]}}`,
	)
	p := writePiFixture(t, dir, "pi.jsonl", lines)
	srv, _ := piTestServer(t, p)
	code, body, _ := request(t, srv, "/sessions/k-pi-0005/messages", true)
	if code != http.StatusOK {
		t.Fatalf("status = %d; body %s", code, body)
	}
	var got mirrorWire
	decodeJSON(t, body, &got)
	// The fat turn drops; the user turn before it and the assistant turn after survive.
	var texts []string
	for _, m := range got.Messages {
		texts = append(texts, partsText(m.Parts))
	}
	if len(got.Messages) != 4 {
		t.Fatalf("turns = %d (%v), want 4: fat line loses only its own turn", len(got.Messages), texts)
	}
	if texts[3] != "after the fat line" {
		t.Errorf("last turn = %q, want 'after the fat line'", texts[3])
	}
}

func TestTranscriptTextBudgetDropsOldest(t *testing.T) {
	lines := []string{
		`{"type":"session","version":3,"id":"budget","timestamp":"2026-10-05T09:00:00Z"}`,
	}
	big := strings.Repeat("y", 4<<20) // 4 MiB per turn: 5 turns overshoot the 8 MiB budget
	for i := 0; i < 5; i++ {
		lines = append(lines, fmt.Sprintf(`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"%d%[2]s"}]}}`, i, big))
	}
	dir := t.TempDir()
	p := writePiFixture(t, dir, "pi.jsonl", lines)
	srv, _ := piTestServer(t, p)
	_, body, _ := request(t, srv, "/sessions/k-pi-0005/messages", true)
	var got mirrorWire
	decodeJSON(t, body, &got)
	total := 0
	for _, m := range got.Messages {
		total += len(partsText(m.Parts))
	}
	if total > maxTranscriptTextBytes+(1<<20) {
		t.Fatalf("retained text = %d bytes, want <= maxTranscriptTextBytes + one newest turn", total)
	}
	if len(got.Messages) == 5 {
		t.Fatal("the oldest over-budget turns must drop, all 5 were kept")
	}
	first := partsText(got.Messages[0].Parts)
	if !strings.HasPrefix(first, "3") && !strings.HasPrefix(first, "4") {
		t.Errorf("oldest kept turn = %.20q, want the newest budget window", first)
	}
}

func TestHeadlessWithoutSessionFileKeepsLogTail(t *testing.T) {
	// Regression pin: the pre-lane log-tail behaviour must survive for headless
	// rows that carry no piSessionFile. newTestServer's fixture registry has
	// h-pi-0003 headless with a seeded log.
	srv := newTestServer(t, fixtureRegistry)
	code, body, _ := request(t, srv, "/sessions/h-pi-0003/messages", true)
	if code != http.StatusOK {
		t.Fatalf("status = %d; body %s", code, body)
	}
	var got mirrorWire
	decodeJSON(t, body, &got)
	if len(got.Messages) == 0 {
		t.Fatal("headless log-tail answer disappeared")
	}
	for _, m := range got.Messages {
		if m.Role != "assistant" {
			t.Errorf("log-tail answer must be assistant turns, got %q", m.Role)
		}
	}
}
