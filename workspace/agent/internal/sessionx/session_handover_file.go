package sessionx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/paths"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

// The handover file lives in the target's throwaway directory, ~/.af-work/<target>, never in
// the working copy, so it cannot be committed (ADR 0115 decision 9). The path is computed from
// the name, not read from $AF_WORK_DIR, because a managed target has no such variable.

const handoverPrefix, handoverSuffix = "handover-", ".md"

var handoverNameRe = regexp.MustCompile(`^handover-([A-Za-z0-9._-]+)\.md$`)

// HandoverFileName is the file that records a move out of source.
func HandoverFileName(source string) string { return handoverPrefix + source + handoverSuffix }

// WriteHandoverFile writes md as target's handover-<source>.md and returns its path. The write
// is complete and synced when it returns: a reader never sees a half file, and a crash right
// after cannot leave the target pointing at an empty one.
func WriteHandoverFile(target, source, md string) (string, error) {
	if !session.ValidName(source) {
		return "", fmt.Errorf("handover: bad source name %q", source)
	}
	dir := paths.SessionWorkDir(target)
	if dir == "" || !session.ValidName(target) {
		return "", fmt.Errorf("handover: bad target name %q", target)
	}
	dest := filepath.Join(dir, HandoverFileName(source))
	return dest, writeFileSynced(dest, []byte(md))
}

// ListHandoverFiles names the handover files in a session's own directory, sorted. Those are
// the earlier records the session itself depends on (decision 9's chain).
func ListHandoverFiles(name string) []string {
	dir := paths.SessionWorkDir(name)
	if dir == "" {
		return nil
	}
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		m := handoverNameRe.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() || !session.ValidName(m[1]) {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// CopyHandoverChain copies source's own handover files into target's directory and returns
// their names, so target owns every file it depends on: deleting an earlier session later
// takes nothing the target needs. A file named for target or source is skipped (a retried
// move's own file, never an earlier record).
func CopyHandoverChain(source, target string) ([]string, error) {
	if !session.ValidName(source) || !session.ValidName(target) {
		return nil, errors.New("handover: bad session name")
	}
	srcDir, dstDir := paths.SessionWorkDir(source), paths.SessionWorkDir(target)
	if srcDir == "" || dstDir == "" {
		return nil, errors.New("handover: bad session name")
	}
	var copied []string
	for _, n := range ListHandoverFiles(source) {
		if n == HandoverFileName(target) || n == HandoverFileName(source) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(srcDir, n))
		if err != nil {
			return copied, err
		}
		if err := writeFileSynced(filepath.Join(dstDir, n), b); err != nil {
			return copied, err
		}
		copied = append(copied, n)
	}
	return copied, nil
}

// writeFileSynced replaces dest atomically: temp file in the same directory, fsync, rename,
// fsync of the directory.
func writeFileSynced(dest string, b []byte) error {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".handover-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if _, err := f.Write(b); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
