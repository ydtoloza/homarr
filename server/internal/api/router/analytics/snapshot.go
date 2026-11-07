package analytics

// The snapshot pipeline: due check, property collection, PostHog capture and
// the derived snapshot uuid. Ported from packages/analytics/src
// (send-server-analytics.ts, snapshot-properties.ts, client.ts).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// snapshotIntervalMs is the snapshot interval of snapshot-properties.ts:1:
// one week.
const snapshotIntervalMs = 7 * 24 * 60 * 60 * 1000

// postHogAPIKey and postHogHost are POSTHOG_API_KEY / POSTHOG_HOST
// (client.ts:5-6).
const (
	postHogAPIKey = "phc_vYBmGWNbRshvfeC7EHfeSmUm2pD2Neg5nGqzJuGvS8Hs"
	postHogHost   = "https://hog.homarr.dev"
)

// defaultVersion is the package.json version the snapshot reports.
const defaultVersion = "2.11.1"

// IsSnapshotDue is isSnapshotDue (snapshot-properties.ts:3-9): due when no
// successful snapshot is recorded, when the stored value is unparsable, or
// when the interval has elapsed.
func IsSnapshotDue(lastSuccessfulSnapshotAt string, now time.Time) bool {
	if lastSuccessfulSnapshotAt == "" {
		return true
	}
	last, err := time.Parse(time.RFC3339Nano, lastSuccessfulSnapshotAt)
	if err != nil {
		return true
	}
	return now.Sub(last) >= time.Duration(snapshotIntervalMs)*time.Millisecond
}

// snapshotPeriod is getSnapshotPeriod (snapshot-properties.ts:11).
func snapshotPeriod(now time.Time) int64 {
	return now.UnixMilli() / snapshotIntervalMs
}

// SnapshotUUID is getSnapshotUuid (send-server-analytics.ts:57-60): a
// version-5-shaped uuid derived from
// sha256(instanceId:snapshotPeriod:server-analytics), deterministic per
// instance and 7-day period so PostHog dedupes replays.
func SnapshotUUID(instanceID string, now time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:server-analytics", instanceID, snapshotPeriod(now))))
	h := hex.EncodeToString(sum[:])[:32]
	return fmt.Sprintf("%s-%s-5%s-a%s-%s", h[0:8], h[8:12], h[13:16], h[17:20], h[20:32])
}

// itemCountBucket is getItemCountBucket (snapshot-properties.ts:13-19).
func itemCountBucket(n int64) string {
	switch {
	case n == 0:
		return "empty"
	case n == 1:
		return "one"
	case n <= 3:
		return "two-to-three"
	case n <= 7:
		return "four-to-seven"
	case n <= 15:
		return "eight-to-fifteen"
	default:
		return "sixteen-plus"
	}
}

// now resolves the clock (Options.Now override).
func (s *service) now() time.Time {
	if s.opts.Now != nil {
		return s.opts.Now()
	}
	return time.Now()
}

// defaults applies the option defaults.
func (o *Options) defaults() {
	if o.Version == "" {
		o.Version = defaultVersion
	}
	if o.PostHogURL == "" {
		o.PostHogURL = postHogHost + "/batch/"
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{}
	}
}

// collectProperties renders the properties object of
// send-server-analytics.ts:162-205. All datasets ride one SQL pass per
// query (the Promise.all upstream).
func (s *service) collectProperties(ctx context.Context, now time.Time) (map[string]any, error) {
	properties := map[string]any{
		"homarrVersion":     s.opts.Version,
		"databaseType":      "sqlite",
		"dockerEnabled":     false, // the docker env flag reaches the composition root; the fork default is off until wired
		"kubernetesEnabled": false,
		"authProviders":     []string{"credentials"},
		"osPlatform":        runtime.GOOS,
		"osArch":            runtime.GOARCH,
		"uptimeSeconds":     int64(now.Sub(processStart) / time.Second),
	}
	if s.opts.UptimeSeconds > 0 {
		properties["uptimeSeconds"] = s.opts.UptimeSeconds
	}

	// defaultLocale from the culture settings (defaults to "en").
	culture := "en"
	row, err := s.db.GetServerSetting(ctx, "culture")
	if err == nil {
		var envelope struct {
			JSON struct {
				DefaultLocale string `json:"defaultLocale"`
			} `json:"json"`
		}
		if json.Unmarshal([]byte(row.Value), &envelope) == nil && envelope.JSON.DefaultLocale != "" {
			culture = envelope.JSON.DefaultLocale
		}
	} else if !errors.Is(err, db.ErrNotFound) {
		return nil, err
	}
	properties["defaultLocale"] = culture

	// Simple counts (the db.$count list, send-server-analytics.ts:99-123).
	counts := map[string]string{
		"countBoards":                  `SELECT COUNT(*) FROM "board"`,
		"countPublicBoards":            `SELECT COUNT(*) FROM "board" WHERE is_public = 1`,
		"countBoardsWithStatusEnabled": `SELECT COUNT(*) FROM "board" WHERE disable_status = 0`,
		"countGroups":                  `SELECT COUNT(*) FROM "group"`,
		"countApps":                    `SELECT COUNT(*) FROM "app"`,
		"countSections":                `SELECT COUNT(*) FROM "section"`,
		"countSearchEngines":           `SELECT COUNT(*) FROM "search_engine"`,
		"countIconRepositories":        `SELECT COUNT(*) FROM "iconRepository"`,
		"countIcons":                   `SELECT COUNT(*) FROM "icon"`,
		"countLayouts":                 `SELECT COUNT(*) FROM "layout"`,
		"countItemLayouts":             `SELECT COUNT(*) FROM "item_layout"`,
		"countSectionLayouts":          `SELECT COUNT(*) FROM "section_layout"`,
		"countCronJobConfigs":          `SELECT COUNT(*) FROM "cron_job_configuration"`,
		"countCustomWidgets":           `SELECT COUNT(*) FROM "custom_widget_v2_definition"`,
		"countTrustedCertificates":     `SELECT COUNT(*) FROM "trusted_certificate_hostname"`,
		"countUsers":                   `SELECT COUNT(*) FROM "user"`,
		"countApiKeys":                 `SELECT COUNT(*) FROM "apiKey"`,
		"countInvites":                 `SELECT COUNT(*) FROM "invite"`,
		"countMedias":                  `SELECT COUNT(*) FROM "media"`,
		"countSessions":                `SELECT COUNT(*) FROM "session"`,
		"countAccounts":                `SELECT COUNT(*) FROM "account"`,
	}
	for key, query := range counts {
		n, err := s.scalar(ctx, query)
		if err != nil {
			return nil, err
		}
		properties[key] = n
	}

	// Per-kind distributions.
	integrationKinds, err := s.kindCounts(ctx, `SELECT kind, COUNT(id) FROM "integration" GROUP BY kind`)
	if err != nil {
		return nil, err
	}
	widgetKinds, err := s.kindCounts(ctx, `SELECT kind, COUNT(id) FROM "item" GROUP BY kind`)
	if err != nil {
		return nil, err
	}
	boardsWithWidgetKinds, err := s.kindCounts(ctx, `SELECT kind, COUNT(DISTINCT board_id) FROM "item" GROUP BY kind`)
	if err != nil {
		return nil, err
	}
	boardsWithIntegrationKinds, err := s.kindCounts(ctx, `SELECT i.kind, COUNT(DISTINCT it.board_id)
		FROM "integration_item" ii
		JOIN "integration" i ON i.id = ii.integration_id
		JOIN "item" it ON it.id = ii.item_id
		GROUP BY i.kind`)
	if err != nil {
		return nil, err
	}
	layoutRoleCounts, err := s.kindCounts(ctx, `SELECT role, COUNT(id) FROM "layout" GROUP BY role`)
	if err != nil {
		return nil, err
	}
	sectionKindCounts, err := s.kindCounts(ctx, `SELECT kind, COUNT(id) FROM "section" GROUP BY kind`)
	if err != nil {
		return nil, err
	}

	sum := func(rows map[string]int64) int64 {
		var total int64
		for _, n := range rows {
			total += n
		}
		return total
	}
	properties["countIntegrations"] = sum(integrationKinds)
	properties["countWidgets"] = sum(widgetKinds)

	// countBoardsWithStatusEnabled is only reported when status is not
	// force-disabled and at least one board enables it
	// (send-server-analytics.ts:198-200); the upstream shape adds the key
	// conditionally, so a forced-disable removes it here.
	forceDisableStatus, err := boardForceDisableStatus(ctx, s.db)
	if err != nil {
		return nil, err
	}
	if forceDisableStatus {
		delete(properties, "countBoardsWithStatusEnabled")
	}

	for kind, n := range integrationKinds {
		if n > 0 {
			properties["integration_"+kind] = n
		}
	}
	for kind, n := range widgetKinds {
		if n > 0 {
			properties["widget_"+kind] = n
		}
	}
	for kind, n := range boardsWithWidgetKinds {
		if n > 0 {
			properties["boardsWithWidget_"+kind] = n
		}
	}
	for kind, n := range boardsWithIntegrationKinds {
		if n > 0 {
			properties["boardsWithIntegration_"+kind] = n
		}
	}
	for kind, n := range layoutRoleCounts {
		if n > 0 {
			properties["layout_"+kind] = n
		}
	}
	for kind, n := range sectionKindCounts {
		if n > 0 {
			properties["section_"+kind] = n
		}
	}

	// Board size buckets (send-server-analytics.ts:231-241 upstream tail).
	boardItemCounts, err := s.kindCounts(ctx, `SELECT board_id, COUNT(id) FROM "item" GROUP BY board_id`)
	if err != nil {
		return nil, err
	}
	emptyBoards := properties["countBoards"].(int64) - int64(len(boardItemCounts))
	buckets := map[string]int64{}
	if emptyBoards > 0 {
		buckets["empty"] = emptyBoards
	}
	for _, n := range boardItemCounts {
		bucket := itemCountBucket(n)
		buckets[bucket]++
	}
	for bucket, n := range buckets {
		properties["boardsWithItemCount_"+bucket] = n
	}

	// omitZeroCounts (snapshot-properties.ts:21-24): count* keys with 0 are
	// dropped.
	for key, v := range properties {
		if strings.HasPrefix(key, "count") {
			if n, ok := v.(int64); ok && n == 0 {
				delete(properties, key)
			}
		}
	}
	return properties, nil
}

// scalar runs a one-value query.
func (s *service) scalar(ctx context.Context, query string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, query).Scan(&n)
	return n, err
}

// kindCounts runs a GROUP BY into a kind → count map.
func (s *service) kindCounts(ctx context.Context, query string) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var (
			kind string
			n    int64
		)
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, err
		}
		out[kind] = n
	}
	return out, rows.Err()
}

// boardForceDisableStatus reads board.forceDisableStatus from the board
// server settings (defaults false).
func boardForceDisableStatus(ctx context.Context, database *db.DB) (bool, error) {
	row, err := database.GetServerSetting(ctx, "board")
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	var envelope struct {
		JSON struct {
			ForceDisableStatus bool `json:"forceDisableStatus"`
		} `json:"json"`
	}
	if err := json.Unmarshal([]byte(row.Value), &envelope); err != nil {
		return false, nil
	}
	return envelope.JSON.ForceDisableStatus, nil
}

// postHogMessage is the captured event inside the /batch/ frame.
type postHogMessage struct {
	Event      string         `json:"event"`
	DistinctID string         `json:"distinct_id"`
	Properties map[string]any `json:"properties"`
	Timestamp  string         `json:"timestamp"`
	UUID       string         `json:"uuid"`
}

type postHogBatch struct {
	APIKey string           `json:"api_key"`
	Batch  []postHogMessage `json:"batch"`
}

// post captures the snapshot (client.capture + flush,
// send-server-analytics.ts:209-213): one POST to the /batch/ endpoint with
// the event and the geoip disabled.
func (s *service) post(ctx context.Context, instanceID string, now time.Time, properties map[string]any) error {
	// The capture properties: the omitZeroCounts output plus the
	// $process_person_profile opt-out (send-server-analytics.ts:210-215) and
	// the geoip disable flag (disableGeoip → $geoip_disable, posthog-node).
	payload := make(map[string]any, len(properties)+2)
	for k, v := range properties {
		payload[k] = v
	}
	payload["$process_person_profile"] = false
	payload["$geoip_disable"] = true

	body, err := json.Marshal(postHogBatch{
		APIKey: postHogAPIKey,
		Batch: []postHogMessage{{
			Event:      "server-analytics",
			DistinctID: instanceID,
			Properties: payload,
			Timestamp:  admin.ISOString(now),
			UUID:       SnapshotUUID(instanceID, now),
		}},
	})
	if err != nil {
		return fmt.Errorf("analytics: marshal snapshot: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.opts.PostHogURL, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("analytics: build capture request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := s.opts.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("analytics: capture: %w", err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("analytics: capture responded %d", res.StatusCode)
	}
	return nil
}

// processStart anchors the uptime to the process start (process.uptime()
// upstream).
var processStart = time.Now()
