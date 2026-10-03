package database

import (
	"context"
	"time"

	"github.com/cryskram/veridb/internal/config"
)

// ResultSet is a driver-neutral query result.
type ResultSet struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
	// Truncated is true when the row cap stopped the read early.
	Truncated bool `json:"truncated"`
	// RowCount is len(Rows), kept explicit for convenience in responses.
	RowCount int           `json:"row_count"`
	Duration time.Duration `json:"-"`
}

// ExecResult is a driver-neutral mutation result.
type ExecResult struct {
	RowsAffected int64         `json:"rows_affected"`
	Duration     time.Duration `json:"-"`
}

// Driver is the contract every database adapter implements. The rest of VeriDB
// is written against this interface, so adding MySQL or SQLite later means
// adding one implementation plus a config driver name.
type Driver interface {
	// Name is the driver identifier, e.g. "postgres".
	Name() string
	// Ping verifies the connection is usable.
	Ping(ctx context.Context) error
	// ServerVersion returns a short version string for status output.
	ServerVersion(ctx context.Context) (string, error)
	// Query runs a read statement and returns normalised, JSON-friendly values.
	Query(ctx context.Context, sql string, args ...any) (*ResultSet, error)
	// Exec runs a mutating statement.
	Exec(ctx context.Context, sql string, args ...any) (ExecResult, error)
	// Begin starts a transaction. VeriDB uses one to run a mutation, check how
	// many rows it touched, and roll back when the policy cap is exceeded.
	Begin(ctx context.Context) (Tx, error)
	// Close releases all pooled connections.
	Close()
}

// Tx is a transaction in progress.
type Tx interface {
	Query(ctx context.Context, sql string, args ...any) (*ResultSet, error)
	Exec(ctx context.Context, sql string, args ...any) (ExecResult, error)
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// ConnectionOptions carries everything a driver needs besides the DSN.
type ConnectionOptions struct {
	// Name is the logical VeriDB database name, used for logging.
	Name string
	// Driver is the adapter name from config.
	Driver string
	// DSN targets the physical database.
	DSN string
	// MaintenanceDSN targets the maintenance database on the same server and is
	// used only for existence checks and listing.
	MaintenanceDSN string
	// Pool bounds the connection pool.
	Pool config.PoolConfig
	// StatementTimeout is applied server-side to every statement.
	StatementTimeout time.Duration
	// ReadOnly sets default_transaction_read_only on every pooled connection,
	// so the server itself refuses writes as a second line of defence.
	ReadOnly bool
}

// Warning records a database that VeriDB refused to expose, or something it
// could not verify during startup.
type Warning struct {
	Database string `json:"database"`
	Level    string `json:"level"` // "warn" or "error"
	Message  string `json:"message"`
}
