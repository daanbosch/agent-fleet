package claude

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
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
	logs, err := exactLogs(id)
	if err != nil {
		return err
	}
	var live string
	var liveInfo fs.FileInfo
	for _, p := range logs {
		if !resumablePaths([]string{p}) {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		if live == "" || fi.ModTime().After(liveInfo.ModTime()) {
			live, liveInfo = p, fi
		}
	}
	if live == "" {
		return nil
	}
	project := filepath.Base(filepath.Dir(live))
	have, haveProject, err := backupFile(slotDir, id)
	if err != nil {
		return err
	}
	if have != "" && haveProject == project {
		bi, err := os.Stat(have)
		if err == nil && !liveInfo.ModTime().After(bi.ModTime()) {
			return nil // unchanged, or the backup is the newer of the two
		}
	}
	dstDir := filepath.Join(slotDir, project)
	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		return err
	}
	tmp, err := copyPreserving(live, slotDir, ".tmp-*")
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dstDir, id+".jsonl")); err != nil {
		_ = os.Remove(tmp)
		return err
	}
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

// backupRetry spaces the background attempts per slot: the session list polls every few
// seconds, and a stopped session's log does not change.
const backupRetry = time.Minute

var lastBackupTry = struct {
	mu sync.Mutex
	m  map[string]time.Time
}{m: map[string]time.Time{}}

// backupStopped runs BackupTranscript off the caller's goroutine (the list poll must not wait
// for a large copy), at most once per backupRetry per session.
func backupStopped(m session.Meta) {
	slot := session.UUID(m.Dir, m.Name)
	lastBackupTry.mu.Lock()
	if time.Since(lastBackupTry.m[slot]) < backupRetry {
		lastBackupTry.mu.Unlock()
		return
	}
	lastBackupTry.m[slot] = time.Now()
	lastBackupTry.mu.Unlock()
	go func() {
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
	if _, err := os.Lstat(dest); err == nil {
		if err := setAside(dest); err != nil {
			return false, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	created, err := placeCopy(have, dest)
	if err != nil {
		return false, err
	}
	if created {
		log.Printf("claude transcript %s restored from the Agent's backup", id)
	}
	return created, nil
}

// placeCopy creates dest as a copy of src unless something is already there, and never
// replaces anything. The copy is staged next to dest and linked in (atomic, so claude never
// sees half a log); where the filesystem has no hard links it is written to dest directly with
// O_EXCL instead, and removed again if that fails part way.
func placeCopy(src, dest string) (created bool, err error) {
	tmp, err := copyPreserving(src, filepath.Dir(dest), ".restore-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp)
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

// ArchiveBackup returns the backed-up log of a session whose own log is gone, for the cleanup
// archive: the file to read and the path a restore of the archive should write it to. ok=false
// when there is no backup. Deleting the session removes the backup, so without this a
// conversation whose log vanished before the delete would be lost in the same step.
func ArchiveBackup(m session.Meta) (src, dest string, ok bool, err error) {
	slot := session.UUID(m.Dir, m.Name)
	slotDir, err := backupSlotDir(slot)
	if err != nil {
		return "", "", false, err
	}
	unlock := lockBackup(slot)
	defer unlock()
	id := recordedLogID(slot)
	have, project, err := backupFile(slotDir, id)
	if err != nil || have == "" {
		return "", "", false, err
	}
	return have, logDest(m.CWD(), project, id), true, nil
}

// supersedeBackup is StartFresh's part for the backup: the user chose to leave the old
// conversation behind, so the backup must not bring it back. Like the live logs it is kept,
// not deleted, under <slot>.superseded.<unixnano> (removed with the session), because when the
// original log is already gone the backup is the only copy.
func supersedeBackup(slot string) error {
	slotDir, err := backupSlotDir(slot)
	if err != nil {
		return err
	}
	unlock := lockBackup(slot)
	defer unlock()
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
