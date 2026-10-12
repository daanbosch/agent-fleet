package main

// The inbox's own live nudge and its CP-side upkeep (ADR 0111 decisions 8 and 9).
//
// GET /api/inbox/stream is an SSE connection of its own, not a stream added to
// GET /api/events: that route is withResolved and would create a workspace record for a
// member who only uses the inbox. The database stays the truth; a frame only says that the
// member's mailbox moved, and the Console re-reads GET /api/inbox. Where the stream is not
// available the Console polls that route instead.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/k-k1/agent-fleet/control-plane/internal/store"
)

const (
	// inboxStreamTick is how often a stream reads its member's counter: one indexed row.
	inboxStreamTick = 3 * time.Second
	inboxStreamPing = 20 * time.Second
	// inboxReconcileEvery applies the removal and block rows of the state table to tenants
	// nobody is reading. Reads reconcile their own tenant first, so this bounds staleness
	// for streams only.
	inboxReconcileEvery = time.Minute
	// inboxPurgeEvery runs the purge. Reads filter expired rows on their own, so the purge
	// only bounds how long an expired body stays in the database.
	inboxPurgeEvery = time.Hour
)

// inboxPushWire is the one frame the stream sends: the mailbox as the badge needs it.
type inboxPushWire struct {
	Stream string           `json:"stream"`
	Data   inboxMailboxWire `json:"data"`
}

// stream (GET /api/inbox/stream). It is filed under the membership exactly as withResolved
// files its requests, so removing the member closes it (member_removal.go): here, at once,
// and on other replicas at the removed-member sweep.
func (a inboxAPI) stream(w http.ResponseWriter, r *http.Request, _ store.Identity, mv store.MembershipView) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeAPIErr(w, &apiError{http.StatusInternalServerError, "internal", "streaming unsupported"})
		return
	}
	ctx, untrack := a.mgr.memberConns.track(r.Context(), mv.MembershipID)
	defer untrack()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	last := ""
	lastWrite := time.Now()
	tick := func() {
		mb, err := a.mgr.store.GetInboxMailbox(ctx, mv.MembershipID)
		if err != nil || ctx.Err() != nil {
			return
		}
		unseen, err := a.mgr.store.CountInboxUnseen(ctx, mv.MembershipID, inboxRetentionAt(time.Now().UTC()))
		if err != nil {
			return
		}
		b, _ := json.Marshal(inboxPushWire{Stream: "inbox", Data: mailboxWire(mb, unseen)})
		switch {
		case string(b) != last:
			last = string(b)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		case time.Since(lastWrite) >= a.ping:
			_, _ = fmt.Fprint(w, ": ping\n\n")
		default:
			return
		}
		lastWrite = time.Now()
		fl.Flush()
	}
	tick()
	t := time.NewTicker(a.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// runInboxUpkeep reconciles every tenant's requests each inboxReconcileEvery and purges
// each inboxPurgeEvery, from start-up on. Both are idempotent and safe on every replica at
// once, so there is no lease.
func (m *manager) runInboxUpkeep(ctx context.Context) {
	lastPurge := time.Time{}
	everyInterval(ctx, inboxReconcileEvery, func(ctx context.Context) {
		m.inboxUpkeepOnce(ctx, time.Since(lastPurge) >= inboxPurgeEvery)
		if time.Since(lastPurge) >= inboxPurgeEvery {
			lastPurge = time.Now()
		}
	})
}

func (m *manager) inboxUpkeepOnce(ctx context.Context, purge bool) {
	tenants, err := m.store.ListTenants(ctx)
	if err != nil {
		log.Printf("inbox upkeep: list tenants: %v", err)
		return
	}
	for _, t := range tenants {
		if _, err := m.store.ReconcileInbox(ctx, t.ID, store.NowTS()); err != nil {
			log.Printf("inbox upkeep: reconcile %s: %v", t.Slug, err)
		}
	}
	if !purge {
		return
	}
	n, err := m.store.PurgeInbox(ctx, inboxRetentionAt(time.Now().UTC()))
	if err != nil {
		log.Printf("inbox upkeep: purge: %v", err)
		return
	}
	if n > 0 {
		log.Printf("inbox upkeep: purged %d item(s)", n)
	}
}
