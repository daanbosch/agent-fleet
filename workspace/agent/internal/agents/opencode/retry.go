package opencode

// A provider error under retry.
//
// Measured (1.19.0, loopback stub provider answering every request with one status):
//
//	400 401 402 403 404 413   fail at once, nothing is retried (the assistant message carries the error)
//	429 (rate limit AND insufficient_quota), 500, 503, 529
//	                           retried without end: GET /session/status keeps answering
//	                           {"type":"retry","attempt":N,"message":…,"next":<epoch ms>} with the
//	                           backoff growing to ~17s by attempt 4, and /message never returns
//
// So a retry status is the only place a permanent condition that rides on a retryable status shows
// up (an exhausted quota on a 429, a region or privacy refusal a gateway wraps in a 500). The same
// status is an SSE session.status event on v1; v2 sends session.retry.scheduled and gives up on its
// own after ~11 attempts, but a permanent message there still burns that minute and a half.
// Aborting a session that is retrying leaves no error on the assistant message (measured: error
// null, parts empty), so the failure has to be carried by the driver (threadHandle.retryStop).

import (
	"fmt"
	"strings"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
)

// permanentRetryPhrases are lower-cased fragments of upstream messages that no retry can fix.
// Only well-defined phrases belong here; when in doubt a message is transient, because ending a
// turn that would have recovered is worse than waiting for one that never will.
var permanentRetryPhrases = []string{
	// region / privacy settings the member has to change
	"requires global regions",
	"not available in your region",
	"unsupported_country_region_territory",
	// quota and billing
	"exceeded your current quota",
	"insufficient_quota",
	"insufficient balance",
	"insufficient credits",
	"credit balance is too low",
	// credentials
	"incorrect api key",
	"invalid api key",
	"invalid_api_key",
	"invalid x-api-key",
}

// permanentRetry reports whether a retry message names a condition that retrying cannot fix.
func permanentRetry(msg string) bool {
	m := strings.ToLower(msg)
	for _, p := range permanentRetryPhrases {
		if strings.Contains(m, p) {
			return true
		}
	}
	return false
}

// permanentRetryMinAttempt is the attempt from which a permanent-looking message ends the turn:
// seeing it twice keeps one odd response from a flaky gateway from failing a turn that recovers.
const permanentRetryMinAttempt = 2

// retryFailure is the turn's failure text for a message that ended it, tagged like errors.go's.
func retryFailure(msg string) string {
	return fmt.Sprintf("[error] provider error opencode kept retrying: %s", msg)
}

// noteRetry records that the runtime is retrying this handle's session. A message that cannot
// succeed ends the turn: the abort is ours (abortAsked), and runTurn lands TurnFailed with the
// message once /message comes back.
func (h *threadHandle) noteRetry(attempt int, msg string, nextMs int64) {
	h.mu.Lock()
	if !h.running {
		h.mu.Unlock()
		return // another client's turn (an attached TUI's) is not ours to fail
	}
	info := &agents.RetryInfo{Attempt: attempt, Message: msg, NextMs: nextMs}
	if h.retryStop != "" {
		h.mu.Unlock()
		return // the abort is already on its way
	}
	stop := attempt >= permanentRetryMinAttempt && permanentRetry(msg)
	if !stop {
		h.retry = info
		h.mu.Unlock()
		return
	}
	info.Failed = true
	h.retry, h.retryFailed = nil, info
	h.retryStop = retryFailure(msg)
	h.abortAsked = true
	addr, ses, dir := h.addr, h.ses, h.dir
	h.mu.Unlock()
	go abortSession(addr, ses, dir)
}

// clearRetry drops the live retry note: the retried call produced output, or the turn ended.
func (h *threadHandle) clearRetry() {
	h.mu.Lock()
	h.retry = nil
	h.mu.Unlock()
}

// retryInfo is what the mirror shows: the live retry, or the one that ended the last turn.
// Caller holds h.mu.
func (h *threadHandle) retryInfo() *agents.RetryInfo {
	if h.retry != nil {
		c := *h.retry
		return &c
	}
	if h.retryFailed != nil {
		c := *h.retryFailed
		return &c
	}
	return nil
}
