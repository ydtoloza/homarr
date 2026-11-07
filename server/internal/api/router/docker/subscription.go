package docker

import (
	"context"
	"encoding/json"
	"io"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/bus"
	"github.com/ydtoloza/homarr/server/internal/integrations/docker"
	"github.com/ydtoloza/homarr/server/internal/ws"
)

// docker.subscribeLogs over the WebSocket hub — the port of the subscription
// branch of docker-router.ts:201-256.
//
// Upstream creates an observable that opens a follow log stream and emits
// every demultiplexed frame as a string data event. In homarr-go the data
// frames ride the bus (internal/ws/subscription.go), so the handler:
//
//  1. enforces the upstream gates in the upstream order (dockerMiddleware →
//     permissionRequiredProcedure("admin") → zod input → logs capability →
//     container lookup);
//  2. fetches the Initial frame: the recent `tail` lines decoded as one
//     string (the upstream observable's follow stream includes the tail as
//     its first frames; splitting tail/l between an Initial frame and a
//     tail=0 follow stream reproduces the same user-visible output);
//  3. acquires the shared pump for the topic, which publishes live frames to
//     "events.docker.logs.<endpointId>.<containerId>";
//  4. returns Subscription{Pattern: that topic, Decode: passthrough} — the
//     exact topic match is the per-subscription filter (the input's
//     endpointId/containerId ARE the topic).
//
// Error mapping mirrors upstream's observable wiring (docker-router.ts:
// 216-247): a failing stream START rejects the subscription with NOT_FOUND
// (the catch around initializeStreamAsync); the capability gate is a sync
// FORBIDDEN; after the stream is live the bus has no error channel, so
// mid-stream failures stop the pump silently (deviation documented in
// DOCKER.md section 5).

// subscribeLogs implements the ws.SubscriptionHandler for
// "docker.subscribeLogs".
func (s *service) subscribeLogs(ctx context.Context, req *ws.Request) (*ws.Subscription, error) {
	if terr := s.dockerGate(ctx); terr != nil {
		return nil, terr
	}
	// permissionRequiredProcedure extends protectedProcedure: anonymous
	// callers get UNAUTHORIZED, non-admin callers FORBIDDEN "Permission
	// denied" (packages/api/src/trpc.ts:195-207).
	if req.Session != nil {
		ctx = ctxauth.WithSession(ctx, req.Session)
	}
	caller, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequirePermission(caller, "admin"); terr != nil {
		return nil, terr
	}

	in, terr := parseLogsInput(req.Input)
	if terr != nil {
		return nil, terr
	}
	if terr := s.assertLogsCapability(in.EndpointID); terr != nil {
		return nil, terr
	}
	if err := s.findContainer(ctx, in.EndpointID, in.ID, docker.CapabilityLogs); err != nil {
		return nil, trpc.NewError(trpc.CodeNotFound, err.Error())
	}

	// Initial: the recent tail as one string (getContainerLogsAsync
	// semantics: tail, stdout, stderr, follow=false).
	initial, err := s.tailSnapshot(ctx, in.EndpointID, in.ID, in.resolvedTail())
	if err != nil {
		// Upstream: the rejection of the initialization awaits surfaces as
		// NOT_FOUND with the engine message (docker-router.ts:237-246).
		return nil, trpc.NewError(trpc.CodeNotFound, err.Error())
	}

	// Live frames: the shared pump (tail=0 + follow=true).
	if _, err := s.pumps.acquire(in.EndpointID, in.ID, in.resolvedTail()); err != nil {
		return nil, trpc.NewError(trpc.CodeNotFound, err.Error())
	}

	return &ws.Subscription{
		Initial: initial,
		Pattern: LogTopic(in.EndpointID, in.ID),
		Decode:  passthroughDecode,
	}, nil
}

// tailSnapshot fetches the last tail lines decoded as one string
// (getContainerLogsAsync, request-handler/src/docker.ts:254-268).
func (s *service) tailSnapshot(ctx context.Context, endpointID, containerID string, tail int) (string, error) {
	client, err := s.factory.ClientFor(endpointID)
	if err != nil {
		return "", err
	}
	raw, err := client.Logs(ctx, containerID, docker.LogsOptions{Tail: tail, Follow: false})
	if err != nil {
		return "", err
	}
	defer raw.Close()
	body, err := io.ReadAll(raw)
	if err != nil {
		return "", err
	}
	return docker.DecodeLogs(body), nil
}

// passthroughDecode forwards the bus payload unchanged (json.RawMessage
// zero-copy): the pump already publishes the exact JSON-string frame the
// frontend consumes.
func passthroughDecode(msg bus.Message) (any, bool) {
	return json.RawMessage(msg.Payload), true
}
