package memoryx

// The git plumbing under the member's purge of one memory's history (agent_memory_purge.go):
// a persistent `cat-file --batch`, tree surgery in Go, and hash-object to write the results.
// Nothing here knows what a memory is; it removes one path from commits and re-links them.

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// purgeCatFile is one long-lived `git cat-file --batch`. Thousands of commits and trees are read
// through it; a process per object would take minutes.
type purgeCatFile struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

func startPurgeCatFile() (*purgeCatFile, error) {
	cmd := memoryGit("cat-file", "--batch")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &purgeCatFile{cmd: cmd, in: in, out: bufio.NewReaderSize(stdout, 256<<10)}, nil
}

func (c *purgeCatFile) close() {
	_ = c.in.Close()
	_ = c.cmd.Wait()
}

// get returns the type and raw content of one object.
func (c *purgeCatFile) get(sha string) (string, []byte, error) {
	if _, err := io.WriteString(c.in, sha+"\n"); err != nil {
		return "", nil, err
	}
	header, err := c.out.ReadString('\n')
	if err != nil {
		return "", nil, err
	}
	f := strings.Fields(header)
	if len(f) != 3 {
		return "", nil, fmt.Errorf("object %s is missing", sha)
	}
	size, err := strconv.Atoi(f[2])
	if err != nil {
		return "", nil, err
	}
	body := make([]byte, size+1)
	if _, err := io.ReadFull(c.out, body); err != nil {
		return "", nil, err
	}
	return f[1], body[:size], nil
}

// purgeTreeEntry is one line of a tree object.
type purgeTreeEntry struct {
	mode string
	name string
	sha  []byte // raw, rawLen bytes
}

func parsePurgeTree(data []byte, rawLen int) ([]purgeTreeEntry, error) {
	var out []purgeTreeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			return nil, fmt.Errorf("malformed tree")
		}
		nul := bytes.IndexByte(data[sp:], 0)
		if nul < 0 || len(data) < sp+nul+1+rawLen {
			return nil, fmt.Errorf("malformed tree")
		}
		nul += sp
		out = append(out, purgeTreeEntry{
			mode: string(data[:sp]), name: string(data[sp+1 : nul]),
			sha: append([]byte(nil), data[nul+1:nul+1+rawLen]...),
		})
		data = data[nul+1+rawLen:]
	}
	return out, nil
}

func renderPurgeTree(es []purgeTreeEntry) []byte {
	var b bytes.Buffer
	for _, e := range es {
		b.WriteString(e.mode + " " + e.name)
		b.WriteByte(0)
		b.Write(e.sha)
	}
	return b.Bytes()
}

// purgeWriter removes a path from trees and writes the new objects. Trees are cached by id, so a
// subtree shared by many commits is rewritten once.
type purgeWriter struct {
	cat    *purgeCatFile
	rawLen int
	cache  map[string]string
}

func newPurgeWriter(cat *purgeCatFile) (*purgeWriter, error) {
	f, err := memoryGitRun("rev-parse", "--show-object-format")
	if err != nil {
		return nil, err
	}
	w := &purgeWriter{cat: cat, cache: map[string]string{}, rawLen: 20}
	if f == "sha256" {
		w.rawLen = 32
	}
	return w, nil
}

// hashObject writes one object and returns its id.
func (w *purgeWriter) hashObject(typ string, data []byte) (string, error) {
	cmd := memoryGit("hash-object", "-w", "-t", typ, "--stdin")
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("write %s object: %w", typ, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// removePath returns the tree without the file at comps; "" when nothing is left of it. A
// directory emptied by the removal disappears too, as git would not store it.
func (w *purgeWriter) removePath(tree string, comps []string) (string, error) {
	key := tree + "\x00" + strings.Join(comps, "/")
	if v, ok := w.cache[key]; ok {
		return v, nil
	}
	typ, data, err := w.cat.get(tree)
	if err != nil {
		return "", err
	}
	if typ != "tree" {
		return "", fmt.Errorf("object %s is not a tree", tree)
	}
	es, err := parsePurgeTree(data, w.rawLen)
	if err != nil {
		return "", err
	}
	out := es[:0:0]
	changed := false
	for _, e := range es {
		if e.name != comps[0] {
			out = append(out, e)
			continue
		}
		if len(comps) == 1 {
			changed = true
			continue
		}
		if e.mode != "40000" {
			out = append(out, e)
			continue
		}
		sub, err := w.removePath(hex.EncodeToString(e.sha), comps[1:])
		if err != nil {
			return "", err
		}
		if sub == hex.EncodeToString(e.sha) {
			out = append(out, e)
			continue
		}
		changed = true
		if sub == "" {
			continue
		}
		raw, err := hex.DecodeString(sub)
		if err != nil || len(raw) != w.rawLen {
			return "", fmt.Errorf("bad tree id")
		}
		e.sha = raw
		out = append(out, e)
	}
	res := tree
	switch {
	case !changed:
	case len(out) == 0:
		res = ""
	default:
		if res, err = w.hashObject("tree", renderPurgeTree(out)); err != nil {
			return "", err
		}
	}
	w.cache[key] = res
	return res, nil
}
