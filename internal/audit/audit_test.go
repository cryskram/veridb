package audit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryskram/veridb/internal/config"
)

func testConfig() config.ResolvedAudit {
	return config.ResolvedAudit{
		Enabled:       true,
		Sinks:         []string{"stderr"},
		IncludeSQL:    true,
		IncludeParams: false,
		MaxValueLen:   64,
	}
}

func TestDisabledLoggerDoesNothing(t *testing.T) {
	cfg := testConfig()
	cfg.Enabled = false

	var buf bytes.Buffer

	logger, err := New(cfg, &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if logger.Enabled() {
		t.Error("logger should report disabled")
	}

	logger.Log(Record{Tool: "query", Status: StatusOK, SQL: "SELECT 1"})

	if buf.Len() != 0 {
		t.Errorf("disabled logger wrote output: %q", buf.String())
	}
}

func TestStderrSinkIsReadable(t *testing.T) {
	var buf bytes.Buffer

	logger, err := New(testConfig(), &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	logger.Log(Record{
		Time:       time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Tool:       "query",
		Database:   "app",
		Status:     StatusOK,
		SQL:        "SELECT 1",
		Rows:       5,
		Truncated:  true,
		DurationMS: 12.5,
	})

	line := buf.String()
	for _, want := range []string{"query", "app", "rows=5", "truncated", "12.5ms"} {
		if !strings.Contains(line, want) {
			t.Errorf("stderr line missing %q: %s", want, line)
		}
	}
}

func TestStderrSinkReportsDenialsWithReason(t *testing.T) {
	var buf bytes.Buffer

	logger, _ := New(testConfig(), &buf)
	logger.Log(Record{
		Tool:     "execute",
		Database: "identity",
		Status:   StatusDenied,
		Reason:   "read_only",
		Error:    "refused: database is read-only",
		SQL:      "DELETE FROM x",
	})

	line := buf.String()
	if !strings.Contains(line, "denied") {
		t.Errorf("expected denied status: %s", line)
	}
	if !strings.Contains(line, "read-only") {
		t.Errorf("expected the error text: %s", line)
	}
}

func TestFileSinkWritesJSONLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "audit.jsonl")

	cfg := testConfig()
	cfg.Sinks = []string{"file"}
	cfg.File = path

	logger, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	logger.Log(Record{Tool: "query", Database: "a", Status: StatusOK, SQL: "SELECT 1", Rows: 1})
	logger.Log(Record{Tool: "query", Database: "b", Status: StatusOK, SQL: "SELECT 2", Rows: 2})

	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSONL records, got %d: %s", len(lines), raw)
	}

	for i, line := range lines {
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d is not valid JSON: %v", i, err)
		}
		if rec.Tool != "query" {
			t.Errorf("line %d tool = %q", i, rec.Tool)
		}
		if rec.Time.IsZero() {
			t.Errorf("line %d has no timestamp; the viewer sorts on it", i)
		}
	}
}

// The viewer and any external consumer must be able to rely on one JSON object
// per line, so a SQL statement containing newlines must not break the format.
func TestFileSinkKeepsOneRecordPerLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	cfg := testConfig()
	cfg.Sinks = []string{"file"}
	cfg.File = path
	cfg.MaxValueLen = 4096

	logger, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	logger.Log(Record{
		Tool:   "run_in_transaction",
		Status: StatusOK,
		SQL:    "INSERT INTO t VALUES (1);\nINSERT INTO t VALUES (2);\nSELECT 1\n",
	})
	logger.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if got := strings.Count(string(raw), "\n"); got != 1 {
		t.Errorf("expected exactly one line, found %d newlines: %q", got, raw)
	}
}

func TestSanitizeHonoursIncludeSwitches(t *testing.T) {
	cfg := testConfig()
	cfg.Sinks = []string{"file"}
	cfg.File = filepath.Join(t.TempDir(), "audit.jsonl")
	cfg.IncludeSQL = false
	cfg.IncludeParams = false

	logger, _ := New(cfg, nil)
	logger.Log(Record{
		Tool:   "query",
		Status: StatusOK,
		SQL:    "SELECT secret FROM t",
		Params: []any{"hunter2"},
	})
	logger.Close()

	raw, _ := os.ReadFile(cfg.File)
	if strings.Contains(string(raw), "secret") {
		t.Errorf("include_sql is off but SQL was stored: %s", raw)
	}
	if strings.Contains(string(raw), "hunter2") {
		t.Errorf("include_params is off but params were stored: %s", raw)
	}
}

func TestSanitizeIncludesParamsWhenEnabled(t *testing.T) {
	cfg := testConfig()
	cfg.Sinks = []string{"file"}
	cfg.File = filepath.Join(t.TempDir(), "audit.jsonl")
	cfg.IncludeParams = true

	logger, _ := New(cfg, nil)
	logger.Log(Record{Tool: "query", Status: StatusOK, Params: []any{"hello", int64(3)}})
	logger.Close()

	raw, _ := os.ReadFile(cfg.File)
	if !strings.Contains(string(raw), "hello") {
		t.Errorf("include_params is on but params were dropped: %s", raw)
	}
}

func TestSanitizeTruncatesLongValues(t *testing.T) {
	cfg := testConfig()
	cfg.Sinks = []string{"file"}
	cfg.File = filepath.Join(t.TempDir(), "audit.jsonl")
	cfg.MaxValueLen = 20

	long := strings.Repeat("x", 500)

	logger, _ := New(cfg, nil)
	logger.Log(Record{Tool: "query", Status: StatusOK, SQL: long, Error: long})
	logger.Close()

	raw, _ := os.ReadFile(cfg.File)

	var rec Record
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(rec.SQL) > 40 {
		t.Errorf("SQL was not truncated: %d chars", len(rec.SQL))
	}
	if !strings.HasSuffix(rec.SQL, "...[truncated]") {
		t.Errorf("truncation should be visible, got: %q", rec.SQL)
	}
}

// A process killed mid-write leaves a fragment with no newline. Appending onto
// it would corrupt the next record too, so the sink closes the dangling line
// first.
func TestFileSinkRepairsDanglingLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	if err := os.WriteFile(path, []byte(`{"tool":"query","datab`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()
	cfg.Sinks = []string{"file"}
	cfg.File = path

	logger, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	logger.Log(Record{Tool: "execute", Status: StatusOK, SQL: "UPDATE t SET a=1 WHERE id=1"})
	logger.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected the fragment and the new record on separate lines, got %d: %q", len(lines), raw)
	}

	// The fragment stays unparseable, but the new record must be intact.
	var rec Record
	if err := json.Unmarshal([]byte(lines[1]), &rec); err != nil {
		t.Fatalf("the appended record was corrupted: %v (line: %q)", err, lines[1])
	}
	if rec.Tool != "execute" {
		t.Errorf("tool = %q, want execute", rec.Tool)
	}
}

// A file that already ends in a newline must not gain a blank line.
func TestFileSinkDoesNotAddBlankLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	cfg := testConfig()
	cfg.Sinks = []string{"file"}
	cfg.File = path

	for i := 0; i < 3; i++ {
		logger, err := New(cfg, nil)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		logger.Log(Record{Tool: "query", Status: StatusOK, SQL: "SELECT 1"})
		logger.Close()
	}

	raw, _ := os.ReadFile(path)
	if got := strings.Count(string(raw), "\n"); got != 3 {
		t.Errorf("expected 3 lines, found %d newlines: %q", got, raw)
	}
}

func TestLoggerSurvivesUnwritableSink(t *testing.T) {
	cfg := testConfig()
	cfg.Sinks = []string{"file"}
	// A path under a file, not a directory, cannot be created.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.File = filepath.Join(blocker, "audit.jsonl")

	if _, err := New(cfg, nil); err == nil {
		t.Fatal("expected New to fail when the audit file cannot be opened")
	}
}
