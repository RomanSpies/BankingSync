package budget

import (
	"context"
	"testing"
)

// TestUSample_excludesTheRowItWasPairedWith is the whole basis of the estimate.
// Every candidate in a window except the one that turned out to be the match is
// a non-match, and that is what u describes.
func TestUSample_excludesTheRowItWasPairedWith(t *testing.T) {
	s := &fakeStore{txns: []*Transaction{
		{ID: "match", AccountID: "acct", Date: onDay(12), AmountCents: -4200, PayeeName: "Hotel Berlin"},
		{ID: "other", AccountID: "acct", Date: onDay(12), AmountCents: -1900, PayeeName: "Kino Astor"},
	}}
	out, err := Reconcile(context.Background(), s, "acct", ImportedFields{
		Date: onDay(12), AmountCents: -4200, PayeeName: "Hotel Berlin",
	}, nil, Policy{HoldForReview: true, TolerancePercent: 25, ToleranceCents: 5000})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if out.Created || out.Transaction == nil || out.Transaction.ID != "match" {
		t.Fatalf("expected the matching row to be adopted, got created=%v %+v", out.Created, out.Transaction)
	}
	if len(out.Unchosen) != 1 {
		t.Fatalf("sampled %d pairs, want 1 — the adopted row must not be counted as a non-match",
			len(out.Unchosen))
	}
	if out.Unchosen[0].Amount == AmountExact {
		t.Error("the sample carries the adopted row's comparison rather than the other one")
	}
}

// TestUSample_takesEveryCandidateWhenNothingWasPaired covers the import case:
// if the transaction was new, every row it was weighed against is a non-match.
func TestUSample_takesEveryCandidateWhenNothingWasPaired(t *testing.T) {
	s := &fakeStore{txns: []*Transaction{
		{ID: "a", AccountID: "acct", Date: onDay(12), AmountCents: -1900, PayeeName: "Kino Astor"},
		{ID: "b", AccountID: "acct", Date: onDay(12), AmountCents: -700, PayeeName: "Baecker"},
	}}
	out, err := Reconcile(context.Background(), s, "acct", ImportedFields{
		Date: onDay(12), AmountCents: -4200, PayeeName: "Hotel Berlin",
	}, nil, Policy{HoldForReview: true, TolerancePercent: 25, ToleranceCents: 5000})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !out.Created {
		t.Fatal("expected a new transaction")
	}
	if len(out.Unchosen) != 2 {
		t.Fatalf("sampled %d pairs, want 2", len(out.Unchosen))
	}
}

// TestUSample_recordsAHeldWindowEntire is the correction to what this test used
// to assert, and the reasoning matters more than the assertion.
//
// It used to require that a held window contribute nothing, on the argument that
// the model has not settled which candidate is the match and so cannot call any
// of them a non-match. That argument proves too much. A window is held precisely
// because it contains a strong-looking pair, so excluding held windows truncates
// the non-match sample on the window's own maximum — and measured against the
// true within-window rates over two hundred thousand windows, that truncation was
// worth +0.97 bits of spurious evidence on an exact payee with an exact amount on
// the same day, in the direction that makes the model more confident of what it
// already believed.
//
// Recording the runners-up and still excluding the best candidate does not fix it
// — it comes to +0.95 bits, because the exclusion is what does the damage rather
// than the omission. What fixes it is recording the window entire: +0.04 bits. In
// the adopted branch the best candidate is left out because the model has just
// concluded it is the counterpart; here it has concluded nothing, and leaving it
// out would presume the answer to the question being asked.
func TestUSample_recordsAHeldWindowEntire(t *testing.T) {
	// An exact payee, an amount inside tolerance and a settlement a week later:
	// +3.459 − ... which lands between the two thresholds, so it is held rather
	// than acted on. The second row is in the window and agrees on nothing.
	s := &fakeStore{txns: []*Transaction{
		{ID: "x", AccountID: "acct", Date: onDay(5), AmountCents: -1200, PayeeName: "Netflix"},
		{ID: "y", AccountID: "acct", Date: onDay(11), AmountCents: -4200, PayeeName: "Edeka"},
	}}
	out, err := Reconcile(context.Background(), s, "acct", ImportedFields{
		Date: onDay(12), AmountCents: -1000, PayeeName: "Netflix",
	}, nil, Policy{HoldForReview: true, TolerancePercent: 25, ToleranceCents: 5000})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(out.Held) == 0 {
		t.Fatalf("this pair was not held (created=%v), so there is nothing to check here",
			out.Created)
	}
	if got, want := len(out.Unchosen), len(out.Held); got != want {
		t.Errorf("a held window of %d candidates contributed %d to the non-match sample; "+
			"all of them belong there, the best one included", want, got)
	}
}

// TestUSample_countsOnlyTheNonMatchSide keeps the two halves of the model apart.
// These pairs are known not to be matches; they say nothing about how two rows
// that ARE one payment compare, and letting them touch m would be inventing
// evidence.
func TestUSample_countsOnlyTheNonMatchSide(t *testing.T) {
	got := CountUnchosen([]Comparison{
		{Payee: PayeeNone, Amount: AmountOutsideHigher, Date: DateSame},
		{Payee: PayeeNone, Amount: AmountExact, Date: DateAfterFar},
	})
	if got.PayeeU[PayeeNone] != 2 || got.AmountU[AmountExact] != 1 || got.DateU[DateSame] != 1 {
		t.Errorf("the u side was not filled as expected: %+v", got)
	}
	for _, n := range []int{len(got.PayeeM), len(got.AmountM), len(got.DateM)} {
		if n != 0 {
			t.Fatalf("a non-match landed on the m side")
		}
	}
}

// TestUSample_makesARefitReachableWithoutLabelledNonMatches is the reason any of
// this exists.
//
// The label sources in service produce almost nothing but confirmed matches: a
// bank's own reference only ever says "these two are one payment". Left to those
// alone the u side never moves off its stated priors however long an
// installation runs, so the refit can only ever adjust half the model. A sample
// of unpaired candidates moves it, and needs nobody to answer anything.
func TestUSample_makesARefitReachableWithoutLabelledNonMatches(t *testing.T) {
	base := DefaultLinkage()

	// Every label a match, which is what a bank reference produces.
	labels := make([]LabelledDecision, 0, 300)
	for i := 0; i < 300; i++ {
		labels = append(labels, LabelledDecision{
			Comparison: Comparison{Payee: PayeeExact, Amount: AmountExact, Date: DateSame},
			Candidates: 2, Match: true,
		})
	}

	withoutSample := ProposeTrial(base, labels, LevelCounts{}, defaultAlpha, defaultOverlap)
	if withoutSample.Linkage.PayeeU[PayeeNone] != base.PayeeU[PayeeNone] {
		t.Fatalf("without a sample the u side moved to %.5f from %.5f, so this test is not "+
			"measuring what it claims",
			withoutSample.Linkage.PayeeU[PayeeNone], base.PayeeU[PayeeNone])
	}

	sample := CountUnchosen(repeat(Comparison{
		Payee: PayeeNone, Amount: AmountOutsideHigher, Date: DateAfterFar,
	}, 2000))
	withSample := ProposeTrial(base, labels, sample, defaultAlpha, defaultOverlap)

	if !above(withSample.Linkage.PayeeU[PayeeNone], withoutSample.Linkage.PayeeU[PayeeNone]) {
		t.Errorf("the sample did not move the u side: %.5f against %.5f",
			withSample.Linkage.PayeeU[PayeeNone], withoutSample.Linkage.PayeeU[PayeeNone])
	}
	// The m side of a level the labels DID speak about is untouched by it, because
	// a non-match is not evidence about matches.
	//
	// Only levels the labels are silent about are coupled to the u side, and then
	// only to hold their weight where it was; PayeeExact is what these three
	// hundred labels are all about, so nothing in a non-match sample may reach it.
	if withSample.Linkage.PayeeM[PayeeExact] != withoutSample.Linkage.PayeeM[PayeeExact] {
		t.Errorf("the non-match sample moved the m side of the level the labels are "+
			"about, from %.5f to %.5f",
			withoutSample.Linkage.PayeeM[PayeeExact], withSample.Linkage.PayeeM[PayeeExact])
	}
}

func repeat(c Comparison, n int) []Comparison {
	out := make([]Comparison, n)
	for i := range out {
		out[i] = c
	}
	return out
}
