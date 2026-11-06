// Package trpc implements the subset of the tRPC v11 wire protocol that the
// Homarr frontend (and its @trpc/client 11.18.0 links) speaks over HTTP, so
// the Go backend can replace the Next.js tRPC route handlers unchanged. The
// exact protocol contract, with upstream file:line citations, lives in
// server/internal/api/PROTOCOL.md.
//
// What is implemented:
//   - GET single (?input=) and GET batch (?batch=1&input={"0":...})
//   - POST single (body is the input) and POST batch (body keyed by index)
//   - the httpBatchStreamLink response mode (trpc-accept: application/jsonl):
//     a single JSONL "head" line with every op response, which the official
//     jsonlStreamConsumer accepts (M1 has no async/iterable values, so no
//     promise-chunk lines are needed)
//   - classic JSON responses (single envelope or batch array) for httpLink
//   - tRPC 11 error envelopes and the code -> HTTP status table
//
// Auth seam: upstream builds a per-request context with the session
// (packages/api/src/trpc.ts:53-71) and the route handler resolves the session
// / API key before tRPC runs (apps/nextjs/src/app/api/trpc/[trpc]/route.ts).
// In homarr-go that will be a middleware storing the caller in the request
// context; procedures read it from the ctx passed to them. Not built yet.
package trpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// basePath matches trpcPath in packages/api/src/shared.ts:49 ("/api/trpc"),
// the URL the frontend addresses the API through.
const basePath = "/api/trpc"

// Procedure handles one tRPC procedure. input is the raw JSON value after the
// superjson envelope has been removed (nil input arrives as JSON "null").
// The returned value must be JSON-serializable; it is re-wrapped on the wire
// by this package. A *Error return becomes the tRPC error envelope.
//
// Queries and mutations are not distinguished at the wire level (tRPC v11
// HTTP requests carry no operation type; methodOverride works because of
// this), so the same Procedure type serves both.
type Procedure func(ctx context.Context, input json.RawMessage) (any, *Error)

// Router dispatches /api/trpc calls to registered procedures.
type Router struct {
	mu    sync.RWMutex
	procs map[string]Procedure

	// MaxBatchSize rejects ?batch=1 calls with more operations. Upstream
	// enforces its own option maxBatchSize with the same BAD_REQUEST message
	// (http/contentType.ts:88-97); homarr-go enables the guard by default.
	MaxBatchSize int
	// MaxBodyBytes caps the POST body size via http.MaxBytesReader; exceeding
	// it maps to PAYLOAD_TOO_LARGE.
	MaxBodyBytes int64
}

// New creates an empty Router with the default limits applied.
func New() *Router {
	return &Router{
		procs:        make(map[string]Procedure),
		MaxBatchSize: 100,
		MaxBodyBytes: 10 << 20, // 10 MiB
	}
}

// Register registers a procedure under its full dotted path, e.g.
// "board.getByNames" (routers nested as in packages/api/src/root.ts).
// Register panics on duplicates or empty names: procedure wiring happens at
// startup and a collision is a programming error.
func (r *Router) Register(name string, proc Procedure) {
	if name == "" {
		panic("trpc: procedure name must not be empty")
	}
	if proc == nil {
		panic("trpc: procedure " + name + " must not be nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.procs[name]; dup {
		panic("trpc: procedure " + name + " already registered")
	}
	r.procs[name] = proc
}

// Handler returns the http.Handler to mount at /api/trpc/* (chi:
// r.Handle("/api/trpc/*", router.Handler())).
func (r *Router) Handler() http.Handler {
	return http.HandlerFunc(r.ServeHTTP)
}

// lookup returns the procedure at path, or nil.
func (r *Router) lookup(path string) Procedure {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.procs[path]
}

// opOutcome is the result of executing one operation of a call. path is the
// operation's procedure path; upstream embeds it in every per-op error shape.
type opOutcome struct {
	err  *Error // nil on success
	data any    // procedure output on success
	path string
}

// ServeHTTP implements the tRPC v11 HTTP protocol. The control flow mirrors
// resolveResponse (packages/server/src/unstable-core-do-not-import/http/resolveResponse.ts)
// and getRequestInfo (http/contentType.ts) of tRPC 11.18.0.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// HEAD is a warmup no-op (resolveResponse.ts:227-232).
	if req.Method == http.MethodHead {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// CORS preflight, mirroring the OPTIONS handler of the upstream route
	// (apps/nextjs/src/app/api/trpc/[trpc]/route.ts:27-33).
	if req.Method == http.MethodOptions {
		setCORS(w)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Every tRPC response carries the CORS headers added by the upstream
	// route handler plus the vary header set by resolveResponse.ts:222.
	setCORS(w)
	w.Header().Set("Vary", "trpc-accept, accept")

	// The x-trpc-source header (values: "nextjs-react (json)",
	// "nextjs-react (form-data)", "fetch", "rsc"; packages/api/src/shared.ts:11-24,
	// rsc-context.ts:10) is informational upstream — it is only logged.
	// Accept it silently, exactly like the server does.

	path := strings.TrimPrefix(req.URL.Path, basePath)
	path = strings.TrimPrefix(path, "/")

	// Batch calls are flagged by the ?batch=1 query parameter
	// (contentType.ts:84), not by the commas in the path.
	isBatch := req.URL.Query().Get("batch") == "1"
	// Streaming calls are detected on trpc-accept (or accept) equal to
	// "application/jsonl" (contentType.ts:8-18, resolveResponse.ts:305).
	stream := acceptsJSONL(req)

	paths := []string{path}
	if isBatch {
		paths = strings.Split(path, ",")
		if len(paths) > r.MaxBatchSize {
			writeFatal(w, NewError(CodeBadRequest, "Batch call exceeds maximum size"))
			return
		}
	}
	// A stream call must be a batch (httpBatchStreamLink always batches, even
	// a single op travels as a batch of 1); resolveResponse.ts:320-325.
	if stream && !isBatch {
		writeFatal(w, NewError(CodeBadRequest, "Streaming requests must be batched (you can do a batch of 1)"))
		return
	}

	// Content-type dispatch for bodies (contentType.ts:296-313): JSON is
	// handled here; FormData/octet-stream uploads are a documented M1 gap.
	// GET falls back to JSON so plain URLs keep working (contentType.ts:302-305).
	if req.Method != http.MethodGet {
		ct := req.Header.Get("Content-Type")
		switch {
		case ct == "":
			writeFatal(w, NewError(CodeUnsupportedMediaType, "Missing content-type header"))
			return
		case strings.HasPrefix(ct, "application/json"):
			// ok
		case strings.HasPrefix(ct, "multipart/form-data"),
			strings.HasPrefix(ct, "application/octet-stream"):
			// Upstream hands the raw FormData/body to the procedure
			// (contentType.ts:214-288); homarr-go does not implement uploads yet.
			writeFatal(w, NewError(CodeUnsupportedMediaType,
				"Binary upload content types are not implemented in homarr-go yet"))
			return
		default:
			writeFatal(w, NewError(CodeUnsupportedMediaType, `Unsupported content-type "`+ct+`"`))
			return
		}
		req.Body = http.MaxBytesReader(w, req.Body, r.MaxBodyBytes)
	}

	inputs, parseErr := r.resolveInputs(req, isBatch, len(paths))

	// M1 executes operations sequentially in op order. Upstream runs them
	// concurrently (rpcCalls map); a future iteration can fan out with
	// errgroup while keeping the same output contract.
	outcomes := make([]opOutcome, len(paths))
	for i, p := range paths {
		// Order mirrors the per-op block of resolveResponse
		// (resolveResponse.ts:333-408): lookup first, then method pairing,
		// and only then the (lazy, per-op) input parsing.
		proc := r.lookup(p)
		switch {
		case proc == nil:
			outcomes[i] = opOutcome{path: p,
				err: NewError(CodeNotFound, `No procedure found on path "`+p+`"`)}
		case req.Method != http.MethodGet && req.Method != http.MethodPost:
			// Upstream pairs method with procedure type
			// (resolveResponse.ts:55-68, 348-353); homarr mounts the handler
			// with allowMethodOverride: true (route.ts), which additionally
			// allows POST for queries. homarr-go has no procedure types yet:
			// every procedure accepts GET and POST alike.
			outcomes[i] = opOutcome{path: p, err: NewError(CodeMethodNotSupported,
				`Unsupported `+req.Method+`-request at path "`+p+`"`)}
		case parseErr != nil:
			// Shared input parsing failure: upstream reports it per operation
			// through getRawInput (contentType.ts:53-61), so every op of the
			// batch gets its own BAD_REQUEST envelope.
			outcomes[i] = opOutcome{path: p, err: NewError(parseErr.Code, parseErr.Message)}
		default:
			outcomes[i] = invoke(req.Context(), proc, p, inputs[i])
			outcomes[i].path = p
		}
	}

	if stream {
		writeStream(w, outcomes)
		return
	}
	writePlain(w, isBatch, outcomes)
}

// invoke unwraps the input, runs the procedure and isolates panics per
// operation so one broken handler cannot take a batch (or the server) down.
func invoke(ctx context.Context, proc Procedure, path string, input json.RawMessage) (out opOutcome) {
	unwrapped, err := UnwrapInput(input)
	if err != nil {
		return opOutcome{err: NewError(CodeBadRequest, "Failed to parse input: "+err.Error())}
	}
	defer func() {
		if rec := recover(); rec != nil {
			// Equivalent of getTRPCErrorFromUnknown wrapping an unknown cause.
			if terr, ok := rec.(*Error); ok {
				out = opOutcome{err: terr}
				return
			}
			err := NewError(CodeInternalServerError, "internal server error")
			if msg, ok := rec.(string); ok && msg != "" {
				err.Message = msg
			} else if e, ok := rec.(error); ok && e != nil {
				err.Message = e.Error()
			}
			out = opOutcome{err: err}
		}
	}()
	data, terr := proc(ctx, unwrapped)
	if terr != nil {
		return opOutcome{err: terr}
	}
	return opOutcome{data: data}
}

// resolveInputs extracts the per-operation inputs, mirroring
// jsonContentTypeHandler.parse (contentType.ts:100-137):
//   - GET: the raw "input" query param; absent means "no input".
//   - POST: the body; single calls carry the input directly, batch calls an
//     object keyed by op index ("0", "1", ...).
//
// A failure is returned as parseErr and applied to every operation (see the
// loop in ServeHTTP); missing indices inside a valid batch record are just
// absent inputs, like upstream.
func (r *Router) resolveInputs(req *http.Request, isBatch bool, n int) ([]json.RawMessage, *Error) {
	inputs := make([]json.RawMessage, n)

	if req.Method == http.MethodGet {
		raw := req.URL.Query().Get("input")
		if raw == "" {
			return inputs, nil // absent input -> "no input" (undefined upstream)
		}
		var parsed json.RawMessage
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			return inputs, NewError(CodeBadRequest, "Failed to parse input: "+err.Error())
		}
		if !isBatch {
			inputs[0] = parsed
			return inputs, nil
		}
		return splitBatchRecord(parsed, inputs)
	}

	// POST: body already validated as application/json in ServeHTTP.
	body, err := io.ReadAll(req.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return inputs, NewError(CodePayloadTooLarge, "Request body exceeds the configured limit")
		}
		return inputs, NewError(CodeBadRequest, "Failed to read request body: "+err.Error())
	}
	var parsed json.RawMessage
	if err := json.Unmarshal(body, &parsed); err != nil {
		return inputs, NewError(CodeBadRequest, "Failed to parse input: "+err.Error())
	}
	if !isBatch {
		inputs[0] = parsed
		return inputs, nil
	}
	return splitBatchRecord(parsed, inputs)
}

// splitBatchRecord validates that a batch input is a JSON object keyed by op
// index and fills inputs accordingly (contentType.ts:121-134).
func splitBatchRecord(parsed json.RawMessage, inputs []json.RawMessage) ([]json.RawMessage, *Error) {
	var record map[string]json.RawMessage
	if err := json.Unmarshal(parsed, &record); err != nil {
		return inputs, NewError(CodeBadRequest, `"input" needs to be an object when doing a batch call`)
	}
	for i := range inputs {
		inputs[i] = record[strconv.Itoa(i)]
	}
	return inputs, nil
}

// acceptsJSONL reports whether the client asked for the streaming response
// mode: a "trpc-accept: application/jsonl" header, or "application/jsonl"
// listed in the standard accept header (getAcceptHeader, contentType.ts:8-18).
func acceptsJSONL(req *http.Request) bool {
	if req.Header.Get("trpc-accept") == "application/jsonl" {
		return true
	}
	for _, part := range strings.Split(req.Header.Get("Accept"), ",") {
		if strings.TrimSpace(part) == "application/jsonl" {
			return true
		}
	}
	return false
}

// setCORS mirrors setCorsHeaders of the upstream route handler
// (apps/nextjs/src/app/api/trpc/[trpc]/route.ts:17-22).
func setCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Request-Method", "*")
	w.Header().Set("Access-Control-Allow-Methods", "OPTIONS, GET, POST")
	w.Header().Set("Access-Control-Allow-Headers", "*")
}

// errorShape is the untransformed tRPC error envelope payload
// (getErrorShape.ts:21-35).
type errorShape struct {
	Message string         `json:"message"`
	Code    int            `json:"code"`
	Data    map[string]any `json:"data"`
}

// writeFatal answers with a single error envelope for failures that happen
// before any operation can run (bad call shape, unsupported media type, ...).
// This is the catch branch of resolveResponse (resolveResponse.ts:745-774).
// It intentionally answers classic JSON even to stream-accepted requests —
// the client's !res.ok branch parses the body as plain JSON and propagates
// the error to every op (httpBatchStreamLink.ts:103-123).
func writeFatal(w http.ResponseWriter, terr *Error) {
	body, err := marshalJSON(errorEnvelope{Error: envelopeError{JSON: terr.shape("")}})
	if err != nil {
		body = []byte(`{"error":{"json":{"message":"internal server error","code":-32603,"data":{"code":"INTERNAL_SERVER_ERROR","httpStatus":500}}}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(terr.HTTPStatus())
	_, _ = w.Write(body)
}

// envelope types for the classic (non-streaming) JSON responses. The
// transformer wrapping matches transformTRPCResponseItem (transformer.ts:87-108):
// result.data and the whole error shape are wrapped by the output transformer,
// which for superjson means {"json": ...} (meta omitted for plain values).
type superjsonValue struct {
	JSON any `json:"json"`
}

type envelopeResult struct {
	Data any `json:"data"` // superjsonValue or a literal {} for undefined
}

type successEnvelope struct {
	Result envelopeResult `json:"result"`
}

type envelopeError struct {
	JSON errorShape `json:"json"`
}

type errorEnvelope struct {
	Error envelopeError `json:"error"`
}

// responseStatus computes the HTTP status of a classic response:
// 200 when every op succeeded, the common error status when all errors share
// it, and 207 (Multi-Status) when statuses differ — getHTTPStatusCode
// (getHTTPStatusCode.ts:71-94).
func responseStatus(outcomes []opOutcome) int {
	first := -1
	uniform := true
	for _, o := range outcomes {
		s := http.StatusOK
		if o.err != nil {
			s = o.err.HTTPStatus()
		}
		if first == -1 {
			first = s
		} else if s != first {
			uniform = false
		}
	}
	if !uniform {
		return http.StatusMultiStatus
	}
	return first
}

// writePlain writes the classic JSON response: a single envelope for
// non-batch calls (httpLink) or an array of envelopes for batches
// (httpBatchLink — always an array, even for a batch of 1, matching the
// `!info.isBatchCall` split in resolveResponse.ts:411,573,673), with the
// status derived from the op errors.
func writePlain(w http.ResponseWriter, isBatch bool, outcomes []opOutcome) {
	bodies := make([][]byte, len(outcomes))
	for i := range outcomes {
		body, err := marshalPlainEnvelope(outcomes[i])
		if err != nil {
			// Unserializable procedure output becomes an INTERNAL_SERVER_ERROR
			// envelope (resolveResponse fatal catch, resolveResponse.ts:745-774).
			outcomes[i] = opOutcome{path: outcomes[i].path, err: NewError(CodeInternalServerError,
				"Failed to serialize procedure output")}
			body, _ = marshalPlainEnvelope(outcomes[i])
		}
		bodies[i] = body
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(responseStatus(outcomes))

	if !isBatch {
		_, _ = w.Write(bodies[0])
		return
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, body := range bodies {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(body)
	}
	buf.WriteByte(']')
	_, _ = w.Write(buf.Bytes())
}

// marshalPlainEnvelope builds one element of a classic JSON response.
func marshalPlainEnvelope(o opOutcome) ([]byte, error) {
	if o.err != nil {
		return marshalJSON(errorEnvelope{Error: envelopeError{JSON: o.err.shape(o.path)}})
	}
	var data any
	if o.data == nil {
		// nil mirrors superjson serializing `undefined`: {"json": undefined}
		// collapses to {} once JSON.stringify drops the undefined member.
		data = json.RawMessage("{}")
	} else {
		data = superjsonValue{JSON: o.data}
	}
	return marshalJSON(successEnvelope{Result: envelopeResult{Data: data}})
}

// writeStream writes the httpBatchStreamLink response mode: HTTP 200 (even
// when ops error — errors ride inside the stream, resolveResponse.ts:577-584,
// 667-670) with content-type application/json and a JSONL body.
//
// The body is one "head" line where each op is keyed by its index and encoded
// as an EncodedValue tuple [[payload]] with no promise chunks
// (stream/jsonl.ts:234-266). The official producer emits per-op promise chunk
// lines because results may resolve late; homarr-go executes sequentially, so
// the head-only encoding — which jsonlStreamConsumer.decode handles natively
// (jsonl.ts:588-602) — is equivalent and needs no buffering past one line.
// The whole line is superjson-wrapped (the producer applies the output
// transformer per line, jsonl.ts:286-298), hence the {"json": ...} root.
func writeStream(w http.ResponseWriter, outcomes []opOutcome) {
	var buf bytes.Buffer
	buf.WriteString(`{"json":{`)
	for i := range outcomes {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteByte('"')
		buf.WriteString(strconv.Itoa(i))
		buf.WriteString(`":[[`)
		entry, err := marshalStreamEntry(outcomes[i])
		if err != nil {
			outcomes[i] = opOutcome{path: outcomes[i].path, err: NewError(CodeInternalServerError,
				"Failed to serialize procedure output")}
			entry, _ = marshalStreamEntry(outcomes[i])
		}
		buf.Write(entry)
		buf.WriteString(`]]`)
	}
	buf.WriteString("}}\n")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// marshalStreamEntry builds the EncodedValue payload of one op inside the
// head line. Success data is NOT wrapped in {"json": ...} here: the per-line
// superjson wrapping done by writeStream covers it, exactly like the official
// producer (the line-level transformer walks the whole head).
func marshalStreamEntry(o opOutcome) ([]byte, error) {
	if o.err != nil {
		return marshalJSON(struct {
			Error errorShape `json:"error"`
		}{Error: o.err.shape(o.path)})
	}
	if o.data == nil {
		return []byte(`{"result":{}}`), nil
	}
	payload, err := marshalJSON(o.data)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteString(`{"result":{"data":`)
	buf.Write(payload)
	buf.WriteString(`}}`)
	return buf.Bytes(), nil
}
