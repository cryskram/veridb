package guard

import (
	"strings"

	"github.com/cryskram/veridb/internal/config"
	"github.com/cryskram/veridb/internal/glob"
)

// Enforce applies a database policy to an analysed statement. database is used
// only to make the refusal message actionable.
func Enforce(stmt Statement, pol config.Policy, database string) error {
	if err := enforceKeywords(stmt, pol, database); err != nil {
		return err
	}

	if err := enforceTables(stmt, pol, database); err != nil {
		return err
	}

	return enforceKind(stmt, pol, database)
}

// Check analyses and enforces in one step, which is what tools call.
func Check(sql string, pol config.Policy, database string) (Statement, error) {
	stmt, err := Analyze(sql)
	if err != nil {
		return stmt, err
	}

	if err := Enforce(stmt, pol, database); err != nil {
		return stmt, err
	}

	return stmt, nil
}

func enforceKeywords(stmt Statement, pol config.Policy, database string) error {
	if len(pol.DenyKeywords) == 0 {
		return nil
	}

	// The skeleton has comments and literals removed, so a keyword cannot be
	// smuggled in a string and cannot be spoofed by a comment.
	haystack := strings.ToUpper(stmt.Skeleton)

	for _, keyword := range pol.DenyKeywords {
		keyword = strings.TrimSpace(keyword)
		if keyword == "" {
			continue
		}

		if strings.Contains(haystack, strings.ToUpper(keyword)) {
			return denf(
				"keyword",
				"refused: the statement uses %q, which is blocked for database %q by deny_keywords",
				keyword, database,
			)
		}
	}

	return nil
}

func enforceTables(stmt Statement, pol config.Policy, database string) error {
	for _, table := range stmt.Tables {
		if err := enforceSchema(table, pol, database); err != nil {
			return err
		}

		if len(pol.DenyTables) > 0 && matchesAnyTable(pol.DenyTables, table) {
			return denf(
				"table",
				"refused: table %q is excluded for database %q by deny_tables",
				table.String(), database,
			)
		}
	}

	return nil
}

func enforceSchema(table QualifiedName, pol config.Policy, database string) error {
	// An unqualified name resolves through search_path, which normally starts
	// at public. Treating it as public is the useful assumption and makes
	// allow_schemas fail closed for unqualified references.
	schema := table.Schema
	if schema == "" {
		schema = "public"
	}

	for _, denied := range pol.DenySchemas {
		if strings.EqualFold(denied, schema) {
			return denf(
				"schema",
				"refused: schema %q is not readable in database %q (deny_schemas). "+
					"Use the schema tools (list_tables, describe_table) to inspect it instead",
				schema, database,
			)
		}
	}

	if len(pol.AllowSchemas) == 0 {
		return nil
	}

	for _, allowed := range pol.AllowSchemas {
		if strings.EqualFold(allowed, schema) {
			return nil
		}
	}

	return denf(
		"schema",
		"refused: schema %q is outside allow_schemas for database %q (allowed: %s)",
		schema, database, strings.Join(pol.AllowSchemas, ", "),
	)
}

// matchesAnyTable tests a pattern against the bare name and the schema-qualified
// name, so both "*.password_resets" and "public.password_resets" work.
func matchesAnyTable(patterns []string, table QualifiedName) bool {
	return glob.MatchName(patterns, table.Name, table.String())
}

func enforceKind(stmt Statement, pol config.Policy, database string) error {
	if pol.ReadOnly {
		if stmt.Kind == KindRead {
			return nil
		}
		return denf(
			"read_only",
			"refused: database %q is read-only, so %s is not permitted. "+
				"Set read_only: false plus the matching allow_* flag in the VeriDB config to permit it",
			database, stmt.Operation(),
		)
	}

	switch stmt.Kind {
	case KindRead:
		return nil

	case KindWrite:
		if !pol.AllowWrite {
			return denf(
				"not_allowed",
				"refused: INSERT/UPDATE is not allowed on database %q (allow_write is false)",
				database,
			)
		}

	case KindDelete:
		if !pol.AllowDelete {
			return denf(
				"not_allowed",
				"refused: DELETE is not allowed on database %q (allow_delete is false)",
				database,
			)
		}

	case KindDDL:
		if stmt.Truncating && !pol.AllowTruncate {
			return denf(
				"not_allowed",
				"refused: TRUNCATE is not allowed on database %q (allow_truncate is false)",
				database,
			)
		}
		if !pol.AllowDDL {
			return denf(
				"not_allowed",
				"refused: schema changes are not allowed on database %q (allow_ddl is false)",
				database,
			)
		}

	case KindAdmin:
		if !pol.AllowDDL {
			return denf(
				"not_allowed",
				"refused: %s needs administrative access, which is not enabled for database %q "+
					"(set allow_ddl: true and enable the admin service)",
				stmt.Operation(), database,
			)
		}
	}

	if pol.RequireWhereForMutation && !stmt.HasWhere &&
		(stmt.Kind == KindWrite || stmt.Kind == KindDelete) {
		return denf(
			"missing_where",
			"refused: this %s has no WHERE clause, and database %q requires one "+
				"(require_where_for_mutation is on)",
			stmt.Operation(), database,
		)
	}

	return nil
}

// Operation names the statement in a way an operator or agent recognises, using
// the verb that determined the risk class.
func (s Statement) Operation() string {
	if s.Truncating {
		return "TRUNCATE"
	}

	if s.Verb != "" {
		return s.Verb
	}

	switch s.Kind {
	case KindRead:
		return "reads"
	case KindWrite:
		return "this write"
	case KindDelete:
		return "DELETE"
	case KindDDL:
		return "schema changes"
	case KindAdmin:
		return "this statement"
	}

	return "this statement"
}
