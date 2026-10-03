package schema

import (
	"context"
	"fmt"
	"strings"

	"github.com/cryskram/veridb/internal/database"
)

// Inspector answers schema questions about a registered database. Every query
// it issues goes through the driver, so it inherits the configured statement
// timeout and read-only enforcement.
type Inspector struct {
	registry *database.Registry
}

// NewInspector builds an Inspector over the registry.
func NewInspector(registry *database.Registry) *Inspector {
	return &Inspector{registry: registry}
}

// Table summarises a relation without touching its data.
type Table struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	// EstimatedRows comes from pg_class.reltuples and is -1 when never analysed.
	EstimatedRows int64 `json:"estimated_rows"`
	// TotalBytes includes indexes and TOAST.
	TotalBytes int64 `json:"total_bytes"`
}

// Column describes one column of a relation.
type Column struct {
	Schema      string `json:"schema"`
	Table       string `json:"table"`
	Name        string `json:"name"`
	Position    int    `json:"position"`
	DataType    string `json:"data_type"`
	Nullable    bool   `json:"nullable"`
	Default     string `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
	PrimaryKey  bool   `json:"primary_key"`
}

// ListSchemas returns the user-visible schemas of a database.
func (i *Inspector) ListSchemas(ctx context.Context, databaseName string) ([]string, error) {
	entry, err := i.registry.Get(databaseName)
	if err != nil {
		return nil, err
	}

	rs, err := entry.Driver.Query(ctx, `
		SELECT nspname
		FROM pg_namespace
		WHERE nspname NOT LIKE 'pg\_%'
		  AND nspname <> 'information_schema'
		ORDER BY nspname
	`)
	if err != nil {
		return nil, fmt.Errorf("list schemas: %w", err)
	}

	schemas := make([]string, 0, rs.RowCount)
	for _, row := range rs.Rows {
		if len(row) == 0 {
			continue
		}
		if name, ok := row[0].(string); ok {
			schemas = append(schemas, name)
		}
	}

	return schemas, nil
}

// ListTables returns tables, views, materialized views and foreign tables with
// their estimated size. reltuples is used instead of COUNT(*) so this stays
// cheap on databases with millions of rows.
func (i *Inspector) ListTables(ctx context.Context, databaseName string) ([]Table, error) {
	entry, err := i.registry.Get(databaseName)
	if err != nil {
		return nil, err
	}

	rs, err := entry.Driver.Query(ctx, `
		SELECT n.nspname,
		       c.relname,
		       CASE c.relkind
		         WHEN 'r' THEN 'table'
		         WHEN 'p' THEN 'partitioned_table'
		         WHEN 'f' THEN 'foreign_table'
		         WHEN 'v' THEN 'view'
		         WHEN 'm' THEN 'materialized_view'
		         ELSE c.relkind::text
		       END,
		       c.reltuples::bigint,
		       pg_total_relation_size(c.oid)::bigint
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p', 'f', 'v', 'm')
		  AND n.nspname NOT LIKE 'pg\_%'
		  AND n.nspname <> 'information_schema'
		ORDER BY n.nspname, c.relname
	`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}

	tables := make([]Table, 0, rs.RowCount)
	for _, row := range rs.Rows {
		if len(row) < 5 {
			continue
		}
		tables = append(tables, Table{
			Schema:        asString(row[0]),
			Name:          asString(row[1]),
			Kind:          asString(row[2]),
			EstimatedRows: asInt64(row[3]),
			TotalBytes:    asInt64(row[4]),
		})
	}

	return tables, nil
}

// DescribeTable returns the columns of a relation, including nullability,
// defaults, comments and primary-key membership.
func (i *Inspector) DescribeTable(
	ctx context.Context,
	databaseName string,
	schemaName string,
	tableName string,
) ([]Column, error) {
	entry, err := i.registry.Get(databaseName)
	if err != nil {
		return nil, err
	}

	rs, err := entry.Driver.Query(ctx, `
		SELECT n.nspname,
		       c.relname,
		       a.attname,
		       a.attnum::int,
		       pg_catalog.format_type(a.atttypid, a.atttypmod),
		       NOT a.attnotnull,
		       COALESCE(pg_get_expr(d.adbin, d.adrelid), ''),
		       COALESCE(col_description(a.attrelid, a.attnum), ''),
		       COALESCE(a.attnum = ANY(pk.conkey), false)
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		LEFT JOIN pg_constraint pk ON pk.conrelid = a.attrelid AND pk.contype = 'p'
		WHERE n.nspname = $1
		  AND c.relname = $2
		  AND a.attnum > 0
		  AND NOT a.attisdropped
		ORDER BY a.attnum
	`, schemaName, tableName)
	if err != nil {
		return nil, fmt.Errorf("describe table: %w", err)
	}

	columns := make([]Column, 0, rs.RowCount)
	for _, row := range rs.Rows {
		if len(row) < 9 {
			continue
		}
		columns = append(columns, Column{
			Schema:      asString(row[0]),
			Table:       asString(row[1]),
			Name:        asString(row[2]),
			Position:    int(asInt64(row[3])),
			DataType:    asString(row[4]),
			Nullable:    asBool(row[5]),
			Default:     asString(row[6]),
			Description: asString(row[7]),
			PrimaryKey:  asBool(row[8]),
		})
	}

	return columns, nil
}

// ColumnMatch is a hit from SearchColumns.
type ColumnMatch struct {
	Schema string `json:"schema"`
	Table  string `json:"table"`
	Column string `json:"column"`
	Type   string `json:"data_type"`
}

// SearchColumns finds columns whose name matches a case-insensitive pattern.
// The pattern is matched with ILIKE, so "%pan%" style wildcards are accepted.
func (i *Inspector) SearchColumns(ctx context.Context, databaseName, pattern string) ([]ColumnMatch, error) {
	entry, err := i.registry.Get(databaseName)
	if err != nil {
		return nil, err
	}

	if !strings.Contains(pattern, "%") {
		pattern = "%" + pattern + "%"
	}

	rs, err := entry.Driver.Query(ctx, `
		SELECT n.nspname,
		       c.relname,
		       a.attname,
		       pg_catalog.format_type(a.atttypid, a.atttypmod)
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f')
		  AND n.nspname NOT LIKE 'pg\_%'
		  AND n.nspname <> 'information_schema'
		  AND a.attnum > 0
		  AND NOT a.attisdropped
		  AND a.attname ILIKE $1
		ORDER BY n.nspname, c.relname, a.attnum
	`, pattern)
	if err != nil {
		return nil, fmt.Errorf("search columns: %w", err)
	}

	matches := make([]ColumnMatch, 0, rs.RowCount)
	for _, row := range rs.Rows {
		if len(row) < 4 {
			continue
		}
		matches = append(matches, ColumnMatch{
			Schema: asString(row[0]),
			Table:  asString(row[1]),
			Column: asString(row[2]),
			Type:   asString(row[3]),
		})
	}

	return matches, nil
}
