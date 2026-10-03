package guard

import (
	"errors"
	"fmt"
	"strings"
)

// ApplyReadLimit bounds a read statement server-side by wrapping it in a
// subquery with a LIMIT one higher than the cap, so the caller can tell the
// difference between "exactly at the limit" and "truncated".
//
// It returns the statement to run and whether it was wrapped. Statements that
// cannot legally appear inside a subquery (SHOW), that lock rows (FOR UPDATE),
// or that EXPLAIN ANALYZE already handles, are returned unchanged: the caller
// must then enforce the cap while reading and accept that the server may do
// more work.
func ApplyReadLimit(stmt Statement, maxRows int) (string, bool) {
	if maxRows <= 0 || !stmt.Wrappable() {
		return stmt.SQL, false
	}

	body := strings.TrimRight(strings.TrimSpace(stmt.SQL), ";")

	return fmt.Sprintf(
		"SELECT * FROM (\n%s\n) AS veridb_limited LIMIT %d",
		body, maxRows+1,
	), true
}

// QuoteIdent quotes a PostgreSQL identifier safely. It is used by the sample
// tool, which must interpolate a table name that cannot be a bind parameter.
func QuoteIdent(s string) (string, error) {
	if s == "" {
		return "", denf("identifier", "identifier must not be empty")
	}
	if strings.ContainsRune(s, 0) {
		return "", denf("identifier", "identifier must not contain a null byte")
	}

	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`, nil
}

// IsDenial reports whether err is a policy refusal rather than a failure.
func IsDenial(err error) bool {
	var d *Denial
	return errors.As(err, &d)
}
