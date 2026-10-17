package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"sync/atomic"
)

// sqlDB wraps *sql.DB to rewrite placeholders per dialect before delegating, so
// the store's dialect-neutral `?` queries run unchanged on SQLite (positional ?)
// and Postgres ($1..$n). Every other *sql.DB method passes through the embedding.
type sqlDB struct {
	*sql.DB
	rb func(string) string
	// stmts counts the statements sent through this wrapper (transactions included), so a
	// test can pin a write's database round trips rather than only its wall time.
	stmts atomic.Int64
}

func (d *sqlDB) ExecContext(ctx context.Context, q string, a ...any) (sql.Result, error) {
	d.stmts.Add(1)
	return d.DB.ExecContext(ctx, d.rb(q), a...)
}
func (d *sqlDB) QueryContext(ctx context.Context, q string, a ...any) (*sql.Rows, error) {
	d.stmts.Add(1)
	return d.DB.QueryContext(ctx, d.rb(q), a...)
}
func (d *sqlDB) QueryRowContext(ctx context.Context, q string, a ...any) *sql.Row {
	d.stmts.Add(1)
	return d.DB.QueryRowContext(ctx, d.rb(q), a...)
}
func (d *sqlDB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sqlTx, error) {
	tx, err := d.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &sqlTx{Tx: tx, rb: d.rb, stmts: &d.stmts}, nil
}

// sqlTx is the transaction analogue of sqlDB (same rebind, same counter).
type sqlTx struct {
	*sql.Tx
	rb    func(string) string
	stmts *atomic.Int64
}

func (t *sqlTx) ExecContext(ctx context.Context, q string, a ...any) (sql.Result, error) {
	t.stmts.Add(1)
	return t.Tx.ExecContext(ctx, t.rb(q), a...)
}
func (t *sqlTx) QueryContext(ctx context.Context, q string, a ...any) (*sql.Rows, error) {
	t.stmts.Add(1)
	return t.Tx.QueryContext(ctx, t.rb(q), a...)
}
func (t *sqlTx) QueryRowContext(ctx context.Context, q string, a ...any) *sql.Row {
	t.stmts.Add(1)
	return t.Tx.QueryRowContext(ctx, t.rb(q), a...)
}

// rebindNoop leaves ? placeholders as-is (SQLite / modernc uses positional ?).
func rebindNoop(q string) string { return q }

// rebindDollar rewrites ordinal ? placeholders to Postgres $1..$n, skipping any
// inside single-quoted string literals (the store has none today; defensive).
func rebindDollar(q string) string {
	var b strings.Builder
	n := 0
	inStr := false
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c == '\'':
			inStr = !inStr
			b.WriteByte(c)
		case c == '?' && !inStr:
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// StatementCount is how many statements this store has sent so far. Tests and benchmarks
// take the difference across one call to measure its database round trips.
func (s *SQL) StatementCount() int64 { return s.db.stmts.Load() }

// DB is the escape hatch for main-side tests that drive tables with raw SQL
// (workspace_activity in reaper_test.go, tenant.status in limits_test.go). The returned
// type stays unexported on purpose: a caller outside this package can do nothing with it
// beyond ExecContext.
//
// Not for production code — dialect differences are confined to rebind below.
func (s *SQL) DB() *sqlDB { return s.db }
