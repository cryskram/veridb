package mcpserver

import (
	"context"
	"fmt"
	"time"

	"github.com/cryskram/veridb/internal/audit"
	"github.com/cryskram/veridb/internal/database"
	"github.com/cryskram/veridb/internal/guard"
	"github.com/cryskram/veridb/internal/redact"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxStatementsPerTransaction bounds a batch so one call cannot become an
// unbounded script.
const maxStatementsPerTransaction = 50

type executeInput struct {
	Database string `json:"database" jsonschema:"database name from list_databases"`
	SQL      string `json:"sql" jsonschema:"a single INSERT, UPDATE, DELETE or DDL statement; use $1, $2 for bind parameters"`
	Params   []any  `json:"params,omitempty" jsonschema:"positional values for $1, $2, ..."`
}

type statementInput struct {
	SQL    string `json:"sql" jsonschema:"one statement"`
	Params []any  `json:"params,omitempty" jsonschema:"positional values for $1, $2, ..."`
}

type transactionInput struct {
	Database   string           `json:"database" jsonschema:"database name from list_databases"`
	Statements []statementInput `json:"statements" jsonschema:"the statements to run atomically, in order"`
}

type executeOutput struct {
	Database     string  `json:"database"`
	RowsAffected int64   `json:"rows_affected"`
	AppliedCap   int     `json:"applied_cap,omitempty"`
	DurationMS   float64 `json:"duration_ms"`
	// Rows is present when the statement used RETURNING.
	Rows     []rowObject `json:"rows,omitempty"`
	RowCount int         `json:"row_count,omitempty"`
	Notice   string      `json:"notice,omitempty"`
}

func registerWriteTools(server *mcp.Server, deps *Deps) {
	if !deps.enabledForAny(writeToolsEnabled) {
		return
	}

	mcp.AddTool(
		server,
		&mcp.Tool{
			Name: "execute",
			Description: "Run one mutating SQL statement. Refused unless the database explicitly " +
				"allows the operation: INSERT/UPDATE needs allow_write, DELETE needs allow_delete, " +
				"schema changes need allow_ddl. UPDATE and DELETE must have a WHERE clause. The " +
				"statement runs in a transaction that is rolled back if it would affect more than " +
				"max_affected_rows, so an over-broad statement changes nothing. Add RETURNING to " +
				"see the affected rows.",
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, in executeInput) (*mcp.CallToolResult, executeOutput, error) {
			out, err := deps.runExecute(ctx, in)
			return nil, out, err
		},
	)

	mcp.AddTool(
		server,
		&mcp.Tool{
			Name: "run_in_transaction",
			Description: "Run several statements atomically: either every statement succeeds or none " +
				"of them take effect. Every statement is checked against the same policy as execute. " +
				"Use this when related changes must not land half-applied.",
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, in transactionInput) (*mcp.CallToolResult, executeOutput, error) {
			out, err := deps.runTransaction(ctx, in)
			return nil, out, err
		},
	)
}

func (d *Deps) runExecute(ctx context.Context, in executeInput) (executeOutput, error) {
	var out executeOutput

	entry, err := d.entry(in.Database, "write", writeToolsEnabled)
	if err != nil {
		return out, err
	}

	start := time.Now()
	rec := audit.Record{Tool: "execute", Database: in.Database, SQL: in.SQL, Params: in.Params}

	stmt, err := guard.Check(in.SQL, entry.Config.Policy, in.Database)
	if err != nil {
		return out, d.finish(rec, start, err)
	}

	if stmt.Kind == guard.KindRead {
		err := denial("not_allowed", "this is a read statement; use the query tool instead")
		return out, d.finish(rec, start, err)
	}

	params, err := normalizeParams(in.Params)
	if err != nil {
		return out, d.finish(rec, start, err)
	}

	tx, err := entry.Driver.Begin(ctx)
	if err != nil {
		return out, d.finish(rec, start, err)
	}

	cap := entry.Config.Policy.MaxAffectedRows

	out, err = d.execInTx(ctx, entry, tx, stmt, params, in.Database)
	if err != nil {
		_ = tx.Rollback(ctx)
		return out, d.finish(rec, start, err)
	}

	// The row cap can only be checked after the fact, so the statement runs in
	// a transaction that is rolled back when it touched too many rows. A
	// half-applied over-broad UPDATE would otherwise be unrecoverable.
	if cap > 0 && out.RowsAffected > int64(cap) {
		_ = tx.Rollback(ctx)

		affected := out.RowsAffected
		out.RowsAffected = 0
		rec.RowsAffected = &affected

		return out, d.finish(rec, start, denial(
			"cap_exceeded",
			"refused: this statement would affect %d rows, above the max_affected_rows cap of %d "+
				"for database %q. It was rolled back and nothing changed; add a narrower WHERE clause, "+
				"or raise max_affected_rows in the config if this is intended",
			affected, cap, in.Database,
		))
	}

	if err := tx.Commit(ctx); err != nil {
		return out, d.finish(rec, start, fmt.Errorf("commit: %w", err))
	}

	out.DurationMS = float64(time.Since(start).Microseconds()) / 1000
	rec.RowsAffected = &out.RowsAffected
	rec.Rows = out.RowCount
	rec.SQL = fmt.Sprintf("%s /* committed */", in.SQL)

	return out, d.finish(rec, start, nil)
}

func (d *Deps) runTransaction(ctx context.Context, in transactionInput) (executeOutput, error) {
	var out executeOutput

	entry, err := d.entry(in.Database, "write", writeToolsEnabled)
	if err != nil {
		return out, err
	}

	start := time.Now()

	// Audit the whole batch as one logical call, with every statement listed.
	rec := audit.Record{Tool: "run_in_transaction", Database: in.Database}
	for _, s := range in.Statements {
		rec.SQL += s.SQL + ";\n"
	}

	if len(in.Statements) == 0 {
		return out, d.finish(rec, start, denial("empty", "no statements were supplied"))
	}
	if len(in.Statements) > maxStatementsPerTransaction {
		return out, d.finish(rec, start, denial(
			"not_allowed", "at most %d statements are allowed per transaction, got %d",
			maxStatementsPerTransaction, len(in.Statements),
		))
	}

	// Check every statement before running any of them, so a batch containing a
	// refusal never touches the database.
	type prepared struct {
		stmt   guard.Statement
		params []any
	}

	preparedStatements := make([]prepared, 0, len(in.Statements))

	for i, s := range in.Statements {
		stmt, err := guard.Check(s.SQL, entry.Config.Policy, in.Database)
		if err != nil {
			return out, d.finish(rec, start, fmt.Errorf("statement %d: %w", i+1, err))
		}

		params, err := normalizeParams(s.Params)
		if err != nil {
			return out, d.finish(rec, start, fmt.Errorf("statement %d: %w", i+1, err))
		}

		preparedStatements = append(preparedStatements, prepared{stmt: stmt, params: params})
	}

	tx, err := entry.Driver.Begin(ctx)
	if err != nil {
		return out, d.finish(rec, start, err)
	}

	cap := entry.Config.Policy.MaxAffectedRows

	for i, p := range preparedStatements {
		result, err := d.execInTx(ctx, entry, tx, p.stmt, p.params, in.Database)
		if err != nil {
			_ = tx.Rollback(ctx)
			return out, d.finish(rec, start, fmt.Errorf("statement %d: %w", i+1, err))
		}

		out.RowsAffected += result.RowsAffected

		if cap > 0 && out.RowsAffected > int64(cap) {
			_ = tx.Rollback(ctx)

			over := out.RowsAffected
			out.RowsAffected = 0

			return out, d.finish(rec, start, denial(
				"cap_exceeded",
				"statement %d pushed the transaction to %d affected rows, above the "+
					"max_affected_rows cap of %d for database %q; the whole transaction was rolled back "+
					"and nothing changed",
				i+1, over, cap, in.Database,
			))
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return out, d.finish(rec, start, fmt.Errorf("commit: %w", err))
	}

	out.Database = in.Database
	out.AppliedCap = cap
	out.DurationMS = float64(time.Since(start).Microseconds()) / 1000

	rec.RowsAffected = &out.RowsAffected

	return out, d.finish(rec, start, nil)
}

// execInTx runs one already-authorised statement inside tx. The affected-row cap
// is enforced by the caller, which rolls the transaction back when it is broken.
func (d *Deps) execInTx(
	ctx context.Context,
	entry *database.Entry,
	tx database.Tx,
	stmt guard.Statement,
	params []any,
	databaseName string,
) (executeOutput, error) {
	out := executeOutput{Database: databaseName, AppliedCap: entry.Config.Policy.MaxAffectedRows}

	// A statement with RETURNING yields rows, so it must be run as a query.
	if stmt.Returning {
		result, err := tx.Query(ctx, stmt.SQL, params...)
		if err != nil {
			return out, err
		}

		mask := redact.Plan(entry.Config.Policy.RedactColumns, "", result.Columns)
		redact.Apply(mask, result.Rows)

		out.Rows = toObjects(result.Columns, result.Rows)
		out.RowCount = len(result.Rows)
		out.RowsAffected = int64(len(result.Rows))

		return out, nil
	}

	result, err := tx.Exec(ctx, stmt.SQL, params...)
	if err != nil {
		return out, err
	}

	out.RowsAffected = result.RowsAffected

	return out, nil
}
