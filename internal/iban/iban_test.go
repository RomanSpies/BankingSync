package iban

import (
	"strings"
	"testing"
)

func TestValid_acceptsPublishedExamples(t *testing.T) {
	// The registry examples, deliberately not all German: the mod-97 fold has to
	// handle letters inside the BBAN, and a German IBAN is all digits after the
	// country code, so a German-only table would never exercise that branch.
	for _, s := range []string{
		"DE89370400440532013000",
		"GB82WEST12345698765432",
		"FR1420041010050500013M02606",
		"NL91ABNA0417164300",
		"CH9300762011623852957",
		"MT84MALT011000012345MTLCAST001S",
	} {
		if !Valid(s) {
			t.Errorf("Valid(%q) = false, want true", s)
		}
	}
}

func TestValid_rejectsAWrongCheckDigit(t *testing.T) {
	// One digit off the published example. This is the whole point of the
	// package: the string still looks exactly like an IBAN.
	for _, s := range []string{
		"DE88370400440532013000",
		"DE89370400440532013001",
		"GB83WEST12345698765432",
	} {
		if Valid(s) {
			t.Errorf("Valid(%q) = true, want false", s)
		}
	}
}

func TestValid_rejectsMalformedInput(t *testing.T) {
	for name, s := range map[string]string{
		"empty":                                 "",
		"too short":                             "DE89",
		"too long":                              "DE89" + strings.Repeat("3", 40),
		"no country":                            "1289370400440532013000",
		"letters where the check digits belong": "DEXX370400440532013000",
		"punctuation":                           "DE89-3704-0044-0532-0130-00",
		"the SEPA creditor identifier":          "DE98ZZZ09999999999",
	} {
		if Valid(s) {
			t.Errorf("Valid(%q) = true for %s, want false", s, name)
		}
	}
}

// TestValid_ignoresGroupingAndCase covers the printed form. A human copying an
// IBAN out of a bank statement brings the spaces along.
func TestValid_ignoresGroupingAndCase(t *testing.T) {
	for _, s := range []string{
		"DE89 3704 0044 0532 0130 00",
		"de89370400440532013000",
		"gb82 west 1234 5698 7654 32",
	} {
		if !Valid(s) {
			t.Errorf("Valid(%q) = false, want true", s)
		}
	}
}

func TestGenerateDE_producesValidGermanIBANs(t *testing.T) {
	for n := range 200 {
		got := GenerateDE("ns", n)
		if len(got) != 22 {
			t.Fatalf("GenerateDE(ns, %d) = %q, length %d, want 22", n, got, len(got))
		}
		if !Valid(got) {
			t.Fatalf("GenerateDE(ns, %d) = %q, which fails its own check", n, got)
		}
	}
}

// TestGenerateDE_isDeterministic is what lets a failed live run be reproduced:
// the account in Firefly's UI can be tied back to the namespace that made it.
func TestGenerateDE_isDeterministic(t *testing.T) {
	if a, b := GenerateDE("bstest-abc-", 3), GenerateDE("bstest-abc-", 3); a != b {
		t.Errorf("two calls disagree: %q vs %q", a, b)
	}
}

// TestGenerateDE_separatesSeedsAndIndices guards the collision that would matter:
// Firefly treats the IBAN as an identity, so two runs sharing one would have the
// second silently handed the first run's account.
func TestGenerateDE_separatesSeedsAndIndices(t *testing.T) {
	seen := map[string]string{}
	for _, seed := range []string{"run-1/TestA", "run-1/TestB", "run-2/TestA"} {
		for n := range 50 {
			got := GenerateDE(seed, n)
			key := seed + "#" + string(rune('0'+n%10))
			if prev, dup := seen[got]; dup {
				t.Fatalf("GenerateDE collided: %s and %s both produced %q", prev, key, got)
			}
			seen[got] = key
		}
	}
}

// TestGenerateDE_neverProducesASubstringOfAnother pins the property the account
// lookup depends on. Firefly's search is a substring match, so Store rejects a
// hit whose IBAN merely contains the one it asked for; fixed-length output is
// what makes that rejection unreachable for anything this generator made.
func TestGenerateDE_neverProducesASubstringOfAnother(t *testing.T) {
	var all []string
	for n := range 100 {
		all = append(all, GenerateDE("ns", n))
	}
	for i, a := range all {
		for j, b := range all {
			if i != j && strings.Contains(b, a) {
				t.Fatalf("%q contains %q", b, a)
			}
		}
	}
}
