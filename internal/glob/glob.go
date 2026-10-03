// Package glob implements the small pattern language VeriDB uses in
// configuration: table deny lists and column redaction rules.
//
// Supported syntax:
//
//   - matches any sequence of characters, including dots
//     ?  matches exactly one character
//
// Patterns are matched case-insensitively, because PostgreSQL folds unquoted
// identifiers to lower case and operators rarely write them consistently.
//
// A pattern is tested against several candidate forms by the caller. For
// example the pattern "*.pan" is tried against "pan", "employees.pan" and
// "public.employees.pan", so it matches a column in any table.
package glob

import "strings"

// Match reports whether s matches pattern.
func Match(pattern, s string) bool {
	return matchFold(strings.ToLower(pattern), strings.ToLower(s))
}

// MatchAny reports whether s matches any of the patterns.
func MatchAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if Match(p, s) {
			return true
		}
	}
	return false
}

// MatchName matches a pattern against a bare name and its qualified form.
// Qualified must be the most specific form available, such as "table.column" or
// "schema.table.column".
//
// Because '*' spans dots, the pattern "*.pan" matches "cards.pan" but not a
// bare "pan". Practically it usually does mean the latter, so a leading "*." is
// also retried against the bare name. That is the only special case, and it
// exists because configuration is written by hand and "*.pan" is what people
// reach for when they mean "a column called pan anywhere".
func MatchName(patterns []string, name, qualified string) bool {
	for _, p := range patterns {
		if Match(p, qualified) || Match(p, name) {
			return true
		}

		if bare, ok := strings.CutPrefix(p, "*."); ok && Match(bare, name) {
			return true
		}
	}

	return false
}

// matchFold runs an iterative wildcard match with backtracking on the last '*'.
// It is O(len(pattern)*len(s)) in the worst case and allocates nothing.
func matchFold(pattern, s string) bool {
	var (
		p, si        int
		starP, starS = -1, 0
	)

	for si < len(s) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == s[si]):
			p++
			si++

		case p < len(pattern) && pattern[p] == '*':
			// Remember the star and try to match zero characters first.
			starP, starS = p, si
			p++

		case starP >= 0:
			// Backtrack: let the last star consume one more character.
			starS++
			si = starS
			p = starP + 1

		default:
			return false
		}
	}

	for p < len(pattern) && pattern[p] == '*' {
		p++
	}

	return p == len(pattern)
}
