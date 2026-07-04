package config

import (
	"log/slog"
	"os"
	"time"
)

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

// getDurationEnv parses a Go duration (e.g. "6h", "30s") from the environment,
// falling back to defaultVal when unset or unparseable.
func getDurationEnv(key string, defaultVal time.Duration) time.Duration {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal
	}
	d, err := time.ParseDuration(val)
	if err != nil {
		slog.Warn("invalid duration env var, using default", "key", key, "value", val, "default", defaultVal)
		return defaultVal
	}
	return d
}

type Config struct {
	DBPath      string
	Listen      string
	MasterKey   string
	Environment string
	// APIToken guards the Hermes-facing /api/* routes. If empty, every /api/*
	// request fails closed with 503 (never silently open).
	APIToken string
	// SyncInterval is the scheduled-sync safety-net floor. Hermes drives
	// freshness via POST /api/sync; this guarantees a backup even if Hermes is
	// down. Configurable via KATCHUP_SYNC_INTERVAL (default 6h).
	SyncInterval time.Duration
	// CoalesceWindow is how long after a run finishes a new POST /api/sync
	// trigger is coalesced into that run instead of starting another. Configurable
	// via KATCHUP_COALESCE_WINDOW (default 30s).
	CoalesceWindow time.Duration
	// MeiliURL / MeiliKey configure the Meilisearch backend for header-only search.
	// If MeiliURL is empty, /search degrades to a SQLite LIKE over subject/from —
	// search is never a hard dependency.
	MeiliURL string
	MeiliKey string
}

func Load() Config {
	env := getEnv("KATCHUP_ENV", "development")
	return Config{
		DBPath:         getEnv("DB_PATH", "data/katchup.db"),
		Listen:         getEnv("KATCHUP_LISTEN", ":8080"),
		MasterKey:      os.Getenv("KATCHUP_MASTER_KEY"),
		Environment:    env,
		APIToken:       os.Getenv("KATCHUP_API_TOKEN"),
		SyncInterval:   getDurationEnv("KATCHUP_SYNC_INTERVAL", 6*time.Hour),
		CoalesceWindow: getDurationEnv("KATCHUP_COALESCE_WINDOW", 30*time.Second),
		MeiliURL:       os.Getenv("MEILI_URL"),
		MeiliKey:       os.Getenv("MEILI_KEY"),
	}
}
