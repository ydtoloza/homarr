package qbittorrent_test

import (
	"context"
	"sync"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient"
	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient/qbittorrent"
)

// TestConcurrentCallsShareTheSession hammers one client from many goroutines:
// the session state (sid/version) is guarded, so calls either share the
// initial login or trigger additional ones without corrupting the state
// (the -race run of CI is the authoritative check).
func TestConcurrentCallsShareTheSession(t *testing.T) {
	f, srv := newFakeQbit(t, "v5.0.2")
	client := qbittorrent.New(testInput(srv.URL), loginSecrets())
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.GetJobsAndStatus(context.Background(), downloadclient.JobsInput{Limit: 5}); err != nil {
				t.Errorf("GetJobsAndStatus: %v", err)
			}
		}()
	}
	wg.Wait()
	if len(f.requests) < 16 {
		t.Fatalf("requests = %d", len(f.requests))
	}
}
