package viewer

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T, token string) (*Server, *Store) {
	t.Helper()

	store, err := OpenStore(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	trail := writeTrail(t, okRecord, deniedRecord)
	if _, err := store.Ingest(t.Context(), trail); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	server, err := NewServer(Config{
		Token:           token,
		CacheTTL:        time.Minute,
		SourceFile:      trail,
		RefreshInterval: time.Hour,
	}, store, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	return server, store
}

func TestAuthRejectsMissingOrWrongToken(t *testing.T) {
	server, _ := newTestServer(t, "s3cret")
	handler := server.Handler()

	cases := []struct {
		name string
		url  string
		hdr  string
	}{
		{"no token", "/api/records", ""},
		{"wrong token in query", "/api/records?token=nope", ""},
		{"wrong token in header", "/api/records", "nope"},
		{"empty header", "/api/records", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			if tc.hdr != "" {
				req.Header.Set("X-Viewer-Token", tc.hdr)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestAuthAcceptsTokenFromQueryAndHeader(t *testing.T) {
	server, _ := newTestServer(t, "s3cret")
	handler := server.Handler()

	for _, tc := range []struct {
		name string
		url  string
		hdr  string
	}{
		{"query", "/api/stats?token=s3cret", ""},
		{"header", "/api/stats", "s3cret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			if tc.hdr != "" {
				req.Header.Set("X-Viewer-Token", tc.hdr)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// A token in the query string is remembered so links and refreshes keep working.
func TestAuthSetsCookieFromQueryToken(t *testing.T) {
	server, _ := newTestServer(t, "s3cret")

	req := httptest.NewRequest(http.MethodGet, "/api/stats?token=s3cret", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected a cookie to be set")
	}
	if cookies[0].Name != "veridb_token" || cookies[0].Value != "s3cret" {
		t.Errorf("unexpected cookie: %+v", cookies[0])
	}
	if !cookies[0].HttpOnly {
		t.Error("the token cookie should be HttpOnly")
	}
}

func TestCookieAuthenticatesLaterRequests(t *testing.T) {
	server, _ := newTestServer(t, "s3cret")

	req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	req.AddCookie(&http.Cookie{Name: "veridb_token", Value: "s3cret"})
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHealthzNeedsNoToken(t *testing.T) {
	server, _ := newTestServer(t, "s3cret")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("healthz is not JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %v", body["status"])
	}
}

func TestEmptyTokenDisablesAuth(t *testing.T) {
	server, _ := newTestServer(t, "")

	req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 when auth is disabled", rec.Code)
	}
}

func TestAPIRecordsAndFilters(t *testing.T) {
	server, _ := newTestServer(t, "")

	cases := []struct {
		name      string
		url       string
		wantTotal float64
	}{
		{"all", "/api/records", 2},
		{"by status", "/api/records?status=denied", 1},
		{"by database", "/api/records?db=app", 1},
		{"search", "/api/records?q=DELETE", 1},
		{"no match", "/api/records?q=zzz", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}

			var body struct {
				Total   float64  `json:"total"`
				Records []Record `json:"records"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Total != tc.wantTotal {
				t.Errorf("total = %v, want %v", body.Total, tc.wantTotal)
			}
		})
	}
}

// The token must not become part of the cache key, or every visitor would get
// their own cache entry and the cache would never be shared.
func TestCacheIsSharedAcrossTokensAndReportsHits(t *testing.T) {
	server, _ := newTestServer(t, "")

	first := httptest.NewRecorder()
	server.Handler().ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/stats", nil))

	if got := first.Header().Get("X-Veridb-Cache"); got != "miss" {
		t.Errorf("first response cache header = %q, want miss", got)
	}

	second := httptest.NewRecorder()
	server.Handler().ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/api/stats", nil))

	if got := second.Header().Get("X-Veridb-Cache"); got != "hit" {
		t.Errorf("second response cache header = %q, want hit", got)
	}

	if first.Body.String() != second.Body.String() {
		t.Error("cached response differs from the original")
	}

	if cc := second.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=") {
		t.Errorf("Cache-Control = %q, want a max-age", cc)
	}
}

func TestCacheKeyIgnoresToken(t *testing.T) {
	req1 := httptest.NewRequest(http.MethodGet, "/api/records?token=aaa&status=ok", nil)
	req2 := httptest.NewRequest(http.MethodGet, "/api/records?token=bbb&status=ok", nil)

	if cacheKey(req1) != cacheKey(req2) {
		t.Errorf("cache keys differ by token: %q vs %q", cacheKey(req1), cacheKey(req2))
	}
	if strings.Contains(cacheKey(req1), "aaa") {
		t.Error("cache key leaked the token")
	}
}

func TestIngestFlushesCache(t *testing.T) {
	server, _ := newTestServer(t, "")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stats", nil))

	if got := rec.Header().Get("X-Veridb-Cache"); got != "miss" {
		t.Fatalf("first response cache header = %q, want miss", got)
	}

	// Append to the trail the server actually watches, then force an ingest: a
	// freshly loaded record must invalidate what was just cached.
	f, err := os.OpenFile(server.cfg.SourceFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"ts":"2026-01-02T04:00:00Z","tool":"query","database":"analytics","status":"ok","sql":"SELECT 2"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	server.IngestNow(t.Context())

	after := httptest.NewRecorder()
	server.Handler().ServeHTTP(after, httptest.NewRequest(http.MethodGet, "/api/stats", nil))

	if got := after.Header().Get("X-Veridb-Cache"); got != "miss" {
		t.Errorf("cache header after ingest = %q, want miss", got)
	}

	var body struct {
		Total float64 `json:"total"`
	}
	if err := json.Unmarshal(after.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 3 {
		t.Errorf("total after ingest = %v, want 3", body.Total)
	}
}

func TestIndexRendersHTML(t *testing.T) {
	server, _ := newTestServer(t, "")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}

	body := rec.Body.String()
	for _, want := range []string{"audit trail", "app", "identity", "SELECT 1"} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
}

func TestAPIMetaListsFilterValues(t *testing.T) {
	server, _ := newTestServer(t, "")

	req := httptest.NewRequest(http.MethodGet, "/api/meta", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	var body struct {
		Databases []string `json:"databases"`
		Tools     []string `json:"tools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.Databases) != 2 || len(body.Tools) != 2 {
		t.Errorf("meta = %+v, want 2 databases and 2 tools", body)
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	server, _ := newTestServer(t, "")

	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestTTLCacheExpires(t *testing.T) {
	cache := newTTLCache(10 * time.Millisecond)

	cache.put("/x", []byte("body"))

	if _, ok := cache.get("/x"); !ok {
		t.Fatal("entry should be present immediately")
	}

	time.Sleep(20 * time.Millisecond)

	if _, ok := cache.get("/x"); ok {
		t.Error("entry should have expired")
	}
}

func TestTTLCacheEvictsWhenFull(t *testing.T) {
	cache := newTTLCache(time.Minute)

	for i := 0; i < 600; i++ {
		cache.put(string(rune('a'+i%26))+string(rune(i)), []byte("x"))
	}

	if len(cache.entries) > 512 {
		t.Errorf("cache grew to %d entries", len(cache.entries))
	}
}
