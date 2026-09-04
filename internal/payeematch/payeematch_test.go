package payeematch

import (
	"testing"

	"bankingsync/budget"
)

// defaultPrefixes mirrors store.DefaultPayeePrefixesList. It is repeated rather
// than imported so this package stays independent of the settings layer.
var defaultPrefixes = []string{"VISA", "MASTERCARD", "MC", "MAESTRO", "DEBIT", "KARTENZAHLUNG", "POS"}

// golden is the calibration for the comparison levels. Every real case from an
// issue belongs here.
//
// It records a *shape*, not a score, which is what makes it durable: whether
// "Da Luigi Roma" and "Visa Da Luigi" are the same purchase is a judgement the
// weight table makes, but that one name is the other with a word cut off is a
// fact about the strings and will not change when a threshold moves.
var golden = []struct {
	pending string // as the bank writes it while the payment is authorised
	booked  string // as the bank writes it once it settles
	want    budget.PayeeLevel
	why     string
}{
	{"Hotel Berlin", "VISA Hotel Berlin", budget.PayeeExact,
		"prefix stripping alone already makes these equal"},
	{"Edeka Sued", "Edeka Süd", budget.PayeeExact,
		"the same word spelled around a field that cannot carry umlauts"},
	{"hotel   berlin", "Hotel Berlin", budget.PayeeExact,
		"case and spacing are not information"},

	{"Da Luigi Roma", "Visa Da Luigi", budget.PayeeTruncated,
		"the reported case: prefix prepended, one word cut from the tail"},
	{"Shell Tankstelle", "VISA Shell", budget.PayeeTruncated,
		"one word dropped; the same shape as the reported case"},

	{"Aldi", "Aldi Süd Nürnberg", budget.PayeeSubset,
		"a brand inside a branch name, two words unexplained"},
	{"Da", "Da Luigi Roma", budget.PayeeSubset,
		"a two-letter fragment, contained and meaningless — the same shape as " +
			"the Aldi row above, which is why both are one level and neither is decided here"},
	{"Rewe", "REWE SAGT DANKE 1234", budget.PayeeSubset,
		"terminal noise appended"},

	{"Da Luigi Roma", "Visa Da Luigi Milano", budget.PayeeConflict,
		"the same chain in another city. This pair is why the level scheme " +
			"exists: under Sørensen-Dice it is indistinguishable from Shell above"},
	{"Lidl Nuernberg", "Visa Lidl Muenchen", budget.PayeeConflict,
		"the same shape as Milano, spelled without umlauts"},

	{"Hotel Berlim", "Hotel Berlin", budget.PayeeFuzzy,
		"every word paired, one only approximately — a typo"},

	{"Hotel Berlin", "Elektromarkt Nord", budget.PayeeNone,
		"nothing in common"},
	{"Spotify", "Netflix", budget.PayeeNone,
		"two subscriptions at the same price is the classic false merge"},

	{"", "VISA Hotel Berlin", budget.PayeeMissing,
		"an absent name is not a disagreement"},
	{"Hotel Berlin", "", budget.PayeeMissing, "the same, the other way round"},
	{"VISA", "Hotel Berlin", budget.PayeeNone,
		"a name that is nothing but a card scheme prefix keeps it: NormalisePayee " +
			"only strips when something would be left, so this is a payee called " +
			"VISA rather than an empty one"},
}

func TestCompare_matchesTheGoldenTable(t *testing.T) {
	compare := New(defaultPrefixes)
	for _, c := range golden {
		if got := compare(c.pending, c.booked); got != c.want {
			t.Errorf("%q vs %q: got %s, want %s — %s", c.pending, c.booked, got, c.want, c.why)
		}
	}
}

// TestCompare_isSymmetric matters because the caller does not always know which
// side is the pending one: the eligibility check compares a candidate against an
// incoming row, and ranking compares the same two the other way round.
func TestCompare_isSymmetric(t *testing.T) {
	compare := New(defaultPrefixes)
	for _, c := range golden {
		if a, b := compare(c.pending, c.booked), compare(c.booked, c.pending); a != b {
			t.Errorf("%q/%q: %s one way, %s the other", c.pending, c.booked, a, b)
		}
	}
}

// TestCompare_truncationIsCountedInWordsNotProportion pins the rule that
// separates a cut-off name from a fragment, and pins it as a count.
//
// A proportion would need a threshold nobody can defend; a count states
// something arguable — a bank drops the tail to fit a field, and dropping one
// word is a different act from dropping three.
func TestCompare_truncationIsCountedInWordsNotProportion(t *testing.T) {
	compare := New(defaultPrefixes)

	for _, c := range []struct {
		long, short string
		want        budget.PayeeLevel
	}{
		{"Alpha Beta Gamma", "Alpha Beta", budget.PayeeTruncated},
		{"Alpha Beta Gamma", "Alpha", budget.PayeeSubset},
		{"Alpha Beta Gamma Delta", "Alpha Beta Gamma", budget.PayeeTruncated},
		{"Alpha Beta Gamma Delta", "Alpha Beta", budget.PayeeSubset},
	} {
		if got := compare(c.long, c.short); got != c.want {
			t.Errorf("%q vs %q: got %s, want %s (%d words unexplained)",
				c.long, c.short, got, c.want, len(words(c.long))-len(words(c.short)))
		}
	}
}

// TestCompare_conflictBeatsContainment guards the ordering of the two rules that
// matter most. A name that both drops a word and contradicts one is a conflict,
// not a truncation: the contradiction is the stronger signal, because absence
// happens by accident and contradiction does not.
func TestCompare_conflictBeatsContainment(t *testing.T) {
	compare := New(defaultPrefixes)

	if got := compare("Alpha Beta Gamma", "Alpha Delta"); got != budget.PayeeConflict {
		t.Errorf("got %s, want conflict: \"Delta\" contradicts rather than being absent", got)
	}
}

func TestCompare_prefixStrippingRespectsWordBoundaries(t *testing.T) {
	compare := New(defaultPrefixes)

	// "VISABANK" must keep its name — the prefix rule only strips on a boundary,
	// and this is the case that proves the boundary check is doing work.
	if got := compare("VISABANK GmbH", "Hotel Berlin"); got != budget.PayeeNone {
		t.Errorf("got %s, want none: VISABANK is a payee, not a prefixed one", got)
	}
}

func TestFold_rewritesWhatABankRewrites(t *testing.T) {
	for in, want := range map[string]string{
		"süd": "sued", "münchen": "muenchen", "straße": "strasse",
		"köln": "koeln", "märkte": "maerkte", "café": "cafe",
		"hotel berlin": "hotel berlin",
	} {
		if got := fold.Replace(in); got != want {
			t.Errorf("fold(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPayeeLevel_allLevelsName(t *testing.T) {
	for _, l := range []budget.PayeeLevel{
		budget.PayeeMissing, budget.PayeeNone, budget.PayeeConflict,
		budget.PayeeSubset, budget.PayeeTruncated, budget.PayeeFuzzy, budget.PayeeExact,
	} {
		if l.String() == "unknown" {
			t.Errorf("level %d has no name", int(l))
		}
	}
}
