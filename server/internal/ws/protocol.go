package ws

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
)

// Wire protocol (tRPC 11.18.0, JSON encoder — packages/server/src/adapters/
// wsEncoder.ts jsonEncoder, and the envelopes of
// packages/server/src/unstable-core-do-not-import/rpc/envelopes.ts:85-153):
//
//	client → server:
//	  {"id":1,"method":"subscription","params":{"path":"log.subscribe","input":{"json":...}}}
//	  {"id":1,"method":"subscription.stop"}                      (stop by request id)
//	  {"id":null,"method":"reconnect"}                           (server → client only)
//	  text frames "PING" / "PONG"                                (keepalive, not JSON-RPC)
//	server → client:
//	  {"id":1,"result":{"type":"started"}}
//	  {"id":1,"result":{"type":"data","data":{"json":<payload>}}}   (superjson wrap)
//	  {"id":1,"result":{"type":"stopped"}}
//	  {"id":1,"error":{"json":{"message","code","data":{"code","httpStatus","path"}}}}
//	  {"id":null,"error":{...}}                                  (protocol-level errors)
//	  {"id":null,"method":"reconnect"}                           (shutdown broadcast)
//
// The transformer only superjson-wraps result.data and the whole error shape;
// "started"/"stopped" frames carry no data key at all
// (transformer.ts transformTRPCResponseItem:92-113).

// textPing/textPong are the plain-text keepalive messages tRPC exchanges
// outside the JSON-RPC envelope (ws.ts handleKeepAlive sends 'PING'; both
// peers answer 'PONG' and skip them in their JSON dispatch, ws.ts
// client.on('message') and wsLink wsClient.ts:323-324).
const (
	textPing = "PING"
	textPong = "PONG"
)

// incomingMessage is one parsed client frame (parseTRPCMessage output).
type incomingMessage struct {
	// ID is the raw JSON request id (number or string). Missing/null is
	// rejected before dispatch ("`id` is required", ws.ts handleRequest).
	ID json.RawMessage
	// Method is "query", "mutation", "subscription" or "subscription.stop".
	Method string
	// Path and Input are params.path / params.input (input superjson-unwrapped).
	Path  string
	Input json.RawMessage
	// LastEventID merges into Input when the client resumes a subscription.
	LastEventID string
}

// parseMessage replicates parseTRPCMessage
// (rpc/parseTRPCMessage.ts:51-89) for one raw frame object: structural
// assertions fail with PARSE_ERROR carrying the same messages; the input is
// superjson-unwrapped like transformer.input.deserialize.
func parseMessage(raw json.RawMessage) (incomingMessage, *trpc.Error) {
	var probe struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&probe); err != nil {
		return incomingMessage{}, &trpc.Error{
			Code:    trpc.CodeParseError,
			Message: err.Error(), // cause?.message ?? code (TRPCError.ts:76)
		}
	}

	// method === 'subscription.stop' short-circuits before the type assert
	// (parseTRPCMessage.ts:61-67): only the id travels.
	if probe.Method == "subscription.stop" {
		return incomingMessage{ID: probe.ID, Method: probe.Method}, nil
	}

	switch probe.Method {
	case "query", "mutation", "subscription":
	default:
		// assertIsProcedureType throws Error("Invalid procedure type"), which
		// the message handler catch wraps as PARSE_ERROR (ws.ts:494-509).
		return incomingMessage{}, &trpc.Error{
			Code:    trpc.CodeParseError,
			Message: "Invalid procedure type",
		}
	}

	var params struct {
		Path        string          `json:"path"`
		Input       json.RawMessage `json:"input"`
		LastEventID *string         `json:"lastEventId"`
	}
	if !isJSONObject(probe.Params) {
		// assertIsObject(params) → Error("Not an object") → PARSE_ERROR.
		return incomingMessage{}, &trpc.Error{
			Code:    trpc.CodeParseError,
			Message: "Not an object",
		}
	}
	if err := json.Unmarshal(probe.Params, &params); err != nil {
		// The params object exists but a member has the wrong type
		// (assertIsString for path/lastEventId → "Invalid string").
		return incomingMessage{}, &trpc.Error{
			Code:    trpc.CodeParseError,
			Message: "Invalid string",
		}
	}
	if params.Path == "" {
		// assertIsString(params.path) → Error("Invalid string").
		return incomingMessage{}, &trpc.Error{
			Code:    trpc.CodeParseError,
			Message: "Invalid string",
		}
	}

	input, err := trpc.UnwrapInput(params.Input)
	if err != nil {
		return incomingMessage{}, &trpc.Error{Code: trpc.CodeParseError, Message: err.Error()}
	}

	msg := incomingMessage{
		ID:     probe.ID,
		Method: probe.Method,
		Path:   params.Path,
		Input:  input,
	}
	// lastEventId is merged into the input by the WS adapter
	// (ws.ts:171-180): object input gets the key added, absent input is
	// replaced by {"lastEventId": ...}, any other value keeps lastEventId
	// dropped.
	if params.LastEventID != nil {
		msg.LastEventID = *params.LastEventID
		compact := bytes.TrimSpace(input)
		if len(compact) > 0 && compact[0] == '{' {
			// Insert the key before the closing brace so the client's key
			// order is preserved (the upstream spread does the same).
			quoted, err := marshalNoEscape(*params.LastEventID)
			if err == nil {
				closeIdx := bytes.LastIndexByte(compact, '}')
				merged := make(json.RawMessage, 0, len(compact)+len(quoted)+20)
				merged = append(merged, compact[:closeIdx]...)
				merged = append(merged, `,"lastEventId":`...)
				merged = append(merged, quoted...)
				merged = append(merged, '}')
				msg.Input = merged
			}
		} else if string(compact) == "null" {
			msg.Input, _ = marshalNoEscape(map[string]string{"lastEventId": *params.LastEventID})
		}
	}
	return msg, nil
}

// parseFrames decodes one WebSocket text message into the messages it
// carries. A single object is one message; an array is a batch
// (ws.ts client.on('message'): `Array.isArray(msgJSON) ? msgJSON : [msgJSON]`).
// All-or-nothing: any structural failure yields a single PARSE_ERROR and
// drops the whole batch, exactly like upstream (the parse runs inside one
// try/catch before any dispatch).
func parseFrames(data []byte) ([]incomingMessage, *trpc.Error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			return nil, &trpc.Error{Code: trpc.CodeParseError, Message: err.Error()}
		}
		msgs := make([]incomingMessage, 0, len(batch))
		for _, raw := range batch {
			m, terr := parseMessage(raw)
			if terr != nil {
				return nil, terr
			}
			msgs = append(msgs, m)
		}
		return msgs, nil
	}
	m, terr := parseMessage(trimmed)
	if terr != nil {
		return nil, terr
	}
	return []incomingMessage{m}, nil
}

func isJSONObject(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == '{'
}

// --- outgoing frame builders ---

// errorShape mirrors getErrorShape (tRPC error/getErrorShape.ts:21-35) with
// the homarr errorFormatter additions omitted (see PROTOCOL.md §5):
// data.stack only exists in dev, so homarr-go never emits it.
type errorShape struct {
	Message string         `json:"message"`
	Code    int            `json:"code"`
	Data    map[string]any `json:"data"`
}

func buildErrorShape(code trpc.Code, message, path string) errorShape {
	data := map[string]any{
		"code":       string(code),
		"httpStatus": code.HTTPStatus(),
	}
	if path != "" {
		data["path"] = path
	}
	return errorShape{Message: message, Code: code.Number(), Data: data}
}

// frameError builds {"id":<id>,"error":{"json":<shape>}} — the whole error
// shape is superjson-wrapped by transformTRPCResponseItem (transformer.ts:92-97).
func frameError(id json.RawMessage, code trpc.Code, message, path string) ([]byte, error) {
	wrapped, err := trpc.WrapJSON(buildErrorShape(code, message, path))
	if err != nil {
		return nil, err
	}
	return frameWithID(id, `"error"`, wrapped), nil
}

// frameData builds {"id":<id>,"result":{"type":"data","data":<wrapped>}}.
// wrapped must already be the superjson envelope ({"json": ...}).
func frameData(id json.RawMessage, wrapped json.RawMessage) []byte {
	var buf bytes.Buffer
	buf.WriteString(`{"id":`)
	buf.Write(idOrEmpty(id))
	buf.WriteString(`,"result":{"type":"data","data":`)
	buf.Write(wrapped)
	buf.WriteString(`}}`)
	return buf.Bytes()
}

// frameResultState builds the started/stopped frames: no data key
// (transformTRPCResponseItem only wraps when 'data' in result).
func frameResultState(id json.RawMessage, state string) []byte {
	var buf bytes.Buffer
	buf.WriteString(`{"id":`)
	buf.Write(idOrEmpty(id))
	buf.WriteString(`,"result":{"type":"`)
	buf.WriteString(state)
	buf.WriteString(`"}}`)
	return buf.Bytes()
}

// frameWithID builds {"id":<id>,<key>:<value>} for error frames.
func frameWithID(id json.RawMessage, key string, value json.RawMessage) []byte {
	var buf bytes.Buffer
	buf.WriteString(`{"id":`)
	buf.Write(idOrEmpty(id))
	buf.WriteString(`,`)
	buf.WriteString(key)
	buf.WriteString(`:`)
	buf.Write(value)
	buf.WriteString(`}`)
	return buf.Bytes()
}

func idOrEmpty(id json.RawMessage) []byte {
	if len(id) == 0 {
		return []byte("null")
	}
	return id
}

// frameReconnect is the shutdown notification
// (applyWSSHandler broadcastReconnectNotification, ws.ts:617-627:
// `{id: null, method: 'reconnect'}`).
func frameReconnect() []byte {
	return []byte(`{"id":null,"method":"reconnect"}`)
}

// marshalNoEscape encodes v without HTML escaping, matching JSON.stringify
// byte-for-byte for the values the hub emits.
func marshalNoEscape(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// errNoProcedure is the NOT_FOUND the router raises for unknown paths or
// type mismatches (tRPC router.ts callProcedure:411-419:
// `No "<type>"-procedure on path "<path>"`).
func errNoProcedure(procType, path string) *trpc.Error {
	return &trpc.Error{
		Code:    trpc.CodeNotFound,
		Message: fmt.Sprintf("No %q-procedure on path %q", procType, path),
	}
}
