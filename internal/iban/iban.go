// Package iban implements the ISO 13616 check digit scheme.
//
// It exists because a real Firefly III instance validates the IBAN it is handed
// while the test fixture does not, so every IBAN a live test sends has to be
// arithmetically correct rather than merely IBAN-shaped. Twelve IBAN-shaped
// literals in this repository predate that requirement and exactly one of them
// passed.
//
// This is not a validation library. It knows nothing about national BBAN
// formats, bank codes or whether an account exists — only the mod-97 rule, which
// is the part a server checks first and the part a hand-written literal gets
// wrong.
package iban

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
)

// Valid reports whether s satisfies the ISO 13616 check.
//
// Length is checked against the ISO bounds only, not against the country's own
// fixed length: a German IBAN is always 22 characters, but encoding that table
// here would be a second thing to keep current for no gain.
func Valid(s string) bool {
	s = strings.ToUpper(strings.ReplaceAll(s, " ", ""))
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	for i := range 4 {
		if i < 2 && (s[i] < 'A' || s[i] > 'Z') {
			return false
		}
		if i >= 2 && (s[i] < '0' || s[i] > '9') {
			return false
		}
	}
	rem, ok := mod97(s[4:] + s[:4])
	return ok && rem == 1
}

// GenerateDE returns a valid German IBAN derived from seed and n.
//
// Deterministic on purpose: a failing live test names an account, and whoever
// looks it up in Firefly's UI should be able to reproduce the IBAN from the run
// rather than hunt for it. Seeding from the namespace keeps two runs against the
// same instance from colliding, which matters because Firefly treats the IBAN as
// an identity and would hand the second run the first run's account.
//
// The BBAN is 18 digits, which is the German length. Every German IBAN is
// therefore exactly 22 characters, so no two can be substrings of one another —
// the property the substring rejection in Store.GetOrCreateAccount relies on
// having, and the reason the fixture's deliberately over-long counterexample
// cannot be reproduced against a real instance.
func GenerateDE(seed string, n int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s/%d", seed, n))
	bban := fmt.Sprintf("%018d", binary.BigEndian.Uint64(sum[:8])%1_000_000_000_000_000_000)

	rem, ok := mod97(bban + "DE00")
	if !ok {
		panic("iban: generated BBAN is not numeric")
	}
	return fmt.Sprintf("DE%02d%s", 98-rem, bban)
}

// mod97 folds the rearranged string down to its remainder, letters counting as
// their position plus nine.
//
// The running remainder is what keeps this out of big-integer arithmetic: an
// IBAN reaches 34 characters, which is far past what a uint64 holds, but the
// remainder never exceeds 96 so a single digit at a time stays well inside one.
func mod97(s string) (int, bool) {
	rem := 0
	for i := range len(s) {
		var part int
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			rem = (rem*10 + int(c-'0')) % 97
			continue
		case c >= 'A' && c <= 'Z':
			part = int(c-'A') + 10
		default:
			return 0, false
		}
		rem = (rem*100 + part) % 97
	}
	return rem, true
}
