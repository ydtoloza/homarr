// Package config loads homarr-go configuration from the environment,
// mirroring the upstream Homarr env var conventions where possible.
package config

import (
	"os"
	"path/filepath"
	"strconv"
)

// Config holds the runtime configuration of homarr-go.
type Config struct {
	// Port is the HTTP listen port (upstream default: 7575).
	Port string
	// DataDir holds the SQLite database and mutable state (upstream: /appdata).
	DataDir string
}

// Load reads configuration from HOMARR_* environment variables.
func Load() Config {
	return Config{
		Port:    envOr("HOMARR_PORT", "7575"),
		DataDir: envOr("HOMARR_DATA_DIR", defaultDataDir()),
	}
}

// MemLimitBytes returns the soft memory limit in bytes when
// HOMARR_GOMEMLIMIT is set (same semantics as GOMEMLIMIT).
func (c Config) MemLimitBytes() (int64, bool) {
	raw := os.Getenv("HOMARR_GOMEMLIMIT")
	if raw == "" {
		return 0, false
	}
	limit, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || limit <= 0 {
		return 0, false
	}
	return limit, true
}

// DBPath is the SQLite database path inside DataDir, matching the
// upstream layout (/appdata/db/db.sqlite) so an existing Homarr
// database can be opened as-is.
func (c Config) DBPath() string {
	return filepath.Join(c.DataDir, "db", "db.sqlite")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func defaultDataDir() string {
	if st, err := os.Stat("/appdata"); err == nil && st.IsDir() {
		return "/appdata"
	}
	return "./appdata"
}
