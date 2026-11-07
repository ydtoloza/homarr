package widgets

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient"
)

// Go port of the downloads widget router
// (packages/api/src/router/widgets/downloads.ts:49-194).
//
//	widget.downloads.getJobsAndStatuses (query, publicProcedure +
//	createManyWidgetIntegrationMiddleware("query", "downloads")):
//	  input: { integrationIds: string[],
//	           limitPerIntegration?: number (default 50),
//	           selection?: { sort: <14 values>, descending, categoryFilter,
//	                         filterIsWhitelist, showCompletedTorrent,
//	                         activeTorrentThreshold },
//	           includeArchivedHistory?: boolean (default false),
//	           historyWindowDays?: int >= 1 (default 7) }
//	  output per integration:
//	    success: { integrationId, integrationName,
//	               integration: { id, name, kind, updatedAt },
//	               data: DownloadClientJobsAndStatus }
//	    failure: { integrationId, integrationName,
//	               integration: { id, name, kind },
//	               data: null, error: "INTEGRATION_REQUEST_FAILED" }
//	  (throwOnAllFailures: true -> every integration failed throws
//	  INTERNAL_SERVER_ERROR "All integration queries failed").
//
//	widget.downloads.pause / pauseItem / resume / resumeItem / deleteItem
//	(mutation, protectedProcedure + interact middleware): run the mutation on
//	every integration, invalidate the cache, then throw the first failure
//	(runDownloadMutationsAsync, downloads.ts:41-47) as INTERNAL_SERVER_ERROR
//	carrying the upstream IntegrationError message.
//
// The data comes from the downloads request handler
// (packages/request-handler/src/downloads.ts:7-23), cached for 10s; the
// updatedAt timestamp is the cache entry time (upstream serializes the Date
// through superjson; the port emits the ISO string convention of ADMIN.md §4).
type downloadsResult struct {
	IntegrationID   string                        `json:"integrationId"`
	IntegrationName string                        `json:"integrationName"`
	Integration     downloadClientInfo            `json:"integration"`
	Data            *downloadclient.JobsAndStatus `json:"data"`
	Error           *string                       `json:"error,omitempty"`
}

type downloadClientInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// selectionSorts is the z.enum of downloads.ts:64-80.
var selectionSorts = []string{
	"name", "progress", "size", "downSpeed", "upSpeed", "time", "added",
	"ratio", "received", "sent", "index", "type", "state", "category", "integration",
}

// DefaultHistoryWindowDays is the historyWindowDays input default
// (downloads.ts:89: z.number().int().min(1).default(7)).
const DefaultHistoryWindowDays = 7

// maxCategoryFilters / maxCategoryFilterLength mirror downloads.ts:82
// (z.array(z.string().max(1000)).max(100)).
const (
	maxCategoryFilters      = 100
	maxCategoryFilterLength = 1000
)

func (s *service) downloadsGetJobsAndStatuses(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	d, terr := newDecoder(input)
	if terr != nil {
		return nil, terr
	}
	integrationIDs, terr := d.StrList("integrationIds")
	if terr != nil {
		return nil, terr
	}

	rows, terr := s.resolve(ctx, ResolveRequest{
		Action:         "query",
		WidgetKind:     WidgetKindDownloads,
		IntegrationIDs: integrationIDs,
	})
	if terr != nil {
		return nil, terr
	}

	// Remaining input fields (procedure-level zod, parsed after the
	// middleware like pageSize above).
	limit := float64(50)
	if d.Has("limitPerIntegration") {
		v, ok := decoderNumber(d, "limitPerIntegration")
		if !ok {
			return nil, trpc.NewError(trpc.CodeBadRequest, `expected a number for input key "limitPerIntegration"`)
		}
		limit = v
	}
	selection, terr := decodeSelection(d)
	if terr != nil {
		return nil, terr
	}
	includeArchived := false
	if d.Has("includeArchivedHistory") {
		b, terr := d.Bool("includeArchivedHistory")
		if terr != nil {
			return nil, terr
		}
		includeArchived = b
	}
	historyWindowDays := int64(DefaultHistoryWindowDays)
	if d.Has("historyWindowDays") {
		v, ok := decoderNumber(d, "historyWindowDays")
		if !ok || v != float64(int64(v)) {
			return nil, trpc.NewError(trpc.CodeBadRequest, `expected an integer for input key "historyWindowDays"`)
		}
		if v < 1 {
			return nil, trpc.NewError(trpc.CodeBadRequest, `input key "historyWindowDays" must be at least 1`)
		}
		historyWindowDays = int64(v)
	}

	results := make([]downloadsResult, len(rows))
	runBounded(ctx, s.opts.parallelism, len(rows), func(idx int) {
		row := rows[idx]
		data, timestamp, err := s.downloadClientDataFor(ctx, row, downloadclient.JobsInput{
			Limit:                  limit,
			Selection:              selection,
			IncludeArchivedHistory: includeArchived,
			HistoryWindowDays:      historyWindowDays,
		})
		if err != nil {
			results[idx] = downloadsResult{
				IntegrationID:   row.ID,
				IntegrationName: row.Name,
				Integration:     downloadClientInfo{ID: row.ID, Name: row.Name, Kind: row.Kind},
				Data:            nil,
				Error:           strPtr(PublicIntegrationError),
			}
			return
		}
		results[idx] = downloadsResult{
			IntegrationID:   row.ID,
			IntegrationName: row.Name,
			Integration: downloadClientInfo{
				ID:        row.ID,
				Name:      row.Name,
				Kind:      row.Kind,
				UpdatedAt: timestamp,
			},
			Data: &data,
		}
	})

	failures := 0
	for _, r := range results {
		if r.Error != nil {
			failures++
		}
	}
	if len(rows) > 0 && failures == len(rows) {
		return nil, allQueriesFailed()
	}
	return results, nil
}

// decodeSelection validates the optional selection object (downloads.ts:62-87):
// every member is required when the object is present.
func decodeSelection(d *decoder) (*downloadclient.Selection, *trpc.Error) {
	raw, ok := d.Raw("selection")
	if !ok || raw == nil {
		return nil, nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, trpc.NewError(trpc.CodeBadRequest, `expected an object for input key "selection"`)
	}
	sd := &decoder{obj: obj}
	sort, terr := sd.Str("sort")
	if terr != nil {
		return nil, badRequestKey("selection.sort")
	}
	if !containsString(selectionSorts, sort) {
		return nil, trpc.NewError(trpc.CodeBadRequest, `input key "selection.sort" has an invalid value`)
	}
	descending, terr := sd.Bool("descending")
	if terr != nil {
		return nil, badRequestKey("selection.descending")
	}
	filterList, terr := sd.StrList("categoryFilter")
	if terr != nil {
		return nil, badRequestKey("selection.categoryFilter")
	}
	if len(filterList) > maxCategoryFilters {
		return nil, badRequestKey("selection.categoryFilter")
	}
	for _, filter := range filterList {
		if len(filter) > maxCategoryFilterLength {
			return nil, badRequestKey("selection.categoryFilter")
		}
	}
	whitelist, terr := sd.Bool("filterIsWhitelist")
	if terr != nil {
		return nil, badRequestKey("selection.filterIsWhitelist")
	}
	showCompleted, terr := sd.Bool("showCompletedTorrent")
	if terr != nil {
		return nil, badRequestKey("selection.showCompletedTorrent")
	}
	threshold, ok := decoderNumber(sd, "activeTorrentThreshold")
	if !ok {
		return nil, badRequestKey("selection.activeTorrentThreshold")
	}
	if threshold < 0 {
		return nil, badRequestKey("selection.activeTorrentThreshold")
	}
	return &downloadclient.Selection{
		Sort:                   sort,
		Descending:             descending,
		CategoryFilter:         filterList,
		FilterIsWhitelist:      whitelist,
		ShowCompletedTorrent:   showCompleted,
		ActiveTorrentThreshold: threshold,
	}, nil
}

func badRequestKey(key string) *trpc.Error {
	return trpc.NewError(trpc.CodeBadRequest, fmt.Sprintf(`invalid input for key "%s"`, key))
}

func containsString(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// downloadClientDataFor builds (or reads from cache) the jobs/status view of
// one integration (downloads.ts requestAsync).
func (s *service) downloadClientDataFor(ctx context.Context, row Integration, input downloadclient.JobsInput) (downloadclient.JobsAndStatus, string, error) {
	key := row.ID + "\x00" + inputHash(input)
	data, ts, err := s.downloadsCache.getOrLoad(key, func() (any, error) {
		client, err := s.opts.newDownloadClient(row)
		if err != nil {
			return nil, err
		}
		jobs, err := client.GetJobsAndStatus(ctx, input)
		if err != nil {
			return nil, err
		}
		if jobs.Items == nil {
			jobs.Items = []downloadclient.Item{}
		}
		if jobs.Status.Types == nil {
			jobs.Status.Types = []string{}
		}
		return jobs, nil
	})
	if err != nil {
		return downloadclient.JobsAndStatus{}, "", err
	}
	return data.(downloadclient.JobsAndStatus), isoMillis(ts), nil
}

// isoMillis renders the cache timestamp like a JS toISOString() call (UTC,
// millisecond precision, trailing Z) — the port convention for Date fields
// (ADMIN.md §4).
func isoMillis(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// --- mutations ---

// runDownloadMutations ports runDownloadMutationsAsync (downloads.ts:41-47):
// run the mutation on EVERY resolved integration, invalidate the cache, then
// surface the first failure as INTERNAL_SERVER_ERROR with the integration
// error message (upstream throws the IntegrationError itself, whose message
// travels through getTRPCErrorFromUnknown).
func (s *service) runDownloadMutations(ctx context.Context, rows []Integration, run func(ctx context.Context, client downloadclient.Client) error) *trpc.Error {
	firstErr := newFirstError()
	runBounded(ctx, s.opts.parallelism, len(rows), func(idx int) {
		row := rows[idx]
		client, err := s.opts.newDownloadClient(row)
		if err != nil {
			firstErr.set(err)
			return
		}
		if err := run(ctx, client); err != nil {
			firstErr.set(err)
		}
	})
	// Cache invalidation runs even when a mutation failed (upstream awaits
	// invalidateCacheAsync before throwing).
	s.downloadsCache.invalidate(idsOf(rows))
	if err := firstErr.get(); err != nil {
		return trpc.NewError(trpc.CodeInternalServerError, downloadclient.AsError(err).Error())
	}
	return nil
}

type firstError struct {
	mu  sync.Mutex
	err error
}

func newFirstError() *firstError { return &firstError{} }

func (f *firstError) set(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err == nil {
		f.err = err
	}
}

func (f *firstError) get() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func idsOf(rows []Integration) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

// decodeItem validates the downloadClientItemSchema of downloads.ts:142
// (id string, index number, name string, type enum, size number, time number,
// state enum, progress 0..1; the optionals are ignored for the mutations —
// only the id and progress are consumed).
func decodeItem(d *decoder) (downloadclient.Item, *trpc.Error) {
	raw, ok := d.Raw("item")
	if !ok || raw == nil {
		return downloadclient.Item{}, badRequestKey("item")
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return downloadclient.Item{}, badRequestKey("item")
	}
	idm := &decoder{obj: obj}
	item := downloadclient.Item{}
	if v, err := idm.Str("id"); err != nil {
		return downloadclient.Item{}, badRequestKey("item.id")
	} else {
		item.ID = v
	}
	if v, ok := decoderNumber(idm, "index"); ok {
		item.Index = int64(v)
	}
	if v, err := idm.Str("name"); err == nil {
		item.Name = v
	}
	if v, err := idm.Str("type"); err == nil {
		switch v {
		case "torrent", "usenet", "miscellaneous":
			item.Type = v
		default:
			return downloadclient.Item{}, badRequestKey("item.type")
		}
	} else {
		return downloadclient.Item{}, badRequestKey("item.type")
	}
	if v, ok := decoderNumber(idm, "size"); ok {
		item.Size = v
	}
	if v, ok := decoderNumber(idm, "time"); ok {
		item.Time = v
	}
	state, err := idm.Str("state")
	if err != nil {
		return downloadclient.Item{}, badRequestKey("item.state")
	}
	switch state {
	case "unknown", "downloading", "queued", "paused", "completed", "failed",
		"processing", "leeching", "stalled", "seeding":
		item.State = state
	default:
		return downloadclient.Item{}, badRequestKey("item.state")
	}
	progress, ok := decoderNumber(idm, "progress")
	if !ok || progress < 0 || progress > 1 {
		return downloadclient.Item{}, badRequestKey("item.progress")
	}
	item.Progress = progress
	return item, nil
}

// downloadsInput parses the common mutation input: the middleware's
// integrationIds plus the procedure fields.
type downloadsMutationInput struct {
	integrationIDs []string
	item           downloadclient.Item
	fromDisk       bool
}

func (s *service) decodeMutationInput(input json.RawMessage, withItem, withFromDisk bool) (downloadsMutationInput, *trpc.Error) {
	d, terr := newDecoder(input)
	if terr != nil {
		return downloadsMutationInput{}, terr
	}
	ids, terr := d.StrList("integrationIds")
	if terr != nil {
		return downloadsMutationInput{}, terr
	}
	out := downloadsMutationInput{integrationIDs: ids}
	if withItem {
		item, terr := decodeItem(d)
		if terr != nil {
			return downloadsMutationInput{}, terr
		}
		out.item = item
	}
	if withFromDisk {
		fromDisk, terr := d.Bool("fromDisk")
		if terr != nil {
			return downloadsMutationInput{}, badRequestKey("fromDisk")
		}
		out.fromDisk = fromDisk
	}
	return out, nil
}

func (s *service) downloadsPause(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	in, terr := s.downloadsMutation(ctx, input, false, false)
	if terr != nil {
		return nil, terr
	}
	if terr := s.runDownloadMutations(ctx, in.rows, func(ctx context.Context, client downloadclient.Client) error {
		return client.PauseQueue(ctx)
	}); terr != nil {
		return nil, terr
	}
	return nil, nil
}

func (s *service) downloadsResume(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	in, terr := s.downloadsMutation(ctx, input, false, false)
	if terr != nil {
		return nil, terr
	}
	if terr := s.runDownloadMutations(ctx, in.rows, func(ctx context.Context, client downloadclient.Client) error {
		return client.ResumeQueue(ctx)
	}); terr != nil {
		return nil, terr
	}
	return nil, nil
}

func (s *service) downloadsPauseItem(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	in, terr := s.downloadsMutation(ctx, input, true, false)
	if terr != nil {
		return nil, terr
	}
	if terr := s.runDownloadMutations(ctx, in.rows, func(ctx context.Context, client downloadclient.Client) error {
		return client.PauseItem(ctx, in.input.item)
	}); terr != nil {
		return nil, terr
	}
	return nil, nil
}

func (s *service) downloadsResumeItem(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	in, terr := s.downloadsMutation(ctx, input, true, false)
	if terr != nil {
		return nil, terr
	}
	if terr := s.runDownloadMutations(ctx, in.rows, func(ctx context.Context, client downloadclient.Client) error {
		return client.ResumeItem(ctx, in.input.item)
	}); terr != nil {
		return nil, terr
	}
	return nil, nil
}

func (s *service) downloadsDeleteItem(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	in, terr := s.downloadsMutation(ctx, input, true, true)
	if terr != nil {
		return nil, terr
	}
	if terr := s.runDownloadMutations(ctx, in.rows, func(ctx context.Context, client downloadclient.Client) error {
		return client.DeleteItem(ctx, in.input.item, in.input.fromDisk)
	}); terr != nil {
		return nil, terr
	}
	return nil, nil
}

// downloadsMutation resolves the integrations with the "interact" action
// (the permission gate lives in the resolver, like the upstream middleware).
func (s *service) downloadsMutation(ctx context.Context, input json.RawMessage, withItem, withFromDisk bool) (*mutationInput, *trpc.Error) {
	decoded, terr := s.decodeMutationInput(input, withItem, withFromDisk)
	if terr != nil {
		return nil, terr
	}
	rows, terr := s.resolve(ctx, ResolveRequest{
		Action:         "interact",
		WidgetKind:     WidgetKindDownloads,
		IntegrationIDs: decoded.integrationIDs,
	})
	if terr != nil {
		return nil, terr
	}
	return &mutationInput{rows: rows, input: decoded}, nil
}

type mutationInput struct {
	rows  []Integration
	input downloadsMutationInput
}
