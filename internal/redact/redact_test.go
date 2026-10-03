package redact

import (
	"reflect"
	"testing"
)

func TestPlanMatchesBareAndQualifiedColumns(t *testing.T) {
	patterns := []string{"*.pan", "*password*"}

	cases := []struct {
		name      string
		qualifier string
		columns   []string
		want      []bool
	}{
		{
			name:      "bare column with star-dot pattern",
			qualifier: "public.users",
			columns:   []string{"id", "pan"},
			want:      []bool{false, true},
		},
		{
			name:      "qualified column",
			qualifier: "public.users",
			columns:   []string{"id", "pan", "password_hash"},
			want:      []bool{false, true, true},
		},
		{
			name:    "no qualifier known",
			columns: []string{"pan", "email"},
			want:    []bool{true, false},
		},
		{
			name:    "nothing matches returns nil",
			columns: []string{"id", "email"},
			want:    nil,
		},
		{
			name:    "no patterns means no masking",
			columns: []string{"pan"},
			want:    nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := patterns
			if tc.name == "no patterns means no masking" {
				p = nil
			}

			got := Plan(p, tc.qualifier, tc.columns)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Plan() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApplyMasksInPlace(t *testing.T) {
	columns := []string{"id", "pan"}
	mask := Plan([]string{"*.pan"}, "public.users", columns)

	rows := [][]any{
		{"1", "ABCDE1234F"},
		{"2", "XYZAB9876Q"},
	}

	count := Apply(mask, rows)
	if count != 2 {
		t.Errorf("Apply returned %d, want 2", count)
	}

	for i, row := range rows {
		if row[0] != string(rune('1'+i)) {
			t.Errorf("row %d non-sensitive column was modified: %v", i, row[0])
		}
		if row[1] != Mask {
			t.Errorf("row %d sensitive column = %v, want %s", i, row[1], Mask)
		}
	}
}

func TestApplyIsSafeWithNilMask(t *testing.T) {
	rows := [][]any{{"a", "b"}}
	if got := Apply(nil, rows); got != 0 {
		t.Errorf("Apply(nil) = %d, want 0", got)
	}
	if rows[0][1] != "b" {
		t.Error("Apply(nil) must not modify rows")
	}
}

// A short row (fewer values than columns) must not panic or leak.
func TestApplyHandlesShortRows(t *testing.T) {
	mask := []bool{false, true, true}
	rows := [][]any{{"only-one"}, {"a", "b"}}

	if got := Apply(mask, rows); got != 1 {
		t.Errorf("Apply = %d, want 1", got)
	}
	if rows[0][0] != "only-one" {
		t.Errorf("short row was modified unexpectedly: %v", rows[0])
	}
	if rows[1][1] != Mask {
		t.Errorf("row 1 column 1 = %v, want %s", rows[1][1], Mask)
	}
}

func TestNames(t *testing.T) {
	columns := []string{"id", "pan", "email"}
	mask := Plan([]string{"*.pan", "*.email"}, "public.users", columns)

	got := Names(mask, columns)
	want := []string{"pan", "email"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}

	if Names(nil, columns) != nil {
		t.Error("Names(nil) should be nil")
	}
}

func TestPlanHandlesRaggedInput(t *testing.T) {
	// More columns than values is normal; more values than columns is not, but
	// must not panic either.
	if got := Plan([]string{"*"}, "", nil); got != nil {
		t.Errorf("Plan with no columns = %v, want nil", got)
	}
}
