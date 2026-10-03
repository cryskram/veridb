package database

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/cryskram/veridb/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DriverNamePostgres is the config value that selects this adapter.
const DriverNamePostgres = "postgres"

const (
	defaultMaxConns          = int32(10)
	defaultMinConns          = int32(1)
	defaultMaxConnLifetime   = time.Hour
	defaultMaxConnIdleTime   = 30 * time.Minute
	defaultHealthCheckPeriod = time.Minute
)

// Postgres is the pgx-backed Driver implementation.
type Postgres struct {
	name string
	pool *pgxpool.Pool
}

var _ Driver = (*Postgres)(nil)

// NewPostgres opens a pool for opts.DSN.
func NewPostgres(ctx context.Context, opts ConnectionOptions) (*Postgres, error) {
	poolCfg, err := pgxpool.ParseConfig(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}

	poolCfg.MaxConns = int32Or(opts.Pool.MaxConns, defaultMaxConns)
	poolCfg.MinConns = int32Or(opts.Pool.MinConns, defaultMinConns)
	poolCfg.MaxConnLifetime = durationOr(opts.Pool.MaxConnLifetime, defaultMaxConnLifetime)
	poolCfg.MaxConnIdleTime = durationOr(opts.Pool.MaxConnIdleTime, defaultMaxConnIdleTime)
	poolCfg.HealthCheckPeriod = durationOr(opts.Pool.HealthCheckPeriod, defaultHealthCheckPeriod)

	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	poolCfg.ConnConfig.RuntimeParams["application_name"] = "veridb"

	// Server-side statement timeout so a runaway query is killed even if the
	// agent ignores the client-side context deadline.
	if opts.StatementTimeout > 0 {
		poolCfg.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(
			opts.StatementTimeout.Milliseconds(), 10,
		)
		poolCfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = strconv.FormatInt(
			opts.StatementTimeout.Milliseconds(), 10,
		)
	}

	// Defence in depth: even if the SQL guard were bypassed, Postgres itself
	// rejects writes on a read-only database.
	if opts.ReadOnly {
		poolCfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}

	return &Postgres{name: opts.Name, pool: pool}, nil
}

func (p *Postgres) Name() string { return DriverNamePostgres }

func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

func (p *Postgres) ServerVersion(ctx context.Context) (string, error) {
	var version string
	if err := p.pool.QueryRow(ctx, "SHOW server_version").Scan(&version); err != nil {
		return "", err
	}
	return version, nil
}

func (p *Postgres) Query(ctx context.Context, sql string, args ...any) (*ResultSet, error) {
	return collect(ctx, p.pool, sql, args...)
}

// querier is satisfied by both a pool and a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// collect runs a query and normalises every value for JSON output.
func collect(ctx context.Context, q querier, sql string, args ...any) (*ResultSet, error) {
	start := time.Now()

	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	fields := rows.FieldDescriptions()
	result := &ResultSet{
		Columns: make([]string, len(fields)),
	}
	for i, f := range fields {
		result.Columns[i] = string(f.Name)
	}

	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}

		row := make([]any, len(values))
		for i, v := range values {
			row[i] = normalizeValue(v)
		}
		result.Rows = append(result.Rows, row)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	result.RowCount = len(result.Rows)
	result.Duration = time.Since(start)

	return result, nil
}

func (p *Postgres) Exec(ctx context.Context, sql string, args ...any) (ExecResult, error) {
	start := time.Now()

	tag, err := p.pool.Exec(ctx, sql, args...)
	if err != nil {
		return ExecResult{}, err
	}

	return ExecResult{
		RowsAffected: tag.RowsAffected(),
		Duration:     time.Since(start),
	}, nil
}

func (p *Postgres) Close() { p.pool.Close() }

// Begin starts a transaction.
func (p *Postgres) Begin(ctx context.Context) (Tx, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}

	return &postgresTx{tx: tx}, nil
}

type postgresTx struct {
	tx pgx.Tx
}

var _ Tx = (*postgresTx)(nil)

func (t *postgresTx) Query(ctx context.Context, sql string, args ...any) (*ResultSet, error) {
	return collect(ctx, t.tx, sql, args...)
}

func (t *postgresTx) Exec(ctx context.Context, sql string, args ...any) (ExecResult, error) {
	start := time.Now()

	tag, err := t.tx.Exec(ctx, sql, args...)
	if err != nil {
		return ExecResult{}, err
	}

	return ExecResult{RowsAffected: tag.RowsAffected(), Duration: time.Since(start)}, nil
}

func (t *postgresTx) Commit(ctx context.Context) error { return t.tx.Commit(ctx) }

func (t *postgresTx) Rollback(ctx context.Context) error {
	// A rollback after a failed statement is expected; pgx reports
	// ErrTxClosed in that case and it is not worth surfacing.
	err := t.tx.Rollback(ctx)
	if err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return err
	}
	return nil
}

// ListServerDatabases returns the databases that exist on the server reachable
// via maintenanceDSN. It is a standalone function rather than a method because
// it must work before any per-database pool exists.
func ListServerDatabases(ctx context.Context, maintenanceDSN string) (map[string]struct{}, error) {
	conn, err := pgx.Connect(ctx, maintenanceDSN)
	if err != nil {
		return nil, fmt.Errorf("connect to maintenance database: %w", err)
	}
	defer conn.Close(ctx)

	rows, err := conn.Query(ctx, `
		SELECT datname
		FROM pg_database
		WHERE datallowconn
		  AND NOT datistemplate
	`)
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	defer rows.Close()

	found := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan database name: %w", err)
		}
		found[name] = struct{}{}
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return found, nil
}

func int32Or(v *int32, def int32) int32 {
	if v == nil {
		return def
	}
	return *v
}

func durationOr(v *config.Duration, def time.Duration) time.Duration {
	if v == nil {
		return def
	}
	return v.Std()
}
