package trpc

import (
	"bytes"
	"encoding/json"
	"errors"
)

// superjson helpers.
//
// Homarr configures tRPC with the superjson transformer
// (packages/api/src/trpc.ts:79-93). superjson 2.x serializes a payload as
// {"json": <payload>} and adds "meta" ONLY when the payload contains
// non-JSON-native types (superjson src/index.ts, serialize: res.meta is set
// just when the walker produced value annotations or referential-equality
// annotations, then stamped meta.v = 1). For plain JSON payloads the wire
// form is exactly {"json": <payload>} with no meta key; the client's
// deserialize reads meta?.values optionally and is unaffected by its absence.
//
// homarr-go procedure payloads are plain JSON (dates travel as ISO strings),
// so this package always omits meta on output and ignores meta on input:
// special types arrive in their JSON-shaped form, which is what the Go
// handlers work with anyway. Reconstructing Date/Map/BigInt types (meta.v
// annotations) is a future extension if a procedure ever needs it.

// jsonNull is the canonical JSON null used for absent inputs. Upstream passes
// `undefined` through; on the Go side a handler sees an explicit null.
var jsonNull = json.RawMessage("null")

// UnwrapInput removes the superjson envelope from a serialized procedure
// input: {"json": X, "meta": ...} -> X. This is the server-side half of the
// transformer (transformer.input.deserialize; tRPC applies it per input in
// http/contentType.ts:117 and :132). Inputs that are valid JSON but not
// envelope objects (arrays, strings, numbers, objects without a "json"
// member) pass through unchanged so plain-JSON callers (no transformer)
// still work; empty inputs become "null". Invalid JSON is an error —
// surfaced as a per-operation BAD_REQUEST, mirroring tRPC where input
// parsing failures hit the op-level catch.
func UnwrapInput(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return jsonNull, nil
	}
	if !json.Valid(raw) {
		return nil, errors.New("invalid JSON")
	}
	var probe struct {
		JSON json.RawMessage `json:"json"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil && len(probe.JSON) > 0 {
		return probe.JSON, nil
	}
	// Not an envelope object: pass the value through untouched.
	return raw, nil
}

// WrapJSON marshals v as a superjson payload ({"json": v}). v must be
// JSON-serializable. A nil v mirrors superjson serializing `undefined`, which
// JSON.stringify collapses to an empty object.
//
// A SuperJSON value passes through verbatim: it already carries its own
// json/meta envelope (see SuperJSON).
func WrapJSON(v any) (json.RawMessage, error) {
	if v == nil {
		return json.RawMessage("{}"), nil
	}
	if sj, ok := v.(SuperJSON); ok {
		return json.RawMessage(sj), nil
	}
	return marshalJSON(struct {
		JSON any `json:"json"`
	}{JSON: v})
}

// SuperJSON is a fully transformed payload. Procedures, subscription Initial
// values and subscription Decode results that need superjson TYPE annotations
// (meta.values — e.g. Date fields the SPA must observe as real Date objects)
// return this raw envelope instead of a plain value: the default
// {"json": <output>} wrapping is skipped and the message reaches the wire
// verbatim inside result.data, both on the HTTP side (writePlain, SSE data
// events) and the WebSocket side (ws/conn.go wraps through WrapJSON).
//
// The message must be exactly what superjson's serialize would emit:
// {"json": <value>, "meta": {"values": {...}, "v": 1}} (superjson src/index.ts
// serialize: meta carries the walker annotations and is stamped v = 1).
type SuperJSON json.RawMessage

// WrapJSONWithMeta builds the full superjson envelope for a payload whose
// client-side reconstruction needs type annotations: values maps DOTTED
// payload paths (the superjson walker format — object members joined with ".",
// array elements by index) to type names, e.g. {"timestamp": "Date"} for a
// top-level Date or {"messages.0.timestamp": "Date"} inside a list. The
// result feeds procedures as a SuperJSON return value.
func WrapJSONWithMeta(v any, values map[string]string) (SuperJSON, error) {
	payload, err := marshalJSON(v)
	if err != nil {
		return nil, err
	}
	metaValues := make(map[string]string, len(values))
	for path, typeName := range values {
		metaValues[path] = typeName
	}
	meta, err := marshalJSON(struct {
		Values map[string]string `json:"values"`
		V      int               `json:"v"`
	}{Values: metaValues, V: 1})
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteString(`{"json":`)
	buf.Write(payload)
	buf.WriteString(`,"meta":`)
	buf.Write(meta)
	buf.WriteString(`}`)
	return SuperJSON(buf.Bytes()), nil
}

// marshalJSON encodes v without HTML escaping, matching JSON.stringify on the
// JavaScript side byte-for-byte for the values homarr-go emits.
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
