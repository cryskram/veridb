package viewer

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

// Config configures the viewer server.
type Config struct {
	// Addr is the listen address, for example ":8080".
	Addr string
	// Token is the shared secret required for every request. Empty disables
	// authentication, which is only appropriate for a local-only viewer.
	Token string
	// CacheTTL bounds how long an API response may be reused.
	CacheTTL time.Duration
	// SourceFile is the JSONL audit trail to ingest.
	SourceFile string
	// RefreshInterval is how often SourceFile is polled.
	RefreshInterval time.Duration
}

// Server serves the audit UI and API.
type Server struct {
	cfg   Config
	store *Store
	cache *ttlCache
	tmpl  *template.Template
	log   *log.Logger

	mu   sync.Mutex
	last int // records ingested on the most recent pass
}

// NewServer builds the HTTP handler.
func NewServer(cfg Config, store *Store, logger *log.Logger) (*Server, error) {
	if logger == nil {
		logger = log.Default()
	}

	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse viewer templates: %w", err)
	}

	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 5 * time.Second
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = 2 * time.Second
	}

	return &Server{
		cfg:   cfg,
		store: store,
		cache: newTTLCache(cfg.CacheTTL),
		tmpl:  tmpl,
		log:   logger,
	}, nil
}

// Handler returns the routed, authenticated handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/records", s.handleAPIRecords)
	mux.HandleFunc("GET /api/stats", s.handleAPIStats)
	mux.HandleFunc("GET /api/meta", s.handleAPIMeta)

	return s.withAuth(mux)
}

// RunIngest polls the audit source until ctx is cancelled. It is meant to be
// started in its own goroutine.
func (s *Server) RunIngest(ctx context.Context) {
	s.ingestOnce(ctx)

	ticker := time.NewTicker(s.cfg.RefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.ingestOnce(ctx)
		}
	}
}

// IngestNow forces a synchronous ingest, so a request can observe a record that
// was just written.
func (s *Server) IngestNow(ctx context.Context) {
	s.ingestOnce(ctx)
}

func (s *Server) ingestOnce(ctx context.Context) {
	result, err := s.store.Ingest(ctx, s.cfg.SourceFile)
	if err != nil {
		s.log.Printf("viewer: ingest %s: %v", s.cfg.SourceFile, err)
		return
	}

	if result.Skipped > 0 {
		s.log.Printf("viewer: skipped %d unparseable line(s) in %s", result.Skipped, s.cfg.SourceFile)
	}

	s.mu.Lock()
	s.last = result.Inserted
	s.mu.Unlock()

	if result.Inserted > 0 {
		// A new record means cached responses are stale.
		s.cache.flush()
		s.log.Printf("viewer: ingested %d new audit record(s)", result.Inserted)
	}
}

// withAuth enforces the shared token. A token supplied in the query string is
// remembered in a cookie so that following links does not require it again.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Health checks are for the container orchestrator, not for humans.
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}

		supplied := r.Header.Get("X-Viewer-Token")
		if supplied == "" {
			supplied = r.URL.Query().Get("token")
		}

		fromQuery := supplied != ""
		if supplied == "" {
			if cookie, err := r.Cookie("veridb_token"); err == nil {
				supplied = cookie.Value
			}
		}

		if !tokenMatches(s.cfg.Token, supplied) {
			s.unauthorized(w, r)
			return
		}

		if fromQuery {
			http.SetCookie(w, &http.Cookie{
				Name:     "veridb_token",
				Value:    supplied,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
				MaxAge:   int((12 * time.Hour).Seconds()),
				// Secure is intentionally left off: the viewer is reached
				// through an ngrok HTTPS tunnel but also often through plain
				// http://localhost during development.
			})
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) unauthorized(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="veridb-audit"`)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)

	if strings.HasPrefix(r.URL.Path, "/api/") {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "missing or invalid token; append ?token=... or send the X-Viewer-Token header",
		})
		return
	}

	fmt.Fprintln(w, "unauthorized: append ?token=<VIEWER_TOKEN> to the URL")
}

// tokenMatches compares in constant time to avoid leaking the token length.
func tokenMatches(want, got string) bool {
	if want == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	last := s.last
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"source":           s.cfg.SourceFile,
		"last_ingested":    last,
		"refresh_interval": s.cfg.RefreshInterval.String(),
	})
}

type pageData struct {
	Token       string
	Records     []Record
	Filter      Filter
	Stats       Stats
	Databases   []string
	Tools       []string
	Total       int
	Limit       int
	Offset      int
	HasPrev     bool
	HasNext     bool
	PrevOffset  int
	NextOffset  int
	SourceFile  string
	GeneratedAt string
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	// Ingest before rendering so a just-recorded call is visible.
	s.ingestOnce(r.Context())

	filter := parseFilter(r)

	records, err := s.store.Query(r.Context(), filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	total, err := s.store.Count(r.Context(), filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	stats, err := s.store.Stats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	databases, _ := s.store.DistinctDatabases(r.Context())
	tools, _ := s.store.DistinctTools(r.Context())

	limit := filter.Limit
	if limit <= 0 {
		limit = defaultLimit
	}

	data := pageData{
		Token:       s.cfg.Token,
		Records:     records,
		Filter:      filter,
		Stats:       stats,
		Databases:   databases,
		Tools:       tools,
		Total:       total,
		Limit:       limit,
		Offset:      filter.Offset,
		HasPrev:     filter.Offset > 0,
		HasNext:     filter.Offset+limit < total,
		PrevOffset:  max(0, filter.Offset-limit),
		NextOffset:  filter.Offset + limit,
		SourceFile:  s.cfg.SourceFile,
		GeneratedAt: time.Now().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", fmt.Sprintf("private, max-age=%d", int(s.cfg.CacheTTL.Seconds())))

	if err := s.tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		s.log.Printf("viewer: render index: %v", err)
	}
}

func (s *Server) handleAPIRecords(w http.ResponseWriter, r *http.Request) {
	s.serveCached(w, r, func() (any, error) {
		filter := parseFilter(r)

		records, err := s.store.Query(r.Context(), filter)
		if err != nil {
			return nil, err
		}

		total, err := s.store.Count(r.Context(), filter)
		if err != nil {
			return nil, err
		}

		limit := filter.Limit
		if limit <= 0 {
			limit = defaultLimit
		}

		return map[string]any{
			"total":    total,
			"limit":    limit,
			"offset":   filter.Offset,
			"returned": len(records),
			"records":  records,
		}, nil
	})
}

func (s *Server) handleAPIStats(w http.ResponseWriter, r *http.Request) {
	s.serveCached(w, r, func() (any, error) {
		return s.store.Stats(r.Context())
	})
}

func (s *Server) handleAPIMeta(w http.ResponseWriter, r *http.Request) {
	s.serveCached(w, r, func() (any, error) {
		databases, err := s.store.DistinctDatabases(r.Context())
		if err != nil {
			return nil, err
		}

		tools, err := s.store.DistinctTools(r.Context())
		if err != nil {
			return nil, err
		}

		return map[string]any{
			"databases": databases,
			"tools":     tools,
			"source":    s.cfg.SourceFile,
		}, nil
	})
}

// serveCached runs build and stores the JSON body for the cache TTL. The key
// ignores the token so one visitor's authenticated response can serve another.
func (s *Server) serveCached(w http.ResponseWriter, r *http.Request, build func() (any, error)) {
	key := cacheKey(r)

	if body, ok := s.cache.get(key); ok {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", fmt.Sprintf("private, max-age=%d", int(s.cfg.CacheTTL.Seconds())))
		w.Header().Set("X-Veridb-Cache", "hit")
		_, _ = w.Write(body)
		return
	}

	value, err := build()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	s.cache.put(key, body)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", fmt.Sprintf("private, max-age=%d", int(s.cfg.CacheTTL.Seconds())))
	w.Header().Set("X-Veridb-Cache", "miss")
	_, _ = w.Write(body)
}

const defaultLimit = 100

func parseFilter(r *http.Request) Filter {
	q := r.URL.Query()

	return Filter{
		Database: strings.TrimSpace(q.Get("db")),
		Tool:     strings.TrimSpace(q.Get("tool")),
		Status:   strings.TrimSpace(q.Get("status")),
		Search:   strings.TrimSpace(q.Get("q")),
		Limit:    atoiDefault(q.Get("limit"), defaultLimit),
		Offset:   atoiDefault(q.Get("offset"), 0),
	}
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(value)
}

// cacheKey is the request URI without the token, so the cache is shared across
// authenticated visitors.
func cacheKey(r *http.Request) string {
	q := r.URL.Query()
	q.Del("token")
	return r.URL.Path + "?" + q.Encode()
}

// ttlCache is a tiny in-process response cache.
type ttlCache struct {
	ttl     time.Duration
	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	body    []byte
	expires time.Time
}

func newTTLCache(ttl time.Duration) *ttlCache {
	return &ttlCache{ttl: ttl, entries: map[string]cacheEntry{}}
}

func (c *ttlCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}

	if time.Now().After(entry.expires) {
		delete(c.entries, key)
		return nil, false
	}

	return entry.body, true
}

func (c *ttlCache) put(key string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Bound the cache so an adversarial query space cannot grow it forever.
	if len(c.entries) > 512 {
		c.entries = map[string]cacheEntry{}
	}

	c.entries[key] = cacheEntry{body: body, expires: time.Now().Add(c.ttl)}
}

func (c *ttlCache) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[string]cacheEntry{}
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"dur": FormatDuration,
		"short": func(s string, max int) string {
			s = strings.ReplaceAll(s, "\n", " ")
			s = strings.Join(strings.Fields(s), " ")
			if len(s) <= max {
				return s
			}
			return s[:max] + "…"
		},
		"ts": func(s string) string {
			t, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return s
			}
			return t.Local().Format("2006-01-02 15:04:05")
		},
		"params": func(p []any) string {
			if len(p) == 0 {
				return ""
			}
			encoded, err := json.Marshal(p)
			if err != nil {
				return ""
			}
			return string(encoded)
		},
		"add": func(a, b int) int { return a + b },
		// list lets the template iterate over a set of options without the
		// handler having to build a slice of ints.
		"list": func(values ...int) []int { return values },
	}
}
