// Package split computes N-way expense splits that always sum exactly to
// the transaction total.
//
// Rounding uses the largest-remainder method: each participant's share is
// floored to the cent, then the leftover cents are distributed one at a
// time to the participants with the largest fractional remainders. Ties
// are broken by participant order (stable).
package split

import (
	"fmt"
	"strconv"

	"github.com/WanderingLesserPanda/eventally/internal/money"
)

// Mode is how a participant's share is expressed in a form submission.
type Mode string

const (
	// ModePercent expresses the share as a percentage of the total.
	ModePercent Mode = "percent"
	// ModeAmount expresses the share as an exact base-currency amount.
	ModeAmount Mode = "amount"
)

// Part is one participant's contribution to a split request.
type Part struct {
	// Key identifies the participant (e.g. "user:1" or "counterparty:4").
	// Must be unique within a request.
	Key string
	// Mode selects Percent or Amount.
	Mode Mode
	// Percent is the share in percent when Mode == ModePercent.
	// Accepted range: 0..100, up to 4 decimal places.
	Percent float64
	// Amount is the exact share when Mode == ModeAmount.
	Amount money.Money
}

// frac is an exact rational weight.
type frac struct{ num, den int64 }

// Compute splits total among the parts, returning shares (in the parts'
// order) that sum exactly to total. It returns an error when:
//   - parts is empty
//   - keys are duplicated
//   - percents don't sum to 100 (within 0.0001%)
//   - amounts don't sum exactly to total
//   - modes are mixed
func Compute(total money.Money, parts []Part) ([]money.Money, error) {
	n := len(parts)
	if n == 0 {
		return nil, fmt.Errorf("split: no participants")
	}
	totalC := total.AsCents()

	seen := make(map[string]bool, n)
	var sumPercent float64
	for i, p := range parts {
		if p.Key == "" {
			return nil, fmt.Errorf("split: participant %d has no key", i)
		}
		if seen[p.Key] {
			return nil, fmt.Errorf("split: duplicate participant %q", p.Key)
		}
		seen[p.Key] = true
		switch p.Mode {
		case ModePercent:
			if p.Percent < -1e-9 || p.Percent > 100+1e-9 {
				return nil, fmt.Errorf("split: percent %s out of range 0..100",
					strconv.FormatFloat(p.Percent, 'f', -1, 64))
			}
			sumPercent += p.Percent
		case ModeAmount:
		default:
			return nil, fmt.Errorf("split: unknown mode %q", p.Mode)
		}
	}

	// The form picks one mode for all rows.
	mode := parts[0].Mode
	for _, p := range parts[1:] {
		if p.Mode != mode {
			return nil, fmt.Errorf("split: mixed percent and amount parts")
		}
	}

	if mode == ModeAmount {
		var sum int64
		for _, p := range parts {
			sum += p.Amount.AsCents()
		}
		if sum != totalC {
			return nil, fmt.Errorf("split: amounts sum to %s, total is %s",
				money.FromCents(sum).String(), money.FromCents(totalC).String())
		}
		out := make([]money.Money, n)
		for i, p := range parts {
			out[i] = p.Amount
		}
		return out, nil
	}

	d := sumPercent - 100
	if d < 0 {
		d = -d
	}
	if d > 0.0001 {
		return nil, fmt.Errorf("split: percents sum to %s%%, must be exactly 100%%",
			strconv.FormatFloat(sumPercent, 'f', 4, 64))
	}

	fracs := make([]frac, n)
	var sumNum int64
	for i, p := range parts {
		// percent a means share = total * a/100. With a rounded to 4
		// decimals: share = total * (a*10^4) / (100 * 10^4).
		fracs[i] = frac{num: roundPermille(p.Percent), den: 100 * 10000}
		sumNum += fracs[i].num
	}
	// Normalise form rounding slack: make weights sum exactly to 1 by
	// adjusting the last participant (the tolerance above is 0.0001% =
	// 1 unit of the numerator), so the result always sums to totalC.
	fracs[n-1].num += 100*10000 - sumNum
	return largestRemainder(totalC, fracs), nil
}

// roundPermille converts a percent value to integer percent*10^4.
func roundPermille(p float64) int64 {
	if p < 0 {
		return int64(p*10000 - 0.5)
	}
	return int64(p*10000 + 0.5)
}

// largestRemainder distributes totalC according to rational weights.
// Returns shares (in weights' order) summing exactly to totalC.
func largestRemainder(totalC int64, fracs []frac) []money.Money {
	n := len(fracs)
	// remNum[i] = totalC*num_i mod den_i ; remDen[i] = den_i
	remNum := make([]int64, n)
	remDen := make([]int64, n)
	shares := make([]int64, n)
	var assigned int64
	for i := range fracs {
		shareNum := totalC * fracs[i].num
		shares[i] = shareNum / fracs[i].den
		assigned += shares[i]
		remNum[i] = shareNum % fracs[i].den
		remDen[i] = fracs[i].den
	}
	// Hand the leftover cents to the largest remainders, ties by index.
	for leftover := totalC - assigned; leftover > 0; leftover-- {
		best := -1
		for i := range fracs {
			if remNum[i] == 0 {
				continue // fully distributed
			}
			if best == -1 {
				best = i
				continue
			}
			// rem[i] > rem[best]  <=>  remNum[i]*remDen[best] > remNum[best]*remDen[i]
			if remNum[i]*remDen[best] > remNum[best]*remDen[i] {
				best = i
			}
		}
		shares[best]++
		// One cent given out = den/den of this participant's remainder.
		remNum[best] -= remDen[best]
	}
	out := make([]money.Money, n)
	for i := range shares {
		out[i] = money.FromCents(shares[i])
	}
	return out
}

// EqualSplit splits total equally among n participants, returning shares
// (in cents) summing exactly to total. n must be >= 1.
func EqualSplit(total money.Money, n int) []money.Money {
	if n < 1 {
		return nil
	}
	totalC := total.AsCents()
	base := totalC / int64(n)
	out := make([]money.Money, n)
	for i := 0; i < n; i++ {
		out[i] = money.FromCents(base)
	}
	for k := int64(0); k < totalC-base*int64(n); k++ {
		out[int(k)] = out[int(k)].Add(money.FromCents(1))
	}
	return out
}

// Sum returns the sum of shares; used by validation layers.
func Sum(shares []money.Money) money.Money {
	var s money.Money
	for _, m := range shares {
		s = s.Add(m)
	}
	return s
}
