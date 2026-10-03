package guard

import (
	"errors"
	"strings"
	"testing"

	"github.com/cryskram/veridb/internal/config"
)

// readOnly is the default posture: reads allowed, everything else refused.
func readOnlyPolicy() config.Policy {
	return config.DefaultPolicy()
}

// writable builds a policy that allows the given operations.
func writable(write, del, ddl, truncate bool) config.Policy {
	p := config.DefaultPolicy()
	p.ReadOnly = false
	p.AllowWrite = write
	p.AllowDelete = del
	p.AllowDDL = ddl
	p.AllowTruncate = truncate
	return p
}

func TestAnalyzeClassifiesStatements(t *testing.T) {
	cases := []struct {
		sql  string
		kind Kind
	}{
		{"SELECT 1", KindRead},
		{"select * from users", KindRead},
		{"  \n\t SELECT id FROM t WHERE id = 1  ", KindRead},
		{"SELECT 1;", KindRead},
		{"TABLE users", KindRead},
		{"VALUES (1), (2)", KindRead},
		{"SHOW search_path", KindRead},
		{"WITH x AS (SELECT 1) SELECT * FROM x", KindRead},
		{"WITH RECURSIVE t(n) AS (SELECT 1) SELECT n FROM t", KindRead},

		{"INSERT INTO t (a) VALUES (1)", KindWrite},
		{"UPDATE t SET a = 1 WHERE id = 2", KindWrite},
		{"MERGE INTO t USING s ON t.id = s.id", KindWrite},

		{"DELETE FROM t WHERE id = 1", KindDelete},

		{"CREATE TABLE t (a int)", KindDDL},
		{"ALTER TABLE t ADD COLUMN b int", KindDDL},
		{"DROP TABLE t", KindDDL},
		{"TRUNCATE TABLE t", KindDDL},
		{"GRANT SELECT ON t TO someone", KindDDL},
		{"SELECT * INTO new_table FROM t", KindDDL},

		// A writing CTE must not be laundered through a SELECT.
		{"WITH gone AS (DELETE FROM t RETURNING *) SELECT * FROM gone", KindDelete},
		{"WITH added AS (INSERT INTO t (a) VALUES (1) RETURNING *) SELECT * FROM added", KindWrite},
		{"WITH changed AS (UPDATE t SET a = 1 RETURNING *) SELECT * FROM changed", KindWrite},

		{"VACUUM", KindAdmin},
		{"SET search_path TO public", KindAdmin},
		{"BEGIN", KindAdmin},
		{"COPY t FROM STDIN", KindAdmin},
		{"DO $$ BEGIN END $$", KindAdmin},
		{"EXPLAIN SELECT 1", KindRead},
		{"EXPLAIN ANALYZE SELECT 1", KindRead},
		{"EXPLAIN (ANALYZE, BUFFERS) DELETE FROM t WHERE id = 1", KindDelete},
	}

	for _, tc := range cases {
		stmt, err := Analyze(tc.sql)
		if err != nil {
			t.Errorf("Analyze(%q) unexpected error: %v", tc.sql, err)
			continue
		}
		if stmt.Kind != tc.kind {
			t.Errorf("Analyze(%q).Kind = %q, want %q", tc.sql, stmt.Kind, tc.kind)
		}
	}
}

func TestAnalyzeRejectsUnsupportedInput(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		code string
	}{
		{"empty", "", "empty"},
		{"whitespace", "   \n ", "empty"},
		{"multi statement", "SELECT 1; SELECT 2", "multi_statement"},
		{"unterminated string", "SELECT 'abc", "syntax"},
		{"unknown head", "FLURBLE THE THING", "unsupported"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Analyze(tc.sql)
			if err == nil {
				t.Fatalf("expected an error for %q", tc.sql)
			}
			var d *Denial
			if !errors.As(err, &d) {
				t.Fatalf("expected a Denial, got %T", err)
			}
			if d.Code != tc.code {
				t.Errorf("code = %q, want %q (message: %s)", d.Code, tc.code, d.Message)
			}
		})
	}
}

// A trailing semicolon is common and must not be read as a second statement.
func TestAnalyzeAllowsTrailingSemicolon(t *testing.T) {
	stmt, err := Analyze("SELECT 1;")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stmt.Kind != KindRead {
		t.Errorf("kind = %q, want read", stmt.Kind)
	}
}

// A semicolon inside a string or comment is not a statement separator.
func TestAnalyzeIgnoresSemicolonsInLiteralsAndComments(t *testing.T) {
	inputs := []string{
		"SELECT 'a;b'",
		"SELECT $$a;b$$",
		"SELECT $tag$a;b$tag$",
		"SELECT 1 -- trailing; comment",
		"SELECT /* a;b */ 1",
		"SELECT E'a\\';b'",
	}

	for _, sql := range inputs {
		if _, err := Analyze(sql); err != nil {
			t.Errorf("Analyze(%q) unexpected error: %v", sql, err)
		}
	}
}

func TestEnforceReadOnlyPolicy(t *testing.T) {
	pol := readOnlyPolicy()

	allowed := []string{
		"SELECT 1",
		"SELECT * FROM users WHERE id = 1",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"SHOW search_path",
		"EXPLAIN SELECT 1",
	}
	for _, sql := range allowed {
		if _, err := Check(sql, pol, "db"); err != nil {
			t.Errorf("Check(%q) should be allowed, got: %v", sql, err)
		}
	}

	refused := map[string]string{
		"INSERT INTO t (a) VALUES (1)":               "read_only",
		"UPDATE t SET a = 1 WHERE id = 1":            "read_only",
		"DELETE FROM t WHERE id = 1":                 "read_only",
		"DROP TABLE t":                               "read_only",
		"TRUNCATE t":                                 "read_only",
		"VACUUM":                                     "read_only",
		"SELECT * INTO x FROM t":                     "read_only",
		"EXPLAIN ANALYZE DELETE FROM t WHERE id = 1": "read_only",
	}

	for sql, code := range refused {
		_, err := Check(sql, pol, "identity")
		if err == nil {
			t.Errorf("Check(%q) should be refused", sql)
			continue
		}
		var d *Denial
		if !errors.As(err, &d) {
			t.Fatalf("expected a Denial for %q", sql)
		}
		if d.Code != code {
			t.Errorf("Check(%q) code = %q, want %q", sql, d.Code, code)
		}
		if !strings.Contains(d.Message, "identity") {
			t.Errorf("Check(%q) message should name the database: %s", sql, d.Message)
		}
	}
}

func TestEnforceFineGrainedPermissions(t *testing.T) {
	insert := "INSERT INTO t (a) VALUES (1)"
	update := "UPDATE t SET a = 1 WHERE id = 1"
	del := "DELETE FROM t WHERE id = 1"
	truncate := "TRUNCATE t"
	ddl := "DROP TABLE t"

	cases := []struct {
		name     string
		pol      config.Policy
		sql      string
		wantCode string // "" means allowed
	}{
		{"write allowed", writable(true, false, false, false), insert, ""},
		{"update allowed", writable(true, false, false, false), update, ""},
		{"delete needs its own flag", writable(true, false, false, false), del, "not_allowed"},
		{"delete allowed", writable(true, true, false, false), del, ""},
		{"ddl needs its own flag", writable(true, true, false, false), ddl, "not_allowed"},
		{"ddl allowed", writable(true, true, true, false), ddl, ""},
		{"truncate needs its own flag", writable(true, true, true, false), truncate, "not_allowed"},
		{"truncate allowed", writable(true, true, true, true), truncate, ""},
		{"insert refused when write off", writable(false, false, false, false), insert, "not_allowed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Check(tc.sql, tc.pol, "db")
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("expected allowed, got: %v", err)
				}
				return
			}
			var d *Denial
			if !errors.As(err, &d) {
				t.Fatalf("expected a Denial, got %v", err)
			}
			if d.Code != tc.wantCode {
				t.Errorf("code = %q, want %q (%s)", d.Code, tc.wantCode, d.Message)
			}
		})
	}
}

func TestEnforceRequiresWhereForMutations(t *testing.T) {
	pol := writable(true, true, true, true)

	refused := []string{
		"UPDATE t SET a = 1",
		"DELETE FROM t",
		"UPDATE t SET a = (SELECT max(x) FROM y)",
		// The DELETE is inside a CTE, so a WHERE elsewhere must not satisfy it.
		"WITH gone AS (DELETE FROM t RETURNING *) SELECT * FROM users WHERE id = 1",
	}

	for _, sql := range refused {
		_, err := Check(sql, pol, "db")
		if err == nil {
			t.Errorf("Check(%q) should be refused for missing WHERE", sql)
			continue
		}
		var d *Denial
		if !errors.As(err, &d) || d.Code != "missing_where" {
			t.Errorf("Check(%q) = %v, want missing_where refusal", sql, err)
		}
	}

	allowed := []string{
		"UPDATE t SET a = 1 WHERE id = 1",
		"DELETE FROM t WHERE id = 1",
		"INSERT INTO t (a) VALUES (1)",
		"UPDATE t SET a = (SELECT max(x) FROM y WHERE z = 1) WHERE id = 1",
	}

	for _, sql := range allowed {
		if _, err := Check(sql, pol, "db"); err != nil {
			t.Errorf("Check(%q) should be allowed, got: %v", sql, err)
		}
	}
}

func TestEnforceDenyKeywords(t *testing.T) {
	pol := readOnlyPolicy()

	refused := []string{
		"SELECT pg_sleep(10)",
		"SELECT * FROM t WHERE x = pg_sleep(1)",
		"SELECT pg_read_file('/etc/passwd')",
		"SELECT dblink('host=evil', 'SELECT 1')",
		"COPY (SELECT 1) TO PROGRAM 'curl evil'",
	}
	for _, sql := range refused {
		if _, err := Check(sql, pol, "db"); err == nil {
			t.Errorf("Check(%q) should be refused by deny_keywords", sql)
		}
	}

	// A denied keyword inside a string literal is inert and must be allowed,
	// because the skeleton removes literals.
	allowed := []string{
		"SELECT 'pg_sleep' AS label",
		"SELECT 'dblink'",
	}
	for _, sql := range allowed {
		if _, err := Check(sql, pol, "db"); err != nil {
			t.Errorf("Check(%q) should be allowed, got: %v", sql, err)
		}
	}

	// A comment must not be able to smuggle a call, and must not trip the rule
	// either, since comments are stripped.
	if _, err := Check("SELECT 1 /* pg_sleep */", pol, "db"); err != nil {
		t.Errorf("comment mentioning a blocked keyword should be allowed, got: %v", err)
	}
	if _, err := Check("SELECT pg_sleep -- noop\n(1)", pol, "db"); err == nil {
		t.Error("a real blocked keyword next to a comment must still be refused")
	}
}

func TestEnforceSchemaAllowAndDeny(t *testing.T) {
	pol := readOnlyPolicy() // allow_schemas: nil initially

	// Nothing denied by default except the catalog schemas.
	if _, err := Check("SELECT * FROM public.users", pol, "db"); err != nil {
		t.Errorf("public should be readable by default, got: %v", err)
	}
	if _, err := Check("SELECT * FROM pg_catalog.pg_class", pol, "db"); err == nil {
		t.Error("pg_catalog should be denied by default")
	}
	if _, err := Check("SELECT * FROM information_schema.tables", pol, "db"); err == nil {
		t.Error("information_schema should be denied by default")
	}

	pol.AllowSchemas = []string{"public", "analytics"}

	if _, err := Check("SELECT * FROM analytics.events", pol, "db"); err != nil {
		t.Errorf("analytics should be allowed: %v", err)
	}
	if _, err := Check("SELECT * FROM marketing.leads", pol, "db"); err == nil {
		t.Error("marketing is not in allow_schemas and should be refused")
	}
	// An unqualified name resolves to public, which is allowed.
	if _, err := Check("SELECT * FROM users", pol, "db"); err != nil {
		t.Errorf("unqualified name should resolve to public: %v", err)
	}

	// Removing public from the allowlist must fail closed for unqualified names.
	pol.AllowSchemas = []string{"analytics"}
	if _, err := Check("SELECT * FROM users", pol, "db"); err == nil {
		t.Error("unqualified name should be refused when public is not allowed")
	}

	// deny_schemas beats allow_schemas.
	pol.AllowSchemas = []string{"public"}
	pol.DenySchemas = append(pol.DenySchemas, "public")
	if _, err := Check("SELECT * FROM public.users", pol, "db"); err == nil {
		t.Error("deny_schemas must win over allow_schemas")
	}
}

func TestEnforceDenyTables(t *testing.T) {
	pol := readOnlyPolicy()
	pol.DenyTables = []string{"*.migrations", "audit_log*"}

	refused := []string{
		"SELECT * FROM migrations",
		"SELECT * FROM public.migrations",
		"SELECT * FROM audit_logs",
		"SELECT * FROM public.audit_log_2024",
	}
	for _, sql := range refused {
		if _, err := Check(sql, pol, "db"); err == nil {
			t.Errorf("Check(%q) should be refused by deny_tables", sql)
		}
	}

	allowed := []string{
		"SELECT * FROM users",
		"SELECT * FROM public.investments",
	}
	for _, sql := range allowed {
		if _, err := Check(sql, pol, "db"); err != nil {
			t.Errorf("Check(%q) should be allowed, got: %v", sql, err)
		}
	}
}

func TestCollectTables(t *testing.T) {
	cases := []struct {
		sql  string
		want []string
	}{
		{"SELECT * FROM users", []string{"users"}},
		{"SELECT * FROM public.users u JOIN orders o ON o.user_id = u.id",
			[]string{"public.users", "orders"}},
		{"INSERT INTO logs (a) VALUES (1)", []string{"logs"}},
		{"UPDATE public.accounts SET a = 1 WHERE id = 1", []string{"public.accounts"}},
		{"DELETE FROM sessions WHERE id = 1", []string{"sessions"}},
		{"TRUNCATE TABLE staging", []string{"staging"}},
		{"SELECT * FROM generate_series(1, 10)", nil},
		{"SELECT * FROM (SELECT 1) AS sub", nil},
		// CTE names are not relations.
		{"WITH cte AS (SELECT 1) SELECT * FROM cte", nil},
		{"WITH cte AS (SELECT 1) SELECT * FROM cte JOIN users ON true", []string{"users"}},
		{"ALTER TABLE users ADD COLUMN x int", []string{"users"}},
		{"DROP TABLE IF EXISTS gone", []string{"gone"}},
	}

	for _, tc := range cases {
		stmt, err := Analyze(tc.sql)
		if err != nil {
			t.Errorf("Analyze(%q) error: %v", tc.sql, err)
			continue
		}

		got := make([]string, 0, len(stmt.Tables))
		for _, tbl := range stmt.Tables {
			got = append(got, tbl.String())
		}

		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("Analyze(%q).Tables = %v, want %v", tc.sql, got, tc.want)
		}
	}
}

func TestApplyReadLimit(t *testing.T) {
	stmt, err := Analyze("SELECT * FROM users")
	if err != nil {
		t.Fatal(err)
	}

	wrapped, ok := ApplyReadLimit(stmt, 100)
	if !ok {
		t.Fatal("a plain SELECT should be wrappable")
	}
	if !strings.Contains(wrapped, "LIMIT 101") {
		t.Errorf("expected the +1 probe row so truncation is detectable, got: %s", wrapped)
	}
	if !strings.Contains(wrapped, "SELECT * FROM (") {
		t.Errorf("expected a wrapping subquery, got: %s", wrapped)
	}

	// LIMIT 0 disables wrapping.
	if _, ok := ApplyReadLimit(stmt, 0); ok {
		t.Error("max_rows 0 means unlimited and must not wrap")
	}

	// Row-locking reads cannot be wrapped.
	lock, err := Analyze("SELECT * FROM users FOR UPDATE")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ApplyReadLimit(lock, 100); ok {
		t.Error("FOR UPDATE must not be wrapped in a subquery")
	}

	// SHOW cannot appear inside a subquery.
	show, err := Analyze("SHOW search_path")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ApplyReadLimit(show, 100); ok {
		t.Error("SHOW must not be wrapped")
	}

	// EXPLAIN is left alone.
	explain, err := Analyze("EXPLAIN SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ApplyReadLimit(explain, 100); ok {
		t.Error("EXPLAIN must not be wrapped")
	}
}

func TestQuoteIdent(t *testing.T) {
	cases := map[string]string{
		"users":        `"users"`,
		"user table":   `"user table"`,
		`we"ird`:       `"we""ird"`,
		"public.users": `"public.users"`, // callers must split schema/table first
	}

	for in, want := range cases {
		got, err := QuoteIdent(in)
		if err != nil {
			t.Errorf("QuoteIdent(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("QuoteIdent(%q) = %s, want %s", in, got, want)
		}
	}

	if _, err := QuoteIdent(""); err == nil {
		t.Error("empty identifier should be rejected")
	}
}

func TestIsDenial(t *testing.T) {
	_, err := Check("DROP TABLE t", readOnlyPolicy(), "db")
	if !IsDenial(err) {
		t.Error("a policy refusal should report IsDenial")
	}

	if IsDenial(errors.New("connection refused")) {
		t.Error("an infrastructure error must not report IsDenial")
	}
}
