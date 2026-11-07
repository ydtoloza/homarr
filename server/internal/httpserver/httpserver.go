// Package httpserver wires the homarr-go HTTP surface: health endpoints,
// the embedded static frontend, and — from M1 on — the tRPC-compatible API.
package httpserver

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Options configures the HTTP server.
type Options struct {
	// Static is the frontend dist filesystem (SPA root). Optional during M0.
	Static fs.FS
	// TRPC is the tRPC v11 API handler mounted at /api/trpc/*. Nil skips the
	// mount (placeholder builds without the API).
	TRPC http.Handler
	// Auth is the Auth.js-compatible REST surface mounted at /api/auth/*
	// (csrf, callback/credentials, session, signout). Nil skips the mount.
	Auth http.Handler
	// WebSockets is the tRPC WS hub mounted at /websockets (the path the
	// frontend connects to; upstream nginx proxies it there). Nil skips the
	// mount.
	WebSockets http.Handler
	// Version is reported by the health endpoints.
	Version string
	// Started is the process start time, reported as uptime.
	Started time.Time
}

// New builds the root chi router of homarr-go.
func New(opts Options) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(requestLogger)

	started := opts.Started
	if started.IsZero() {
		started = time.Now()
	}

	r.Get("/api/health/live", healthHandler(opts, started, "live"))
	r.Get("/api/health/ready", healthHandler(opts, started, "ready"))

	if opts.TRPC != nil {
		r.Handle("/api/trpc/*", opts.TRPC)
	}
	if opts.Auth != nil {
		r.Handle("/api/auth/*", opts.Auth)
	}
	if opts.WebSockets != nil {
		r.Handle("/websockets", opts.WebSockets)
	}

	// M4 mount point (planned):
	//   SSE subscriptions ride /api/trpc (httpSubscriptionLink) — no extra mount.

	if opts.Static != nil {
		r.Handle("/*", spaHandler(opts.Static))
	} else {
		r.Get("/", landingHandler(opts))
	}

	return r
}

func healthHandler(opts Options, started time.Time, check string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"check":   check,
			"service": "homarr-go",
			"version": opts.Version,
			"uptime":  time.Since(started).Round(time.Second).String(),
		})
	}
}

func landingHandler(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(
			`<!doctype html><html><body style="font-family:system-ui;background:#0b0d10;color:#e6e6e6">` +
				`<h1>homarr-go</h1><p>No frontend embedded in this build (M0).</p></body></html>`))
		_ = opts
	}
}

// spaHandler serves the embedded SPA, falling back to index.html for
// unknown paths so the client-side router takes over (boards, settings...).
func spaHandler(dist fs.FS) http.HandlerFunc {
	fileServer := http.FileServerFS(dist)
	return func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(dist, p); err != nil {
			// Unknown path → serve the SPA shell.
			http.ServeFileFS(w, r, dist, "index.html")
			return
		}
		fileServer.ServeHTTP(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Hijack forwards the WebSocket upgrade: coder/websocket requires the raw
// ResponseWriter to implement http.Hijacker (the type assertion is direct,
// Unwrap is not consulted).
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("httpserver: ResponseWriter does not implement http.Hijacker")
	}
	return hj.Hijack()
}

// Flush forwards stream flushes (tRPC SSE/jsonl responses).
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration", time.Since(start).Round(time.Millisecond),
		)
	})
}
