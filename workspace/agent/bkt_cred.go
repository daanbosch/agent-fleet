package main

// The REST credential of a pasted Bitbucket API-token connection, for the `bkt` wrapper
// (workspace/bkt-auth-wrapper.sh).
//
// `git credential fill` cannot carry it: the helper rewrites the stored Atlassian email to the
// static `x-bitbucket-api-token-auth` username, because git-over-HTTPS refuses the email, while
// the REST API (and so bkt) accepts nothing but email:token as HTTP Basic. This subcommand
// answers that one question and no other, and mirrors the helper's precedence: an OAuth
// connection is served by `git credential fill` (bearer), so it prints nothing here.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/secrets"
)

// bktCredStoreWait bounds the wait for the store: the wrapper blocks on this process, so a
// writer wedged on the lock must cost bkt a delay, not a hang (the git helper does the same).
var bktCredStoreWait = 30 * time.Second

// runBktCred is `workspace-agent bkt-cred`: no arguments, no stdin.
func runBktCred(_ []string) { bktCredGet(os.Stdout) }

// bktCredGet writes `username=<email>` and `password=<token>` for the bitbucket.org
// API-token connection, or nothing (an unreadable store, an OAuth connection, no
// connection, or a stored user that is not an Atlassian email).
func bktCredGet(w io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), bktCredStoreWait)
	defer cancel()
	s, err := secrets.LoadContext(ctx)
	if err != nil || s.Bitbucket != nil {
		return
	}
	e, ok := s.Git["bitbucket.org"]
	if !ok || e.Token == "" || !strings.Contains(e.User, "@") {
		return
	}
	fmt.Fprintf(w, "username=%s\npassword=%s\n", e.User, e.Token)
}
