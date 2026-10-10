package database

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/cryskram/veridb/internal/config"
)

// Entry pairs a resolved database configuration with a live driver.
type Entry struct {
	Config config.ResolvedDatabase
	Driver Driver
}

// Name is the logical VeriDB database name.
func (e *Entry) Name() string { return e.Config.Name }

// Registry holds the databases VeriDB successfully connected to, plus the
// warnings explaining anything it had to skip.
type Registry struct {
	mu       sync.RWMutex
	entries  map[string]*Entry
	order    []string
	warnings []Warning
	// writeMode holds session-scoped write overrides set through the
	// set_write_mode tool. The base config is never mutated: the override is
	// consulted whenever the effective policy or services are needed, and a
	// server restart drops every override. Guarded by mu like everything else.
	writeMode map[string]bool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		entries:   map[string]*Entry{},
		writeMode: map[string]bool{},
	}
}

// Add registers an entry. Duplicate names are rejected.
func (r *Registry) Add(entry *Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	name := entry.Config.Name
	if _, exists := r.entries[name]; exists {
		return fmt.Errorf("database %q already registered", name)
	}

	r.entries[name] = entry
	r.order = append(r.order, name)
	sort.Strings(r.order)

	return nil
}

// Get returns the entry for name.
func (r *Registry) Get(name string) (*Entry, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.entries[name]
	if !ok {
		return nil, fmt.Errorf("database %q is not available (see list_databases for the enabled set)", name)
	}

	return entry, nil
}

// Names returns the available database names, alphabetically.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Entries returns every entry in stable order.
func (r *Registry) Entries() []*Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*Entry, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.entries[name])
	}
	return out
}

// Len reports how many databases are connected.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// Warnings returns the startup warnings: databases that were configured but
// skipped, and problems encountered while verifying them.
func (r *Registry) Warnings() []Warning {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Warning, len(r.warnings))
	copy(out, r.warnings)
	return out
}

// Close releases every connection pool.
func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, entry := range r.entries {
		entry.Driver.Close()
	}
}

// Build connects to every resolved database.
//
// A database that does not exist on its host is skipped with a warning rather
// than aborting startup: a config shared across environments will legitimately
// name databases that only exist in some of them. The returned registry is
// always usable; inspect Warnings() to see what was dropped.
//
// connectParallelism bounds concurrent database connects at startup: enough
// to collapse N round trips into roughly one, few enough not to hammer the
// host or trip connection limits.
const connectParallelism = 8

// logf receives human-readable startup messages and may be nil.
func Build(ctx context.Context, resolved *config.Resolved, logf func(format string, args ...any)) (*Registry, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}

	registry := NewRegistry()

	// List the databases on each connection exactly once, so a config with 30
	// databases on one host costs a single extra round trip.
	available, unreachable, listWarnings := discoverDatabases(ctx, resolved, logf)
	registry.warnings = append(registry.warnings, listWarnings...)

	// Existence filtering is cheap and stays sequential; the expensive part
	// is the per-database connect, which runs concurrently below.
	type target struct {
		name string
		db   config.ResolvedDatabase
	}
	targets := make([]target, 0, len(resolved.Order))
	for _, name := range resolved.Order {
		db := resolved.Databases[name]

		// The listing already proved this host unreachable; re-dialling it
		// per database would only burn another dial timeout each. Anything
		// else (auth, permissions) still falls through to a direct attempt.
		if unreachable[db.Connection] {
			warning := Warning{
				Database: name,
				Level:    "warn",
				Message: fmt.Sprintf(
					"database %q skipped: host for connection %q is unreachable (see listing warning); skipping",
					db.Database, db.Connection,
				),
			}
			registry.warnings = append(registry.warnings, warning)
			logf("[skip] %s", warning.Message)
			continue
		}

		if known, ok := available[db.Connection]; ok {
			if _, exists := known[db.Database]; !exists {
				warning := Warning{
					Database: name,
					Level:    "warn",
					Message: fmt.Sprintf(
						"database %q does not exist on connection %q (host lookup succeeded); skipping",
						db.Database, db.Connection,
					),
				}
				registry.warnings = append(registry.warnings, warning)
				logf("[skip] %s", warning.Message)
				continue
			}
		}

		targets = append(targets, target{name: name, db: db})
	}

	// Connect concurrently with bounded parallelism. Sequentially this loop
	// cost one network round trip per database before the MCP handshake
	// could even begin, which made the server look dead to the client on a
	// slow network. Results are applied in config order so startup logs
	// stay stable and duplicate names fail exactly as before.
	type connectResult struct {
		target target
		driver Driver
		err    error
	}
	results := make([]connectResult, len(targets))

	var wg sync.WaitGroup
	sem := make(chan struct{}, connectParallelism)
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t target) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			driver, err := newDriver(ctx, t.db)
			results[i] = connectResult{target: t, driver: driver, err: err}
		}(i, t)
	}
	wg.Wait()

	for _, r := range results {
		db := r.target.db

		if r.err != nil {
			warning := Warning{
				Database: r.target.name,
				Level:    "error",
				Message: fmt.Sprintf(
					"could not connect to database %q on connection %q: %v; skipping",
					db.Database, db.Connection, r.err,
				),
			}
			registry.warnings = append(registry.warnings, warning)
			logf("[skip] %s", warning.Message)
			continue
		}

		if err := registry.Add(&Entry{Config: db, Driver: r.driver}); err != nil {
			r.driver.Close()
			return nil, err
		}

		logf("[ok] %s (%s, %s, %d service(s) enabled)",
			r.target.name, db.Database, db.Policy.Summarize(), db.Services.Enabled())
	}

	return registry, nil
}

// discoverDatabases maps connection name -> set of database names on that
// server. A connection whose listing failed is simply absent from the map,
// which makes Build fall back to attempting a direct connection.
// connectivityFailure reports whether err shows the host itself is
// unreachable, as opposed to a live host refusing us (bad credentials,
// missing privileges). Only the former lets startup skip the direct-connect
// fallback: re-dialling a dead host just burns another dial timeout.
// Unrecognised errors fail open towards attempting the connection.
func connectivityFailure(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, sub := range []string{
		"dial error",
		"connection refused",
		"connection reset",
		"no such host",
		"network is unreachable",
		"temporary failure in name resolution",
		"context deadline exceeded",
	} {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}

func discoverDatabases(
	ctx context.Context,
	resolved *config.Resolved,
	logf func(format string, args ...any),
) (map[string]map[string]struct{}, map[string]bool, []Warning) {
	available := map[string]map[string]struct{}{}
	unreachable := map[string]bool{}
	var warnings []Warning

	// Unique connections in config order. Only postgres can enumerate
	// databases today; other drivers skip the check and rely on the
	// connection attempt itself.
	type discovery struct {
		connection string
		db         config.ResolvedDatabase
	}
	var jobs []discovery
	seen := map[string]bool{}
	for _, name := range resolved.Order {
		db := resolved.Databases[name]
		if seen[db.Connection] || db.Driver != DriverNamePostgres {
			continue
		}
		seen[db.Connection] = true
		jobs = append(jobs, discovery{connection: db.Connection, db: db})
	}

	// List concurrently: on a dead host each listing burns the full dial
	// timeout, and there is no reason to pay that serially per host.
	// Results merge in config order so warnings stay stable.
	type discoverResult struct {
		found map[string]struct{}
		err   error
	}
	results := make([]discoverResult, len(jobs))
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Add(1)
		go func(i int, job discovery) {
			defer wg.Done()
			found, err := ListServerDatabases(ctx, job.db.MaintenanceDSN)
			results[i] = discoverResult{found: found, err: err}
		}(i, job)
	}
	wg.Wait()

	for i, job := range jobs {
		res := results[i]
		if res.err != nil {
			warning := Warning{
				Database: "",
				Level:    "warn",
				Message: fmt.Sprintf(
					"could not list databases on connection %q (%v); existence check skipped, connecting directly",
					job.connection, res.err,
				),
			}
			warnings = append(warnings, warning)
			logf("[warn] %s", warning.Message)
			if connectivityFailure(res.err) {
				unreachable[job.connection] = true
			}
			continue
		}

		available[job.connection] = res.found
	}

	return available, unreachable, warnings
}

// SetWriteMode enables or clears the session write override for a connected
// database. Enabling flips the effective policy to read-write (INSERT and
// UPDATE) and exposes the write service for that database; every other
// policy flag — DELETE, DDL, TRUNCATE, caps, timeouts — stays exactly as
// configured. Disabling drops the override and the base config applies again.
// Unknown names are rejected so a typo cannot silently do nothing.
func (r *Registry) SetWriteMode(name string, on bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.entries[name]; !ok {
		return fmt.Errorf("database %q is not available (see list_databases for the enabled set)", name)
	}

	if on {
		r.writeMode[name] = true
	} else {
		delete(r.writeMode, name)
	}

	return nil
}

// WriteMode reports whether the session write override is active for name.
// Unknown names report false.
func (r *Registry) WriteMode(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.writeMode[name]
}

// EffectiveConfig returns a copy of the named entry's resolved config with
// the session write override applied. Handlers must enforce against this,
// never against the entry's base config, or a toggle would change the
// display while the guard kept refusing. The copy shares the base slices,
// which is safe because the override only flips boolean fields.
func (r *Registry) EffectiveConfig(name string) (config.ResolvedDatabase, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.entries[name]
	if !ok {
		return config.ResolvedDatabase{}, fmt.Errorf("database %q is not available (see list_databases for the enabled set)", name)
	}

	cfg := entry.Config
	if r.writeMode[name] {
		cfg.Policy.ReadOnly = false
		cfg.Policy.AllowWrite = true
		cfg.Services.Write = true
	}

	return cfg, nil
}

func newDriver(ctx context.Context, db config.ResolvedDatabase) (Driver, error) {
	switch db.Driver {
	case DriverNamePostgres:
		return NewPostgres(ctx, ConnectionOptions{
			Name:             db.Name,
			Driver:           db.Driver,
			DSN:              db.DSN,
			MaintenanceDSN:   db.MaintenanceDSN,
			Pool:             db.Pool,
			StatementTimeout: db.Policy.StatementTimeout.Std(),
			ReadOnly:         db.Policy.ReadOnly,
		})
	default:
		return nil, fmt.Errorf("unsupported driver %q", db.Driver)
	}
}
