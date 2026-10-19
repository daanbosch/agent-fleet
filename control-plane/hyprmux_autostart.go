package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/k-k1/agent-fleet/control-plane/internal/store"
)

// hyprmuxAutostartRetryAfter is the one retry of a boot start that failed. A second failure
// is left stopped and visible on the workspace, not retried in a loop.
const hyprmuxAutostartRetryAfter = 30 * time.Second

// hyprmuxAutostartOn is the opt-in for starting the host's workspaces at boot. It applies to
// the hyprmux-host profile only, and only when AF_HYPRMUX_AUTOSTART=1.
func hyprmuxAutostartOn(profile string) bool {
	return profile == "hyprmux-host" && envBool("AF_HYPRMUX_AUTOSTART", false)
}

// autostartHyprmuxWorkspaces starts every workspace of the default tenant once, then retries
// the ones that failed a single time after retryAfter. It never stops the CP: a workspace
// that still fails stays stopped, and the failure is logged.
func (m *manager) autostartHyprmuxWorkspaces(ctx context.Context, retryAfter time.Duration) {
	wss, err := m.store.ListWorkspaces(ctx, m.defaultTenantID)
	if err != nil {
		log.Printf("hyprmux autostart: list workspaces: %v", err)
		return
	}
	var failed []store.Workspace
	for _, ws := range wss {
		if err := m.bootStartWorkspace(ctx, ws); err != nil {
			log.Printf("hyprmux autostart: workspace %s: %v (retrying in %s)", ws.ID, err, retryAfter)
			failed = append(failed, ws)
		}
	}
	if len(failed) == 0 {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(retryAfter):
	}
	for _, ws := range failed {
		if err := m.bootStartWorkspace(ctx, ws); err != nil {
			log.Printf("hyprmux autostart: workspace %s stays stopped: %v", ws.ID, err)
		}
	}
}

func (m *manager) bootStartWorkspace(ctx context.Context, ws store.Workspace) error {
	identityID, ok, err := m.store.IdentityIDForMembership(ctx, ws.MembershipID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("membership %s has no identity", ws.MembershipID)
	}
	res, aerr := m.resolveByMembership(ctx, identityID, ws.MembershipID)
	if aerr != nil {
		return fmt.Errorf("resolve: %s", aerr.message)
	}
	if aerr := newWorkspaceAPI(m, false).ensureWorkspaceStarted(ctx, res); aerr != nil {
		return fmt.Errorf("start: %s", aerr.message)
	}
	log.Printf("hyprmux autostart: workspace %s started", ws.ID)
	return nil
}
