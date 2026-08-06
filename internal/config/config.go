package config

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	ListenAddr string
	DBPath     string
	// Secret seeds the AES-GCM key used to encrypt provider API keys at rest.
	Secret string
	// DebugLevel controls terminal logging verbosity (slog text):
	//   0 Info (lifecycle/warnings/operational errors);
	//   1 Debug (+ access/upstream/failover/OAuth metadata; no bodies);
	//   2 Trace (+ detailed ingress/probe exchange metadata; no bodies).
	// Bodies require HAR capture (independent of debug level): runtime Start/Stop
	// from the dashboard, or always-on -har-file mode.
	DebugLevel int
	// HARFile, when set, enables always-on MitM-style HAR 1.2 capture of both legs
	// of every proxied request (headers and bodies up to the HAR per-body cap).
	// The live document is served at GET /debug/har and flushed to this path on
	// shutdown. Empty enables runtime dashboard-controlled capture instead (in
	// memory only; never written on shutdown). HAR contains sensitive
	// prompt/credential material.
	HARFile string
	// Version, when true, prints the build version and exits.
	Version bool
}

// debugLevel is a flag.Value backing -debug. It accepts the historical boolean
// spellings (-debug, -debug=true) as level 1 and numeric levels (-debug=2) for
// higher verbosity, so existing AIROUTER_DEBUG=true usage keeps working.
type debugLevel int

func (d *debugLevel) String() string { return strconv.Itoa(int(*d)) }

func (d *debugLevel) Set(s string) error {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "true", "yes", "on":
		*d = 1
	case "false", "no", "off":
		*d = 0
	default:
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("invalid debug level %q", s)
		}
		*d = debugLevel(n)
	}
	return nil
}

// IsBoolFlag lets the flag parser accept a bare -debug (no value) as level 1
// while still allowing -debug=2.
func (d *debugLevel) IsBoolFlag() bool { return true }

// devSecret is used only when no secret is supplied, so the binary runs out of
// the box. It is insecure by design: a fixed key means anyone with the DB file
// can decrypt the stored API keys. A warning is logged when this path is taken.
const devSecret = "airouter-insecure-dev-secret"

func Load() Config {
	c := Config{}
	flag.StringVar(&c.ListenAddr, "listen", env("AIROUTER_LISTEN", ":31415"), "HTTP listen address")
	flag.StringVar(&c.DBPath, "db", env("AIROUTER_DB", "airouter.db"), "SQLite database path")
	flag.StringVar(&c.Secret, "secret", env("AIROUTER_SECRET", ""), "secret seeding the at-rest encryption key")
	level := debugLevel(envDebugLevel())
	flag.Var(&level, "debug", "log verbosity: 1=access and upstream diagnostics, 2=detailed exchange metadata; bodies require HAR capture (dashboard or -har-file)")
	flag.StringVar(&c.HARFile, "har-file", env("AIROUTER_HAR_FILE", ""), "always-on HAR capture of proxied request/response pairs (both legs, headers and bodies up to the HAR per-body cap); live at GET /debug/har and flushed to this path on shutdown. Without this flag, use dashboard Settings to start/stop runtime capture. Contains prompt content and provider secrets")
	flag.BoolVar(&c.Version, "version", false, "print version and exit")
	flag.Parse()
	c.DebugLevel = int(level)
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

// envDebugLevel reads AIROUTER_DEBUG, accepting both boolean spellings (mapped
// to level 1) and explicit numeric levels.
func envDebugLevel() int {
	v := strings.TrimSpace(os.Getenv("AIROUTER_DEBUG"))
	switch strings.ToLower(v) {
	case "":
		return 0
	case "true", "yes", "on":
		return 1
	case "false", "no", "off":
		return 0
	default:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		return 0
	}
}
