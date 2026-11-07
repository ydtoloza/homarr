package board

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"sync"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
)

// JSON helpers shared by the board-domain procedures. The Homarr transformer
// is superjson (packages/api/src/trpc.ts:83): payloads on the wire and option
// blobs in the database are wrapped as {"json": <value>} with a "meta" member
// only when non-JSON-native types are present. The Go port works with plain
// JSON, so reading unwraps the envelope and writing re-wraps it.

// unwrapSuperJSON returns the payload of a superjson envelope. Values that
// are not envelope objects pass through unchanged (same contract as
// trpc.UnwrapInput). Invalid JSON yields a BAD_REQUEST.
func unwrapSuperJSON(raw json.RawMessage, what string) (json.RawMessage, *trpc.Error) {
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
		return nil, trpc.NewError(trpc.CodeBadRequest, "Failed to parse input: invalid JSON in "+what)
	}
	return raw, nil
}

// wrapSuperJSON serializes v as a superjson payload string for storage in the
// options / advancedOptions columns: superjson.stringify(v). Plain JSON
// values never carry meta (superjson serialize omits it; see
// trpc/superjson.go for the verified contract).
func wrapSuperJSON(v any) string {
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

// parseObject decodes a JSON object into a generic map.
func parseObject(raw json.RawMessage, what string) (map[string]any, *trpc.Error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, badRequestf("expected an object for %s", what)
	}
	return obj, nil
}

// badRequestf builds a BAD_REQUEST tRPC error (the equivalent of throwing
// new TRPCError({ code: "BAD_REQUEST", message })).
func badRequestf(format string, args ...any) *trpc.Error {
	return trpc.NewError(trpc.CodeBadRequest, fmt.Sprintf(format, args...))
}

// internalError wraps an unexpected Go error (getTRPCErrorFromUnknown).
func internalError(cause error) *trpc.Error {
	return trpc.Internal(cause)
}

// --- input decoding ----------------------------------------------------------
//
// Upstream validates inputs with zod schemas (packages/validation). The Go
// procedures decode the JSON envelope and validate field by field with the
// helpers below; every violation becomes BAD_REQUEST like a tRPC input
// validation failure.

// inputDecoder walks one decoded JSON input object.
type inputDecoder struct {
	obj map[string]any
}

// newInputDecoder decodes a procedure input into an object decoder. A nil /
// null input yields an empty object (fields then fail as missing).
func newInputDecoder(input json.RawMessage) (*inputDecoder, *trpc.Error) {
	obj, err := parseObject(input, "input")
	if err != nil {
		return nil, err
	}
	return &inputDecoder{obj: obj}, nil
}

func (d *inputDecoder) raw(key string) (json.RawMessage, bool) {
	v, ok := d.obj[key]
	if !ok || v == nil {
		return nil, false
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return data, true
}

func (d *inputDecoder) has(key string) bool {
	_, ok := d.obj[key]
	return ok
}

// str returns a required string field.
func (d *inputDecoder) str(key string) (string, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok {
		return "", badRequestf(`expected a string for input key "%s"`, key)
	}
	s, ok := v.(string)
	if !ok {
		return "", badRequestf(`expected a string for input key "%s"`, key)
	}
	return s, nil
}

// optStr returns an optional string field (missing -> ok=false).
func (d *inputDecoder) optStr(key string) (string, bool, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok || v == nil {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", false, badRequestf(`expected a string for input key "%s"`, key)
	}
	return s, true, nil
}

// int64 returns a required integer field (JSON numbers decode as float64).
func (d *inputDecoder) int64(key string, min, max int64) (int64, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok {
		return 0, badRequestf(`expected a number for input key "%s"`, key)
	}
	n, err := numberToInt64(v)
	if err != nil {
		return 0, badRequestf(`expected an integer for input key "%s"`, key)
	}
	if n < min || n > max {
		return 0, badRequestf(`input key "%s" must be between %d and %d`, key, min, max)
	}
	return n, nil
}

// optInt64 returns an optional integer field.
func (d *inputDecoder) optInt64(key string, min, max int64) (int64, bool, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	n, err := numberToInt64(v)
	if err != nil {
		return 0, false, badRequestf(`expected an integer for input key "%s"`, key)
	}
	if n < min || n > max {
		return 0, false, badRequestf(`input key "%s" must be between %d and %d`, key, min, max)
	}
	return n, true, nil
}

// optNumber returns an optional JSON number without integer coercion
// (boardColumnCountSchema is z.number(), not z.number().int()).
func (d *inputDecoder) optNumber(key string, min, max float64) (float64, bool, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	n, ok := v.(float64)
	if !ok {
		return 0, false, badRequestf(`expected a number for input key "%s"`, key)
	}
	if n < min || n > max {
		return 0, false, badRequestf(`input key "%s" must be between %v and %v`, key, min, max)
	}
	return n, true, nil
}

// bool returns a required boolean field.
func (d *inputDecoder) bool(key string) (bool, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok {
		return false, badRequestf(`expected a boolean for input key "%s"`, key)
	}
	b, ok := v.(bool)
	if !ok {
		return false, badRequestf(`expected a boolean for input key "%s"`, key)
	}
	return b, nil
}

// optBool returns an optional boolean field.
func (d *inputDecoder) optBool(key string) (bool, bool, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok || v == nil {
		return false, false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, false, badRequestf(`expected a boolean for input key "%s"`, key)
	}
	return b, true, nil
}

// strList returns a required array of strings.
func (d *inputDecoder) strList(key string) ([]string, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok {
		return nil, badRequestf(`expected an array for input key "%s"`, key)
	}
	return stringList(v, key)
}

func stringList(v any, key string) ([]string, *trpc.Error) {
	list, ok := v.([]any)
	if !ok {
		return nil, badRequestf(`expected an array for input key "%s"`, key)
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil, badRequestf(`expected strings inside input key "%s"`, key)
		}
		out = append(out, s)
	}
	return out, nil
}

// strListDefault returns an array of strings with a default when the key is
// absent (zod .default([])).
func (d *inputDecoder) strListDefault(key string, def []string) ([]string, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok || v == nil {
		return def, nil
	}
	return stringList(v, key)
}

// enum validates that a string is one of the allowed values.
func (d *inputDecoder) enum(key string, allowed []string) (string, *trpc.Error) {
	v, err := d.str(key)
	if err != nil {
		return "", err
	}
	for _, a := range allowed {
		if a == v {
			return v, nil
		}
	}
	return "", badRequestf(`input key "%s" has an invalid value`, key)
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

// numberToFloat64 accepts any JSON number.
func numberToFloat64(v any) (float64, bool) {
	n, ok := v.(float64)
	return n, ok
}

// stringPtr returns a nullable string field: JSON null and absent both give
// (nil, true). A non-string non-null value is a validation error.
func stringPtr(v any, key string) (*string, bool, *trpc.Error) {
	if v == nil {
		return nil, true, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, false, badRequestf(`expected a string or null for input key "%s"`, key)
	}
	return &s, true, nil
}

// --- id generation -----------------------------------------------------------

// createID generates the application-side ids the upstream procedures create
// with createId() (@homarr/common, cuid2): lowercase alphanumeric. cuid2 ids
// are 24 characters; the exact generator is not load-bearing for the wire
// contract (ids are opaque strings for the frontend), but tests rely on ids
// starting with a letter to stay SQLite-binder friendly.
func createID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	const length = 24
	out := make([]byte, length)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			// crypto/rand failing is unrecoverable here; procedures would
			// misbehave with duplicate ids downstream.
			panic("board: createID: " + err.Error())
		}
		out[i] = alphabet[n.Int64()]
	}
	// Keep the first character alphabetic.
	out[0] = alphabet[int(out[0]-'0')%26]
	return string(out)
}

// --- per-key mutex -----------------------------------------------------------

// keyedMutex serializes work per string key (the upstream
// boardItemPlacementTails promise chain serializes item placements per board).
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{locks: map[string]*sync.Mutex{}}
}

func (m *keyedMutex) lock(key string) func() {
	m.mu.Lock()
	if m.locks == nil {
		m.locks = map[string]*sync.Mutex{}
	}
	l, ok := m.locks[key]
	if !ok {
		l = &sync.Mutex{}
		m.locks[key] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// containsString reports whether the list contains v.
func containsString(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
