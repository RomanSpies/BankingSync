package budget

import (
	"context"
	"testing"
	"time"
)

func onDay(n int) time.Time { return time.Date(2026, 8, n, 0, 0, 0, 0, time.UTC) }

func shadowPolicy(trial *Trial) Policy {
	return Policy{HoldForReview: true, TolerancePercent: 25, ToleranceCents: 5000, Trial: trial}
}

// eagerTrial merges on far less evidence than the shipped parameters do, which
// is what makes a difference visible without inventing an implausible model. It
// moves the level an unrelated payee reaches when no classifier is injected,
// which is what these tests produce.
func eagerTrial() *Trial {
	l := DefaultLinkage()
	l.PayeeM = copyPayee(l.PayeeM)
	l.PayeeU = copyPayee(l.PayeeU)
	l.PayeeM[PayeeNone] = 0.30
	l.PayeeU[PayeeNone] = 0.01
	return &Trial{Linkage: l, Calibration: Identity()}
}

// TestShadow_noTrialMeansNoShadow keeps the ordinary path exactly as it was for
// every installation that never evaluates anything.
func TestShadow_noTrialMeansNoShadow(t *testing.T) {
	s := &fakeStore{txns: []*Transaction{
		{ID: "x", AccountID: "acct", Date: onDay(12), AmountCents: -4200, PayeeName: "Hotel Berlin"},
	}}
	out, err := Reconcile(context.Background(), s, "acct", ImportedFields{
		Date: onDay(12), AmountCents: -4200, PayeeName: "Hotel Berlin",
	}, nil, shadowPolicy(nil))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if out.Shadow != nil {
		t.Errorf("a shadow appeared with no trial to cast it: %+v", out.Shadow)
	}
	if out.Differs() {
		t.Error("an outcome with no shadow reported a difference")
	}
}

// TestShadow_changesNothingItObserves is the guarantee the whole idea rests on.
//
// A shadow that could alter a decision is not a shadow; it is an unreviewed
// parameter change. The same batch is run with and without a trial that decides
// quite differently, and everything that reaches the budget has to be identical
// — the rows written, the rows created, and the patches applied to them.
func TestShadow_changesNothingItObserves(t *testing.T) {
	build := func() *fakeStore {
		return &fakeStore{txns: []*Transaction{
			{ID: "x", AccountID: "acct", Date: onDay(12), AmountCents: -4200, PayeeName: "Edeka Roma"},
		}}
	}
	in := []ImportedFields{{Date: onDay(12), AmountCents: -4200, PayeeName: "Edeka Milano"}}

	plain, pShadow := build(), build()
	a, err := ReconcileBatch(context.Background(), plain, "acct", in, nil, shadowPolicy(nil))
	if err != nil {
		t.Fatalf("without a trial: %v", err)
	}
	b, err := ReconcileBatch(context.Background(), pShadow, "acct", in, nil, shadowPolicy(eagerTrial()))
	if err != nil {
		t.Fatalf("with a trial: %v", err)
	}

	if plain.creates != pShadow.creates {
		t.Errorf("the trial changed how many rows were created: %d against %d",
			plain.creates, pShadow.creates)
	}
	if len(plain.updates) != len(pShadow.updates) {
		t.Errorf("the trial changed how many rows were updated: %d against %d",
			len(plain.updates), len(pShadow.updates))
	}
	if a[0].Created != b[0].Created || len(a[0].Held) != len(b[0].Held) {
		t.Errorf("the trial changed the outcome: created=%v/%v held=%d/%d",
			a[0].Created, b[0].Created, len(a[0].Held), len(b[0].Held))
	}
	if b[0].Shadow == nil {
		t.Fatal("no shadow was recorded, so this proves nothing")
	}
}

// TestShadow_reportsAnOutcomeItWouldHaveChanged is the measurement itself.
func TestShadow_reportsAnOutcomeItWouldHaveChanged(t *testing.T) {
	s := &fakeStore{txns: []*Transaction{
		{ID: "x", AccountID: "acct", Date: onDay(12), AmountCents: -4200, PayeeName: "Edeka Roma"},
	}}
	out, err := Reconcile(context.Background(), s, "acct", ImportedFields{
		Date: onDay(12), AmountCents: -4200, PayeeName: "Edeka Milano",
	}, nil, shadowPolicy(eagerTrial()))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if out.Shadow == nil {
		t.Fatal("no shadow was recorded")
	}
	live, _ := out.liveOutcome()
	if out.Shadow.Outcome == live {
		t.Fatalf("both sets decided %q, so there is no difference to report", live)
	}
	if !out.Differs() {
		t.Errorf("the difference between %q and %q was not reported", live, out.Shadow.Outcome)
	}
}

// TestShadow_agreeingOnTheOutcomeIsNotAgreeingOnTheRow catches the comparison
// that looks right and is not. Two parameter sets that both merge, but merge
// into different budget rows, have not agreed about anything — and counting them
// as agreement would report a change as harmless precisely when it moves money.
func TestShadow_agreeingOnTheOutcomeIsNotAgreeingOnTheRow(t *testing.T) {
	same := Outcome{
		Transaction: &Transaction{ID: "x"},
		Shadow:      &ShadowOutcome{Outcome: "adopted", CandidateID: "x"},
	}
	if same.Differs() {
		t.Error("merging into the same row was reported as a difference")
	}

	elsewhere := Outcome{
		Transaction: &Transaction{ID: "x"},
		Shadow:      &ShadowOutcome{Outcome: "adopted", CandidateID: "y"},
	}
	if !elsewhere.Differs() {
		t.Error("merging into a different row was reported as agreement")
	}
}

// TestShadow_arrangesTheWholeBatch pins that the trial is solved as an
// arrangement rather than pair by pair.
//
// Two bookings compete for two authorisations. Under a one-to-one constraint
// each gets one; scored independently they would both prefer the same row. A
// shadow that skipped the arrangement would report two merges into one
// transaction, which is a state the real matcher cannot produce and would make
// every comparison against it meaningless.
func TestShadow_arrangesTheWholeBatch(t *testing.T) {
	s := &fakeStore{txns: []*Transaction{
		{ID: "auth-a", AccountID: "acct", Date: onDay(12), AmountCents: -4200, PayeeName: "Hotel Berlin"},
		{ID: "auth-b", AccountID: "acct", Date: onDay(12), AmountCents: -4201, PayeeName: "Hotel Berlin"},
	}}
	out, err := ReconcileBatch(context.Background(), s, "acct", []ImportedFields{
		{Date: onDay(12), AmountCents: -4200, PayeeName: "Hotel Berlin"},
		{Date: onDay(12), AmountCents: -4201, PayeeName: "Hotel Berlin"},
	}, nil, shadowPolicy(&Trial{Linkage: DefaultLinkage(), Calibration: Identity()}))
	if err != nil {
		t.Fatalf("ReconcileBatch: %v", err)
	}

	seen := map[string]int{}
	for i, o := range out {
		if o.Shadow == nil {
			t.Fatalf("transaction %d has no shadow", i)
		}
		if id := o.Shadow.CandidateID; id != "" {
			seen[id]++
		}
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("the shadow gave row %s to %d transactions at once", id, n)
		}
	}
	if len(seen) != 2 {
		t.Errorf("the shadow paired %d rows, want 2 — %v", len(seen), seen)
	}
}

// TestShadow_doesNotCastAShadowOfItsOwn stops a trial carrying a trial from
// recursing.
func TestShadow_doesNotCastAShadowOfItsOwn(t *testing.T) {
	inner := eagerTrial()
	pol := shadowPolicy(inner)
	pol.Trial = inner

	s := &fakeStore{txns: []*Transaction{
		{ID: "x", AccountID: "acct", Date: onDay(12), AmountCents: -4200, PayeeName: "Edeka Roma"},
	}}
	out, err := Reconcile(context.Background(), s, "acct", ImportedFields{
		Date: onDay(12), AmountCents: -4200, PayeeName: "Edeka Milano",
	}, nil, pol)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if out.Shadow == nil {
		t.Fatal("no shadow was recorded")
	}
}

// TestShadow_leavesTheLiveScoringAlone catches the aliasing mistake this pass is
// one slice-copy away from. The trial rescoring must not write into the
// candidates the real decision was made on.
func TestShadow_leavesTheLiveScoringAlone(t *testing.T) {
	row := []Candidate{{
		Transaction: &Transaction{ID: "x"},
		Comparison:  Comparison{Payee: PayeeConflict, Amount: AmountExact, Date: DateSame},
		Weight:      3, Probability: 0.5, Plausible: 1,
	}}
	before := row[0]

	got := rescore(row, eagerTrial().apply(Policy{}))

	if row[0].Weight != before.Weight || row[0].Probability != before.Probability {
		t.Errorf("rescoring under the trial overwrote the live figures: %+v became %+v",
			before, row[0])
	}
	// And it did rescore, or the check above holds for the wrong reason.
	if got[0].Weight == before.Weight {
		t.Fatalf("the trial produced the same weight %.4f, so nothing was rescored", got[0].Weight)
	}
}
