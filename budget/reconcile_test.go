package budget

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"
)

type fakeStore struct {
	txns      []*Transaction
	nextID    int
	creates   int
	updates   []Patch
	findCalls []string
}

func (f *fakeStore) Ping(context.Context) error { return nil }

func (f *fakeStore) Close() {
	// Nothing to release: this fake exists only to satisfy the Store interface
	// while the tests exercise the reconciliation policy.
}

func (f *fakeStore) GetOrCreateAccount(_ context.Context, spec AccountSpec) (*Account, error) {
	return &Account{ID: "a1", Name: spec.Name, Currency: spec.Currency}, nil
}

func (f *fakeStore) ListTransactions(_ context.Context, accountID string, from, to time.Time) ([]*Transaction, error) {
	var out []*Transaction
	for _, t := range f.txns {
		if t.AccountID != accountID {
			continue
		}
		if t.Date.Before(from) || !t.Date.Before(to) {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

func (f *fakeStore) FindByExternalRef(_ context.Context, accountID, ref string) (*Transaction, error) {
	f.findCalls = append(f.findCalls, ref)
	for _, t := range f.txns {
		if t.AccountID == accountID && t.ExternalRef == ref {
			return t, nil
		}
	}
	return nil, nil
}

func (f *fakeStore) Create(_ context.Context, accountID string, in ImportedFields) (*Transaction, error) {
	f.creates++
	f.nextID++
	t := &Transaction{
		ID:            string(rune('A' + f.nextID - 1)),
		AccountID:     accountID,
		Date:          in.Date,
		AmountCents:   in.AmountCents,
		Currency:      in.Currency,
		PayeeName:     TitleCase(in.PayeeName),
		Notes:         in.Notes,
		ExternalRef:   in.ExternalRef,
		ImportedPayee: in.ImportedPayee,
		Cleared:       in.Cleared,
	}
	f.txns = append(f.txns, t)
	return t, nil
}

func (f *fakeStore) Update(_ context.Context, t *Transaction, p Patch) error {
	f.updates = append(f.updates, p)
	Apply(t, p)
	return nil
}

func imported(d time.Time, cents int64, payee, ref string) ImportedFields {
	return ImportedFields{
		Date: d, AmountCents: cents, Currency: "EUR",
		PayeeName: payee, ImportedPayee: payee, ExternalRef: ref, Cleared: true,
	}
}

func TestReconcile_externalRefIsTheFastPath(t *testing.T) {
	d := day(2026, time.July, 15)
	existing := &Transaction{ID: "X", AccountID: "a1", Date: d, AmountCents: -1000, ExternalRef: "ref-1"}
	s := &fakeStore{txns: []*Transaction{existing}}

	out, err := Reconcile(context.Background(), s, "a1", imported(d, -1000, "Rewe", "ref-1"), nil, Policy{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if out.Created {
		t.Error("a known external ref must not create a second transaction")
	}
	if out.Transaction.ID != "X" {
		t.Errorf("got %q, want X", out.Transaction.ID)
	}
	if s.creates != 0 {
		t.Errorf("Create was called %d times", s.creates)
	}
}

func TestReconcile_fuzzyMatchAdoptsManualEntry(t *testing.T) {
	d := day(2026, time.July, 15)
	manual := &Transaction{ID: "M", AccountID: "a1", Date: d.AddDate(0, 0, -2), AmountCents: -1000}
	s := &fakeStore{txns: []*Transaction{manual}}

	out, err := Reconcile(context.Background(), s, "a1", imported(d, -1000, "Rewe", "ref-1"), nil, Policy{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if out.Created {
		t.Fatal("a manual entry in the window must be adopted, not duplicated")
	}
	if out.Transaction.ID != "M" {
		t.Errorf("got %q, want M", out.Transaction.ID)
	}
	if out.Transaction.ExternalRef != "ref-1" {
		t.Errorf("the adopted row must carry the bank ref, got %q", out.Transaction.ExternalRef)
	}
	if !out.Transaction.Cleared {
		t.Error("the adopted row must be confirmed")
	}
}

func TestReconcile_createsWhenNothingMatches(t *testing.T) {
	d := day(2026, time.July, 15)
	s := &fakeStore{}

	out, err := Reconcile(context.Background(), s, "a1", imported(d, -1000, "Rewe", "ref-1"), nil, Policy{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !out.Created {
		t.Fatal("expected a new transaction")
	}
	if out.Transaction.ExternalRef != "ref-1" || out.Transaction.AmountCents != -1000 {
		t.Errorf("created transaction is wrong: %+v", out.Transaction)
	}
}

func TestReconcile_outOfWindowIsNotAdopted(t *testing.T) {
	d := day(2026, time.July, 15)
	far := &Transaction{ID: "F", AccountID: "a1", Date: d.AddDate(0, 0, -8), AmountCents: -1000}
	s := &fakeStore{txns: []*Transaction{far}}

	out, err := Reconcile(context.Background(), s, "a1", imported(d, -1000, "Rewe", "ref-1"), nil, Policy{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !out.Created {
		t.Error("a candidate outside the window must not be adopted")
	}
}

func TestReconcile_alreadyMatchedIsNotStolen(t *testing.T) {
	d := day(2026, time.July, 15)
	twin := &Transaction{ID: "T", AccountID: "a1", Date: d, AmountCents: -1000}
	s := &fakeStore{txns: []*Transaction{twin}}

	out, err := Reconcile(context.Background(), s, "a1", imported(d, -1000, "Rewe", "ref-2"), []*Transaction{twin}, Policy{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !out.Created {
		t.Error("a row already claimed this run must not be adopted again")
	}
}

func TestReconcile_emptyRefSkipsTheLookup(t *testing.T) {
	d := day(2026, time.July, 15)
	s := &fakeStore{}

	if _, err := Reconcile(context.Background(), s, "a1", imported(d, -1000, "Rewe", ""), nil, Policy{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(s.findCalls) != 0 {
		t.Errorf("an empty ref must not be looked up, calls: %v", s.findCalls)
	}
}

func TestMergePatch_respectsUserEdits(t *testing.T) {
	base := func() *Transaction {
		return &Transaction{
			ID: "X", Date: day(2026, time.July, 15), AmountCents: -1000,
			PayeeName: "Rewe Markt", ImportedPayee: "REWE MARKT",
		}
	}
	in := ImportedFields{PayeeName: "REWE MARKT", Notes: "Einkauf", Cleared: true, ExternalRef: "ref-1", ImportedPayee: "REWE MARKT"}

	t.Run("reconciled is untouched", func(t *testing.T) {
		tx := base()
		tx.Reconciled = true
		if p := MergePatch(tx, in, nil); !p.IsEmpty() {
			t.Errorf("a reconciled transaction must not be modified, got %+v", p)
		}
	})

	t.Run("renamed payee survives", func(t *testing.T) {
		tx := base()
		tx.PayeeName = "Supermarkt um die Ecke"
		p := MergePatch(tx, in, nil)
		if p.PayeeName != nil {
			t.Errorf("a user-renamed payee must not be overwritten, got %q", *p.PayeeName)
		}
	})

	t.Run("existing notes survive", func(t *testing.T) {
		tx := base()
		tx.Notes = "meine Notiz"
		p := MergePatch(tx, in, nil)
		if p.Notes != nil {
			t.Errorf("existing notes must not be overwritten, got %q", *p.Notes)
		}
	})

	t.Run("empty fields are filled", func(t *testing.T) {
		tx := base()
		p := MergePatch(tx, in, nil)
		if p.Notes == nil || *p.Notes != "Einkauf" {
			t.Error("empty notes must be filled")
		}
		if p.Cleared == nil || !*p.Cleared {
			t.Error("an unconfirmed row must be confirmed")
		}
		if p.ExternalRef == nil || *p.ExternalRef != "ref-1" {
			t.Error("the external ref must be written")
		}
	})
}

func TestAmountPatch(t *testing.T) {
	tx := &Transaction{ID: "X", AmountCents: -1000}

	if p := AmountPatch(tx, -1000); !p.IsEmpty() {
		t.Error("an unchanged amount must produce no patch")
	}
	p := AmountPatch(tx, -1250)
	if p.AmountCents == nil || *p.AmountCents != -1250 {
		t.Errorf("expected an amount correction, got %+v", p)
	}

	tx.Reconciled = true
	if p := AmountPatch(tx, -1250); !p.IsEmpty() {
		t.Error("a reconciled transaction must not have its amount rewritten")
	}
}

// An opening balance sits one day before the fetch window, which puts it inside
// the candidate window of the first transactions that follow it. If it were
// written uncleared, or without an external reference, adoptable() would let a
// same-amount bank transaction claim it instead of creating its own row: the
// opening balance would vanish into a normal expense and the account total
// would be wrong by exactly that amount.
func TestAdoptable_openingBalanceRowIsNotAdoptable(t *testing.T) {
	opening := &Transaction{
		ID:          "opening",
		Date:        time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC),
		AmountCents: -123456,
		PayeeName:   "Starting Balance",
		ExternalRef: "bankingsync-opening-DE77",
		Cleared:     true,
	}

	in := ImportedFields{
		Date:        time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC),
		AmountCents: -123456,
		PayeeName:   "Some Shop",
		ExternalRef: "bank-ref-1",
	}

	if got := Assess([]*Transaction{opening}, in, nil, Policy{}); len(got) != 0 {
		t.Fatalf("the opening balance is a candidate for adoption by %+v", in)
	}

	for name, broken := range map[string]*Transaction{
		"not cleared":     {ID: "o", Date: opening.Date, AmountCents: opening.AmountCents, ExternalRef: opening.ExternalRef},
		"no external ref": {ID: "o", Date: opening.Date, AmountCents: opening.AmountCents, Cleared: true},
	} {
		if got := Assess([]*Transaction{broken}, in, nil, Policy{}); len(got) == 0 {
			t.Errorf("%s: expected this shape to BE adoptable, so the guard above is "+
				"testing something real", name)
		}
	}
}

func tolPolicy() Policy {
	return Policy{PayeePrefixes: []string{"VISA"}, TolerancePercent: 25, ToleranceCents: 5000}
}

// TestAssess_adoptsAnOpenRowBookedAtADifferentAmount is the case the tolerance
// exists for: a hotel authorised at 120.00 and settled at 138.50.
func TestAssess_adoptsAnOpenRowBookedAtADifferentAmount(t *testing.T) {
	target := day(2026, time.July, 15)
	pending := &Transaction{ID: "auth", Date: target.AddDate(0, 0, -2), AmountCents: -12000, PayeeName: "Hotel Berlin"}
	in := imported(target, -13850, "VISA Hotel Berlin", "book-1")

	if got := adopted([]*Transaction{pending}, in, nil, tolPolicy()); got == nil || got.ID != "auth" {
		t.Fatalf("got %v, want the authorisation row", got)
	}
	if got := adopted([]*Transaction{pending}, in, nil, Policy{}); got != nil {
		t.Error("with the tolerance off the row must not be adopted")
	}
}

// TestAssess_neverTouchesAConfirmedForeignRow keeps a settled transaction that
// belongs to another bank row out of reach. This is a rule, not evidence.
func TestAssess_neverTouchesAConfirmedForeignRow(t *testing.T) {
	target := day(2026, time.July, 15)
	settled := &Transaction{
		ID: "done", Date: target, AmountCents: -12000,
		PayeeName: "Hotel Berlin", Cleared: true, ExternalRef: "someone-else",
	}
	in := imported(target, -13850, "VISA Hotel Berlin", "book-1")

	if got := Assess([]*Transaction{settled}, in, nil, tolPolicy()); len(got) != 0 {
		t.Errorf("a settled row owned by another transaction was weighed at all: %v", got)
	}
}

// TestAssess_refusesToGuessBetweenTwoEqualCandidates is the one rule that cannot
// live in the per-pair model, because it is a property of the set: two rows that
// fit equally well cannot both be the settlement, and choosing either would be a
// coin toss dressed up as a decision. A visible duplicate beats an invisible
// mis-adoption.
func TestAssess_refusesToGuessBetweenTwoEqualCandidates(t *testing.T) {
	target := day(2026, time.July, 15)
	a := &Transaction{ID: "a", Date: target, AmountCents: -12000, PayeeName: "Hotel Berlin"}
	b := &Transaction{ID: "b", Date: target, AmountCents: -12100, PayeeName: "Hotel Berlin"}
	in := imported(target, -12500, "VISA Hotel Berlin", "book-1")

	if got := adopted([]*Transaction{a, b}, in, nil, tolPolicy()); got != nil {
		t.Fatalf("adopted %q while another row fitted just as well", got.ID)
	}
	// On its own the same row is adopted, so the refusal above is the ambiguity
	// and not the candidate being too weak to begin with.
	if got := adopted([]*Transaction{a}, in, nil, tolPolicy()); got == nil {
		t.Error("alone, that candidate should have been adopted")
	}
}

// TestAssess_theAmountOutranksTheDate states a property of bank data rather than
// of the code: a bank does not alter an amount at random, while a settlement
// date shifts by days as a matter of course.
//
// The old rule said so by construction — exact amounts shadowed tolerated ones
// outright. The model says it through the weights instead, so the claim now has
// to be checked rather than assumed: a row matching to the cent six days away
// must still beat one a euro out on the same day.
func TestAssess_theAmountOutranksTheDate(t *testing.T) {
	target := day(2026, time.July, 15)
	exact := &Transaction{ID: "exact", Date: target.AddDate(0, 0, -6), AmountCents: -12500}
	near := &Transaction{ID: "near", Date: target, AmountCents: -12400}
	in := ImportedFields{Date: target, AmountCents: -12500, ExternalRef: "book-1"}

	scored := Assess([]*Transaction{exact, near}, in, nil, tolPolicy())
	if len(scored) != 2 {
		t.Fatalf("got %d candidates, want both weighed", len(scored))
	}
	if scored[0].Transaction.ID != "exact" {
		t.Errorf("order = %s then %s (%.2f vs %.2f bits), want the exact amount first",
			scored[0].Transaction.ID, scored[1].Transaction.ID, scored[0].Weight, scored[1].Weight)
	}
}

func TestPolicy_toleranceIsCappedInAbsoluteTerms(t *testing.T) {
	p := tolPolicy()
	if p.withinTolerance(-300000, -400000) {
		t.Error("25% of 4000.00 would be 1000.00 of slack; the absolute cap must bind")
	}
	if !p.withinTolerance(-12000, -13850) {
		t.Error("a normal hotel pre-authorisation was rejected")
	}
	// A fuel release of 100.00 that books at 60.00 differs by 40% and is NOT
	// covered at the default. That is deliberate: at that distance the two are
	// genuinely ambiguous, and the setting is in the UI for operators whose bank
	// behaves that way.
	if p.withinTolerance(-10000, -6000) {
		t.Error("a 40% difference was accepted at the 25% default")
	}
	if !p.withinTolerance(-10000, -8000) {
		t.Error("a 20% fuel release was rejected")
	}
	// Symmetry: booked above or below the authorisation reads the same.
	if p.withinTolerance(-6000, -10000) != p.withinTolerance(-10000, -6000) {
		t.Error("the tolerance is not symmetric in the direction of the difference")
	}
}

// Adopting at a different amount without correcting it leaves the row standing
// at the authorised value forever.
func TestReconcile_adoptedRowCarriesTheBookedAmount(t *testing.T) {
	target := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	pending := &Transaction{ID: "auth", AccountID: "a1", Date: target.AddDate(0, 0, -2),
		AmountCents: -12000, PayeeName: "Hotel Berlin", ImportedPayee: "Hotel Berlin"}

	s := &fakeStore{txns: []*Transaction{pending}}
	in := ImportedFields{Date: target, AmountCents: -13850,
		PayeeName: "VISA Hotel Berlin", ImportedPayee: "VISA Hotel Berlin", ExternalRef: "book-1"}

	out, err := Reconcile(context.Background(), s, "a1", in, nil, tolPolicy())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if out.Created {
		t.Fatal("a duplicate was created instead of adopting the authorisation")
	}
	if out.Transaction.AmountCents != -13850 {
		t.Errorf("amount: got %d, want -13850 — the row still shows the authorised value",
			out.Transaction.AmountCents)
	}
}

// After a confirm the row must not look user-renamed to the importer, or it
// stops maintaining its own payee from then on.
func TestMergePatch_importedPayeeFollowsTheBookedSpelling(t *testing.T) {
	tx := &Transaction{ID: "t", PayeeName: "Hotel Berlin", ImportedPayee: "Hotel Berlin"}
	in := ImportedFields{PayeeName: "VISA Hotel Berlin", ImportedPayee: "VISA Hotel Berlin"}

	p := MergePatch(tx, in, []string{"VISA"})
	if p.ImportedPayee == nil || *p.ImportedPayee != "VISA Hotel Berlin" {
		t.Fatalf("ImportedPayee: got %v, want the booked spelling", p.ImportedPayee)
	}

	Apply(tx, p)
	if PayeeWasRenamedByUser(tx) {
		t.Error("the row now looks user-renamed to the importer, which stops it from " +
			"ever maintaining the payee again")
	}
}

func TestMergePatch_leavesARealUserRenameAlone(t *testing.T) {
	tx := &Transaction{ID: "t", PayeeName: "My Favourite Hotel", ImportedPayee: "Hotel Berlin"}
	in := ImportedFields{PayeeName: "VISA Hotel Berlin", ImportedPayee: "VISA Hotel Berlin"}

	p := MergePatch(tx, in, []string{"VISA"})
	if p.PayeeName != nil {
		t.Errorf("a user rename was overwritten with %q", *p.PayeeName)
	}
}

func TestReconcile_reportsANearMissBeforeCreating(t *testing.T) {
	target := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	a := &Transaction{ID: "a", AccountID: "a1", Date: target, AmountCents: -12000}
	b := &Transaction{ID: "b", AccountID: "a1", Date: target, AmountCents: -12100}
	s := &fakeStore{txns: []*Transaction{a, b}}

	var reasons []string
	pol := tolPolicy()
	pol.OnNearMiss = func(reason string, _ *Transaction) { reasons = append(reasons, reason) }

	in := ImportedFields{Date: target, AmountCents: -12500, ExternalRef: "book-1"}
	if out, err := Reconcile(context.Background(), s, "a1", in, nil, pol); err != nil || !out.Created {
		t.Fatalf("Reconcile: created=%v err=%v", out.Created, err)
	}
	if len(reasons) == 0 {
		t.Fatal("a duplicate was created with two near candidates and nothing was reported")
	}
}

// TestAssess_anUnrelatedBookingDoesNotTakeAnAuthorisation is the defect reported
// once the tolerance was introduced: any single open row within 25% of a booking
// was adopted whatever it was, and since an adopted row is rewritten with the
// booked amount, payee and reference, the authorisation was not left unmatched
// but replaced.
func TestAssess_anUnrelatedBookingDoesNotTakeAnAuthorisation(t *testing.T) {
	target := day(2026, time.July, 15)
	pending := &Transaction{ID: "auth", Date: target.AddDate(0, 0, -2), AmountCents: -12000, PayeeName: "Hotel Berlin"}

	unrelated := imported(target, -13850, "Elektromarkt Nord", "book-1")
	if got := adopted([]*Transaction{pending}, unrelated, nil, tolPolicy()); got != nil {
		t.Fatalf("an unrelated booking took the authorisation for %q", pending.PayeeName)
	}

	settled := imported(target, -13850, "VISA Hotel Berlin", "book-1")
	if got := adopted([]*Transaction{pending}, settled, nil, tolPolicy()); got == nil {
		t.Fatal("the settling authorisation was not adopted")
	}
}

// TestAssess_anAbsentPayeeIsNotEvidence covers the widest reading of the weakest
// evidence. A name nobody supplied says nothing, and a differing amount plus a
// silent payee must not add up to a merge.
func TestAssess_anAbsentPayeeIsNotEvidence(t *testing.T) {
	target := day(2026, time.July, 15)

	for name, tc := range map[string]struct{ candidate, incoming string }{
		"neither side names anyone": {"", ""},
		"only the candidate does":   {"Hotel Berlin", ""},
		"only the booking does":     {"", "VISA Hotel Berlin"},
	} {
		t.Run(name, func(t *testing.T) {
			c := &Transaction{ID: "auth", Date: target, AmountCents: -12000, PayeeName: tc.candidate}
			in := imported(target, -13850, tc.incoming, "b")
			if got := adopted([]*Transaction{c}, in, nil, tolPolicy()); got != nil {
				t.Error("adopted on a differing amount and no payee evidence at all")
			}
		})
	}
}

// TestAssess_anExactAmountCarriesADifferentSpelling is what lets a transaction
// somebody typed by hand — spelled however they spell it — be adopted rather
// than duplicated. An amount agreeing to the cent is evidence in its own right.
func TestAssess_anExactAmountCarriesADifferentSpelling(t *testing.T) {
	target := day(2026, time.July, 15)
	manual := &Transaction{ID: "manual", Date: target, AmountCents: -2500, PayeeName: "Rewe"}
	in := imported(target, -2500, "REWE SAGT DANKE 1234", "book-1")

	// The level is supplied rather than derived: this test is about what the
	// rules do with a weak payee, not about how the classifier arrives at one.
	// Which level these two spellings actually produce is settled in
	// internal/payeematch, against its own table.
	pol := tolPolicy()
	pol.Compare = func(string, string) PayeeLevel { return PayeeSubset }

	if got := adopted([]*Transaction{manual}, in, nil, pol); got == nil {
		t.Fatal("an exact amount must still carry a differently spelled payee")
	}
}

// TestReconcile_reportsThePayeeRefusal makes the new rule observable. If a bank
// spells its booked payee differently from its pending one, this counter is where
// that shows up — rather than in a user noticing that nothing ever confirms.
func TestReconcile_reportsThePayeeRefusal(t *testing.T) {
	target := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	pending := &Transaction{
		ID: "auth", AccountID: "acct", Date: target, AmountCents: -12000, PayeeName: "Hotel Berlin",
	}

	var reasons []string
	pol := tolPolicy()
	pol.OnNearMiss = func(reason string, _ *Transaction) { reasons = append(reasons, reason) }

	in := ImportedFields{Date: target, AmountCents: -13850, PayeeName: "Elektromarkt Nord", ExternalRef: "book-1"}
	s := &fakeStore{txns: []*Transaction{pending}}

	if out, err := Reconcile(context.Background(), s, "acct", in, nil, pol); err != nil || !out.Created {
		t.Fatalf("Reconcile: created=%v err=%v, want a new transaction", out.Created, err)
	}
	var sawPayee bool
	for _, r := range reasons {
		if r == "payee" {
			sawPayee = true
		}
	}
	if !sawPayee {
		t.Errorf("near-miss reasons = %v, want the payee refusal named so it can be counted", reasons)
	}
}

// TestReconcile_holdsInsteadOfGuessing covers the middle band.
//
// The same amount on the same day, but a payee with nothing in common: too much
// agreement to dismiss, too little to act on. Importing it risks a duplicate
// somebody has to clean up; adopting it risks overwriting an unrelated
// authorisation, which nobody would notice at all. So neither happens and a
// person decides.
func TestReconcile_holdsInsteadOfGuessing(t *testing.T) {
	target := day(2026, time.July, 15)
	other := &Transaction{ID: "other", AccountID: "a1", Date: target, AmountCents: -2500, PayeeName: "Spotify"}
	s := &fakeStore{txns: []*Transaction{other}}

	pol := tolPolicy()
	pol.HoldForReview = true
	in := imported(target, -2500, "Netflix", "book-1")

	out, err := Reconcile(context.Background(), s, "a1", in, nil, pol)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if out.Transaction != nil {
		t.Fatalf("something was written: %+v", out.Transaction)
	}
	if len(out.Held) == 0 {
		t.Fatal("nothing was held; the transaction would have vanished")
	}
	if out.Held[0].Transaction.ID != "other" {
		t.Errorf("held the wrong candidate: %q", out.Held[0].Transaction.ID)
	}
	if s.creates != 0 {
		t.Errorf("%d transactions were created while the decision was still open", s.creates)
	}
}

// TestReconcile_holdingIsOffByDefault keeps a caller that has nowhere to put a
// held transaction from losing one. Without the flag the old behaviour stands:
// what cannot be adopted is imported as new.
func TestReconcile_holdingIsOffByDefault(t *testing.T) {
	target := day(2026, time.July, 15)
	other := &Transaction{ID: "other", AccountID: "a1", Date: target, AmountCents: -2500, PayeeName: "Spotify"}
	s := &fakeStore{txns: []*Transaction{other}}

	out, err := Reconcile(context.Background(), s, "a1", imported(target, -2500, "Netflix", "book-1"), nil, tolPolicy())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(out.Held) != 0 {
		t.Fatal("held without being asked to")
	}
	if !out.Created {
		t.Error("the transaction was neither held nor created")
	}
}

// TestDecide_equalThresholdsLeaveOnlyTies pins what the narrowest legal setting
// actually does, because "the band is empty" and "nothing is ever held" are not
// the same claim and the difference is the margin rule.
func TestDecide_equalThresholdsLeaveOnlyTies(t *testing.T) {
	pol := Policy{AutoProbability: 0.9, ReviewProbability: 0.9}
	clear := []Candidate{
		{Weight: 6, Probability: 0.98},
		{Weight: 1, Probability: 0.67},
	}
	if _, auto := pol.Decide(clear); !auto {
		t.Error("a clear winner must still merge without asking")
	}

	tied := []Candidate{
		{Weight: 6, Probability: 0.98},
		{Weight: 5.5, Probability: 0.97},
	}
	best, auto := pol.Decide(tied)
	if auto || best == nil {
		t.Error("two rows that fit equally well must still be asked about; " +
			"the margin rule is not a threshold and closing the band does not disable it")
	}
}

// TestLinkage_thePriorIsWhatItClaimsToBe replaces the identity that used to be
// pinned here, which was that one candidate carries a prior of exactly zero bits.
//
// That identity was true of 1/(n+1) and it was load-bearing: fieldEvidence
// computed "the fields alone" as Weight at one candidate, and the plausibility
// cut was taken on the result. Both are gone. The prior is now pi/n against
// 1 - pi/n, so one candidate carries log2(pi/(1-pi)) — at the shipped two in five
// that is -0.585 bits, not nought — and the plausible count is window membership
// rather than anything derived from the weight.
//
// What is pinned instead is the formula, at the sizes that matter.
func TestLinkage_thePriorIsWhatItClaimsToBe(t *testing.T) {
	l := DefaultLinkage()
	c := Comparison{Payee: PayeeExact, Amount: AmountExact, Date: DateSame}

	fields := evidence(l.PayeeM[c.Payee], l.PayeeU[c.Payee]) +
		evidence(l.AmountM[c.Amount], l.AmountU[c.Amount]) +
		evidence(l.DateM[c.Date], l.DateU[c.Date])

	for _, n := range []int{1, 2, 3, 6, 15, 40} {
		lambda := defaultOverlap / float64(n)
		want := fields + math.Log2(lambda/(1-defaultOverlap))
		if got := l.Weight(c, n, defaultOverlap); math.Abs(got-want) > 1e-9 {
			t.Errorf("%d candidates: weight %.4f, want %.4f = fields %.4f plus a prior "+
				"of %.4f bits", n, got, want, fields, math.Log2(lambda/(1-defaultOverlap)))
		}
	}

	// At an even overlap the prior term is log2(1/n) exactly, which is what the
	// 1/(n+1) form this replaced already computed. The reformulation is a
	// statement an operator can now make, not a change of behaviour, and this is
	// what says so.
	for _, n := range []int{1, 2, 3, 8, 13, 40} {
		old := 1 / float64(n+1)
		want := fields + math.Log2(old/(1-old))
		if got := l.Weight(c, n, 0.5); math.Abs(got-want) > 1e-12 {
			t.Errorf("%d candidates at an even overlap: %.12f, want %.12f — the "+
				"reformulation was supposed to leave the arithmetic alone", n, got, want)
		}
	}

	// The direction is the whole point of the change: more candidates must make a
	// specific one less likely a priori, and the old flat prior said the opposite
	// about whether any of them matched at all.
	prev := math.Inf(1)
	for _, n := range []int{1, 2, 5, 15, 40} {
		got := l.Weight(c, n, defaultOverlap)
		if got >= prev {
			t.Fatalf("%d candidates scored %.4f against %.4f for fewer; a crowded window "+
				"must not make a given pairing more likely", n, got, prev)
		}
		prev = got
	}

	// And "none of them" keeps a fixed share whatever the window holds, which is
	// the reading the old form did not support: taken as a flat prior over n+1
	// outcomes it made P(new) fall to zero as the window filled.
	for _, n := range []int{1, 5, 40} {
		lambda := defaultOverlap / float64(n)
		if pNone := 1 - float64(n)*lambda; math.Abs(pNone-(1-defaultOverlap)) > 1e-12 {
			t.Errorf("%d candidates leave P(no counterpart) at %.4f, want %.4f",
				n, pNone, 1-defaultOverlap)
		}
	}

	// Moving the overlap shifts every weight by the same amount whatever the
	// candidate count, which is worth pinning because it is the reason this is a
	// modest setting rather than a powerful one. On its own it does what moving
	// the thresholds does. What makes it more than that is that it also sets the
	// balance between "no counterpart" and "a different one", and that balance is
	// what the arrangement in Solve reads.
	for _, pi := range []float64{0.2, 0.35, 0.65, 0.8} {
		want := math.Log2(pi/(1-pi)) - math.Log2(defaultOverlap/(1-defaultOverlap))
		for _, n := range []int{1, 2, 7, 25} {
			got := l.Weight(c, n, pi) - l.Weight(c, n, defaultOverlap)
			if math.Abs(got-want) > 1e-12 {
				t.Errorf("overlap %.2f at %d candidates shifted the weight by %.6f, and by "+
					"%.6f at other counts; it is supposed to be the same shift everywhere",
					pi, n, got, want)
			}
		}
	}
}

// TestAssess_unrelatedRowsDoNotPunishTheWinner is the defect this rule was added
// for.
//
// The prior is one over the candidate count, so a window full of rows the
// evidence has already dismissed used to weigh against the one that fits.
// Measured: a payee agreeing exactly and an amount agreeing to the cent stopped
// being merged automatically once fifteen unrelated rows shared the fortnight,
// because the prior cost 3.9 bits. Which candidate wins among several is the
// assignment's question; the prior only says how likely a counterpart exists.
func TestAssess_unrelatedRowsDoNotPunishTheWinner(t *testing.T) {
	target := day(2026, time.July, 15)
	in := imported(target, -12000, "Hotel Berlin", "book-1")

	match := &Transaction{ID: "auth", Date: target.AddDate(0, 0, -2),
		AmountCents: -12000, PayeeName: "Hotel Berlin"}

	alone := Assess([]*Transaction{match}, in, nil, tolPolicy())
	if len(alone) != 1 {
		t.Fatalf("got %d candidates alone, want 1", len(alone))
	}

	crowd := []*Transaction{match}
	for i := 0; i < 20; i++ {
		crowd = append(crowd, &Transaction{
			ID: fmt.Sprintf("noise-%d", i), Date: target,
			AmountCents: int64(-300 * (i + 1)), PayeeName: fmt.Sprintf("Elektromarkt %d", i),
		})
	}
	crowded := Assess(crowd, in, nil, tolPolicy())
	if crowded[0].Transaction.ID != "auth" {
		t.Fatalf("the crowd took the top place: %q", crowded[0].Transaction.ID)
	}

	if math.Abs(crowded[0].Weight-alone[0].Weight) > 1e-9 {
		t.Errorf("the match is worth %.3f bits among twenty unrelated rows and %.3f bits "+
			"alone; rows the evidence has dismissed are still counted against it",
			crowded[0].Weight, alone[0].Weight)
	}
	if crowded[0].Plausible != 1 {
		t.Errorf("the prior was taken over %d candidates, want 1", crowded[0].Plausible)
	}

	// Counting the window instead was tried and withdrawn; this is the case that
	// withdrew it. Twenty rows agreeing on nothing would cost the winner
	// log2(1/21) = 4.39 bits, taking an exact payee with an exact amount on the
	// same day from 0.991 to 0.890 — below the threshold that merges, on the
	// strength of rows that agree on nothing at all.
	if crowded[0].Probability < tolPolicy().autoProbability() {
		t.Errorf("an exact payee, an exact amount and the same day reach only %.3f among "+
			"twenty unrelated rows, below the %.2f that merges", crowded[0].Probability,
			tolPolicy().autoProbability())
	}
}

// TestAssess_realCompetitionStillCounts is the other side, and the reason this
// is a narrowing rather than a removal. Several authorisations that all fit make
// each of them less likely to be the one, and the prior is what says so.
func TestAssess_realCompetitionStillCounts(t *testing.T) {
	target := day(2026, time.July, 15)
	in := imported(target, -12000, "Hotel Berlin", "book-1")

	var rivals []*Transaction
	for i := 0; i < 4; i++ {
		rivals = append(rivals, &Transaction{
			ID: fmt.Sprintf("auth-%d", i), Date: target.AddDate(0, 0, -i),
			AmountCents: -12000, PayeeName: "Hotel Berlin",
		})
	}

	one := Assess(rivals[:1], in, nil, tolPolicy())
	four := Assess(rivals, in, nil, tolPolicy())

	if four[0].Plausible != 4 {
		t.Errorf("the prior was taken over %d candidates, want 4 — all of them fit",
			four[0].Plausible)
	}
	if !below(four[0].Weight, one[0].Weight) {
		t.Errorf("four authorisations that all fit weigh %.3f bits, no less than one at "+
			"%.3f; a busy window has to remain a weaker starting point",
			four[0].Weight, one[0].Weight)
	}
}

// TestTrace_reportsWhatTheMatcherDidAndWhy is the hook the traces are built on,
// tested here rather than through a span so that what is asserted is the content
// rather than the exporter.
//
// The record has to explain rather than state. "Held" alone does not distinguish
// a case that sat in the review band from one that cleared the automatic
// threshold and was held anyway for want of a clear alternative, and those want
// different things done about them.
func TestTrace_reportsWhatTheMatcherDidAndWhy(t *testing.T) {
	s := &fakeStore{txns: []*Transaction{
		{ID: "x", AccountID: "acct", Date: onDay(5), AmountCents: -1200, PayeeName: "Netflix"},
		{ID: "y", AccountID: "acct", Date: onDay(11), AmountCents: -4200, PayeeName: "Edeka"},
	}}

	var batches []BatchTrace
	var decisions []DecisionTrace
	pol := Policy{
		HoldForReview: true, TolerancePercent: 25, ToleranceCents: 5000,
		OnBatch:    func(b BatchTrace) { batches = append(batches, b) },
		OnDecision: func(d DecisionTrace) { decisions = append(decisions, d) },
	}

	out, err := Reconcile(context.Background(), s, "acct", ImportedFields{
		Date: onDay(12), AmountCents: -1000, PayeeName: "Netflix",
	}, nil, pol)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(batches) != 1 {
		t.Fatalf("%d batch records, want 1", len(batches))
	}
	if batches[0].Incoming != 1 || batches[0].Weighed != 2 {
		t.Errorf("the batch weighed %d candidates for %d transactions, want 2 for 1",
			batches[0].Weighed, batches[0].Incoming)
	}

	if len(decisions) != 1 {
		t.Fatalf("%d decision records, want 1", len(decisions))
	}
	d := decisions[0]
	t.Logf("%s: %s (weight %+.3f, P=%.4f, window %d, adoptable %d, plausible %d)",
		d.Outcome, d.Reason, d.Best.Weight, d.Best.Probability, d.Window, d.Adoptable, d.Plausible)

	if d.Outcome != out.Name() {
		t.Errorf("the record says %q and the outcome is %q", d.Outcome, out.Name())
	}
	if d.Reason == "" {
		t.Error("no reason recorded; the outcome alone does not say which rule settled it")
	}
	if d.Window != 2 || d.Adoptable != 2 {
		t.Errorf("window %d, adoptable %d, want 2 and 2", d.Window, d.Adoptable)
	}
	if d.Best == nil || d.RunnerUp == nil {
		t.Fatal("two candidates were weighed and the record names fewer")
	}
	if !below(d.RunnerUp.Weight, d.Best.Weight) {
		t.Errorf("the runner-up outweighs the best: %+.3f against %+.3f",
			d.RunnerUp.Weight, d.Best.Weight)
	}

	// The thresholds are carried so that a record read a month later can still be
	// interpreted against the policy that produced it.
	if d.AutoProbability != pol.autoProbability() || d.ReviewProbability != pol.reviewProbability() {
		t.Errorf("the record carries thresholds %.2f/%.2f and the policy is %.2f/%.2f",
			d.AutoProbability, d.ReviewProbability, pol.autoProbability(), pol.reviewProbability())
	}

	// And the evidence has to add up to the weight, or it explains something else.
	var sum float64
	for _, e := range d.Evidence {
		sum += e.Bits
	}
	if math.Abs(sum-d.Best.Weight) > 1e-9 {
		t.Errorf("the evidence sums to %+.9f and the weight is %+.9f; a term is missing",
			sum, d.Best.Weight)
	}
}

// TestTrace_isOptional keeps the hooks from becoming a requirement. Nothing in
// the matcher may depend on being watched.
func TestTrace_isOptional(t *testing.T) {
	s := &fakeStore{txns: []*Transaction{
		{ID: "x", AccountID: "acct", Date: onDay(12), AmountCents: -1000, PayeeName: "Netflix"},
	}}
	withHooks := Policy{HoldForReview: true, TolerancePercent: 25, ToleranceCents: 5000,
		OnBatch: func(BatchTrace) {}, OnDecision: func(DecisionTrace) {}}
	without := Policy{HoldForReview: true, TolerancePercent: 25, ToleranceCents: 5000}

	in := ImportedFields{Date: onDay(12), AmountCents: -1000, PayeeName: "Netflix"}
	a, err := Reconcile(context.Background(), s, "acct", in, nil, without)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	b, err := Reconcile(context.Background(), s, "acct", in, nil, withHooks)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if a.Name() != b.Name() {
		t.Errorf("watching changed the decision: %q without, %q with", a.Name(), b.Name())
	}
}
