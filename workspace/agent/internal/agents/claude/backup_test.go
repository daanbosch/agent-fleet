package claude

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

// backupFixture is a claude session with a meta, a finished turn on record and one log in its
// own project directory.
func backupFixture(t *testing.T, name string) (cfg string, m session.Meta, sid, logPath string) {
	t.Helper()
	cfg = isolateSlot(t)
	m = session.Meta{Kind: session.KindClaude, Name: name, Dir: t.TempDir(), CreatedAt: time.Now().Format(time.RFC3339)}
	session.WriteMeta(m)
	sid = session.UUID(m.Dir, m.Name)
	logPath = writeSlotJSONL(t, cfg, projectKey(m.Dir), sid)
	NoteTalked(sid)
	return
}

func mustBackup(t *testing.T, m session.Meta) {
	t.Helper()
	if err := BackupTranscript(m); err != nil {
		t.Fatal(err)
	}
}

// The acceptance case: the log is gone, the backup brings the conversation back, and the
// restored file keeps the original's permission bits and mtime.
func TestBuildLaunchRestoresDeletedLogFromBackup(t *testing.T) {
	_, m, sid, logPath := backupFixture(t, "restore-me")
	if err := os.Chmod(logPath, 0o640); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(logPath, old, old); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(logPath)
	mustBackup(t, m)
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}

	plan, err := launchOf(t, m)
	if err != nil {
		t.Fatalf("launch after the log was deleted: %v", err)
	}
	if !strings.Contains(plan.Program, "--resume") {
		t.Fatalf("program = %q, want a resume of the restored log", plan.Program)
	}
	got, err := os.ReadFile(logPath)
	if err != nil || string(got) != string(want) {
		t.Fatalf("restored log = %q, %v; want %q", got, err, want)
	}
	fi, _ := os.Stat(logPath)
	if fi.Mode().Perm() != 0o640 || !fi.ModTime().Equal(old) {
		t.Fatalf("restored mode %v mtime %v, want 0640 and %v", fi.Mode().Perm(), fi.ModTime(), old)
	}
	if !HadConversation(sid) {
		t.Fatal("conversation marker lost")
	}
}

// Without a backup the refusal of the previous change stands.
func TestBuildLaunchWithoutBackupStillRefuses(t *testing.T) {
	_, m, _, logPath := backupFixture(t, "no-backup")
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if _, err := launchOf(t, m); !errors.Is(err, ErrTranscriptLost) {
		t.Fatalf("err = %v, want ErrTranscriptLost", err)
	}
}

// A live log that is newer than the backup is never replaced by it.
func TestRestoreNeverOverwritesNewerLiveLog(t *testing.T) {
	_, m, sid, logPath := backupFixture(t, "live-newer")
	mustBackup(t, m)
	newer := []byte(`{"type":"user"}` + "\n" + `{"type":"assistant"}` + "\n")
	if err := os.WriteFile(logPath, newer, 0o600); err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreTranscript(sid, m.Dir)
	if err != nil || restored {
		t.Fatalf("restored = %v, %v; want no restore while a live log exists", restored, err)
	}
	if got, _ := os.ReadFile(logPath); string(got) != string(newer) {
		t.Fatalf("live log was overwritten: %q", got)
	}
	// Positive control: the same call does restore once the log is gone.
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if restored, err := RestoreTranscript(sid, m.Dir); err != nil || !restored {
		t.Fatalf("control: restored = %v, %v", restored, err)
	}
}

// The backup is only ever replaced by a newer live log: an older or stub one leaves it alone,
// and an unchanged one is not copied again.
func TestBackupKeepsNewestAndSkipsUnchanged(t *testing.T) {
	_, m, sid, logPath := backupFixture(t, "bk-newest")
	mustBackup(t, m)
	slotDir, _ := backupSlotDir(sid)
	bp, _, err := backupFile(slotDir, sid)
	if err != nil || bp == "" {
		t.Fatalf("backup file: %q %v", bp, err)
	}
	first, _ := os.Stat(bp)

	mustBackup(t, m)
	if again, _ := os.Stat(bp); !os.SameFile(first, again) {
		t.Fatal("an unchanged log was copied again")
	}

	// A stub (no user/assistant line) is not a conversation: the backup must survive it.
	if err := os.WriteFile(logPath, []byte(`{"type":"bridge-session"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustBackup(t, m)
	if got, _ := os.ReadFile(bp); !strings.Contains(string(got), `"type":"user"`) {
		t.Fatalf("a stub replaced the backup: %q", got)
	}

	// A resumable log with an OLDER mtime than the backup does not replace it either.
	grown := []byte(`{"type":"user"}` + "\n" + `{"type":"assistant","x":1}` + "\n")
	if err := os.WriteFile(logPath, grown, 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(logPath, past, past); err != nil {
		t.Fatal(err)
	}
	mustBackup(t, m)
	if got, _ := os.ReadFile(bp); string(got) == string(grown) {
		t.Fatal("an older live log replaced a newer backup")
	}

	// A newer one does.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(logPath, future, future); err != nil {
		t.Fatal(err)
	}
	mustBackup(t, m)
	if got, _ := os.ReadFile(bp); string(got) != string(grown) {
		t.Fatalf("a newer live log did not update the backup: %q", got)
	}
}

// The backup lives under the Agent's data directory, never where claude's sweep looks, and
// holds at most one log per slot even after the conversation drifted to a new id.
func TestBackupLocationAndOneCopyPerSlot(t *testing.T) {
	cfg, m, sid, _ := backupFixture(t, "bk-one")
	mustBackup(t, m)
	drifted := "47000000-0000-4000-8000-0000000drift"
	writeSlotJSONL(t, cfg, projectKey(m.Dir), drifted)
	sids.Write(sid, drifted)
	mustBackup(t, m)

	slotDir, _ := backupSlotDir(sid)
	if strings.HasPrefix(slotDir, cfg) {
		t.Fatalf("backup %s is inside CLAUDE_CONFIG_DIR %s", slotDir, cfg)
	}
	var files []string
	_ = filepath.WalkDir(slotDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) != 1 || filepath.Base(files[0]) != drifted+".jsonl" {
		t.Fatalf("backup files = %v, want only the drifted id's log", files)
	}
}

// StartFresh must not leave the backup to be restored by the launch it exists for; the backup is
// kept aside and goes with the session.
func TestStartFreshSetsAsideBackupAndRemoveClearsIt(t *testing.T) {
	_, m, sid, logPath := backupFixture(t, "bk-fresh")
	mustBackup(t, m)
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if err := StartFresh(sid); err != nil {
		t.Fatal(err)
	}
	plan, err := launchOf(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan.Program, "--resume") {
		t.Fatalf("program = %q: the fresh start resumed the backup", plan.Program)
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Fatal("the backup was restored after a fresh start")
	}
	slotDir, _ := backupSlotDir(sid)
	kept, _ := filepath.Glob(slotDir + ".superseded.*")
	if len(kept) != 1 {
		t.Fatalf("superseded backups = %v, want one kept", kept)
	}
	if err := RemoveTranscriptBackup(sid); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(slotDir + "*"); len(left) != 0 {
		t.Fatalf("backup left after removal: %v", left)
	}
}

// An unreadable projects tree is an error, not "no log": restoring over a log that is merely
// hidden would fork the conversation.
func TestRestoreReturnsEnumerationError(t *testing.T) {
	cfg, m, sid, logPath := backupFixture(t, "bk-eacces")
	mustBackup(t, m)
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(cfg, "projects")
	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(root, 0o700)
	if _, err := os.ReadDir(root); err == nil {
		t.Skip("directory permissions are not enforced (running as root)")
	}
	if restored, err := RestoreTranscript(sid, m.Dir); err == nil || restored {
		t.Fatalf("restored = %v, err = %v; want an error", restored, err)
	}
	if _, err := launchOf(t, m); errors.Is(err, ErrTranscriptLost) || err == nil {
		t.Fatalf("launch err = %v, want the I/O error and not transcript_lost", err)
	}
}

// A stub sitting where the log belongs is set aside, not overwritten or deleted.
func TestRestoreSetsAsideStubInTheWay(t *testing.T) {
	_, m, sid, logPath := backupFixture(t, "bk-stub")
	mustBackup(t, m)
	stub := []byte(`{"type":"bridge-session"}` + "\n")
	if err := os.WriteFile(logPath, stub, 0o600); err != nil {
		t.Fatal(err)
	}
	if restored, err := RestoreTranscript(sid, m.Dir); err != nil || !restored {
		t.Fatalf("restored = %v, %v", restored, err)
	}
	kept, _ := filepath.Glob(logPath + ".superseded.*")
	if len(kept) != 1 {
		t.Fatalf("set-aside stubs = %v, want one", kept)
	}
	if got, _ := os.ReadFile(kept[0]); string(got) != string(stub) {
		t.Fatalf("set-aside content = %q", got)
	}
}

// No meta, no backup: a delete that raced the copy must not leave one behind.
func TestBackupSkipsDeletedSession(t *testing.T) {
	_, m, sid, _ := backupFixture(t, "bk-gone")
	session.RemoveMeta(m.Name)
	mustBackup(t, m)
	slotDir, _ := backupSlotDir(sid)
	if _, err := os.Stat(slotDir); err == nil {
		t.Fatal("a backup was made for a session with no meta")
	}
}
