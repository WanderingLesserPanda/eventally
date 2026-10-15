package money

import (
	"math/big"
	"testing"
)

func mp(t *testing.T, s string) Money {
	t.Helper()
	m, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return m
}

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Money
	}{
		{"10", 1000},
		{"10.00", 1000},
		{"10.5", 1050},
		{"0.01", 1},
		{"-5.25", -525},
		{"+5.25", 525},
		{" 5.25 ", 525},
		{"1,234.56", 123456},
		{"RM10.32", 1032},
		{"$10.32", 1032},
		{"0", 0},
		{"0.00", 0},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Parse(%q) = %s, want %s", c.in, got.String(), c.want.String())
		}
	}

	bad := []string{"", "abc", "1.234", "1..2", "12a3", "-"}
	for _, in := range bad {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) expected error, got none", in)
		}
	}
}

func TestFormat(t *testing.T) {
	cases := []struct {
		in   Money
		want string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{1032, "10.32"},
		{-1032, "-10.32"},
		{123456, "1,234.56"},
		{100000000, "1,000,000.00"},
	}
	for _, c := range cases {
		if got := c.in.String(); got != c.want {
			t.Errorf("Format(%d) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := Money(425313).Format("RM"); got != "RM 4,253.13" {
		t.Errorf("Format with symbol = %q", got)
	}
}

func TestArithmetic(t *testing.T) {
	a := mp(t, "50.75")
	if got := a.Add(mp(t, "0.25")); got.AsCents() != 5100 {
		t.Errorf("Add = %s", got)
	}
	if got := a.Sub(mp(t, "0.75")); got.AsCents() != 5000 {
		t.Errorf("Sub = %s", got)
	}
	if got := a.Neg(); got.AsCents() != -5075 {
		t.Errorf("Neg = %s", got)
	}
	if got := a.Cmp(mp(t, "50.76")); got != -1 {
		t.Errorf("Cmp = %d", got)
	}
}

func TestScanBigFloat(t *testing.T) {
	// pgx stdlib delivers NUMERIC as *big.Float.
	var m Money
	bf, ok := new(big.Float).SetString("4253.13")
	if !ok {
		t.Fatal("bad big.Float literal")
	}
	if err := m.Scan(bf); err != nil {
		t.Fatal(err)
	}
	if m.AsCents() != 425313 {
		t.Errorf("scan = %s", m)
	}
	bf2, ok := new(big.Float).SetString("-0.05")
	if !ok {
		t.Fatal("bad big.Float literal")
	}
	if err := m.Scan(bf2); err != nil {
		t.Fatal(err)
	}
	if m.AsCents() != -5 {
		t.Errorf("scan = %s", m)
	}
	if err := m.Scan(nil); err != nil {
		t.Fatal(err)
	}
	if m != 0 {
		t.Errorf("nil scan = %s", m)
	}
}

func TestValueRoundTrip(t *testing.T) {
	m := mp(t, "4253.13")
	v, err := m.Value()
	if err != nil {
		t.Fatal(err)
	}
	var m2 Money
	if err := m2.Scan(v); err != nil {
		t.Fatal(err)
	}
	if m2 != m {
		t.Errorf("round trip %s -> %s", m, m2)
	}
}
