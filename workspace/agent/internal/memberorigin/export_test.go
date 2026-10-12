package memberorigin

// RestartForTest is what a process restart does to the verifier: an empty nonce cache and a new
// start time.
func RestartForTest(at int64) {
	Reload()
	SetStartedAt(at)
}
