package widgets

import (
	"context"
	"encoding/json"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/integrations/network"
)

// Go port of the dnsHole widget router
// (packages/api/src/router/widgets/dns-hole.ts:28-97).
//
//	widget.dnsHole.summary (query, publicProcedure +
//	createManyWidgetIntegrationMiddleware("query", "dnsHoleSummary")):
//	  input: { integrationIds: string[] } (middleware only)
//	  output per integration:
//	    success: { integrationId, integrationName,
//	               integration: { id, name, kind, updatedAt },
//	               summary: DnsHoleSummary }
//	    failure: { integrationId, integrationName,
//	               integration: { id, name, kind },
//	               summary: null, error: "INTEGRATION_REQUEST_FAILED" }
//	  (throwOnAllFailures: true -> every integration failed throws
//	  INTERNAL_SERVER_ERROR "All integration queries failed").
//
//	widget.dnsHole.enable (mutation, protectedProcedure +
//	createOneWidgetIntegrationMiddleware("interact", "dnsHoleControls")):
//	  input: { integrationId: string } (ONE integration); runs enableAsync
//	  then invalidates the cache (dns-hole.ts:72-75: the invalidation happens
//	  AFTER the client call, so a failed mutation does NOT invalidate).
//
//	widget.dnsHole.disable (mutation, same gates):
//	  input: { integrationId: string, duration?: number (seconds) }; runs
//	  disableAsync(duration) then invalidates.
//
// Mutation failures throw the raw client error: the pi-hole v5/adguard plain
// messages ("Failed to enable PiHole for <name> (<id>): <statusText>") and
// the pi-hole v6 "Response did not indicate success" travel through
// getTRPCErrorFromUnknown as INTERNAL_SERVER_ERROR verbatim.
//
// The summary comes from the dns-hole request handler
// (packages/request-handler/src/dns-hole.ts:7-17, cacheNamespace
// "dns-hole:summary") with the default request-handler TTL of 10s; the
// updatedAt timestamp is the cache entry time (upstream serializes the Date
// through superjson; the port emits the ISO string convention of ADMIN.md §4).
type dnsHoleSummaryResult struct {
	IntegrationID   string                  `json:"integrationId"`
	IntegrationName string                  `json:"integrationName"`
	Integration     dnsHoleInfo             `json:"integration"`
	Summary         *network.DnsHoleSummary `json:"summary"`
	Error           *string                 `json:"error,omitempty"`
}

type dnsHoleInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

func (s *service) dnsHoleSummary(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
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
		WidgetKind:     WidgetKindDNSHoleSummary,
		IntegrationIDs: integrationIDs,
	})
	if terr != nil {
		return nil, terr
	}

	results := make([]dnsHoleSummaryResult, len(rows))
	runBounded(ctx, s.opts.parallelism, len(rows), func(idx int) {
		row := rows[idx]
		summary, timestamp, err := s.dnsHoleSummaryFor(ctx, row)
		if err != nil {
			results[idx] = dnsHoleSummaryResult{
				IntegrationID:   row.ID,
				IntegrationName: row.Name,
				Integration:     dnsHoleInfo{ID: row.ID, Name: row.Name, Kind: row.Kind},
				Summary:         nil,
				Error:           strPtr(PublicIntegrationError),
			}
			return
		}
		results[idx] = dnsHoleSummaryResult{
			IntegrationID:   row.ID,
			IntegrationName: row.Name,
			Integration: dnsHoleInfo{
				ID:        row.ID,
				Name:      row.Name,
				Kind:      row.Kind,
				UpdatedAt: timestamp,
			},
			Summary: &summary,
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

// dnsHoleSummaryFor builds (or reads from cache) the summary of one
// integration (dns-hole.ts requestAsync).
func (s *service) dnsHoleSummaryFor(ctx context.Context, row Integration) (network.DnsHoleSummary, string, error) {
	key := row.ID + "\x00" + inputHash(map[string]any{})
	data, ts, err := s.dnsHoleCache.getOrLoad(key, func() (any, error) {
		client, err := s.opts.newNetwork(ctx, row)
		if err != nil {
			return nil, err
		}
		return client.GetSummary(ctx)
	})
	if err != nil {
		return network.DnsHoleSummary{}, "", err
	}
	return data.(network.DnsHoleSummary), isoMillis(ts), nil
}

// --- mutations ---

func (s *service) dnsHoleEnable(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	rows, terr := s.dnsHoleMutation(ctx, input)
	if terr != nil {
		return nil, terr
	}
	row := rows[0]
	client, err := s.opts.newNetwork(ctx, row)
	if err != nil {
		return nil, mutationClientError(err)
	}
	if err := client.Enable(ctx); err != nil {
		return nil, mutationClientError(err)
	}
	// The invalidation runs AFTER the client call succeeded (dns-hole.ts
	// :73-75) — a failed mutation does NOT invalidate.
	s.dnsHoleCache.invalidate([]string{row.ID})
	return nil, nil
}

func (s *service) dnsHoleDisable(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	d, terr := newDecoder(input)
	if terr != nil {
		return nil, terr
	}
	integrationID, terr := d.Str("integrationId")
	if terr != nil {
		return nil, terr
	}
	// duration is optional (z.number().optional()): absent/null skip, a
	// present non-number is a BAD_REQUEST.
	var duration *network.DisableDurationSeconds
	if d.Has("duration") {
		v, ok := decoderNumber(d, "duration")
		if !ok {
			return nil, trpc.NewError(trpc.CodeBadRequest, `expected a number for input key "duration"`)
		}
		duration = network.Ptr(v)
	}
	rows, terr := s.resolve(ctx, ResolveRequest{
		Action:         "interact",
		WidgetKind:     WidgetKindDNSHoleControls,
		IntegrationIDs: []string{integrationID},
		Cardinality:    CardinalityOne,
	})
	if terr != nil {
		return nil, terr
	}
	if len(rows) == 0 {
		return nil, nil
	}
	row := rows[0]
	client, err := s.opts.newNetwork(ctx, row)
	if err != nil {
		return nil, mutationClientError(err)
	}
	if err := client.Disable(ctx, duration); err != nil {
		return nil, mutationClientError(err)
	}
	s.dnsHoleCache.invalidate([]string{row.ID})
	return nil, nil
}

// dnsHoleMutation parses the enable input (integrationId only) and resolves
// the ONE integration with the interact action (the permission gate lives in
// the resolver, like the upstream middleware).
func (s *service) dnsHoleMutation(ctx context.Context, input json.RawMessage) ([]Integration, *trpc.Error) {
	d, terr := newDecoder(input)
	if terr != nil {
		return nil, terr
	}
	integrationID, terr := d.Str("integrationId")
	if terr != nil {
		return nil, terr
	}
	return s.resolve(ctx, ResolveRequest{
		Action:         "interact",
		WidgetKind:     WidgetKindDNSHoleControls,
		IntegrationIDs: []string{integrationID},
		Cardinality:    CardinalityOne,
	})
}

// mutationClientError maps a client failure of a mutation to INTERNAL_SERVER
// _ERROR carrying the raw message (getTRPCErrorFromUnknown of the thrown
// upstream error: IntegrationError subclasses and plain Errors both keep
// their message).
func mutationClientError(err error) *trpc.Error {
	return trpc.NewError(trpc.CodeInternalServerError, err.Error())
}
