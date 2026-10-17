//go:build !unix

package agy

import "os"

// tryFlock is a no-op where flock is unavailable: writers still re-check the file before
// publishing, which narrows the race without closing it.
func tryFlock(*os.File) bool { return true }
