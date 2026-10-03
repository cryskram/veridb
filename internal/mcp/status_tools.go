package mcpserver

import (
	"context"
	"sort"
	"sync"

	"github.com/cryskram/veridb/internal/database"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type statusInput struct {
	// Probe enables a live ping and version read per database. Disable it when
	// you only want the configured view and do not want to touch the servers.
	Probe bool `json:"probe,omitempty" jsonschema:"ping each database and read its version (default false)"`
}

type databaseStatus struct {
	Name     string       `json:"name"`
	Database string       `json:"database"`
	Mode     string       `json:"mode"`
	Services serviceFlags `json:"services"`
	Healthy  *bool        `json:"healthy,omitempty"`
	Version  string       `json:"version,omitempty"`
	Error    string       `json:"error,omitempty"`
}

type statusOutput struct {
	Server      string             `json:"server"`
	Version     string             `json:"version"`
	Connected   int                `json:"connected"`
	Databases   []databaseStatus   `json:"databases"`
	Unavailable []database.Warning `json:"unavailable,omitempty"`
}

func registerStatusTools(server *mcp.Server, deps *Deps) {
	mcp.AddTool(
		server,
		&mcp.Tool{
			Name: "veridb_status",
			Description: "Report VeriDB's own state: which databases connected, which were " +
				"skipped and why, and (with probe=true) whether each server is reachable. " +
				"Use this when a database name from the config is missing.",
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, in statusInput) (*mcp.CallToolResult, statusOutput, error) {
			entries := deps.Registry.Entries()

			out := statusOutput{
				Server:      deps.Server.Name,
				Version:     deps.Server.Version,
				Connected:   len(entries),
				Databases:   make([]databaseStatus, len(entries)),
				Unavailable: deps.Registry.Warnings(),
			}

			var wg sync.WaitGroup
			for i, entry := range entries {
				status := databaseStatus{
					Name:     entry.Config.Name,
					Database: entry.Config.Database,
					Mode:     entry.Config.Policy.Summarize(),
					Services: flagsOf(entry.Config.Services),
				}
				out.Databases[i] = status

				if in.Probe && entry.Config.Services.Health {
					wg.Add(1)
					go func(i int, entry *database.Entry) {
						defer wg.Done()
						probe(ctx, entry, &out.Databases[i])
					}(i, entry)
				}
			}
			wg.Wait()

			sort.Slice(out.Databases, func(i, j int) bool {
				return out.Databases[i].Name < out.Databases[j].Name
			})

			return nil, out, nil
		},
	)
}

// probe fills in the live health fields for one database. Writing to distinct
// slice elements from separate goroutines is safe.
func probe(ctx context.Context, entry *database.Entry, status *databaseStatus) {
	healthy := entry.Driver.Ping(ctx) == nil
	status.Healthy = &healthy

	if !healthy {
		return
	}

	version, err := entry.Driver.ServerVersion(ctx)
	if err != nil {
		status.Error = err.Error()
		return
	}
	status.Version = version
}
