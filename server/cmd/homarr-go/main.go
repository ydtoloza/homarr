// Command homarr-go is the single Go binary that replaces the Homarr
// runtime stack (nginx + redis + next.js + websocket/tasks apps).
//
// Phase M0: HTTP server with health endpoints and embedded static frontend.
// Later phases mount the tRPC-compatible API, the WebSocket hub and the
// job scheduler under the same process.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	homarr "github.com/ydtoloza/homarr/server"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/config"
	"github.com/ydtoloza/homarr/server/internal/httpserver"
)

// version is injected at build time: -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("homarr-go: fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	cfg := config.Load()
	setupLogging()
	if limit, ok := cfg.MemLimitBytes(); ok {
		debug.SetMemoryLimit(limit)
		slog.Info("soft memory limit set", "bytes", limit)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	api := trpc.New()
	// M2 registers the ported routers here (board, user, auth, ...). Until
	// then every call answers the correct tRPC NOT_FOUND envelope on the wire.
	srv := &http.Server{
		Addr: ":" + cfg.Port,
		Handler: httpserver.New(httpserver.Options{
			Static:  homarr.Sub(),
			TRPC:    api.Handler(),
			Version: version,
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("homarr-go listening", "port", cfg.Port, "data_dir", cfg.DataDir, "version", version)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
	}
	return nil
}

func setupLogging() {
	level := slog.LevelInfo
	if os.Getenv("HOMARR_LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
}
