package web

import (
	"strings"
	"testing"
)

func TestValidateUsername(t *testing.T) {
	cases := []struct {
		in   string
		want string // "" = valid
	}{
		{"", "Username is required."},
		{"HandsomeBob01", ""},
		{"bob01x", ""},
		{"b0b1x2", ""},
		{"A1x9zQ", ""},
		{"bob", "Username must be at least 6 characters."},
		{"abc", "Username must be at least 6 characters."},
		{"bob_1", "Username may only contain letters and numbers — no spaces or special characters."},
		{"bob 1", "Username may only contain letters and numbers — no spaces or special characters."},
		{"bo.b", "Username may only contain letters and numbers — no spaces or special characters."},
		{"bób", "Username may only contain letters and numbers — no spaces or special characters."}, // accent not allowed in the credential
		{strings.Repeat("a", 30), ""},
		{strings.Repeat("a", 31), "Username is at most 30 characters."},
	}
	for _, c := range cases {
		if got := validateUsername(c.in); got != c.want {
			t.Errorf("validateUsername(len %d) = %q, want %q", len(c.in), got, c.want)
		}
	}
}

func TestValidatePersonalName(t *testing.T) {
	cases := []struct {
		in   string
		want string // "" = valid
	}{
		{"", "required."},
		{"B", "must be at least 2 characters."},
		{"Bo", ""},
		{"Bob", ""},
		{"Fujita, Sho", ""},
		{"A. B. C", ""},
		{"Bo b", ""}, // space is allowed
		{"Bo$b", "use only letters, numbers, spaces, periods and commas."},
		{"Bo!b", "use only letters, numbers, spaces, periods and commas."},
		{"Bób", ""}, // accents fine in a display name
		{strings.Repeat("a", 60), ""},
		{strings.Repeat("a", 61), "at most 60 characters."},
	}
	for _, c := range cases {
		if got := validatePersonalName(c.in, 2); got != c.want {
			t.Errorf("validatePersonalName(len %d) = %q, want %q", len(c.in), got, c.want)
		}
	}
}

func TestValidateHouseholdName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "Household name required."},
		{"Ou", "Household name must be at least 3 characters."},
		{"Our", ""},
		{"Our home", ""},
		{"House, Apt. 2", ""},
		{"Our!home", "Household name use only letters, numbers, spaces, periods and commas."},
		{strings.Repeat("a", 61), "Household name at most 60 characters."},
	}
	for _, c := range cases {
		if got := validateHouseholdName(c.in); got != c.want {
			t.Errorf("validateHouseholdName(len %d) = %q, want %q", len(c.in), got, c.want)
		}
	}
}

func TestValidateEmail(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "Email address is required."},
		{"you@example.com", ""},
		{"first.last+tag@sub.domain.org", ""},
		{"a@b.co", ""},
		{"no-at-sign", "Enter a valid email address, e.g. you@example.com."},
		{"missing@tld", "Enter a valid email address, e.g. you@example.com."},
		{"@example.com", "Enter a valid email address, e.g. you@example.com."},
		{"spaces in@x.com", "Enter a valid email address, e.g. you@example.com."},
	}
	for _, c := range cases {
		if got := validateEmail(c.in); got != c.want {
			t.Errorf("validateEmail(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestValidateSetupPassword(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "Password is required (8-64 characters)."},
		{"1234567", "Password must be at least 8 characters."},
		{"12345678", ""},
		{"P@ssw0rd!$%^&*+-", ""}, // specials allowed
		{strings.Repeat("a", 64), ""},
		{strings.Repeat("a", 65), "Password is at most 64 characters."},
	}
	for _, c := range cases {
		if got := validateSetupPassword(c.in); got != c.want {
			t.Errorf("validateSetupPassword(len %d) = %q, want %q", len(c.in), got, c.want)
		}
	}
}
