// Command server is the HTTP service behind anurupbansal.in.
//
// It serves the static site from an embedded filesystem (so the binary is the
// entire deployable — no volume mounts, no files to sync) and exposes a health
// endpoint. Caddy sits in front and handles TLS.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/AnurupBansal/portfolio/internal/web"
)

// Injected at build time via -ldflags. See Dockerfile.
var (
	version = "dev"
	commit  = "unknown"
)

var startedAt = time.Now()

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: routes(logger),

		// These matter more than they look on a box facing the open internet.
		// Without them a slow client can hold a connection open indefinitely,
		// and enough of those exhausts the server. Caddy absorbs most of this,
		// but defence in depth is free here.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Run the server in its own goroutine so main can wait on signals.
	errCh := make(chan error, 1)
	go func() {
		logger.Info("server starting",
			slog.String("addr", addr),
			slog.String("version", version),
			slog.String("commit", commit),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// Graceful shutdown. Docker sends SIGTERM on `docker compose down` and on
	// container replacement during a deploy; without this, in-flight requests
	// are severed mid-response and the client sees a connection reset.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		logger.Error("server failed", slog.Any("err", err))
		os.Exit(1)
	case sig := <-stop:
		logger.Info("shutdown signal received", slog.String("signal", sig.String()))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("graceful shutdown failed, forcing close", slog.Any("err", err))
		_ = srv.Close()
		os.Exit(1)
	}
	logger.Info("shutdown complete")
}

func routes(logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /api/health", handleHealth())
	mux.Handle("GET /", web.StaticHandler())

	return requestLogger(logger, mux)
}

// handleHealth reports liveness plus a little build and runtime detail.
// Deliberately public: it doubles as the first entry in the API playground
// planned for a later phase.
func handleHealth() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "ok",
			"version":    version,
			"commit":     commit,
			"uptime_sec": int64(time.Since(startedAt).Seconds()),
			"go":         runtime.Version(),
			"goroutines": runtime.NumGoroutine(),
		})
	})
}

// requestLogger emits one structured line per request. Status and byte count
// need capturing off the ResponseWriter, which is what statusRecorder is for.
func requestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		logger.Info("request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int64("bytes", rec.bytes),
			slog.Int64("dur_ms", time.Since(start).Milliseconds()),
			// Caddy sets this; it's the real client IP rather than the proxy's.
			slog.String("ip", clientIP(r)),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func clientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return ip
	}
	return r.RemoteAddr
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := encodeJSON(w, v); err != nil {
		slog.Error("encode response", slog.Any("err", err))
	}
}
