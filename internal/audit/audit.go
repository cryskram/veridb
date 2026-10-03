// Package audit records every tool call VeriDB serves.
//
// Records are written to one or more sinks. The stderr sink is meant to be read
// live by a human or an MCP client; the file sink writes JSON Lines, which is an
// append-only and crash-safe format that an external viewer can tail and load
// into a queryable store.
package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cryskram/veridb/internal/config"
)

// Status classifies the outcome of a call.
type Status string

const (
	// StatusOK means the call completed.
	StatusOK Status = "ok"
	// StatusDenied means the policy refused the call before it ran.
	StatusDenied Status = "denied"
	// StatusError means the call ran and failed, or could not run.
	StatusError Status = "error"
)

// Record is one audited tool call.
type Record struct {
	Time     time.Time `json:"ts"`
	Tool     string    `json:"tool"`
	Database string    `json:"database,omitempty"`
	Status   Status    `json:"status"`

	// SQL is the statement as submitted, truncated to max_value_len.
	SQL string `json:"sql,omitempty"`
	// Params are bind parameters, present only when include_params is on.
	Params []any `json:"params,omitempty"`

	Rows         int     `json:"rows,omitempty"`
	RowsAffected *int64  `json:"rows_affected,omitempty"`
	Truncated    bool    `json:"truncated,omitempty"`
	DurationMS   float64 `json:"duration_ms,omitempty"`

	// Reason is a stable denial code such as "read_only" or "missing_where".
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
	// Client identifies the MCP client when the transport exposes one.
	Client string `json:"client,omitempty"`
}

type sink interface {
	write(rec Record) error
	close() error
}

// Logger fans records out to the configured sinks. It is safe for concurrent
// use and never blocks a tool call on a slow sink: write failures are reported
// on stderr and otherwise ignored, because an audit failure must not take the
// database tooling down with it.
type Logger struct {
	cfg   config.ResolvedAudit
	sinks []sink

	mu       sync.Mutex
	failures int
}

// New builds a Logger for the given configuration. It returns a disabled Logger
// rather than an error when auditing is off.
func New(cfg config.ResolvedAudit, stderr io.Writer) (*Logger, error) {
	logger := &Logger{cfg: cfg}
	if !cfg.Enabled {
		return logger, nil
	}

	if stderr == nil {
		stderr = os.Stderr
	}

	for _, name := range cfg.Sinks {
		switch name {
		case "stderr":
			logger.sinks = append(logger.sinks, &stderrSink{w: stderr})
		case "file":
			s, err := newFileSink(cfg.File)
			if err != nil {
				// Close whatever was already opened so a partial setup does not
				// leak descriptors.
				logger.closeSinks()
				return nil, err
			}
			logger.sinks = append(logger.sinks, s)
		}
	}

	return logger, nil
}

// Enabled reports whether any sink is attached.
func (l *Logger) Enabled() bool { return l != nil && len(l.sinks) > 0 }

// Log records one call.
func (l *Logger) Log(rec Record) {
	if !l.Enabled() {
		return
	}

	if rec.Time.IsZero() {
		rec.Time = time.Now().UTC()
	}
	rec = l.sanitize(rec)

	for _, s := range l.sinks {
		if err := s.write(rec); err != nil {
			l.noteFailure(err)
		}
	}
}

// Close flushes and closes every sink.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	return l.closeSinks()
}

func (l *Logger) closeSinks() error {
	var firstErr error
	for _, s := range l.sinks {
		if err := s.close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	l.sinks = nil
	return firstErr
}

// sanitize applies the include_* switches and the value length cap, so sinks
// receive exactly what the operator allowed to be stored.
func (l *Logger) sanitize(rec Record) Record {
	if !l.cfg.IncludeSQL {
		rec.SQL = ""
	} else {
		rec.SQL = truncate(rec.SQL, l.cfg.MaxValueLen)
	}

	if !l.cfg.IncludeParams {
		rec.Params = nil
	} else {
		params := make([]any, len(rec.Params))
		for i, p := range rec.Params {
			params[i] = truncateValue(p, l.cfg.MaxValueLen)
		}
		rec.Params = params
	}

	rec.Error = truncate(rec.Error, l.cfg.MaxValueLen)

	return rec
}

func (l *Logger) noteFailure(err error) {
	l.mu.Lock()
	l.failures++
	n := l.failures
	l.mu.Unlock()

	// Complain once, then stay quiet: an unwritable audit sink must not flood
	// the MCP client's stderr on every call.
	if n <= 3 {
		fmt.Fprintf(os.Stderr, "veridb: audit write failed (%d): %v\n", n, err)
	}
}

// stderrSink writes one readable line per call.
type stderrSink struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *stderrSink) write(rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var detail string

	switch {
	case rec.Error != "":
		detail = " error=" + quote(rec.Error)
	case rec.RowsAffected != nil:
		detail = fmt.Sprintf(" affected=%d", *rec.RowsAffected)
	case rec.Rows > 0:
		detail = fmt.Sprintf(" rows=%d", rec.Rows)
	}

	truncated := ""
	if rec.Truncated {
		truncated = " truncated"
	}

	_, err := fmt.Fprintf(
		s.w,
		"%s %-9s db=%-16s tool=%-17s%s%s %.1fms%s\n",
		rec.Time.Format(time.RFC3339),
		rec.Status,
		dash(rec.Database),
		rec.Tool,
		detail,
		truncated,
		rec.DurationMS,
		sqlSuffix(rec.SQL),
	)

	return err
}

func (s *stderrSink) close() error { return nil }

// fileSink appends JSON Lines.
type fileSink struct {
	mu   sync.Mutex
	file *os.File
}

func newFileSink(path string) (*fileSink, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create audit directory %s: %w", dir, err)
		}
	}

	// O_RDWR (not O_WRONLY) so an interrupted previous write can be detected by
	// reading the final byte. VeriDB owns this file.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open audit file %s: %w", path, err)
	}

	// If a previous process died mid-write the file ends without a newline.
	// Appending straight onto that fragment would corrupt this record as well,
	// so close the dangling line first. The fragment remains as its own
	// unparseable line, which the viewer skips.
	if err := healDanglingLine(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("repair audit file %s: %w", path, err)
	}

	return &fileSink{file: f}, nil
}

// healDanglingLine appends a newline when the file is non-empty and does not
// already end with one.
func healDanglingLine(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return nil
	}

	if _, err := f.Seek(-1, io.SeekEnd); err != nil {
		return err
	}

	var last [1]byte
	n, err := f.Read(last[:])
	if err != nil && err != io.EOF {
		return err
	}
	if n == 0 || last[0] == '\n' {
		return nil
	}

	if _, err := f.Write([]byte{'\n'}); err != nil {
		return err
	}

	// Writes are O_APPEND so they always land at the end; only reads use the
	// offset, and they start from the end on the next open.
	return nil
}

func (s *fileSink) write(rec Record) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal audit record: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// A single Write call of a complete line keeps concurrent appends from
	// interleaving, which is what makes the file safe to tail.
	if _, err := s.file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("append audit record: %w", err)
	}

	return nil
}

func (s *fileSink) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.file == nil {
		return nil
	}

	err := s.file.Close()
	s.file = nil
	return err
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "...[truncated]"
}

func truncateValue(v any, max int) any {
	if s, ok := v.(string); ok {
		return truncate(s, max)
	}
	return v
}

// quote renders a string for a single-line log field.
func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Sprintf("%q", s)
	}
	return string(b)
}

func sqlSuffix(sql string) string {
	if sql == "" {
		return ""
	}
	return " sql=" + quote(sql)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
