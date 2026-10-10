package config

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the complete VeriDB control plane, loaded from a single YAML file.
//
//	server:      identity advertised over MCP
//	defaults:    policy applied to every database
//	services:    which tool groups exist by default
//	connections: named connection targets (hosts / DSNs)
//	databases:   the explicit allowlist of databases to expose
//	audit:       where tool calls are recorded
type Config struct {
	Server      ServerConfig                `yaml:"server"`
	Defaults    *PolicyOverride             `yaml:"defaults"`
	Services    *ServicesConfig             `yaml:"services"`
	Connections map[string]ConnectionConfig `yaml:"connections"`
	Databases   map[string]DatabaseConfig   `yaml:"databases"`
	Audit       AuditConfig                 `yaml:"audit"`
	// Path is the file this config was loaded from. Relative paths inside
	// the config (currently the audit file) resolve against its directory,
	// so the server behaves the same no matter which working directory an
	// MCP client launches it from.
	Path string `yaml:"-"`
}

// ServerConfig describes the MCP server identity.
type ServerConfig struct {
	Name         string `yaml:"name"`
	Version      string `yaml:"version"`
	Instructions string `yaml:"instructions"`
}

// ConnectionConfig describes how to reach one host. Credentials are always
// resolved from the environment, never inlined in the config file.
type ConnectionConfig struct {
	// Driver selects the adapter. Only "postgres" is supported today.
	Driver string `yaml:"driver"`
	// DSN is an inline DSN. It may contain a {database} placeholder, which is
	// substituted with the physical database name.
	DSN string `yaml:"dsn"`
	// DSNEnv names an environment variable holding the DSN. Takes precedence
	// over Host/Port/User/PasswordEnv when set. Supports {database}.
	DSNEnv string `yaml:"dsn_env"`
	// Host/Port/User describe the server when no DSN is supplied.
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	User string `yaml:"user"`
	// PasswordEnv names an environment variable holding the password.
	PasswordEnv string `yaml:"password_env"`
	// SSLMode is passed through as the sslmode connection parameter.
	SSLMode string `yaml:"ssl_mode"`
	// MaintenanceDatabase is the database used to verify which databases exist.
	// Defaults to "postgres".
	MaintenanceDatabase string     `yaml:"maintenance_database"`
	Pool                PoolConfig `yaml:"pool"`
}

// PoolConfig bounds the pgx connection pool.
type PoolConfig struct {
	MaxConns          *int32    `yaml:"max_conns"`
	MinConns          *int32    `yaml:"min_conns"`
	MaxConnLifetime   *Duration `yaml:"max_conn_lifetime"`
	MaxConnIdleTime   *Duration `yaml:"max_conn_idle_time"`
	HealthCheckPeriod *Duration `yaml:"health_check_period"`
}

// DatabaseConfig describes one database to expose. Policy options are embedded
// inline so an operator can override a single field without restating the rest.
type DatabaseConfig struct {
	// Enabled defaults to true. Set false to keep an entry documented but off.
	Enabled *bool `yaml:"enabled"`
	// Connection names an entry in the top-level `connections` map.
	Connection string `yaml:"connection"`
	// Database is the physical database name. Defaults to the config key, so
	// `app:` needs nothing extra unless the real name differs.
	Database    string   `yaml:"database"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags"`
	// Services overrides the global service defaults for this database.
	Services *ServicesConfig `yaml:"services"`

	PolicyOverride `yaml:",inline"`
}

// AuditConfig controls call recording.
type AuditConfig struct {
	Enabled *bool `yaml:"enabled"`
	// Sinks lists where records go. Supported: "stderr", "file".
	Sinks []string `yaml:"sinks"`
	// File is the JSONL destination used by the "file" sink.
	File          string `yaml:"file"`
	IncludeSQL    *bool  `yaml:"include_sql"`
	IncludeParams *bool  `yaml:"include_params"`
	MaxValueLen   *int   `yaml:"max_value_len"`
}

// SupportedAuditSinks lists the sink names VeriDB understands. The viewer added
// later consumes the file sink, which is why "file" writes JSONL.
func SupportedAuditSinks() []string { return []string{"stderr", "file"} }

// ResolvedAudit is AuditConfig with defaults applied.
type ResolvedAudit struct {
	Enabled       bool
	Sinks         []string
	File          string
	IncludeSQL    bool
	IncludeParams bool
	MaxValueLen   int
}

// HasSink reports whether the named sink is enabled.
func (a ResolvedAudit) HasSink(name string) bool {
	for _, s := range a.Sinks {
		if strings.EqualFold(s, name) {
			return true
		}
	}
	return false
}

// DefaultConfigPath is used when VERIDB_CONFIG is unset.
const DefaultConfigPath = "configs/veridb.yaml"

// Load reads path, parses it and appends defaults. Environment references are
// resolved per field during resolution, never by rewriting the file, so a
// ${VAR} written inside a comment or a description is left alone.
func Load(path string) (*Config, error) {
	return LoadWithEnv(path, os.LookupEnv)
}

// LoadWithEnv is Load with an injectable environment, used by tests.
func LoadWithEnv(path string, lookup EnvLookup) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	if err := cfg.ExpandEnv(lookup); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}

	if cfg.Server.Name == "" {
		cfg.Server.Name = "veridb"
	}
	if cfg.Server.Version == "" {
		cfg.Server.Version = "0.0.0"
	}

	cfg.Path = path

	return &cfg, nil
}

// ExpandEnv substitutes ${VAR} references in the fields that VeriDB resolves
// itself. Only these fields are expanded; free text such as descriptions and
// instructions is never rewritten.
func (c *Config) ExpandEnv(lookup EnvLookup) error {
	for name, conn := range c.Connections {
		fields := map[string]*string{
			"dsn":                  &conn.DSN,
			"dsn_env":              &conn.DSNEnv,
			"host":                 &conn.Host,
			"user":                 &conn.User,
			"password_env":         &conn.PasswordEnv,
			"ssl_mode":             &conn.SSLMode,
			"maintenance_database": &conn.MaintenanceDatabase,
		}

		for field, ptr := range fields {
			expanded, err := ExpandEnvWith(*ptr, lookup)
			if err != nil {
				return fmt.Errorf("connection %q: %s: %w", name, field, err)
			}
			*ptr = expanded
		}

		c.Connections[name] = conn
	}

	for name, db := range c.Databases {
		expanded, err := ExpandEnvWith(db.Database, lookup)
		if err != nil {
			return fmt.Errorf("database %q: database: %w", name, err)
		}
		db.Database = expanded
		c.Databases[name] = db
	}

	expanded, err := ExpandEnvWith(c.Audit.File, lookup)
	if err != nil {
		return fmt.Errorf("audit: file: %w", err)
	}
	c.Audit.File = expanded

	return nil
}

// Validate checks structural correctness. It reports every problem it can find
// rather than stopping at the first, so a fresh config can be fixed in one pass.
func (c *Config) Validate() error {
	var problems []string

	if len(c.Connections) == 0 {
		problems = append(problems, "no connections defined")
	}
	if len(c.Databases) == 0 {
		problems = append(problems, "no databases defined")
	}

	for name, conn := range c.Connections {
		if conn.Driver == "" {
			problems = append(problems, fmt.Sprintf("connection %q: driver is required", name))
		} else if !IsSupportedDriver(conn.Driver) {
			problems = append(problems, fmt.Sprintf(
				"connection %q: unsupported driver %q (supported: %s)",
				name, conn.Driver, strings.Join(SupportedDrivers(), ", "),
			))
		}

		hasDSN := conn.DSN != "" || conn.DSNEnv != ""
		if !hasDSN && conn.Host == "" {
			problems = append(problems, fmt.Sprintf(
				"connection %q: set dsn, dsn_env or host", name,
			))
		}
		if !hasDSN && conn.User == "" {
			problems = append(problems, fmt.Sprintf(
				"connection %q: user is required when building a DSN from parts", name,
			))
		}
		if conn.Port < 0 || conn.Port > 65535 {
			problems = append(problems, fmt.Sprintf(
				"connection %q: port %d is out of range", name, conn.Port,
			))
		}
	}

	for name, db := range c.Databases {
		if db.Connection == "" {
			problems = append(problems, fmt.Sprintf("database %q: connection is required", name))
			continue
		}
		if _, ok := c.Connections[db.Connection]; !ok {
			problems = append(problems, fmt.Sprintf(
				"database %q: unknown connection %q", name, db.Connection,
			))
		}
		if db.Enabled != nil && !*db.Enabled {
			continue // disabled entries are exempt from further checks
		}
		if db.PolicyOverride.StatementTimeout != nil && db.PolicyOverride.StatementTimeout.Std() < 0 {
			problems = append(problems, fmt.Sprintf(
				"database %q: statement_timeout must not be negative", name,
			))
		}
		if db.PolicyOverride.MaxRows != nil && *db.PolicyOverride.MaxRows < 0 {
			problems = append(problems, fmt.Sprintf(
				"database %q: max_rows must not be negative", name,
			))
		}
	}

	for _, sink := range c.Audit.Sinks {
		if !isSupportedAuditSink(sink) {
			problems = append(problems, fmt.Sprintf(
				"audit: unknown sink %q (supported: %s)",
				sink, strings.Join(SupportedAuditSinks(), ", "),
			))
		}
	}
	if len(c.Audit.Sinks) > 0 && containsFold(c.Audit.Sinks, "file") && c.Audit.File == "" {
		problems = append(problems, "audit: sink is file but no file path is set")
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("invalid config:\n  - %s", strings.Join(problems, "\n  - "))
	}

	return nil
}
