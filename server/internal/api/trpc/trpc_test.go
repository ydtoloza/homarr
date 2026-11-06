package trpc_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
)

// echo returns its input untouched (as raw JSON) — handy to assert the exact
// envelope the wire carries.
func echo(_ context.Context, input json.RawMessage) (any, *trpc.Error) {
	return input, nil
}

func upper(_ context.Context, input json.RawMessage) (any, *trpc.Error) {
	var s string
	if err := json.Unmarshal(input, &s); err != nil {
		return nil, trpc.NewError(trpc.CodeBadRequest, "expected a string")
	}
	return strings.ToUpper(s), nil
}

func boom(_ context.Context, _ json.RawMessage) (any, *trpc.Error) {
	panic("procedure exploded")
}

func failWith(code trpc.Code, msg string) trpc.Procedure {
	return func(_ context.Context, _ json.RawMessage) (any, *trpc.Error) {
		return nil, trpc.NewError(code, msg)
	}
}

func newTestRouter() *trpc.Router {
	r := trpc.New()
	r.Register("echo", echo)
	r.Register("upper", upper)
	r.Register("boom", boom)
	return r
}

// response is the outcome of one HTTP round trip against the handler.
type response struct {
	status int
	body   string
	header http.Header
}

func do(t *testing.T, r *trpc.Router, method, target string, body io.Reader, headers map[string]string) response {
	t.Helper()
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	req, err := http.NewRequest(method, srv.URL+target, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response{status: res.StatusCode, body: string(raw), header: res.Header}
}

// decodeHeadEntry peels the JSONL EncodedValue tuple [[entry]] and returns
// the inner payload (stream/jsonl.ts:234-266: value[0][0] is the data).
func decodeHeadEntry(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	var tuple [1][]json.RawMessage
	if err := json.Unmarshal(raw, &tuple); err != nil {
		t.Fatalf("head entry %s is not an [[entry]] tuple: %v", raw, err)
	}
	if len(tuple[0]) != 1 {
		t.Fatalf("head entry %s: want exactly one data element, got %d", raw, len(tuple[0]))
	}
	return tuple[0][0]
}

func TestSingleGETWithInput(t *testing.T) {
	r := newTestRouter()
	res := do(t, r, http.MethodGet,
		`/api/trpc/echo?input=%7B%22json%22%3A%7B%22msg%22%3A%22hi%22%7D%7D`,
		nil, nil)

	if res.status != http.StatusOK {
		t.Errorf("got status %d, want 200", res.status)
	}
	want := `{"result":{"data":{"json":{"msg":"hi"}}}}`
	if res.body != want {
		t.Errorf("body = %s, want %s", res.body, want)
	}
	if ct := res.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	// resolveResponse sets vary on every response; the route handler adds CORS.
	if v := res.header.Get("Vary"); v != "trpc-accept, accept" {
		t.Errorf("Vary = %q, want %q", v, "trpc-accept, accept")
	}
	if res.header.Get("Access-Control-Allow-Origin") != "*" {
		t.Error("missing Access-Control-Allow-Origin from upstream setCorsHeaders")
	}
}

func TestSingleGETNoInputPassesNull(t *testing.T) {
	r := newTestRouter()
	res := do(t, r, http.MethodGet, "/api/trpc/echo", nil, nil)
	if res.status != http.StatusOK {
		t.Fatalf("got status %d, want 200", res.status)
	}
	// Absent input is "no input" upstream; the Go handler sees an explicit null.
	want := `{"result":{"data":{"json":null}}}`
	if res.body != want {
		t.Errorf("body = %s, want %s", res.body, want)
	}
}

func TestSinglePOSTDirectBody(t *testing.T) {
	// httpLink (packages/api/src/client.ts fetchApi) posts the serialized
	// input directly, with no batch parameter.
	r := newTestRouter()
	res := do(t, r, http.MethodPost, "/api/trpc/upper",
		strings.NewReader(`{"json":"homarr"}`),
		map[string]string{
			"Content-Type":  "application/json",
			"X-Trpc-Source": "fetch",
		})
	if res.status != http.StatusOK {
		t.Fatalf("got status %d, want 200", res.status)
	}
	want := `{"result":{"data":{"json":"HOMARR"}}}`
	if res.body != want {
		t.Errorf("body = %s, want %s", res.body, want)
	}
}

func TestSinglePOSTMethodOverrideOnQuery(t *testing.T) {
	// widget.releases.getLatest uses httpBatchStreamLink with
	// methodOverride: "POST" (trpc.tsx:222-229); the server must execute the
	// query from a POST body. The stream link still batches (batch of 1) and
	// requests a stream, so the answer is a head line keyed "0".
	r := newTestRouter()
	res := do(t, r, http.MethodPost, "/api/trpc/upper?batch=1",
		strings.NewReader(`{"0":{"json":"board"}}`),
		map[string]string{
			"Content-Type": "application/json",
			"trpc-accept":  "application/jsonl",
		})
	if res.status != http.StatusOK {
		t.Fatalf("got status %d, want 200", res.status)
	}
	want := "{\"json\":{\"0\":[[{\"result\":{\"data\":\"BOARD\"}}]]}}\n"
	if res.body != want {
		t.Errorf("body = %s, want %s", res.body, want)
	}
}

func TestBatchOfOneClassicIsAnArray(t *testing.T) {
	// A ?batch=1 call without the stream header (e.g. methodOverride POST
	// answered by a non-streaming server) gets the classic ARRAY response,
	// even with a single op (resolveResponse.ts:411 vs :673).
	r := newTestRouter()
	res := do(t, r, http.MethodPost, "/api/trpc/upper?batch=1",
		strings.NewReader(`{"0":{"json":"board"}}`),
		map[string]string{"Content-Type": "application/json"})
	if res.status != http.StatusOK {
		t.Fatalf("got status %d, want 200", res.status)
	}
	want := `[{"result":{"data":{"json":"BOARD"}}}]`
	if res.body != want {
		t.Errorf("body = %s, want %s", res.body, want)
	}
}

func TestBatchGETStream(t *testing.T) {
	// The frontend default path: httpBatchStreamLink GET with ?batch=1 and a
	// batch-shaped input even when a single op travels alone.
	r := newTestRouter()
	res := do(t, r, http.MethodGet,
		`/api/trpc/echo?batch=1&input=%7B%220%22%3A%7B%22json%22%3A%7B%22a%22%3A1%7D%7D%7D`,
		nil,
		map[string]string{"trpc-accept": "application/jsonl"})

	if res.status != http.StatusOK {
		t.Errorf("stream status = %d, want 200 (stream calls always answer 200)", res.status)
	}
	if ct := res.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	want := "{\"json\":{\"0\":[[{\"result\":{\"data\":{\"a\":1}}}]]}}\n"
	if res.body != want {
		t.Errorf("head line = %s, want %s", res.body, want)
	}
}

func TestBatchGETStreamMixedResultAndError(t *testing.T) {
	r := newTestRouter()
	// op0 = echo success (raw number), op1 = unknown procedure, op2 = echo of {}.
	res := do(t, r, http.MethodGet,
		`/api/trpc/echo,missing,echo?batch=1&input=%7B%220%22%3A%7B%22json%22%3A7%7D%2C%222%22%3A%7B%7D%7D`,
		nil,
		map[string]string{"trpc-accept": "application/jsonl"})

	if res.status != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", res.status)
	}
	lines := strings.Split(strings.TrimSuffix(res.body, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("want a single head line, got %d lines: %q", len(lines), res.body)
	}
	var head struct {
		JSON map[string]json.RawMessage `json:"json"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &head); err != nil {
		t.Fatalf("head line is not valid JSON: %v\n%s", err, lines[0])
	}
	if len(head.JSON) != 3 {
		t.Fatalf("head has %d entries, want 3: %s", len(head.JSON), lines[0])
	}
	for _, key := range []string{"0", "1", "2"} {
		if _, ok := head.JSON[key]; !ok {
			t.Fatalf("head entry %q missing: %s", key, lines[0])
		}
	}
	// op0 succeeded with the raw (unwrapped) value; plain payloads carry no meta.
	if !strings.Contains(lines[0], `"result":{"data":7}`) {
		t.Errorf("op0 payload wrong: %s", lines[0])
	}
	if strings.Contains(lines[0], `"meta"`) {
		t.Errorf("plain payloads must not carry a superjson meta: %s", lines[0])
	}
	// op1 is the NOT_FOUND error with the numeric code and data block.
	var op1 struct {
		Error struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
			Data    struct {
				Code       string `json:"code"`
				HTTPStatus int    `json:"httpStatus"`
				Path       string `json:"path"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(decodeHeadEntry(t, head.JSON["1"]), &op1); err != nil {
		t.Fatalf("decode op1 entry: %v", err)
	}
	if op1.Error.Code != -32004 || op1.Error.Data.Code != "NOT_FOUND" || op1.Error.Data.HTTPStatus != 404 {
		t.Errorf("op1 error shape wrong: %+v", op1.Error)
	}
	if op1.Error.Data.Path != "missing" {
		t.Errorf("op1 error path = %q, want missing", op1.Error.Data.Path)
	}
	// op2 had input {} (no json member) and echo'd it back.
	var op2 struct {
		Result struct {
			Data json.RawMessage `json:"data"`
		} `json:"result"`
	}
	if err := json.Unmarshal(decodeHeadEntry(t, head.JSON["2"]), &op2); err != nil {
		t.Fatalf("decode op2 entry: %v", err)
	}
	if string(op2.Result.Data) != "{}" {
		t.Errorf("op2 data = %s, want {}", op2.Result.Data)
	}
}

func TestStreamRequiresBatch(t *testing.T) {
	// resolveResponse.ts:320-325: streaming a non-batch call is a bad request.
	r := newTestRouter()
	res := do(t, r, http.MethodGet, "/api/trpc/echo", nil,
		map[string]string{"trpc-accept": "application/jsonl"})
	if res.status != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400", res.status)
	}
	want := `{"error":{"json":{"message":"Streaming requests must be batched (you can do a batch of 1)","code":-32600,"data":{"code":"BAD_REQUEST","httpStatus":400}}}}`
	if res.body != want {
		t.Errorf("body = %s, want %s", res.body, want)
	}
}

func TestBatchPOSTClassicMixedStatuses(t *testing.T) {
	// Non-stream batch (httpBatchLink shape): JSON array, and the HTTP status
	// is 207 when the op statuses differ (getHTTPStatusCode.ts:86-88).
	r := newTestRouter()
	res := do(t, r, http.MethodPost, "/api/trpc/upper,missing?batch=1",
		strings.NewReader(`{"0":{"json":"go"},"1":{"json":null}}`),
		map[string]string{"Content-Type": "application/json"})

	if res.status != http.StatusMultiStatus {
		t.Fatalf("got status %d, want 207 (mixed 200/404)", res.status)
	}
	want := `[{` +
		`"result":{"data":{"json":"GO"}}` +
		`},{` +
		`"error":{"json":{"message":"No procedure found on path \"missing\"","code":-32004,"data":{"code":"NOT_FOUND","httpStatus":404,"path":"missing"}}}` +
		`}]`
	if res.body != want {
		t.Errorf("body = %s\n                    want %s", res.body, want)
	}
}

func TestBatchGETClassicUniformStatus(t *testing.T) {
	// Both ops fail with NOT_FOUND -> the single status 404 is used.
	r := newTestRouter()
	res := do(t, r, http.MethodGet, `/api/trpc/a,b?batch=1`, nil, nil)
	if res.status != http.StatusNotFound {
		t.Fatalf("got status %d, want 404 (uniform NOT_FOUND)", res.status)
	}
	want := `[` +
		`{"error":{"json":{"message":"No procedure found on path \"a\"","code":-32004,"data":{"code":"NOT_FOUND","httpStatus":404,"path":"a"}}}},` +
		`{"error":{"json":{"message":"No procedure found on path \"b\"","code":-32004,"data":{"code":"NOT_FOUND","httpStatus":404,"path":"b"}}}}` +
		`]`
	if res.body != want {
		t.Errorf("body = %s\n                    want %s", res.body, want)
	}
}

func TestBatchAllSuccessClassic(t *testing.T) {
	r := newTestRouter()
	res := do(t, r, http.MethodGet,
		`/api/trpc/echo,upper?batch=1&input=%7B%220%22%3A%7B%22json%22%3A%5B1%2C2%5D%7D%2C%221%22%3A%7B%22json%22%3A%22hi%22%7D%7D`,
		nil, nil)
	if res.status != http.StatusOK {
		t.Fatalf("got status %d, want 200", res.status)
	}
	want := `[{"result":{"data":{"json":[1,2]}}},{"result":{"data":{"json":"HI"}}}]`
	if res.body != want {
		t.Errorf("body = %s, want %s", res.body, want)
	}
}

func TestUnknownProcedureSingle(t *testing.T) {
	r := newTestRouter()
	res := do(t, r, http.MethodGet, "/api/trpc/board.nothing", nil, nil)
	if res.status != http.StatusNotFound {
		t.Fatalf("got status %d, want 404", res.status)
	}
	want := `{"error":{"json":{"message":"No procedure found on path \"board.nothing\"","code":-32004,"data":{"code":"NOT_FOUND","httpStatus":404,"path":"board.nothing"}}}}`
	if res.body != want {
		t.Errorf("body = %s, want %s", res.body, want)
	}
}

func TestMalformedRequests(t *testing.T) {
	r := newTestRouter()
	tests := []struct {
		name       string
		method     string
		target     string
		body       string
		headers    map[string]string
		wantStatus int
		wantSubstr string
	}{
		{
			name:       "invalid GET input json",
			method:     http.MethodGet,
			target:     "/api/trpc/echo?input=%7Bnot-json",
			wantStatus: http.StatusBadRequest,
			wantSubstr: `"code":"BAD_REQUEST"`,
		},
		{
			name:       "invalid POST body",
			method:     http.MethodPost,
			target:     "/api/trpc/echo",
			body:       `{invalid`,
			headers:    map[string]string{"Content-Type": "application/json"},
			wantStatus: http.StatusBadRequest,
			wantSubstr: `Failed to parse input`,
		},
		{
			name:       "batch input is an array, not an object",
			method:     http.MethodGet,
			target:     "/api/trpc/echo,echo?batch=1&input=%5B1%2C2%5D",
			wantStatus: http.StatusBadRequest,
			wantSubstr: `needs to be an object when doing a batch call`,
		},
		{
			name:       "POST without content-type",
			method:     http.MethodPost,
			target:     "/api/trpc/echo",
			body:       `{"json":1}`,
			wantStatus: http.StatusUnsupportedMediaType,
			wantSubstr: `Missing content-type header`,
		},
		{
			name:       "POST with unsupported content-type",
			method:     http.MethodPost,
			target:     "/api/trpc/echo",
			body:       `x`,
			headers:    map[string]string{"Content-Type": "text/plain"},
			wantStatus: http.StatusUnsupportedMediaType,
			wantSubstr: `Unsupported content-type`,
		},
		{
			name:       "multipart upload not implemented yet (M1 gap)",
			method:     http.MethodPost,
			target:     "/api/trpc/media.create",
			body:       `--boundary`,
			headers:    map[string]string{"Content-Type": "multipart/form-data; boundary=boundary"},
			wantStatus: http.StatusUnsupportedMediaType,
			wantSubstr: `not implemented in homarr-go yet`,
		},
		{
			name:       "method not supported",
			method:     http.MethodDelete,
			target:     "/api/trpc/echo",
			headers:    map[string]string{"Content-Type": "application/json"},
			wantStatus: http.StatusMethodNotAllowed,
			wantSubstr: `"code":"METHOD_NOT_SUPPORTED"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := do(t, r, tt.method, tt.target, strings.NewReader(tt.body), tt.headers)
			if res.status != tt.wantStatus {
				t.Errorf("got status %d, want %d (body %s)", res.status, tt.wantStatus, res.body)
			}
			if !strings.Contains(res.body, tt.wantSubstr) {
				t.Errorf("body %q does not contain %q", res.body, tt.wantSubstr)
			}
		})
	}
}

func TestBatchSizeGuard(t *testing.T) {
	r := newTestRouter()
	r.MaxBatchSize = 2
	res := do(t, r, http.MethodGet, "/api/trpc/a,b,c?batch=1", nil, nil)
	if res.status != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400", res.status)
	}
	want := `{"error":{"json":{"message":"Batch call exceeds maximum size","code":-32600,"data":{"code":"BAD_REQUEST","httpStatus":400}}}}`
	if res.body != want {
		t.Errorf("body = %s, want %s", res.body, want)
	}
}

func TestBodySizeLimit(t *testing.T) {
	r := newTestRouter()
	r.MaxBodyBytes = 8
	res := do(t, r, http.MethodPost, "/api/trpc/echo",
		strings.NewReader(`{"json":"way too long for the limit"}`),
		map[string]string{"Content-Type": "application/json"})
	if res.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("got status %d, want 413", res.status)
	}
	if !strings.Contains(res.body, `"code":"PAYLOAD_TOO_LARGE"`) {
		t.Errorf("body %q missing PAYLOAD_TOO_LARGE", res.body)
	}
}

func TestPanicIsolationPerOperation(t *testing.T) {
	r := newTestRouter()
	res := do(t, r, http.MethodGet, `/api/trpc/boom,echo?batch=1&input=%7B%221%22%3A%7B%22json%22%3A%22ok%22%7D%7D`, nil, nil)
	if res.status != http.StatusMultiStatus {
		t.Fatalf("got status %d, want 207 (mixed 500/200)", res.status)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal([]byte(res.body), &entries); err != nil {
		t.Fatalf("batch body is not an array: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	var op0 struct {
		Error struct {
			JSON struct {
				Code int `json:"code"`
				Data struct {
					Code string `json:"code"`
				} `json:"data"`
			} `json:"json"`
		} `json:"error"`
	}
	if err := json.Unmarshal(entries[0], &op0); err != nil {
		t.Fatalf("decode op0: %v", err)
	}
	if op0.Error.JSON.Code != -32603 || op0.Error.JSON.Data.Code != "INTERNAL_SERVER_ERROR" {
		t.Errorf("op0 should be INTERNAL_SERVER_ERROR, got %+v", op0.Error)
	}
	var op1 struct {
		Result struct {
			Data json.RawMessage `json:"data"`
		} `json:"result"`
	}
	if err := json.Unmarshal(entries[1], &op1); err != nil {
		t.Fatalf("decode op1: %v", err)
	}
	if string(op1.Result.Data) != `{"json":"ok"}` {
		t.Errorf("op1 data = %s, want the surviving op to succeed", op1.Result.Data)
	}
}

func TestHandlerErrorPropagates(t *testing.T) {
	r := trpc.New()
	r.Register("deny", failWith(trpc.CodeForbidden, "Permission denied"))
	res := do(t, r, http.MethodGet, "/api/trpc/deny", nil, nil)
	if res.status != http.StatusForbidden {
		t.Fatalf("got status %d, want 403", res.status)
	}
	want := `{"error":{"json":{"message":"Permission denied","code":-32003,"data":{"code":"FORBIDDEN","httpStatus":403,"path":"deny"}}}}`
	if res.body != want {
		t.Errorf("body = %s, want %s", res.body, want)
	}
}

func TestErrorHTTPStatusMapping(t *testing.T) {
	// The full tRPC 11 table (rpc/codes.ts + getHTTPStatusCode.ts).
	tests := []struct {
		code     trpc.Code
		number   int
		httpCode int
	}{
		{trpc.CodeParseError, -32700, 400},
		{trpc.CodeBadRequest, -32600, 400},
		{trpc.CodeInternalServerError, -32603, 500},
		{trpc.CodeNotImplemented, -32603, 501},
		{trpc.CodeBadGateway, -32603, 502},
		{trpc.CodeServiceUnavailable, -32603, 503},
		{trpc.CodeGatewayTimeout, -32603, 504},
		{trpc.CodeUnauthorized, -32001, 401},
		{trpc.CodePaymentRequired, -32002, 402},
		{trpc.CodeForbidden, -32003, 403},
		{trpc.CodeNotFound, -32004, 404},
		{trpc.CodeMethodNotSupported, -32005, 405},
		{trpc.CodeTimeout, -32008, 408},
		{trpc.CodeConflict, -32009, 409},
		{trpc.CodePreconditionFailed, -32012, 412},
		{trpc.CodePayloadTooLarge, -32013, 413},
		{trpc.CodeUnsupportedMediaType, -32015, 415},
		{trpc.CodeUnprocessableContent, -32022, 422},
		{trpc.CodePreconditionRequired, -32028, 428},
		{trpc.CodeTooManyRequests, -32029, 429},
		{trpc.CodeClientClosedRequest, -32099, 499},
		{trpc.Code("MADE_UP"), -32603, 500}, // unknown codes fall back like upstream
	}
	for _, tt := range tests {
		if got := tt.code.Number(); got != tt.number {
			t.Errorf("%s.Number() = %d, want %d", tt.code, got, tt.number)
		}
		if got := tt.code.HTTPStatus(); got != tt.httpCode {
			t.Errorf("%s.HTTPStatus() = %d, want %d", tt.code, got, tt.httpCode)
		}
	}
}

func TestUnwrapInput(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"superjson envelope", `{"json":{"a":1}}`, `{"a":1}`},
		{"envelope with meta is unwrapped, meta ignored", `{"json":"2026-01-01T00:00:00Z","meta":{"values":{"json":["Date"]},"v":1}}`, `"2026-01-01T00:00:00Z"`},
		{"plain object without envelope passes through", `{"a":1}`, `{"a":1}`},
		{"array passes through", `[1,2]`, `[1,2]`},
		{"json null", `{"json":null}`, `null`},
		{"bare null", `null`, `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := trpc.UnwrapInput(json.RawMessage(tt.in))
			if err != nil {
				t.Fatalf("UnwrapInput(%s): %v", tt.in, err)
			}
			if string(got) != tt.want {
				t.Errorf("UnwrapInput(%s) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
	empty, err := trpc.UnwrapInput(nil)
	if err != nil || string(empty) != "null" {
		t.Errorf("UnwrapInput(nil) = %s, %v; want null", empty, err)
	}
	if _, err := trpc.UnwrapInput(json.RawMessage(`{invalid`)); err == nil {
		t.Error("UnwrapInput should surface JSON syntax errors")
	}
}

func TestWrapJSON(t *testing.T) {
	b, err := trpc.WrapJSON(map[string]any{"a": 1})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"json":{"a":1}}` {
		t.Errorf("WrapJSON = %s", b)
	}
	empty, err := trpc.WrapJSON(nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(empty) != `{}` {
		t.Errorf("WrapJSON(nil) = %s, want {} (superjson undefined)", empty)
	}
}

func TestHandlerResultNilMirrorsUndefined(t *testing.T) {
	r := trpc.New()
	r.Register("void", func(_ context.Context, _ json.RawMessage) (any, *trpc.Error) { return nil, nil })
	res := do(t, r, http.MethodGet, "/api/trpc/void", nil, nil)
	if res.status != http.StatusOK {
		t.Fatalf("got status %d, want 200", res.status)
	}
	// superjson-serializing `undefined` collapses to {} inside result.data.
	want := `{"result":{"data":{}}}`
	if res.body != want {
		t.Errorf("body = %s, want %s", res.body, want)
	}
}

func TestHEADAndOptions(t *testing.T) {
	r := newTestRouter()
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	// HEAD acts as a warmup endpoint (resolveResponse.ts:227-232).
	res, err := srv.Client().Head(srv.URL + "/api/trpc/echo")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Errorf("HEAD: got status %d, want 204", res.StatusCode)
	}

	// OPTIONS is the CORS preflight of the upstream route handler.
	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/api/trpc/echo", nil)
	res2, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusNoContent {
		t.Errorf("OPTIONS: got status %d, want 204", res2.StatusCode)
	}
	for k, want := range map[string]string{
		"Access-Control-Allow-Origin":   "*",
		"Access-Control-Request-Method": "*",
		"Access-Control-Allow-Methods":  "OPTIONS, GET, POST",
		"Access-Control-Allow-Headers":  "*",
	} {
		if got := res2.Header.Get(k); got != want {
			t.Errorf("OPTIONS header %s = %q, want %q", k, got, want)
		}
	}
}

func TestXTrpcSourceAccepted(t *testing.T) {
	// The server only logs x-trpc-source upstream (trpc.ts:55-61); any value
	// (or none) must be accepted.
	r := newTestRouter()
	for _, source := range []string{"", "nextjs-react (json)", "fetch", "unknown-client"} {
		headers := map[string]string{}
		if source != "" {
			headers["X-Trpc-Source"] = source
		}
		res := do(t, r, http.MethodGet, "/api/trpc/echo?input=%7B%22json%22%3A1%7D", nil, headers)
		if res.status != http.StatusOK {
			t.Errorf("source %q: got status %d, want 200", source, res.status)
		}
	}
}

func TestRegisterPanicsOnDuplicates(t *testing.T) {
	r := trpc.New()
	r.Register("x", echo)
	mustPanic(t, "duplicate", func() { r.Register("x", echo) })
	mustPanic(t, "empty name", func() { r.Register("", echo) })
	mustPanic(t, "nil proc", func() { r.Register("y", nil) })
}

func mustPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: expected panic", name)
		}
	}()
	fn()
}
