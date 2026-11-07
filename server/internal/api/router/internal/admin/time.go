package admin

import (
	"time"
)

// Date wire format.
//
// Upstream serializes JS Date values through superjson, which emits
// date.toISOString() plus a meta annotation. The homarr-go trpc package
// intentionally emits plain JSON (dates travel as ISO strings without the
// Date meta; see trpc/superjson.go) — these helpers produce the same string
// superjson would: UTC with milliseconds and the Z suffix
// (Date.prototype.toISOString).

// ISOString renders t like Date.prototype.toISOString().
func ISOString(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// ISOStringOrNull renders t as an ISO string, or JSON nil (null) when the
// timestamp is zero (NULL columns).
func ISOStringOrNull(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return ISOString(t)
}
