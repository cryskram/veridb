// Command veridb-viewer serves the VeriDB audit trail as a small web UI and a
// JSON API.
//
// It tails the JSONL file that veridb writes (audit.sinks must include "file"),
// loads new records into SQLite, and answers queries from there. It is designed
// to run as a container with an ngrok sidecar for remote access.
//
// Everything is configured through the environment:
//
//	AUDIT_FILE        JSONL trail written by veridb   (default /data/veridb-audit.jsonl)
//	AUDIT_DB          SQLite file the viewer owns     (default /data/veridb-audit.db)
//	VIEWER_ADDR       listen address                  (default :8080)
//	VIEWER_TOKEN      shared secret; empty disables auth
//	VIEWER_CACHE_TTL  response cache lifetime         (default 5s)
//	VIEWER_INTERVAL   audit file poll interval        (default 2s)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cryskram/veridb/internal/viewer"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("veridb-viewer: %v", err)
	}
}

func run() error {
	addr := flag.String("addr", env("VIEWER_ADDR", ":8080"), "listen address")
	token := flag.String("token", env("VIEWER_TOKEN", ""), "shared access token; empty disables auth")
	source := flag.String("source", env("AUDIT_FILE", "/data/veridb-audit.jsonl"), "JSONL audit trail to ingest")
	storePath := flag.String("db", env("AUDIT_DB", "/data/veridb-audit.db"), "SQLite store path")
	cacheTTL := flag.Duration("cache-ttl", envDuration("VIEWER_CACHE_TTL", 5*time.Second), "response cache lifetime")
	interval := flag.Duration("interval", envDuration("VIEWER_INTERVAL", 2*time.Second), "audit file poll interval")
	healthcheck := flag.Bool("healthcheck", false, "probe a running viewer and exit 0 when healthy")
	flag.Parse()

	// A distroless image has no shell and no curl, so the container healthcheck
	// asks the binary to probe itself.
	if *healthcheck {
		return selfCheck(*addr, 3*time.Second)
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)

	if *token == "" {
		logger.Printf("warning: VIEWER_TOKEN is not set, the viewer is unprotected. " +
			"Set it before exposing the viewer through a tunnel")
	}

	store, err := viewer.OpenStore(*storePath)
	if err != nil {
		return err
	}
	defer store.Close()

	server, err := viewer.NewServer(viewer.Config{
		Addr:            *addr,
		Token:           *token,
		CacheTTL:        *cacheTTL,
		SourceFile:      *source,
		RefreshInterval: *interval,
	}, store, logger)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go server.RunIngest(ctx)

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Printf("veridb-viewer listening on %s (source %s, store %s, cache %s)",
			*addr, *source, *storePath, cacheTTL.String())
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Printf("veridb-viewer shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return httpServer.Shutdown(shutdownCtx)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// selfCheck requests /healthz on the local listener and reports whether it came
// back healthy. It is used as the container healthcheck.
func selfCheck(addr string, timeout time.Duration) error {
	// The listen address is of the form ":8080" or "0.0.0.0:8080"; a probe must
	// always target loopback.
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse listen address %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	client := &http.Client{Timeout: timeout}
	url := fmt.Sprintf("http://%s/healthz", net.JoinHostPort(host, port))

	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("health probe %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health probe %s: status %d", url, resp.StatusCode)
	}

	return nil
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}

	d, err := time.ParseDuration(raw)
	if err != nil {
		log.Printf("veridb-viewer: %s=%q is not a duration, using %s", key, raw, fallback)
		return fallback
	}

	return d
}
