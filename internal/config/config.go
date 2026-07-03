package config

import "os"

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

type Config struct {
	DBPath      string
	Listen      string
	MasterKey   string
	Environment string
}

func Load() Config {
	env := getEnv("KATCHUP_ENV", "development")
	return Config{
		DBPath:      getEnv("DB_PATH", "data/katchup.db"),
		Listen:      getEnv("KATCHUP_LISTEN", ":8080"),
		MasterKey:   os.Getenv("KATCHUP_MASTER_KEY"),
		Environment: env,
	}
}
