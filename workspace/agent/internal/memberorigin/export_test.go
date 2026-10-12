package memberorigin

// RestartForTest is what a process restart does to the verifier: an empty nonce cache and a new
// start time.
func RestartForTest(at int64) {
	Reload()
	mu.Lock()
	startedAt = at
	mu.Unlock()
}

// StartedBeforeForTest moves the start time back, so marks issued "now" are after it.
func StartedBeforeForTest(at int64) {
	mu.Lock()
	startedAt = at
	mu.Unlock()
}
