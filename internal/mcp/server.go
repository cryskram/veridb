package mcpserver

import (
	"fmt"

	"github.com/cryskram/veridb/internal/audit"
	"github.com/cryskram/veridb/internal/config"
	"github.com/cryskram/veridb/internal/database"
	"github.com/cryskram/veridb/internal/schema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Deps is everything the MCP tools need from the rest of VeriDB.
type Deps struct {
	Server    config.ServerConfig
	Registry  *database.Registry
	Inspector *schema.Inspector
	Audit     *audit.Logger
}

// NewServer builds the MCP server and registers every tool group that at least
// one database has enabled. Per-database gating happens inside each handler, so
// a tool can exist globally while remaining refused for a specific database.
func NewServer(deps *Deps) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    deps.Server.Name,
			Version: deps.Server.Version,
		},
		&mcp.ServerOptions{
			Instructions: deps.instructions(),
		},
	)

	registerSchemaTools(server, deps)
	registerQueryTools(server, deps)
	registerWriteTools(server, deps)
	registerStatusTools(server, deps)

	return server
}

func (d *Deps) instructions() string {
	if d.Server.Instructions != "" {
		return d.Server.Instructions
	}

	return "VeriDB exposes a policy-controlled set of databases. " +
		"Call list_databases first to see which databases are available and which " +
		"services are enabled for each of them. Read tools are bounded by the " +
		"server-side row limit and statement timeout configured by the operator."
}

// enabledForAny reports whether at least one registered database has the named
// service on, which decides whether a tool is registered at all.
func (d *Deps) enabledForAny(enabled func(config.Services) bool) bool {
	for _, entry := range d.Registry.Entries() {
		if enabled(entry.Config.Services) {
			return true
		}
	}
	return false
}

// entry loads a database entry and verifies the requested service is enabled
// for it, returning an agent-friendly error when it is not.
func (d *Deps) entry(databaseName, service string, enabled func(config.Services) bool) (*database.Entry, error) {
	entry, err := d.Registry.Get(databaseName)
	if err != nil {
		return nil, err
	}

	if !enabled(entry.Config.Services) {
		return nil, fmt.Errorf(
			"the %q service is disabled for database %q; "+
				"enable services.%s in the VeriDB config for that database to use this tool",
			service, databaseName, service,
		)
	}

	return entry, nil
}

// serviceFlags is the JSON shape of a database's enabled services.
type serviceFlags struct {
	Schema  bool `json:"schema"`
	Query   bool `json:"query"`
	Explain bool `json:"explain"`
	Sample  bool `json:"sample"`
	Health  bool `json:"health"`
	Write   bool `json:"write"`
	Admin   bool `json:"admin"`
}

func flagsOf(s config.Services) serviceFlags {
	return serviceFlags{
		Schema:  s.Schema,
		Query:   s.Query,
		Explain: s.Explain,
		Sample:  s.Sample,
		Health:  s.Health,
		Write:   s.Write,
		Admin:   s.Admin,
	}
}
