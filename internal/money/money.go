// Package money represents monetary amounts with two-decimal precision.
//
// Money is an integer count of cents of the household base currency,
// mapping to PostgreSQL NUMERIC(14,2). All arithmetic is exact integer
// arithmetic; float is only ever an input at the form boundary.
package money

import (
	"database/sql/driver"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Money is an amount in base currency, stored as integer cents.
type Money int64

const (
	// CentsPerUnit is the number of stored cents in one base unit.
	CentsPerUnit int64 = 100
)

// Parse parses a decimal string into Money. It tolerates surrounding
// whitespace, thousand separators, currency prefixes/symbols and a
// trailing decimal with up to 2 significant digits. More than 2
// fractional digits is an error (amounts never have sub-cent precision).
func Parse(s string) (Money, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty amount")
	}
	neg := false
	switch {
	case strings.HasPrefix(s, "-"):
		neg = true
		s = strings.TrimPrefix(s, "-")
	case strings.HasPrefix(s, "+"):
		s = strings.TrimPrefix(s, "+")
	}
	// Strip common currency markers and separators.
	s = strings.TrimLeft(s, "RM$€¥ ")
	s = strings.TrimRight(s, " RM$€¥ ")
	s = strings.ReplaceAll(s, ",", "")
	if s == "" {
		return 0, fmt.Errorf("no numeric content in %q", s)
	}
	var units, frac int64
	var fracDigits int
	seenDot := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			if !seenDot {
				units = units*10 + int64(r-'0')
			} else {
				if fracDigits >= 2 {
					return 0, fmt.Errorf("more than 2 decimal places in %q", s)
				}
				frac = frac*10 + int64(r-'0')
				fracDigits++
			}
		case r == '.' && !seenDot:
			seenDot = true
		default:
			return 0, fmt.Errorf("invalid character %q in amount %q", r, s)
		}
	}
	for fracDigits < 2 {
		frac *= 10
		fracDigits++
	}
	m := Money(units*CentsPerUnit + frac)
	if neg {
		m = -m
	}
	return m, nil
}

// String renders m as a decimal string, e.g. "-12.34". No currency symbol:
// presentation layers add the household currency.
func (m Money) String() string {
	return m.Format("")
}

// Format renders m with a currency symbol prefix if one is given,
// e.g. Format("RM") -> "RM 1,234.56".
func (m Money) Format(symbol string) string {
	neg := m < 0
	abs := int64(m)
	if neg {
		abs = -abs
	}
	units := abs / CentsPerUnit
	frac := abs % CentsPerUnit
	s := fmt.Sprintf("%d.%02d", units, frac)
	if units >= 1000 {
		// Insert thousand separators into the integer part.
		intPart := strconv.FormatInt(units, 10)
		var b strings.Builder
		for i, r := range intPart {
			if i > 0 && (len(intPart)-i)%3 == 0 {
				b.WriteByte(',')
			}
			b.WriteRune(r)
		}
		s = b.String() + "." + fmt.Sprintf("%02d", frac)
	}
	out := s
	if neg {
		out = "-" + out
	}
	if symbol != "" {
		return symbol + " " + out
	}
	return out
}

// Scan implements database/sql.Scanner for pgx NUMERIC decoding.
func (m *Money) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*m = 0
		return nil
	case Money:
		*m = v
		return nil
	case *big.Float:
		return m.scanBigFloat(v)
	case *big.Rat:
		rf := v
		return m.scanRatio(rf.Num(), rf.Denom())
	case string:
		p, err := Parse(v)
		if err != nil {
			return fmt.Errorf("money: %w", err)
		}
		*m = p
		return nil
	case int64:
		// Whole units only; integer columns are never NUMERIC.
		*m = Money(v * CentsPerUnit)
		return nil
	case float64:
		return m.scanFloat(v)
	default:
		return fmt.Errorf("money: unsupported scan type %T", src)
	}
}

func (m *Money) scanBigFloat(v *big.Float) error {
	if v.Sign() == 0 {
		*m = 0
		return nil
	}
	// big.Float holds a binary approximation of the decimal, so round
	// the scaled exact rational to the nearest cent. The approximation
	// error (~1e-12) is far below half a cent, so this recovers the
	// typed value exactly.
	r := new(big.Rat)
	v.Rat(r)
	cents := new(big.Int).Mul(r.Num(), big.NewInt(CentsPerUnit))
	q := new(big.Int)
	rem := new(big.Int)
	q.QuoRem(cents, r.Denom(), rem)
	// Round half away from zero.
	absRem := new(big.Int).Abs(rem)
	if new(big.Int).Lsh(absRem, 1).Cmp(r.Denom()) >= 0 {
		if cents.Sign() >= 0 {
			q.Add(q, big.NewInt(1))
		} else {
			q.Sub(q, big.NewInt(1))
		}
	}
	return m.setInt(q)
}

func (m *Money) setInt(q *big.Int) error {
	if q.BitLen() > 62 {
		return fmt.Errorf("money: amount %s out of range", q)
	}
	*m = Money(q.Int64())
	return nil
}

func (m *Money) scanFloat(v float64) error {
	// Round to the nearest cent to absorb float representation error.
	c := int64(math.Round(v * float64(CentsPerUnit)))
	if c < -1<<62 || c > 1<<62 {
		return fmt.Errorf("money: amount %v out of range", v)
	}
	*m = Money(c)
	return nil
}

func (m *Money) scanRatio(num, den *big.Int) error {
	cents := new(big.Int).Mul(num, big.NewInt(CentsPerUnit))
	cents.Quo(cents, den)
	if cents.BitLen() > 62 {
		return fmt.Errorf("money: amount %v out of range", cents)
	}
	*m = Money(cents.Int64())
	return nil
}

// Value implements driver.Valuer; pgx encodes *big.Float as NUMERIC.
func (m Money) Value() (driver.Value, error) {
	f := new(big.Float).SetInt64(int64(m))
	f.Quo(f, big.NewFloat(float64(CentsPerUnit)))
	return f, nil
}

// Add returns m + o.
func (m Money) Add(o Money) Money { return m + o }

// Sub returns m - o.
func (m Money) Sub(o Money) Money { return m - o }

// Neg returns -m.
func (m Money) Neg() Money { return -m }

// Cmp compares m and o: -1, 0, or 1.
func (m Money) Cmp(o Money) int {
	switch {
	case m < o:
		return -1
	case m > o:
		return 1
	default:
		return 0
	}
}

// IsZero reports whether m is 0.
func (m Money) IsZero() bool { return m == 0 }

// IsNegative reports whether m < 0.
func (m Money) IsNegative() bool { return m < 0 }

// AsCents returns the raw cent count.
func (m Money) AsCents() int64 { return int64(m) }

// FromCents constructs Money from a raw cent count.
func FromCents(c int64) Money { return Money(c) }

// New constructs Money from integer units and sub-units (cents).
func New(units, frac int64) Money {
	f := frac
	if f < 0 {
		f = -f
	}
	m := Money(units*CentsPerUnit + f)
	if units < 0 || (units == 0 && frac < 0) {
		return m.Neg()
	}
	return m
}
