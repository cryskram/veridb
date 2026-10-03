package guard

import (
	"fmt"
	"strings"
)

type tokenKind int

const (
	tokIdent tokenKind = iota
	tokQuotedIdent
	tokString
	tokNumber
	tokParam
	tokPunct
)

func (k tokenKind) String() string {
	switch k {
	case tokIdent:
		return "identifier"
	case tokQuotedIdent:
		return "quoted identifier"
	case tokString:
		return "string"
	case tokNumber:
		return "number"
	case tokParam:
		return "parameter"
	case tokPunct:
		return "punctuation"
	}
	return "unknown"
}

type token struct {
	kind tokenKind
	// text is the token exactly as written, except strings which are reduced
	// to their placeholder.
	text string
	// upper is the upper-cased text, used for keyword comparison.
	upper string
}

// is reports whether the token is an unquoted identifier equal to kw,
// compared case-insensitively the way PostgreSQL folds unquoted identifiers.
func (t token) is(kw string) bool { return t.kind == tokIdent && t.upper == kw }

// lexer walks SQL text. It never needs to build the full token slice in memory
// for a statement, but statements here are small enough that it does.
type lexer struct {
	src string
	pos int
}

// lex splits sql into tokens, discarding comments and recording string
// literals as opaque placeholders. Errors are reported for unterminated
// constructs, which is enough to refuse a malformed statement early.
func lex(sql string) ([]token, error) {
	l := &lexer{src: sql}
	var tokens []token

	for {
		tok, ok, err := l.next()
		if err != nil {
			return nil, err
		}
		if !ok {
			return tokens, nil
		}
		tokens = append(tokens, tok)
	}
}

func (l *lexer) next() (token, bool, error) {
	l.skipSpaceAndComments()

	if l.pos >= len(l.src) {
		return token{}, false, nil
	}

	c := l.src[l.pos]

	switch {
	case c == '\'':
		if err := l.scanString(); err != nil {
			return token{}, false, err
		}
		return token{kind: tokString, text: "''", upper: "''"}, true, nil

	case c == '"':
		text, err := l.scanQuotedIdent()
		if err != nil {
			return token{}, false, err
		}
		return token{kind: tokQuotedIdent, text: text, upper: strings.ToUpper(text)}, true, nil

	case c == '$':
		// Either a dollar-quoted string ($tag$ ... $tag$) or a positional
		// parameter ($1).
		if delim, ok := l.dollarQuoteDelim(); ok {
			if err := l.scanDollarQuoted(delim); err != nil {
				return token{}, false, err
			}
			return token{kind: tokString, text: "''", upper: "''"}, true, nil
		}
		if l.pos+1 < len(l.src) && isDigit(l.src[l.pos+1]) {
			start := l.pos
			l.pos++
			for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
				l.pos++
			}
			text := l.src[start:l.pos]
			return token{kind: tokParam, text: text, upper: text}, true, nil
		}
		l.pos++
		return token{kind: tokPunct, text: "$", upper: "$"}, true, nil

	case isIdentStart(c):
		text := l.scanIdent()
		// A letter prefix glued to a quote is a prefixed literal (E'..', B'..',
		// X'..', N'..'). Swallow it so the prefix is not mistaken for a column.
		if l.pos < len(l.src) && l.src[l.pos] == '\'' {
			if err := l.scanString(); err != nil {
				return token{}, false, err
			}
			return token{kind: tokString, text: "''", upper: "''"}, true, nil
		}
		return token{kind: tokIdent, text: text, upper: strings.ToUpper(text)}, true, nil

	case isDigit(c) || (c == '.' && l.pos+1 < len(l.src) && isDigit(l.src[l.pos+1])):
		text := l.scanNumber()
		return token{kind: tokNumber, text: text, upper: text}, true, nil

	default:
		l.pos++
		return token{kind: tokPunct, text: string(c), upper: string(c)}, true, nil
	}
}

func (l *lexer) skipSpaceAndComments() {
	for l.pos < len(l.src) {
		c := l.src[l.pos]

		if isSpace(c) {
			l.pos++
			continue
		}

		// Line comment.
		if c == '-' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '-' {
			l.pos += 2
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
			continue
		}

		// Block comment, which PostgreSQL allows to nest.
		if c == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '*' {
			depth := 0
			for l.pos < len(l.src) {
				if l.pos+1 < len(l.src) && l.src[l.pos] == '/' && l.src[l.pos+1] == '*' {
					depth++
					l.pos += 2
					continue
				}
				if l.pos+1 < len(l.src) && l.src[l.pos] == '*' && l.src[l.pos+1] == '/' {
					depth--
					l.pos += 2
					if depth == 0 {
						break
					}
					continue
				}
				l.pos++
			}
			continue
		}

		return
	}
}

// scanString consumes a single-quoted literal. Backslash escapes are honoured
// unconditionally, which is a superset of standard_conforming_strings behaviour
// and therefore safe for parsing purposes.
func (l *lexer) scanString() error {
	l.pos++ // opening quote

	for l.pos < len(l.src) {
		switch l.src[l.pos] {
		case '\\':
			l.pos += 2
		case '\'':
			l.pos++
			if l.pos < len(l.src) && l.src[l.pos] == '\'' {
				l.pos++ // doubled quote
				continue
			}
			return nil
		default:
			l.pos++
		}
	}

	return fmt.Errorf("unterminated string literal")
}

func (l *lexer) scanQuotedIdent() (string, error) {
	l.pos++ // opening quote

	var sb strings.Builder
	for l.pos < len(l.src) {
		if l.src[l.pos] == '"' {
			l.pos++
			if l.pos < len(l.src) && l.src[l.pos] == '"' {
				sb.WriteByte('"')
				l.pos++
				continue
			}
			return sb.String(), nil
		}
		sb.WriteByte(l.src[l.pos])
		l.pos++
	}

	return "", fmt.Errorf("unterminated quoted identifier")
}

// dollarQuoteDelim recognises the opening delimiter of a dollar-quoted string
// and returns it (for example "$$" or "$tag$"). The lexer position is left
// immediately after the delimiter when it matches.
//
// The tag may contain letters, digits and underscores but never a dollar sign,
// which is what distinguishes $$ from $1.
func (l *lexer) dollarQuoteDelim() (string, bool) {
	if l.src[l.pos] != '$' {
		return "", false
	}

	i := l.pos + 1

	// A tag must not begin with a digit, otherwise $1 would look like a tag.
	if i < len(l.src) && (isLetter(l.src[i]) || l.src[i] == '_') {
		i++
		for i < len(l.src) && (isLetter(l.src[i]) || isDigit(l.src[i]) || l.src[i] == '_') {
			i++
		}
	}

	if i < len(l.src) && l.src[i] == '$' {
		delim := l.src[l.pos : i+1]
		l.pos = i + 1
		return delim, true
	}

	return "", false
}

func (l *lexer) scanDollarQuoted(delim string) error {
	idx := strings.Index(l.src[l.pos:], delim)
	if idx < 0 {
		return fmt.Errorf("unterminated dollar-quoted string %s", delim)
	}
	l.pos += idx + len(delim)
	return nil
}

func (l *lexer) scanIdent() string {
	start := l.pos
	for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
		l.pos++
	}
	return l.src[start:l.pos]
}

func (l *lexer) scanNumber() string {
	start := l.pos
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if isDigit(c) || c == '.' || c == 'e' || c == 'E' || c == 'x' || c == 'X' ||
			(c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			l.pos++
			continue
		}
		break
	}
	return l.src[start:l.pos]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentStart(c byte) bool {
	return c == '_' || isLetter(c)
}

func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) || c == '$' }
