package mcpserver

import (
	"reflect"
	"strings"
	"testing"
)

func TestEffectiveLimit(t *testing.T) {
	cases := []struct {
		name       string
		maxRows    int
		requested  int
		wantLimit  int
		wantNotice bool
	}{
		{"uses the cap when nothing is asked", 100, 0, 100, false},
		{"honours a lower request", 100, 20, 20, false},
		{"honours a request equal to the cap", 100, 100, 100, false},
		{"clamps a request above the cap and says so", 100, 5000, 100, true},
		{"unlimited policy still bounds the response", 0, 0, 1000, true},
		{"unlimited policy honours an explicit request", 0, 25, 25, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limit, notice := effectiveLimit(tc.maxRows, tc.requested)
			if limit != tc.wantLimit {
				t.Errorf("limit = %d, want %d", limit, tc.wantLimit)
			}
			if (notice != "") != tc.wantNotice {
				t.Errorf("notice = %q, wantNotice %v", notice, tc.wantNotice)
			}
		})
	}
}

func TestToObjects(t *testing.T) {
	columns := []string{"id", "amount"}
	rows := [][]any{{"a1", "1500.00"}, {"b2", nil}}

	got := toObjects(columns, rows)
	want := []rowObject{
		{"id": "a1", "amount": "1500.00"},
		{"id": "b2", "amount": nil},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("toObjects() = %v, want %v", got, want)
	}
}

// A row shorter than the column list must not panic, and the missing key simply
// stays absent.
func TestToObjectsHandlesShortRows(t *testing.T) {
	got := toObjects([]string{"a", "b", "c"}, [][]any{{"1"}})

	if len(got) != 1 {
		t.Fatalf("expected one object, got %d", len(got))
	}
	if got[0]["a"] != "1" {
		t.Errorf("a = %v", got[0]["a"])
	}
	if _, present := got[0]["c"]; present {
		t.Error("a column with no value should be absent rather than nil")
	}
}

func TestNormalizeParams(t *testing.T) {
	cases := []struct {
		name string
		in   []any
		want []any
	}{
		{"empty", nil, nil},
		{"scalars pass through", []any{"s", true, nil}, []any{"s", true, nil}},
		// encoding/json decodes every number as float64, which pgx would send
		// to an integer column and lose.
		{"integral float becomes int64", []any{float64(42)}, []any{int64(42)}},
		{"fractional float stays float64", []any{1.5}, []any{1.5}},
		{"go int becomes int64", []any{7}, []any{int64(7)}},
		{"float32 widens", []any{float32(2.5)}, []any{2.5}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeParams(tc.in)
			if err != nil {
				t.Fatalf("normalizeParams: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("normalizeParams(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// Arrays and objects are passed as JSON text so a statement can cast them.
func TestNormalizeParamsEncodesStructuredValues(t *testing.T) {
	got, err := normalizeParams([]any{[]any{1, 2}, map[string]any{"k": "v"}})
	if err != nil {
		t.Fatalf("normalizeParams: %v", err)
	}

	if got[0] != "[1,2]" {
		t.Errorf("array = %v, want [1,2]", got[0])
	}
	if got[1] != `{"k":"v"}` {
		t.Errorf("object = %v, want {\"k\":\"v\"}", got[1])
	}
}

func TestFirstN(t *testing.T) {
	rows := [][]any{{1}, {2}, {3}}

	if got := firstN(rows, 2); len(got) != 2 {
		t.Errorf("firstN(rows, 2) length = %d, want 2", len(got))
	}
	if got := firstN(rows, 0); len(got) != 3 {
		t.Errorf("firstN(rows, 0) should mean no limit, got %d", len(got))
	}
	if got := firstN(rows, 10); len(got) != 3 {
		t.Errorf("firstN(rows, 10) should keep all rows, got %d", len(got))
	}
}

func TestTruncateSQL(t *testing.T) {
	if got := truncateSQL("SELECT 1"); got != "SELECT 1" {
		t.Errorf("short SQL was modified: %q", got)
	}

	long := strings.Repeat("x", 5000)
	got := truncateSQL(long)
	if len(got) > 2100 {
		t.Errorf("long SQL was not truncated: %d chars", len(got))
	}
	if !strings.HasSuffix(got, "[truncated]") {
		t.Errorf("truncation should be visible: %q", got[len(got)-20:])
	}
}
