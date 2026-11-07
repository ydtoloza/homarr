package docker_test

import (
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/integrations/docker"
)

// Stats calculation tests — the port of the calculateCpuUsage /
// calculateMemoryUsage cases in packages/request-handler/src/test/docker.spec.ts.

func statsWith(onlineCPUs int, totalUsage, systemUsage uint64) docker.ContainerStats {
	var s docker.ContainerStats
	s.CPUStats.OnlineCPUs = onlineCPUs
	s.CPUStats.CPUUsage.TotalUsage = totalUsage
	s.CPUStats.SystemCPUUsage = systemUsage
	return s
}

func TestCalculateCPUUsage(t *testing.T) {
	t.Run("zero when cpu_stats missing (Podman)", func(t *testing.T) {
		if got := docker.CalculateCPUUsage(docker.ContainerStats{}, nil); got != 0 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("zero when online_cpus is 0", func(t *testing.T) {
		if got := docker.CalculateCPUUsage(statsWith(0, 1000, 5000), nil); got != 0 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("zero when total_usage is 0", func(t *testing.T) {
		if got := docker.CalculateCPUUsage(statsWith(4, 0, 5000), nil); got != 0 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("zero when system_cpu_usage is 0", func(t *testing.T) {
		if got := docker.CalculateCPUUsage(statsWith(4, 1000, 0), nil); got != 0 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("valid stats", func(t *testing.T) {
		// (2000 / 10000) * 100 = 20
		if got := docker.CalculateCPUUsage(statsWith(4, 2000, 10000), nil); got != 20 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("fractional usage", func(t *testing.T) {
		got := docker.CalculateCPUUsage(statsWith(4, 1500, 10000), nil)
		if got != 15 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("precpu baseline preferred", func(t *testing.T) {
		s := statsWith(4, 3000, 10000)
		s.PreCPUStats.CPUUsage.TotalUsage = 2000
		s.PreCPUStats.SystemCPUUsage = 8000
		// ((3000-2000) / (10000-8000)) * 100 = 50
		if got := docker.CalculateCPUUsage(s, nil); got != 50 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("previous counters as one-shot fallback baseline", func(t *testing.T) {
		// One-shot responses carry zeroed precpu_stats; the previous poll's
		// counters are the baseline (docker.ts:352-356).
		s := statsWith(4, 3000, 10000)
		previous := &docker.CpuUsageCounters{TotalUsage: 1000, SystemUsage: 5000}
		// ((3000-1000) / (10000-5000)) * 100 = 40
		if got := docker.CalculateCPUUsage(s, previous); got != 40 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("negative deltas yield 0", func(t *testing.T) {
		s := statsWith(4, 500, 10000)
		previous := &docker.CpuUsageCounters{TotalUsage: 1000, SystemUsage: 5000}
		if got := docker.CalculateCPUUsage(s, previous); got != 0 {
			t.Fatalf("cpuDelta < 0: got %v", got)
		}
		s2 := statsWith(4, 3000, 4000)
		if got := docker.CalculateCPUUsage(s2, previous); got != 0 {
			t.Fatalf("systemDelta <= 0: got %v", got)
		}
	})
}

func TestCalculateMemoryUsage(t *testing.T) {
	t.Run("zero when usage missing", func(t *testing.T) {
		if got := docker.CalculateMemoryUsage(docker.ContainerStats{}); got != 0 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("cache subtracted (cgroup v1)", func(t *testing.T) {
		var s docker.ContainerStats
		s.MemoryStats.Usage = 1024
		cache := uint64(512)
		s.MemoryStats.Stats.Cache = &cache
		if got := docker.CalculateMemoryUsage(s); got != 512 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("total_inactive_file fallback (cgroup v2)", func(t *testing.T) {
		var s docker.ContainerStats
		s.MemoryStats.Usage = 1024
		totalInactive := uint64(700)
		s.MemoryStats.Stats.TotalInactiveFile = &totalInactive
		if got := docker.CalculateMemoryUsage(s); got != 324 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("inactive_file fallback (Podman)", func(t *testing.T) {
		var s docker.ContainerStats
		s.MemoryStats.Usage = 1024
		inactive := uint64(1024)
		s.MemoryStats.Stats.InactiveFile = &inactive
		if got := docker.CalculateMemoryUsage(s); got != 0 {
			t.Fatalf("got %v", got)
		}
	})
}

func TestStatsCacheTTL(t *testing.T) {
	now := time.Unix(0, 0)
	sc := docker.NewStatsCache(func() time.Time { return now })

	sc.Set("ep:container", docker.CpuUsageCounters{TotalUsage: 10, SystemUsage: 20})

	if got, ok := sc.Get("ep:container"); !ok || got.TotalUsage != 10 {
		t.Fatalf("Get = %+v ok=%v", got, ok)
	}

	// Entries expire after 10 minutes (cpuUsageCountersCacheTtlMs).
	now = now.Add(docker.StatsCacheTTL)
	if _, ok := sc.Get("ep:container"); ok {
		t.Fatalf("expired entry still cached")
	}
	if _, ok := sc.Get("ep:container"); ok {
		t.Fatalf("expired entry should have been evicted")
	}
}

func TestExtractContainerImageName(t *testing.T) {
	cases := map[string]string{
		"lscr.io/linuxserver/sonarr:latest": "sonarr",
		"ghcr.io/homarr-labs/homarr":        "homarr",
		"nginx":                             "nginx",
		"registry:5000/app:v1.2":            "app",
	}
	for image, want := range cases {
		if got := docker.ExtractContainerImageName(image); got != want {
			t.Fatalf("%q: got %q, want %q", image, got, want)
		}
	}
}

func TestGetReleaseSourceFromDockerImage(t *testing.T) {
	cases := []struct {
		image      string
		provider   docker.ReleaseProviderKind
		identifier string
		nil        bool
	}{
		{"lscr.io/linuxserver/sonarr:latest", docker.ReleaseProviderLinuxServerIO, "linuxserver/sonarr", false},
		{"lscr.io/linuxserver/sonarr:v4.5", docker.ReleaseProviderLinuxServerIO, "linuxserver/sonarr", false},
		// Non-version tags for LinuxServer are dropped (release-provider.ts:111).
		{"lscr.io/linuxserver/sonarr:nightly", "", "", true},
		{"ghcr.io/homarr-labs/homarr:latest", docker.ReleaseProviderGitHubContainerRegistry, "homarr-labs/homarr", false},
		{"nginx:latest", docker.ReleaseProviderDockerHub, "nginx", false},
		{"docker.io/library/nginx:1.25", docker.ReleaseProviderDockerHub, "library/nginx", false},
		{"quay.io/argoproj/workflow-controller:v3.5.0", docker.ReleaseProviderQuay, "argoproj/workflow-controller", false},
		{"myregistry.com/app:v2", docker.ReleaseProviderDockerHub, "myregistry.com/app", false},
		// "2024.1.0" is version-like (\d\.\d matches), so it stays in the
		// version field and the identifier keeps no track suffix
		// (release-provider.ts:116-119).
		{"ghcr.io/owner/app:2024.1.0", docker.ReleaseProviderGitHubContainerRegistry, "owner/app", false},
		// A non-version-like version on a tracked provider becomes a track
		// suffix on the identifier.
		{"ghcr.io/owner/app:stable", docker.ReleaseProviderGitHubContainerRegistry, "owner/app:stable", false},
	}
	for _, tc := range cases {
		src := docker.GetReleaseSourceFromDockerImage(tc.image)
		if tc.nil {
			if src != nil {
				t.Fatalf("%q: want nil, got %+v", tc.image, src)
			}
			continue
		}
		if src == nil || src.Provider != tc.provider || src.Identifier != tc.identifier {
			t.Fatalf("%q: got %+v, want %s/%s", tc.image, src, tc.provider, tc.identifier)
		}
	}
}

func TestReleaseSourceToken(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	token := docker.ReleaseSourceToken("ghcr.io/homarr-labs/homarr:latest", key)
	if token == "" {
		t.Fatalf("empty token")
	}
	if token != docker.CreateKeyedFingerprint(`["releases-docker-suggestion-v1","gitHubContainerRegistry","homarr-labs/homarr"]`, key) {
		t.Fatalf("token mismatch: %s", token)
	}
	// Images without a release source (lscr with a non-version tag) carry no
	// token; plain names still map to dockerHub like upstream.
	if docker.ReleaseSourceToken("lscr.io/linuxserver/sonarr:nightly", key) != "" {
		t.Fatalf("lscr nightly must not produce a token")
	}
	if docker.ReleaseSourceToken("", key) != "" {
		t.Fatalf("empty image must not produce a token")
	}
}
