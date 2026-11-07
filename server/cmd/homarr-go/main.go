// Command homarr-go is the single Go binary that replaces the Homarr
// runtime stack (nginx + redis + next.js + websocket/tasks apps).
//
// M2: SQLite database (drizzle-compatible migrations), Auth.js-compatible
// credentials login at /api/auth/*, and the ported tRPC routers (board,
// section, app, userItemState, user, group, invite, onboard, serverSettings,
// searchEngine, apiKeys, info) mounted behind the session middleware.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	homarr "github.com/ydtoloza/homarr/server"
	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/apikeys"
	"github.com/ydtoloza/homarr/server/internal/api/router/bangs"
	"github.com/ydtoloza/homarr/server/internal/api/router/board"
	dockerrouter "github.com/ydtoloza/homarr/server/internal/api/router/docker"
	"github.com/ydtoloza/homarr/server/internal/api/router/group"
	"github.com/ydtoloza/homarr/server/internal/api/router/info"
	"github.com/ydtoloza/homarr/server/internal/api/router/invite"
	"github.com/ydtoloza/homarr/server/internal/api/router/onboard"
	"github.com/ydtoloza/homarr/server/internal/api/router/searchengine"
	"github.com/ydtoloza/homarr/server/internal/api/router/serversettings"
	"github.com/ydtoloza/homarr/server/internal/api/router/user"
	"github.com/ydtoloza/homarr/server/internal/api/router/widgets"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/auth"
	"github.com/ydtoloza/homarr/server/internal/bus"
	"github.com/ydtoloza/homarr/server/internal/config"
	"github.com/ydtoloza/homarr/server/internal/db"
	"github.com/ydtoloza/homarr/server/internal/httpserver"
	dockerint "github.com/ydtoloza/homarr/server/internal/integrations/docker"
	"github.com/ydtoloza/homarr/server/internal/jobs"
	"github.com/ydtoloza/homarr/server/internal/ws"
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

	database, err := db.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer database.Close()
	// Apply the embedded drizzle migrations (idempotent; upstream runs them
	// at container start via node ./db/migrations/sqlite/migrate.cjs).
	if err := database.Migrate(); err != nil {
		return fmt.Errorf("apply database migrations: %w", err)
	}
	slog.Info("database ready", "path", cfg.DBPath(), "migrations", mustMigrationCount(database))

	authSvc := auth.New(database, auth.Options{Secret: secretForCSRF()})

	// In-process event bus replaces upstream's Redis pub/sub; the scheduler
	// produces domain events (ping results, job status) and the WebSocket hub
	// fans them out to browsers.
	eventBus := bus.New()
	scheduler := jobs.New(database, eventBus)
	scheduler.Register(jobs.PingJob(database, eventBus))
	hub := ws.New(eventBus, websocketSessions(authSvc, database), ws.Options{
		KeepAlive: &ws.KeepAlive{}, // upstream defaults: 30s ping / 5s pong timeout
	})
	registerSubscriptions(hub)
	if err := scheduler.Start(ctx); err != nil {
		return err
	}
	// LIFO on shutdown: the scheduler stops producing first, then the hub
	// broadcasts the reconnect notification and closes (upstream SIGTERM
	// behavior, apps/websocket/src/main.ts:110-118).
	defer hub.Close()
	defer scheduler.Stop()
	slog.Info("cron scheduler and websocket hub ready")

	api := trpc.New()
	registerRouters(api, database)

	// Docker domain: endpoints come from the environment (DOCKER_ENDPOINTS /
	// DOCKER_SOCKET_PATHS / DOCKER_HOST, see DOCKER.md), not the database.
	// The service also exposes the docker.subscribeLogs WebSocket bridge.
	var dockerSvc *dockerrouter.Service
	if dockerRegistry, err := dockerint.RegistryFromEnv(os.Getenv); err != nil {
		slog.Warn("docker endpoint registry unavailable; docker routes disabled", "err", err)
	} else {
		dockerSvc = dockerrouter.Register(api, database, dockerRegistry, dockerrouter.Options{})
		for path, handler := range dockerSvc.SubscriptionHandlers() {
			hub.Register(path, handler)
		}
		slog.Info("docker routes enabled", "endpoints", len(dockerRegistry.Endpoints()))
	}
	slog.Info("tRPC routers registered")

	srv := &http.Server{
		Addr: ":" + cfg.Port,
		Handler: httpserver.New(httpserver.Options{
			Static:          homarr.Sub(),
			TRPC:            authSvc.Middleware()(withRequestMeta(api.Handler())),
			Auth:            authSvc.Handler(),
			WebSockets:      hub.Handler(),
			OnboardingClaim: authSvc.Middleware()(onboard.ClaimHandler(database)),
			Version:         version,
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

// registerSubscriptions wires the tRPC subscription paths the WebSocket hub
// serves to their event sources. Only paths with a live producer are
// registered; log.subscribe and docker.subscribeLogs land in M4 with their
// producers (see WEBSOCKET.md).
func registerSubscriptions(hub *ws.Hub) {
	// cronJobs.subscribeToStatusUpdates: admin-only, mirrors
	// packages/api/src/router/cron-jobs.ts:62-82.
	hub.Register("cronJobs.subscribeToStatusUpdates", ws.SubscriptionHandlerFunc(
		func(ctx context.Context, req *ws.Request) (*ws.Subscription, error) {
			if req.Session == nil {
				return nil, trpc.NewError(trpc.CodeUnauthorized, "UNAUTHORIZED")
			}
			if !req.Session.IsAdministrator {
				return nil, trpc.NewError(trpc.CodeForbidden, "Permission denied")
			}
			return &ws.Subscription{Pattern: "events.cronjob.*"}, nil
		}))
}

// websocketSessions adapts auth.Auth to the ws.SessionValidator contract.
func websocketSessions(authSvc *auth.Auth, database *db.DB) ws.SessionValidator {
	return ws.SessionValidatorFunc(func(ctx context.Context, token string) (*ctxauth.Session, error) {
		s, err := authSvc.ValidateSession(ctx, token)
		if err != nil {
			return nil, err
		}
		// "admin" is the admin group permission key
		// (packages/definitions/src/group.ts).
		isAdmin, err := database.UserHasPermission(ctx, s.UserID, "admin")
		if err != nil {
			return nil, err
		}
		return &ctxauth.Session{UserID: s.UserID, IsAdministrator: isAdmin}, nil
	})
}

// registerRouters mounts every ported tRPC router under its exact upstream
// paths. Deferred procedures and routers are listed in the per-domain docs
// (BOARDS.md, ADMIN.md, AUTH.md).
func registerRouters(api *trpc.Router, database *db.DB) {
	board.Register(api, database, board.Options{
		DeviceTypeFromContext: deviceTypeFromContext,
	})
	user.Register(api, database, user.Options{
		OnboardingClaimTokenFromContext: onboardingClaimFromContext,
	})
	group.Register(api, database, group.Options{})
	invite.Register(api, database, invite.Options{})
	onboard.Register(api, database, onboard.Options{
		OnboardingClaimTokenFromContext: onboardingClaimFromContext,
	})
	serversettings.Register(api, database, serversettings.Options{})
	searchengine.Register(api, database, searchengine.Options{})
	bangs.Register(api, database, bangs.Options{}) // deferred upstream (duckduckgo proxy), M3
	apikeys.Register(api, database, apikeys.Options{})
	info.Register(api, database, info.Options{Version: version})
	widgets.Register(api, widgets.Options{
		ResolveIntegrations: resolveIntegrations(database),
	})
}

// Request-scoped context values consumed by router hooks, mirroring the
// upstream tRPC context creation (packages/api/src/trpc.ts:53-71): the
// caller device type comes from the User-Agent and the onboarding claim
// token from its cookie.
type deviceKeyType struct{}
type onboardingClaimKeyType struct{}

// withRequestMeta injects the device type and onboarding claim cookie into
// the request context.
func withRequestMeta(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ctx = context.WithValue(ctx, deviceKeyType{}, deviceTypeFromUserAgent(r.UserAgent()))
		if c, err := r.Cookie(onboard.OnboardingClaimCookieName); err == nil && c.Value != "" {
			ctx = context.WithValue(ctx, onboardingClaimKeyType{}, c.Value)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// deviceTypeFromUserAgent classifies the caller the way the frontend does:
// only phones get the mobile home board (packages/api/src/trpc.ts:65).
func deviceTypeFromUserAgent(ua string) string {
	u := strings.ToLower(ua)
	if strings.Contains(u, "mobile") && !strings.Contains(u, "ipad") && !strings.Contains(u, "tablet") {
		return "mobile"
	}
	return "desktop"
}

func deviceTypeFromContext(ctx context.Context) string {
	v, _ := ctx.Value(deviceKeyType{}).(string)
	return v
}

// resolveIntegrations is the DB-backed integration middleware seam
// (WIDGETS.md §2). Until the integration CRUD routers land (M4), no
// integrations resolve and the widget queries report their per-integration
// public errors exactly like an empty upstream database would.
func resolveIntegrations(database *db.DB) widgets.ResolveIntegrations {
	return nil
}

func onboardingClaimFromContext(ctx context.Context) string {
	v, _ := ctx.Value(onboardingClaimKeyType{}).(string)
	return v
}

// secretForCSRF returns a fresh random secret per process start, matching
// the upstream lifecycle (AUTH_SECRET is regenerated in scripts/run.sh:65);
// the CSRF cookie only needs stability within one process lifetime.
func secretForCSRF() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("homarr-go: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func mustMigrationCount(database *db.DB) int {
	n, err := database.MigrationCount()
	if err != nil {
		return -1
	}
	return n
}

func setupLogging() {
	level := slog.LevelInfo
	if os.Getenv("HOMARR_LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
}
