package widgets

import (
	"context"
	"encoding/json"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaserver"
)

// Go port of the mediaServer widget router
// (packages/api/src/router/widgets/media-server.ts:9-47).
//
//	widget.mediaServer.getCurrentStreams (query, publicProcedure +
//	createManyWidgetIntegrationMiddleware("query", "mediaServer")):
//	  input: { integrationIds: string[], showOnlyPlaying: boolean }
//	  output: one entry per integration, in the requested order:
//	    success: { integrationId, integrationName, integrationKind,
//	               sessions: StreamSession[] }      (error: undefined dropped)
//	    failure: { integrationId, integrationName, integrationKind,
//	               sessions: [], error: "INTEGRATION_REQUEST_FAILED" }
//	  with settleIntegrationQueries(..., { throwOnAllFailures: true }): when
//	  EVERY integration failed the query throws INTERNAL_SERVER_ERROR
//	  "All integration queries failed".
//
// The data comes from the media-server request handler
// (packages/request-handler/src/media-server.ts:7-19,
// cacheNamespace "media-server:sessions") with the default request-handler
// TTL of 10s (packages/request-handler/src/lib/request-handler.ts:34
// DEFAULT_TTL_MS). There is no websocket or bus event for these sessions
// upstream — the frontend polls every 10s
// (packages/widgets/src/refetch-intervals.ts:18).
type mediaServerResult struct {
	IntegrationID   string                      `json:"integrationId"`
	IntegrationName string                      `json:"integrationName"`
	IntegrationKind string                      `json:"integrationKind"`
	Sessions        []mediaserver.StreamSession `json:"sessions"`
	// Error is "INTEGRATION_REQUEST_FAILED" on failure and absent on success
	// (upstream spreads `error: undefined`, which superjson drops).
	Error *string `json:"error,omitempty"`
}

func (s *service) mediaServerGetCurrentStreams(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
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
		WidgetKind:     WidgetKindMediaServer,
		IntegrationIDs: integrationIDs,
	})
	if terr != nil {
		return nil, terr
	}

	// The procedure-level zod input (showOnlyPlaying, REQUIRED boolean)
	// parses AFTER the middleware resolved the integrations — a missing
	// showOnlyPlaying with unknown ids answers NOT_FOUND upstream, like the
	// media-organizer pageSize.
	showOnlyPlaying, terr := d.Bool("showOnlyPlaying")
	if terr != nil {
		return nil, terr
	}

	results := make([]mediaServerResult, len(rows))
	runBounded(ctx, s.opts.parallelism, len(rows), func(idx int) {
		row := rows[idx]
		sessions, err := s.mediaServerSessionsFor(ctx, row, showOnlyPlaying)
		if err != nil {
			results[idx] = mediaServerResult{
				IntegrationID:   row.ID,
				IntegrationName: row.Name,
				IntegrationKind: row.Kind,
				Sessions:        []mediaserver.StreamSession{},
				Error:           strPtr(PublicIntegrationError),
			}
			return
		}
		results[idx] = mediaServerResult{
			IntegrationID:   row.ID,
			IntegrationName: row.Name,
			IntegrationKind: row.Kind,
			Sessions:        sessions,
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

// mediaServerSessionsFor builds (or reads from cache) the active streams of
// one integration (request-handler media-server.ts requestAsync).
func (s *service) mediaServerSessionsFor(ctx context.Context, row Integration, showOnlyPlaying bool) ([]mediaserver.StreamSession, error) {
	key := row.ID + "\x00" + inputHash(map[string]any{"showOnlyPlaying": showOnlyPlaying})
	data, _, err := s.mediaServerCache.getOrLoad(key, func() (any, error) {
		client, err := s.opts.newMediaServer(row)
		if err != nil {
			return nil, err
		}
		sessions, err := client.GetCurrentSessions(ctx, mediaserver.CurrentSessionsInput{
			ShowOnlyPlaying: showOnlyPlaying,
		})
		if err != nil {
			return nil, err
		}
		if sessions == nil {
			sessions = []mediaserver.StreamSession{}
		}
		return sessions, nil
	})
	if err != nil {
		return nil, err
	}
	return data.([]mediaserver.StreamSession), nil
}
