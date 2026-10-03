package database

import (
	"context"
	"fmt"
	"sort"
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
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		entries: map[string]*Entry{},
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
// logf receives human-readable startup messages and may be nil.
func Build(ctx context.Context, resolved *config.Resolved, logf func(format string, args ...any)) (*Registry, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}

	registry := NewRegistry()

	// List the databases on each connection exactly once, so a config with 30
	// databases on one host costs a single extra round trip.
	available, listWarnings := discoverDatabases(ctx, resolved, logf)
	registry.warnings = append(registry.warnings, listWarnings...)

	for _, name := range resolved.Order {
		db := resolved.Databases[name]

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

		driver, err := newDriver(ctx, db)
		if err != nil {
			warning := Warning{
				Database: name,
				Level:    "error",
				Message: fmt.Sprintf(
					"could not connect to database %q on connection %q: %v; skipping",
					db.Database, db.Connection, err,
				),
			}
			registry.warnings = append(registry.warnings, warning)
			logf("[skip] %s", warning.Message)
			continue
		}

		if err := registry.Add(&Entry{Config: db, Driver: driver}); err != nil {
			driver.Close()
			return nil, err
		}

		logf("[ok] %s (%s, %s, %d service(s) enabled)",
			name, db.Database, db.Policy.Summarize(), db.Services.Enabled())
	}

	return registry, nil
}

// discoverDatabases maps connection name -> set of database names on that
// server. A connection whose listing failed is simply absent from the map,
// which makes Build fall back to attempting a direct connection.
func discoverDatabases(
	ctx context.Context,
	resolved *config.Resolved,
	logf func(format string, args ...any),
) (map[string]map[string]struct{}, []Warning) {
	available := map[string]map[string]struct{}{}
	var warnings []Warning

	seen := map[string]bool{}

	for _, name := range resolved.Order {
		db := resolved.Databases[name]

		if seen[db.Connection] {
			continue
		}
		seen[db.Connection] = true

		// Only postgres can enumerate databases today. Other drivers skip the
		// check and rely on the connection attempt itself.
		if db.Driver != DriverNamePostgres {
			continue
		}

		found, err := ListServerDatabases(ctx, db.MaintenanceDSN)
		if err != nil {
			warning := Warning{
				Database: "",
				Level:    "warn",
				Message: fmt.Sprintf(
					"could not list databases on connection %q (%v); existence check skipped, connecting directly",
					db.Connection, err,
				),
			}
			warnings = append(warnings, warning)
			logf("[warn] %s", warning.Message)
			continue
		}

		available[db.Connection] = found
	}

	return available, warnings
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
