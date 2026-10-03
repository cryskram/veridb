package viewer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()

	store, err := OpenStore(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	return store
}

func writeTrail(t *testing.T, lines ...string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "audit.jsonl")

	var content string
	for _, l := range lines {
		content += l + "\n"
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write trail: %v", err)
	}

	return path
}

const okRecord = `{"ts":"2026-01-02T03:04:05Z","tool":"query","database":"app","status":"ok","sql":"SELECT 1","rows":1,"duration_ms":2.5}`
const deniedRecord = `{"ts":"2026-01-02T03:05:05Z","tool":"execute","database":"identity","status":"denied","sql":"DELETE FROM users","reason":"read_only","error":"refused","duration_ms":0.5}`

func TestIngestLoadsRecords(t *testing.T) {
	store := newTestStore(t)
	path := writeTrail(t, okRecord, deniedRecord)

	res, err := store.Ingest(context.Background(), path)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if res.Inserted != 2 {
		t.Errorf("inserted = %d, want 2", res.Inserted)
	}
	if res.Skipped != 0 {
		t.Errorf("skipped = %d, want 0", res.Skipped)
	}

	records, err := store.Query(context.Background(), Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("query returned %d records, want 2", len(records))
	}

	// Newest first.
	if records[0].Tool != "execute" {
		t.Errorf("first record tool = %q, want execute (newest first)", records[0].Tool)
	}
	if records[0].Status != "denied" || records[0].Reason != "read_only" {
		t.Errorf("denied record lost its reason: %+v", records[0])
	}
	if records[1].Rows != 1 {
		t.Errorf("rows = %d, want 1", records[1].Rows)
	}
}

// A second pass must load only what was appended, not re-read the file.
func TestIngestIsIncremental(t *testing.T) {
	store := newTestStore(t)
	path := writeTrail(t, okRecord)

	if res, _ := store.Ingest(context.Background(), path); res.Inserted != 1 {
		t.Fatalf("first pass inserted %d, want 1", res.Inserted)
	}

	if res, _ := store.Ingest(context.Background(), path); res.Inserted != 0 {
		t.Errorf("second pass inserted %d, want 0", res.Inserted)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(deniedRecord + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	res, err := store.Ingest(context.Background(), path)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Inserted != 1 {
		t.Errorf("third pass inserted %d, want 1", res.Inserted)
	}

	records, _ := store.Query(context.Background(), Filter{})
	if len(records) != 2 {
		t.Errorf("total records = %d, want 2", len(records))
	}
}

// A line without a trailing newline is a write in progress: leave it alone.
func TestIngestLeavesPartialLineForLater(t *testing.T) {
	store := newTestStore(t)
	path := writeTrail(t, okRecord)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// A complete record immediately followed by a fragment, as happens when a
	// process is killed mid-write.
	f.WriteString(deniedRecord + "\n" + `{"ts":"2026-01-02T03:06:05Z","tool":"que`)
	f.Close()

	res, err := store.Ingest(context.Background(), path)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Inserted != 2 {
		t.Errorf("inserted = %d, want 2 (the fragment is not a record)", res.Inserted)
	}

	records, _ := store.Query(context.Background(), Filter{})
	if len(records) != 2 {
		t.Errorf("records = %d, want 2", len(records))
	}
}

func TestIngestSkipsCorruptLinesAndContinues(t *testing.T) {
	store := newTestStore(t)
	path := writeTrail(t, okRecord, "not json at all", deniedRecord)

	res, err := store.Ingest(context.Background(), path)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Inserted != 2 {
		t.Errorf("inserted = %d, want 2", res.Inserted)
	}
	if res.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", res.Skipped)
	}
}

func TestIngestRereadsAfterTruncation(t *testing.T) {
	store := newTestStore(t)
	path := writeTrail(t, okRecord, deniedRecord)

	store.Ingest(context.Background(), path)

	// Simulate log rotation: same path, smaller file.
	if err := os.WriteFile(path, []byte(okRecord+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := store.Ingest(context.Background(), path)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Inserted != 1 {
		t.Errorf("inserted = %d, want 1 after truncation", res.Inserted)
	}
}

func TestIngestMissingFileIsNotAnError(t *testing.T) {
	store := newTestStore(t)

	res, err := store.Ingest(context.Background(), filepath.Join(t.TempDir(), "absent.jsonl"))
	if err != nil {
		t.Fatalf("Ingest on a missing file should be a no-op, got: %v", err)
	}
	if res.Inserted != 0 {
		t.Errorf("inserted = %d, want 0", res.Inserted)
	}
}

func TestFilterAndCount(t *testing.T) {
	store := newTestStore(t)
	path := writeTrail(t, okRecord, deniedRecord)

	if _, err := store.Ingest(context.Background(), path); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		filter Filter
		want   int
	}{
		{"by database", Filter{Database: "app"}, 1},
		{"by tool", Filter{Tool: "execute"}, 1},
		{"by status", Filter{Status: "denied"}, 1},
		{"search matches sql", Filter{Search: "DELETE"}, 1},
		{"search matches error", Filter{Search: "refused"}, 1},
		{"search misses", Filter{Search: "nope"}, 0},
		{"combined", Filter{Database: "identity", Status: "denied"}, 1},
		{"no filter", Filter{}, 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records, err := store.Query(context.Background(), tc.filter)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if len(records) != tc.want {
				t.Errorf("Query returned %d, want %d", len(records), tc.want)
			}

			count, err := store.Count(context.Background(), tc.filter)
			if err != nil {
				t.Fatalf("Count: %v", err)
			}
			if count != tc.want {
				t.Errorf("Count = %d, want %d", count, tc.want)
			}
		})
	}
}

func TestQueryRespectsLimitAndOffset(t *testing.T) {
	store := newTestStore(t)
	path := writeTrail(t, okRecord, deniedRecord)
	store.Ingest(context.Background(), path)

	records, err := store.Query(context.Background(), Filter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("limit ignored: got %d", len(records))
	}

	first := records[0].ID

	records, err = store.Query(context.Background(), Filter{Limit: 1, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("offset ignored: got %d", len(records))
	}
	if records[0].ID == first {
		t.Error("offset did not move the window")
	}
}

func TestStats(t *testing.T) {
	store := newTestStore(t)
	path := writeTrail(t, okRecord, deniedRecord)
	store.Ingest(context.Background(), path)

	stats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	if stats.Total != 2 {
		t.Errorf("total = %d, want 2", stats.Total)
	}
	if stats.OldestRecord == "" || stats.NewestRecord == "" {
		t.Error("stats should report the covered time range")
	}

	found := false
	for _, sc := range stats.ByStatus {
		if sc.Value == "denied" && sc.Count == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("by_status missing denied=1: %+v", stats.ByStatus)
	}

	// The fixture timestamps are in 2026, so they are excluded from a 24h
	// window computed against the real clock.
	if stats.Last24h != 0 {
		t.Logf("note: Last24h = %d (fixture timestamps are historical)", stats.Last24h)
	}
}

func TestDistinctLists(t *testing.T) {
	store := newTestStore(t)
	path := writeTrail(t, okRecord, deniedRecord)
	store.Ingest(context.Background(), path)

	databases, err := store.DistinctDatabases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(databases) != 2 {
		t.Errorf("databases = %v, want 2 entries", databases)
	}

	tools, err := store.DistinctTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Errorf("tools = %v, want 2 entries", tools)
	}
}

func TestOpenStorePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.db")
	trail := writeTrail(t, okRecord)

	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ingest(context.Background(), trail); err != nil {
		t.Fatal(err)
	}
	store.Close()

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	// The ingest offset persisted, so the same file is not re-read.
	res, err := reopened.Ingest(context.Background(), trail)
	if err != nil {
		t.Fatal(err)
	}
	if res.Inserted != 0 {
		t.Errorf("reopened store re-read %d record(s)", res.Inserted)
	}

	stats, _ := reopened.Stats(context.Background())
	if stats.Total != 1 {
		t.Errorf("total after reopen = %d, want 1", stats.Total)
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[float64]string{
		0:    "-",
		-1:   "-",
		0.25: "0.25ms",
		12.5: "12.5ms",
		1500: "1.50s",
	}

	for ms, want := range cases {
		if got := FormatDuration(ms); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", ms, got, want)
		}
	}
}

// A record with no timestamp must still be stored, since the UI sorts on time.
func TestIngestStampsMissingTimestamps(t *testing.T) {
	store := newTestStore(t)
	path := writeTrail(t, `{"tool":"query","database":"x","status":"ok","sql":"SELECT 1"}`)

	if _, err := store.Ingest(context.Background(), path); err != nil {
		t.Fatal(err)
	}

	records, err := store.Query(context.Background(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if _, err := time.Parse(time.RFC3339Nano, records[0].Time); err != nil {
		t.Errorf("stored timestamp %q is not parseable: %v", records[0].Time, err)
	}
}
