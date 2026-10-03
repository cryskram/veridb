package config

// Policy is the fully resolved, effective safety policy for a single database.
// Every agent-visible operation is checked against it server-side.
type Policy struct {
	// ReadOnly is the master switch. When true every mutation is refused,
	// regardless of the individual allow_* flags below.
	ReadOnly bool

	// AllowWrite permits INSERT/UPDATE/UPSERT.
	AllowWrite bool
	// AllowDelete permits DELETE. Kept separate from AllowWrite because
	// deleting rows is usually the riskiest thing an agent can do.
	AllowDelete bool
	// AllowDDL permits CREATE/ALTER/DROP/TRUNCATE and friends.
	AllowDDL bool
	// AllowTruncate permits TRUNCATE specifically. Truncate requires both
	// AllowDDL and AllowTruncate so it can never be enabled by accident.
	AllowTruncate bool

	// RequireWhereForMutation refuses UPDATE/DELETE without a WHERE clause.
	RequireWhereForMutation bool

	// MaxRows caps rows returned by a single read query. 0 means unlimited.
	MaxRows int
	// StatementTimeout is applied to every statement on this database.
	StatementTimeout Duration
	// MaxAffectedRows caps rows touched by a single mutation. 0 means unlimited.
	MaxAffectedRows int

	// AllowSchemas is an allowlist of schemas agents may touch. Empty means
	// "anything not denied".
	AllowSchemas []string
	// DenySchemas is always subtracted, even when AllowSchemas is set.
	DenySchemas []string
	// DenyTables holds glob patterns of tables that are never exposed.
	DenyTables []string

	// DenyKeywords are substrings that make a statement invalid.
	DenyKeywords []string

	// RedactColumns holds glob patterns like "*.password" whose values are
	// masked in results before they reach the agent.
	RedactColumns []string
}

// PolicyOverride is the YAML-facing form of Policy. Every field is a pointer or
// a slice so "unset" can be distinguished from "explicitly set to the zero
// value" during defaults merging.
//
// It is embedded inline in database definitions, which is what makes per-DB
// overrides feel like "just mention the option I care about":
//
//	databases:
//	  app:
//	    connection: primary
//	    read_only: false
//	    allow_write: true
//	    max_rows: 500
type PolicyOverride struct {
	ReadOnly                *bool     `yaml:"read_only"`
	AllowWrite              *bool     `yaml:"allow_write"`
	AllowDelete             *bool     `yaml:"allow_delete"`
	AllowDDL                *bool     `yaml:"allow_ddl"`
	AllowTruncate           *bool     `yaml:"allow_truncate"`
	RequireWhereForMutation *bool     `yaml:"require_where_for_mutation"`
	MaxRows                 *int      `yaml:"max_rows"`
	StatementTimeout        *Duration `yaml:"statement_timeout"`
	MaxAffectedRows         *int      `yaml:"max_affected_rows"`
	AllowSchemas            []string  `yaml:"allow_schemas"`
	DenySchemas             []string  `yaml:"deny_schemas"`
	DenyTables              []string  `yaml:"deny_tables"`
	DenyKeywords            []string  `yaml:"deny_keywords"`
	RedactColumns           []string  `yaml:"redact_columns"`
}

// DefaultDenyKeywords is the baseline deny list. These functions are either
// filesystem/network escape hatches or can disrupt other sessions, so they are
// blocked unless an operator deliberately replaces the list.
func DefaultDenyKeywords() []string {
	return []string{
		"pg_read_file",
		"pg_read_binary_file",
		"pg_ls_dir",
		"pg_stat_file",
		"pg_terminate_backend",
		"pg_cancel_backend",
		"lo_import",
		"lo_export",
		"COPY TO PROGRAM",
		"COPY FROM PROGRAM",
		"dblink",
		"pg_sleep",
	}
}

// DefaultPolicy returns the baseline policy applied to every database before
// any overrides. It is deliberately restrictive: reads only, bounded rows and
// runtime, common escape hatches denied.
func DefaultPolicy() Policy {
	return Policy{
		ReadOnly:                true,
		AllowWrite:              false,
		AllowDelete:             false,
		AllowDDL:                false,
		AllowTruncate:           false,
		RequireWhereForMutation: true,
		MaxRows:                 100,
		StatementTimeout:        Duration(15_000_000_000), // 15s
		MaxAffectedRows:         1000,
		AllowSchemas:            nil,
		DenySchemas:             []string{"pg_catalog", "information_schema", "pg_toast"},
		DenyTables:              nil,
		DenyKeywords:            DefaultDenyKeywords(),
		RedactColumns:           nil,
	}
}

// Apply mutates p with every field the override explicitly sets. Slices
// replace rather than append, so a database can narrow the inherited list.
func (p *Policy) Apply(o *PolicyOverride) {
	if o == nil {
		return
	}

	setBool(&p.ReadOnly, o.ReadOnly)
	setBool(&p.AllowWrite, o.AllowWrite)
	setBool(&p.AllowDelete, o.AllowDelete)
	setBool(&p.AllowDDL, o.AllowDDL)
	setBool(&p.AllowTruncate, o.AllowTruncate)
	setBool(&p.RequireWhereForMutation, o.RequireWhereForMutation)
	setInt(&p.MaxRows, o.MaxRows)
	setInt(&p.MaxAffectedRows, o.MaxAffectedRows)

	if o.StatementTimeout != nil {
		p.StatementTimeout = *o.StatementTimeout
	}
	if o.AllowSchemas != nil {
		p.AllowSchemas = o.AllowSchemas
	}
	if o.DenySchemas != nil {
		p.DenySchemas = o.DenySchemas
	}
	if o.DenyTables != nil {
		p.DenyTables = o.DenyTables
	}
	if o.DenyKeywords != nil {
		p.DenyKeywords = o.DenyKeywords
	}
	if o.RedactColumns != nil {
		p.RedactColumns = o.RedactColumns
	}
}

// WritesEnabled reports whether any mutation is possible, and is what the
// write tools check before doing anything else.
func (p Policy) WritesEnabled() bool {
	if p.ReadOnly {
		return false
	}
	return p.AllowWrite || p.AllowDelete || p.AllowDDL
}

// Summarize renders the policy as a short human-readable string used by the
// status tool and startup logs.
func (p Policy) Summarize() string {
	mode := "read-only"
	if !p.ReadOnly {
		mode = "read-write"
	}
	return mode
}

func setBool(dst *bool, src *bool) {
	if src != nil {
		*dst = *src
	}
}

func setInt(dst *int, src *int) {
	if src != nil {
		*dst = *src
	}
}

// Services says which tool groups are registered for a database.
type Services struct {
	Schema  bool
	Query   bool
	Explain bool
	Sample  bool
	Health  bool
	Write   bool
	Admin   bool
}

// ServicesConfig is the YAML-facing form of Services. A nil field means
// "inherit from the global default".
type ServicesConfig struct {
	Schema  *bool `yaml:"schema"`
	Query   *bool `yaml:"query"`
	Explain *bool `yaml:"explain"`
	Sample  *bool `yaml:"sample"`
	Health  *bool `yaml:"health"`
	Write   *bool `yaml:"write"`
	Admin   *bool `yaml:"admin"`
}

// DefaultServices is the baseline tool set: everything an agent needs to
// inspect and query, nothing that mutates.
func DefaultServices() Services {
	return Services{
		Schema:  true,
		Query:   true,
		Explain: true,
		Sample:  true,
		Health:  true,
		Write:   false,
		Admin:   false,
	}
}

// Apply overlays non-nil fields onto s.
func (s *Services) Apply(o *ServicesConfig) {
	if o == nil {
		return
	}
	setBool(&s.Schema, o.Schema)
	setBool(&s.Query, o.Query)
	setBool(&s.Explain, o.Explain)
	setBool(&s.Sample, o.Sample)
	setBool(&s.Health, o.Health)
	setBool(&s.Write, o.Write)
	setBool(&s.Admin, o.Admin)
}

// Enabled counts the tool groups that are on. Used by validation to warn when a
// database would expose nothing at all.
func (s Services) Enabled() int {
	n := 0
	for _, on := range []bool{s.Schema, s.Query, s.Explain, s.Sample, s.Health, s.Write, s.Admin} {
		if on {
			n++
		}
	}
	return n
}
