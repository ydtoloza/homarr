// Package admin carries the helpers shared by the administration tRPC
// routers (user, group, invite, onboard, serverSettings, searchEngine,
// bangs, apiKeys, info): input decoding with upstream zod parity, the
// caller/permission gates that mirror the upstream tRPC middlewares, the
// onboarding step resolution and the cuid2-like id generator.
//
// The board domain keeps its own copies inside package board (json.go,
// perms.go); this package exists so the nine smaller admin packages do not
// duplicate them. It lives under router/internal so only router packages can
// import it.
package admin

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
)

// JSON helpers shared by the administration procedures. The Homarr
// transformer is superjson (packages/api/src/trpc.ts:83): payloads on the
// wire and option blobs in the database are wrapped as {"json": <value>}
// with a "meta" member only when non-JSON-native types are present. The Go
// port works with plain JSON, so reading unwraps the envelope and writing
// re-wraps it.

// UnwrapSuperJSON returns the payload of a superjson envelope. Values that
// are not envelope objects pass through unchanged (same contract as
// trpc.UnwrapInput). Invalid JSON yields a BAD_REQUEST.
func UnwrapSuperJSON(raw json.RawMessage, what string) (json.RawMessage, *trpc.Error) {
	if len(raw) == 0 {
		return json.RawMessage("null"), nil
	}
	var probe struct {
		JSON json.RawMessage `json:"json"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil && len(probe.JSON) > 0 {
		return probe.JSON, nil
	}
	if !json.Valid(raw) {
		return nil, BadRequestf("Failed to parse input: invalid JSON in %s", what)
	}
	return raw, nil
}

// WrapSuperJSON serializes v as a superjson payload string for storage in
// the serverSetting.value column: superjson.stringify(v). Plain JSON values
// never carry meta (superjson serialize omits it; see trpc/superjson.go for
// the verified contract).
func WrapSuperJSON(v any) string {
	if v == nil {
		return `{"json": {}}`
	}
	data, err := json.Marshal(struct {
		JSON any `json:"json"`
	}{JSON: v})
	if err != nil {
		// Callers pass already-decoded JSON values, which always marshal.
		return `{"json": {}}`
	}
	return string(data)
}

// BadRequestf builds a BAD_REQUEST tRPC error (the equivalent of throwing
// new TRPCError({ code: "BAD_REQUEST", message })). Input validation in the
// admin routers follows the board house style: BAD_REQUEST, like a tRPC
// input validation failure.
func BadRequestf(format string, args ...any) *trpc.Error {
	return trpc.NewError(trpc.CodeBadRequest, fmt.Sprintf(format, args...))
}

// InternalError wraps an unexpected Go error (getTRPCErrorFromUnknown).
func InternalError(cause error) *trpc.Error {
	return trpc.Internal(cause)
}

// --- input decoding ----------------------------------------------------------
//
// Upstream validates inputs with zod schemas (packages/validation). The Go
// procedures decode the JSON envelope and validate field by field with the
// helpers below; every violation becomes BAD_REQUEST like a tRPC input
// validation failure.

// InputDecoder walks one decoded JSON input object.
type InputDecoder struct {
	obj map[string]any
}

// NewInputDecoder decodes a procedure input into an object decoder. A nil /
// null input yields an empty object (fields then fail as missing).
func NewInputDecoder(input json.RawMessage) (*InputDecoder, *trpc.Error) {
	obj, err := parseObject(input, "input")
	if err != nil {
		return nil, err
	}
	return &InputDecoder{obj: obj}, nil
}

// parseObject decodes a JSON object into a generic map.
func parseObject(raw json.RawMessage, what string) (map[string]any, *trpc.Error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, BadRequestf("expected an object for %s", what)
	}
	return obj, nil
}

// Has reports whether the key is present (present-but-null included).
func (d *InputDecoder) Has(key string) bool {
	_, ok := d.obj[key]
	return ok
}

// Object returns a decoder for a nested object field. A missing or null
// field yields an empty decoder (fields then fail as missing), matching the
// zod behavior for .optional() nested objects read field-wise.
func (d *InputDecoder) Object(key string) (*InputDecoder, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok || v == nil {
		return &InputDecoder{obj: map[string]any{}}, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, BadRequestf(`expected an object for input key "%s"`, key)
	}
	return &InputDecoder{obj: obj}, nil
}

// Raw returns the raw decoded value of a key (present-but-null included).
func (d *InputDecoder) Raw(key string) (any, bool) {
	v, ok := d.obj[key]
	return v, ok
}

// All returns every key/value of the decoded object. Callers must not
// mutate the returned map.
func (d *InputDecoder) All() map[string]any {
	return d.obj
}

// UnknownKeys lists the keys that are not in the allowed set — used to
// replicate zod .strict() objects (unknown keys reject).
func (d *InputDecoder) UnknownKeys(allowed []string) []string {
	var unknown []string
	for key := range d.obj {
		if !ContainsString(allowed, key) {
			unknown = append(unknown, key)
		}
	}
	return unknown
}

// OptNumberRange returns an optional JSON number with range bounds
// (zod z.number().min(...).max(...)).
func (d *InputDecoder) OptNumberRange(key string, min, max float64) (float64, bool, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	n, isNumber := v.(float64)
	if !isNumber {
		return 0, false, BadRequestf(`expected a number for input key "%s"`, key)
	}
	if n < min || n > max {
		return 0, false, BadRequestf(`input key "%s" must be between %v and %v`, key, min, max)
	}
	return n, true, nil
}

// str returns a required string field.
func (d *InputDecoder) Str(key string) (string, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok {
		return "", BadRequestf(`expected a string for input key "%s"`, key)
	}
	s, ok := v.(string)
	if !ok {
		return "", BadRequestf(`expected a string for input key "%s"`, key)
	}
	return s, nil
}

// optStr returns an optional string field (missing or null -> ok=false).
func (d *InputDecoder) OptStr(key string) (string, bool, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok || v == nil {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", false, BadRequestf(`expected a string for input key "%s"`, key)
	}
	return s, true, nil
}

// int64 returns a required integer field (JSON numbers decode as float64).
func (d *InputDecoder) Int64(key string, min, max int64) (int64, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok {
		return 0, BadRequestf(`expected a number for input key "%s"`, key)
	}
	n, err := numberToInt64(v)
	if err != nil {
		return 0, BadRequestf(`expected an integer for input key "%s"`, key)
	}
	if n < min || n > max {
		return 0, BadRequestf(`input key "%s" must be between %d and %d`, key, min, max)
	}
	return n, nil
}

// optInt64 returns an optional integer field (missing or null -> ok=false).
func (d *InputDecoder) OptInt64(key string, min, max int64) (int64, bool, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	n, err := numberToInt64(v)
	if err != nil {
		return 0, false, BadRequestf(`expected an integer for input key "%s"`, key)
	}
	if n < min || n > max {
		return 0, false, BadRequestf(`input key "%s" must be between %d and %d`, key, min, max)
	}
	return n, true, nil
}

// bool returns a required boolean field.
func (d *InputDecoder) Bool(key string) (bool, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok {
		return false, BadRequestf(`expected a boolean for input key "%s"`, key)
	}
	b, ok := v.(bool)
	if !ok {
		return false, BadRequestf(`expected a boolean for input key "%s"`, key)
	}
	return b, nil
}

// optBool returns an optional boolean field (missing or null -> ok=false).
func (d *InputDecoder) OptBool(key string) (bool, bool, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok || v == nil {
		return false, false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, false, BadRequestf(`expected a boolean for input key "%s"`, key)
	}
	return b, true, nil
}

// strList returns a required array of strings.
func (d *InputDecoder) StrList(key string) ([]string, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok {
		return nil, BadRequestf(`expected an array for input key "%s"`, key)
	}
	return stringList(v, key)
}

// strListDefault returns an array of strings; a missing or null key yields
// def (zod .default()).
func (d *InputDecoder) StrListDefault(key string, def []string) ([]string, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok || v == nil {
		return def, nil
	}
	return stringList(v, key)
}

// optStrList returns an optional array of strings (missing -> ok=false).
func (d *InputDecoder) OptStrList(key string) ([]string, bool, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok || v == nil {
		return nil, false, nil
	}
	list, err := stringList(v, key)
	return list, err == nil, err
}

func stringList(v any, key string) ([]string, *trpc.Error) {
	list, ok := v.([]any)
	if !ok {
		return nil, BadRequestf(`expected an array for input key "%s"`, key)
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil, BadRequestf(`expected strings inside input key "%s"`, key)
		}
		out = append(out, s)
	}
	return out, nil
}

// enum validates that a string is one of the allowed values.
func (d *InputDecoder) Enum(key string, allowed []string) (string, *trpc.Error) {
	v, err := d.Str(key)
	if err != nil {
		return "", err
	}
	for _, a := range allowed {
		if a == v {
			return v, nil
		}
	}
	return "", BadRequestf(`input key "%s" has an invalid value`, key)
}

// stringPtr returns a nullable string field: JSON null and absent both give
// (nil, true). A non-string non-null value is a validation error.
func (d *InputDecoder) StringPtr(key string) (*string, bool, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok || v == nil {
		return nil, true, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, false, BadRequestf(`expected a string or null for input key "%s"`, key)
	}
	return &s, true, nil
}

// numberToInt64 coerces a decoded JSON number into an int64, accepting only
// integral values (zod z.number().int()).
func numberToInt64(v any) (int64, error) {
	n, ok := v.(float64)
	if !ok {
		if i, isInt := v.(int64); isInt {
			return i, nil
		}
		return 0, fmt.Errorf("not a number")
	}
	if n != float64(int64(n)) {
		return 0, fmt.Errorf("not an integer")
	}
	return int64(n), nil
}

// --- id generation -----------------------------------------------------------

// CreateID generates the application-side ids the upstream procedures create
// with createId() (@homarr/common, cuid2): lowercase alphanumeric. cuid2 ids
// are 24 characters; the exact generator is not load-bearing for the wire
// contract (ids are opaque strings for the frontend), but tests rely on ids
// starting with a letter to stay SQLite-binder friendly.
func CreateID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	const length = 24
	out := make([]byte, length)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			// crypto/rand failing is unrecoverable here; procedures would
			// misbehave with duplicate ids downstream.
			panic("admin: CreateID: " + err.Error())
		}
		out[i] = alphabet[n.Int64()]
	}
	// Keep the first character alphabetic.
	out[0] = alphabet[int(out[0]-'0')%26]
	return string(out)
}

// ContainsString reports whether the list contains v.
func ContainsString(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// Unique keeps the first occurrence of every string, preserving order (the
// equivalent of [...new Set(list)] in the upstream procedures).
func Unique(list []string) []string {
	seen := make(map[string]bool, len(list))
	out := make([]string, 0, len(list))
	for _, item := range list {
		if seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}
