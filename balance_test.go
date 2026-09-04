package main

import (
	"testing"
	"time"

	"bankingsync/enablebanking"
	"bankingsync/store"
)

func d(day int) time.Time { return time.Date(2026, 8, day, 0, 0, 0, 0, time.UTC) }

func txn(status string, day int, cents int64) enablebanking.Transaction {
	return enablebanking.Transaction{Status: status, Date: d(day), AmountCents: cents, Currency: "EUR"}
}

func clbd(cents int64) enablebanking.Balance {
	return enablebanking.Balance{Type: "CLBD", AmountCents: cents, Currency: "EUR"}
}

func TestOpeningCents_subtractsOnlyBookedTransactions(t *testing.T) {
	fetched := []enablebanking.Transaction{
		txn("BOOK", 10, -20000),
		txn("BOOK", 11, 5000),
		txn("PDNG", 12, -3000),
	}

	got := OpeningCents(clbd(100000), fetched)
	if got != 115000 {
		t.Errorf("opening: got %d, want 115000 (100000 − (−20000 + 5000)); a pending "+
			"transaction was folded in, but a booked balance does not contain one", got)
	}
}

// A CLBD reported as of yesterday does not contain today's bookings, so
// subtracting them would make the opening balance too low by exactly those.
func TestOpeningCents_cutsTheSumAtTheReferenceDate(t *testing.T) {
	picked := clbd(100000)
	picked.ReferenceDate = d(11)

	fetched := []enablebanking.Transaction{
		txn("BOOK", 10, -20000),
		txn("BOOK", 11, -5000),
		txn("BOOK", 12, -7000),
	}

	got := OpeningCents(picked, fetched)
	if got != 125000 {
		t.Errorf("opening: got %d, want 125000 — the transaction after the reference "+
			"date must not be subtracted", got)
	}
}

func TestPendingCents_totalsOnlyPending(t *testing.T) {
	got := PendingCents([]enablebanking.Transaction{
		txn("BOOK", 10, -20000),
		txn("PDNG", 11, -3000),
		txn("PDNG", 12, -1500),
	})
	if got != -4500 {
		t.Errorf("pending: got %d, want -4500", got)
	}
}

func TestOpeningDate_sitsBeforeTheEarliestTransaction(t *testing.T) {
	got := OpeningDate(d(10), []enablebanking.Transaction{txn("BOOK", 7, -100)})
	if !got.Equal(d(6)) {
		t.Errorf("date: got %s, want %s — a bank that returns entries older than "+
			"date_from must not end up before the opening balance",
			got.Format("2006-01-02"), d(6).Format("2006-01-02"))
	}

	got = OpeningDate(d(10), nil)
	if !got.Equal(d(9)) {
		t.Errorf("date with no transactions: got %s, want %s", got.Format("2006-01-02"), d(9).Format("2006-01-02"))
	}
}

func TestDeferReason_namesEveryUnsafeCondition(t *testing.T) {
	tomorrow := time.Now().UTC().AddDate(0, 0, 1)
	future := enablebanking.Transaction{Status: "BOOK", Date: tomorrow, AmountCents: -100, Currency: "EUR"}
	foreign := enablebanking.Transaction{Status: "BOOK", Date: d(10), AmountCents: -100, Currency: "USD"}

	cases := []struct {
		name        string
		dropped     int
		writeFailed bool
		interrupted bool
		held        int
		fetched     []enablebanking.Transaction
		currency    string
		wantDefer   bool
	}{
		{name: "clean", currency: "EUR", fetched: []enablebanking.Transaction{txn("BOOK", 10, -100)}},
		{name: "dropped transaction", dropped: 1, currency: "EUR", wantDefer: true},
		{name: "write failure", writeFailed: true, currency: "EUR", wantDefer: true},
		{name: "interrupted run", interrupted: true, currency: "EUR", wantDefer: true},
		// The opening balance is written once and never revised. A transaction
		// still awaiting a decision is not in the budget, so writing the balance
		// now would absorb it — and it would stay absorbed after the decision.
		{name: "transaction awaiting a decision", held: 1, currency: "EUR", wantDefer: true},
		{name: "future dated", fetched: []enablebanking.Transaction{future}, currency: "EUR", wantDefer: true},
		{name: "foreign currency txn", fetched: []enablebanking.Transaction{foreign}, currency: "EUR", wantDefer: true},
		{name: "account currency mismatch", currency: "CHF", wantDefer: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deferReason(tc.dropped, tc.writeFailed, tc.interrupted, tc.held,
				clbd(100000), tc.fetched, tc.currency)
			if tc.wantDefer && got == "" {
				t.Error("want a defer reason, got none — this would write a wrong opening balance")
			}
			if !tc.wantDefer && got != "" {
				t.Errorf("want no defer, got %q", got)
			}
		})
	}
}

func TestSameBalance_detectsMovement(t *testing.T) {
	first := clbd(100000)
	first.LastCommitted = "ref-9"

	same := []enablebanking.Balance{first}
	if !sameBalance(first, same, "EUR") {
		t.Error("an unchanged balance was reported as moved")
	}

	moved := clbd(99000)
	moved.LastCommitted = "ref-9"
	if sameBalance(first, []enablebanking.Balance{moved}, "EUR") {
		t.Error("a changed amount was reported as unchanged")
	}

	// Two offsetting entries leave the amount alone; only the committed marker
	// shows that the account moved underneath us.
	offset := clbd(100000)
	offset.LastCommitted = "ref-11"
	if sameBalance(first, []enablebanking.Balance{offset}, "EUR") {
		t.Error("an account that moved by offsetting entries was reported as unchanged")
	}

	if sameBalance(first, nil, "EUR") {
		t.Error("a missing second reading was treated as agreement")
	}
}

func TestShouldNotifyDrift_requiresTwoConsecutiveRuns(t *testing.T) {
	const threshold = 1000

	first := store.BankAccount{DriftState: store.DriftOK}
	if ShouldNotifyDrift(first, -5000, threshold) {
		t.Error("an alarm fired on the first run; a settling pending looks exactly " +
			"like this for one cycle")
	}

	second := store.BankAccount{DriftState: store.DriftAlert, DriftCents: -5000}
	if !ShouldNotifyDrift(second, -5000, threshold) {
		t.Error("persistent drift did not raise an alarm")
	}

	if ShouldNotifyDrift(second, -500, threshold) {
		t.Error("drift below the threshold raised an alarm")
	}
	if ShouldNotifyDrift(second, -5000, 0) {
		t.Error("a zero threshold must disable the email entirely")
	}

	// Below threshold last time, above now: still only one qualifying run.
	nearMiss := store.BankAccount{DriftState: store.DriftAlert, DriftCents: -100}
	if ShouldNotifyDrift(nearMiss, -5000, threshold) {
		t.Error("the previous run was below the threshold, so this is the first " +
			"qualifying run")
	}
}

func itav(cents int64) enablebanking.Balance {
	return enablebanking.Balance{Type: "ITAV", AmountCents: cents, Currency: "EUR"}
}

// The two balance families need different arithmetic. Treating an available
// balance like a booked one double-counts every outstanding authorisation.
func TestOpeningCents_followsWhatTheBalanceAlreadyContains(t *testing.T) {
	fetched := []enablebanking.Transaction{
		txn("BOOK", 10, -20000),
		txn("PDNG", 12, -3000),
	}

	if got := OpeningCents(clbd(100000), fetched); got != 120000 {
		t.Errorf("booked: got %d, want 120000 (only the booked −20000 comes off)", got)
	}
	if got := OpeningCents(itav(100000), fetched); got != 123000 {
		t.Errorf("available: got %d, want 123000 (both −20000 and −3000 come off, "+
			"because the hold is already reflected in the balance)", got)
	}
}

func TestExpectedTotal_addsPendingOnlyWhenTheBalanceLacksThem(t *testing.T) {
	fetched := []enablebanking.Transaction{
		txn("BOOK", 10, -20000),
		txn("PDNG", 12, -3000),
	}

	if got := ExpectedTotal(clbd(100000), fetched); got != 97000 {
		t.Errorf("booked: got %d, want 97000 — the budget leads a booked balance by "+
			"the outstanding pendings", got)
	}
	if got := ExpectedTotal(itav(100000), fetched); got != 100000 {
		t.Errorf("available: got %d, want 100000 — adding the pending again would "+
			"report permanent drift on every Revolut account", got)
	}
}
