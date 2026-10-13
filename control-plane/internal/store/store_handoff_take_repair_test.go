package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/k-k1/agent-fleet/control-plane/internal/pgtest"
)

// A database that ran version 0095 (pg 0080) as member_ui_state recorded it as applied and
// never ran handoff_take. These rebuild that state (drop the three columns after a full
// migrate) and check that the next Migrate puts them back, and that a healthy database is left
// alone.
func TestHandoffTakeColumnsRepairSQLite(t *testing.T) {
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	checkHandoffTakeRepair(t, st)
}

func TestHandoffTakeColumnsRepairPostgres(t *testing.T) {
	url, ok := pgtest.Schema(t)
	if !ok {
		t.Skip("set AF_TEST_DATABASE_URL to run against Postgres")
	}
	st, err := OpenPostgres(url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	checkHandoffTakeRepair(t, st)
}

func checkHandoffTakeRepair(t *testing.T, st *SQL) {
	t.Helper()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	probe := func() error {
		_, err := st.DB().ExecContext(ctx, `SELECT take_token, taken_at, take_confirmed_at FROM session_handoff_offer`)
		return err
	}
	if err := probe(); err != nil {
		t.Fatalf("fresh database lacks the columns: %v", err)
	}
	for _, c := range []string{"take_token", "taken_at", "take_confirmed_at"} {
		if _, err := st.DB().ExecContext(ctx, `ALTER TABLE session_handoff_offer DROP COLUMN `+c); err != nil {
			t.Fatalf("drop %s: %v", c, err)
		}
	}
	if probe() == nil {
		t.Fatal("the columns are still there after the drop; the test rebuilds nothing")
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate over the broken state: %v", err)
	}
	if err := probe(); err != nil {
		t.Fatalf("migrate did not repair the columns: %v", err)
	}
	// Idempotent: a further boot on the healthy database changes nothing and does not fail.
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("second migrate on a healthy database: %v", err)
	}
}
