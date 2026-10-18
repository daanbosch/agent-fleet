package memreview

// The review's file store. Every record the review relies on after a stop - the attempt written
// before a call, the daily count, a queued proposal - must survive not only the process ending but
// the host or the storage failing right after the write returned. A rename of an unsynced temp file
// can come back after a power cut empty or missing, and a directory entry that was never synced can
// vanish, so a write here is: write a temp file inside the store, fsync it, rename it into place,
// fsync the directory it left and the one it entered. Removal syncs the directory too and reports
// its error: a text that could not be deleted is not deleted.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fsOps are the calls that decide durability; tests replace them to see the order and to fail.
var fsOps = struct {
	syncFile func(*os.File) error
	syncDir  func(string) error
	rename   func(oldpath, newpath string) error
}{
	syncFile: func(f *os.File) error { return f.Sync() },
	syncDir: func(dir string) error {
		d, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer d.Close()
		return d.Sync()
	},
	rename: os.Rename,
}

const stagingDir = ".staging"

// store is one directory of JSON records, <root>/<sub>/<key>.json.
type store[T any] struct{ sub string }

func (s store[T]) Dir() string { return filepath.Join(stateRoot(), s.sub) }

func (s store[T]) path(key string) string { return filepath.Join(s.Dir(), key+".json") }

func (s store[T]) staging() string { return filepath.Join(s.Dir(), stagingDir) }

// Write stores v under key durably (see the file comment).
func (s store[T]) Write(key string, v T) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	dir := s.Dir()
	if err := mkdirAllSynced(s.staging()); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.staging(), key+".*") // mode 0600
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	if werr == nil {
		werr = fsOps.syncFile(f)
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = fsOps.rename(tmp, s.path(key))
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return werr
	}
	for _, d := range []string{dir, s.staging()} {
		if err := fsOps.syncDir(d); err != nil {
			return err
		}
	}
	return nil
}

// Read returns the record, ok=false when it is missing or unreadable.
func (s store[T]) Read(key string) (T, bool) {
	var v T
	b, err := os.ReadFile(s.path(key))
	if err != nil || len(b) == 0 || json.Unmarshal(b, &v) != nil {
		var zero T
		return zero, false
	}
	return v, true
}

// Remove deletes the record and syncs the directory. A record that is already gone is not an
// error; anything else is, so a caller never believes a text is deleted when it is not. Temp files
// the store staged for the key are removed too.
func (s store[T]) Remove(key string) error {
	err := os.Remove(s.path(key))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if stale, _ := filepath.Glob(filepath.Join(s.staging(), key+".*")); len(stale) > 0 {
		for _, p := range stale {
			if rerr := os.Remove(p); rerr != nil && !os.IsNotExist(rerr) {
				return rerr
			}
		}
		if serr := fsOps.syncDir(s.staging()); serr != nil && !os.IsNotExist(serr) {
			return serr
		}
	}
	if serr := fsOps.syncDir(s.Dir()); serr != nil && !os.IsNotExist(serr) {
		return serr
	}
	return nil
}

// keys lists the stored keys.
func (s store[T]) keys() []string {
	ents, err := os.ReadDir(s.Dir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if name, ok := strings.CutSuffix(e.Name(), ".json"); ok && e.Type().IsRegular() {
			out = append(out, name)
		}
	}
	return out
}

// sweepStaging removes staged files older than age. A writer killed between the create and the
// rename leaves one behind, and what it holds may be a proposal's text.
func (s store[T]) sweepStaging(age time.Duration, now time.Time) {
	ents, err := os.ReadDir(s.staging())
	if err != nil {
		return
	}
	removed := false
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > age {
			if os.Remove(filepath.Join(s.staging(), e.Name())) == nil {
				removed = true
			}
		}
	}
	if removed {
		_ = fsOps.syncDir(s.staging())
	}
}

// mkdirAllSynced creates dir and every missing ancestor and makes each new directory entry
// durable: every directory it created, and the existing directory the chain hangs from. Creating
// them without that can lose the whole chain, a state root included, to a host failure that comes
// after the first record in it was reported written.
func mkdirAllSynced(dir string) error {
	var created []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			created = append(created, d) // the first directory that already existed
			break
		}
		created = append(created, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if len(created) == 1 {
		return nil // nothing was created
	}
	for _, d := range created {
		if err := fsOps.syncDir(d); err != nil {
			return err
		}
	}
	return nil
}
