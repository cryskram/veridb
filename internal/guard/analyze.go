package guard

import (
	"fmt"
	"strings"
)

// Kind is the risk class of a statement.
type Kind string

const (
	// KindRead reads data and never changes it.
	KindRead Kind = "read"
	// KindWrite inserts or updates rows.
	KindWrite Kind = "write"
	// KindDelete removes rows.
	KindDelete Kind = "delete"
	// KindDDL changes schema: CREATE, ALTER, DROP, TRUNCATE, GRANT.
	KindDDL Kind = "ddl"
	// KindAdmin covers maintenance and session control: VACUUM, COPY, SET,
	// transaction control, cursors, LISTEN/NOTIFY.
	KindAdmin Kind = "admin"
)

// rank orders kinds by how much privilege they need. A statement is judged at
// the highest rank found anywhere in it, so a data-modifying CTE cannot hide
// inside an apparently harmless SELECT.
func (k Kind) rank() int {
	switch k {
	case KindRead:
		return 0
	case KindWrite:
		return 1
	case KindDelete:
		return 2
	case KindDDL:
		return 3
	case KindAdmin:
		return 4
	}
	return 5
}

func maxKind(a, b Kind) Kind {
	if b.rank() > a.rank() {
		return b
	}
	return a
}

// QualifiedName is a possibly schema-qualified relation name.
type QualifiedName struct {
	Schema string `json:"schema,omitempty"`
	Name   string `json:"name"`
}

func (q QualifiedName) String() string {
	if q.Schema == "" {
		return q.Name
	}
	return q.Schema + "." + q.Name
}

// Statement is the result of analysing a single SQL statement.
type Statement struct {
	// SQL is the statement as supplied, with any trailing semicolon removed.
	SQL string `json:"-"`
	// Skeleton is the statement with comments and literals replaced by
	// placeholders and whitespace normalised. Keyword rules match against it,
	// so a keyword inside a string literal cannot trigger or evade a rule.
	Skeleton string `json:"-"`
	// Kind is the highest risk class present in the statement.
	Kind Kind `json:"kind"`
	// CTEs lists common table expression names, which are not real tables.
	CTEs []string `json:"ctes,omitempty"`
	// Tables lists relations the statement touches, best effort. Used to apply
	// schema and table deny rules.
	Tables []QualifiedName `json:"tables,omitempty"`
	// HasWhere reports whether every UPDATE/DELETE in the statement is
	// constrained by a WHERE clause at its own nesting depth.
	HasWhere bool `json:"has_where"`
	// ForUpdate marks a locking read, which cannot be wrapped in a subquery.
	ForUpdate bool `json:"for_update,omitempty"`
	// ExplainAnalyze marks EXPLAIN ANALYZE, which executes the statement.
	ExplainAnalyze bool `json:"explain_analyze,omitempty"`
	// Truncating marks TRUNCATE, which needs its own permission.
	Truncating bool `json:"truncating,omitempty"`
	// Returning marks a statement that returns rows, so it must be run as a
	// query rather than an exec.
	Returning bool `json:"returning,omitempty"`
	// Verb is the operation that drove Kind, for example UPDATE for a statement
	// whose highest risk came from a data-modifying CTE. It is used to make
	// refusal messages name the actual operation.
	Verb string `json:"verb,omitempty"`
	// noWrap marks a read that cannot be placed inside a subquery.
	noWrap bool
}

// Wrappable reports whether the statement can be bounded by wrapping it in a
// limiting subquery. SHOW and row-locking reads cannot.
func (s Statement) Wrappable() bool {
	return s.Kind == KindRead && !s.noWrap && !s.ForUpdate && !s.ExplainAnalyze
}

// Denial describes a refusal. It is returned as an error so callers can use
// errors.As to distinguish a policy refusal from an infrastructure failure.
type Denial struct {
	// Code is a stable identifier: "empty", "multi_statement", "syntax",
	// "unsupported", "keyword", "schema", "table", "read_only",
	// "not_allowed", "missing_where", "identifier".
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (d *Denial) Error() string { return d.Message }

// denf builds a Denial.
func denf(code, format string, args ...any) *Denial {
	return &Denial{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Analyze parses sql at the syntax level. It does not consider policy.
func Analyze(sql string) (Statement, error) {
	trimmed := strings.TrimSpace(sql)
	if trimmed == "" {
		return Statement{}, denf("empty", "the statement is empty")
	}

	tokens, err := lex(trimmed)
	if err != nil {
		return Statement{}, denf("syntax", "could not parse the statement: %v", err)
	}

	statements := splitStatements(tokens)
	if len(statements) == 0 {
		return Statement{}, denf("empty", "the statement contains no SQL")
	}
	if len(statements) > 1 {
		return Statement{}, denf(
			"multi_statement",
			"only one statement may be sent at a time; %d were found. "+
				"Use run_in_transaction for several statements that must succeed together",
			len(statements),
		)
	}

	stmt, err := classify(statements[0])
	if err != nil {
		return Statement{}, err
	}

	stmt.SQL = stripTrailingSemicolon(trimmed)
	stmt.Skeleton = skeleton(statements[0])
	stmt.Tables = collectTables(statements[0], stmt.CTEs)
	stmt.HasWhere = allMutationsHaveWhere(statements[0])
	stmt.Returning = hasTopLevel(statements[0], "RETURNING")

	return stmt, nil
}

// splitStatements splits a token stream on top-level semicolons.
func splitStatements(tokens []token) [][]token {
	var (
		out   [][]token
		cur   []token
		depth int
	)

	for _, t := range tokens {
		if t.kind == tokPunct {
			switch t.text {
			case "(":
				depth++
			case ")":
				if depth > 0 {
					depth--
				}
			case ";":
				if depth == 0 {
					if len(cur) > 0 {
						out = append(out, cur)
					}
					cur = nil
					continue
				}
			}
		}
		cur = append(cur, t)
	}

	if len(cur) > 0 {
		out = append(out, cur)
	}

	return out
}

func classify(tokens []token) (Statement, error) {
	var st Statement

	idx := 0
	for idx < len(tokens) && isPunct(tokens[idx], "(") {
		idx++
	}
	if idx >= len(tokens) {
		return st, denf("syntax", "the statement contains no SQL")
	}

	head := tokens[idx]
	if head.kind != tokIdent {
		return st, denf("unsupported", "cannot interpret a statement starting with %q", head.text)
	}

	// EXPLAIN (ANALYZE, ...) <statement> is judged by the inner statement,
	// because ANALYZE actually executes it.
	if head.is("EXPLAIN") {
		inner, analyze, err := unwrapExplain(tokens[idx+1:])
		if err != nil {
			return st, err
		}

		innerStmt, err := classify(inner)
		if err != nil {
			return st, err
		}
		innerStmt.ExplainAnalyze = analyze
		// Tables from the whole token stream, minus any CTE names the inner
		// statement defined, so a data-modifying CTE is still attributed to a
		// real relation.
		innerStmt.Tables = collectTables(tokens, innerStmt.CTEs)
		innerStmt.noWrap = true

		return innerStmt, nil
	}

	st.Kind = kindOf(head.upper)
	if st.Kind == "" {
		return st, denf("unsupported", "statement type %s is not supported by VeriDB", head.upper)
	}
	st.Verb = head.upper

	switch head.upper {
	case "TRUNCATE":
		st.Truncating = true
	case "SHOW":
		// SHOW returns a result set but cannot appear inside a subquery.
		st.noWrap = true
	case "SELECT":
		if hasTopLevel(tokens[idx+1:], "INTO") {
			// SELECT ... INTO creates a table.
			st.Kind = KindDDL
		}
	case "WITH":
		ctes, err := collectCTENames(tokens[idx+1:])
		if err != nil {
			return st, err
		}
		st.CTEs = ctes
	}

	// Raise the kind if a data-modifying statement hides at any nesting depth,
	// for example a data-modifying CTE.
	for i, t := range tokens {
		if t.kind != tokIdent {
			continue
		}
		k := kindOf(t.upper)
		if k == "" || k == KindRead {
			continue
		}
		if t.upper == "UPDATE" && i > 0 && tokens[i-1].is("FOR") {
			continue // FOR UPDATE is a locking read, not a mutation
		}
		if !atStatementPosition(tokens, i) {
			continue // a column or alias that happens to share a keyword's name
		}
		if k.rank() > st.Kind.rank() {
			st.Verb = t.upper
		}
		st.Kind = maxKind(st.Kind, k)
	}

	if hasForUpdate(tokens) {
		st.ForUpdate = true
	}

	return st, nil
}

// kindOf maps a leading keyword to its risk class. An empty result means the
// keyword is not a statement head VeriDB recognises.
func kindOf(upper string) Kind {
	switch upper {
	case "SELECT", "SHOW", "TABLE", "VALUES":
		return KindRead

	case "INSERT", "UPDATE", "MERGE":
		return KindWrite

	case "COPY":
		// COPY can read or write files and programs, so it is never a plain read.
		return KindAdmin

	case "DELETE":
		return KindDelete

	case "CREATE", "ALTER", "DROP", "TRUNCATE", "COMMENT", "GRANT", "REVOKE", "REASSIGN", "SECURITY", "IMPORT":
		return KindDDL

	case "WITH":
		// Resolved by the surrounding token scan.
		return KindRead

	case "VACUUM", "ANALYZE", "REINDEX", "CLUSTER", "REFRESH", "CHECKPOINT",
		"DO", "CALL", "SET", "RESET", "DISCARD", "LOAD",
		"BEGIN", "START", "COMMIT", "END", "ROLLBACK", "SAVEPOINT", "RELEASE", "ABORT",
		"PREPARE", "EXECUTE", "DEALLOCATE", "DECLARE", "FETCH", "MOVE", "CLOSE",
		"LISTEN", "NOTIFY", "UNLISTEN", "LOCK":
		return KindAdmin
	}

	return ""
}

// atStatementPosition reports whether the identifier at index i begins a
// statement: at the very start, or just after an opening paren, a closing
// paren, a comma or a semicolon. This keeps a column named "update" from being
// mistaken for a mutation, while still catching data-modifying CTEs.
func atStatementPosition(tokens []token, i int) bool {
	if i == 0 {
		return true
	}

	prev := tokens[i-1]
	if prev.kind != tokPunct {
		return false
	}

	switch prev.text {
	case "(", ")", ",", ";":
		return true
	}

	return false
}

// unwrapExplain peels EXPLAIN and its option list off the front of tokens,
// reporting whether ANALYZE was requested.
func unwrapExplain(tokens []token) ([]token, bool, error) {
	analyze := false
	i := 0

	// EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) SELECT ...
	if i < len(tokens) && isPunct(tokens[i], "(") {
		depth := 0
		for ; i < len(tokens); i++ {
			t := tokens[i]
			if t.kind == tokPunct {
				switch t.text {
				case "(":
					depth++
				case ")":
					depth--
					if depth == 0 {
						i++
						goto rest
					}
					continue
				}
			}
			if depth == 1 && t.is("ANALYZE") {
				analyze = true
			}
		}
		return nil, false, denf("syntax", "EXPLAIN has an unterminated option list")
	}

	// EXPLAIN ANALYZE SELECT ...
	for i < len(tokens) && tokens[i].kind == tokIdent {
		switch tokens[i].upper {
		case "ANALYZE", "ANALYSE":
			analyze = true
			i++
			continue
		case "VERBOSE":
			i++
			continue
		}
		break
	}

rest:
	if i >= len(tokens) {
		return nil, false, denf("syntax", "EXPLAIN is not followed by a statement")
	}

	return tokens[i:], analyze, nil
}

// collectCTENames returns the names defined by a WITH clause.
func collectCTENames(tokens []token) ([]string, error) {
	i := 0
	if i < len(tokens) && tokens[i].is("RECURSIVE") {
		i++
	}

	var (
		names []string
		depth int
	)

	for ; i < len(tokens); i++ {
		t := tokens[i]

		if t.kind == tokPunct {
			switch t.text {
			case "(":
				depth++
			case ")":
				if depth > 0 {
					depth--
				}
			}
			continue
		}

		if depth != 0 || t.kind != tokIdent {
			continue
		}

		if _, isDML := dmlKind(t.upper); isDML {
			return names, nil // reached the main statement
		}

		if i+1 < len(tokens) {
			next := tokens[i+1]
			if next.is("AS") || isPunct(next, "(") {
				names = append(names, strings.ToLower(t.text))
			}
		}
	}

	return names, denf("syntax", "WITH clause is not followed by SELECT, INSERT, UPDATE or DELETE")
}

func dmlKind(upper string) (Kind, bool) {
	switch upper {
	case "SELECT", "VALUES", "TABLE":
		return KindRead, true
	case "INSERT", "UPDATE", "MERGE":
		return KindWrite, true
	case "DELETE":
		return KindDelete, true
	}
	return "", false
}

// allMutationsHaveWhere verifies that every UPDATE and DELETE at its own
// nesting depth is constrained by a WHERE clause at the same depth.
func allMutationsHaveWhere(tokens []token) bool {
	type site struct {
		index int
		depth int
	}

	var (
		sites []site
		depth int
	)

	for i, t := range tokens {
		if t.kind == tokPunct {
			switch t.text {
			case "(":
				depth++
			case ")":
				if depth > 0 {
					depth--
				}
			}
			continue
		}

		if t.kind != tokIdent {
			continue
		}

		switch t.upper {
		case "UPDATE":
			if i > 0 && tokens[i-1].is("FOR") {
				continue
			}
			if !atStatementPosition(tokens, i) {
				continue
			}
			sites = append(sites, site{index: i, depth: depth})

		case "DELETE":
			if !atStatementPosition(tokens, i) {
				continue
			}
			sites = append(sites, site{index: i, depth: depth})

		case "WHERE":
			for s := range sites {
				if sites[s].index < i && sites[s].depth == depth {
					sites[s].index = -1 // satisfied
				}
			}
		}
	}

	for _, s := range sites {
		if s.index >= 0 {
			return false
		}
	}

	return true
}

// collectTables extracts relation names from FROM, JOIN, INTO, USING, UPDATE
// and TRUNCATE positions. It is deliberately conservative: anything it cannot
// confidently read as a relation is skipped, and CTE names are excluded.
func collectTables(tokens []token, ctes []string) []QualifiedName {
	skip := make(map[string]struct{}, len(ctes))
	for _, name := range ctes {
		skip[name] = struct{}{}
	}

	var (
		out  []QualifiedName
		seen = map[string]struct{}{}
	)

	add := func(q QualifiedName) {
		if q.Name == "" {
			return
		}
		if _, isCTE := skip[strings.ToLower(q.Name)]; isCTE && q.Schema == "" {
			return
		}
		key := strings.ToLower(q.String())
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		out = append(out, q)
	}

	// prevKeyword reports the nearest preceding identifier keyword.
	prevKeyword := func(i int) string {
		for j := i - 1; j >= 0; j-- {
			if tokens[j].kind == tokIdent {
				return tokens[j].upper
			}
			if tokens[j].kind != tokPunct {
				return ""
			}
		}
		return ""
	}

	for i, t := range tokens {
		if t.kind != tokIdent {
			continue
		}

		var isRelation bool

		switch t.upper {
		case "FROM", "JOIN", "USING":
			isRelation = true

		case "INTO":
			// INSERT INTO relation / MERGE INTO relation / SELECT INTO relation
			isRelation = true

		case "UPDATE":
			if i > 0 && tokens[i-1].is("FOR") {
				continue
			}
			isRelation = true

		case "TRUNCATE":
			isRelation = true

		case "TABLE":
			// ALTER TABLE x, DROP TABLE x, ... but not `SELECT table FROM y`.
			switch prevKeyword(i) {
			case "ALTER", "DROP", "CREATE", "COMMENT", "TRUNCATE", "ANALYZE",
				"VACUUM", "REINDEX", "CLUSTER", "REFRESH", "LOCK", "":
				isRelation = true
			}

		default:
			continue
		}

		if !isRelation {
			continue
		}

		j := i + 1
		for j < len(tokens) && skipBeforeRelation(tokens[j]) {
			j++
		}

		name, ok, next := readQualifiedName(tokens, j)
		if !ok {
			continue
		}

		// FROM some_function(...) is a set-returning function, not a relation.
		if t.upper != "INTO" && next < len(tokens) && isPunct(tokens[next], "(") {
			continue
		}

		add(name)

		// Aliases and comma-separated relation lists: `FROM a x, b y`,
		// `DROP TABLE a, b`.
		next = skipAlias(tokens, next)
		for next < len(tokens) && isPunct(tokens[next], ",") {
			j := next + 1
			for j < len(tokens) && skipBeforeRelation(tokens[j]) {
				j++
			}

			more, ok, after := readQualifiedName(tokens, j)
			if !ok {
				break
			}

			add(more)
			next = skipAlias(tokens, after)
		}
	}

	return out
}

// skipBeforeRelation reports whether a token may sit between a relation keyword
// and the relation name: TRUNCATE TABLE x, DROP TABLE IF EXISTS x, FROM ONLY x.
func skipBeforeRelation(t token) bool {
	if t.kind != tokIdent {
		return false
	}

	switch t.upper {
	case "ONLY", "LATERAL", "TABLE", "CONCURRENTLY", "IF", "NOT", "EXISTS":
		return true
	}

	return false
}

// skipAlias advances past `AS name` or a bare `name` alias, so that a comma
// after the alias is still seen as separating relations.
func skipAlias(tokens []token, j int) int {
	if j >= len(tokens) {
		return j
	}

	if tokens[j].is("AS") {
		if j+1 < len(tokens) && isIdentifier(tokens[j+1]) {
			return j + 2
		}
		return j + 1
	}

	if isIdentifier(tokens[j]) && !isClauseKeyword(tokens[j].upper) {
		return j + 1
	}

	return j
}

func isIdentifier(t token) bool { return t.kind == tokIdent || t.kind == tokQuotedIdent }

// isClauseKeyword reports whether a keyword ends the FROM/JOIN relation list
// rather than naming an alias.
func isClauseKeyword(upper string) bool {
	switch upper {
	case "WHERE", "JOIN", "INNER", "LEFT", "RIGHT", "FULL", "CROSS", "NATURAL",
		"ON", "USING", "GROUP", "ORDER", "LIMIT", "OFFSET", "FETCH", "HAVING",
		"WINDOW", "UNION", "INTERSECT", "EXCEPT", "SET", "VALUES", "RETURNING",
		"FOR", "INTO", "AS", "WITH", "SELECT", "LATERAL", "AND", "OR",
		"WHEN", "THEN", "ELSE", "END":
		return true
	}

	return false
}

// readQualifiedName reads `name` or `schema.name` starting at index j.
func readQualifiedName(tokens []token, j int) (QualifiedName, bool, int) {
	if j >= len(tokens) {
		return QualifiedName{}, false, j
	}

	first, ok := identifierText(tokens[j])
	if !ok {
		return QualifiedName{}, false, j
	}

	if j+2 < len(tokens) && isPunct(tokens[j+1], ".") {
		if second, ok := identifierText(tokens[j+2]); ok {
			return QualifiedName{Schema: first, Name: second}, true, j + 3
		}
	}

	return QualifiedName{Name: first}, true, j + 1
}

// identifierText returns the folded text of an identifier token.
func identifierText(t token) (string, bool) {
	switch t.kind {
	case tokIdent:
		return strings.ToLower(t.text), true
	case tokQuotedIdent:
		return t.text, true
	}
	return "", false
}

// hasTopLevel reports whether kw occurs at parenthesis depth zero.
func hasTopLevel(tokens []token, kw string) bool {
	depth := 0
	for _, t := range tokens {
		if t.kind == tokPunct {
			switch t.text {
			case "(":
				depth++
			case ")":
				if depth > 0 {
					depth--
				}
			}
			continue
		}
		if depth == 0 && t.is(kw) {
			return true
		}
	}
	return false
}

// hasForUpdate reports whether the statement takes row locks, which makes it
// unsafe to wrap in a limiting subquery.
func hasForUpdate(tokens []token) bool {
	depth := 0
	for i, t := range tokens {
		if t.kind == tokPunct {
			switch t.text {
			case "(":
				depth++
			case ")":
				if depth > 0 {
					depth--
				}
			}
			continue
		}
		if depth != 0 || !t.is("FOR") || i+1 >= len(tokens) {
			continue
		}
		switch tokens[i+1].upper {
		case "UPDATE", "SHARE", "NO", "KEY":
			return true
		}
	}
	return false
}

// skeleton renders the token stream as normalised text with literals removed.
func skeleton(tokens []token) string {
	parts := make([]string, 0, len(tokens))
	for _, t := range tokens {
		parts = append(parts, t.text)
	}
	return strings.Join(parts, " ")
}

func stripTrailingSemicolon(sql string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(sql), ";")
	return strings.TrimSpace(trimmed)
}

func isPunct(t token, s string) bool { return t.kind == tokPunct && t.text == s }
