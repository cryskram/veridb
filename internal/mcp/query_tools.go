package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/cryskram/veridb/internal/audit"
	"github.com/cryskram/veridb/internal/config"
	"github.com/cryskram/veridb/internal/database"
	"github.com/cryskram/veridb/internal/guard"
	"github.com/cryskram/veridb/internal/redact"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func queryToolsEnabled(s config.Services) bool   { return s.Query }
func explainToolsEnabled(s config.Services) bool { return s.Explain }
func sampleToolsEnabled(s config.Services) bool  { return s.Sample }
func writeToolsEnabled(s config.Services) bool   { return s.Write }

type queryInput struct {
	Database string `json:"database" jsonschema:"database name from list_databases"`
	SQL      string `json:"sql" jsonschema:"a single read-only statement; use $1, $2 for bind parameters instead of embedding values"`
	// Params are positional bind parameters for $1, $2, ...
	Params []any `json:"params,omitempty" jsonschema:"positional values for $1, $2, ... in the statement"`
	// Limit asks for fewer rows than the database cap. It can only lower it.
	Limit int `json:"limit,omitempty" jsonschema:"maximum rows to return; cannot exceed the database cap"`
}

// rowObject is a single result row keyed by column name.
type rowObject map[string]any

type queryOutput struct {
	Database string      `json:"database"`
	Columns  []string    `json:"columns"`
	Rows     []rowObject `json:"rows"`
	RowCount int         `json:"row_count"`
	// Truncated means more rows matched than the applied limit returned.
	Truncated bool `json:"truncated"`
	// AppliedLimit is the row cap that was actually used.
	AppliedLimit int `json:"applied_limit"`
	// RedactedColumns names columns whose values were masked by policy.
	RedactedColumns []string `json:"redacted_columns,omitempty"`
	// Notice carries advisories, for example that a requested limit was lowered.
	Notice string `json:"notice,omitempty"`
	// DurationMS is the server-side execution time.
	DurationMS float64 `json:"duration_ms"`
}

func registerQueryTools(server *mcp.Server, deps *Deps) {
	if deps.enabledForAny(queryToolsEnabled) {
		mcp.AddTool(
			server,
			&mcp.Tool{
				Name: "query",
				Description: "Run a single read-only SQL statement and return rows as objects. " +
					"Parameters are positional ($1, $2, ...) and passed in `params`, which is safer " +
					"than embedding literals. Results are capped by the database's max_rows: when " +
					"`truncated` is true, narrow the query instead of assuming you saw everything. " +
					"Use explain_query to check a plan before running something expensive.",
			},
			func(ctx context.Context, _ *mcp.CallToolRequest, in queryInput) (*mcp.CallToolResult, queryOutput, error) {
				out, err := deps.runQuery(ctx, in)
				return nil, out, err
			},
		)
	}

	if deps.enabledForAny(explainToolsEnabled) {
		mcp.AddTool(
			server,
			&mcp.Tool{
				Name: "explain_query",
				Description: "Return the PostgreSQL query plan for a statement without running it. " +
					"Set analyze=true to actually execute it and get real row counts and timings, " +
					"which requires the write service when the statement is not a read.",
			},
			func(ctx context.Context, _ *mcp.CallToolRequest, in explainInput) (*mcp.CallToolResult, explainOutput, error) {
				out, err := deps.runExplain(ctx, in)
				return nil, out, err
			},
		)
	}

	if deps.enabledForAny(sampleToolsEnabled) {
		mcp.AddTool(
			server,
			&mcp.Tool{
				Name: "sample_rows",
				Description: "Return a few rows from one table so you can see real shapes and values " +
					"before writing a query. Prefer this over SELECT * on an unknown table.",
			},
			func(ctx context.Context, _ *mcp.CallToolRequest, in sampleInput) (*mcp.CallToolResult, queryOutput, error) {
				out, err := deps.runSample(ctx, in)
				return nil, out, err
			},
		)
	}
}

type explainInput struct {
	Database string `json:"database" jsonschema:"database name from list_databases"`
	SQL      string `json:"sql" jsonschema:"the statement to plan"`
	Params   []any  `json:"params,omitempty" jsonschema:"positional values for $1, $2, ..."`
	Analyze  bool   `json:"analyze,omitempty" jsonschema:"execute the statement to collect real timings; default false"`
}

type explainOutput struct {
	Database string   `json:"database"`
	Analyze  bool     `json:"analyze"`
	Plan     []string `json:"plan"`
	Notice   string   `json:"notice,omitempty"`
}

type sampleInput struct {
	Database string `json:"database" jsonschema:"database name from list_databases"`
	Schema   string `json:"schema" jsonschema:"schema containing the table, usually public"`
	Table    string `json:"table" jsonschema:"table to sample"`
	Limit    int    `json:"limit,omitempty" jsonschema:"number of rows, default 10; cannot exceed the database cap"`
}

// runQuery is the whole read path: authorise, guard, bound, execute, redact,
// audit.
func (d *Deps) runQuery(ctx context.Context, in queryInput) (queryOutput, error) {
	var out queryOutput

	entry, err := d.entry(in.Database, "query", queryToolsEnabled)
	if err != nil {
		return out, err
	}

	pol := entry.Config.Policy
	start := time.Now()
	rec := audit.Record{Tool: "query", Database: in.Database, SQL: in.SQL, Params: in.Params}

	stmt, err := guard.Check(in.SQL, pol, in.Database)
	if err != nil {
		return out, d.finish(rec, start, err)
	}

	if stmt.Kind != guard.KindRead {
		err := denial(
			"not_allowed",
			"the query tool only runs read statements; this is a %s. Use the execute tool instead",
			stmt.Kind,
		)
		return out, d.finish(rec, start, err)
	}

	limit, notice := effectiveLimit(pol.MaxRows, in.Limit)

	params, err := normalizeParams(in.Params)
	if err != nil {
		return out, d.finish(rec, start, err)
	}

	sql, wrapped := guard.ApplyReadLimit(stmt, limit)

	result, err := d.withTimeout(ctx, entry, func(ctx context.Context) (*database.ResultSet, error) {
		return entry.Driver.Query(ctx, sql, params...)
	})
	if err != nil {
		return out, d.finish(rec, start, err)
	}

	// A wrapped query fetches limit+1 rows so truncation is provable rather
	// than guessed.
	if len(result.Rows) > limit {
		result.Rows = result.Rows[:limit]
		result.Truncated = true
	} else if !wrapped {
		result.Truncated = result.RowCount > limit
		result.Rows = firstN(result.Rows, limit)
	}

	mask := redact.Plan(pol.RedactColumns, "", result.Columns)
	redacted := redact.Apply(mask, result.Rows)

	out = queryOutput{
		Database:        in.Database,
		Columns:         result.Columns,
		Rows:            toObjects(result.Columns, result.Rows),
		RowCount:        len(result.Rows),
		Truncated:       result.Truncated,
		AppliedLimit:    limit,
		RedactedColumns: redact.Names(mask, result.Columns),
		Notice:          notice,
		DurationMS:      float64(result.Duration.Microseconds()) / 1000,
	}

	rec.Rows = out.RowCount
	rec.Truncated = out.Truncated

	if redacted > 0 {
		rec.SQL = fmt.Sprintf("%s /* %d value(s) redacted */", in.SQL, redacted)
	}

	return out, d.finish(rec, start, nil)
}

func (d *Deps) runExplain(ctx context.Context, in explainInput) (explainOutput, error) {
	var out explainOutput

	entry, err := d.entry(in.Database, "explain", explainToolsEnabled)
	if err != nil {
		return out, err
	}

	pol := entry.Config.Policy
	start := time.Now()
	rec := audit.Record{Tool: "explain_query", Database: in.Database, SQL: in.SQL, Params: in.Params}

	stmt, err := guard.Check(in.SQL, pol, in.Database)
	if err != nil {
		return out, d.finish(rec, start, err)
	}

	// EXPLAIN ANALYZE executes the statement. Explaining a mutation therefore
	// needs the write service, otherwise an agent could run DML through the
	// explain tool.
	if stmt.Kind != guard.KindRead && !entry.Config.Services.Write {
		err := denial(
			"not_allowed",
			"explaining a %s with analyze executes it, so the write service must be enabled "+
				"for database %q; without it only read statements can be explained",
			stmt.Kind, in.Database,
		)
		return out, d.finish(rec, start, err)
	}

	params, err := normalizeParams(in.Params)
	if err != nil {
		return out, d.finish(rec, start, err)
	}

	// The plan is requested in text because a text plan is far easier to read
	// and reason about than the nested JSON form.
	options := "COSTS, VERBOSE"
	if in.Analyze {
		options += ", ANALYZE, BUFFERS, TIMING"
	}

	result, err := d.withTimeout(ctx, entry, func(ctx context.Context) (*database.ResultSet, error) {
		return entry.Driver.Query(ctx, fmt.Sprintf("EXPLAIN (%s) %s", options, stmt.SQL), params...)
	})
	if err != nil {
		return out, d.finish(rec, start, err)
	}

	plan := make([]string, 0, len(result.Rows))
	for _, row := range result.Rows {
		if len(row) == 0 {
			continue
		}
		if line, ok := row[0].(string); ok {
			plan = append(plan, line)
		}
	}

	out = explainOutput{
		Database: in.Database,
		Analyze:  in.Analyze,
		Plan:     plan,
	}

	if in.Analyze {
		out.Notice = "the statement was executed to collect real timings"
		rec.RowsAffected = nil
	}

	rec.Rows = len(plan)

	return out, d.finish(rec, start, nil)
}

func (d *Deps) runSample(ctx context.Context, in sampleInput) (queryOutput, error) {
	var out queryOutput

	entry, err := d.entry(in.Database, "sample", sampleToolsEnabled)
	if err != nil {
		return out, err
	}

	if in.Schema == "" || in.Table == "" {
		return out, denial("identifier", "schema and table are both required")
	}

	pol := entry.Config.Policy
	limit, notice := effectiveLimit(pol.MaxRows, in.Limit)
	if in.Limit == 0 {
		// A sample is meant to be small; 10 is a better default than max_rows.
		limit = min(limit, 10)
	}

	start := time.Now()

	schemaIdent, err := guard.QuoteIdent(in.Schema)
	if err != nil {
		return out, d.finish(audit.Record{Tool: "sample_rows", Database: in.Database}, start, err)
	}
	tableIdent, err := guard.QuoteIdent(in.Table)
	if err != nil {
		return out, d.finish(audit.Record{Tool: "sample_rows", Database: in.Database}, start, err)
	}

	sql := fmt.Sprintf("SELECT * FROM %s.%s LIMIT %d", schemaIdent, tableIdent, limit)
	rec := audit.Record{Tool: "sample_rows", Database: in.Database, SQL: sql}

	// Run the generated statement through the guard so deny_tables and schema
	// rules apply to it exactly as they would to a hand-written query.
	stmt, err := guard.Check(sql, pol, in.Database)
	if err != nil {
		return out, d.finish(rec, start, err)
	}

	result, err := d.withTimeout(ctx, entry, func(ctx context.Context) (*database.ResultSet, error) {
		return entry.Driver.Query(ctx, stmt.SQL)
	})
	if err != nil {
		return out, d.finish(rec, start, err)
	}

	// The relation is known here, so patterns like "*.pan" can be matched
	// against schema.table.column.
	qualifier := in.Schema + "." + in.Table
	mask := redact.Plan(pol.RedactColumns, qualifier, result.Columns)
	redacted := redact.Apply(mask, result.Rows)

	out = queryOutput{
		Database:        in.Database,
		Columns:         result.Columns,
		Rows:            toObjects(result.Columns, result.Rows),
		RowCount:        len(result.Rows),
		AppliedLimit:    limit,
		RedactedColumns: redact.Names(mask, result.Columns),
		Notice:          notice,
		DurationMS:      float64(result.Duration.Microseconds()) / 1000,
	}

	rec.Rows = out.RowCount
	if redacted > 0 {
		rec.SQL = fmt.Sprintf("%s /* %d value(s) redacted */", sql, redacted)
	}

	return out, d.finish(rec, start, nil)
}

// withTimeout runs fn under a deadline derived from the database's statement
// timeout. The server enforces the same limit, so this only bounds the client
// side wait.
func (d *Deps) withTimeout(
	ctx context.Context,
	entry *database.Entry,
	fn func(context.Context) (*database.ResultSet, error),
) (*database.ResultSet, error) {
	timeout := entry.Config.Policy.StatementTimeout.Std()
	if timeout <= 0 {
		return fn(ctx)
	}

	// A little slack so the server's own timeout is what normally fires, and a
	// client deadline does not pre-empt a query that is about to succeed.
	ctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()

	return fn(ctx)
}

// effectiveLimit clamps a requested limit to the database cap.
func effectiveLimit(maxRows, requested int) (int, string) {
	if maxRows <= 0 {
		// Unlimited by policy. Still bound the response to something sane, and
		// say so, because unlimited results are rarely what an agent wants.
		if requested <= 0 {
			return 1000, "the database has no max_rows configured; the result was capped at 1000 rows"
		}
		return requested, ""
	}

	if requested <= 0 || requested > maxRows {
		if requested > maxRows {
			return maxRows, fmt.Sprintf(
				"requested limit %d exceeds the %d-row cap for this database; the cap was applied",
				requested, maxRows,
			)
		}
		return maxRows, ""
	}

	return requested, ""
}

// normalizeParams converts JSON-decoded values into types pgx handles well.
// encoding/json decodes every number as float64, which would otherwise be sent
// to an integer column and rejected.
func normalizeParams(params []any) ([]any, error) {
	if len(params) == 0 {
		return nil, nil
	}

	out := make([]any, len(params))

	for i, p := range params {
		switch v := p.(type) {
		case nil, bool, string, int64:
			out[i] = v

		case float64:
			out[i] = integralOrFloat(v)

		case float32:
			out[i] = integralOrFloat(float64(v))

		case int:
			out[i] = int64(v)

		case int32:
			out[i] = int64(v)

		case json.Number:
			if n, err := v.Int64(); err == nil {
				out[i] = n
				continue
			}
			f, err := v.Float64()
			if err != nil {
				return nil, denial("syntax", "parameter %d is not a valid number", i+1)
			}
			out[i] = integralOrFloat(f)

		default:
			// Arrays and objects are passed as JSON text so the statement can
			// cast them, for example $1::jsonb.
			encoded, err := json.Marshal(v)
			if err != nil {
				return nil, denial("syntax", "parameter %d could not be encoded: %v", i+1, err)
			}
			out[i] = string(encoded)
		}
	}

	return out, nil
}

// integralOrFloat narrows a JSON number to an integer when it has no fractional
// part. encoding/json decodes every number as float64, and pgx would then send
// it as a float8 parameter, which makes `WHERE id = $1` fail against an integer
// column. A 42 that arrives as 42.0 is almost always meant as an integer.
func integralOrFloat(f float64) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return f
	}
	if f == math.Trunc(f) && f >= -9.007199254740992e15 && f <= 9.007199254740992e15 {
		return int64(f)
	}
	return f
}

// toObjects zips column names onto rows.
func toObjects(columns []string, rows [][]any) []rowObject {
	out := make([]rowObject, 0, len(rows))

	for _, row := range rows {
		obj := make(rowObject, len(columns))
		for i, column := range columns {
			if i < len(row) {
				obj[column] = row[i]
			}
		}
		out = append(out, obj)
	}

	return out
}

func firstN(rows [][]any, n int) [][]any {
	if n <= 0 || len(rows) <= n {
		return rows
	}
	return rows[:n]
}

// denial builds a guard-style refusal so that audit records and error messages
// look the same whether the refusal came from the guard or from a tool rule.
func denial(code, format string, args ...any) error {
	return &guard.Denial{Code: code, Message: fmt.Sprintf(format, args...)}
}

// finish records the outcome of a call and returns err unchanged, so handlers
// can end with `return out, d.finish(rec, start, err)`.
func (d *Deps) finish(rec audit.Record, start time.Time, err error) error {
	rec.DurationMS = float64(time.Since(start).Microseconds()) / 1000
	rec.Status = audit.StatusOK

	if err != nil {
		rec.Status = audit.StatusError
		rec.Error = err.Error()

		var den *guard.Denial
		if errors.As(err, &den) {
			rec.Status = audit.StatusDenied
			rec.Reason = den.Code
		}

		if rec.SQL != "" {
			rec.SQL = truncateSQL(rec.SQL)
		}
	}

	d.Audit.Log(rec)

	return err
}

func truncateSQL(sql string) string {
	const max = 2000
	if len(sql) <= max {
		return sql
	}
	return sql[:max] + "...[truncated]"
}
