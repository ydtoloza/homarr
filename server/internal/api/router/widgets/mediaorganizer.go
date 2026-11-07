package widgets

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
)

// Go port of the mediaOrganizer widget router
// (packages/api/src/router/widgets/media-organizer.ts:9-47).
//
//	widget.mediaOrganizer.getData (query, publicProcedure +
//	createManyWidgetIntegrationMiddleware("query", "mediaMissing")):
//	  input: { integrationIds: string[], pageSize?: 1..50 (default 10) }
//	  output: one entry per integration, in the requested order:
//	    success: { integrationId, integrationName, missing, missingCount,
//	               queued, queuedCount }              (error: undefined dropped)
//	    failure: { integrationId, integrationName, missing: [], missingCount: 0,
//	               queued: [], queuedCount: 0,
//	               error: "INTEGRATION_REQUEST_FAILED" }
//	  with settleIntegrationQueries(..., { throwOnAllFailures: true }): when
//	  EVERY integration failed the query throws INTERNAL_SERVER_ERROR
//	  "All integration queries failed"
//	  (packages/api/src/settle-integrations.ts:52-66).
//
// The data shape comes from the media-organizer request handler
// (packages/request-handler/src/media-organizer.ts:14-34): getMissingAsync +
// getMediaQueueAsync in parallel, flattened to
// { missing, missingCount, queued, queuedCount }, cached for 60s.
type mediaOrganizerData struct {
	Missing      []mediaorganizer.MissingItem `json:"missing"`
	MissingCount int64                        `json:"missingCount"`
	Queued       []mediaorganizer.QueuedItem  `json:"queued"`
	QueuedCount  int64                        `json:"queuedCount"`
}

type mediaOrganizerResult struct {
	IntegrationID   string                       `json:"integrationId"`
	IntegrationName string                       `json:"integrationName"`
	Missing         []mediaorganizer.MissingItem `json:"missing"`
	MissingCount    int64                        `json:"missingCount"`
	Queued          []mediaorganizer.QueuedItem  `json:"queued"`
	QueuedCount     int64                        `json:"queuedCount"`
	// Error is "INTEGRATION_REQUEST_FAILED" on failure and absent on success
	// (upstream spreads `error: undefined`, which superjson drops).
	Error *string `json:"error,omitempty"`
}

func (s *service) mediaOrganizerGetData(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	// The middleware input parse runs first: integrationIds must be an array
	// of strings (zod z.object({integrationIds: z.array(z.string())})).
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
		WidgetKind:     WidgetKindMediaMissing,
		IntegrationIDs: integrationIDs,
	})
	if terr != nil {
		return nil, terr
	}

	// The procedure-level zod input (pageSize 1..50 default 10) parses AFTER
	// the middleware resolved the integrations — a bad pageSize with unknown
	// ids answers NOT_FOUND upstream.
	pageSize := mediaorganizer.DefaultPageSize
	if d.Has("pageSize") {
		v, ok := decoderNumber(d, "pageSize")
		if !ok {
			return nil, trpc.NewError(trpc.CodeBadRequest, `expected a number for input key "pageSize"`)
		}
		if v < 1 || v > 50 {
			return nil, trpc.NewError(trpc.CodeBadRequest, `input key "pageSize" must be between 1 and 50`)
		}
		// zod z.number() accepts fractions; the clients page with the
		// truncated value (JS slice semantics).
		pageSize = int(v)
	}

	results := make([]mediaOrganizerResult, len(rows))
	runBounded(ctx, s.opts.parallelism, len(rows), func(idx int) {
		row := rows[idx]
		data, err := s.mediaOrganizerDataFor(ctx, row, pageSize)
		if err != nil {
			results[idx] = mediaOrganizerResult{
				IntegrationID:   row.ID,
				IntegrationName: row.Name,
				Missing:         []mediaorganizer.MissingItem{},
				MissingCount:    0,
				Queued:          []mediaorganizer.QueuedItem{},
				QueuedCount:     0,
				Error:           strPtr(PublicIntegrationError),
			}
			return
		}
		results[idx] = mediaOrganizerResult{
			IntegrationID:   row.ID,
			IntegrationName: row.Name,
			Missing:         data.Missing,
			MissingCount:    data.MissingCount,
			Queued:          data.Queued,
			QueuedCount:     data.QueuedCount,
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

// mediaOrganizerDataFor builds (or reads from cache) the flattened
// missing/queue view of one integration (request-handler media-organizer.ts
// requestAsync: both calls in parallel).
func (s *service) mediaOrganizerDataFor(ctx context.Context, row Integration, pageSize int) (mediaOrganizerData, error) {
	key := row.ID + "\x00" + inputHash(map[string]any{"pageSize": pageSize})
	data, _, err := s.mediaOrganizerCache.getOrLoad(key, func() (any, error) {
		client, err := s.opts.newMediaOrganizer(row)
		if err != nil {
			return nil, err
		}
		var (
			wg       sync.WaitGroup
			missing  mediaorganizer.MissingResult
			queued   mediaorganizer.QueueResult
			missErr  error
			queueErr error
		)
		wg.Add(2)
		go func() { defer wg.Done(); missing, missErr = client.GetMissing(ctx, pageSize) }()
		go func() { defer wg.Done(); queued, queueErr = client.GetMediaQueue(ctx, pageSize) }()
		wg.Wait()
		if missErr != nil {
			return nil, missErr
		}
		if queueErr != nil {
			return nil, queueErr
		}
		if missing.Items == nil {
			missing.Items = []mediaorganizer.MissingItem{}
		}
		if queued.Items == nil {
			queued.Items = []mediaorganizer.QueuedItem{}
		}
		return mediaOrganizerData{
			Missing:      missing.Items,
			MissingCount: missing.TotalCount,
			Queued:       queued.Items,
			QueuedCount:  queued.TotalCount,
		}, nil
	})
	if err != nil {
		return mediaOrganizerData{}, err
	}
	return data.(mediaOrganizerData), nil
}
