//go:build unix

package agy

import (
	"os"
	"syscall"
)

// tryFlock takes an exclusive advisory lock on f without blocking; the kernel drops it when the
// holder exits.
func tryFlock(f *os.File) bool {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
}
