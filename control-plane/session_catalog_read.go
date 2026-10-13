package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/k-k1/agent-fleet/control-plane/internal/runtime"
)

// agentCatalogMaxBytes bounds GET /sessions/catalog. The catalog has one row per session,
// archived ones included, so it grows with the member's history; agentText's 1 MiB is a cap
// sized for MCP tool output and cut a real 1,174-session catalog mid-row. The Agent now
// sends only the fields the sync stores, but an older Agent still sends every session field,
// so this matches ownerGET's 16 MiB rather than the slimmed size.
const agentCatalogMaxBytes = 16 << 20

// errCatalogTooLarge marks a catalog that did not fit in agentCatalogMaxBytes. A cut body
// otherwise surfaces as a JSON syntax error, which says nothing about why.
var errCatalogTooLarge = errors.New("session catalog exceeds the read limit")

// catalogSyncError is a sync that failed on the owner Agent's side: unreachable, an error
// status, an oversized or undecodable catalog. A store error during the same sync is not
// one, so callers can tell "the owner's inventory is unavailable" from an internal failure.
type catalogSyncError struct{ err error }

func (e *catalogSyncError) Error() string { return "session catalog sync: " + e.err.Error() }
func (e *catalogSyncError) Unwrap() error { return e.err }

// catalogSyncAPIError is what a failed sync answers when the workspace IS running. It must
// not be workspace_not_running: the owner then checks a running workspace and has nothing to
// fix.
func catalogSyncAPIError(err error) *apiError {
	var se *catalogSyncError
	if !errors.As(err, &se) {
		return internalErr(err)
	}
	// Too large is permanent, so it gets its own code: under catalog_sync_failed the Console's
	// translation would tell the user to retry something no retry fixes.
	if errors.Is(err, errCatalogTooLarge) {
		return &apiError{http.StatusBadGateway, errCodeCatalogTooLarge, "owner workspace session catalog is too large to read"}
	}
	return &apiError{http.StatusBadGateway, errCodeCatalogSyncFailed, "owner workspace session catalog could not be read"}
}

// agentCatalog is one answer of GET /sessions/catalog.
type agentCatalog struct {
	sessions []sessionWire
	// cursor is where the Agent's catalog stood when it answered; empty from an Agent that
	// does not speak the delta protocol. Hand it back as `since` on the next read.
	cursor string
	// delta: sessions holds only the rows changed after the cursor we sent, and deleted the
	// names that are gone. False means sessions is the whole catalog (an Agent from before
	// deltas, or one that did not recognise our cursor: restarted, or forgot the history).
	delta   bool
	deleted []string
}

// agentCatalogWire is the Agent's GET /sessions/catalog body as the sync reads it
// (workspace/agent session.CatalogResponse). Both modules pin their half in wire.golden.
type agentCatalogWire struct {
	Sessions []sessionWire `json:"sessions"`
	Cursor   string        `json:"cursor"`
	Delta    bool          `json:"delta"`
	Deleted  []string      `json:"deleted"`
}

// readAgentCatalog fetches GET /sessions/catalog with its own size limit, reading one byte
// past it so truncation is reported as such instead of as malformed JSON. `since` is the
// cursor of the last catalog applied ("" asks for the whole catalog plus a cursor). An Agent
// that predates deltas ignores the parameter and answers the whole catalog without a cursor,
// which is exactly the no-delta case.
func readAgentCatalog(ctx context.Context, rt runtime.Runtime, since string) (agentCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rt.Endpoint()+"/sessions/catalog?since="+url.QueryEscape(since), nil)
	if err != nil {
		return agentCatalog{}, &catalogSyncError{err}
	}
	if rt.Token() != "" {
		req.Header.Set("Authorization", "Bearer "+rt.Token())
	}
	resp, err := agentHTTPClient.Do(req)
	if err != nil {
		return agentCatalog{}, &catalogSyncError{fmt.Errorf("workspace agent unreachable: %w", err)}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, agentCatalogMaxBytes+1))
	if err != nil {
		return agentCatalog{}, &catalogSyncError{fmt.Errorf("read: %w", err)}
	}
	if resp.StatusCode >= 400 {
		if len(b) > 512 {
			b = b[:512]
		}
		return agentCatalog{}, &catalogSyncError{&agentHTTPError{status: resp.StatusCode, body: strings.TrimSpace(string(b))}}
	}
	if len(b) > agentCatalogMaxBytes {
		return agentCatalog{}, &catalogSyncError{errCatalogTooLarge}
	}
	var wire agentCatalogWire
	if err := json.Unmarshal(b, &wire); err != nil {
		return agentCatalog{}, &catalogSyncError{fmt.Errorf("decode: %w", err)}
	}
	// A delta answer is only meaningful to a caller that sent a cursor and got one back.
	delta := wire.Delta && since != "" && wire.Cursor != ""
	return agentCatalog{sessions: wire.Sessions, cursor: wire.Cursor, delta: delta, deleted: wire.Deleted}, nil
}
