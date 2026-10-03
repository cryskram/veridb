package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
)

// databasePlaceholder is substituted with the physical database name inside a
// DSN template. This lets one connection entry serve many databases:
//
//	dsn_env: VERIDB_PRIMARY_DSN
//	# VERIDB_PRIMARY_DSN=postgres://USER:PASSWORD@HOST:PORT/{database}
const databasePlaceholder = "{database}"

// EnvLookup resolves an environment variable. Injectable for tests.
type EnvLookup func(string) (string, bool)

// Resolved is the effective configuration VeriDB runs with: defaults merged
// into every database, DSNs resolved from the environment.
type Resolved struct {
	Server    ServerConfig
	Audit     ResolvedAudit
	Databases map[string]ResolvedDatabase
	// Order lists database names alphabetically for stable output.
	Order []string
}

// ResolvedDatabase is a single database with its effective policy, services and
// connection details.
type ResolvedDatabase struct {
	// Name is the logical name used in config and tool calls.
	Name string
	// Database is the physical database name on the server.
	Database    string
	Description string
	Tags        []string
	Connection  string
	Driver      string
	// DSN connects to Database.
	DSN string
	// MaintenanceDSN connects to the maintenance database and is used to check
	// which databases exist on the host.
	MaintenanceDSN string
	Pool           PoolConfig
	Policy         Policy
	Services       Services
}

// Enabled reports whether any tool group is available for this database.
func (d ResolvedDatabase) Enabled() bool { return d.Services.Enabled() > 0 }

// Resolve merges defaults, overrides and environment into a runtime view.
func (c *Config) Resolve() (*Resolved, error) {
	return c.ResolveWithEnv(os.LookupEnv)
}

// ResolveWithEnv is Resolve with an injectable environment for tests.
func (c *Config) ResolveWithEnv(lookup EnvLookup) (*Resolved, error) {
	basePolicy := DefaultPolicy()
	basePolicy.Apply(c.Defaults)

	baseServices := DefaultServices()
	baseServices.Apply(c.Services)

	out := &Resolved{
		Server:    c.Server,
		Audit:     c.resolveAudit(),
		Databases: make(map[string]ResolvedDatabase, len(c.Databases)),
	}

	// Count how many enabled databases use each connection. A connection whose
	// DSN has no {database} placeholder may only serve one database, otherwise
	// every entry would silently point at the same place.
	usage := map[string]int{}
	for _, db := range c.Databases {
		if db.Enabled != nil && !*db.Enabled {
			continue
		}
		usage[db.Connection]++
	}

	names := make([]string, 0, len(c.Databases))
	for name := range c.Databases {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		db := c.Databases[name]

		if db.Enabled != nil && !*db.Enabled {
			continue
		}

		conn, ok := c.Connections[db.Connection]
		if !ok {
			return nil, fmt.Errorf("database %q: unknown connection %q", name, db.Connection)
		}

		physical := db.Database
		if physical == "" {
			physical = name
		}

		dsn, template, err := resolveDSN(conn, physical, lookup)
		if err != nil {
			return nil, fmt.Errorf("database %q: %w", name, err)
		}

		if usesExplicitDSN(conn) && !strings.Contains(template, databasePlaceholder) && usage[db.Connection] > 1 {
			return nil, fmt.Errorf(
				"database %q: connection %q is shared by %d databases but its DSN has no %s placeholder; "+
					"add %s to the DSN or give each database its own connection",
				name, db.Connection, usage[db.Connection], databasePlaceholder, databasePlaceholder,
			)
		}

		maintenanceDB := conn.MaintenanceDatabase
		if maintenanceDB == "" {
			maintenanceDB = "postgres"
		}
		maintenanceDSN, _, err := resolveDSN(conn, maintenanceDB, lookup)
		if err != nil {
			return nil, fmt.Errorf("database %q: maintenance connection: %w", name, err)
		}

		policy := basePolicy
		applyPolicySlices(&policy, &basePolicy)
		policy.Apply(&db.PolicyOverride)

		services := baseServices
		services.Apply(db.Services)

		out.Databases[name] = ResolvedDatabase{
			Name:           name,
			Database:       physical,
			Description:    db.Description,
			Tags:           db.Tags,
			Connection:     db.Connection,
			Driver:         conn.Driver,
			DSN:            dsn,
			MaintenanceDSN: maintenanceDSN,
			Pool:           conn.Pool,
			Policy:         policy,
			Services:       services,
		}
		out.Order = append(out.Order, name)
	}

	return out, nil
}

// applyPolicySlices deep-copies the inherited slices so that a per-database
// override cannot mutate the shared default (Policy.Apply replaces slices
// wholesale, but a caller could still append to the inherited backing array).
func applyPolicySlices(dst *Policy, src *Policy) {
	dst.AllowSchemas = cloneStrings(src.AllowSchemas)
	dst.DenySchemas = cloneStrings(src.DenySchemas)
	dst.DenyTables = cloneStrings(src.DenyTables)
	dst.DenyKeywords = cloneStrings(src.DenyKeywords)
	dst.RedactColumns = cloneStrings(src.RedactColumns)
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func (c *Config) resolveAudit() ResolvedAudit {
	a := ResolvedAudit{
		Enabled:       true,
		Sinks:         []string{"stderr"},
		IncludeSQL:    true,
		IncludeParams: false,
		MaxValueLen:   512,
	}

	setBool(&a.Enabled, c.Audit.Enabled)
	if len(c.Audit.Sinks) > 0 {
		a.Sinks = make([]string, 0, len(c.Audit.Sinks))
		for _, sink := range c.Audit.Sinks {
			a.Sinks = append(a.Sinks, strings.ToLower(strings.TrimSpace(sink)))
		}
	}
	if c.Audit.File != "" {
		a.File = c.Audit.File
	}
	setBool(&a.IncludeSQL, c.Audit.IncludeSQL)
	setBool(&a.IncludeParams, c.Audit.IncludeParams)
	setInt(&a.MaxValueLen, c.Audit.MaxValueLen)

	return a
}

// usesExplicitDSN reports whether the connection supplies a full DSN (as opposed
// to host/port/user parts). Only explicit DSNs need a {database} placeholder to
// be reusable, because VeriDB builds a fresh DSN per database for the parts form.
func usesExplicitDSN(conn ConnectionConfig) bool {
	return conn.DSNEnv != "" || conn.DSN != ""
}

// resolveDSN returns both the DSN for the given database and the raw template it
// was derived from, so callers can inspect placeholder usage.
func resolveDSN(conn ConnectionConfig, database string, lookup EnvLookup) (dsn string, template string, err error) {
	switch {
	case conn.DSNEnv != "":
		value, ok := lookup(conn.DSNEnv)
		if !ok {
			return "", "", fmt.Errorf("environment variable %s is not set", conn.DSNEnv)
		}
		template, err = ExpandEnvWith(value, lookup)
		if err != nil {
			return "", "", err
		}
	case conn.DSN != "":
		template = conn.DSN
	default:
		template, err = buildDSNFromParts(conn, database, lookup)
		if err != nil {
			return "", "", err
		}
	}

	dsn = template
	if strings.Contains(template, databasePlaceholder) {
		dsn = strings.ReplaceAll(template, databasePlaceholder, url.PathEscape(database))
	}

	return dsn, template, nil
}

func buildDSNFromParts(conn ConnectionConfig, database string, lookup EnvLookup) (string, error) {
	password := ""
	if conn.PasswordEnv != "" {
		value, ok := lookup(conn.PasswordEnv)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", conn.PasswordEnv)
		}
		password = value
	}

	port := conn.Port
	if port == 0 {
		port = 5432
	}

	u := &url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(conn.Host, strconv.Itoa(port)),
		Path:   "/" + database,
	}

	if conn.User != "" {
		if password != "" {
			u.User = url.UserPassword(conn.User, password)
		} else {
			u.User = url.User(conn.User)
		}
	}

	query := url.Values{}
	if conn.SSLMode != "" {
		query.Set("sslmode", conn.SSLMode)
	}
	u.RawQuery = query.Encode()

	return u.String(), nil
}
