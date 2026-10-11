package uiprefs

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// A reader racing a PUT must never see a half-written file: a launch-time gate that read an
// empty file would answer "off" and pin the wrong environment on a long-lived host.
func TestWriteAtomicNeverExposesAPartialFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"agentMemory":true,"padding":"x"}`)
	if err := WriteAtomic(body); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if err := WriteAtomic(body); err != nil {
					t.Error(err)
					return
				}
			}
		}
	}()
	for i := 0; i < 20000; i++ {
		if !AgentMemory() {
			close(stop)
			wg.Wait()
			t.Fatalf("read %d saw AgentMemory=false while the file always says true", i)
		}
	}
	close(stop)
	wg.Wait()
	if fi, err := os.Stat(Path()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, err = %v; want 0600", fi.Mode().Perm(), err)
	}
}

// The prefs file may be a symlink (a dotfile mapped onto durable storage). The link must survive
// a save, for an existing target, a not-yet-created one (dangling), a relative link and a chain.
func TestWriteAtomicKeepsSymlinks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing bool
		relative bool
		chain    bool
	}{
		{"existing", true, false, false},
		{"dangling", false, false, false},
		{"relative", false, true, false},
		{"chain", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			store := filepath.Join(home, "store")
			if err := os.MkdirAll(store, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(store, "prefs.json")
			if tc.existing {
				if err := os.WriteFile(target, []byte(`{"old":1}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			link := target
			if tc.chain {
				link = filepath.Join(store, "hop.json")
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
			}
			if tc.relative {
				rel, _ := filepath.Rel(filepath.Dir(Path()), link)
				link = rel
			}
			if err := os.Symlink(link, Path()); err != nil {
				t.Fatal(err)
			}
			if err := WriteAtomic([]byte(`{"agentMemory":true}`)); err != nil {
				t.Fatal(err)
			}
			if fi, err := os.Lstat(Path()); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("the symlink was replaced (err=%v)", err)
			}
			got, err := os.ReadFile(target)
			if err != nil || string(got) != `{"agentMemory":true}` {
				t.Fatalf("target = %q, err = %v", got, err)
			}
		})
	}
}
