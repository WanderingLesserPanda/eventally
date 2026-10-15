// Package config loads runtime configuration from environment variables.
package config

import (
	"encoding/hex"
	"fmt"
	"os"
)

// Config holds the process-wide runtime configuration.
type Config struct {
	// Addr is the listen address (host:port).
	Addr string
	// DSN is the Postgres connection string.
	DSN string
	// Secret signs session cookies and session token hashes.
	Secret [32]byte
	// DataDir holds uploaded receipts and other user files.
	DataDir string
}

// Load reads configuration from the environment. Every failure is fatal:
// the app should not silently run with a guessed secret or DSN.
func Load() (*Config, error) {
	c := &Config{
		Addr:    env("EVENTALLY_ADDR", "127.0.0.1:8090"),
		DSN:     env("EVENTALLY_DSN", "postgres://eventally:eventally@127.0.0.1:5432/eventally"),
		DataDir: env("EVENTALLY_DATA_DIR", "./data"),
	}
	secret := os.Getenv("EVENTALLY_SECRET")
	if secret == "" {
		return nil, fmt.Errorf("EVENTALLY_SECRET must be set (generate: openssl rand -hex 32)")
	}
	if len(secret) != 64 {
		return nil, fmt.Errorf("EVENTALLY_SECRET must be 64 hex characters, got %d", len(secret))
	}
	if _, err := hex.DecodeString(secret); err != nil {
		return nil, fmt.Errorf("EVENTALLY_SECRET must be hex: %w", err)
	}
	copy(c.Secret[:], secret)
	return c, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
