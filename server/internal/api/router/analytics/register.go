// Package analytics is the Go port of the analytics tRPC router
// (packages/api/src/router/analytics.ts) and of the snapshot pipeline it
// triggers (packages/analytics/src): one admin mutation,
// analytics.sendAnalytics, that returns {status} where status is
//
//	"sent"     — the snapshot reached PostHog and the success timestamp advanced;
//	"skipped"  — the last successful snapshot is younger than 7 days (isSnapshotDue);
//	"disabled" — the analytics server setting has enableGeneral = false;
//	"failed"   — any error along the way (including the missing analytics
//	             setting row of a fresh instance: upstream's
//	             updateAnalyticsServerSettingAsync throws
//	             "Analytics server settings are missing" and the catch
//	             degrades the status, send-server-analytics.ts:223-226).
//
// The snapshot itself (sendSnapshotAsync, send-server-analytics.ts:76-220)
// counts ~25 datasets over the shared database, renders the PostHog
// properties (omitZeroCounts drops zero count* keys) and captures ONE event
// "server-analytics" whose uuid is derived from the instance id and the
// 7-day snapshot period (getSnapshotUuid, send-server-analytics.ts:57-60).
// Concurrent calls share one in-flight snapshot (the pendingSnapshot promise,
// send-server-analytics.ts:228-236).
//
// Documented deviations: the PostHog wire is a direct POST of the
// posthog-node /batch/ frame (api key, batch with the captured message and
// $geoip_disable) to the same host — the exact posthog-node client internals
// (feature flag prefetches, compression) are not load-bearing for Homarr;
// databaseType is always "sqlite" and the auth providers list is always
// ["credentials"] (the fork ships one driver, ARCHITECTURE.md); the
// env.NO_EXTERNAL_CONNECTION branch lives behind the router gate upstream
// (PRECONDITION_FAILED "External connections are disabled") and is not
// ported like the other external proxies. The settings update keeps the
// upstream compare-and-swap loop (5 attempts, updateAnalyticsServerSettingAsync,
// server-setting.ts:66-90) instead of the read-merge-write the other routers
// use, because the mutation is concurrent by nature.
package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Register registers the analytics router. Call once at startup.
func Register(r *trpc.Router, database *db.DB, opts Options) {
	opts.defaults()
	s := &service{db: database, opts: opts}

	// analytics.sendAnalytics (analytics.ts:11-19):
	// permissionRequiredProcedure.requiresPermission("admin"), a mutation —
	// wrapped with admin.Writable (the credentialProcedure read-only gate).
	r.Register("analytics.sendAnalytics", admin.Writable(s.sendAnalytics))
}

// Options configures the analytics router. The zero value posts to the real
// PostHog host with the version of the upstream package.json.
type Options struct {
	// Version is the reported homarrVersion (default "2.11.1").
	Version string
	// PostHogURL overrides the capture endpoint (test seam; the default is
	// POSTHOG_HOST + "/batch/", client.ts:5-6).
	PostHogURL string
	// HTTPClient overrides the HTTP client (tests).
	HTTPClient poster
	// Now overrides the clock (tests).
	Now func() time.Time
	// UptimeSeconds overrides the process uptime (tests).
	UptimeSeconds int64
}

// poster is the HTTP client seam (the Do of *http.Client).
type poster interface {
	Do(req *http.Request) (*http.Response, error)
}

type service struct {
	db   *db.DB
	opts Options

	// pending dedups concurrent snapshots (pendingSnapshot,
	// send-server-analytics.ts:228-236). The mutex is held for the whole
	// snapshot: a concurrent caller waits and shares the result, like
	// awaiting the same promise.
	pendingMu  sync.Mutex
	pendingRun bool
	pendingRes chan string
}

// sendAnalytics mirrors the router body (analytics.ts:12-19): admin gate,
// then the snapshot status.
func (s *service) sendAnalytics(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	caller, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequirePermission(caller, "admin"); terr != nil {
		return nil, terr
	}
	return map[string]any{"status": s.sendServerAnalytics(ctx)}, nil
}

// sendServerAnalytics is sendServerAnalyticsAsync (send-server-analytics.ts:
// 228-236): one shared in-flight snapshot.
func (s *service) sendServerAnalytics(ctx context.Context) string {
	s.pendingMu.Lock()
	if s.pendingRun {
		res := s.pendingRes
		s.pendingMu.Unlock()
		select {
		case status := <-res:
			return status
		case <-ctx.Done():
			return "failed"
		}
	}
	s.pendingRun = true
	res := make(chan string, 1)
	s.pendingRes = res
	s.pendingMu.Unlock()

	status := s.sendSnapshot(ctx)

	s.pendingMu.Lock()
	s.pendingRun = false
	s.pendingRes = nil
	s.pendingMu.Unlock()
	res <- status
	return status
}

// sendSnapshot is sendSnapshotAsync (send-server-analytics.ts:76-220):
// settings gate, due check, property collection, capture, success stamp.
func (s *service) sendSnapshot(ctx context.Context) string {
	settings, terr := analyticsSettings(ctx, s.db)
	if terr != nil {
		slog.Warn("analytics: failed to read settings", "error", terr)
		return "disabled"
	}
	if !settings.EnableGeneral {
		slog.Info("Analytics are disabled. No data will be sent. Enable analytics in the settings")
		return "disabled"
	}

	now := s.now()
	if !IsSnapshotDue(settings.LastSuccessfulSnapshotAt, now) {
		return "skipped"
	}

	status, err := s.capture(ctx, settings, now)
	if err != nil {
		slog.Warn("Failed to send analytics to PostHog", "error", err)
		return "failed"
	}
	return status
}

// capture collects the properties, posts the snapshot and stamps the
// success. Errors after the due check produce "failed"; the disabled re-check
// before the capture produces "disabled" (send-server-analytics.ts:207-208).
func (s *service) capture(ctx context.Context, settings analyticsSettingsShape, now time.Time) (string, error) {
	// getOrCreateInstanceId runs BEFORE the property collection
	// (send-server-analytics.ts:90-92).
	instanceID := settings.InstanceID
	if instanceID == "" {
		id, err := s.getOrCreateInstanceID(ctx, settings)
		if err != nil {
			return "", err
		}
		instanceID = id
	}

	properties, err := s.collectProperties(ctx, now)
	if err != nil {
		return "", err
	}

	// The upstream re-read happens AFTER the collection: a settings change
	// during a slow snapshot still disables the send
	// (send-server-analytics.ts:206-208).
	current, terr := analyticsSettings(ctx, s.db)
	if terr != nil {
		return "", terr
	}
	if !current.EnableGeneral {
		return "disabled", nil
	}

	if err := s.post(ctx, instanceID, now, properties); err != nil {
		return "", err
	}
	// lastSuccessfulSnapshotAt = now (send-server-analytics.ts:213-215).
	if _, err := updateAnalyticsSettings(ctx, s.db, func(current analyticsSettingsShape) analyticsSettingsShape {
		current.LastSuccessfulSnapshotAt = admin.ISOString(now)
		return current
	}); err != nil {
		return "", err
	}
	slog.Info(fmt.Sprintf("Sent analytics to PostHog in %s", "a moment"))
	return "sent", nil
}

// getOrCreateInstanceID is getOrCreateInstanceId
// (send-server-analytics.ts:34-45): the stored instanceId or a fresh cuid
// written through the CAS update.
func (s *service) getOrCreateInstanceID(ctx context.Context, settings analyticsSettingsShape) (string, error) {
	if settings.InstanceID != "" {
		return settings.InstanceID, nil
	}
	instanceID := admin.CreateID()
	updated, err := updateAnalyticsSettings(ctx, s.db, func(current analyticsSettingsShape) analyticsSettingsShape {
		if current.InstanceID != "" {
			return current
		}
		current.InstanceID = instanceID
		return current
	})
	if err != nil {
		return "", err
	}
	if updated.InstanceID != "" {
		return updated.InstanceID, nil
	}
	return instanceID, nil
}

// analyticsSettingsShape is ServerSettings["analytics"] (server-settings
// index.ts:131-135) with the defaults merged.
type analyticsSettingsShape struct {
	EnableGeneral            bool
	InstanceID               string // empty = null
	LastSuccessfulSnapshotAt string // empty = null
}

// analyticsSettings is getServerSettingByKeyAsync(db, "analytics")
// (server-setting.ts:34-51): the row parsed over the defaults; a missing row
// falls back to the defaults like the upstream read.
func analyticsSettings(ctx context.Context, database *db.DB) (analyticsSettingsShape, *trpc.Error) {
	out := analyticsSettingsShape{EnableGeneral: true}
	row, err := database.GetServerSetting(ctx, "analytics")
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return out, nil
		}
		return analyticsSettingsShape{}, admin.InternalError(err)
	}
	var envelope struct {
		JSON struct {
			EnableGeneral            *bool  `json:"enableGeneral"`
			InstanceID               string `json:"instanceId"`
			LastSuccessfulSnapshotAt string `json:"lastSuccessfulSnapshotAt"`
		} `json:"json"`
	}
	if err := json.Unmarshal([]byte(row.Value), &envelope); err != nil {
		// A corrupt row reads as the defaults (the upstream parse would
		// throw; the port degrades like parseBrandingSettings does).
		return out, nil
	}
	if envelope.JSON.EnableGeneral != nil {
		out.EnableGeneral = *envelope.JSON.EnableGeneral
	}
	out.InstanceID = envelope.JSON.InstanceID
	out.LastSuccessfulSnapshotAt = envelope.JSON.LastSuccessfulSnapshotAt
	return out, nil
}

// updateAnalyticsSettings is updateAnalyticsServerSettingAsync
// (server-setting.ts:66-90): 5 compare-and-swap attempts over the raw value
// column; a missing row errors ("Analytics server settings are missing") and
// losing 5 races errors too ("...changed too often to update").
func updateAnalyticsSettings(ctx context.Context, database *db.DB, update func(analyticsSettingsShape) analyticsSettingsShape) (analyticsSettingsShape, error) {
	for attempt := 0; attempt < 5; attempt++ {
		row, err := database.GetServerSetting(ctx, "analytics")
		if err != nil {
			return analyticsSettingsShape{}, fmt.Errorf("analytics: %w", err)
		}
		current, perr := parseAnalyticsRow(row.Value)
		if perr != nil {
			return analyticsSettingsShape{}, fmt.Errorf("analytics: parse settings: %w", perr)
		}
		next := update(current)
		payload := admin.WrapSuperJSON(next)
		res, err := database.ExecContext(ctx,
			`UPDATE "serverSetting" SET value = ? WHERE setting_key = 'analytics' AND value = ?`,
			payload, row.Value)
		if err != nil {
			return analyticsSettingsShape{}, fmt.Errorf("analytics: update settings: %w", err)
		}
		if affected, _ := res.RowsAffected(); affected > 0 {
			return next, nil
		}
	}
	return analyticsSettingsShape{}, errors.New("Analytics server settings changed too often to update")
}

// parseAnalyticsRow decodes one stored analytics value over the defaults.
func parseAnalyticsRow(raw string) (analyticsSettingsShape, error) {
	out := analyticsSettingsShape{EnableGeneral: true}
	var envelope struct {
		JSON struct {
			EnableGeneral            *bool  `json:"enableGeneral"`
			InstanceID               string `json:"instanceId"`
			LastSuccessfulSnapshotAt string `json:"lastSuccessfulSnapshotAt"`
		} `json:"json"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return out, err
	}
	if envelope.JSON.EnableGeneral != nil {
		out.EnableGeneral = *envelope.JSON.EnableGeneral
	}
	out.InstanceID = envelope.JSON.InstanceID
	out.LastSuccessfulSnapshotAt = envelope.JSON.LastSuccessfulSnapshotAt
	return out, nil
}
