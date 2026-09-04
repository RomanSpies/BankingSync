// Package payeematch decides how two payee spellings relate to one another.
//
// It answers with a budget.PayeeLevel — a category, not a number. That is
// deliberate and was arrived at the hard way. The first attempt scored
// similarity on a scale, which cannot work here: measured, "Shell
// Tankstelle"/"Visa Shell" (the same shop, truncated) and "Da Luigi Roma"/"Visa
// Da Luigi Milano" (a different branch) both score 0.667 under Sørensen-Dice,
// and a sweep over every weighting of Dice against Jaro-Winkler left the second
// scoring *above* the first at every point. They are not alike in different
// degrees. Truncation removes a word; a different branch contradicts one. That
// is a difference in shape, and shape is what a comparison level records.
//
// What each level is worth is not decided here. This package observes; the
// weight table in budget/ judges.
package payeematch

import (
	"strings"

	"github.com/adrg/strutil/metrics"

	"bankingsync/budget"
)

// nearWord is how alike two words must be to count as the same word, used for
// typos. It is strict on purpose: a false pairing at this level invents
// agreement that the containment rule then trusts completely.
//
// It is not what handles umlaut spellings — fold does, before this runs.
// Measured, "sued" against "süd" scores 0.750 and "münchen" against "muenchen"
// 0.782, so a threshold low enough to accept them would sit below "tankstelle"
// against "shell" at 0.733 and start pairing words that merely rhyme.
const nearWord = 0.85

// fold rewrites German umlauts and eszett the way a bank does when its own field
// will not carry them, plus the accents that arrive with foreign merchant names.
// Deterministic, and it loosens nothing — which lowering nearWord would.
var fold = strings.NewReplacer(
	"ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss",
	"à", "a", "á", "a", "â", "a", "è", "e", "é", "e", "ê", "e",
	"ì", "i", "í", "i", "ò", "o", "ó", "o", "ô", "o", "ù", "u", "ú", "u",
)

// New returns a comparison for two raw payee spellings.
//
// prefixes are the card scheme prefixes to discount — the same list the rest of
// the matcher uses. Normalisation is budget.NormalisePayee rather than a second
// set of rules living here.
func New(prefixes []string) func(a, b string) budget.PayeeLevel {
	jw := metrics.NewJaroWinkler()

	return func(a, b string) budget.PayeeLevel {
		na := normalise(a, prefixes)
		nb := normalise(b, prefixes)
		if na == "" || nb == "" {
			return budget.PayeeMissing
		}
		if na == nb {
			return budget.PayeeExact
		}

		short, long := words(na), words(nb)
		if len(short) > len(long) {
			short, long = long, short
		}

		paired := 0
		for _, s := range short {
			for _, l := range long {
				if s == l || jw.Compare(s, l) >= nearWord {
					paired++
					break
				}
			}
		}

		switch {
		case paired == 0:
			return budget.PayeeNone

		// A word of the shorter name has no counterpart at all. Something in one
		// name actively disagrees with the other rather than merely being absent
		// from it, which is what tells a different branch from a truncation.
		case paired < len(short):
			return budget.PayeeConflict

		// Every word on both sides is accounted for, yet the strings differ — so
		// they are the same name spelled differently.
		case paired == len(long):
			return budget.PayeeFuzzy

		// The shorter name survives whole inside the longer one. How much is left
		// over is the difference between a name that was cut short and a fragment
		// that happens to be contained: counted in words rather than as a ratio,
		// because "the bank dropped one word" is a claim one can argue about and
		// a coverage threshold of 0.6 is not.
		case len(long)-paired == 1:
			return budget.PayeeTruncated
		default:
			return budget.PayeeSubset
		}
	}
}

func normalise(s string, prefixes []string) string {
	return fold.Replace(strings.ToLower(budget.NormalisePayee(s, prefixes)))
}

func words(s string) []string {
	var out []string
	for _, f := range strings.Fields(s) {
		if f = strings.Trim(f, ".,;:*/-()"); f != "" {
			out = append(out, f)
		}
	}
	return out
}
