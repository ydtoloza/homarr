package logging

// In-package retention test: it shrinks HistoryMaxEntries/retentionInterval
// (the real caps are LOG_HISTORY_MAX_ENTRIES = 5000 and a 250-insert trim
// interval) so the LPUSH+LTRIM semantics run in milliseconds.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/db"
)

func TestSQLiteStoreRetention(t *testing.T) {
	oldMax, oldInterval := HistoryMaxEntries, retentionInterval
	HistoryMaxEntries, retentionInterval = 50, 10
	defer func() { HistoryMaxEntries, retentionInterval = oldMax, oldInterval }()

	d := db.OpenTest(t)
	ctx := context.Background()
	store, err := NewSQLiteStore(ctx, d)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const total = 120 // 120 inserts against a 50-row cap
	for i := 0; i < total; i++ {
		e := Entry{
			ID:        fmt.Sprintf("id-%04d", i),
			Timestamp: base.Add(time.Duration(i) * time.Millisecond),
			Message:   fmt.Sprintf("line %d", i),
			Level:     LevelInfo,
		}
		if err := store.Save(ctx, e); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}

	var count int
	if err := d.QueryRow(`SELECT COUNT(*) FROM "log"`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	// The trim runs once per 10 inserts and keeps the newest 50, so at most
	// 59 rows survive (50 + the 9 inserts after the last trim).
	if count > HistoryMaxEntries+retentionInterval-1 {
		t.Fatalf("retention failed: %d rows", count)
	}
	if count < HistoryMaxEntries {
		t.Fatalf("overzealous trim: %d rows", count)
	}

	// The OLDEST entries are gone.
	oldest, err := store.Recent(ctx, nil, count)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if oldest[0].ID != fmt.Sprintf("id-%04d", total-count) {
		t.Errorf("oldest survivor = %s, want id-%04d", oldest[0].ID, total-count)
	}
	if last := oldest[len(oldest)-1].ID; last != fmt.Sprintf("id-%04d", total-1) {
		t.Errorf("newest = %s, want id-%04d", last, total-1)
	}
}
