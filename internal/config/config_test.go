package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func fakeEnv(vars map[string]string) EnvLookup {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

// parse builds a Config from YAML source with a fake environment, mirroring what
// Load does but without touching the real process environment.
func parse(t *testing.T, src string, vars map[string]string) (*Config, error) {
	t.Helper()

	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		return nil, err
	}

	if err := cfg.ExpandEnv(fakeEnv(vars)); err != nil {
		return nil, err
	}

	return &cfg, nil
}

const minimalYAML = `
server:
  name: veridb-test
  version: 1.2.3

connections:
  primary:
    driver: postgres
    host: db.example.com
    port: 5432
    user: reader
    password_env: PG_PASSWORD

databases:
  app:
    connection: primary
    description: investments
`

func TestResolveAppliesDefaults(t *testing.T) {
	cfg, err := parse(t, minimalYAML, map[string]string{"PG_PASSWORD": "s3cret"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	resolved, err := cfg.ResolveWithEnv(fakeEnv(map[string]string{"PG_PASSWORD": "s3cret"}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if resolved.Server.Name != "veridb-test" {
		t.Errorf("server name = %q, want veridb-test", resolved.Server.Name)
	}

	db, ok := resolved.Databases["app"]
	if !ok {
		t.Fatal("app not resolved")
	}

	if !db.Policy.ReadOnly {
		t.Error("read_only should default to true")
	}
	if db.Policy.WritesEnabled() {
		t.Error("writes should be disabled by default")
	}
	if db.Policy.MaxRows != 100 {
		t.Errorf("max_rows = %d, want 100", db.Policy.MaxRows)
	}
	if got := db.Policy.StatementTimeout.Std().Seconds(); got != 15 {
		t.Errorf("statement_timeout = %vs, want 15s", got)
	}
	if db.Policy.MaxAffectedRows != 1000 {
		t.Errorf("max_affected_rows = %d, want 1000", db.Policy.MaxAffectedRows)
	}
	if !db.Services.Query || !db.Services.Schema {
		t.Error("query and schema services should default to enabled")
	}
	if db.Services.Write {
		t.Error("write service should default to disabled")
	}
	if db.Database != "app" {
		t.Errorf("physical database = %q, want app (config key)", db.Database)
	}

	wantDSN := "postgres://reader:s3cret@db.example.com:5432/app"
	if got := strings.TrimSuffix(db.DSN, "?"); got != wantDSN {
		t.Errorf("dsn = %q, want %q", got, wantDSN)
	}
}

func TestResolvePerDatabaseOverrides(t *testing.T) {
	src := minimalYAML + `
    read_only: false
    allow_write: true
    max_rows: 500
    statement_timeout: 30s
    redact_columns: ["*.password"]
`
	cfg, err := parse(t, src, map[string]string{"PG_PASSWORD": "x"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	resolved, err := cfg.ResolveWithEnv(fakeEnv(map[string]string{"PG_PASSWORD": "x"}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	db := resolved.Databases["app"]
	if db.Policy.ReadOnly {
		t.Error("read_only override to false was ignored")
	}
	if !db.Policy.AllowWrite {
		t.Error("allow_write override was ignored")
	}
	if db.Policy.AllowDelete {
		t.Error("allow_delete should remain false")
	}
	if db.Policy.MaxRows != 500 {
		t.Errorf("max_rows = %d, want 500", db.Policy.MaxRows)
	}
	if got := db.Policy.StatementTimeout.Std().Seconds(); got != 30 {
		t.Errorf("statement_timeout = %vs, want 30s", got)
	}
	if len(db.Policy.RedactColumns) != 1 || db.Policy.RedactColumns[0] != "*.password" {
		t.Errorf("redact_columns = %v, want [*.password]", db.Policy.RedactColumns)
	}
	if !db.Policy.WritesEnabled() {
		t.Error("writes should be enabled after override")
	}
	// Deny keywords must survive the override since they were not restated.
	if len(db.Policy.DenyKeywords) == 0 {
		t.Error("deny_keywords should be inherited from defaults")
	}
}

func TestOverridesDoNotLeakBetweenDatabases(t *testing.T) {
	src := `
connections:
  c:
    driver: postgres
    dsn_env: DSN

databases:
  a:
    connection: c
    redact_columns: ["*.secret"]
  b:
    connection: c
`
	cfg, err := parse(t, src, map[string]string{"DSN": "postgres://u:p@h:5432/{database}"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	resolved, err := cfg.ResolveWithEnv(fakeEnv(map[string]string{"DSN": "postgres://u:p@h:5432/{database}"}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if len(resolved.Databases["a"].Policy.RedactColumns) != 1 {
		t.Fatal("a should have redact_columns")
	}
	if len(resolved.Databases["b"].Policy.RedactColumns) != 0 {
		t.Error("redact_columns leaked from a into b")
	}
	if got := resolved.Databases["b"].DSN; !strings.HasSuffix(got, "/b") {
		t.Errorf("dsn for b = %q, want suffix /b", got)
	}
}

func TestResolveSkipsDisabledDatabases(t *testing.T) {
	src := `
connections:
  c:
    driver: postgres
    dsn_env: DSN

databases:
  on:
    connection: c
  off:
    connection: c
    enabled: false
`
	vars := map[string]string{"DSN": "postgres://u:p@h:5432/{database}"}
	cfg, err := parse(t, src, vars)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	resolved, err := cfg.ResolveWithEnv(fakeEnv(vars))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if _, ok := resolved.Databases["off"]; ok {
		t.Error("disabled database should not be resolved")
	}
	if _, ok := resolved.Databases["on"]; !ok {
		t.Error("enabled database missing")
	}
	if len(resolved.Order) != 1 || resolved.Order[0] != "on" {
		t.Errorf("order = %v, want [on]", resolved.Order)
	}
}

func TestResolveFailsOnMissingEnv(t *testing.T) {
	cfg, err := parse(t, minimalYAML, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	_, err = cfg.ResolveWithEnv(fakeEnv(nil))
	if err == nil {
		t.Fatal("expected error for unset PG_PASSWORD")
	}
	if !strings.Contains(err.Error(), "PG_PASSWORD") {
		t.Errorf("error should name the missing variable, got: %v", err)
	}
}

func TestResolveRejectsSharedDSNWithoutPlaceholder(t *testing.T) {
	src := `
connections:
  c:
    driver: postgres
    dsn_env: DSN

databases:
  a:
    connection: c
  b:
    connection: c
`
	vars := map[string]string{"DSN": "postgres://u:p@h:5432/fixed"}
	cfg, err := parse(t, src, vars)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	_, err = cfg.ResolveWithEnv(fakeEnv(vars))
	if err == nil {
		t.Fatal("expected error when a shared DSN has no {database} placeholder")
	}
	if !strings.Contains(err.Error(), "{database}") {
		t.Errorf("error should mention the placeholder, got: %v", err)
	}
}

func TestValidateCatchesProblems(t *testing.T) {
	src := `
connections:
  bad:
    driver: oracle
    host: h
    user: u

databases:
  orphan:
    connection: nope
`
	cfg, err := parse(t, src, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	err = cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}

	for _, want := range []string{"unsupported driver", "unknown connection"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validation error missing %q:\n%v", want, err)
		}
	}
}

func TestExpandEnvLeavesFreeTextAlone(t *testing.T) {
	// Docs and descriptions routinely mention ${VAR}; expansion must be
	// field-scoped so those do not break startup.
	src := `
server:
  instructions: |
    Every value written as ${VAR} is read from the environment.

connections:
  c:
    driver: postgres
    host: ${DB_HOST}
    port: 5432
    user: ${DB_USER}
    dsn_env: DSN

databases:
  a:
    connection: c
    description: "see ${SOMETHING_ELSE}"
`
	vars := map[string]string{
		"DB_HOST": "db.internal",
		"DB_USER": "svc",
		"DSN":     "postgres://svc@db.internal:5432/{database}",
	}

	cfg, err := parse(t, src, vars)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if got := cfg.Connections["c"].Host; got != "db.internal" {
		t.Errorf("host = %q, want db.internal", got)
	}
	if got := cfg.Connections["c"].User; got != "svc" {
		t.Errorf("user = %q, want svc", got)
	}
	if !strings.Contains(cfg.Server.Instructions, "${VAR}") {
		t.Error("instructions should not have been expanded")
	}
	if !strings.Contains(cfg.Databases["a"].Description, "${SOMETHING_ELSE}") {
		t.Error("description should not have been expanded")
	}
}

func TestExpandEnvFailsOnUndefinedReference(t *testing.T) {
	src := `
connections:
  c:
    driver: postgres
    host: ${NOT_SET_ANYWHERE}
    user: u

databases:
  a:
    connection: c
`
	_, err := parse(t, src, nil)
	if err == nil {
		t.Fatal("expected error for undefined ${NOT_SET_ANYWHERE}")
	}
	if !strings.Contains(err.Error(), "NOT_SET_ANYWHERE") {
		t.Errorf("error should name the variable, got: %v", err)
	}
}

func TestDurationAcceptsStringAndMilliseconds(t *testing.T) {
	src := `
connections:
  c:
    driver: postgres
    dsn_env: DSN

databases:
  a:
    connection: c
    statement_timeout: 1500
  b:
    connection: c
    statement_timeout: 2m
`
	vars := map[string]string{"DSN": "postgres://u:p@h:5432/{database}"}
	cfg, err := parse(t, src, vars)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	resolved, err := cfg.ResolveWithEnv(fakeEnv(vars))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got := resolved.Databases["a"].Policy.StatementTimeout.Std().Seconds(); got != 1.5 {
		t.Errorf("integer ms duration = %vs, want 1.5s", got)
	}
	if got := resolved.Databases["b"].Policy.StatementTimeout.Std().Minutes(); got != 2 {
		t.Errorf("string duration = %vm, want 2m", got)
	}
}

func TestResolveAuditDefaultsAndOverride(t *testing.T) {
	src := minimalYAML + `
audit:
  sinks: [file]
  file: /tmp/audit.jsonl
  include_params: true
`
	cfg, err := parse(t, src, map[string]string{"PG_PASSWORD": "x"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	resolved, err := cfg.ResolveWithEnv(fakeEnv(map[string]string{"PG_PASSWORD": "x"}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if !resolved.Audit.Enabled {
		t.Error("audit should default to enabled")
	}
	if !resolved.Audit.HasSink("file") {
		t.Errorf("sinks = %v, want file", resolved.Audit.Sinks)
	}
	if resolved.Audit.File != "/tmp/audit.jsonl" {
		t.Errorf("file = %q", resolved.Audit.File)
	}
	if !resolved.Audit.IncludeParams {
		t.Error("include_params override ignored")
	}
	if !resolved.Audit.IncludeSQL {
		t.Error("include_sql should default to true")
	}
}

func TestValidateRejectsFileAuditWithoutPath(t *testing.T) {
	src := minimalYAML + `
audit:
  sinks: [file]
`
	cfg, err := parse(t, src, map[string]string{"PG_PASSWORD": "x"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for file sink without path")
	}
}
