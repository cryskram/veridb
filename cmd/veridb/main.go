package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/cryskram/veridb/internal/audit"
	"github.com/cryskram/veridb/internal/config"
	"github.com/cryskram/veridb/internal/database"
	mcpserver "github.com/cryskram/veridb/internal/mcp"
	"github.com/cryskram/veridb/internal/schema"
	"github.com/joho/godotenv"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("veridb: %v", err)
	}
}

func run() error {
	ctx := context.Background()

	configPath := flag.String("config", defaultConfigPath(), "path to the VeriDB config file")
	envFile := flag.String("env-file", os.Getenv("VERIDB_ENV_FILE"),
		"environment file to load before resolving credentials (VERIDB_ENV_FILE)")
	flag.Parse()

	// Load credentials. An explicit --env-file is authoritative, because an
	// agent may launch VeriDB from a different working directory than the
	// checkout, where a relative .env would not be found.
	switch {
	case *envFile != "":
		if err := godotenv.Load(*envFile); err != nil {
			return fmt.Errorf("load env file %s: %w", *envFile, err)
		}

	default:
		// devenv/direnv normally provide the environment; godotenv covers
		// running the binary directly from a checkout.
		if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
			log.Printf("no .env file loaded: %v", err)
		}
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	resolved, err := cfg.Resolve()
	if err != nil {
		return err
	}

	registry, err := database.Build(ctx, resolved, log.Printf)
	if err != nil {
		return err
	}
	defer registry.Close()

	auditLog, err := audit.New(resolved.Audit, os.Stderr)
	if err != nil {
		return err
	}
	defer auditLog.Close()

	if auditLog.Enabled() {
		log.Printf("veridb: audit enabled, sinks: %v", resolved.Audit.Sinks)
	}

	// Surface skipped databases loudly on stderr: MCP clients show server
	// stderr, and a silently missing database is the most confusing failure.
	for _, warning := range registry.Warnings() {
		log.Printf("veridb: %s: %s", warning.Level, warning.Message)
	}

	if registry.Len() == 0 {
		log.Printf("veridb: warning: no databases are available; the server will start anyway " +
			"so veridb_status can explain why")
	}

	server := mcpserver.NewServer(&mcpserver.Deps{
		Server:    resolved.Server,
		Registry:  registry,
		Inspector: schema.NewInspector(registry),
		Audit:     auditLog,
	})

	log.Printf("veridb: %s %s serving %d database(s) over stdio",
		resolved.Server.Name, resolved.Server.Version, registry.Len())

	return server.Run(ctx, &mcp.StdioTransport{})
}

func defaultConfigPath() string {
	if path := os.Getenv("VERIDB_CONFIG"); path != "" {
		return path
	}
	return config.DefaultConfigPath
}
