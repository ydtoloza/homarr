package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/integrations/docker"
)

// docker.logs query (docker-router.ts:170-200) and the log capability gate.

// logsInput is the zod input shared by docker.logs and docker.subscribeLogs
// (docker-router.ts:180-208): {endpointId, id, tail?} with tail 1..1000.
type logsInput struct {
	EndpointID string `json:"endpointId"`
	ID         string `json:"id"`
	Tail       *int   `json:"tail"`
}

func (i logsInput) resolvedTail() int {
	if i.Tail == nil {
		return defaultLogTail // input.tail ?? 200 (docker-router.ts:188)
	}
	return *i.Tail
}

// defaultLogTail is the upstream default tail.
const defaultLogTail = 200

// parseLogsInput validates the shared input: endpointId/id non-empty, tail
// within 1..1000 (zod: tail: z.number().min(1).max(1000).optional()).
func parseLogsInput(input json.RawMessage) (logsInput, *trpc.Error) {
	var in logsInput
	if len(input) == 0 || string(input) == "null" {
		return in, trpc.NewError(trpc.CodeBadRequest, `Input "endpointId" and "id" are required`)
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return in, trpc.NewError(trpc.CodeBadRequest, "Failed to parse input: "+err.Error())
	}
	if in.EndpointID == "" {
		return in, trpc.NewError(trpc.CodeBadRequest, `Input "endpointId" must contain at least 1 character(s)`)
	}
	if in.ID == "" {
		return in, trpc.NewError(trpc.CodeBadRequest, `Input "id" must contain at least 1 character(s)`)
	}
	if in.Tail != nil && (*in.Tail < 1 || *in.Tail > 1000) {
		return in, trpc.NewError(trpc.CodeBadRequest, `Input "tail" must be a number between 1 and 1000`)
	}
	return in, nil
}

// assertLogsCapability is assertDockerEndpointCapability(endpointId, "logs")
// (docker-router.ts:297-303): FORBIDDEN "Docker endpoint does not permit
// logs access" when the endpoint is unknown or lacks the capability.
func (s *service) assertLogsCapability(endpointID string) *trpc.Error {
	ep, ok := s.factory.Endpoint(endpointID)
	if !ok || !ep.HasCapability(docker.CapabilityLogs) {
		return trpc.NewError(trpc.CodeForbidden, "Docker endpoint does not permit logs access")
	}
	return nil
}

// findContainer ports findDockerContainerAsync
// (packages/request-handler/src/docker.ts:241-252): the endpoint must exist
// with the required capability and the container must inspect cleanly. ANY
// inspect error — including an unreachable engine — means "unknown
// container" upstream (the promise resolves null on error).
func (s *service) findContainer(ctx context.Context, endpointID, containerID string, required docker.Capability) error {
	ep, ok := s.factory.Endpoint(endpointID)
	if !ok || !ep.HasCapability(required) {
		return errContainerNotFound
	}
	client, err := s.factory.ClientFor(endpointID)
	if err != nil {
		return errContainerNotFound
	}
	cctx, cancel := context.WithTimeout(ctx, docker.DefaultTimeout)
	defer cancel()
	if err := client.Inspect(cctx, containerID); err != nil {
		return errContainerNotFound
	}
	return nil
}

var errContainerNotFound = errors.New("Container not found")

// dockerLogs is the docker.logs query: the last tail lines of the container,
// decoded. Upstream calls getContainerLogsAsync
// (packages/request-handler/src/docker.ts:254-268: tail, stdout, stderr,
// follow=false) and returns {logs: string}.
func (s *service) dockerLogs(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	if terr := s.dockerGate(ctx); terr != nil {
		return nil, terr
	}
	if terr := s.requireAdmin(ctx); terr != nil {
		return nil, terr
	}
	in, terr := parseLogsInput(input)
	if terr != nil {
		return nil, terr
	}
	if terr := s.assertLogsCapability(in.EndpointID); terr != nil {
		return nil, terr
	}
	if err := s.findContainer(ctx, in.EndpointID, in.ID, docker.CapabilityLogs); err != nil {
		return nil, trpc.NewError(trpc.CodeNotFound, err.Error())
	}

	client, err := s.factory.ClientFor(in.EndpointID)
	if err != nil {
		return nil, trpc.NewError(trpc.CodeNotFound, "Container not found")
	}
	raw, err := client.Logs(ctx, in.ID, docker.LogsOptions{Tail: in.resolvedTail(), Follow: false})
	if err != nil {
		// Upstream lets the dockerode rejection bubble: unhandled →
		// INTERNAL_SERVER_ERROR with the original message.
		return nil, trpc.NewError(trpc.CodeInternalServerError, err.Error())
	}
	defer raw.Close()
	body, err := io.ReadAll(raw)
	if err != nil {
		return nil, trpc.NewError(trpc.CodeInternalServerError, err.Error())
	}
	return map[string]any{"logs": docker.DecodeLogs(body)}, nil
}
