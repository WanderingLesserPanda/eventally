// Package auth: tests for the password hashing round-trip.
package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHashCheckRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if len(hash) < 40 || hash[:10] != "$argon2id$" {
		t.Fatalf("unexpected hash format: %s", hash[:10])
	}
	if err := CheckPassword(hash, "correct horse battery staple"); err != nil {
		t.Fatalf("valid password rejected: %v", err)
	}
	if err := CheckPassword(hash, "wrong password"); err == nil {
		t.Fatal("wrong password accepted")
	}
}

func TestHashUniqueness(t *testing.T) {
	h1, _ := HashPassword("same-password")
	h2, _ := HashPassword("same-password")
	if h1 == h2 {
		t.Fatal("two hashes of the same password are identical (salt not random?)")
	}
}

// TestPostLoginDest pins the 2026-11-20 field fix: a browser submitting the
// form at /login always sends Referer: <site>/login, and honouring that
// referer bounced users back to the login form. Auth-page referers must
// fall back to the dashboard; other same-site referers still are honoured.
func TestPostLoginDest(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		referer string
		want    string
	}{
		{"no referer", "example.test", "", "/"},
		{"browser case: referer is the login form", "example.test", "http://example.test/login", "/"},
		{"login referer with query (failed attempt)", "example.test", "http://example.test/login?err=1", "/"},
		{"setup referer", "example.test", "http://example.test/setup", "/"},
		{"same-site protected page", "example.test", "http://example.test/txns", "http://example.test/txns"},
		{"cross-site referer", "example.test", "http://evil.example/txns", "/"},
		{"reverse proxy rewrites Host (referer host differs)", "127.0.0.1:8090", "http://nas.local/login", "/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/login", nil)
			r.Host = tc.host
			if tc.referer != "" {
				r.Header.Set("Referer", tc.referer)
			}
			if got := PostLoginDest(r); got != tc.want {
				t.Fatalf("PostLoginDest() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCheckPasswordMalformed(t *testing.T) {
	for _, bad := range []string{"", "garbage", "$argon2id$v=19", "$argon2id$v=19$m=1,t=1,p=4$"} {
		if err := CheckPassword(bad, "x"); err == nil {
			t.Errorf("CheckPassword(%q) accepted malformed hash", bad)
		}
	}
}

func TestToken(t *testing.T) {
	a, err := Token()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Token()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("tokens are not random")
	}
	if len(a) != 43 { // 32 bytes base64url raw = 43 chars
		t.Fatalf("unexpected token length %d", len(a))
	}
}
