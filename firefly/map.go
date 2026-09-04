package firefly

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	typeWithdrawal = "withdrawal"
	typeDeposit    = "deposit"
	typeTransfer   = "transfer"
)

// TypeForAmount maps a signed amount onto the Firefly transaction type. Firefly
// carries direction in the type, not in the sign, so a zero amount has no
// representation and must be rejected before it reaches the API.
func TypeForAmount(cents int64) (string, error) {
	switch {
	case cents < 0:
		return typeWithdrawal, nil
	case cents > 0:
		return typeDeposit, nil
	default:
		return "", fmt.Errorf("a zero amount has no direction and is rejected by Firefly")
	}
}

// FormatAmount renders cents as the positive decimal string Firefly expects.
func FormatAmount(cents int64) string {
	if cents < 0 {
		cents = -cents
	}
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

// ParseAmount reads a decimal amount into cents, discarding any sign. Direction
// comes from which side of the transaction holds the asset account.
func ParseAmount(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("empty amount")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parse amount %q: %w", raw, err)
	}
	return int64(math.Abs(math.Round(f * 100))), nil
}

// SignedCents derives the amount from the perspective of assetAccountID. Money
// leaving the asset account is negative, money arriving is positive. Reading the
// sign off the type instead would break on transfers and reconciliations, where
// the same type occurs in both directions.
func SignedCents(raw, sourceID, destinationID, assetAccountID string) (int64, error) {
	cents, err := ParseAmount(raw)
	if err != nil {
		return 0, err
	}
	switch assetAccountID {
	case "":
		return 0, fmt.Errorf("no asset account to orient the amount against")
	case sourceID:
		return -cents, nil
	case destinationID:
		return cents, nil
	default:
		return 0, fmt.Errorf("transaction touches neither side of account %s", assetAccountID)
	}
}

// FormatSignedAmount renders cents keeping the sign.
//
// FormatAmount deliberately drops it, because a transaction's direction travels
// in its type. An opening balance has no type to carry direction — a negative
// value is an overdraft or a credit card and must stay negative.
func FormatSignedAmount(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}

// ParseSignedAmount reads a decimal amount into cents, keeping the sign.
//
// Unlike ParseAmount it does not go through a float and does not take the
// absolute value: it reads balances, where both the sign and the last cent are
// the point.
func ParseSignedAmount(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("empty amount")
	}
	neg := false
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		neg = true
		s = s[1:]
	}
	intPart, frac, hasDot := strings.Cut(s, ".")
	if intPart == "" || strings.Contains(frac, ".") || (hasDot && frac == "") {
		return 0, fmt.Errorf("parse amount %q", raw)
	}
	if len(frac) > 2 {
		frac = frac[:2]
	}
	for len(frac) < 2 {
		frac += "0"
	}
	v, err := strconv.ParseInt(intPart+frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse amount %q: %w", raw, err)
	}
	if neg {
		v = -v
	}
	return v, nil
}
