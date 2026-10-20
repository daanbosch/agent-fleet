package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// The console chat mirror renders transcriptTurn{role, parts}. A kitty session writes no
// log, but the toolbar persists the real pi session JSONL per row (registry field
// piSessionFile), so the conversation is servable from it instead of a frozen pane.

const (
	// maxTranscriptTurns bounds the tail of the conversation the mirror resets to.
	maxTranscriptTurns = 200
	// maxTranscriptBytes refuses an implausibly large session file (the tallest real
	// sessions are a few hundred MB).
	maxTranscriptBytes = 512 << 20
	// maxTranscriptLineBytes caps one JSONL line; the system preamble is the widest
	// line a real session writes, far under this.
	maxTranscriptLineBytes = 8 << 20
	// transcriptCacheEntries bounds the cache; past it the whole cache drops — the
	// next poll re-parses, and turns per entry are already capped.
	transcriptCacheEntries = 128
)

// piMessages extracts the user/assistant text turns of a pi session JSONL. system,
// toolResult rows and thinking/toolCall blocks are skipped: the mirror renders the
// conversation, not the reasoning or the tool traffic. A truncated tail line (a live
// writer appends mid-line) loses only its own turn.
func piMessages(path string, maxTurns int) ([]transcriptTurn, error) {
	var stat os.FileInfo
	var err error
	if stat, err = os.Stat(path); err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() {
		return nil, os.ErrInvalid
	}
	if stat.Size() > int64(maxTranscriptBytes) {
		return nil, os.ErrInvalid
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	turns := make([]transcriptTurn, 0, 64)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), maxTranscriptLineBytes)
	for scanner.Scan() {
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &entry) != nil || entry.Type != "message" {
			continue // non-message entries and truncated tail lines are skipped
		}
		role := entry.Message.Role
		if role != "user" && role != "assistant" {
			continue
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(entry.Message.Content, &blocks) != nil {
			continue // string content (system) and odd shapes stay out
		}
		parts := make([]transcriptPart, 0, 2)
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, transcriptPart{Kind: "text", Text: b.Text})
			}
		}
		if len(parts) == 0 {
			continue
		}
		turns = append(turns, transcriptTurn{Role: role, Parts: parts})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(turns) > maxTurns {
		turns = turns[len(turns)-maxTurns:]
	}
	return turns, nil
}

// safeSessionFile accepts only the canonical spelling of an absolute regular file, so a
// doctored registry row cannot make the shim open a dot-dot path or a symlinked detour.
func safeSessionFile(raw string) (string, bool) {
	if raw == "" || !filepath.IsAbs(raw) || filepath.Clean(raw) != raw {
		return "", false
	}
	return raw, true
}

type transcriptCacheEntry struct {
	modTimeNsec int64
	size        int64
	turns       []transcriptTurn
}

type transcriptCache struct {
	mu      sync.Mutex
	entries map[string]transcriptCacheEntry
}

var transcript = transcriptCache{}

// piTurnsFor serves the cached conversation for a transcript path, re-parsing only when
// the file changed since the last poll (the mirror polls live sessions every few
// seconds; a tall session file must not be walked each time).
func piTurnsFor(raw string, maxTurns int) []transcriptTurn {
	path, ok := safeSessionFile(raw)
	if !ok {
		return nil
	}
	stat, err := os.Stat(path)
	if err != nil || stat.Size() > int64(maxTranscriptBytes) || !stat.Mode().IsRegular() {
		return nil
	}
	transcript.mu.Lock()
	defer transcript.mu.Unlock()
	if hit, ok := transcript.entries[path]; ok && hit.modTimeNsec == stat.ModTime().UnixNano() && hit.size == stat.Size() {
		return hit.turns
	}
	turns, err := piMessages(path, maxTurns)
	if err != nil {
		return nil // empty mirror answer, no serve failure: a missing file is not a 500
	}
	if transcript.entries == nil || len(transcript.entries) >= transcriptCacheEntries {
		transcript.entries = make(map[string]transcriptCacheEntry, 16)
	}
	transcript.entries[path] = transcriptCacheEntry{modTimeNsec: stat.ModTime().UnixNano(), size: stat.Size(), turns: turns}
	return turns
}
