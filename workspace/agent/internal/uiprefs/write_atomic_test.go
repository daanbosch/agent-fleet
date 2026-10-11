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
