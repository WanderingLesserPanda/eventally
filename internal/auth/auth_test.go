// Package auth: tests for the password hashing round-trip.
package auth

import (
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
