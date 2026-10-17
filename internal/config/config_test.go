package config

import (
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// secret is a valid 64-char hex value for EVENTALLY_SECRET.
const secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	// Clear the whole PG set first so ambient env can't leak in.
	for _, k := range []string{
		"EVENTALLY_SECRET", "EVENTALLY_DSN", "EVENTALLY_ADDR", "EVENTALLY_DATA_DIR",
		"EVENTALLY_PG_HOST", "EVENTALLY_PG_PORT", "EVENTALLY_PG_DATABASE",
		"EVENTALLY_PG_USER", "EVENTALLY_PG_PASSWORD",
	} {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		if v == "" {
			os.Unsetenv(k)
		} else {
			t.Setenv(k, v)
		}
	}
}

func TestLoadStructuredPasswordRoundTrip(t *testing.T) {
	// Every character the app must accept, at once.
	pw := "a!b@c#d$e%f^g&h*i+j-k"
	setEnv(t, map[string]string{
		"EVENTALLY_SECRET":      secret,
		"EVENTALLY_PG_HOST":     "db",
		"EVENTALLY_PG_PORT":     "5432",
		"EVENTALLY_PG_DATABASE": "eventally",
		"EVENTALLY_PG_USER":     "eventally",
		"EVENTALLY_PG_PASSWORD": pw,
	})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DSN == "" {
		t.Fatal("empty DSN")
	}
	cc, err := pgxpool.ParseConfig(c.DSN)
	if err != nil {
		t.Fatalf("ParseConfig(%q): %v", c.DSN, err)
	}
	if got := cc.ConnConfig.Password; got != pw {
		t.Fatalf("password round-trip: got %q want %q", got, pw)
	}
	if cc.ConnConfig.Host != "db" || cc.ConnConfig.Database != "eventally" || cc.ConnConfig.User != "eventally" {
		t.Fatalf("host/db/user: %q %q %q", cc.ConnConfig.Host, cc.ConnConfig.Database, cc.ConnConfig.User)
	}
}

func TestValidatePassword(t *testing.T) {
	// Empty password = local trust/socket auth; allowed.
	if err := validatePassword(""); err != nil {
		t.Errorf("empty password (trust auth) should pass: %v", err)
	}
	if err := validatePassword("a!23456"); err == nil {
		t.Error("7-char password should be rejected")
	}
	if err := validatePassword("a!234567"); err != nil {
		t.Errorf("8-char (min) password should pass: %v", err)
	}
	if err := validatePassword("a!" + strings.Repeat("x", 62)); err != nil {
		t.Errorf("64-char (max) password should pass: %v", err)
	}
	if err := validatePassword("a!" + strings.Repeat("x", 63)); err == nil {
		t.Error("65-char password should be rejected")
	}
	// Specials are supported, not required: an all-alphanumeric password
	// of valid length must pass (e.g. openssl rand -hex output).
	if err := validatePassword("nospecials1"); err != nil {
		t.Errorf("all-alnum 8-char password should pass: %v", err)
	}
}
