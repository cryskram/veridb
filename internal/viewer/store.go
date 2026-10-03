// Package viewer serves the VeriDB audit trail: it tails the JSONL file that
// veridb writes, loads it into SQLite, and exposes a small web UI and JSON API.
//
// Ingest is incremental and idempotent per source file: the byte offset of the
// last complete line is persisted, so restarting the viewer resumes rather than
// re-reading. A file that shrank (rotation, truncation) is re-read from the
// start.
package viewer

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/cryskram/veridb/internal/audit"

	_ "modernc.org/sqlite"
)

// Store is the SQLite-backed audit store.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS audit_log (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  ts            TEXT    NOT NULL,
  ts_unix       REAL    NOT NULL,
  tool          TEXT    NOT NULL,
  database_name TEXT,
  status        TEXT    NOT NULL,
  sql_text      TEXT,
  params        TEXT,
  rows_count    INTEGER,
  rows_affected INTEGER,
  truncated     INTEGER,
  duration_ms   REAL,
  reason        TEXT,
  error         TEXT,
  client        TEXT
);

CREATE INDEX IF NOT EXISTS idx_audit_ts     ON audit_log(ts_unix DESC);
CREATE INDEX IF NOT EXISTS idx_audit_db     ON audit_log(database_name);
CREATE INDEX IF NOT EXISTS idx_audit_tool   ON audit_log(tool);
CREATE INDEX IF NOT EXISTS idx_audit_status ON audit_log(status);

CREATE TABLE IF NOT EXISTS ingest_state (
  source     TEXT PRIMARY KEY,
  offset     INTEGER NOT NULL,
  updated_at TEXT NOT NULL
);
`

// OpenStore opens or creates the SQLite store at path.
func OpenStore(path string) (*Store, error) {
	// WAL keeps readers (HTTP requests) from blocking the ingest writer, and
	// busy_timeout avoids spurious "database is locked" errors.
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(on)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open audit store %s: %w", path, err)
	}

	// modernc's SQLite allows one writer; serialising all access removes lock
	// contention entirely and audit volumes are modest.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping audit store %s: %w", path, err)
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create audit schema: %w", err)
	}

	return &Store{db: db}, nil
}

// Close releases the store.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for read queries.
func (s *Store) DB() *sql.DB { return s.db }

// IngestResult reports what one ingest pass did.
type IngestResult struct {
	// Inserted is how many records were loaded.
	Inserted int
	// Skipped is how many lines could not be parsed. A truncated or corrupt
	// line is skipped rather than blocking the rest of the trail.
	Skipped int
	// Bytes is how many bytes were consumed.
	Bytes int64
}

// Ingest reads any complete lines appended to source since the last call and
// loads them. It returns how many records were inserted.
func (s *Store) Ingest(ctx context.Context, source string) (IngestResult, error) {
	var result IngestResult

	file, err := os.Open(source)
	if err != nil {
		if os.IsNotExist(err) {
			// veridb may not have written anything yet.
			return result, nil
		}
		return result, fmt.Errorf("open audit source %s: %w", source, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return result, fmt.Errorf("stat audit source %s: %w", source, err)
	}

	offset, err := s.offset(ctx, source)
	if err != nil {
		return result, err
	}

	if offset > info.Size() {
		// The file was rotated or truncated; start over.
		offset = 0
	}

	if offset == info.Size() {
		return result, nil
	}

	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return result, fmt.Errorf("seek audit source %s: %w", source, err)
	}

	records, decoded, err := decodeLines(bufio.NewReader(file))
	if err != nil {
		return result, err
	}

	result.Skipped = decoded.Skipped
	result.Bytes = decoded.Bytes

	if len(records) > 0 {
		if err := s.insert(ctx, records); err != nil {
			// Do not advance the offset: the next pass retries the same lines.
			return result, err
		}
	}

	if err := s.setOffset(ctx, source, offset+decoded.Bytes); err != nil {
		return result, err
	}

	result.Inserted = len(records)

	return result, nil
}

// decodedLines is the outcome of parsing a chunk of the trail.
type decodedLines struct {
	Records []audit.Record
	// Bytes counts whole lines, including the skipped ones, so the offset always
	// moves forward over everything that was examined.
	Bytes   int64
	Skipped int
}

// decodeLines parses whole lines only. A trailing fragment without a newline is
// left unconsumed so a partially written record is retried next pass instead of
// being stored truncated.
func decodeLines(r *bufio.Reader) ([]audit.Record, decodedLines, error) {
	out := decodedLines{}

	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				return out.Records, out, nil
			}
			return out.Records, out, fmt.Errorf("read audit source: %w", err)
		}

		out.Bytes += int64(len(line))

		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}

		var rec audit.Record
		if err := json.Unmarshal(trimmed, &rec); err != nil {
			out.Skipped++
			continue
		}

		out.Records = append(out.Records, rec)
	}
}

func (s *Store) insert(ctx context.Context, records []audit.Record) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin ingest: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO audit_log (
			ts, ts_unix, tool, database_name, status, sql_text, params,
			rows_count, rows_affected, truncated, duration_ms, reason, error, client
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("prepare ingest: %w", err)
	}
	defer stmt.Close()

	for _, rec := range records {
		ts := rec.Time
		if ts.IsZero() {
			ts = time.Now().UTC()
		}

		var params any
		if len(rec.Params) > 0 {
			if encoded, err := json.Marshal(rec.Params); err == nil {
				params = string(encoded)
			}
		}

		var rowsAffected any
		if rec.RowsAffected != nil {
			rowsAffected = *rec.RowsAffected
		}

		if _, err := stmt.ExecContext(ctx,
			ts.UTC().Format(time.RFC3339Nano),
			float64(ts.UnixNano())/1e9,
			rec.Tool,
			nullable(rec.Database),
			string(rec.Status),
			nullable(rec.SQL),
			params,
			rec.Rows,
			rowsAffected,
			boolToInt(rec.Truncated),
			rec.DurationMS,
			nullable(rec.Reason),
			nullable(rec.Error),
			nullable(rec.Client),
		); err != nil {
			return fmt.Errorf("insert audit record: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit ingest: %w", err)
	}

	return nil
}

func (s *Store) offset(ctx context.Context, source string) (int64, error) {
	var offset int64

	err := s.db.QueryRowContext(ctx,
		`SELECT offset FROM ingest_state WHERE source = ?`, source,
	).Scan(&offset)

	switch {
	case err == sql.ErrNoRows:
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("read ingest offset: %w", err)
	}

	return offset, nil
}

func (s *Store) setOffset(ctx context.Context, source string, offset int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ingest_state (source, offset, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(source) DO UPDATE SET offset = excluded.offset, updated_at = excluded.updated_at
	`, source, offset, time.Now().UTC().Format(time.RFC3339Nano))

	if err != nil {
		return fmt.Errorf("store ingest offset: %w", err)
	}

	return nil
}

// Record is one row as returned by a query.
type Record struct {
	ID           int64   `json:"id"`
	Time         string  `json:"ts"`
	Tool         string  `json:"tool"`
	Database     string  `json:"database,omitempty"`
	Status       string  `json:"status"`
	SQL          string  `json:"sql,omitempty"`
	Params       []any   `json:"params,omitempty"`
	Rows         int     `json:"rows,omitempty"`
	RowsAffected *int64  `json:"rows_affected,omitempty"`
	Truncated    bool    `json:"truncated,omitempty"`
	DurationMS   float64 `json:"duration_ms,omitempty"`
	Reason       string  `json:"reason,omitempty"`
	Error        string  `json:"error,omitempty"`
	Client       string  `json:"client,omitempty"`
}

// Filter narrows a query over the audit trail.
type Filter struct {
	Database string
	Tool     string
	Status   string
	// Search matches the SQL text and the error message.
	Search string
	Limit  int
	Offset int
}

const recordColumns = `
	id, ts, tool, COALESCE(database_name, ''), status, COALESCE(sql_text, ''),
	params, COALESCE(rows_count, 0), rows_affected, COALESCE(truncated, 0),
	COALESCE(duration_ms, 0), COALESCE(reason, ''), COALESCE(error, ''), COALESCE(client, '')
`

// buildWhere returns the WHERE clause and bind arguments for a filter.
func buildWhere(f Filter) (string, []any) {
	clause := " WHERE 1 = 1"
	var args []any

	if f.Database != "" {
		clause += " AND database_name = ?"
		args = append(args, f.Database)
	}
	if f.Tool != "" {
		clause += " AND tool = ?"
		args = append(args, f.Tool)
	}
	if f.Status != "" {
		clause += " AND status = ?"
		args = append(args, f.Status)
	}
	if f.Search != "" {
		clause += " AND (sql_text LIKE ? OR error LIKE ?)"
		like := "%" + f.Search + "%"
		args = append(args, like, like)
	}

	return clause, args
}

// Query returns audit records matching f, newest first.
func (s *Store) Query(ctx context.Context, f Filter) ([]Record, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	where, args := buildWhere(f)
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+recordColumns+` FROM audit_log`+where+
			` ORDER BY ts_unix DESC, id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("query audit trail: %w", err)
	}
	defer rows.Close()

	records := make([]Record, 0, limit)

	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audit trail: %w", err)
	}

	return records, nil
}

// Count returns how many records match f, for pagination.
func (s *Store) Count(ctx context.Context, f Filter) (int, error) {
	where, args := buildWhere(f)

	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_log`+where, args...,
	).Scan(&total); err != nil {
		return 0, fmt.Errorf("count audit trail: %w", err)
	}

	return total, nil
}

func scanRecord(rows *sql.Rows) (Record, error) {
	var (
		rec          Record
		params       sql.NullString
		rowsAffected sql.NullInt64
		truncated    int
	)

	err := rows.Scan(
		&rec.ID, &rec.Time, &rec.Tool, &rec.Database, &rec.Status, &rec.SQL,
		&params, &rec.Rows, &rowsAffected, &truncated,
		&rec.DurationMS, &rec.Reason, &rec.Error, &rec.Client,
	)
	if err != nil {
		return rec, fmt.Errorf("scan audit record: %w", err)
	}

	if params.Valid && params.String != "" {
		_ = json.Unmarshal([]byte(params.String), &rec.Params)
	}
	if rowsAffected.Valid {
		v := rowsAffected.Int64
		rec.RowsAffected = &v
	}
	rec.Truncated = truncated != 0

	return rec, nil
}

// StatusCount is a count grouped by a value.
type StatusCount struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// Stats summarizes the trail for the dashboard.
type Stats struct {
	Total        int           `json:"total"`
	ByStatus     []StatusCount `json:"by_status"`
	ByDatabase   []StatusCount `json:"by_database"`
	ByTool       []StatusCount `json:"by_tool"`
	Last24h      int           `json:"last_24h"`
	DeniedLast24 int           `json:"denied_last_24h"`
	OldestRecord string        `json:"oldest_record,omitempty"`
	NewestRecord string        `json:"newest_record,omitempty"`
}

// Stats aggregates the trail.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var stats Stats

	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log`).Scan(&stats.Total); err != nil {
		return stats, fmt.Errorf("count audit records: %w", err)
	}

	cutoff := float64(time.Now().Add(-24*time.Hour).UnixNano()) / 1e9

	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE ts_unix >= ?`, cutoff,
	).Scan(&stats.Last24h); err != nil {
		return stats, fmt.Errorf("count recent audit records: %w", err)
	}

	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE ts_unix >= ? AND status = 'denied'`, cutoff,
	).Scan(&stats.DeniedLast24); err != nil {
		return stats, fmt.Errorf("count recent denials: %w", err)
	}

	var oldest, newest sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT MIN(ts), MAX(ts) FROM audit_log`,
	).Scan(&oldest, &newest); err != nil {
		return stats, fmt.Errorf("read audit range: %w", err)
	}
	stats.OldestRecord = oldest.String
	stats.NewestRecord = newest.String

	var err error
	if stats.ByStatus, err = s.grouped(ctx, "status"); err != nil {
		return stats, err
	}
	if stats.ByDatabase, err = s.grouped(ctx, "database_name"); err != nil {
		return stats, err
	}
	if stats.ByTool, err = s.grouped(ctx, "tool"); err != nil {
		return stats, err
	}

	return stats, nil
}

// grouped counts records per column value. The column name is never taken from
// user input, so interpolating it is safe.
func (s *Store) grouped(ctx context.Context, column string) ([]StatusCount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(`+column+`, '(none)') AS value, COUNT(*) AS n
		FROM audit_log
		GROUP BY value
		ORDER BY n DESC
		LIMIT 15
	`)
	if err != nil {
		return nil, fmt.Errorf("group audit records by %s: %w", column, err)
	}
	defer rows.Close()

	var out []StatusCount
	for rows.Next() {
		var sc StatusCount
		if err := rows.Scan(&sc.Value, &sc.Count); err != nil {
			return nil, fmt.Errorf("scan grouped audit records: %w", err)
		}
		out = append(out, sc)
	}

	return out, rows.Err()
}

// DistinctDatabases lists the databases seen in the trail, for the filter UI.
func (s *Store) DistinctDatabases(ctx context.Context) ([]string, error) {
	return s.distinct(ctx, "database_name")
}

// DistinctTools lists the tools seen in the trail.
func (s *Store) DistinctTools(ctx context.Context) ([]string, error) {
	return s.distinct(ctx, "tool")
}

func (s *Store) distinct(ctx context.Context, column string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT `+column+` FROM audit_log
		WHERE `+column+` IS NOT NULL AND `+column+` <> ''
		ORDER BY 1
	`)
	if err != nil {
		return nil, fmt.Errorf("list distinct %s: %w", column, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan distinct %s: %w", column, err)
		}
		out = append(out, v)
	}

	return out, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// FormatDuration renders a duration for display.
func FormatDuration(ms float64) string {
	switch {
	case ms <= 0:
		return "-"
	case ms < 1:
		return strconv.FormatFloat(ms, 'f', 2, 64) + "ms"
	case ms < 1000:
		return strconv.FormatFloat(ms, 'f', 1, 64) + "ms"
	default:
		return strconv.FormatFloat(ms/1000, 'f', 2, 64) + "s"
	}
}
