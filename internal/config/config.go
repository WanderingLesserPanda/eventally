// Package config loads runtime configuration from environment variables.
package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Config holds the process-wide runtime configuration.
type Config struct {
	// Addr is the listen address (host:port).
	Addr string
	// DSN is the Postgres connection string actually used to connect.
	// It is always emitted in keyword/value form with the password
	// single-quote-escaped, so a password may contain any character —
	// including #, % and ^, which break the URL (postgres://) form.
	DSN string
	// PG holds the structured Postgres connection parameters that were
	// resolved (from EVENTALLY_PG_* and/or EVENTALLY_DSN).
	PG Postgres
	// Secret is a 256-bit random key (64 hex chars). Currently reserved
	// for the planned authentik/OIDC layer (D16) — validated at startup,
	// not yet consumed by any crypto. Sessions and CSRF tokens are
	// DB-backed random values (see internal/auth).
	Secret [32]byte
	// DataDir holds uploaded receipts and other user files.
	DataDir string
}

// Postgres is the structured connection spec.
type Postgres struct {
	Host     string
	Port     string
	Database string
	User     string
	Password string
}

// Load reads configuration from the environment. Every failure is fatal:
// the app should not silently run with a guessed secret or DSN.
func Load() (*Config, error) {
	c := &Config{
		Addr:    env("EVENTALLY_ADDR", "127.0.0.1:8090"),
		DataDir: env("EVENTALLY_DATA_DIR", "./data"),
	}
	secret := os.Getenv("EVENTALLY_SECRET")
	if secret == "" {
		return nil, fmt.Errorf("EVENTALLY_SECRET must be set (generate: openssl rand -hex 32)")
	}
	if len(secret) != 64 {
		return nil, fmt.Errorf("EVENTALLY_SECRET must be 64 hex characters, got %d", len(secret))
	}
	decoded, err := hex.DecodeString(secret)
	if err != nil {
		return nil, fmt.Errorf("EVENTALLY_SECRET must be hex: %w", err)
	}
	// Store the decoded 32 bytes (256 bits), not the 64 ASCII hex
	// characters — the field is a byte array meant to hold a 256-bit key.
	copy(c.Secret[:], decoded)

	pg, err := resolvePostgres()
	if err != nil {
		return nil, err
	}
	c.PG = pg
	c.DSN = buildDSN(pg)
	return c, nil
}

// resolvePostgres determines the effective Postgres connection spec.
//
// Precedence (highest wins):
//  1. Explicit EVENTALLY_PG_HOST / _PORT / _DATABASE / _USER / _PASSWORD.
//  2. Fields parsed from EVENTALLY_DSN (URL or keyword/value).
//  3. Built-in defaults.
//
// The structured variables exist so a password may contain any character:
// they are passed through verbatim and only escaped when the final DSN is
// built. EVENTALLY_DSN is parsed for its fields but, if it is in URL form
// with a special-character password, that password would be lossy — so use
// EVENTALLY_PG_PASSWORD for such passwords.
func resolvePostgres() (Postgres, error) {
	pg := Postgres{
		Host:     "127.0.0.1",
		Port:     "5432",
		Database: "eventally",
		User:     "eventally",
		Password: "change-me",
	}

	if dsn := os.Getenv("EVENTALLY_DSN"); dsn != "" {
		if cc, err := pgx.ParseConfig(dsn); err == nil {
			pg.Host = cc.Host
			pg.Port = strconv.Itoa(int(cc.Port))
			pg.Database = cc.Database
			pg.User = cc.User
			pg.Password = cc.Password
		} else if os.Getenv("EVENTALLY_PG_PASSWORD") == "" {
			// A URL-form DSN whose password contains #, %, or ^ fails to
			// parse; the structured variables are the way in for those.
			return pg, fmt.Errorf("EVENTALLY_DSN could not be parsed (%v); if its password contains characters such as #, %% or ^, supply the connection via EVENTALLY_PG_HOST/PORT/DATABASE/USER/PASSWORD instead", err)
		}
	}

	if v := os.Getenv("EVENTALLY_PG_HOST"); v != "" {
		pg.Host = v
	}
	if v := os.Getenv("EVENTALLY_PG_PORT"); v != "" {
		pg.Port = v
	}
	if v := os.Getenv("EVENTALLY_PG_DATABASE"); v != "" {
		pg.Database = v
	}
	if v := os.Getenv("EVENTALLY_PG_USER"); v != "" {
		pg.User = v
	}
	if v := os.Getenv("EVENTALLY_PG_PASSWORD"); v != "" {
		pg.Password = v
	}

	if err := validatePassword(pg.Password); err != nil {
		return pg, err
	}
	return pg, nil
}

// buildDSN renders the connection spec as a keyword/value string.
// The password is single-quote-wrapped with any embedded single quotes
// doubled (libpq escaping), which the pgx keyword/value parser round-trips
// verbatim for the full typical password character set.
//
// Known limitation: pgx's keyword/value parser ends a quoted value at the
// next single quote (no '' escape), so a password containing a single
// quote cannot be expressed in the DSN form. That character is outside
// the supported set; if it is ever required, switch to a pgx.ConnConfig
// built directly from the structured fields.
func buildDSN(pg Postgres) string {
	return "host=" + kv(pg.Host) +
		" port=" + kv(pg.Port) +
		" user=" + kv(pg.User) +
		" dbname=" + kv(pg.Database) +
		" password=" + quote(pg.Password)
}

// kv quotes a value only if it contains a space (host/port/user/db are
// expected to be plain tokens).
func kv(v string) string {
	if strings.ContainsAny(v, " \t'") {
		return quote(v)
	}
	return v
}

// quote wraps v in single quotes, doubling any embedded single quote.
func quote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// validatePassword enforces the password policy when a password is set:
// 8-64 characters. An empty password is allowed — it is the local
// trust/socket-auth path used by development. The typical password
// special characters (!@#$%^&*+-) are explicitly supported: buildDSN
// emits a single-quote-escaped keyword/value DSN, so they round-trip
// verbatim instead of breaking the URL form. (No "must contain a special
// character" rule is imposed — the requirement is that such passwords
// *work*, not that they *must* be present.)
func validatePassword(pw string) error {
	if pw == "" {
		return nil
	}
	if len(pw) < 8 || len(pw) > 64 {
		return fmt.Errorf("Postgres password must be 8-64 characters, got %d (set EVENTALLY_PG_PASSWORD)", len(pw))
	}
	return nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
