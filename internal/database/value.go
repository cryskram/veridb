package database

import (
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
)

// normalizeValue converts a pgx-decoded value into something that survives
// JSON encoding in a form an agent can reason about. pgx returns some types as
// Go structs (numeric, interval, geometric types) which would otherwise leak
// internal fields into the MCP response.
func normalizeValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil

	case pgtype.Numeric:
		return numericValue(t)

	case pgtype.Interval:
		return intervalValue(t)

	case pgtype.Time:
		if !t.Valid {
			return nil
		}
		return t.Microseconds

	case [16]byte:
		// pgx decodes uuid into a raw 16-byte array.
		return formatUUID(t)

	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)

	case []byte:
		// Text-decoded values (jsonb, citext, ...) arrive as bytes.
		if utf8.Valid(t) {
			return string(t)
		}
		return base64.StdEncoding.EncodeToString(t)

	case net.HardwareAddr:
		return t.String()
	}

	rv := reflect.ValueOf(v)

	// Any remaining pgtype struct (point, box, bits, range, ...) is better
	// rendered via its String method than as a JSON object of internals.
	if rv.Kind() == reflect.Struct && strings.HasPrefix(rv.Type().PkgPath(), "github.com/jackc/pgx/") {
		if s, ok := v.(fmt.Stringer); ok {
			return s.String()
		}
		// Types with a Valid flag and no String method: report NULL when
		// invalid rather than a struct full of zero values.
		if f := rv.FieldByName("Valid"); f.IsValid() && f.Kind() == reflect.Bool && !f.Bool() {
			return nil
		}
		return fmt.Sprintf("%v", v)
	}

	return v
}

// numericValue renders a Postgres numeric. Integers are returned as strings so
// large values do not lose precision through float64 and so an agent cannot
// accidentally round a monetary amount.
func numericValue(n pgtype.Numeric) any {
	if !n.Valid {
		return nil
	}

	switch n.InfinityModifier {
	case pgtype.Infinity:
		return "Infinity"
	case pgtype.NegativeInfinity:
		return "-Infinity"
	}

	if n.NaN {
		return "NaN"
	}

	if n.Int == nil {
		return nil
	}

	if n.Exp >= 0 {
		// Whole number: return the exact decimal digits.
		return n.Int.String() + strings.Repeat("0", int(n.Exp))
	}

	// Fractional. Prefer the exact decimal representation; fall back to
	// float64 only when the scale is beyond what we want to format.
	if scale := int(-n.Exp); scale <= 100 {
		return formatDecimal(n.Int, scale)
	}

	f, err := n.Float64Value()
	if err == nil && f.Valid {
		return f.Float64
	}

	return n.Int.String() + "e" + fmt.Sprint(n.Exp)
}

// formatDecimal renders digits with the decimal point placed scale positions
// from the right, preserving sign and trailing zeros.
func formatDecimal(i *big.Int, scale int) string {
	sign := ""
	digits := i.String()
	if strings.HasPrefix(digits, "-") {
		sign = "-"
		digits = digits[1:]
	}

	if len(digits) <= scale {
		digits = strings.Repeat("0", scale-len(digits)+1) + digits
	}

	cut := len(digits) - scale

	return sign + digits[:cut] + "." + digits[cut:]
}

func intervalValue(v pgtype.Interval) any {
	if !v.Valid {
		return nil
	}

	// Postgres months cannot be converted to a fixed duration, so report the
	// parts and let the caller decide.
	if v.Months != 0 {
		return fmt.Sprintf("%d mons %d days %s", v.Months, v.Days, time.Duration(v.Microseconds)*time.Microsecond)
	}

	return (time.Duration(v.Days)*24*time.Hour + time.Duration(v.Microseconds)*time.Microsecond).String()
}

func formatUUID(b [16]byte) string {
	const hexdigits = "0123456789abcdef"

	var sb strings.Builder
	sb.Grow(36)

	for i, c := range b {
		switch i {
		case 4, 6, 8, 10:
			sb.WriteByte('-')
		}
		sb.WriteByte(hexdigits[c>>4])
		sb.WriteByte(hexdigits[c&0x0f])
	}

	return sb.String()
}
