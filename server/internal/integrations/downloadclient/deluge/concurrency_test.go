package deluge_test

import (
	"context"
	"sync"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient"
	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient/deluge"
)

// TestConcurrentCallsShareTheSession hammers one client from many goroutines
// (cookie + msgId guarded by the client mutex; -race in CI is authoritative).
func TestConcurrentCallsShareTheSession(t *testing.T) {
	f, srv := newFakeDeluge(t)
	f.payload = delugePayload
	client := deluge.New(testInput(srv.URL), deluge.Secrets{Password: "p"})
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
