package actual

import (
	"context"
	"testing"
	"time"

	"bankingsync/budget"
)

func newTestAdapter(t *testing.T) (*Adapter, *DB) {
	t.Helper()
	d := newTestDB(t)
	insertAccount(t, d, "a1", "Main")
	return NewAdapterForDB(d), d
}

func importedFields(d time.Time, cents int64, payee, ref string, cleared bool) budget.ImportedFields {
	return budget.ImportedFields{
		Date: d, AmountCents: cents, Currency: "EUR",
		PayeeName: payee, ImportedPayee: payee, ExternalRef: ref, Cleared: cleared,
	}
}

func TestAdapter_createThenListRoundtrip(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx := context.Background()
	when := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)

	created, err := a.Create(ctx, "a1", importedFields(when, -1250, "REWE MARKT", "ref-1", true))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	from, to := budget.WindowBounds(when)
	got, err := a.ListTransactions(ctx, "a1", from, to)
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d transactions, want 1", len(got))
	}
	if got[0].ID != created.ID {
		t.Errorf("ID: got %q, want %q", got[0].ID, created.ID)
	}
	if got[0].ExternalRef != "ref-1" {
		t.Errorf("ExternalRef: got %q, want ref-1", got[0].ExternalRef)
	}
	if got[0].AmountCents != -1250 {
		t.Errorf("AmountCents: got %d", got[0].AmountCents)
	}
	if !got[0].Cleared {
		t.Error("Cleared must survive the round trip")
	}
	if got[0].ImportedPayee != "REWE MARKT" {
		t.Errorf("ImportedPayee: got %q", got[0].ImportedPayee)
	}
}

func TestAdapter_listIsWindowed(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx := context.Background()
	when := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)

	if _, err := a.Create(ctx, "a1", importedFields(when.AddDate(0, 0, -30), -100, "Old", "old", true)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := a.Create(ctx, "a1", importedFields(when, -200, "New", "new", true)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	from, to := budget.WindowBounds(when)
	got, err := a.ListTransactions(ctx, "a1", from, to)
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	if len(got) != 1 || got[0].ExternalRef != "new" {
		t.Fatalf("the window must exclude the old row, got %d rows", len(got))
	}
}

func TestAdapter_findByExternalRefIsAccountScoped(t *testing.T) {
	a, d := newTestAdapter(t)
	insertAccount(t, d, "a2", "Other")
	ctx := context.Background()
	when := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)

	if _, err := a.Create(ctx, "a2", importedFields(when, -100, "Shop", "shared-ref", true)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := a.FindByExternalRef(ctx, "a1", "shared-ref")
	if err != nil {
		t.Fatalf("FindByExternalRef: %v", err)
	}
	if got != nil {
		t.Fatalf("a ref belonging to another account must not be found, got %q", got.ID)
	}

	got, err = a.FindByExternalRef(ctx, "a2", "shared-ref")
	if err != nil {
		t.Fatalf("FindByExternalRef: %v", err)
	}
	if got == nil {
		t.Fatal("the owning account must find its own ref")
	}
}

func TestAdapter_updateWritesEachField(t *testing.T) {
	a, d := newTestAdapter(t)
	ctx := context.Background()
	when := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)

	created, err := a.Create(ctx, "a1", budget.ImportedFields{Date: when, AmountCents: -1000})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	err = a.Update(ctx, created, budget.Patch{
		AmountCents:   budget.Int64(-1250),
		Notes:         budget.String("Trinkgeld"),
		ExternalRef:   budget.String("ref-9"),
		Cleared:       budget.Bool(true),
		PayeeName:     budget.String("Rewe Markt"),
		ImportedPayee: budget.String("REWE MARKT"),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	from, to := budget.WindowBounds(when)
	got, _ := a.ListTransactions(ctx, "a1", from, to)
	if len(got) != 1 {
		t.Fatalf("got %d rows", len(got))
	}
	row := got[0]
	if row.AmountCents != -1250 {
		t.Errorf("AmountCents: got %d, want -1250", row.AmountCents)
	}
	if row.Notes != "Trinkgeld" {
		t.Errorf("Notes: got %q", row.Notes)
	}
	if row.ExternalRef != "ref-9" {
		t.Errorf("ExternalRef: got %q", row.ExternalRef)
	}
	if !row.Cleared {
		t.Error("Cleared was not written")
	}
	if row.PayeeName != "Rewe Markt" {
		t.Errorf("PayeeName: got %q", row.PayeeName)
	}
	if row.ImportedPayee != "REWE MARKT" {
		t.Errorf("ImportedPayee: got %q", row.ImportedPayee)
	}

	if len(d.FlushChanges()) == 0 {
		t.Error("every write must queue sync messages, otherwise the server never learns about it")
	}
}

func TestAdapter_updateWithEmptyPatchIsNoop(t *testing.T) {
	a, d := newTestAdapter(t)
	ctx := context.Background()
	when := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)

	created, _ := a.Create(ctx, "a1", importedFields(when, -1000, "Shop", "ref-1", true))
	d.FlushChanges()

	if err := a.Update(ctx, created, budget.Patch{}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := len(d.FlushChanges()); got != 0 {
		t.Errorf("an empty patch must not touch the database, queued %d changes", got)
	}
}

func TestAdapter_updateRejectsUnknownTransaction(t *testing.T) {
	a, _ := newTestAdapter(t)
	stray := &budget.Transaction{ID: "never-seen", AccountID: "a1"}

	if err := a.Update(context.Background(), stray, budget.Patch{Notes: budget.String("x")}); err == nil {
		t.Fatal("updating a transaction the adapter never saw must fail loudly, not silently do nothing")
	}
}

func TestAdapter_readsReconciledFlag(t *testing.T) {
	a, d := newTestAdapter(t)
	ctx := context.Background()
	when := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)

	created, _ := a.Create(ctx, "a1", importedFields(when, -1000, "Shop", "ref-1", true))
	if _, err := d.sql.Exec(`UPDATE transactions SET reconciled = 1 WHERE id = ?`, created.ID); err != nil {
		t.Fatalf("mark reconciled: %v", err)
	}

	from, to := budget.WindowBounds(when)
	got, _ := a.ListTransactions(ctx, "a1", from, to)
	if len(got) != 1 {
		t.Fatalf("got %d rows", len(got))
	}
	if !got[0].Reconciled {
		t.Error("Reconciled must be read from the database, otherwise the write lock never engages")
	}
}

func TestAdapter_reconcileAdoptsManualEntryAgainstRealDB(t *testing.T) {
	a, d := newTestAdapter(t)
	ctx := context.Background()
	when := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)

	insertTxnAt(t, d, "manual", "a1", when.AddDate(0, 0, -2), -1000)

	out, err := budget.Reconcile(ctx, a, "a1", importedFields(when, -1000, "Rewe", "ref-1", true), nil, budget.Policy{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if out.Created {
		t.Fatal("a manual entry inside the window must be adopted, not duplicated")
	}
	if out.Transaction.ID != "manual" {
		t.Errorf("adopted %q, want manual", out.Transaction.ID)
	}
	if out.Transaction.ExternalRef != "ref-1" {
		t.Errorf("the adopted row must carry the bank ref, got %q", out.Transaction.ExternalRef)
	}
}

func TestAdapter_applyRulesUsesNativeTransaction(t *testing.T) {
	a, d := newTestAdapter(t)
	ctx := context.Background()
	when := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)

	if _, err := d.sql.Exec(
		`INSERT INTO rules (id, stage, conditions_op, conditions, actions, tombstone) VALUES (?, '', 'and', ?, ?, 0)`,
		"rule-1",
		`[{"field":"imported_payee","op":"contains","value":"REWE","type":"string"}]`,
		`[{"field":"notes","op":"set","value":"kategorisiert","type":"string"}]`,
	); err != nil {
		t.Fatalf("insert rule: %v", err)
	}

	created, err := a.Create(ctx, "a1", importedFields(when, -1000, "REWE MARKT", "ref-1", true))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	applied, err := a.ApplyRules(ctx, []*budget.Transaction{created})
	if err != nil {
		t.Fatalf("ApplyRules: %v", err)
	}
	if applied != 1 {
		t.Fatalf("applied %d rule actions, want 1", applied)
	}
	if created.Notes != "kategorisiert" {
		t.Errorf("the budget view must be refreshed after rules ran, got %q", created.Notes)
	}
}

func TestAdapter_clearedUpdateQueuesSyncMessage(t *testing.T) {
	a, d := newTestAdapter(t)
	ctx := context.Background()
	when := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)

	created, _ := a.Create(ctx, "a1", importedFields(when, -1000, "Shop", "ref-1", false))
	d.FlushChanges()

	if err := a.Update(ctx, created, budget.Patch{Cleared: budget.Bool(true)}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	var sawCleared bool
	for _, ch := range d.FlushChanges() {
		if ch.Dataset == "transactions" && ch.Row == created.ID && ch.Column == "cleared" {
			sawCleared = true
		}
	}
	if !sawCleared {
		t.Error("confirming a transaction must queue a sync message, otherwise the server never sees it")
	}
}

func TestSetOpeningBalance_createsAClearedSentinelRow(t *testing.T) {
	a, d := newTestAdapter(t)
	ctx := context.Background()

	ob := budget.OpeningBalance{
		AmountCents: -123456,
		Currency:    "EUR",
		Date:        time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC),
		Ref:         "bankingsync-opening-DE77",
		PayeeName:   "Starting Balance",
	}
	written, err := a.SetOpeningBalance(ctx, "a1", ob)
	if err != nil {
		t.Fatalf("SetOpeningBalance: %v", err)
	}
	if !written {
		t.Fatal("written = false on a fresh account")
	}

	var amount, cleared, tombstone int64
	var financialID string
	err = d.sql.QueryRow(
		`SELECT amount, cleared, tombstone, COALESCE(financial_id, '')
		   FROM transactions WHERE acct = 'a1'`,
	).Scan(&amount, &cleared, &tombstone, &financialID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if amount != -123456 {
		t.Errorf("amount: got %d, want -123456", amount)
	}
	if cleared != 1 {
		t.Error("the opening balance row is not cleared — budget.adoptable would let " +
			"an incoming transaction of the same amount swallow it")
	}
	if financialID != ob.Ref {
		t.Errorf("financial_id: got %q, want %q — without it the row is adoptable "+
			"and the write is not idempotent", financialID, ob.Ref)
	}
	if tombstone != 0 {
		t.Errorf("tombstone: got %d, want 0", tombstone)
	}
}

func TestSetOpeningBalance_secondCallIsANoop(t *testing.T) {
	a, d := newTestAdapter(t)
	ctx := context.Background()
	ob := budget.OpeningBalance{
		AmountCents: 100000, Currency: "EUR",
		Date: time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC),
		Ref:  "bankingsync-opening-DE77", PayeeName: "Starting Balance",
	}

	if _, err := a.SetOpeningBalance(ctx, "a1", ob); err != nil {
		t.Fatalf("first: %v", err)
	}
	ob.AmountCents = 999999
	written, err := a.SetOpeningBalance(ctx, "a1", ob)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if written {
		t.Error("written = true on the second call")
	}

	var n int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM transactions WHERE acct = 'a1'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("transactions: got %d, want 1 — the opening balance was written twice", n)
	}
}

// The row has to reach the Actual server, not just the local SQLite. A missing
// CRDT message leaves it visible on this machine and nowhere else.
func TestSetOpeningBalance_tracksChangesForSync(t *testing.T) {
	a, d := newTestAdapter(t)
	before := len(d.FlushChanges())

	if _, err := a.SetOpeningBalance(context.Background(), "a1", budget.OpeningBalance{
		AmountCents: 100000, Currency: "EUR",
		Date: time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC),
		Ref:  "r", PayeeName: "Starting Balance",
	}); err != nil {
		t.Fatalf("SetOpeningBalance: %v", err)
	}

	if got := len(d.FlushChanges()); got <= before {
		t.Errorf("no sync messages were tracked (%d -> %d); the opening balance would "+
			"exist locally and never reach the server", before, got)
	}
}

func TestAccountBalance_sumsTheAccount(t *testing.T) {
	a, _ := newTestAdapter(t)
	ctx := context.Background()

	if _, err := a.SetOpeningBalance(ctx, "a1", budget.OpeningBalance{
		AmountCents: 100000, Currency: "EUR",
		Date: time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC),
		Ref:  "r", PayeeName: "Starting Balance",
	}); err != nil {
		t.Fatalf("SetOpeningBalance: %v", err)
	}
	if _, err := a.Create(ctx, "a1",
		importedFields(time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), -2500, "Rewe", "t1", true)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := a.AccountBalance(ctx, "a1")
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if got != 97500 {
		t.Errorf("balance: got %d, want 97500", got)
	}
}

func TestAccountBalance_excludesTombstonedAndSplitParents(t *testing.T) {
	a, d := newTestAdapter(t)

	_, err := d.sql.Exec(`
		INSERT INTO transactions (id, acct, date, amount, cleared, tombstone, isParent, isChild, reconciled, sort_order)
		VALUES ('keep',   'a1', 20260714,  10000, 1, 0, 0, 0, 0, 1),
		       ('gone',   'a1', 20260715,  55555, 1, 1, 0, 0, 0, 2),
		       ('parent', 'a1', 20260716,  -3000, 1, 0, 1, 0, 0, 3),
		       ('childA', 'a1', 20260716,  -1000, 1, 0, 0, 1, 0, 4),
		       ('childB', 'a1', 20260716,  -2000, 1, 0, 0, 1, 0, 5)`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := a.AccountBalance(context.Background(), "a1")
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if got != 7000 {
		t.Errorf("balance: got %d, want 7000 (10000 − 1000 − 2000); a tombstoned row "+
			"or a double-counted split parent is included", got)
	}
}
