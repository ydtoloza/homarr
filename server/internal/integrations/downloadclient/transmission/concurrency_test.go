package transmission_test

import (
	"context"
	"sync"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient"
	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient/transmission"
)

// TestConcurrentCallsShareTheSession hammers one client from many goroutines
// (session id guarded by a mutex; -race in CI is authoritative).
func TestConcurrentCallsShareTheSession(t *testing.T) {
	f, srv := newFakeTransmission(t)
	f.torrents = fixtureTorrents
	client := transmission.New(testInput(srv.URL), transmission.Secrets{Username: "admin", Password: "secret"})
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
}
