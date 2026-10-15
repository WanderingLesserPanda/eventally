package split

import (
	"testing"

	"github.com/WanderingLesserPanda/eventally/internal/money"
)

func mustParse(t *testing.T, s string) money.Money {
	t.Helper()
	m, err := money.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestEqualSplit(t *testing.T) {
	// The documented RM50.75 case: 25.38 + 25.37.
	total := mustParse(t, "50.75")
	got := EqualSplit(total, 2)
	if Sum(got) != total {
		t.Fatalf("sum %s != %s", Sum(got).String(), total.String())
	}
	want := []string{"25.38", "25.37"}
	for i, w := range want {
		if got[i].String() != w {
			t.Errorf("share %d = %s, want %s", i, got[i].String(), w)
		}
	}
}

func TestEqualSplitThreeWay(t *testing.T) {
	// 100.00 / 3 = 33.34 + 33.33 + 33.33.
	total := mustParse(t, "100.00")
	got := EqualSplit(total, 3)
	if Sum(got) != total {
		t.Fatalf("sum %s != %s", Sum(got).String(), total.String())
	}
	if got[0].String() != "33.34" || got[1].String() != "33.33" || got[2].String() != "33.33" {
		t.Errorf("got %v", []string{got[0].String(), got[1].String(), got[2].String()})
	}
}

func TestEqualSplitSingle(t *testing.T) {
	total := mustParse(t, "12.34")
	got := EqualSplit(total, 1)
	if len(got) != 1 || got[0] != total {
		t.Errorf("got %v", got)
	}
}

func TestComputeAmounts(t *testing.T) {
	// The documented RM1000 = 750 + 250 case.
	total := mustParse(t, "1000.00")
	parts := []Part{
		{Key: "user:1", Mode: ModeAmount, Amount: mustParse(t, "750.00")},
		{Key: "user:2", Mode: ModeAmount, Amount: mustParse(t, "250.00")},
	}
	got, err := Compute(total, parts)
	if err != nil {
		t.Fatal(err)
	}
	if Sum(got) != total || got[0].String() != "750.00" || got[1].String() != "250.00" {
		t.Errorf("got %v", got)
	}
}

func TestComputeAmountsMustClose(t *testing.T) {
	total := mustParse(t, "1000.00")
	parts := []Part{
		{Key: "user:1", Mode: ModeAmount, Amount: mustParse(t, "750.00")},
		{Key: "user:2", Mode: ModeAmount, Amount: mustParse(t, "249.99")},
	}
	if _, err := Compute(total, parts); err == nil {
		t.Error("expected error for unclosed amounts")
	}
}

func TestComputePercent(t *testing.T) {
	total := mustParse(t, "1000.00")
	parts := []Part{
		{Key: "user:1", Mode: ModePercent, Percent: 75},
		{Key: "user:2", Mode: ModePercent, Percent: 25},
	}
	got, err := Compute(total, parts)
	if err != nil {
		t.Fatal(err)
	}
	if Sum(got) != total || got[0].String() != "750.00" || got[1].String() != "250.00" {
		t.Errorf("got %v", got)
	}
}

func TestComputePercentUneven(t *testing.T) {
	// 50.75 at 33.33/33.33/33.34 percents.
	total := mustParse(t, "50.75")
	parts := []Part{
		{Key: "a", Mode: ModePercent, Percent: 33.33},
		{Key: "b", Mode: ModePercent, Percent: 33.33},
		{Key: "c", Mode: ModePercent, Percent: 33.34},
	}
	got, err := Compute(total, parts)
	if err != nil {
		t.Fatal(err)
	}
	if Sum(got) != total {
		t.Fatalf("sum %s != %s", Sum(got).String(), total.String())
	}
	// Largest remainder: participant a's remainder (7189/10000) beats b's
	// tie-broken later, so a and c get the extra cent.
	if got[0].String() != "16.92" || got[1].String() != "16.91" || got[2].String() != "16.92" {
		t.Errorf("got %v", []string{got[0].String(), got[1].String(), got[2].String()})
	}
}

func TestComputePercentMustSumToOneHundred(t *testing.T) {
	total := mustParse(t, "100.00")
	parts := []Part{
		{Key: "a", Mode: ModePercent, Percent: 60},
		{Key: "b", Mode: ModePercent, Percent: 30},
	}
	if _, err := Compute(total, parts); err == nil {
		t.Error("expected error for percents summing to 90")
	}
}

func TestComputeDuplicateKeys(t *testing.T) {
	total := mustParse(t, "10.00")
	parts := []Part{
		{Key: "a", Mode: ModePercent, Percent: 50},
		{Key: "a", Mode: ModePercent, Percent: 50},
	}
	if _, err := Compute(total, parts); err == nil {
		t.Error("expected duplicate key error")
	}
}

func TestComputeMixedModes(t *testing.T) {
	total := mustParse(t, "10.00")
	parts := []Part{
		{Key: "a", Mode: ModePercent, Percent: 50},
		{Key: "b", Mode: ModeAmount, Amount: mustParse(t, "5.00")},
	}
	if _, err := Compute(total, parts); err == nil {
		t.Error("expected mixed-mode error")
	}
}

func TestComputeEmpty(t *testing.T) {
	if _, err := Compute(mustParse(t, "10.00"), nil); err == nil {
		t.Error("expected empty error")
	}
}

// TestSumProperty: for a large random-ish set of totals and ways to split,
// the generated shares must always sum exactly to the total (the anti-drift
// guarantee from the MVP plan).
func TestSumProperty(t *testing.T) {
	totals := []string{"0.01", "0.99", "50.75", "100.00", "4253.13", "999999.99"}
	for _, ts := range totals {
		total := mustParse(t, ts)
		for n := 1; n <= 12; n++ {
			got := EqualSplit(total, n)
			if Sum(got) != total {
				t.Errorf("EqualSplit(%s, %d) sums to %s", ts, n, Sum(got).String())
			}
			// Percent mode: one participant gets an odd remainder.
			parts := make([]Part, n)
			base := 100.0 / float64(n)
			for i := range parts {
				parts[i] = Part{Key: string(rune('a' + i)), Mode: ModePercent, Percent: base}
			}
			// Force sum to exactly 100 by giving the difference to the last.
			sum := 0.0
			for _, p := range parts {
				sum += p.Percent
			}
			parts[n-1].Percent += 100 - sum
			got, err := Compute(total, parts)
			if err != nil {
				t.Fatalf("Compute(%s, %d-way even): %v", ts, n, err)
			}
			if Sum(got) != total {
				t.Errorf("Compute percent %s %d-way sums to %s", ts, n, Sum(got).String())
			}
		}
	}
}
