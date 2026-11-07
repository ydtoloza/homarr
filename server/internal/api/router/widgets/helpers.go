package widgets

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
)

// Input decoding for the widget procedures, mirroring the upstream zod
// schemas field by field with the house BAD_REQUEST semantics (same messages
// as the shared admin.InputDecoder of router/internal/admin, duplicated here
// so the widget package stays independent of the admin packages).

// decoder walks one decoded JSON input object.
type decoder struct {
	obj map[string]any
}

// newDecoder decodes one procedure input (it arrives superjson-unwrapped
// from trpc; null input yields an empty decoder).
func newDecoder(input json.RawMessage) (*decoder, *trpc.Error) {
	if len(input) == 0 || string(input) == "null" {
		return &decoder{obj: map[string]any{}}, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(input, &obj); err != nil {
		return nil, trpc.NewError(trpc.CodeBadRequest, "expected an object for input")
	}
	return &decoder{obj: obj}, nil
}

// Has reports whether the key is present (present-but-null included).
func (d *decoder) Has(key string) bool {
	_, ok := d.obj[key]
	return ok
}

// Raw returns the raw decoded value of a key.
func (d *decoder) Raw(key string) (any, bool) {
	v, ok := d.obj[key]
	return v, ok
}

// Str returns a required string field.
func (d *decoder) Str(key string) (string, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok {
		return "", trpc.NewError(trpc.CodeBadRequest, fmt.Sprintf(`expected a string for input key "%s"`, key))
	}
	s, ok := v.(string)
	if !ok {
		return "", trpc.NewError(trpc.CodeBadRequest, fmt.Sprintf(`expected a string for input key "%s"`, key))
	}
	return s, nil
}

// StrList returns a required array of strings.
func (d *decoder) StrList(key string) ([]string, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok {
		return nil, trpc.NewError(trpc.CodeBadRequest, fmt.Sprintf(`expected an array for input key "%s"`, key))
	}
	list, ok := v.([]any)
	if !ok {
		return nil, trpc.NewError(trpc.CodeBadRequest, fmt.Sprintf(`expected an array for input key "%s"`, key))
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil, trpc.NewError(trpc.CodeBadRequest, fmt.Sprintf(`expected strings inside input key "%s"`, key))
		}
		out = append(out, s)
	}
	return out, nil
}

// Bool returns a required boolean field.
func (d *decoder) Bool(key string) (bool, *trpc.Error) {
	v, ok := d.obj[key]
	if !ok {
		return false, trpc.NewError(trpc.CodeBadRequest, fmt.Sprintf(`expected a boolean for input key "%s"`, key))
	}
	b, ok := v.(bool)
	if !ok {
		return false, trpc.NewError(trpc.CodeBadRequest, fmt.Sprintf(`expected a boolean for input key "%s"`, key))
	}
	return b, nil
}

// decoderNumber returns an optional JSON number (missing or null -> ok=false;
// non-number -> ok=false as well, callers decide the error).
func decoderNumber(d *decoder, key string) (float64, bool) {
	v, ok := d.obj[key]
	if !ok || v == nil {
		return 0, false
	}
	n, isNumber := v.(float64)
	return n, isNumber
}

// allQueriesFailed is the settleIntegrationQueries throw
// (packages/api/src/settle-integrations.ts:60-66): INTERNAL_SERVER_ERROR with
// the exact upstream message.
func allQueriesFailed() *trpc.Error {
	return trpc.NewError(trpc.CodeInternalServerError, "All integration queries failed")
}

// runBounded runs fn for every index 0..n-1 with at most parallelism
// concurrent goroutines, preserving result slots by index. When the context
// is cancelled pending jobs still run (their HTTP calls abort via ctx, like
// the upstream Promise.allSettled which cannot be cancelled).
func runBounded(ctx context.Context, parallelism, n int, fn func(idx int)) {
	if n == 0 {
		return
	}
	if parallelism <= 0 || parallelism > n {
		parallelism = n
	}
	var (
		wg   sync.WaitGroup
		next = make(chan int)
	)
	wg.Add(parallelism)
	for worker := 0; worker < parallelism; worker++ {
		go func() {
			defer wg.Done()
			for idx := range next {
				fn(idx)
			}
		}()
	}
	for idx := 0; idx < n; idx++ {
		next <- idx
	}
	close(next)
	wg.Wait()
}

func strPtr(v string) *string { return &v }
