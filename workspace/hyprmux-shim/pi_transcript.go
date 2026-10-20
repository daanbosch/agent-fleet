package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
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
	// maxTranscriptLineBytes caps one JSONL line. A wider line (a huge paste or result)
	// loses only its own turn; the rest of the conversation still mirrors.
	maxTranscriptLineBytes = 8 << 20
	// maxTranscriptTextBytes bounds the retained text of one cached conversation, again
	// newest-turns-first, so a doctored file cannot pin unbounded memory in the shim.
	maxTranscriptTextBytes = 8 << 20
	// transcriptCacheEntries bounds the cache; past it the whole cache drops — the
	// next poll re-parses, and turns per entry are already capped.
	transcriptCacheEntries = 128
)

// piMessages extracts the user/assistant text turns of a pi session JSONL. system and
// toolResult rows and thinking/toolCall blocks are skipped: the mirror renders the
// conversation, not the reasoning or the tool traffic. Unparseable lines — a live writer
// appends mid-line — and lines wider than maxTranscriptLineBytes lose only their own
// turn. A read error means the transcript is unusable at all.
func piMessages(path string, maxTurns, maxTextBytes int) ([]transcriptTurn, error) {
	var stat os.FileInfo
	var err error
	if stat, err = os.Stat(path); err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() > int64(maxTranscriptBytes) {
		return nil, os.ErrInvalid
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	turns := make([]transcriptTurn, 0, 64)
	textBytes := 0
	read := bufio.NewReaderSize(f, 64<<10)
	for {
		line, tooLong, rerr := readTranscriptLine(read, maxTranscriptLineBytes)
		if rerr != nil && rerr != io.EOF {
			return nil, rerr
		}
		if len(line) > 0 && !tooLong {
			var entry struct {
				Type    string `json:"type"`
				Message struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &entry) == nil && entry.Type == "message" {
				role := entry.Message.Role
				if role == "user" || role == "assistant" {
					var blocks []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					}
					if json.Unmarshal(entry.Message.Content, &blocks) == nil {
						parts := make([]transcriptPart, 0, 2)
						for _, b := range blocks {
							if b.Type == "text" && b.Text != "" {
								parts = append(parts, transcriptPart{Kind: "text", Text: b.Text})
							}
						}
						if len(parts) > 0 {
							if maxTurns > 0 && len(turns) >= maxTurns {
								// newest turns win, oldest are dropped
								drop := turns[0]
								textBytes -= drop.PartsTextBytes()
								turns = turns[1:]
							}
							textBytes += transcriptPartsBytes(parts)
							turns = append(turns, transcriptTurn{Role: role, Parts: parts})
							for textBytes > maxTextBytes && len(turns) > 1 {
								drop := turns[0]
								textBytes -= drop.PartsTextBytes()
								turns = turns[1:]
							}
						}
					}
				}
			}
		}
		if rerr == io.EOF {
			return turns, nil
		}
	}
}

// readTranscriptLine reads one \n-terminated line, tolerating lines wider than cap by
// discarding them whole and reporting tooLong instead of failing the read.
func readTranscriptLine(r *bufio.Reader, cap int) (line []byte, tooLong bool, err error) {
	var buf bytes.Buffer
	for {
		chunk, rerr := r.ReadSlice('\n')
		if len(chunk) > 0 {
			if !tooLong && buf.Len()+len(chunk) > cap {
				tooLong = true
				buf.Reset()
			}
			if !tooLong {
				buf.Write(chunk)
			}
		}
		if rerr == bufio.ErrBufferFull {
			continue
		}
		return buf.Bytes(), tooLong, rerr
	}
}

// safeSessionFile accepts only the canonical spelling of an absolute path, so a doctored
// registry row cannot make the shim open a dot-dot or relative detour. It does NOT
// resolve symlinks: within this single-user shim the registry is same-UID trusted data,
// and os.Stat/IsRegular refuse special files.
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
// seconds; a tall session file must not be walked each time). The second return value
// distinguishes a readable file with zero turns from an unusable one, so the handler can
// fall back to the headless log tail only in the honest failure case.
func piTurnsFor(raw string, maxTurns, maxTextBytes int) (turns []transcriptTurn, usable bool) {
	path, ok := safeSessionFile(raw)
	if !ok {
		return nil, false
	}
	stat, err := os.Stat(path)
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > int64(maxTranscriptBytes) {
		return nil, false
	}
	modNsec, size := stat.ModTime().UnixNano(), stat.Size()

	transcript.mu.Lock()
	if hit, ok := transcript.entries[path]; ok && hit.modTimeNsec == modNsec && hit.size == size {
		transcript.mu.Unlock()
		return hit.turns, true
	}
	transcript.mu.Unlock()

	parsed, err := piMessages(path, maxTurns, maxTextBytes) // no lock held while parsing
	if err != nil {
		return nil, false
	}
	after, err := os.Stat(path)
	if err != nil || !after.Mode().IsRegular() {
		return parsed, true // answer the conversation read; a failed re-stat skips the cache
	}
	transcript.mu.Lock()
	if transcript.entries == nil || len(transcript.entries) >= transcriptCacheEntries {
		transcript.entries = make(map[string]transcriptCacheEntry, 16)
	}
	transcript.entries[path] = transcriptCacheEntry{modTimeNsec: after.ModTime().UnixNano(), size: after.Size(), turns: parsed}
	transcript.mu.Unlock()
	return parsed, true
}

func transcriptPartsBytes(parts []transcriptPart) int {
	n := 0
	for _, p := range parts {
		n += len(p.Text)
	}
	return n
}

func (t transcriptTurn) PartsTextBytes() int {
	return transcriptPartsBytes(t.Parts)
}
