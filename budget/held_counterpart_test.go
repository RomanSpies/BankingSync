package budget

import (
	"context"
	"testing"
	"time"
)

func heldPolicy() Policy {
	return Policy{HoldForReview: true, TolerancePercent: 25, ToleranceCents: 5000}
}

func heldAuthorisation(id string, d time.Time, cents int64, payee, notes string) *Transaction {
	return &Transaction{ID: id, AccountID: "a1", Date: d, AmountCents: cents, Currency: "EUR",
		PayeeName: payee, ImportedPayee: payee, Notes: notes, Provisional: true}
}

func TestReconcileBatch_aBookingSettlesAHeldAuthorisationWithOneNewRow(t *testing.T) {
	d := day(2026, time.September, 30)
	auth := heldAuthorisation("held:7", d, -2143, "Edeka", "NR 268")
	s := &fakeStore{}

	out, err := ReconcileBatch(context.Background(), s, "a1",
		[]ImportedFields{imported(d.AddDate(0, 0, 7), -2143, "Edeka", "")}, nil, []*Transaction{auth}, heldPolicy())
	if err != nil {
		t.Fatalf("ReconcileBatch: %v", err)
	}
	o := out[0]
	if o.Counterpart != auth {
		t.Fatalf("outcome %s with counterpart %v; the booking should have settled the held authorisation", o.Name(), o.Counterpart)
	}
	if len(s.updates) != 0 {
		t.Fatalf("%d updates; a held authorisation is not in the budget and cannot be patched", len(s.updates))
	}
	if s.creates != 1 || o.Transaction == nil || !o.Created {
		t.Fatalf("creates=%d; the pair must become exactly one new row", s.creates)
	}
	if !o.Transaction.Date.Equal(d) || o.Transaction.AmountCents != -2143 || !o.Transaction.Cleared {
		t.Errorf("new row %+v; want the authorisation's day, the booking's amount, booked", o.Transaction)
	}
	if o.Transaction.Notes != "NR 268" {
		t.Errorf("notes %q; the authorisation's notes are kept as a merge would keep them", o.Transaction.Notes)
	}
}

func TestReconcileBatch_aHeldAuthorisationIsNeverOfferedToAnotherAuthorisation(t *testing.T) {
	d := day(2026, time.September, 30)
	auth := heldAuthorisation("held:7", d, -2143, "Edeka", "")
	in := imported(d.AddDate(0, 0, 1), -2143, "Edeka", "")
	in.Cleared = false

	out, err := ReconcileBatch(context.Background(), &fakeStore{}, "a1",
		[]ImportedFields{in}, nil, []*Transaction{auth}, heldPolicy())
	if err != nil {
		t.Fatalf("ReconcileBatch: %v", err)
	}
	if out[0].Counterpart != nil || out[0].Best != nil {
		t.Fatalf("an authorisation was weighed against a held authorisation; only a booking settles one")
	}
}

func TestReconcileBatch_aHeldAuthorisationOutsideTheWindowIsNotOffered(t *testing.T) {
	d := day(2026, time.September, 30)
	auth := heldAuthorisation("held:7", d.AddDate(0, 0, -8), -2143, "Edeka", "")

	out, err := ReconcileBatch(context.Background(), &fakeStore{}, "a1",
		[]ImportedFields{imported(d, -2143, "Edeka", "")}, nil, []*Transaction{auth}, heldPolicy())
	if err != nil {
		t.Fatalf("ReconcileBatch: %v", err)
	}
	if out[0].Best != nil {
		t.Fatalf("a held authorisation eight days earlier was offered; the window reaches seven")
	}
}

func TestReconcileBatch_aDriftedBookingIsHeldWithTheHeldAuthorisationOnOffer(t *testing.T) {
	d := day(2026, time.September, 30)
	auth := heldAuthorisation("held:7", d, -2143, "Edeka", "")
	s := &fakeStore{txns: []*Transaction{
		{ID: "old-1", AccountID: "a1", Date: d.AddDate(0, 0, 2), AmountCents: -500, PayeeName: "Edeka"},
		{ID: "old-2", AccountID: "a1", Date: d.AddDate(0, 0, 3), AmountCents: -9000, PayeeName: "Edeka"},
	}}

	out, err := ReconcileBatch(context.Background(), s, "a1",
		[]ImportedFields{imported(d.AddDate(0, 0, 7), -2200, "Edeka", "")}, nil, []*Transaction{auth}, heldPolicy())
	if err != nil {
		t.Fatalf("ReconcileBatch: %v", err)
	}
	o := out[0]
	if o.Name() != "held" {
		t.Fatalf("outcome %s; a drifted amount a week out is a question for a person", o.Name())
	}
	offered := false
	for _, c := range o.Held {
		offered = offered || c.Transaction == auth
	}
	if !offered {
		t.Fatal("the held authorisation is not among the candidates put to the person")
	}
	if s.creates != 0 || len(s.updates) != 0 {
		t.Fatalf("a held decision wrote to the budget: creates=%d updates=%d", s.creates, len(s.updates))
	}
}

func TestCounterpartFields_keepTheAuthorisationDateAndItsNotes(t *testing.T) {
	d := day(2026, time.September, 30)
	booking := imported(d.AddDate(0, 0, 7), -2143, "EDEKA AKTIV MARKT Gelnhausen", "b-1")
	booking.Notes = "booked text"

	got := CounterpartFields(booking, heldAuthorisation("held:7", d, -2100, "Visa Edeka", "auth text"))
	if !got.Date.Equal(d) || got.Notes != "auth text" {
		t.Errorf("got date %s notes %q; want the authorisation's day and its notes", got.Date, got.Notes)
	}
	if got.AmountCents != -2143 || got.PayeeName != "EDEKA AKTIV MARKT Gelnhausen" || got.ExternalRef != "b-1" || !got.Cleared {
		t.Errorf("got %+v; everything but day and notes comes from the booking", got)
	}

	got = CounterpartFields(booking, heldAuthorisation("held:7", d, -2100, "Visa Edeka", ""))
	if got.Notes != "booked text" {
		t.Errorf("notes %q; an authorisation without notes leaves the booking's", got.Notes)
	}
}

func TestOutcome_aSettledHeldAuthorisationReadsAsAnAdoption(t *testing.T) {
	auth := heldAuthorisation("held:7", day(2026, time.September, 30), -2143, "Edeka", "")
	o := Outcome{Transaction: &Transaction{ID: "new"}, Created: true, Counterpart: auth,
		Shadow: &ShadowOutcome{Outcome: "adopted", CandidateID: "held:7"}}

	if o.Name() != "adopted" || !o.Adopted() {
		t.Errorf("Name %q Adopted %v; settling a held authorisation is an adoption, not an import", o.Name(), o.Adopted())
	}
	if o.Differs() {
		t.Error("a shadow that made the same pairing is reported as disagreeing")
	}
}

func TestInterchangeable_aHeldAuthorisationNeverStandsInForARowOfTheSameFigures(t *testing.T) {
	d := day(2026, time.September, 30)
	row := &Transaction{ID: "r", Date: d, AmountCents: -2143, PayeeName: "Edeka"}
	auth := heldAuthorisation("held:7", d, -2143, "Edeka", "")

	if interchangeable(row, auth) {
		t.Fatal("a budget row and a held authorisation leave different budgets and must not count as interchangeable")
	}
}
