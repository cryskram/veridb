// Package redact masks sensitive column values before they reach an agent.
//
// The rule of least surprise for an operator writing configuration is that
// "*.pan" means "a column called pan anywhere", so patterns are matched against
// both the bare column name and a qualified name when the relation is known.
package redact

import "github.com/cryskram/veridb/internal/glob"

// Mask replaces a redacted value.
const Mask = "[redacted]"

// Plan decides which of columns must be masked. qualifier is the relation the
// columns belong to when it is known, for example "public.users"; pass an empty
// string for an arbitrary query result whose source tables are unknown.
func Plan(patterns []string, qualifier string, columns []string) []bool {
	if len(patterns) == 0 || len(columns) == 0 {
		return nil
	}

	mask := make([]bool, len(columns))

	any := false
	for i, column := range columns {
		qualified := column
		if qualifier != "" {
			qualified = qualifier + "." + column
		}

		if glob.MatchName(patterns, column, qualified) {
			mask[i] = true
			any = true
		}
	}

	if !any {
		return nil
	}

	return mask
}

// Apply masks the planned columns in place and returns the number of values it
// replaced.
func Apply(mask []bool, rows [][]any) int {
	if mask == nil {
		return 0
	}

	count := 0
	for _, row := range rows {
		for i := range row {
			if i < len(mask) && mask[i] {
				row[i] = Mask
				count++
			}
		}
	}

	return count
}

// Names returns the names of the masked columns, so a response can tell an agent
// that a value exists but was withheld.
func Names(mask []bool, columns []string) []string {
	if mask == nil {
		return nil
	}

	var out []string
	for i, masked := range mask {
		if masked && i < len(columns) {
			out = append(out, columns[i])
		}
	}

	return out
}
