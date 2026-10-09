package mcpserver

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cryskram/veridb/internal/config"
	"github.com/cryskram/veridb/internal/database"
	"github.com/cryskram/veridb/internal/schema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// schemaToolsEnabled reports whether any database exposes the schema service.
func schemaToolsEnabled(s config.Services) bool { return s.Schema }

type databaseInfo struct {
	Name        string       `json:"name" jsonschema:"logical name to pass to other tools"`
	Database    string       `json:"database" jsonschema:"physical database name on the server"`
	Description string       `json:"description,omitempty"`
	Tags        []string     `json:"tags,omitempty"`
	Connection  string       `json:"connection"`
	Mode        string       `json:"mode" jsonschema:"read-only or read-write"`
	Services    serviceFlags `json:"services"`
	// SessionOverride is true when write mode was toggled on for this session
	// via set_write_mode; it resets on restart.
	SessionOverride bool `json:"session_override,omitempty"`
}

type listDatabasesOutput struct {
	Databases []databaseInfo `json:"databases"`
	// Skipped lists configured databases that are unavailable here, with the
	// reason, so an agent does not keep retrying a name that will never resolve.
	Skipped []database.Warning `json:"skipped,omitempty"`
}

type listSchemasInput struct {
	Database string `json:"database" jsonschema:"database name from list_databases"`
}

type listSchemasOutput struct {
	Database string   `json:"database"`
	Schemas  []string `json:"schemas"`
}

type listTablesInput struct {
	Database string `json:"database" jsonschema:"database name from list_databases"`
	Schema   string `json:"schema,omitempty" jsonschema:"restrict to a single schema; omit for all schemas"`
}

type listTablesOutput struct {
	Database string         `json:"database"`
	Schema   string         `json:"schema,omitempty"`
	Count    int            `json:"count"`
	Tables   []schema.Table `json:"tables"`
}

type describeTableInput struct {
	Database string `json:"database" jsonschema:"database name from list_databases"`
	Schema   string `json:"schema" jsonschema:"schema containing the table"`
	Table    string `json:"table" jsonschema:"table or view name"`
}

type describeTableOutput struct {
	Database string          `json:"database"`
	ColumnNo int             `json:"column_count"`
	Columns  []schema.Column `json:"columns"`
}

type searchColumnsInput struct {
	Database string `json:"database" jsonschema:"database name from list_databases"`
	Pattern  string `json:"pattern" jsonschema:"case-insensitive substring or ILIKE pattern, e.g. \"pan\" or \"%email%\""`
}

type searchColumnsOutput struct {
	Database string               `json:"database"`
	Count    int                  `json:"count"`
	Matches  []schema.ColumnMatch `json:"matches"`
}

func registerSchemaTools(server *mcp.Server, deps *Deps) {
	mcp.AddTool(
		server,
		&mcp.Tool{
			Name: "list_databases",
			Description: "List the databases VeriDB exposes, with the safety mode and the " +
				"services enabled for each. Call this first. Databases that were configured " +
				"but are missing on their host are reported under `skipped`.",
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listDatabasesOutput, error) {
			out := listDatabasesOutput{
				Databases: make([]databaseInfo, 0, deps.Registry.Len()),
				Skipped:   deps.Registry.Warnings(),
			}

			for _, entry := range deps.Registry.Entries() {
				cfg, err := deps.Registry.EffectiveConfig(entry.Config.Name)
				if err != nil {
					cfg = entry.Config // unreachable: the entry was just listed
				}
				out.Databases = append(out.Databases, databaseInfo{
					Name:            cfg.Name,
					Database:        cfg.Database,
					Description:     cfg.Description,
					Tags:            cfg.Tags,
					Connection:      cfg.Connection,
					Mode:            cfg.Policy.Summarize(),
					Services:        flagsOf(cfg.Services),
					SessionOverride: deps.Registry.WriteMode(entry.Config.Name),
				})
			}

			return nil, out, nil
		},
	)

	if !deps.enabledForAny(schemaToolsEnabled) {
		return
	}

	mcp.AddTool(
		server,
		&mcp.Tool{
			Name:        "list_schemas",
			Description: "List the user-visible schemas of a database.",
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listSchemasInput) (*mcp.CallToolResult, listSchemasOutput, error) {
			if _, _, err := deps.entry(in.Database, "schema", schemaToolsEnabled); err != nil {
				return nil, listSchemasOutput{}, err
			}

			schemas, err := deps.Inspector.ListSchemas(ctx, in.Database)
			if err != nil {
				return nil, listSchemasOutput{}, err
			}

			return nil, listSchemasOutput{Database: in.Database, Schemas: schemas}, nil
		},
	)

	mcp.AddTool(
		server,
		&mcp.Tool{
			Name: "list_tables",
			Description: "List tables, views and materialized views in a database with an " +
				"estimated row count and total size. Estimates come from pg_class.reltuples, so " +
				"they are cheap and approximate: use them to avoid scanning huge tables.",
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listTablesInput) (*mcp.CallToolResult, listTablesOutput, error) {
			if _, _, err := deps.entry(in.Database, "schema", schemaToolsEnabled); err != nil {
				return nil, listTablesOutput{}, err
			}

			tables, err := deps.Inspector.ListTables(ctx, in.Database)
			if err != nil {
				return nil, listTablesOutput{}, err
			}

			if in.Schema != "" {
				filtered := tables[:0]
				for _, t := range tables {
					if strings.EqualFold(t.Schema, in.Schema) {
						filtered = append(filtered, t)
					}
				}
				tables = filtered
			}

			return nil, listTablesOutput{
				Database: in.Database,
				Schema:   in.Schema,
				Count:    len(tables),
				Tables:   tables,
			}, nil
		},
	)

	mcp.AddTool(
		server,
		&mcp.Tool{
			Name: "describe_table",
			Description: "Describe the columns of a table or view: type, nullability, default, " +
				"comment and whether the column is part of the primary key.",
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, in describeTableInput) (*mcp.CallToolResult, describeTableOutput, error) {
			if _, _, err := deps.entry(in.Database, "schema", schemaToolsEnabled); err != nil {
				return nil, describeTableOutput{}, err
			}
			if in.Schema == "" || in.Table == "" {
				return nil, describeTableOutput{}, fmt.Errorf("schema and table are required")
			}

			columns, err := deps.Inspector.DescribeTable(ctx, in.Database, in.Schema, in.Table)
			if err != nil {
				return nil, describeTableOutput{}, err
			}
			if len(columns) == 0 {
				return nil, describeTableOutput{}, fmt.Errorf(
					"no relation %s.%s found in database %q (it may be excluded or spelled differently)",
					in.Schema, in.Table, in.Database,
				)
			}

			return nil, describeTableOutput{
				Database: in.Database,
				ColumnNo: len(columns),
				Columns:  columns,
			}, nil
		},
	)

	mcp.AddTool(
		server,
		&mcp.Tool{
			Name: "search_columns",
			Description: "Find columns by name across every table in a database, e.g. to locate " +
				"PII or a foreign key before writing a query. Accepts a substring or an ILIKE pattern.",
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, in searchColumnsInput) (*mcp.CallToolResult, searchColumnsOutput, error) {
			if _, _, err := deps.entry(in.Database, "schema", schemaToolsEnabled); err != nil {
				return nil, searchColumnsOutput{}, err
			}
			if strings.TrimSpace(in.Pattern) == "" {
				return nil, searchColumnsOutput{}, fmt.Errorf("pattern is required")
			}

			matches, err := deps.Inspector.SearchColumns(ctx, in.Database, in.Pattern)
			if err != nil {
				return nil, searchColumnsOutput{}, err
			}

			sort.Slice(matches, func(i, j int) bool {
				if matches[i].Schema != matches[j].Schema {
					return matches[i].Schema < matches[j].Schema
				}
				if matches[i].Table != matches[j].Table {
					return matches[i].Table < matches[j].Table
				}
				return matches[i].Column < matches[j].Column
			})

			return nil, searchColumnsOutput{
				Database: in.Database,
				Count:    len(matches),
				Matches:  matches,
			}, nil
		},
	)
}
