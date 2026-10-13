package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/paths"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

// Transcript backup: a stopped session's jsonl is copied under the Agent's own data directory
// (next to the cleanup archive, which also survives a container recreate) so that claude's
// retention sweep, a lower cleanupPeriodDays or a hand delete cannot take the only copy of a
// conversation. BuildLaunch restores it when the original is gone, before it falls back to
// ErrTranscriptLost.
//
// Layout: <AgentDataDir>/claude-transcripts/<slot sid>/<project dir>/<log id>.jsonl, at most one
// file per slot. It is deliberately not under CLAUDE_CONFIG_DIR/projects, where claude's sweep
// looks, and it is always a copy (never a hard link): the data directory and CLAUDE_CONFIG_DIR
// are normally different mounts, and a link would share an inode that claude keeps appending to.
//
// Retention follows the session, not file mtime: the copy is removed when the session is
// deleted (RemoveTranscriptBackup; the cleanup archive carries the conversation from then on,
// see ArchiveBackup). Disk use is bounded by one extra copy of each stopped session's
// current log; an unchanged log is not copied again.

// backupRoot is the parent of every slot's backup directory.
func backupRoot() string { return filepath.Join(paths.AgentDataDir(), "claude-transcripts") }

// backupSlotDir is slot's backup directory. slot is a UUID derived from a session, never a
// path; anything else is refused so a crafted value cannot name a directory outside the root.
func backupSlotDir(slot string) (string, error) {
	if slot == "" || slot != filepath.Base(slot) || strings.HasPrefix(slot, ".") {
		return "", fmt.Errorf("invalid slot id %q", slot)
	}
	return filepath.Join(backupRoot(), slot), nil
}

// backupLocks serialises backup, restore, supersede and removal per slot: a removal that races
// a copy in flight would otherwise leave the copy behind for a deleted session.
var backupLocks = struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}{m: map[string]*sync.Mutex{}}

func lockBackup(slot string) func() {
	backupLocks.mu.Lock()
	l, ok := backupLocks.m[slot]
	if !ok {
		l = &sync.Mutex{}
		backupLocks.m[slot] = l
	}
	backupLocks.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// backupFile finds the backed-up log with this id: its path and the project directory it came
// from. An empty path is the ordinary "no backup"; errors are real I/O failures.
func backupFile(slotDir, id string) (path, project string, err error) {
	projects, err := os.ReadDir(slotDir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	for _, e := range projects {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(slotDir, e.Name(), id+".jsonl")
		if _, err := os.Lstat(p); err == nil {
			return p, e.Name(), nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", "", err
		}
	}
	return "", "", nil
}

// recordedLogID is the id claude last wrote the slot's conversation under, taken at face value
// (LiveSID would fall back to the slot id when the log is missing and could point the backup at
// an older log).
func recordedLogID(slot string) string {
	if live := sids.Read(slot); live != "" {
		return live
	}
	return slot
}

// errSourceChanged marks a copy abandoned because its source changed underneath it.
var errSourceChanged = errors.New("source changed while it was being copied")

// completeTranscript reports whether p is a whole transcript worth keeping: every line a
// complete JSON value (a half-written last line has no newline), and a real conversation in it.
func completeTranscript(p string) bool {
	b, err := os.ReadFile(p)
	return err == nil && CompleteBytes(b)
}

// CompleteBytes is completeTranscript for content already read, so the decision is made on
// the very bytes that get kept.
func CompleteBytes(b []byte) bool {
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return false
	}
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) > 0 && !json.Valid(line) {
			return false
		}
	}
	return bytes.Contains(b, []byte(`"type":"user"`)) || bytes.Contains(b, []byte(`"type":"assistant"`))
}

// copyPreserving copies src to a new file tmpDir/<pattern>*, with src's permission bits and
// mtime, and returns its path. The mtime is what BackupTranscript compares to decide which of
// two copies is newer, so it must survive the copy.
func copyPreserving(src, tmpDir, pattern string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return "", err
	}
	out, err := os.CreateTemp(tmpDir, pattern)
	if err != nil {
		return "", err
	}
	name := out.Name()
	fail := func(err error) (string, error) {
		_ = out.Close()
		_ = os.Remove(name)
		return "", err
	}
	if _, err := io.Copy(out, in); err != nil {
		return fail(err)
	}
	// The source may be appended to while it is copied (a resume racing the background copy).
	// Same file, same size and mtime before and after is the only evidence the copy is a
	// snapshot of one state, not a mix of two.
	if after, err := in.Stat(); err != nil || after.Size() != fi.Size() || !after.ModTime().Equal(fi.ModTime()) {
		return fail(errSourceChanged)
	}
	if at, err := os.Stat(src); err != nil || !os.SameFile(at, fi) {
		return fail(errSourceChanged)
	}
	if err := out.Chmod(fi.Mode().Perm()); err != nil {
		return fail(err)
	}
	if err := out.Sync(); err != nil {
		return fail(err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	if err := os.Chtimes(name, fi.ModTime(), fi.ModTime()); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// BackupTranscript copies the session's current log into the backup, replacing the previous
// copy only when the live log is a real conversation and newer than it. A live log that is
// missing, a stub, or older than the backup leaves the backup as it is. Nothing is copied for a
// session whose meta is gone (a delete that raced the copy).
func BackupTranscript(m session.Meta) error {
	slot := session.UUID(m.Dir, m.Name)
	slotDir, err := backupSlotDir(slot)
	if err != nil {
		return err
	}
	unlock := lockBackup(slot)
	defer unlock()
	if _, ok := session.ReadMeta(m.Name); !ok {
		return nil
	}
	id := recordedLogID(slot)
	have, haveProject, err := backupFile(slotDir, id)
	if err != nil {
		return err
	}
	var haveInfo fs.FileInfo
	if have != "" {
		if haveInfo, err = os.Stat(have); err != nil {
			return err
		}
	}
	cands, swept, err := liveCandidates(slot, id, haveProject, false)
	if err != nil {
		return err
	}
	published, project, err := backupFromCandidates(cands, slotDir, id, haveInfo)
	if err != nil {
		return err
	}
	if !published && !swept {
		// Every cheap candidate was a stub or incomplete: the conversation may have moved to a
		// project the hints do not name, so look everywhere once before giving up.
		if cands, _, err = liveCandidates(slot, id, haveProject, true); err != nil {
			return err
		}
		if published, project, err = backupFromCandidates(cands, slotDir, id, haveInfo); err != nil {
			return err
		}
	}
	if !published || project == "" {
		return nil // nothing taken (or nothing newer to take)
	}
	dstDir := filepath.Join(slotDir, project)
	// Keep one copy per slot: whatever else is in the slot directory (an older id after a drift,
	// another project directory, a leftover temp file) is superseded by the file just placed.
	entries, err := os.ReadDir(slotDir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if e.Name() != project {
			errs = append(errs, os.RemoveAll(filepath.Join(slotDir, e.Name())))
			continue
		}
		inner, err := os.ReadDir(dstDir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, f := range inner {
			if f.Name() != id+".jsonl" {
				errs = append(errs, os.RemoveAll(filepath.Join(dstDir, f.Name())))
			}
		}
	}
	return errors.Join(errs...)
}

// fullSweepEvery bounds how stale the cheap candidates may be: a hint that names an existing
// log is not proof it is the newest, so every slot gets a sweep of all project directories
// (Lstats only, no reads) this often, and at once whenever the hints find nothing usable.
const fullSweepEvery = 10 * time.Minute

var lastSweep = struct {
	mu sync.Mutex
	m  map[string]time.Time
}{m: map[string]time.Time{}}

// liveCandidates lists the logs with this id worth considering: the launch directories'
// projects and the backup's project (single Lstats), plus every project directory when sweep is
// asked for, when nothing cheap exists, or when the last sweep is old. swept says whether it did.
func liveCandidates(slot, id, haveProject string, sweep bool) ([]string, bool, error) {
	var cands []string
	seen := map[string]bool{}
	add := func(p string) error {
		if seen[p] {
			return nil
		}
		if _, err := os.Lstat(p); err == nil {
			seen[p] = true
			cands = append(cands, p)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	root := filepath.Join(ConfigDir(), "projects")
	for _, cwd := range session.CWDCandidatesForUUID(slot) {
		if err := add(filepath.Join(root, projectKey(cwd), id+".jsonl")); err != nil {
			return nil, false, err
		}
	}
	if haveProject != "" {
		if err := add(filepath.Join(root, haveProject, id+".jsonl")); err != nil {
			return nil, false, err
		}
	}
	lastSweep.mu.Lock()
	due := time.Since(lastSweep.m[slot]) >= fullSweepEvery
	lastSweep.mu.Unlock()
	if !sweep && len(cands) > 0 && !due {
		return cands, false, nil
	}
	all, err := exactLogs(id)
	if err != nil {
		return nil, false, err
	}
	lastSweep.mu.Lock()
	lastSweep.m[slot] = time.Now()
	lastSweep.mu.Unlock()
	for _, p := range all {
		if !seen[p] {
			seen[p] = true
			cands = append(cands, p)
		}
	}
	return cands, true, nil
}

// backupFromCandidates tries the candidates newest first and publishes the first whose copy is
// a complete conversation and newer than the existing backup, returning its project directory.
// published=false means only stubs or incomplete logs were found (worth looking further);
// published=true with an empty project means the question is settled with nothing to take:
// nothing newer, or the log is still being written.
func backupFromCandidates(cands []string, slotDir, id string, haveInfo fs.FileInfo) (published bool, project string, err error) {
	type cand struct {
		path string
		fi   fs.FileInfo
	}
	var cs []cand
	for _, p := range cands {
		fi, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, "", err
		}
		cs = append(cs, cand{p, fi})
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].fi.ModTime().After(cs[j].fi.ModTime()) })
	for _, c := range cs {
		// Whatever project the log is in, a backup at least as new wins: an old log left in
		// another project directory must not replace a newer conversation whose original is gone.
		if haveInfo != nil && !c.fi.ModTime().After(haveInfo.ModTime()) {
			return true, "", nil // nothing newer: settled, not a miss
		}
		proj := filepath.Base(filepath.Dir(c.path))
		if err := os.MkdirAll(filepath.Join(slotDir, proj), 0o700); err != nil {
			return false, "", err
		}
		tmp, err := copyPreserving(c.path, slotDir, ".tmp-*")
		if errors.Is(err, errSourceChanged) {
			return true, "", nil // being written to: the next attempt copies the settled log
		}
		if err != nil {
			return false, "", err
		}
		// Judged on the copy, not on the original it was taken from, and its mtime (the
		// inode actually copied) is compared again: the file at the path may have been swapped
		// for an older log between the choice and the open.
		ti, serr := os.Stat(tmp)
		if serr != nil || !completeTranscript(tmp) {
			_ = os.Remove(tmp)
			if serr != nil {
				return false, "", serr
			}
			continue
		}
		if haveInfo != nil && !ti.ModTime().After(haveInfo.ModTime()) {
			_ = os.Remove(tmp)
			return true, "", nil
		}
		if err := os.Rename(tmp, filepath.Join(slotDir, proj, id+".jsonl")); err != nil {
			_ = os.Remove(tmp)
			return false, "", err
		}
		return true, proj, nil
	}
	return false, "", nil
}

// backupRetry spaces the background attempts per slot: the session list polls every few
// seconds, and a stopped session's log does not change.
const backupRetry = time.Minute

var lastBackupTry = struct {
	mu       sync.Mutex
	m        map[string]time.Time
	inflight map[string]bool
}{m: map[string]time.Time{}, inflight: map[string]bool{}}

// backupStopped runs BackupTranscript off the caller's goroutine (the list poll must not wait
// for a large copy), at most once per backupRetry per session.
func backupStopped(m session.Meta) {
	slot := session.UUID(m.Dir, m.Name)
	lastBackupTry.mu.Lock()
	if time.Since(lastBackupTry.m[slot]) < backupRetry {
		lastBackupTry.mu.Unlock()
		return
	}
	if lastBackupTry.inflight[slot] {
		lastBackupTry.mu.Unlock()
		return
	}
	lastBackupTry.m[slot] = time.Now()
	lastBackupTry.inflight[slot] = true
	lastBackupTry.mu.Unlock()
	go func() {
		defer func() {
			lastBackupTry.mu.Lock()
			delete(lastBackupTry.inflight, slot)
			lastBackupTry.mu.Unlock()
		}()
		if err := BackupTranscript(m); err != nil {
			log.Printf("claude transcript backup of %s failed: %v", m.Name, err)
		}
	}()
}

// logDest is where claude looks for the log with this id for a session launched in cwd. A
// resume only searches the launch directory's project, so that wins over the project the
// backup came from; the recorded project is used when the cwd is unknown.
func logDest(cwd, recordedProject, id string) string {
	project := recordedProject
	if cwd != "" {
		project = projectKey(cwd)
	}
	return filepath.Join(ConfigDir(), "projects", project, id+".jsonl")
}

// RestoreTranscript puts the backed-up log back when the slot's conversation is known but no
// resumable log exists any more. It reports whether it placed one. A log that is on disk —
// however old the backup is relative to it — is never replaced, and a stub in the way is set
// aside (kept, never overwritten) rather than removed. Errors are returned, not read as "no
// backup": an unreadable projects tree or backup directory must not look like a lost log.
func RestoreTranscript(slot, cwd string) (bool, error) {
	slotDir, err := backupSlotDir(slot)
	if err != nil {
		return false, err
	}
	unlock := lockBackup(slot)
	defer unlock()
	id, ok := talkedID(slot)
	if !ok {
		return false, nil
	}
	logs, err := exactLogs(id)
	if err != nil {
		return false, err
	}
	if resumablePaths(logs) {
		return false, nil
	}
	have, project, err := backupFile(slotDir, id)
	if err != nil || have == "" {
		return false, err
	}
	if !resumablePaths([]string{have}) {
		return false, nil
	}
	dest := logDest(cwd, project, id)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return false, err
	}
	// Stage first, decide last: the copy is ready before anything at dest is touched, and the
	// logs are looked at again right before the hand-over, so a conversation another writer
	// created since the first look (in any project) aborts the restore instead of being
	// set aside as if it were a stub.
	tmp, err := copyPreserving(have, filepath.Dir(dest), ".restore-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp)
	if logs, err = exactLogs(id); err != nil {
		return false, err
	}
	if resumablePaths(logs) {
		return false, nil
	}
	if err := setAsideStub(dest); err != nil {
		return false, err
	}
	created, err := placeStaged(tmp, dest)
	if err != nil {
		return false, err
	}
	if created {
		log.Printf("claude transcript %s restored from the Agent's backup", id)
	}
	return created, nil
}

// setAsideStub moves a non-conversation file (a bridge-session stub) out of dest's way, keeping
// it as <dest>.superseded.<ns>. The file is opened and judged first and the move is checked
// against that same inode; a file that changed or is a real conversation is moved back (when
// dest is still free) and the restore is abandoned, because something else is writing there.
func setAsideStub(dest string) error {
	f, err := os.Open(dest)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if resumablePaths([]string{dest}) {
		return fmt.Errorf("%s holds a conversation now; not restoring over it", dest)
	}
	if at, err := os.Lstat(dest); err != nil || !os.SameFile(at, fi) {
		return fmt.Errorf("%s changed while it was being checked", dest)
	}
	moved, err := setAsideTo(dest)
	if err != nil {
		return err
	}
	if at, err := os.Lstat(moved); err != nil || !os.SameFile(at, fi) || resumablePaths([]string{moved}) {
		_ = os.Link(moved, dest) // best effort, and never over something that is there now
		return fmt.Errorf("%s changed while it was being set aside", dest)
	}
	return nil
}

// setAsideTo is setAside reporting the name it chose.
func setAsideTo(p string) (string, error) {
	for i := 0; i < 5; i++ {
		dst := fmt.Sprintf("%s.superseded.%d", p, time.Now().UnixNano())
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		return dst, os.Rename(p, dst)
	}
	return "", fmt.Errorf("no free name to set aside %s", p)
}

// placeStaged creates dest from the staged file tmp unless something is already there, and
// never replaces anything. It is linked in (atomic, so claude never sees half a log); where the
// filesystem has no hard links it is copied to dest with O_EXCL instead, and removed again if
// that fails part way.
func placeStaged(tmp, dest string) (created bool, err error) {
	err = os.Link(tmp, dest)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	fi, serr := os.Stat(tmp)
	if serr != nil {
		return false, serr
	}
	in, oerr := os.Open(tmp)
	if oerr != nil {
		return false, oerr
	}
	defer in.Close()
	out, oerr := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if errors.Is(oerr, fs.ErrExist) {
		return false, nil
	}
	if oerr != nil {
		return false, oerr
	}
	if _, cerr := io.Copy(out, in); cerr != nil {
		_ = out.Close()
		_ = os.Remove(dest)
		return false, cerr
	}
	if cerr := out.Close(); cerr != nil {
		_ = os.Remove(dest)
		return false, cerr
	}
	_ = os.Chmod(dest, fi.Mode().Perm())
	_ = os.Chtimes(dest, fi.ModTime(), fi.ModTime())
	return true, nil
}

// ArchiveBackup returns the backed-up log of a session, read under the slot lock, for the
// cleanup archive: its bytes (a snapshot, so later backups cannot pull it out from under the
// caller), its mtime, and the path a restore of the archive should write it to. ok=false when
// there is no backup. Deleting the session removes the backup, so the archive must hold it.
func ArchiveBackup(m session.Meta) (data []byte, mtime time.Time, dest string, ok bool, err error) {
	slot := session.UUID(m.Dir, m.Name)
	slotDir, err := backupSlotDir(slot)
	if err != nil {
		return nil, time.Time{}, "", false, err
	}
	unlock := lockBackup(slot)
	defer unlock()
	id := recordedLogID(slot)
	have, project, err := backupFile(slotDir, id)
	if err != nil || have == "" {
		return nil, time.Time{}, "", false, err
	}
	f, err := os.Open(have)
	if err != nil {
		return nil, time.Time{}, "", false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, time.Time{}, "", false, err
	}
	data, err = io.ReadAll(f)
	if err != nil {
		return nil, time.Time{}, "", false, err
	}
	return data, fi.ModTime(), logDest(m.CWD(), project, id), true, nil
}

// supersedeBackup is StartFresh's part for the backup: the user chose to leave the old
// conversation behind, so the backup must not bring it back. Like the live logs it is kept,
// not deleted, under <slot>.superseded.<unixnano> (removed with the session), because when the
// original log is already gone the backup is the only copy.
//
// The caller holds the slot's backup lock (StartFresh does, across the whole fresh start).
func supersedeBackup(slot string) error {
	slotDir, err := backupSlotDir(slot)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(slotDir); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return setAside(slotDir)
}

// RemoveTranscriptBackup deletes everything the backup holds for slot, superseded copies
// included. Called when the session is deleted.
func RemoveTranscriptBackup(slot string) error {
	slotDir, err := backupSlotDir(slot)
	if err != nil {
		return err
	}
	unlock := lockBackup(slot)
	defer unlock()
	errs := []error{os.RemoveAll(slotDir)}
	old, err := filepath.Glob(slotDir + ".superseded.*")
	errs = append(errs, err)
	for _, p := range old {
		errs = append(errs, os.RemoveAll(p))
	}
	return errors.Join(errs...)
}
