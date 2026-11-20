// Login redirect field-fix test (2026-11-20): the reported symptom — "after
// logging in, it does not redirect to the dashboard/home page but persists
// on the login form" — is the 303 after a successful login pointing back at
// /login because the browser always sends Referer: <site>/login when the
// form at /login is submitted, and handleLoginPost honoured that referer.
// handleLoginPost is called directly (/login is CSRF-exempt, no session
// needed); the d30/d32 helpers seed a real household + user. Skipped unless
// EVENTALLY_TEST_DSN is set (harness: recurring_gate2_test.go).
package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoginRedirectDoesNotBounce(t *testing.T) {
	a, pool := d32App(t)
	hh := d30Household(t, pool, "loginhh")
	u := d30User(t, pool, hh, "login-user", "login-pass")
	d30Membership(t, pool, u, hh, "owner")

	post := func(referer string) *httptest.ResponseRecorder {
		body := "username=login-user&password=login-pass"
		r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if referer != "" {
			r.Header.Set("Referer", referer)
		}
		rec := httptest.NewRecorder()
		a.handleLoginPost(rec, r)
		return rec
	}

	// The browser case: the Referer IS the login form.
	rec := post("http://example.com/login")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login POST: status %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Fatalf("login with Referer /login redirected to %q, want \"/\"", loc)
	}
	// A failed-then-retried login (the form was re-rendered with ?err=1).
	if loc := post("http://example.com/login?err=1").Header().Get("Location"); loc != "/" {
		t.Fatalf("login with Referer /login?err=1 redirected to %q, want \"/\"", loc)
	}
	// No Referer at all: the dashboard fallback.
	if loc := post("").Header().Get("Location"); loc != "/" {
		t.Fatalf("login without Referer redirected to %q, want \"/\"", loc)
	}
	// A same-site referer that is NOT an auth page is still honoured.
	if loc := post("http://example.com/txns").Header().Get("Location"); loc != "http://example.com/txns" {
		t.Fatalf("login with Referer /txns redirected to %q, want the referer", loc)
	}
}
