package actual

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

const testSchema = `
CREATE TABLE accounts (
    id       TEXT PRIMARY KEY,
    name     TEXT,
    offbudget INTEGER DEFAULT 0,
    closed   INTEGER DEFAULT 0,
    tombstone INTEGER DEFAULT 0
);
CREATE TABLE payees (
    id           TEXT PRIMARY KEY,
    name         TEXT,
    tombstone    INTEGER DEFAULT 0,
    transfer_acct TEXT
);
CREATE TABLE payee_mapping (
    id       TEXT PRIMARY KEY,
    targetId TEXT
);
CREATE TABLE transactions (
    id                   TEXT PRIMARY KEY,
    acct                 TEXT,
    date                 INTEGER,
    amount               INTEGER,
    description          TEXT,
    notes                TEXT,
    financial_id         TEXT,
    cleared              INTEGER DEFAULT 0,
    tombstone            INTEGER DEFAULT 0,
    isParent             INTEGER DEFAULT 0,
    isChild              INTEGER DEFAULT 0,
    reconciled           INTEGER DEFAULT 0,
    sort_order           REAL,
    imported_description TEXT,
    category             TEXT
);
CREATE TABLE messages_clock (
    id    TEXT PRIMARY KEY,
    clock TEXT
);
CREATE TABLE rules (
    id            TEXT PRIMARY KEY,
    stage         TEXT,
    conditions_op TEXT,
    conditions    TEXT,
    actions       TEXT,
    tombstone     INTEGER DEFAULT 0
);
`

func newTestDB(t *testing.T) *DB {
	t.Helper()
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := raw.Exec(testSchema); err != nil {
		_ = raw.Close()
		t.Fatalf("create schema: %v", err)
	}
	d := &DB{
		sql:         raw,
		payeeByName: make(map[string]string),
		payeeByID:   make(map[string]string),
		acctByName:  make(map[string]string),
	}
	t.Cleanup(func() { _ = raw.Close() })
	return d
}

func insertAccount(t *testing.T, d *DB, id, name string) {
	t.Helper()
	_, err := d.sql.Exec(
		`INSERT INTO accounts (id, name, offbudget, closed, tombstone) VALUES (?, ?, 0, 0, 0)`,
		id, name,
	)
	if err != nil {
		t.Fatalf("insertAccount: %v", err)
	}
}

func insertPayee(t *testing.T, d *DB, id, name string) {
	t.Helper()
	_, err := d.sql.Exec(
		`INSERT INTO payees (id, name, tombstone) VALUES (?, ?, 0)`, id, name,
	)
	if err != nil {
		t.Fatalf("insertPayee: %v", err)
	}
}

func TestDateToInt(t *testing.T) {
	cases := []struct {
		t    time.Time
		want int
	}{
		{time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), 20240101},
		{time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC), 20241231},
		{time.Date(2000, 6, 9, 0, 0, 0, 0, time.UTC), 20000609},
	}
	for _, tc := range cases {
		got := dateToInt(tc.t)
		if got != tc.want {
			t.Errorf("dateToInt(%v) = %d, want %d", tc.t, got, tc.want)
		}
	}
}

func TestIntToDate(t *testing.T) {
	cases := []struct {
		n    int
		want time.Time
	}{
		{20240101, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)},
		{20241231, time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC)},
		{20000609, time.Date(2000, 6, 9, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		got := intToDate(tc.n)
		if !got.Equal(tc.want) {
			t.Errorf("intToDate(%d) = %v, want %v", tc.n, got, tc.want)
		}
	}
}

func TestDateRoundtrip(t *testing.T) {
	dates := []time.Time{
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(1999, 12, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 7, 4, 0, 0, 0, 0, time.UTC),
	}
	for _, d := range dates {
		if got := intToDate(dateToInt(d)); !got.Equal(d) {
			t.Errorf("roundtrip(%v): got %v", d, got)
		}
	}
}

func TestIsSafeIdentifier(t *testing.T) {
	valid := []string{"amount", "isParent", "description123", "a", "ABC", "col_name"}
	for _, s := range valid {
		if !isSafeIdentifier(s) {
			t.Errorf("isSafeIdentifier(%q) = false, want true", s)
		}
	}
	invalid := []string{"", "drop table", "a;b", "a\"b", "a-b", "a.b", "a/b"}
	for _, s := range invalid {
		if isSafeIdentifier(s) {
			t.Errorf("isSafeIdentifier(%q) = true, want false", s)
		}
	}
}

func TestDecimalToCents(t *testing.T) {
	cases := []struct {
		input string
		want  int64
		isErr bool
	}{
		{"12.34", 1234, false},
		{"0.01", 1, false},
		{"-5.00", -500, false},
		{"100", 10000, false},
		{"0", 0, false},
		{"abc", 0, true},
		{"", 0, true},
	}
	for _, tc := range cases {
		got, err := DecimalToCents(tc.input)
		if tc.isErr {
			if err == nil {
				t.Errorf("DecimalToCents(%q): expected error, got nil", tc.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("DecimalToCents(%q): unexpected error: %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("DecimalToCents(%q) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

func TestGetOrCreateAccount_creates(t *testing.T) {
	d := newTestDB(t)
	acct, err := d.GetOrCreateAccount("Checking")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acct.Name != "Checking" {
		t.Errorf("Name: got %q, want %q", acct.Name, "Checking")
	}
	if acct.ID == "" {
		t.Error("expected non-empty ID")
	}

	var count int
	_ = d.sql.QueryRow(`SELECT COUNT(*) FROM accounts WHERE id = ?`, acct.ID).Scan(&count)
	if count != 1 {
		t.Error("account not found in DB after creation")
	}
}

func TestGetOrCreateAccount_idempotent(t *testing.T) {
	d := newTestDB(t)
	a1, err := d.GetOrCreateAccount("Savings")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	a2, err := d.GetOrCreateAccount("Savings")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if a1.ID != a2.ID {
		t.Errorf("expected same ID, got %q vs %q", a1.ID, a2.ID)
	}
}

func TestGetOrCreateAccount_fromCache(t *testing.T) {
	d := newTestDB(t)
	insertAccount(t, d, "acct-001", "Main")

	if err := d.warmCaches(); err != nil {
		t.Fatalf("warmCaches: %v", err)
	}
	acct, err := d.GetOrCreateAccount("Main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acct.ID != "acct-001" {
		t.Errorf("expected ID acct-001 from cache, got %q", acct.ID)
	}
}

func TestGetOrCreateAccount_tracksChanges(t *testing.T) {
	d := newTestDB(t)
	_, err := d.GetOrCreateAccount("New Account")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	changes := d.FlushChanges()
	if len(changes) == 0 {
		t.Error("expected tracked changes after creating account")
	}

	fields := make(map[string]bool)
	for _, ch := range changes {
		if ch.Dataset == "accounts" {
			fields[ch.Column] = true
		}
	}
	for _, required := range []string{"name", "offbudget", "closed"} {
		if !fields[required] {
			t.Errorf("expected tracked field %q, got changes: %+v", required, changes)
		}
	}
}

func TestGetOrCreatePayee_creates(t *testing.T) {
	d := newTestDB(t)
	id, err := d.getOrCreatePayee("ACME Corp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty ID")
	}

	if d.payeeByName["ACME Corp"] != id {
		t.Error("payee not in cache after creation")
	}
	if d.payeeByID[id] != "ACME Corp" {
		t.Error("payee ID not in reverse cache after creation")
	}
}

func TestGetOrCreatePayee_idempotent(t *testing.T) {
	d := newTestDB(t)
	id1, _ := d.getOrCreatePayee("Landlord")
	id2, _ := d.getOrCreatePayee("Landlord")
	if id1 != id2 {
		t.Errorf("expected same ID, got %q vs %q", id1, id2)
	}
}

func TestGetOrCreatePayee_fromCache(t *testing.T) {
	d := newTestDB(t)
	insertPayee(t, d, "p-existing", "Supermarket")
	if err := d.warmCaches(); err != nil {
		t.Fatalf("warmCaches: %v", err)
	}
	id, err := d.getOrCreatePayee("Supermarket")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "p-existing" {
		t.Errorf("expected p-existing from cache, got %q", id)
	}
}

func TestResolvePayeeName(t *testing.T) {
	d := newTestDB(t)
	d.payeeByID["pid-1"] = "My Payee"
	if got := d.resolvePayeeName("pid-1"); got != "My Payee" {
		t.Errorf("got %q, want %q", got, "My Payee")
	}
	if got := d.resolvePayeeName("missing"); got != "" {
		t.Errorf("expected empty string for missing ID, got %q", got)
	}
}

func TestCreateTransaction(t *testing.T) {
	d := newTestDB(t)
	insertAccount(t, d, "acct-1", "Checking")
	acct := &Account{ID: "acct-1", Name: "Checking"}
	date := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)

	txn, err := d.CreateTransaction(date, acct, "Netflix", "Subscription", -1500, true, "", "")
	if err != nil {
		t.Fatalf("CreateTransaction error: %v", err)
	}
	if txn.ID == "" {
		t.Error("expected non-empty transaction ID")
	}
	if txn.AmountCents != -1500 {
		t.Errorf("AmountCents: got %d, want -1500", txn.AmountCents)
	}
	if !txn.Date.Equal(date) {
		t.Errorf("Date: got %v, want %v", txn.Date, date)
	}
	if txn.PayeeName != "Netflix" {
		t.Errorf("PayeeName: got %q, want Netflix", txn.PayeeName)
	}
	if !txn.Cleared {
		t.Error("expected Cleared = true")
	}

	var dbDate, dbAmount int
	var dbPayee string
	err = d.sql.QueryRow(
		`SELECT date, amount, COALESCE(description,'') FROM transactions WHERE id = ?`, txn.ID,
	).Scan(&dbDate, &dbAmount, &dbPayee)
	if err != nil {
		t.Fatalf("row not found in DB: %v", err)
	}
	if dbDate != 20240615 {
		t.Errorf("DB date: got %d, want 20240615", dbDate)
	}
	if dbAmount != -1500 {
		t.Errorf("DB amount: got %d, want -1500", dbAmount)
	}
}

func TestCreateTransaction_noPayee(t *testing.T) {
	d := newTestDB(t)
	insertAccount(t, d, "acct-1", "Checking")
	acct := &Account{ID: "acct-1"}
	date := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	txn, err := d.CreateTransaction(date, acct, "", "No payee", 500, false, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if txn.PayeeID != "" {
		t.Errorf("expected empty payee ID, got %q", txn.PayeeID)
	}
}

func TestCreateTransaction_tracksChanges(t *testing.T) {
	d := newTestDB(t)
	insertAccount(t, d, "acct-1", "Main")
	acct := &Account{ID: "acct-1"}
	_, err := d.CreateTransaction(time.Now(), acct, "Shop", "note", -200, true, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	changes := d.FlushChanges()
	fields := make(map[string]bool)
	for _, ch := range changes {
		if ch.Dataset == "transactions" {
			fields[ch.Column] = true
		}
	}
	for _, f := range []string{"acct", "date", "amount", "cleared"} {
		if !fields[f] {
			t.Errorf("expected tracked field %q", f)
		}
	}
}

func TestGetTransactions(t *testing.T) {
	d := newTestDB(t)
	insertAccount(t, d, "acct-1", "Checking")
	insertPayee(t, d, "p-1", "Amazon")
	if err := d.warmCaches(); err != nil {
		t.Fatalf("warmCaches: %v", err)
	}
	acct := &Account{ID: "acct-1"}

	_, _ = d.CreateTransaction(time.Date(2024, 1, 10, 0, 0, 0, 0, time.UTC), acct, "Amazon", "Books", -999, true, "", "")
	_, _ = d.CreateTransaction(time.Date(2024, 1, 20, 0, 0, 0, 0, time.UTC), acct, "Amazon", "Kindle", -499, true, "", "")

	_, _ = d.sql.Exec(
		`INSERT INTO transactions (id,acct,date,amount,tombstone,isParent,isChild,reconciled) VALUES ('dead','acct-1',20240115,-100,1,0,0,0)`,
	)

	txns, err := d.GetTransactions("acct-1")
	if err != nil {
		t.Fatalf("GetTransactions error: %v", err)
	}
	if len(txns) != 2 {
		t.Errorf("expected 2 transactions, got %d", len(txns))
	}
}

func TestGetTransactions_payeeNameResolved(t *testing.T) {
	d := newTestDB(t)
	insertAccount(t, d, "a1", "Main")
	if err := d.warmCaches(); err != nil {
		t.Fatalf("warmCaches: %v", err)
	}
	acct := &Account{ID: "a1"}
	txn, _ := d.CreateTransaction(time.Now(), acct, "Spotify", "", -999, false, "", "")

	txns, err := d.GetTransactions("a1")
	if err != nil {
		t.Fatalf("GetTransactions error: %v", err)
	}
	if len(txns) == 0 {
		t.Fatal("expected at least one transaction")
	}
	found := false
	for _, tx := range txns {
		if tx.ID == txn.ID {
			if tx.PayeeName != "Spotify" {
				t.Errorf("PayeeName: got %q, want Spotify", tx.PayeeName)
			}
			found = true
		}
	}
	if !found {
		t.Error("created transaction not found in GetTransactions result")
	}
}

func TestUpdateTransactionCleared(t *testing.T) {
	d := newTestDB(t)
	insertAccount(t, d, "a1", "Checking")
	acct := &Account{ID: "a1"}
	txn, _ := d.CreateTransaction(time.Now(), acct, "", "", -100, false, "", "")

	if err := d.UpdateTransactionCleared(txn); err != nil {
		t.Fatalf("UpdateTransactionCleared error: %v", err)
	}
	if !txn.Cleared {
		t.Error("expected Cleared = true after update")
	}

	var cleared int
	_ = d.sql.QueryRow(`SELECT cleared FROM transactions WHERE id = ?`, txn.ID).Scan(&cleared)
	if cleared != 1 {
		t.Errorf("DB cleared: got %d, want 1", cleared)
	}

	changes := d.FlushChanges()
	found := false
	for _, ch := range changes {
		if ch.Dataset == "transactions" && ch.Column == "cleared" {
			found = true
		}
	}
	if !found {
		t.Error("expected cleared field to be tracked after UpdateTransactionCleared")
	}
}

func TestReconcileTransaction_createsNew(t *testing.T) {
	d := newTestDB(t)
	insertAccount(t, d, "a1", "Main")
	acct := &Account{ID: "a1"}
	date := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)

	txn, wasCreated, err := d.ReconcileTransaction(date, acct, "Shop", "note", -500, true, "", "", nil)
	if err != nil {
		t.Fatalf("ReconcileTransaction error: %v", err)
	}
	if !wasCreated {
		t.Error("expected wasCreated = true when no match exists")
	}
	if txn.AmountCents != -500 {
		t.Errorf("AmountCents: got %d, want -500", txn.AmountCents)
	}
}

func TestReconcileTransaction_matchesExisting(t *testing.T) {
	d := newTestDB(t)
	insertAccount(t, d, "a1", "Main")
	acct := &Account{ID: "a1"}
	date := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)

	existing, _ := d.CreateTransaction(date, acct, "Netflix", "Sub", -1500, true, "", "")
	d.FlushChanges()

	txn, changed, err := d.ReconcileTransaction(date, acct, "Netflix", "Sub", -1500, true, "", "", nil)
	if err != nil {
		t.Fatalf("ReconcileTransaction error: %v", err)
	}

	if changed {
		t.Error("expected changed = false when match found and nothing differs")
	}
	if txn.ID != existing.ID {
		t.Errorf("expected matched ID %q, got %q", existing.ID, txn.ID)
	}
}

func TestReconcileTransaction_matchUpdatesFields(t *testing.T) {
	d := newTestDB(t)
	insertAccount(t, d, "a1", "Main")
	acct := &Account{ID: "a1"}
	date := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)

	existing, _ := d.CreateTransaction(date, acct, "Netflix", "", -1500, false, "", "")
	d.FlushChanges()

	txn, changed, err := d.ReconcileTransaction(date, acct, "Netflix", "Sub", -1500, true, "", "", nil)
	if err != nil {
		t.Fatalf("ReconcileTransaction error: %v", err)
	}
	if !changed {
		t.Error("expected changed = true when notes/cleared differ")
	}
	if txn.ID != existing.ID {
		t.Errorf("expected matched ID %q, got %q", existing.ID, txn.ID)
	}
	if txn.Notes != "Sub" {
		t.Errorf("Notes: got %q, want Sub", txn.Notes)
	}
}

func TestReconcileTransaction_alreadyMatchedExcluded(t *testing.T) {
	d := newTestDB(t)
	insertAccount(t, d, "a1", "Main")
	acct := &Account{ID: "a1"}
	date := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)

	existing, _ := d.CreateTransaction(date, acct, "Netflix", "", -1500, false, "", "")
	d.FlushChanges()

	txn, wasCreated, err := d.ReconcileTransaction(date, acct, "Netflix", "", -1500, true, "", "", []*Transaction{existing})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !wasCreated {
		t.Error("expected a new transaction since the only match was excluded")
	}
	if txn.ID == existing.ID {
		t.Error("should have created a new transaction, not reused the excluded one")
	}
}

func TestMatchTransaction_withinSevenDays(t *testing.T) {
	d := newTestDB(t)
	insertAccount(t, d, "a1", "Main")
	acct := &Account{ID: "a1"}

	base := time.Date(2024, 5, 10, 0, 0, 0, 0, time.UTC)

	existing, _ := d.CreateTransaction(base, acct, "Gym", "", -3000, false, "", "")
	d.FlushChanges()

	queryDate := base.AddDate(0, 0, 3)
	match := d.matchTransaction(queryDate, acct, "Gym", -3000, "", nil)
	if match == nil {
		t.Fatal("expected a match within 7-day window")
	}
	if match.ID != existing.ID {
		t.Errorf("matched wrong transaction: got %q, want %q", match.ID, existing.ID)
	}
}

func TestMatchTransaction_beyondSevenDays(t *testing.T) {
	d := newTestDB(t)
	insertAccount(t, d, "a1", "Main")
	acct := &Account{ID: "a1"}

	base := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	_, _ = d.CreateTransaction(base, acct, "Gym", "", -3000, false, "", "")
	d.FlushChanges()

	queryDate := base.AddDate(0, 0, 8)
	match := d.matchTransaction(queryDate, acct, "Gym", -3000, "", nil)
	if match != nil {
		t.Error("expected no match beyond 7-day window")
	}
}

func TestMatchTransaction_payeePreferred(t *testing.T) {
	d := newTestDB(t)
	insertAccount(t, d, "a1", "Main")
	acct := &Account{ID: "a1"}

	date := time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)

	t1, _ := d.CreateTransaction(date, acct, "ACME", "", -500, false, "", "")
	t2, _ := d.CreateTransaction(date, acct, "Best Match", "", -500, false, "", "")
	d.FlushChanges()

	match := d.matchTransaction(date, acct, "Best Match", -500, "", nil)
	if match == nil {
		t.Fatal("expected a match")
	}
	if match.ID != t2.ID {
		t.Errorf("expected payee-preferred match %q, got %q (want %q)", match.ID, match.ID, t2.ID)
	}
	_ = t1
}

func TestMatchTransaction_wrongAmount(t *testing.T) {
	d := newTestDB(t)
	insertAccount(t, d, "a1", "Main")
	acct := &Account{ID: "a1"}

	date := time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)
	_, _ = d.CreateTransaction(date, acct, "Shop", "", -500, false, "", "")
	d.FlushChanges()

	match := d.matchTransaction(date, acct, "Shop", -999, "", nil)
	if match != nil {
		t.Error("expected no match for different amount")
	}
}

func TestSortByDateProximity(t *testing.T) {
	mkTxn := func(date time.Time) *Transaction {
		return &Transaction{Date: date}
	}
	target := dateToInt(time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC))
	txns := []*Transaction{
		mkTxn(time.Date(2024, 6, 5, 0, 0, 0, 0, time.UTC)),
		mkTxn(time.Date(2024, 6, 11, 0, 0, 0, 0, time.UTC)),
		mkTxn(time.Date(2024, 6, 8, 0, 0, 0, 0, time.UTC)),
	}
	sortByDateProximity(txns, target)

	if !txns[0].Date.Equal(time.Date(2024, 6, 11, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("expected closest date first, got %v", txns[0].Date)
	}
}

func TestApplyMessages_upsertsRows(t *testing.T) {
	d := newTestDB(t)
	msgs := []*ProtoMessage{
		{Dataset: "payees", Row: "p-new", Column: "name", Value: "S:Inserted Payee"},
	}
	if err := d.ApplyMessages(msgs); err != nil {
		t.Fatalf("ApplyMessages error: %v", err)
	}
	var name string
	err := d.sql.QueryRow(`SELECT COALESCE(name,'') FROM payees WHERE id = 'p-new'`).Scan(&name)
	if err != nil {
		t.Fatalf("row not found after ApplyMessages: %v", err)
	}
	if name != "Inserted Payee" {
		t.Errorf("name: got %q, want %q", name, "Inserted Payee")
	}
}

func TestApplyMessages_updatesCache(t *testing.T) {
	d := newTestDB(t)
	insertPayee(t, d, "p-1", "Old Name")
	if err := d.warmCaches(); err != nil {
		t.Fatalf("warmCaches: %v", err)
	}

	msgs := []*ProtoMessage{
		{Dataset: "payees", Row: "p-1", Column: "name", Value: "S:New Name"},
	}
	if err := d.ApplyMessages(msgs); err != nil {
		t.Fatalf("ApplyMessages error: %v", err)
	}

	if id := d.payeeByName["New Name"]; id != "p-1" {
		t.Errorf("cache not updated: payeeByName[New Name] = %q, want p-1", id)
	}
	if name := d.payeeByID["p-1"]; name != "New Name" {
		t.Errorf("cache not updated: payeeByID[p-1] = %q, want New Name", name)
	}
}

func TestApplyMessages_skipsPrefs(t *testing.T) {
	d := newTestDB(t)
	msgs := []*ProtoMessage{
		{Dataset: "prefs", Row: "some-pref", Column: "value", Value: "S:123"},
	}

	if err := d.ApplyMessages(msgs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyMessages_empty(t *testing.T) {
	d := newTestDB(t)
	if err := d.ApplyMessages(nil); err != nil {
		t.Fatalf("unexpected error on nil input: %v", err)
	}
	if err := d.ApplyMessages([]*ProtoMessage{}); err != nil {
		t.Fatalf("unexpected error on empty slice: %v", err)
	}
}

func TestSaveAndLoadHULC(t *testing.T) {
	d := newTestDB(t)
	h := &HULCClient{
		ClientID:     "AABBCCDDEEFF0011",
		InitialCount: 42,
		TS:           time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC),
	}
	if err := d.SaveHULC(h); err != nil {
		t.Fatalf("SaveHULC error: %v", err)
	}
	loaded, err := d.LoadHULC()
	if err != nil {
		t.Fatalf("LoadHULC error: %v", err)
	}
	if loaded.ClientID != h.ClientID {
		t.Errorf("ClientID: got %q, want %q", loaded.ClientID, h.ClientID)
	}
	if loaded.InitialCount != h.InitialCount {
		t.Errorf("InitialCount: got %d, want %d", loaded.InitialCount, h.InitialCount)
	}
}

func TestLoadHULC_noRow_returnsFresh(t *testing.T) {
	d := newTestDB(t)
	h, err := d.LoadHULC()
	if err != nil {
		t.Fatalf("LoadHULC error: %v", err)
	}
	if h == nil {
		t.Fatal("expected non-nil HULCClient")
	}
	if h.InitialCount != 0 {
		t.Errorf("expected fresh count 0, got %d", h.InitialCount)
	}
}

func TestSaveHULC_idempotent(t *testing.T) {
	d := newTestDB(t)
	h1 := &HULCClient{ClientID: "AAAAAAAAAAAAAAAA", InitialCount: 1, TS: time.Now()}
	h2 := &HULCClient{ClientID: "BBBBBBBBBBBBBBBB", InitialCount: 2, TS: time.Now()}

	if err := d.SaveHULC(h1); err != nil {
		t.Fatalf("first SaveHULC: %v", err)
	}
	if err := d.SaveHULC(h2); err != nil {
		t.Fatalf("second SaveHULC: %v", err)
	}

	loaded, _ := d.LoadHULC()
	if loaded.ClientID != "BBBBBBBBBBBBBBBB" {
		t.Errorf("expected second save to win, got ClientID %q", loaded.ClientID)
	}

	var count int
	_ = d.sql.QueryRow(`SELECT COUNT(*) FROM messages_clock`).Scan(&count)
	if count != 1 {
		t.Errorf("expected exactly 1 row in messages_clock, got %d", count)
	}
}

func TestReconcileTransaction_updatesImportedID(t *testing.T) {
	d := newTestDB(t)
	insertAccount(t, d, "a1", "Main")
	acct := &Account{ID: "a1"}
	date := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)

	existing, _ := d.CreateTransaction(date, acct, "Shop", "", -500, true, "", "")
	d.FlushChanges()

	txn, changed, err := d.ReconcileTransaction(date, acct, "Shop", "", -500, true, "FIN-REF-001", "", nil)
	if err != nil {
		t.Fatalf("ReconcileTransaction error: %v", err)
	}
	if !changed {
		t.Error("expected changed = true when importedID differs from existing")
	}
	if txn.ID != existing.ID {
		t.Errorf("expected matched ID %q, got %q", existing.ID, txn.ID)
	}
	if txn.FinancialID != "FIN-REF-001" {
		t.Errorf("FinancialID: got %q, want FIN-REF-001", txn.FinancialID)
	}
}

func TestReconcileTransaction_updatesPayee(t *testing.T) {
	d := newTestDB(t)
	insertAccount(t, d, "a1", "Main")
	acct := &Account{ID: "a1"}
	date := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)

	existing, _ := d.CreateTransaction(date, acct, "Old Shop", "", -500, true, "", "")
	d.FlushChanges()

	txn, changed, err := d.ReconcileTransaction(date, acct, "New Shop", "", -500, true, "", "", nil)
	if err != nil {
		t.Fatalf("ReconcileTransaction error: %v", err)
	}
	if !changed {
		t.Error("expected changed = true when payee name differs")
	}
	if txn.ID != existing.ID {
		t.Errorf("expected matched ID %q, got %q", existing.ID, txn.ID)
	}
	if txn.PayeeName != "New Shop" {
		t.Errorf("PayeeName: got %q, want New Shop", txn.PayeeName)
	}
}

func TestFlushChanges_clearsAfterFlush(t *testing.T) {
	d := newTestDB(t)
	d.track("transactions", "id1", "amount", 100)
	d.track("transactions", "id1", "cleared", 1)

	first := d.FlushChanges()
	if len(first) != 2 {
		t.Errorf("expected 2 changes, got %d", len(first))
	}
	second := d.FlushChanges()
	if len(second) != 0 {
		t.Errorf("expected 0 changes after flush, got %d", len(second))
	}
}
