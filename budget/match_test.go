package budget

import (
	"testing"
	"time"
)

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func txn(id string, d time.Time, payee string) *Transaction {
	return &Transaction{ID: id, Date: d, PayeeName: payee, AmountCents: -1000}
}

// adopted runs the assessment and the decision exactly as Reconcile does, and
// returns the row that would be merged into without asking — or nil when the
// incoming transaction would be created instead.
//
// The ranking tests below go through this rather than through an ordering
// function, because ordering is no longer a decision: what gets adopted is the
// best candidate *and* whether it is good enough, and testing the first without
// the second would pin an implementation rather than a behaviour.
func adopted(candidates []*Transaction, in ImportedFields, claimed []*Transaction, pol Policy) *Transaction {
	if best, auto := pol.Decide(Assess(candidates, in, claimed, pol)); auto {
		return best.Transaction
	}
	return nil
}

// order returns the candidate IDs, best first.
func order(candidates []*Transaction, in ImportedFields, pol Policy) []string {
	var ids []string
	for _, c := range Assess(candidates, in, nil, pol) {
		ids = append(ids, c.Transaction.ID)
	}
	return ids
}

func TestWindowBounds_isHalfOpen(t *testing.T) {
	target := day(2026, time.July, 15)
	from, to := WindowBounds(target)

	if !from.Equal(day(2026, time.July, 8)) {
		t.Errorf("from: got %v, want 2026-07-08", from)
	}
	if !to.Equal(day(2026, time.July, 23)) {
		t.Errorf("to: got %v, want 2026-07-23 (exclusive)", to)
	}

	cases := []struct {
		offset int
		want   bool
	}{{-8, false}, {-7, true}, {0, true}, {7, true}, {8, false}}
	for _, tc := range cases {
		if got := InWindow(target.AddDate(0, 0, tc.offset), target); got != tc.want {
			t.Errorf("offset %+d: InWindow = %v, want %v", tc.offset, got, tc.want)
		}
	}
}

// TestAssess_payeeAgreementOutranksDateProximity keeps the balance between two
// fields. A name that agrees is stronger evidence than a day that happens to be
// closer, and it has to stay that way or every busy account starts adopting
// whatever sits nearest the date.
func TestAssess_payeeAgreementOutranksDateProximity(t *testing.T) {
	target := day(2026, time.July, 15)
	in := imported(target, -1000, "rewe markt", "ref-1")

	got := order([]*Transaction{
		txn("near", target, "Other Shop"),
		txn("far", target.AddDate(0, 0, 5), "Rewe Markt"),
	}, in, Policy{})

	if len(got) == 0 || got[0] != "far" {
		t.Fatalf("order = %v, want the agreeing payee first despite the worse date", got)
	}
}

// TestAssess_closestDateAmongEqualPayees is the tie-break one level down: when
// the payee says the same about both, the nearer date decides.
func TestAssess_closestDateAmongEqualPayees(t *testing.T) {
	target := day(2026, time.July, 15)
	in := imported(target, -1000, "Rewe Markt", "ref-1")

	got := order([]*Transaction{
		txn("far", target.AddDate(0, 0, 5), "Rewe Markt"),
		txn("near", target.AddDate(0, 0, 1), "Rewe Markt"),
	}, in, Policy{})

	if len(got) == 0 || got[0] != "near" {
		t.Fatalf("order = %v, want the closer date first", got)
	}
}

// TestAssess_tieBreaksOnIDDeterministically matters because the two backends
// return rows in different orders. Without a last resort that ignores that
// order, the same budget would match differently under Actual and Firefly.
func TestAssess_tieBreaksOnIDDeterministically(t *testing.T) {
	target := day(2026, time.July, 15)
	in := imported(target, -1000, "Rewe Markt", "ref-1")

	a := txn("aaa", target, "Rewe Markt")
	b := txn("bbb", target, "Rewe Markt")

	first := order([]*Transaction{a, b}, in, Policy{})
	second := order([]*Transaction{b, a}, in, Policy{})

	if len(first) == 0 || first[0] != "aaa" {
		t.Fatalf("order = %v, want aaa first", first)
	}
	if first[0] != second[0] {
		t.Errorf("input order decided the outcome: %v then %v", first, second)
	}
}

// TestAssess_dayDistanceAcrossMonthBoundary guards arithmetic that is easy to
// get wrong: the last day of June and the first of July are one day apart.
func TestAssess_dayDistanceAcrossMonthBoundary(t *testing.T) {
	target := day(2026, time.July, 1)
	in := imported(target, -1000, "Rewe Markt", "ref-1")

	got := order([]*Transaction{
		txn("june", day(2026, time.June, 30), "Rewe Markt"),
		txn("july", day(2026, time.July, 4), "Rewe Markt"),
	}, in, Policy{})

	if len(got) == 0 || got[0] != "june" {
		t.Fatalf("order = %v, want the 30th of June first — it is one day away", got)
	}
}

func TestAssess_emptyCandidates(t *testing.T) {
	in := imported(day(2026, time.July, 15), -1000, "Rewe", "ref-1")
	if got := Assess(nil, in, nil, Policy{}); len(got) != 0 {
		t.Errorf("got %d candidates from none", len(got))
	}
	if got := adopted(nil, in, nil, Policy{}); got != nil {
		t.Errorf("adopted %v out of nothing", got)
	}
}

// TestAssess_hardRulesRunBeforeAnyWeighing pins what stays a rule rather than
// becoming evidence. A row already claimed in this run and a settled row
// carrying somebody else's reference are not weak candidates, they are not
// candidates: no amount of agreement elsewhere may bring them back.
func TestAssess_hardRulesRunBeforeAnyWeighing(t *testing.T) {
	target := day(2026, time.July, 15)
	in := ImportedFields{Date: target, AmountCents: -1000, ExternalRef: "ref-new"}

	claimed := &Transaction{ID: "claimed", Date: target, AmountCents: -1000}
	foreign := &Transaction{ID: "foreign", Date: target, AmountCents: -1000, Cleared: true, ExternalRef: "ref-other"}
	manual := &Transaction{ID: "manual", Date: target, AmountCents: -1000}
	own := &Transaction{ID: "own", Date: target, AmountCents: -1000, Cleared: true, ExternalRef: "ref-new"}

	seen := map[string]bool{}
	for _, c := range Assess([]*Transaction{claimed, foreign, manual, own}, in,
		[]*Transaction{claimed}, Policy{}) {
		seen[c.Transaction.ID] = true
	}

	if seen["claimed"] {
		t.Error("a row claimed earlier in this run must not be weighed at all")
	}
	if seen["foreign"] {
		t.Error("a confirmed row owned by another bank transaction must not be weighed")
	}
	if !seen["manual"] {
		t.Error("a manual entry with no external ref must remain a candidate")
	}
	if !seen["own"] {
		t.Error("a confirmed row carrying our own ref must remain a candidate")
	}
}

// TestAssess_aDifferentAmountIsNotAdopted covers what used to be a filter and is
// now evidence. The amount no longer bars a row from consideration, so the claim
// that has to hold is about the outcome: it must not be merged into.
func TestAssess_aDifferentAmountIsNotAdopted(t *testing.T) {
	target := day(2026, time.July, 15)
	in := imported(target, -1000, "Rewe Markt", "ref-new")
	wrong := &Transaction{ID: "amount", Date: target, AmountCents: -4000, PayeeName: "Rewe Markt"}

	if got := adopted([]*Transaction{wrong}, in, nil, Policy{}); got != nil {
		t.Errorf("adopted %q at a quite different amount", got.ID)
	}
}

func TestNormalisePayee_stripsCardSchemePrefixes(t *testing.T) {
	prefixes := []string{"VISA", "MASTERCARD", "KARTENZAHLUNG"}
	cases := map[string]string{
		"VISA Hotel Berlin":        "Hotel Berlin",
		"visa Hotel Berlin":        "Hotel Berlin",
		"MASTERCARD*Shell 1234":    "Shell 1234",
		"KARTENZAHLUNG/REWE Markt": "REWE Markt",
		"Hotel Berlin":             "Hotel Berlin",
		"  Hotel   Berlin  ":       "Hotel Berlin",
		"VISABANK GmbH":            "VISABANK GmbH",
		"VISA":                     "VISA",
	}
	for in, want := range cases {
		if got := NormalisePayee(in, prefixes); got != want {
			t.Errorf("NormalisePayee(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAssess_prefixedPayeeStillBeatsADecoy is the case card schemes create. The
// same purchase arrives spelled with the scheme in front, and it has to outrank
// an unrelated row that merely sits at the same amount.
func TestAssess_prefixedPayeeStillBeatsADecoy(t *testing.T) {
	target := day(2026, time.July, 15)
	in := imported(target, -1000, "VISA Hotel Berlin", "ref-1")
	pol := Policy{PayeePrefixes: []string{"VISA"}}

	got := order([]*Transaction{
		txn("decoy", target, "Elektromarkt Nord"),
		txn("real", target.AddDate(0, 0, -2), "Hotel Berlin"),
	}, in, pol)

	if len(got) == 0 || got[0] != "real" {
		t.Fatalf("order = %v, want the prefixed twin first", got)
	}
}
