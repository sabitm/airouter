package config

import (
	"flag"
	"os"
)

type Config struct {
	ListenAddr string
	DBPath     string
	// Secret seeds the AES-GCM key used to encrypt provider API keys at rest.
	Secret string
	// Debug logs failed/error upstream exchanges to the terminal.
	Debug bool
}

// devSecret is used only when no secret is supplied, so the binary runs out of
// the box. It is insecure by design: a fixed key means anyone with the DB file
// can decrypt the stored API keys. A warning is logged when this path is taken.
const devSecret = "airouter-insecure-dev-secret"

func Load() Config {
	c := Config{}
	flag.StringVar(&c.ListenAddr, "listen", env("AIROUTER_LISTEN", ":8080"), "HTTP listen address")
	flag.StringVar(&c.DBPath, "db", env("AIROUTER_DB", "airouter.db"), "SQLite database path")
	flag.StringVar(&c.Secret, "secret", env("AIROUTER_SECRET", ""), "secret seeding the at-rest encryption key")
	flag.BoolVar(&c.Debug, "debug", envBool("AIROUTER_DEBUG"), "log failed/error upstream exchanges to the terminal")
	flag.Parse()
	return c
}

// EffectiveSecret returns the configured secret, or the dev fallback together
// with a flag indicating the insecure path was taken.
func (c Config) EffectiveSecret() (secret string, isDev bool) {
	if c.Secret == "" {
		return devSecret, true
	}
	return c.Secret, false
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string) bool {
	switch os.Getenv(key) {
	case "1", "true", "TRUE", "yes":
		return true
	}
	return false
}
