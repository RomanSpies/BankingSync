package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	mrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"bankingsync/actual"
	"bankingsync/budget"
	"bankingsync/enablebanking"
	"bankingsync/firefly"
	"bankingsync/firefly/fireflytest"
	"bankingsync/internal/linkagegen"
	"bankingsync/store"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	_ "modernc.org/sqlite"
)

type fakeStore struct {
	mu        sync.Mutex
	accounts  map[string]*budget.Account
	txns      []*budget.Transaction
	nextID    int
	pingErr   error
	commitErr error

	commitCount int
	clearedIDs  []string

	writeCount        int
	failWriteOnAcct   string
	failAmountUpdates bool
	afterWrite        func(n int)

	openingCalls int
	openingRefs  []string
	openingCents int64
	openingDate  string
	openingErr   error
}

func (f *fakeStore) maybeFailWrite(accountID string) error {
	f.writeCount++
	if f.afterWrite != nil {
		defer f.afterWrite(f.writeCount)
	}
	if f.failWriteOnAcct != "" && f.failWriteOnAcct == accountID {
		return fmt.Errorf("injected write failure for account %s", accountID)
	}
	return nil
}

func newFakeStore() *fakeStore {
	return &fakeStore{accounts: make(map[string]*budget.Account)}
}

// SetOpeningBalance records the call and behaves like a real backend: the same
// Ref twice writes once.
func (f *fakeStore) SetOpeningBalance(_ context.Context, accountID string, ob budget.OpeningBalance) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.openingErr != nil {
		return false, f.openingErr
	}
	f.openingCalls++
	for _, seen := range f.openingRefs {
		if seen == ob.Ref {
			return false, nil
		}
	}
	f.openingRefs = append(f.openingRefs, ob.Ref)
	f.openingCents = ob.AmountCents
	f.openingDate = ob.Date.Format("2006-01-02")
	f.txns = append(f.txns, &budget.Transaction{
		ID: fmt.Sprintf("opening-%d", len(f.openingRefs)), AccountID: accountID,
		Date: ob.Date, AmountCents: ob.AmountCents, PayeeName: ob.PayeeName,
		ExternalRef: ob.Ref, Cleared: true,
	})
	return true, nil
}

func (f *fakeStore) AccountBalance(_ context.Context, accountID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var total int64
	for _, t := range f.txns {
		if t.AccountID == accountID {
			total += t.AmountCents
		}
	}
	return total, nil
}

func (f *fakeStore) Ping(context.Context) error { return f.pingErr }

func (f *fakeStore) Close() {
	// Nothing to release: the fake holds nothing but maps, and the tests assert on
	// its contents after the syncer has finished with it.
}

func (f *fakeStore) Commit(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commitCount++
	return f.commitErr
}

func (f *fakeStore) GetOrCreateAccount(_ context.Context, spec budget.AccountSpec) (*budget.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := spec.Name
	if a, ok := f.accounts[name]; ok {
		return a, nil
	}
	f.nextID++
	a := &budget.Account{ID: fmt.Sprintf("acct-%d", f.nextID), Name: name}
	f.accounts[name] = a
	return a, nil
}

func (f *fakeStore) ListTransactions(_ context.Context, accountID string, from, to time.Time) ([]*budget.Transaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*budget.Transaction
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

func (f *fakeStore) FindByExternalRef(_ context.Context, accountID, ref string) (*budget.Transaction, error) {
	if ref == "" {
		return nil, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.txns {
		if t.AccountID == accountID && t.ExternalRef == ref {
			return t, nil
		}
	}
	return nil, nil
}

func (f *fakeStore) Create(_ context.Context, accountID string, in budget.ImportedFields) (*budget.Transaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.maybeFailWrite(accountID); err != nil {
		return nil, err
	}
	f.nextID++
	t := &budget.Transaction{
		ID:            fmt.Sprintf("txn-%d", f.nextID),
		AccountID:     accountID,
		Date:          in.Date,
		AmountCents:   in.AmountCents,
		Currency:      in.Currency,
		PayeeName:     budget.TitleCase(in.PayeeName),
		Notes:         in.Notes,
		ExternalRef:   in.ExternalRef,
		ImportedPayee: in.ImportedPayee,
		Cleared:       in.Cleared,
	}
	f.txns = append(f.txns, t)
	return t, nil
}

func (f *fakeStore) Update(_ context.Context, t *budget.Transaction, p budget.Patch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAmountUpdates && p.AmountCents != nil {
		f.writeCount++
		return fmt.Errorf("injected amount-update failure")
	}
	if err := f.maybeFailWrite(t.AccountID); err != nil {
		return err
	}
	if p.Cleared != nil && *p.Cleared {
		f.clearedIDs = append(f.clearedIDs, t.ID)
	}
	budget.Apply(t, p)
	return nil
}

func (f *fakeStore) dropTransaction(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.txns[:0]
	for _, t := range f.txns {
		if t.ID != id {
			out = append(out, t)
		}
	}
	f.txns = out
}

func (f *fakeStore) transactionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.txns)
}

type ebMock struct {
	mu       sync.Mutex
	pages    [][]map[string]any
	dateFrom []string

	balances      []map[string]any
	balanceStatus int
	balanceCalls  int
	// onBalanceCall lets a test move the balance between the two readings a run
	// makes, which is the only way to exercise the stability gate.
	onBalanceCall func(call int) []map[string]any
}

func (m *ebMock) setBalances(balances []map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.balances = balances
}

func (m *ebMock) setBalanceStatus(status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.balanceStatus = status
}

func (m *ebMock) setBalanceHook(fn func(call int) []map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onBalanceCall = fn
}

func bookedBalance(amount string) []map[string]any {
	return []map[string]any{{
		"name":           "Booked",
		"balance_type":   "CLBD",
		"balance_amount": map[string]any{"amount": amount, "currency": "EUR"},
	}}
}

func (m *ebMock) setPages(pages [][]map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pages = pages
}

func (m *ebMock) recordedDateFrom() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.dateFrom))
	copy(out, m.dateFrom)
	return out
}

func (m *ebMock) lastDateFrom(t *testing.T) string {
	t.Helper()
	got := m.recordedDateFrom()
	if len(got) == 0 {
		t.Fatal("no fetch requests recorded")
	}
	return got[len(got)-1]
}

type harness struct {
	seed   harnessSeeder
	st     *store.Store
	fake   *fakeStore
	eb     *ebMock
	syncer *Syncer

	// actualPath is the Actual database file behind this harness, empty unless
	// the backend is the real one. Tests that need to reach past the adapter —
	// to install a budget rule, say — open it themselves.
	actualPath string

	// ns prefixes every budget account name this harness uses. It is empty for
	// the in-process backends, which each get a fresh store per test and would
	// only be made harder to read by a prefix. A harness that shares one live
	// backend with every other test sets it, because there the account from the
	// previous test is still sitting in the database.
	ns string
}

// acct qualifies a budget account name with the harness namespace.
func (h *harness) acct(name string) string { return h.ns + name }

func testKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	mock := &ebMock{}
	mux := http.NewServeMux()
	mux.HandleFunc("/accounts/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/balances") {
			mock.mu.Lock()
			mock.balanceCalls++
			call := mock.balanceCalls
			status := mock.balanceStatus
			payload := mock.balances
			if mock.onBalanceCall != nil {
				payload = mock.onBalanceCall(call)
			}
			mock.mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			if status != 0 && status != http.StatusOK {
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "refused"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"balances": payload})
			return
		}

		mock.mu.Lock()
		if r.URL.Query().Get("continuation_key") == "" {
			mock.dateFrom = append(mock.dateFrom, r.URL.Query().Get("date_from"))
		}
		var page []map[string]any
		if len(mock.pages) > 0 {
			page = mock.pages[0]
		}
		mock.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"transactions":     page,
			"continuation_key": "",
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	keyPEM := testKeyPEM(t)
	eb := enablebanking.NewClient(
		func() (string, error) { return "test-app", nil },
		func() ([]byte, error) { return keyPEM, nil },
		nil,
		enablebanking.WithBaseURL(ts.URL),
	)

	state, err := LoadFromStore(st)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	fake := newFakeStore()
	return &harness{
		st:     st,
		fake:   fake,
		eb:     mock,
		syncer: &Syncer{state: state, st: st, ac: fake, eb: eb},
	}
}

func (h *harness) addAccount(t *testing.T, startSyncDate string) int64 {
	t.Helper()
	return h.addNamedAccount(t, "TestBank", "acct-uid", "Checking", startSyncDate)
}

func (h *harness) addNamedAccount(t *testing.T, bank, uid, actualAccount, startSyncDate string) int64 {
	t.Helper()
	id, err := h.st.AddBankAccount(store.NewBankAccount{
		SessionID:     "sess",
		AccountUID:    uid,
		BankName:      bank,
		BankCountry:   "DE",
		ActualAccount: h.acct(actualAccount),
		StartSyncDate: startSyncDate,
		SessionExpiry: time.Now().UTC().AddDate(0, 0, 90).Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("add bank account: %v", err)
	}
	return id
}

func (h *harness) reloadState(t *testing.T) {
	t.Helper()
	state, err := LoadFromStore(h.st)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	h.syncer.state = state
}

func (h *harness) lastSyncLog(t *testing.T) *store.SyncLog {
	t.Helper()
	l, err := h.st.GetLatestSyncLog()
	if err != nil {
		t.Fatalf("get sync log: %v", err)
	}
	if l == nil {
		t.Fatal("no sync log recorded")
	}
	return l
}

func bookedTxn(ref, date, amount string) map[string]any {
	return bookedTxnPayee(ref, date, amount, "Acme GmbH")
}

func bookedTxnPayee(ref, date, amount, payee string) map[string]any {
	return map[string]any{
		"entry_reference":        ref,
		"transaction_date":       date,
		"transaction_amount":     map[string]any{"amount": amount},
		"credit_debit_indicator": "DBIT",
		"status":                 "BOOK",
		"creditor":               map[string]any{"name": payee},
	}
}

func pendingTxn(ref, date, amount string) map[string]any {
	tx := bookedTxn(ref, date, amount)
	tx["status"] = "PDNG"
	return tx
}

func today() string { return time.Now().UTC().Format("2006-01-02") }

func daysAgo(n int) string { return time.Now().UTC().AddDate(0, 0, -n).Format("2006-01-02") }

func TestSync_noBankAccounts_setsNoSession(t *testing.T) {
	h := newHarness(t)
	h.syncer.run()

	if got := h.lastSyncLog(t).Status; got != "no_session" {
		t.Errorf("status: got %q, want no_session", got)
	}
	if len(h.eb.recordedDateFrom()) != 0 {
		t.Error("expected no fetch when no accounts are configured")
	}
}

func TestSync_usesStartSyncDateWhenSet(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "2026-01-15")
	_ = h.st.SetLastSyncDate("2026-06-01")
	h.reloadState(t)

	h.syncer.run()

	if got := h.eb.lastDateFrom(t); got != "2026-01-15" {
		t.Errorf("date_from: got %q, want 2026-01-15 (start_sync_date wins)", got)
	}
}

func TestSync_usesLastSyncDateWhenNoStartDate(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate("2026-06-01")
	h.reloadState(t)

	h.syncer.run()

	if got := h.eb.lastDateFrom(t); got != "2026-06-01" {
		t.Errorf("date_from: got %q, want 2026-06-01", got)
	}
}

func TestSync_fallsBackToThirtyDays(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	h.reloadState(t)

	h.syncer.run()

	if got, want := h.eb.lastDateFrom(t), daysAgo(30); got != want {
		t.Errorf("date_from: got %q, want %q", got, want)
	}
}

func TestSync_advancesLastSyncDateAfterSuccess(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate("2026-06-01")
	h.reloadState(t)

	h.syncer.run()

	got, _ := h.st.GetLastSyncDate()
	if got != today() {
		t.Errorf("last_sync_date: got %q, want %q", got, today())
	}
}

func TestSync_multiCycle_windowAdvancesAndDoesNotCreep(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(3))
	h.reloadState(t)

	h.syncer.run()
	h.syncer.run()
	h.syncer.run()

	recorded := h.eb.recordedDateFrom()
	if len(recorded) != 3 {
		t.Fatalf("got %d fetches, want 3", len(recorded))
	}
	if recorded[0] != daysAgo(3) {
		t.Errorf("cycle 1 date_from: got %q, want %q", recorded[0], daysAgo(3))
	}
	for i, got := range recorded[1:] {
		if got != today() {
			t.Errorf("cycle %d date_from: got %q, want %q (window should not creep)", i+2, got, today())
		}
	}
}

func TestSync_stalePendingPinsWindowAcrossCycles(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(1))
	stale := daysAgo(19)
	if err := h.st.SetPending(1, "zombie-key", "txn-zombie|"+stale); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	h.reloadState(t)

	h.syncer.run()
	h.syncer.run()

	for i, got := range h.eb.recordedDateFrom() {
		if got != stale {
			t.Errorf("cycle %d date_from: got %q, want %q", i+1, got, stale)
		}
	}
	if _, ok := h.syncer.state.Pending(1)["zombie-key"]; !ok {
		t.Error("stale pending entry was unexpectedly removed")
	}
}

func TestSync_pendingCreatedForPendingTransaction(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	h.reloadState(t)
	h.eb.setPages([][]map[string]any{{pendingTxn("ref-p1", daysAgo(1), "12.50")}})

	h.syncer.run()

	if _, ok := h.syncer.state.Pending(1)["ref-p1"]; !ok {
		t.Fatal("expected a pending map entry for the PDNG transaction")
	}
	if h.fake.transactionCount() != 1 {
		t.Errorf("got %d Actual transactions, want 1", h.fake.transactionCount())
	}
	if got := h.lastSyncLog(t).TxAdded; got != 1 {
		t.Errorf("tx_added: got %d, want 1", got)
	}
}

func TestSync_pendingConfirmedWhenTransactionBooks(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{pendingTxn("ref-p1", daysAgo(2), "12.50")}})
	h.syncer.run()
	if _, ok := h.syncer.state.Pending(1)["ref-p1"]; !ok {
		t.Fatal("setup: expected pending entry after first cycle")
	}

	h.eb.setPages([][]map[string]any{{bookedTxn("ref-p1", daysAgo(2), "12.50")}})
	h.syncer.run()

	if _, ok := h.syncer.state.Pending(1)["ref-p1"]; ok {
		t.Error("pending entry should be removed once the transaction books")
	}
	if _, ok := h.syncer.state.Imported(1)["ref-p1"]; !ok {
		t.Error("expected an imported ref after the transaction booked")
	}
	if !h.fake.txns[0].Cleared {
		t.Error("expected the transaction to be cleared after booking")
	}
	if h.fake.transactionCount() != 1 {
		t.Errorf("got %d Actual transactions, want 1 (should update, not duplicate)", h.fake.transactionCount())
	}
}

func TestSync_dedupesAlreadyImportedRefs(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	h.reloadState(t)
	h.eb.setPages([][]map[string]any{{bookedTxn("ref-b1", daysAgo(1), "30.00")}})

	h.syncer.run()
	first := h.fake.transactionCount()
	h.syncer.run()

	if got := h.fake.transactionCount(); got != first {
		t.Errorf("second cycle created duplicates: got %d, want %d", got, first)
	}
	if got := h.lastSyncLog(t).TxSkipped; got != 1 {
		t.Errorf("tx_skipped on second cycle: got %d, want 1", got)
	}
}

func TestSync_droppedTransactionsMarkSyncDegraded(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	h.reloadState(t)
	h.eb.setPages([][]map[string]any{{
		bookedTxn("ref-ok", daysAgo(1), "10.00"),
		{"entry_reference": "ref-broken"},
	}})

	h.syncer.run()

	l := h.lastSyncLog(t)
	if l.Status != "degraded" {
		t.Errorf("status: got %q, want degraded", l.Status)
	}
	if l.Message == "" {
		t.Error("expected the dropped-transaction count in the sync log message")
	}
}

func TestSync_cleanRunIsNotDegraded(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	h.reloadState(t)
	h.eb.setPages([][]map[string]any{{bookedTxn("ref-ok", daysAgo(1), "10.00")}})

	h.syncer.run()

	if got := h.lastSyncLog(t).Status; got != "success" {
		t.Errorf("status: got %q, want success", got)
	}
}

func TestSync_actualFailureDoesNotAdvanceWatermark(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate("2026-06-01")
	h.reloadState(t)
	h.fake.pingErr = fmt.Errorf("connection refused")
	h.syncer.connectBackend = func(context.Context) (budget.Store, error) {
		return nil, fmt.Errorf(`login: Post "http://actual:5006/account/login": dial tcp: connection refused`)
	}

	h.syncer.run()

	if got, _ := h.st.GetLastSyncDate(); got != "2026-06-01" {
		t.Errorf("last_sync_date: got %q, want it unchanged at 2026-06-01", got)
	}
	if got := h.lastSyncLog(t).Status; got != "error" {
		t.Errorf("status: got %q, want error", got)
	}
	if len(h.eb.recordedDateFrom()) != 0 {
		t.Error("expected no EB fetch when Actual is unreachable")
	}
}

func TestSync_commitsAfterSuccessfulImport(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	h.reloadState(t)
	h.eb.setPages([][]map[string]any{{bookedTxn("ref-b1", daysAgo(1), "10.00")}})

	h.syncer.run()

	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if h.fake.commitCount != 1 {
		t.Errorf("commit count: got %d, want 1", h.fake.commitCount)
	}
}

func TestSync_earliestPendingWinsOverRecentLastSync(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(today())
	older := daysAgo(10)
	newer := daysAgo(2)
	_ = h.st.SetPending(1, "k-old", "txn-a|"+older)
	_ = h.st.SetPending(1, "k-new", "txn-b|"+newer)
	h.reloadState(t)

	h.syncer.run()

	if got := h.eb.lastDateFrom(t); got != older {
		t.Errorf("date_from: got %q, want %q (earliest pending)", got, older)
	}
}

const actualTestSchema = `
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

func newRealHarness(t *testing.T) (*harness, *actual.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "actual.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open actual db: %v", err)
	}
	if _, err := raw.Exec(actualTestSchema); err != nil {
		t.Fatalf("create actual schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close schema conn: %v", err)
	}

	db, err := actual.OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(db.Close)

	h := newHarness(t)
	h.fake = nil
	h.syncer.ac = actual.NewAdapterForDB(db)
	h.seed = actualSeeder{db: db}
	h.actualPath = path
	return h, db
}

// addRule installs a rule in the Actual database behind this harness.
//
// Through a second connection to the file rather than through the adapter: the
// adapter reads rules and never writes them, which is correct — they are the
// user's, not this program's.
func (h *harness) addRule(t *testing.T, id, condition, action string) {
	t.Helper()
	raw, err := sql.Open("sqlite", h.actualPath)
	if err != nil {
		t.Fatalf("open actual db: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(
		`INSERT INTO rules (id, stage, conditions_op, conditions, actions, tombstone)
		 VALUES (?, '', 'and', ?, ?, 0)`, id, condition, action); err != nil {
		t.Fatalf("insert rule: %v", err)
	}
}

func (h *harness) actualTxns(t *testing.T) []*budget.Transaction {
	t.Helper()
	ctx := context.Background()
	acct, err := h.syncer.ac.GetOrCreateAccount(ctx, budget.AccountSpec{Name: h.acct("Checking")})
	if err != nil {
		t.Fatalf("GetOrCreateAccount: %v", err)
	}
	// Not time.Time{}: that is year 1, and Firefly rejects a start before
	// 1970-01-02. A decade back covers every date the tests use.
	txns, err := h.syncer.ac.ListTransactions(ctx, acct.ID,
		time.Now().UTC().AddDate(-10, 0, 0), time.Now().UTC().AddDate(1, 0, 0))
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	return txns
}

func TestSync_confirmsWhenRefChangesBetweenPendingAndBooked(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(3))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{pendingTxn("auth-1", daysAgo(2), "12.50")}})
		h.syncer.run()
		if _, ok := h.syncer.state.Pending(1)["auth-1"]; !ok {
			t.Fatal("setup: expected pending entry after first cycle")
		}

		h.eb.setPages([][]map[string]any{{bookedTxn("book-9", daysAgo(1), "12.50")}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 1 {
			t.Fatalf("got %d transactions in Actual, want 1 (booked must confirm the pending twin, not duplicate it)", len(txns))
		}
		if !txns[0].Cleared {
			t.Error("expected the transaction to be cleared after booking")
		}
		if txns[0].ExternalRef != "book-9" {
			t.Errorf("FinancialID: got %q, want book-9", txns[0].ExternalRef)
		}
		if len(h.syncer.state.Pending(1)) != 0 {
			t.Errorf("pending map not cleaned up: %v", h.syncer.state.Pending(1))
		}
		if _, ok := h.syncer.state.Imported(1)["book-9"]; !ok {
			t.Error("expected imported ref for the booked transaction")
		}
		l := h.lastSyncLog(t)
		if l.TxConfirmed != 1 {
			t.Errorf("tx_confirmed: got %d, want 1", l.TxConfirmed)
		}
		if l.TxAdded != 0 {
			t.Errorf("tx_added: got %d, want 0", l.TxAdded)
		}
	})
}

func TestSync_confirmsWhenRefAssignedOnlyAtBooking(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(3))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{pendingTxn("", daysAgo(2), "45.00")}})
		h.syncer.run()
		if len(h.syncer.state.Pending(1)) != 1 {
			t.Fatalf("setup: got %d pending entries, want 1", len(h.syncer.state.Pending(1)))
		}

		h.eb.setPages([][]map[string]any{{bookedTxn("book-42", daysAgo(2), "45.00")}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 1 {
			t.Fatalf("got %d transactions in Actual, want 1", len(txns))
		}
		if !txns[0].Cleared {
			t.Error("expected cleared after booking")
		}
		if len(h.syncer.state.Pending(1)) != 0 {
			t.Errorf("pending map not cleaned up: %v", h.syncer.state.Pending(1))
		}
		if got := h.lastSyncLog(t).TxConfirmed; got != 1 {
			t.Errorf("tx_confirmed: got %d, want 1", got)
		}
	})
}

func TestSync_identicalTwinsInOneBatchNotMerged(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(2))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{
			bookedTxn("twin-1", daysAgo(1), "9.99"),
			bookedTxn("twin-2", daysAgo(1), "9.99"),
		}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 2 {
			t.Fatalf("got %d transactions, want 2 (distinct same-amount transactions must not merge)", len(txns))
		}
		if got := h.lastSyncLog(t).TxAdded; got != 2 {
			t.Errorf("tx_added: got %d, want 2", got)
		}
	})
}

func TestSync_manualEntryMatchedNotDuplicated(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(3))
		h.reloadState(t)

		manualDate := time.Now().UTC().AddDate(0, 0, -1)
		h.seed.foreignTransaction(t, h.acct("Checking"), manualDate, "Acme GmbH", "my note", -3000)

		h.eb.setPages([][]map[string]any{{bookedTxn("bank-1", daysAgo(1), "30.00")}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 1 {
			t.Fatalf("got %d transactions, want 1 (bank import must absorb the manual entry)", len(txns))
		}
		if !txns[0].Cleared {
			t.Error("expected the manual entry to be cleared by the bank import")
		}
		if txns[0].ExternalRef != "bank-1" {
			t.Errorf("FinancialID: got %q, want bank-1", txns[0].ExternalRef)
		}
		l := h.lastSyncLog(t)
		if l.TxAdded != 0 {
			t.Errorf("tx_added: got %d, want 0", l.TxAdded)
		}
		if l.TxConfirmed != 0 {
			t.Errorf("tx_confirmed: got %d, want 0 (a manual entry is not a tracked pending)", l.TxConfirmed)
		}
		if l.TxSkipped != 1 {
			t.Errorf("tx_skipped: got %d, want 1", l.TxSkipped)
		}
	})
}

func TestSync_reimportAfterRefsPrunedDoesNotDuplicate(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(3))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{bookedTxn("r1", daysAgo(2), "20.00")}})
		h.syncer.run()
		if len(h.actualTxns(t)) != 1 {
			t.Fatal("setup: expected 1 transaction after first import")
		}

		if err := h.syncer.state.AddImportedRef(1, "r1", daysAgo(store.RetentionDays+5), h.st); err != nil {
			t.Fatalf("age imported ref: %v", err)
		}

		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 1 {
			t.Fatalf("got %d transactions, want 1 (re-import after ref prune must dedupe)", len(txns))
		}
		if _, ok := h.syncer.state.Imported(1)["r1"]; !ok {
			t.Error("expected the imported ref to be re-armed after the re-import")
		}
		if got := h.lastSyncLog(t).TxAdded; got != 0 {
			t.Errorf("tx_added: got %d, want 0", got)
		}
	})
}

func TestSync_pendingPrunedAfterCutoff(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(1))
	_ = h.st.SetPending(1, "ancient", "txn-a|"+daysAgo(store.RetentionDays+5))
	_ = h.st.SetPending(1, "recent", "txn-b|"+daysAgo(5))
	h.reloadState(t)

	h.syncer.run()

	if _, ok := h.syncer.state.Pending(1)["ancient"]; ok {
		t.Error("pending entry older than 21 days must be pruned")
	}
	if _, ok := h.syncer.state.Pending(1)["recent"]; !ok {
		t.Error("recent pending entry must survive the prune")
	}
	if got, want := h.eb.lastDateFrom(t), daysAgo(5); got != want {
		t.Errorf("date_from: got %q, want %q (window must follow the surviving pending, not the pruned one)", got, want)
	}
	dbVal, ok, err := h.st.GetPendingTxnID(1, "ancient")
	if err != nil {
		t.Fatalf("GetPendingTxnID: %v", err)
	}
	if ok {
		t.Errorf("store still holds pruned entry: %q", dbVal)
	}
}

func TestSync_legacyPendingWithoutDatePruned(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(1))
	_ = h.st.SetPending(1, "legacy-key", "bare-txn-id-without-date")
	h.reloadState(t)

	h.syncer.run()

	if _, ok := h.syncer.state.Pending(1)["legacy-key"]; ok {
		t.Error("legacy pending entry without a parseable date must be pruned")
	}
	if got, want := h.eb.lastDateFrom(t), daysAgo(1); got != want {
		t.Errorf("date_from: got %q, want %q", got, want)
	}
}

func TestSync_droppedTransactionsMetricRecorded(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	h.reloadState(t)

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	h.syncer.met = newSyncMetrics(provider.Meter("bankingsync"))

	h.eb.setPages([][]map[string]any{{
		bookedTxn("ref-ok", daysAgo(1), "10.00"),
		{"entry_reference": "ref-broken"},
		{"entry_reference": "ref-broken-2"},
	}})
	h.syncer.run()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	var droppedTotal int64
	var bankAttrSeen bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "bankingsync_transactions_dropped_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("unexpected data type %T", m.Data)
			}
			for _, dp := range sum.DataPoints {
				droppedTotal += dp.Value
				if v, ok := dp.Attributes.Value("bank"); ok && v.AsString() == "TestBank" {
					bankAttrSeen = true
				}
			}
		}
	}

	if droppedTotal != 2 {
		t.Errorf("dropped counter: got %d, want 2", droppedTotal)
	}
	if !bankAttrSeen {
		t.Error("expected the bank attribute on the dropped counter")
	}
}

func TestSync_concurrentRunsAreSerialised(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	h.reloadState(t)
	h.eb.setPages([][]map[string]any{{
		bookedTxn("c-1", daysAgo(1), "10.00"),
		bookedTxn("c-2", daysAgo(1), "11.00"),
		bookedTxn("c-3", daysAgo(1), "12.00"),
	}})

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.syncer.run()
		}()
	}
	wg.Wait()

	if _, err := h.st.GetLatestSyncLog(); err != nil {
		t.Fatalf("store unusable after concurrent runs: %v", err)
	}
	if got := len(h.syncer.state.Imported(1)); got != 3 {
		t.Errorf("imported refs: got %d, want 3", got)
	}
}

func TestSync_reentrantTriggerIsSkipped(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	h.reloadState(t)

	if !h.syncer.tryAcquire() {
		t.Fatal("setup: expected to acquire the sync lock")
	}
	h.syncer.run()
	h.syncer.release()

	if l, _ := h.st.GetLatestSyncLog(); l != nil {
		t.Errorf("a reentrant run must not record a sync log, got status %q", l.Status)
	}
	if len(h.eb.recordedDateFrom()) != 0 {
		t.Error("a reentrant run must not fetch")
	}
}

func TestSync_publishesStateGauges(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetPending(1, "g-1", "txn-a|"+daysAgo(2))
	h.reloadState(t)

	h.syncer.run()

	if got := h.syncer.pendingGauge.Load(); got != 1 {
		t.Errorf("pending gauge: got %d, want 1", got)
	}
	if !h.syncer.expiryDaysKnown.Load() {
		t.Error("expected the session expiry gauge to be known after a run")
	}
}

func TestSync_commitFailureIsReportedAndDoesNotAdvanceWatermark(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate("2026-06-01")
	h.reloadState(t)
	h.fake.commitErr = fmt.Errorf("sync_sync: HTTP 500")
	h.eb.setPages([][]map[string]any{{bookedTxn("cf-1", daysAgo(1), "10.00")}})

	h.syncer.run()

	l := h.lastSyncLog(t)
	if l.Status != "error" {
		t.Errorf("status: got %q, want error (a failed commit must never report success)", l.Status)
	}
	if l.Message == "" {
		t.Error("expected the commit failure in the sync log message")
	}
	if got, _ := h.st.GetLastSyncDate(); got != "2026-06-01" {
		t.Errorf("last_sync_date: got %q, want it unchanged at 2026-06-01", got)
	}
}

func TestSync_commitSuccessAdvancesWatermark(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate("2026-06-01")
	h.reloadState(t)
	h.eb.setPages([][]map[string]any{{bookedTxn("cs-1", daysAgo(1), "10.00")}})

	h.syncer.run()

	if got := h.lastSyncLog(t).Status; got != "success" {
		t.Errorf("status: got %q, want success", got)
	}
	if got, _ := h.st.GetLastSyncDate(); got != today() {
		t.Errorf("last_sync_date: got %q, want %q", got, today())
	}
}

func TestSync_sameAmountAcrossRunsDoesNotCannibalise(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(5))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{bookedTxn("mon-A", daysAgo(3), "3.50")}})
		h.syncer.run()
		if got := len(h.actualTxns(t)); got != 1 {
			t.Fatalf("setup: got %d transactions, want 1", got)
		}

		h.eb.setPages([][]map[string]any{{
			bookedTxn("mon-A", daysAgo(3), "3.50"),
			bookedTxn("tue-B", daysAgo(2), "3.50"),
		}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 2 {
			t.Fatalf("got %d transactions, want 2 (a second same-amount transaction must not absorb the first)", len(txns))
		}
		fids := map[string]bool{}
		for _, tx := range txns {
			fids[tx.ExternalRef] = true
		}
		if !fids["mon-A"] || !fids["tue-B"] {
			t.Errorf("financial_ids: got %v, want both mon-A and tue-B intact", fids)
		}
	})
}

func TestSync_differentPayeeSameAmountDoesNotMerge(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(5))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{bookedTxnPayee("spot-1", daysAgo(4), "9.99", "Spotify")}})
		h.syncer.run()

		h.eb.setPages([][]map[string]any{{bookedTxnPayee("netflix-1", daysAgo(2), "9.99", "Netflix")}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 2 {
			t.Fatalf("got %d transactions, want 2 (Netflix must not be renamed into the Spotify row)", len(txns))
		}
		payees := map[string]bool{}
		for _, tx := range txns {
			payees[tx.PayeeName] = true
		}
		if !payees["Spotify"] || !payees["Netflix"] {
			t.Errorf("payees: got %v, want both Spotify and Netflix", payees)
		}
	})
}

func TestSync_alreadyImportedRowIsNotFuzzyVictim(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(5))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{bookedTxn("keep-1", daysAgo(3), "42.00")}})
		h.syncer.run()

		h.eb.setPages([][]map[string]any{{
			bookedTxn("keep-1", daysAgo(3), "42.00"),
			bookedTxn("new-2", daysAgo(3), "42.00"),
		}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 2 {
			t.Fatalf("got %d transactions, want 2", len(txns))
		}
		for _, tx := range txns {
			if tx.ExternalRef != "keep-1" && tx.ExternalRef != "new-2" {
				t.Errorf("unexpected financial_id %q", tx.ExternalRef)
			}
		}
	})
}

func TestSync_knownPendingRowIsNotStolenBySameAmountBooked(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(5))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{pendingTxn("pend-1", daysAgo(3), "25.00")}})
		h.syncer.run()
		if len(h.syncer.state.Pending(1)) != 1 {
			t.Fatalf("setup: got %d pending entries, want 1", len(h.syncer.state.Pending(1)))
		}

		h.eb.setPages([][]map[string]any{{
			pendingTxn("pend-1", daysAgo(3), "25.00"),
			bookedTxn("other-2", daysAgo(2), "25.00"),
		}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 2 {
			t.Fatalf("got %d transactions, want 2 (an unrelated booked transaction must not steal the tracked pending row)", len(txns))
		}
		if _, ok := h.syncer.state.Pending(1)["pend-1"]; !ok {
			t.Error("the tracked pending entry was consumed by an unrelated transaction")
		}
		for _, tx := range txns {
			if tx.ExternalRef == "other-2" && !tx.Cleared {
				t.Error("the booked transaction should be its own cleared row")
			}
		}
	})
}

func TestSync_startSyncDateClearedAfterSuccessfulBackfill(t *testing.T) {
	h := newHarness(t)
	id := h.addAccount(t, "2026-01-15")
	_ = h.st.SetLastSyncDate(daysAgo(2))
	h.reloadState(t)

	h.syncer.run()

	if got := h.eb.lastDateFrom(t); got != "2026-01-15" {
		t.Errorf("first cycle date_from: got %q, want the backfill date 2026-01-15", got)
	}
	accounts, _ := h.st.GetAllBankAccounts()
	if accounts[0].StartSyncDate != "" {
		t.Errorf("start_sync_date: got %q, want it cleared after a successful backfill", accounts[0].StartSyncDate)
	}
	_ = id

	h.syncer.run()
	if got := h.eb.lastDateFrom(t); got != today() {
		t.Errorf("second cycle date_from: got %q, want %q (window must stop re-fetching all history)", got, today())
	}
}

func TestSync_startSyncDateSurvivesFailedCommit(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "2026-01-15")
	h.reloadState(t)
	h.fake.commitErr = fmt.Errorf("sync_sync: HTTP 500")

	h.syncer.run()

	accounts, _ := h.st.GetAllBankAccounts()
	if accounts[0].StartSyncDate != "2026-01-15" {
		t.Errorf("start_sync_date: got %q, want it kept when the commit failed", accounts[0].StartSyncDate)
	}
}

func TestSync_userEditsSurviveRefetch(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(5))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{bookedTxnPayee("edit-1", daysAgo(3), "30.00", "AMZN MKTP DE")}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 1 {
			t.Fatalf("setup: got %d transactions, want 1", len(txns))
		}
		h.seed.editPayeeAndNotes(t, txns[0].ID, "Amazon", "my own note")

		h.syncer.run()

		after := h.actualTxns(t)
		if len(after) != 1 {
			t.Fatalf("got %d transactions after refetch, want 1", len(after))
		}
		if after[0].PayeeName != "Amazon" {
			t.Errorf("PayeeName: got %q, want Amazon — a refetch must not revert a user rename", after[0].PayeeName)
		}
		if after[0].Notes != "my own note" {
			t.Errorf("Notes: got %q, want the user note preserved", after[0].Notes)
		}
	})
}

func TestSync_refCollisionAcrossAccountsImportsBoth(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		idA := h.addNamedAccount(t, "BankA", "uid-a", "Checking", "")
		idB := h.addNamedAccount(t, "BankB", "uid-b", "Savings", "")
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{bookedTxn("7", daysAgo(1), "10.00")}})
		h.syncer.run()

		if _, ok := h.syncer.state.Imported(idA)["7"]; !ok {
			t.Error("account A did not record ref 7")
		}
		if _, ok := h.syncer.state.Imported(idB)["7"]; !ok {
			t.Error("account B did not record ref 7 — a colliding ref from another bank must not suppress it")
		}
		if got := h.lastSyncLog(t).TxAdded; got != 2 {
			t.Errorf("tx_added: got %d, want 2 (both banks' transactions must import)", got)
		}
	})
}

func TestSync_pendingKeyCollisionAcrossAccountsIsIndependent(t *testing.T) {
	h := newHarness(t)
	idA := h.addNamedAccount(t, "BankA", "uid-a", "Checking", "")
	idB := h.addNamedAccount(t, "BankB", "uid-b", "Savings", "")
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{pendingTxn("shared-key", daysAgo(1), "15.00")}})
	h.syncer.run()

	if _, ok := h.syncer.state.Pending(idA)["shared-key"]; !ok {
		t.Error("account A pending entry missing")
	}
	if _, ok := h.syncer.state.Pending(idB)["shared-key"]; !ok {
		t.Error("account B pending entry missing — a shared key must not be hijacked")
	}

	if err := h.syncer.state.DeletePending(idA, "shared-key", h.st); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := h.syncer.state.Pending(idB)["shared-key"]; !ok {
		t.Error("deleting account A's entry removed account B's")
	}
}

func TestSync_perAccountWatermark(t *testing.T) {
	h := newHarness(t)
	idA := h.addNamedAccount(t, "BankA", "uid-a", "Checking", "")
	h.addNamedAccount(t, "BankB", "uid-b", "Savings", "")
	h.reloadState(t)

	h.syncer.run()

	accounts, err := h.st.GetAllBankAccounts()
	if err != nil {
		t.Fatalf("GetAllBankAccounts: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("%d accounts read back, want the two that were added; a watermark nobody "+
			"looks at is a watermark nobody checks", len(accounts))
	}
	var sawA bool
	for _, a := range accounts {
		if a.ID == idA {
			sawA = true
		}
		if a.LastSyncDate != today() {
			t.Errorf("account %d last_sync_date: got %q, want %q", a.ID, a.LastSyncDate, today())
		}
	}
	if !sawA {
		t.Errorf("account %d, the one this test names, was not among those checked", idA)
	}
}

func TestSync_bookedStageEnrichesPendingPayeeAndNotes(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(5))
		h.reloadState(t)

		pending := pendingTxn("enrich-1", daysAgo(3), "18.00")
		pending["creditor"] = map[string]any{"name": nil}
		pending["remittance_information"] = []any{"Kartenzahlung"}
		h.eb.setPages([][]map[string]any{{pending}})
		h.syncer.run()

		before := h.actualTxns(t)
		if len(before) != 1 {
			t.Fatalf("setup: got %d transactions, want 1", len(before))
		}

		booked := bookedTxnPayee("enrich-1", daysAgo(3), "18.00", "REWE Markt GmbH")
		booked["remittance_information"] = []any{"Einkauf Filiale 42"}
		h.eb.setPages([][]map[string]any{{booked}})
		h.syncer.run()

		after := h.actualTxns(t)
		if len(after) != 1 {
			t.Fatalf("got %d transactions, want 1", len(after))
		}
		if !after[0].Cleared {
			t.Error("expected the transaction to be cleared")
		}
		if after[0].PayeeName != "REWE Markt GmbH" {
			t.Errorf("payee: got %q, want the merchant resolved at booking time", after[0].PayeeName)
		}
		if after[0].Notes != "Kartenzahlung" {
			t.Errorf("notes: got %q, want the pending note kept — without an "+
				"imported_notes column we cannot tell our own placeholder from a note "+
				"the user typed, so notes are never overwritten", after[0].Notes)
		}
		if got := h.lastSyncLog(t).TxConfirmed; got != 1 {
			t.Errorf("tx_confirmed: got %d, want 1", got)
		}
	})
}

func TestSync_pendingBookedAtDifferentAmountIsNotDuplicated(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(5))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{pendingTxn("tip-1", daysAgo(3), "62.00")}})
		h.syncer.run()
		if len(h.actualTxns(t)) != 1 {
			t.Fatalf("setup: expected the pending authorisation to be imported")
		}

		h.eb.setPages([][]map[string]any{{bookedTxn("tip-1", daysAgo(2), "71.30")}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 1 {
			t.Fatalf("got %d transactions, want 1 — a tip added at booking time must not create a duplicate", len(txns))
		}
		if txns[0].AmountCents != -7130 {
			t.Errorf("amount: got %d, want -7130 (the booked value, not the provisional hold)", txns[0].AmountCents)
		}
		if !txns[0].Cleared {
			t.Error("expected the transaction to be cleared")
		}
		if len(h.syncer.state.Pending(1)) != 0 {
			t.Errorf("pending map not cleaned up: %v", h.syncer.state.Pending(1))
		}
		if got := h.lastSyncLog(t).TxConfirmed; got != 1 {
			t.Errorf("tx_confirmed: got %d, want 1", got)
		}
	})
}

func TestSync_amountUnchangedOnConfirmStaysStable(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(5))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{pendingTxn("same-1", daysAgo(3), "20.00")}})
		h.syncer.run()
		h.eb.setPages([][]map[string]any{{bookedTxn("same-1", daysAgo(3), "20.00")}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 1 {
			t.Fatalf("got %d transactions, want 1", len(txns))
		}
		if txns[0].AmountCents != -2000 {
			t.Errorf("amount: got %d, want -2000", txns[0].AmountCents)
		}
	})
}

func TestSync_updateCheckSurvivesRunCompletion(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	h.reloadState(t)

	started := make(chan context.Context, 1)
	done := make(chan struct{})
	h.syncer.checkUpdate = func(ctx context.Context, _ *store.Store) {
		started <- ctx
		<-done
	}

	h.syncer.run()

	var updCtx context.Context
	select {
	case updCtx = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("update check never started")
	}
	defer close(done)

	select {
	case <-updCtx.Done():
		t.Fatalf("update check context was cancelled when run() returned: %v", updCtx.Err())
	case <-time.After(200 * time.Millisecond):
	}

	deadline, ok := updCtx.Deadline()
	if !ok {
		t.Fatal("the update check should still carry its own deadline")
	}
	if time.Until(deadline) > updateCheckTimeout+time.Second {
		t.Errorf("deadline %v is further out than the configured timeout", time.Until(deadline))
	}
}

func TestSync_confirmUpdatesTheTrackedRowNotANearbyTwin(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(9))
		h.reloadState(t)

		pending := pendingTxn("", daysAgo(6), "25.00")
		h.eb.setPages([][]map[string]any{{pending}})
		h.syncer.run()

		tracked := h.actualTxns(t)
		if len(tracked) != 1 {
			t.Fatalf("setup: got %d transactions, want 1", len(tracked))
		}
		trackedID := tracked[0].ID
		if len(h.syncer.state.Pending(1)) != 1 {
			t.Fatalf("setup: expected one tracked pending entry")
		}

		decoyID := h.seed.foreignTransaction(t, h.acct("Checking"),
			time.Now().UTC().AddDate(0, 0, -6), "Unrelated", "", -2500)

		booked := bookedTxn("", daysAgo(6), "25.00")
		h.eb.setPages([][]map[string]any{{booked}})
		h.syncer.run()

		after := h.actualTxns(t)
		byID := map[string]*budget.Transaction{}
		for _, tx := range after {
			byID[tx.ID] = tx
		}

		if tx := byID[trackedID]; tx == nil || !tx.Cleared {
			t.Errorf("the tracked pending row must be the one that gets confirmed, got %+v", tx)
		}
		if tx := byID[decoyID]; tx != nil && tx.Cleared {
			t.Error("an unrelated same-amount row must not be cleared by the confirm path")
		}
		if len(h.syncer.state.Pending(1)) != 0 {
			t.Error("pending entry should be cleaned up")
		}
	})
}

func TestSync_runReportsWhetherItExecuted(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	h.reloadState(t)

	if !h.syncer.run() {
		t.Error("a normal run must report that it executed")
	}

	if !h.syncer.tryAcquire() {
		t.Fatal("setup: expected to acquire the lock")
	}
	if h.syncer.run() {
		t.Error("a run skipped by the reentrancy guard must report false")
	}
	h.syncer.release()
}

func TestSync_alreadyImportedRowIsShieldedFromFuzzyTheft(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(5))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{bookedTxn("ref-A", daysAgo(3), "10.00")}})
		h.syncer.run()

		imported := h.actualTxns(t)
		if len(imported) != 1 {
			t.Fatalf("setup: got %d transactions, want 1", len(imported))
		}
		h.seed.setCleared(t, imported[0].ID, false)

		h.eb.setPages([][]map[string]any{{
			bookedTxn("ref-A", daysAgo(3), "10.00"),
			bookedTxn("ref-B", daysAgo(3), "10.00"),
		}})
		h.syncer.run()

		after := h.actualTxns(t)
		if len(after) != 2 {
			t.Fatalf("got %d transactions, want 2: a ref already in imported_refs must not be adopted by a different ref in the same batch", len(after))
		}
	})
}

func TestSync_writeFailureHoldsBackOnlyTheAffectedAccount(t *testing.T) {
	h := newHarness(t)
	idA := h.addNamedAccount(t, "BankA", "uid-a", "Checking", "")
	idB := h.addNamedAccount(t, "BankB", "uid-b", "Savings", "")
	_ = h.st.SetLastSyncDate(daysAgo(3))
	h.reloadState(t)

	acctA, err := h.syncer.ac.GetOrCreateAccount(context.Background(), budget.AccountSpec{Name: h.acct("Checking")})
	if err != nil {
		t.Fatalf("GetOrCreateAccount: %v", err)
	}
	h.fake.failWriteOnAcct = acctA.ID

	h.eb.setPages([][]map[string]any{{bookedTxn("ref-1", daysAgo(1), "10.00")}})
	h.syncer.run()

	accounts, err := h.st.GetAllBankAccounts()
	if err != nil {
		t.Fatalf("GetAllBankAccounts: %v", err)
	}
	byID := map[int64]string{}
	for _, a := range accounts {
		byID[a.ID] = a.LastSyncDate
	}
	if byID[idA] != "" {
		t.Errorf("account A had a write failure, its watermark must not advance (got %q)", byID[idA])
	}
	if byID[idB] == "" {
		t.Error("account B synced cleanly, its watermark must advance independently")
	}
}

func TestSync_writeFailureIsReportedNotSwallowed(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(3))
	h.reloadState(t)

	acct, _ := h.syncer.ac.GetOrCreateAccount(context.Background(), budget.AccountSpec{Name: h.acct("Checking")})
	h.fake.failWriteOnAcct = acct.ID

	h.eb.setPages([][]map[string]any{{bookedTxn("ref-1", daysAgo(1), "10.00")}})
	h.syncer.run()

	l := h.lastSyncLog(t)
	if l.Status != "error" {
		t.Errorf("status: got %q, want error — a failed write must not be reported as success", l.Status)
	}
	if l.Message == "" {
		t.Error("a failed write must leave a message in the sync log")
	}
}

func TestSync_startSyncDateSurvivesWriteFailure(t *testing.T) {
	h := newHarness(t)
	id := h.addAccount(t, daysAgo(20))
	h.reloadState(t)

	acct, _ := h.syncer.ac.GetOrCreateAccount(context.Background(), budget.AccountSpec{Name: h.acct("Checking")})
	h.fake.failWriteOnAcct = acct.ID

	h.eb.setPages([][]map[string]any{{bookedTxn("ref-1", daysAgo(1), "10.00")}})
	h.syncer.run()

	accounts, err := h.st.GetAllBankAccounts()
	if err != nil {
		t.Fatalf("GetAllBankAccounts: %v", err)
	}
	var found bool
	for _, a := range accounts {
		if a.ID != id {
			continue
		}
		found = true
		if a.StartSyncDate == "" {
			t.Fatal("clearing start_sync_date after a failed write loses the backfill window permanently")
		}
	}
	if !found {
		t.Fatalf("account %d is not among the %d read back, so the window was never "+
			"looked at", id, len(accounts))
	}
}

func TestSync_failedAmountCorrectionDoesNotMarkRefImported(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(5))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{pendingTxn("auth-1", daysAgo(3), "10.00")}})
	h.syncer.run()
	if _, ok := h.syncer.state.Pending(1)["auth-1"]; !ok {
		t.Fatal("setup: expected a pending entry")
	}

	h.fake.failAmountUpdates = true

	h.eb.setPages([][]map[string]any{{bookedTxn("auth-1", daysAgo(2), "12.50")}})
	h.syncer.run()

	if _, done := h.syncer.state.Imported(1)["auth-1"]; done {
		t.Error("a ref must not be marked imported when the amount correction failed — " +
			"the next run would skip it and leave the pending amount standing forever")
	}
	if _, ok := h.syncer.state.Pending(1)["auth-1"]; !ok {
		t.Error("the pending entry must survive a failed confirmation")
	}
}

func TestSync_pendingEntrySurvivesFailedRecreate(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(5))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{pendingTxn("auth-1", daysAgo(3), "10.00")}})
	h.syncer.run()

	pendingVal, ok := h.syncer.state.Pending(1)["auth-1"]
	if !ok {
		t.Fatal("setup: expected a pending entry")
	}
	trackedID, _ := splitPendingVal(pendingVal)

	h.fake.dropTransaction(trackedID)
	acct, _ := h.syncer.ac.GetOrCreateAccount(context.Background(), budget.AccountSpec{Name: h.acct("Checking")})
	h.fake.failWriteOnAcct = acct.ID

	h.eb.setPages([][]map[string]any{{bookedTxn("auth-1", daysAgo(2), "10.00")}})
	h.syncer.run()

	if _, ok := h.syncer.state.Pending(1)["auth-1"]; !ok {
		t.Error("the pending entry must not be dropped before the replacement is durable — " +
			"otherwise the transaction is orphaned and never confirmed")
	}
}

func newFireflyHarness(t *testing.T) *harness {
	t.Helper()
	srv := fireflytest.New(t)
	client := firefly.New(srv.URL, srv.Token(),
		firefly.WithHTTPClient(srv.Client()),
		firefly.WithBackoffBase(time.Millisecond))

	h := newHarness(t)
	h.fake = nil
	h.syncer.ac = firefly.NewStore(client, firefly.Config{PendingTag: "pending"})
	h.seed = fireflySeeder{srv: srv, pendingTag: "pending"}
	return h
}

type backendCase struct {
	name string
	new  func(*testing.T) *harness
}

// semanticBackends is the set the shared sync semantics must hold for. The
// matching policy lives in budget/, so a divergence here means a backend adapter
// is lying about what the store did, not that the two disagree on matching.
func semanticBackends() []backendCase {
	base := []backendCase{
		{"actual", func(t *testing.T) *harness {
			h, _ := newRealHarness(t)
			return h
		}},
		{"firefly", newFireflyHarness},
	}
	return append(base, liveBackends()...)
}

func forEachBackend(t *testing.T, body func(*testing.T, *harness)) {
	t.Helper()
	for _, b := range semanticBackends() {
		t.Run(b.name, func(t *testing.T) { body(t, b.new(t)) })
	}
}

func TestSync_zeroAmountIsDroppedNotImported(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(3))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{
			bookedTxn("zero-1", daysAgo(1), "0.00"),
			bookedTxn("real-1", daysAgo(1), "10.00"),
		}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 1 {
			t.Fatalf("got %d transactions, want 1: a zero amount has no direction and becomes a "+
				"match magnet for every other zero row", len(txns))
		}
		if txns[0].ExternalRef != "real-1" {
			t.Errorf("the wrong transaction survived: %q", txns[0].ExternalRef)
		}
		if _, done := h.syncer.state.Imported(1)["zero-1"]; done {
			t.Error("a dropped transaction must not be marked as imported")
		}
	})
}

// harnessSeeder produces the states a user creates by hand, which the bank
// import must respect. Each backend implements it against its own storage,
// because these states cannot be reached through the budget.Store port.
type harnessSeeder interface {
	// foreignTransaction is a row the user entered themselves: no bank
	// reference, not confirmed.
	foreignTransaction(t *testing.T, accountName string, date time.Time, payee, notes string, cents int64) string

	// editPayeeAndNotes is a user renaming what the importer wrote.
	editPayeeAndNotes(t *testing.T, txnID, payee, notes string)

	// setCleared is a user ticking or unticking the confirmation.
	setCleared(t *testing.T, txnID string, cleared bool)
}

type actualSeeder struct{ db *actual.DB }

func (s actualSeeder) foreignTransaction(t *testing.T, accountName string, date time.Time, payee, notes string, cents int64) string {
	t.Helper()
	acct, err := s.db.GetOrCreateAccount(accountName)
	if err != nil {
		t.Fatalf("GetOrCreateAccount: %v", err)
	}
	txn, err := s.db.CreateTransaction(date, acct, payee, notes, cents, false, "", "")
	if err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}
	s.db.FlushChanges()
	return txn.ID
}

func (s actualSeeder) editPayeeAndNotes(t *testing.T, txnID, payee, notes string) {
	t.Helper()
	pid, err := s.db.GetOrCreatePayeeForTest(payee)
	if err != nil {
		t.Fatalf("GetOrCreatePayeeForTest: %v", err)
	}
	if err := s.db.SetTransactionFieldsForTest(txnID, pid, notes); err != nil {
		t.Fatalf("SetTransactionFieldsForTest: %v", err)
	}
	s.db.FlushChanges()
}

func (s actualSeeder) setCleared(t *testing.T, txnID string, cleared bool) {
	t.Helper()
	if err := s.db.SetClearedForTest(txnID, cleared); err != nil {
		t.Fatalf("SetClearedForTest: %v", err)
	}
	s.db.FlushChanges()
}

type fireflySeeder struct {
	srv        *fireflytest.Server
	pendingTag string
}

func (s fireflySeeder) assetID(t *testing.T, name string) string {
	t.Helper()
	for _, a := range s.srv.Accounts() {
		if a.Type == "asset" && strings.EqualFold(a.Name, name) {
			return a.ID
		}
	}
	return s.srv.AddAccount(name, "asset", "EUR", "").ID
}

func (s fireflySeeder) foreignTransaction(t *testing.T, accountName string, date time.Time, payee, notes string, cents int64) string {
	t.Helper()
	assetID := s.assetID(t, accountName)

	sp := fireflytest.Split{
		Date: s.srv.FormatDate(date), Amount: firefly.FormatAmount(cents),
		Description: payee, Notes: notes, CurrencyCode: "EUR",
		Tags: []string{s.pendingTag},
	}
	if cents < 0 {
		sp.Type, sp.SourceID, sp.DestinationName = "withdrawal", assetID, payee
	} else {
		sp.Type, sp.SourceName, sp.DestinationID = "deposit", payee, assetID
	}

	g := s.srv.AddGroup(sp)
	id, err := firefly.EncodeID(g.ID, g.Splits[0].JournalID)
	if err != nil {
		t.Fatalf("EncodeID: %v", err)
	}
	return id
}

func (s fireflySeeder) mutate(t *testing.T, txnID string, fn func(*fireflytest.Split)) {
	t.Helper()
	groupID, journalID, err := firefly.SplitID(txnID)
	if err != nil {
		t.Fatalf("SplitID: %v", err)
	}
	if err := s.srv.MutateSplit(groupID, journalID, fn); err != nil {
		t.Fatalf("MutateSplit: %v", err)
	}
}

func (s fireflySeeder) editPayeeAndNotes(t *testing.T, txnID, payee, notes string) {
	t.Helper()
	s.mutate(t, txnID, func(sp *fireflytest.Split) {
		sp.Description = payee
		sp.Notes = notes
	})
}

func (s fireflySeeder) setCleared(t *testing.T, txnID string, cleared bool) {
	t.Helper()
	s.mutate(t, txnID, func(sp *fireflytest.Split) {
		if cleared {
			sp.Tags = nil
			return
		}
		sp.Tags = []string{s.pendingTag}
	})
}

func TestSync_deadlineLeavesAResumePoint(t *testing.T) {
	h := newHarness(t)
	id := h.addAccount(t, daysAgo(20))
	h.reloadState(t)

	ctx, cancel := context.WithCancel(context.Background())
	h.syncer.ctx = ctx

	h.eb.setPages([][]map[string]any{{
		bookedTxn("ref-1", daysAgo(10), "10.00"),
		bookedTxn("ref-2", daysAgo(9), "20.00"),
		bookedTxn("ref-3", daysAgo(8), "30.00"),
	}})

	// Cancel once the first transaction is durable, the way a run deadline
	// would fire in the middle of a long backfill.
	h.fake.afterWrite = func(n int) {
		if n == 1 {
			cancel()
		}
	}

	h.syncer.run()

	accounts, err := h.st.GetAllBankAccounts()
	if err != nil {
		t.Fatalf("GetAllBankAccounts: %v", err)
	}
	var acct store.BankAccount
	for _, a := range accounts {
		if a.ID == id {
			acct = a
		}
	}

	if acct.StartSyncDate == "" {
		t.Fatal("an interrupted backfill must leave a resume point, not clear the start date")
	}
	if want := daysAgo(10); acct.StartSyncDate != want {
		t.Errorf("resume point: got %q, want %q — it must advance to the last durable transaction, "+
			"otherwise every interrupted run restarts from the beginning", acct.StartSyncDate, want)
	}
	if acct.LastSyncDate != "" {
		t.Errorf("the account watermark must not advance on an interrupted run, got %q", acct.LastSyncDate)
	}
	if h.lastSyncLog(t).Status == "success" {
		t.Error("an interrupted run is not a success")
	}
	if h.fake.transactionCount() == 3 {
		t.Error("the run should have stopped early, not imported everything")
	}
}

func TestSync_processesOldestFirst(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(10))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{
		bookedTxn("newest", daysAgo(1), "10.00"),
		bookedTxn("oldest", daysAgo(9), "20.00"),
		bookedTxn("middle", daysAgo(5), "30.00"),
	}})
	h.syncer.run()

	txns := h.actualTxns(t)
	if len(txns) != 3 {
		t.Fatalf("got %d transactions, want 3", len(txns))
	}
	for i := 1; i < len(txns); i++ {
		if txns[i].Date.Before(txns[i-1].Date) {
			t.Fatalf("transactions were not imported oldest first: %v then %v",
				txns[i-1].Date, txns[i].Date)
		}
	}
}

func TestSync_migrationRefetchesTheFullWindowNotJustSinceTheLastRun(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(1))
	_ = h.st.SetBankAccountLastSyncDate(1, daysAgo(1))
	_ = h.st.AddImportedRef(1, "ref-from-actual", daysAgo(2))

	if _, _, err := h.st.ResetImportState(); err != nil {
		t.Fatalf("ResetImportState: %v", err)
	}
	h.reloadState(t)

	h.syncer.run()

	if got, want := h.eb.lastDateFrom(t), daysAgo(30); got != want {
		t.Errorf("date_from after migration: got %q, want %q — the watermark of the "+
			"previous backend is still steering the fetch, so the new backend would "+
			"receive almost nothing", got, want)
	}
}

func TestSync_migrationHonoursStartSyncDate(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, daysAgo(60))
	_ = h.st.SetLastSyncDate(daysAgo(1))
	_ = h.st.SetBankAccountLastSyncDate(1, daysAgo(1))

	if _, _, err := h.st.ResetImportState(); err != nil {
		t.Fatalf("ResetImportState: %v", err)
	}
	h.reloadState(t)

	h.syncer.run()

	if got, want := h.eb.lastDateFrom(t), daysAgo(60); got != want {
		t.Errorf("date_from after migration: got %q, want %q", got, want)
	}
}

func autoBalanceAccount(t *testing.T, h *harness, currency string) int64 {
	t.Helper()
	id, err := h.st.AddBankAccount(store.NewBankAccount{
		SessionID: "sess", AccountUID: "acct-uid", BankName: "TestBank", BankCountry: "DE",
		ActualAccount: h.acct("Checking"), Currency: currency, IBAN: "DE31123456789005193987",
		SessionExpiry:       time.Now().UTC().AddDate(0, 0, 90).Format(time.RFC3339),
		OpeningBalanceState: store.OpeningBalanceAuto,
	})
	if err != nil {
		t.Fatalf("AddBankAccount: %v", err)
	}
	return id
}

func accountRow(t *testing.T, h *harness, id int64) store.BankAccount {
	t.Helper()
	accounts, err := h.st.GetAllBankAccounts()
	if err != nil {
		t.Fatalf("GetAllBankAccounts: %v", err)
	}
	for _, a := range accounts {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("account %d not found", id)
	return store.BankAccount{}
}

func TestSync_writesTheOpeningBalanceOnTheFirstRun(t *testing.T) {
	h := newHarness(t)
	id := autoBalanceAccount(t, h, "EUR")
	h.eb.setBalances(bookedBalance("1000.00"))
	h.eb.setPages([][]map[string]any{{bookedTxn("r1", daysAgo(3), "-25.00")}})
	h.reloadState(t)

	h.syncer.run()

	a := accountRow(t, h, id)
	if a.OpeningBalanceState != store.OpeningBalanceWritten {
		t.Fatalf("state: got %q, want %q", a.OpeningBalanceState, store.OpeningBalanceWritten)
	}
	if a.OpeningBalanceCents != 102500 {
		t.Errorf("opening: got %d, want 102500 (100000 balance − (−2500) imported)",
			a.OpeningBalanceCents)
	}
	if h.fake.openingCalls != 1 {
		t.Errorf("SetOpeningBalance calls: got %d, want 1", h.fake.openingCalls)
	}
}

func TestSync_opensTheBalanceOnceAcrossRuns(t *testing.T) {
	h := newHarness(t)
	autoBalanceAccount(t, h, "EUR")
	h.eb.setBalances(bookedBalance("1000.00"))
	h.eb.setPages([][]map[string]any{{bookedTxn("r1", daysAgo(3), "-25.00")}})
	h.reloadState(t)

	h.syncer.run()
	h.syncer.run()
	h.syncer.run()

	if h.fake.openingCalls != 1 {
		t.Errorf("SetOpeningBalance calls: got %d, want 1 — the balance was written "+
			"more than once", h.fake.openingCalls)
	}
}

// A row that predates the feature must never be filled in unattended; that is
// what the explicit button is for.
func TestSync_legacyAccountGetsNoOpeningBalance(t *testing.T) {
	h := newHarness(t)
	id := h.addAccount(t, "")
	h.eb.setBalances(bookedBalance("1000.00"))
	h.eb.setPages([][]map[string]any{{bookedTxn("r1", daysAgo(3), "-25.00")}})
	h.reloadState(t)

	h.syncer.run()

	if h.fake.openingCalls != 0 {
		t.Errorf("an opening balance was written to a pre-existing account (%d calls)",
			h.fake.openingCalls)
	}
	if got := accountRow(t, h, id).OpeningBalanceState; got != store.OpeningBalanceLegacy {
		t.Errorf("state: got %q, want the legacy marker", got)
	}
}

func TestSync_openingBalanceDeferredWhenTransactionsWereDropped(t *testing.T) {
	h := newHarness(t)
	id := autoBalanceAccount(t, h, "EUR")
	h.eb.setBalances(bookedBalance("1000.00"))
	h.eb.setPages([][]map[string]any{{
		bookedTxn("r1", daysAgo(3), "-25.00"),
		{"entry_reference": "broken", "status": "BOOK"},
	}})
	h.reloadState(t)

	h.syncer.run()

	if h.fake.openingCalls != 0 {
		t.Error("an opening balance was derived while a transaction was unparseable — " +
			"that money would vanish into it and reappear as drift after a parser fix")
	}
	if got := accountRow(t, h, id).OpeningBalanceState; got != store.OpeningBalanceAuto {
		t.Errorf("state: got %q, want %q so the next run retries", got, store.OpeningBalanceAuto)
	}
}

func TestSync_openingBalanceDeferredWhenTheBalanceMoves(t *testing.T) {
	h := newHarness(t)
	id := autoBalanceAccount(t, h, "EUR")
	h.eb.setBalanceHook(func(call int) []map[string]any {
		if call == 1 {
			return bookedBalance("1000.00")
		}
		return bookedBalance("1200.00")
	})
	h.eb.setPages([][]map[string]any{{bookedTxn("r1", daysAgo(3), "-25.00")}})
	h.reloadState(t)

	h.syncer.run()

	if h.fake.openingCalls != 0 {
		t.Error("an opening balance was derived across a moving account")
	}
	a := accountRow(t, h, id)
	if a.OpeningBalanceState != store.OpeningBalanceAuto {
		t.Errorf("state: got %q, want %q", a.OpeningBalanceState, store.OpeningBalanceAuto)
	}
	if a.DriftState != store.DriftUnstable {
		t.Errorf("drift state: got %q, want %q", a.DriftState, store.DriftUnstable)
	}
}

func TestSync_noUsableBalanceIsRecordedAsUnavailable(t *testing.T) {
	h := newHarness(t)
	id := autoBalanceAccount(t, h, "EUR")
	h.eb.setBalances([]map[string]any{{
		"balance_type":   "XPCD",
		"balance_amount": map[string]any{"amount": "3000.00", "currency": "EUR"},
	}})
	h.eb.setPages([][]map[string]any{{bookedTxn("r1", daysAgo(3), "-25.00")}})
	h.reloadState(t)

	h.syncer.run()

	if h.fake.openingCalls != 0 {
		t.Error("a forward-looking balance was used")
	}
	if got := accountRow(t, h, id).OpeningBalanceState; got != store.OpeningBalanceUnavailable {
		t.Errorf("state: got %q, want %q", got, store.OpeningBalanceUnavailable)
	}
}

// Revolut reports ITAV and nothing else, and it has pending transactions. The
// available balance already reflects the hold, so the pending must be subtracted
// as well or it is counted twice.
func TestSync_availableOnlyBankGetsAnOpeningBalance(t *testing.T) {
	h := newHarness(t)
	id := autoBalanceAccount(t, h, "EUR")
	h.eb.setBalances([]map[string]any{{
		"balance_type":   "ITAV",
		"balance_amount": map[string]any{"amount": "1000.00", "currency": "EUR"},
	}})
	h.eb.setPages([][]map[string]any{{
		bookedTxn("r1", daysAgo(3), "-25.00"),
		pendingTxn("p1", daysAgo(1), "-30.00"),
	}})
	h.reloadState(t)

	h.syncer.run()

	a := accountRow(t, h, id)
	if a.OpeningBalanceState != store.OpeningBalanceWritten {
		t.Fatalf("state: got %q, want %q", a.OpeningBalanceState, store.OpeningBalanceWritten)
	}
	if a.OpeningBalanceCents != 105500 {
		t.Errorf("opening: got %d, want 105500 (100000 available − (−2500 booked + "+
			"−3000 pending)); the pending was not subtracted, so it is counted twice",
			a.OpeningBalanceCents)
	}
	if a.DriftState != store.DriftOK || a.DriftCents != 0 {
		t.Errorf("drift: got %d / %q, want 0 / ok — an available balance already "+
			"contains the pending, so it must not be added again",
			a.DriftCents, a.DriftState)
	}
}

func TestSync_deniedBalanceScopeDoesNotFailTheSync(t *testing.T) {
	h := newHarness(t)
	id := autoBalanceAccount(t, h, "EUR")
	h.eb.setBalanceStatus(http.StatusForbidden)
	h.eb.setPages([][]map[string]any{{bookedTxn("r1", daysAgo(3), "-25.00")}})
	h.reloadState(t)

	h.syncer.run()

	if got := h.lastSyncLog(t).Status; got != "success" {
		t.Errorf("sync status: got %q, want success — a missing balance scope must not "+
			"cost the user their transaction import", got)
	}
	if h.fake.transactionCount() != 1 {
		t.Errorf("transactions imported: got %d, want 1", h.fake.transactionCount())
	}
	a := accountRow(t, h, id)
	if a.BalancesAccess != "denied" || a.OpeningBalanceState != store.OpeningBalanceDenied {
		t.Errorf("state: access=%q opening=%q, want denied/denied", a.BalancesAccess, a.OpeningBalanceState)
	}
}

// An account whose window is empty still has a balance, and used to be skipped
// before it was ever created in the budget.
func TestSync_emptyWindowStillCreatesTheAccount(t *testing.T) {
	h := newHarness(t)
	autoBalanceAccount(t, h, "EUR")
	h.eb.setBalances(bookedBalance("1000.00"))
	h.eb.setPages([][]map[string]any{{}})
	h.reloadState(t)

	h.syncer.run()

	if len(h.fake.accounts) != 1 {
		t.Fatalf("budget accounts: got %d, want 1 — a freshly connected account with "+
			"no transactions in the window never reached the backend", len(h.fake.accounts))
	}
	if h.fake.openingCents != 100000 {
		t.Errorf("opening: got %d, want 100000", h.fake.openingCents)
	}
}

func TestSync_driftIsReportedNotCorrected(t *testing.T) {
	h := newHarness(t)
	id := autoBalanceAccount(t, h, "EUR")
	h.eb.setBalances(bookedBalance("1000.00"))
	h.eb.setPages([][]map[string]any{{bookedTxn("r1", daysAgo(3), "-25.00")}})
	h.reloadState(t)

	h.syncer.run()
	if got := accountRow(t, h, id).DriftState; got != store.DriftOK {
		t.Fatalf("first run drift: got %q, want %q", got, store.DriftOK)
	}
	countAfterFirst := h.fake.transactionCount()

	// Someone deletes a row in the budget. The bank still knows about it.
	h.fake.mu.Lock()
	h.fake.txns = h.fake.txns[:len(h.fake.txns)-1]
	h.fake.mu.Unlock()

	h.syncer.run()

	a := accountRow(t, h, id)
	if a.DriftState != store.DriftAlert {
		t.Errorf("drift state: got %q, want %q", a.DriftState, store.DriftAlert)
	}
	if a.DriftCents == 0 {
		t.Error("drift was reported as zero although the budget lost a transaction")
	}
	if h.fake.transactionCount() > countAfterFirst {
		t.Error("drift was corrected by writing a transaction; it must only be reported")
	}
}

func TestSync_pendingTransactionsAreNotDrift(t *testing.T) {
	h := newHarness(t)
	id := autoBalanceAccount(t, h, "EUR")
	h.eb.setBalances(bookedBalance("1000.00"))
	h.eb.setPages([][]map[string]any{{
		bookedTxn("r1", daysAgo(3), "-25.00"),
		pendingTxn("p1", daysAgo(1), "-30.00"),
	}})
	h.reloadState(t)

	h.syncer.run()

	a := accountRow(t, h, id)
	if a.DriftState != store.DriftOK || a.DriftCents != 0 {
		t.Errorf("drift: got %d / %q, want 0 / ok — an outstanding pending is expected "+
			"to make the budget lead the booked balance, not to be an alarm",
			a.DriftCents, a.DriftState)
	}
}

func TestSync_driftNotReportedBeforeAnOpeningBalanceExists(t *testing.T) {
	h := newHarness(t)
	id := h.addAccount(t, "")
	h.eb.setBalances(bookedBalance("1000.00"))
	h.eb.setPages([][]map[string]any{{bookedTxn("r1", daysAgo(3), "-25.00")}})
	h.reloadState(t)

	h.syncer.run()

	a := accountRow(t, h, id)
	if a.DriftState != store.DriftNoOpening {
		t.Errorf("drift state: got %q, want %q — without an opening balance the "+
			"difference is just the pre-window money", a.DriftState, store.DriftNoOpening)
	}
	if a.DriftCents != 0 {
		t.Errorf("drift cents: got %d, want 0", a.DriftCents)
	}
}

// The namespace is dormant for every backend in this repo, so a call site that
// bypasses h.acct would never be noticed until a live backend made two tests
// share an account. This switches it on and checks the three ways a name reaches
// a backend — the sync loop, the read-back helper, and the seeder — against both
// real backends.
func TestHarness_namespaceReachesEveryAccountName(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		// Derived from the harness's own namespace rather than written out. A
		// fixed prefix would collide with itself on the second run against a
		// long-lived instance, and — worse — the bare account below would carry no
		// disposable prefix at all, so AssertDisposable would refuse to touch that
		// instance ever again. Offline h.ns is empty and this reads as it did.
		base := h.ns
		h.ns = base + "ns-"
		h.addAccount(t, "")
		h.eb.setPages([][]map[string]any{{bookedTxn("r1", daysAgo(3), "-25.00")}})
		h.reloadState(t)

		h.syncer.run()

		// Write and read must agree, or the assertions in every other test would
		// be looking at a different account than the sync loop wrote to.
		if got := len(h.actualTxns(t)); got != 1 {
			t.Fatalf("transactions read back: got %d, want 1", got)
		}

		// The decisive check: the unqualified name must resolve to a *different*,
		// empty account. If the namespace were silently dropped somewhere, both
		// names would land on the same one and this would find the transaction.
		bare, err := h.syncer.ac.GetOrCreateAccount(context.Background(),
			budget.AccountSpec{Name: base + "Checking", Currency: "EUR"})
		if err != nil {
			t.Fatalf("GetOrCreateAccount: %v", err)
		}
		lo, hi := budget.WindowBounds(time.Now().UTC())
		leaked, err := h.syncer.ac.ListTransactions(context.Background(), bare.ID, lo.AddDate(0, 0, -30), hi)
		if err != nil {
			t.Fatalf("ListTransactions: %v", err)
		}
		if len(leaked) != 0 {
			t.Errorf("the unqualified account holds %d transaction(s); a call site "+
				"builds the account name without the namespace", len(leaked))
		}

		// The seeder is the third way a name enters a backend.
		h.seed.foreignTransaction(t, h.acct("Checking"), time.Now().UTC().AddDate(0, 0, -2), "Someone", "", -500)
		if got := len(h.actualTxns(t)); got != 2 {
			t.Errorf("after seeding: got %d transactions, want 2 — the seeder wrote "+
				"to a different account than the sync loop", got)
		}
	})
}

// autoBalanceAccount writes the bank account row directly instead of going
// through addNamedAccount, so it needs its own qualification and its own check.
// The balance tests only ever run against the fake backend today, which is
// exactly why this would otherwise be an untested line.
func TestAutoBalanceAccount_carriesTheNamespace(t *testing.T) {
	h := newHarness(t)
	h.ns = "nstest-"

	id := autoBalanceAccount(t, h, "EUR")

	if got := accountRow(t, h, id).ActualAccount; got != "nstest-Checking" {
		t.Errorf("stored account name: got %q, want %q", got, "nstest-Checking")
	}
}

// collectAttrSets returns the attribute set of every data point of a metric,
// whatever its instrument type. The tests here care which labels were attached,
// not what was measured, and a counter and a histogram answer that the same way.
func collectAttrSets(t *testing.T, reader *sdkmetric.ManualReader, name string) []attribute.Set {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var out []attribute.Set
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					out = append(out, dp.Attributes)
				}
			case metricdata.Gauge[int64]:
				for _, dp := range d.DataPoints {
					out = append(out, dp.Attributes)
				}
			case metricdata.Gauge[float64]:
				for _, dp := range d.DataPoints {
					out = append(out, dp.Attributes)
				}
			case metricdata.Histogram[float64]:
				for _, dp := range d.DataPoints {
					out = append(out, dp.Attributes)
				}
			default:
				t.Fatalf("%s is %T, which this helper does not know", name, m.Data)
			}
		}
	}
	return out
}

// collectValues returns each data point of a gauge or counter with the value of
// one attribute alongside it, which is what most of the model-health series need
// checking: the interesting thing is not that a series exists but that a
// particular member of it carries the right number.
func collectValues(t *testing.T, reader *sdkmetric.ManualReader, name, key string) map[string]float64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	out := map[string]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Gauge[float64]:
				for _, dp := range d.DataPoints {
					out[attrOf(dp.Attributes, key)] = dp.Value
				}
			case metricdata.Gauge[int64]:
				for _, dp := range d.DataPoints {
					out[attrOf(dp.Attributes, key)] = float64(dp.Value)
				}
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					out[attrOf(dp.Attributes, key)] += float64(dp.Value)
				}
			default:
				t.Fatalf("%s is %T, which this helper does not know", name, m.Data)
			}
		}
	}
	return out
}

func attrOf(set attribute.Set, key string) string {
	v, ok := set.Value(attribute.Key(key))
	if !ok {
		return ""
	}
	return v.AsString()
}

// TestSync_countersCarryTheBackend is what makes a dashboard usable now that
// there are two backends.
//
// Without the label, "transactions added" is one series covering both, and the
// question a backend switch actually raises — is the new one keeping up — cannot
// be asked of the data at all.
func TestSync_countersCarryTheBackend(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	h.reloadState(t)

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	h.syncer.met = newSyncMetrics(provider.Meter("bankingsync"))

	// Named explicitly. The harness leaves backendName empty, and comparing the
	// label against an empty string would pass whether or not the label is there
	// at all — a check that cannot fail is worse than no check.
	const want = "firefly"
	h.syncer.backendName = want

	h.eb.setPages([][]map[string]any{{bookedTxn("ref-1", daysAgo(1), "10.00")}})
	h.syncer.run()

	for _, name := range []string{
		"bankingsync_transactions_added_total",
		"bankingsync_sync_runs_total",
		"bankingsync_sync_duration_seconds",
	} {
		sets := collectAttrSets(t, reader, name)
		if len(sets) == 0 {
			t.Errorf("%s produced no data point", name)
			continue
		}
		for _, set := range sets {
			if got := attrOf(set, "backend"); got != want {
				t.Errorf("%s: backend label = %q, want %q", name, got, want)
			}
		}
	}
}

// TestSync_driftGaugeIsActuallyObserved pins the fix for a metric that was
// declared, documented and never emitted.
//
// An observable gauge without a registered callback produces nothing. This one
// had none, so bankingsync_balance_drift_cents existed in the code and in the
// README and in no metrics backend anywhere.
func TestSync_driftGaugeIsActuallyObserved(t *testing.T) {
	h := newHarness(t)
	id := h.addAccount(t, "")
	h.reloadState(t)

	// Explicit for the same reason as in the counter test: the harness leaves it
	// empty, and an empty expectation is one no missing label can violate.
	h.syncer.backendName = "firefly"

	if err := h.syncer.st.SetAccountDrift(id, -1234, store.DriftAlert); err != nil {
		t.Fatalf("SetAccountDrift: %v", err)
	}

	var observed []int64
	var states, backends []string
	err := h.syncer.observeDrift(recordingObserver{
		fn: func(v int64, opts []metric.ObserveOption) {
			observed = append(observed, v)
			cfg := metric.NewObserveConfig(opts)
			set := cfg.Attributes()
			for _, kv := range set.ToSlice() {
				switch kv.Key {
				case "state":
					states = append(states, kv.Value.AsString())
				case "backend":
					backends = append(backends, kv.Value.AsString())
				}
			}
		},
	})
	if err != nil {
		t.Fatalf("observeDrift: %v", err)
	}

	if len(observed) != 1 || observed[0] != -1234 {
		t.Fatalf("observed %v, want a single -1234", observed)
	}
	if len(states) != 1 || states[0] != store.DriftAlert {
		t.Errorf("state label: got %v, want [%s] — without it, a drift of zero and "+
			"an account that could not be compared look identical", states, store.DriftAlert)
	}
	if len(backends) != 1 || backends[0] != "firefly" {
		t.Errorf("backend label: got %v, want [firefly]", backends)
	}
}

// TestSync_driftGaugeSkipsAccountsNeverCompared keeps the gauge honest. Reporting
// zero for an account that has never been checked would read as "agrees to the
// cent" on every dashboard.
func TestSync_driftGaugeSkipsAccountsNeverCompared(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	h.reloadState(t)

	var seen int
	if err := h.syncer.observeDrift(recordingObserver{
		fn: func(int64, []metric.ObserveOption) { seen++ },
	}); err != nil {
		t.Fatalf("observeDrift: %v", err)
	}
	if seen != 0 {
		t.Errorf("observed %d account(s) that were never compared", seen)
	}
}

type recordingObserver struct {
	metric.Int64Observer
	fn func(int64, []metric.ObserveOption)
}

func (r recordingObserver) Observe(v int64, opts ...metric.ObserveOption) { r.fn(v, opts) }

// TestSync_balanceChecksAreCountedByOutcome pins the metric the drift gauge
// cannot provide.
//
// The gauge reports the last known state per account, so an account that came out
// unstable on three of the last ten runs and settled on the fourth is
// indistinguishable from one that has never had a problem. Only a counter shows
// how often a comparison was worth nothing.
func TestSync_balanceChecksAreCountedByOutcome(t *testing.T) {
	h := newHarness(t)
	acctID := h.addAccount(t, "")
	h.reloadState(t)

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	h.syncer.met = newSyncMetrics(provider.Meter("bankingsync"))
	h.syncer.backendName = "firefly"

	accounts, err := h.syncer.st.GetAllBankAccounts()
	if err != nil || len(accounts) == 0 {
		t.Fatalf("reading the account back: %v", err)
	}
	var acct store.BankAccount
	for _, a := range accounts {
		if a.ID == acctID {
			acct = a
		}
	}

	h.syncer.recordDrift(acct, 0, store.DriftUnstable)
	h.syncer.recordDrift(acct, 0, store.DriftUnstable)
	h.syncer.recordDrift(acct, -500, store.DriftAlert)

	byState := map[string]int{}
	for _, set := range collectAttrSets(t, reader, "bankingsync_balance_checks_total") {
		if got := attrOf(set, "backend"); got != "firefly" {
			t.Errorf("backend label = %q, want firefly", got)
		}
		byState[attrOf(set, "state")]++
	}
	for _, want := range []string{store.DriftUnstable, store.DriftAlert} {
		if byState[want] == 0 {
			t.Errorf("no series for state %q; got %v", want, byState)
		}
	}
}

// TestSync_balanceCheckCounterSeparatesTheStates guards the label itself. Without
// it every outcome collapses into one number, and "the comparison ran" cannot be
// told from "the comparison concluded nothing".
func TestSync_balanceCheckCounterSeparatesTheStates(t *testing.T) {
	h := newHarness(t)
	acctID := h.addAccount(t, "")
	h.reloadState(t)

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	h.syncer.met = newSyncMetrics(provider.Meter("bankingsync"))

	accounts, _ := h.syncer.st.GetAllBankAccounts()
	var acct store.BankAccount
	for _, a := range accounts {
		if a.ID == acctID {
			acct = a
		}
	}

	h.syncer.recordDrift(acct, 0, store.DriftOK)
	h.syncer.recordDrift(acct, 0, store.DriftIncomplete)

	states := map[string]bool{}
	for _, set := range collectAttrSets(t, reader, "bankingsync_balance_checks_total") {
		states[attrOf(set, "state")] = true
	}
	if len(states) != 2 {
		t.Errorf("got %d distinct states, want 2: %v", len(states), states)
	}
}

// logRecorder captures structured records so a test can assert what reached the
// OTLP pipeline, as opposed to what reached stderr. The distinction matters:
// log.Printf is not bridged into the exporter, so anything logged only that way
// is invisible to every backend the operator actually looks at.
type logRecorder struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (r *logRecorder) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }

func (r *logRecorder) OnEmit(_ context.Context, rec *sdklog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, *rec)
	return nil
}

func (r *logRecorder) Shutdown(context.Context) error   { return nil }
func (r *logRecorder) ForceFlush(context.Context) error { return nil }

func (r *logRecorder) find(body string) (sdklog.Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.records {
		if rec.Body().AsString() == body {
			return rec, true
		}
	}
	return sdklog.Record{}, false
}

func (r *logRecorder) bodies() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, rec := range r.records {
		out = append(out, rec.Body().AsString())
	}
	return out
}

func recordLogs(t *testing.T) *logRecorder {
	t.Helper()
	rec := &logRecorder{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(rec))
	prev := global.GetLoggerProvider()
	global.SetLoggerProvider(lp)
	t.Cleanup(func() { global.SetLoggerProvider(prev) })
	return rec
}

func recordAttr(rec sdklog.Record, key string) string {
	var out string
	rec.WalkAttributes(func(kv otellog.KeyValue) bool {
		if string(kv.Key) == key {
			out = kv.Value.AsString()
			return false
		}
		return true
	})
	return out
}

// TestBalance_decisionsReachTheLogPipeline covers the half of balance.go that was
// invisible: every decision it makes was a log.Printf, which goes to stderr and
// nowhere else.
func TestBalance_decisionsReachTheLogPipeline(t *testing.T) {
	h := newHarness(t)
	id := h.addAccount(t, "")
	h.reloadState(t)
	rec := recordLogs(t)

	accounts, err := h.syncer.st.GetAllBankAccounts()
	if err != nil {
		t.Fatalf("GetAllBankAccounts: %v", err)
	}
	var acct store.BankAccount
	for _, a := range accounts {
		if a.ID == id {
			acct = a
		}
	}

	h.syncer.settleBalances(context.Background(), acct, "1", time.Now().UTC(),
		nil, errors.New("consent expired"), nil, 0, false, false)

	got, ok := rec.find("balance.unavailable")
	if !ok {
		t.Fatalf("nothing structured was emitted for an unreachable balance; got %v", rec.bodies())
	}
	if v := recordAttr(got, "error"); !strings.Contains(v, "consent expired") {
		t.Errorf("the record does not carry the cause: error=%q", v)
	}
	if recordAttr(got, "bank") == "" {
		t.Error("the record does not name the bank, so it cannot be filtered per account")
	}
}

// TestBalance_nearMissReachesTheLogPipeline covers the other end of the same gap.
// A near miss is bankingsync choosing to create a transaction rather than adopt a
// close one, and which reason fired is the whole diagnostic value.
func TestBalance_nearMissReachesTheLogPipeline(t *testing.T) {
	h := newHarness(t)
	rec := recordLogs(t)

	h.syncer.nearMiss("TestBank", "ambiguous", nil)

	got, ok := rec.find("match.near_miss")
	if !ok {
		t.Fatalf("no structured record for a near miss; got %v", rec.bodies())
	}
	if v := recordAttr(got, "reason"); v != "ambiguous" {
		t.Errorf("reason = %q, want ambiguous", v)
	}
}

// TestSync_zeroAmountDoesNotDegradeTheRun separates a deliberate skip from a
// defect.
//
// A zero-amount row has no direction, so bankingsync declines it on purpose —
// and at banks that issue them routinely, Revolut among them, counting each one
// as a dropped transaction turned every ordinary sync into a degraded one and
// sent an alert saying the row "could not be parsed", which was never true.
func TestSync_zeroAmountDoesNotDegradeTheRun(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(3))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{
		bookedTxn("zero-1", daysAgo(1), "0.00"),
		bookedTxn("zero-2", daysAgo(1), "0.00"),
		bookedTxn("real-1", daysAgo(1), "10.00"),
	}})
	h.syncer.run()

	l := h.lastSyncLog(t)
	if l.Status != "success" {
		t.Errorf("status = %q, want success: skipped zero rows are not a fault", l.Status)
	}
	// The message is what the alert mail is built from, so an empty one is also
	// the assertion that no mail went out.
	if l.Message != "" {
		t.Errorf("sync message = %q, want empty; this text is what the alert email "+
			"reports, and there is nothing to report", l.Message)
	}
	if l.TxAdded != 1 {
		t.Errorf("added = %d, want 1", l.TxAdded)
	}
}

// TestSync_parseFailureStillDegradesTheRun is the other half. Separating the two
// counters must not quiet the case that genuinely is a defect.
func TestSync_parseFailureStillDegradesTheRun(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(3))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{
		bookedTxn("real-1", daysAgo(1), "10.00"),
		{"entry_reference": "broken"},
	}})
	h.syncer.run()

	l := h.lastSyncLog(t)
	if l.Status != "degraded" {
		t.Errorf("status = %q, want degraded: a row Enable Banking sent could not be parsed", l.Status)
	}
	if !strings.Contains(l.Message, "could not be parsed") {
		t.Errorf("sync message = %q, want it to name the parse failure", l.Message)
	}
}

// TestSync_zeroAmountRowsAreCounted keeps the skips visible. They stop being an
// alert, they do not stop being a number worth watching — a bank that suddenly
// sends nothing but zero rows is a real problem wearing a quiet coat.
func TestSync_zeroAmountRowsAreCounted(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(3))
	h.reloadState(t)

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	h.syncer.met = newSyncMetrics(provider.Meter("bankingsync"))

	h.eb.setPages([][]map[string]any{{
		bookedTxn("zero-1", daysAgo(1), "0.00"),
		bookedTxn("zero-2", daysAgo(1), "0.00"),
		bookedTxn("real-1", daysAgo(1), "10.00"),
	}})
	h.syncer.run()

	var total int64
	var banked bool
	for _, set := range collectAttrSets(t, reader, "bankingsync_transactions_zero_amount_total") {
		if attrOf(set, "bank") == "TestBank" {
			banked = true
		}
	}
	for _, dp := range collectInt64Points(t, reader, "bankingsync_transactions_zero_amount_total") {
		total += dp.Value
	}
	if total != 2 {
		t.Errorf("zero-amount counter = %d, want 2", total)
	}
	if !banked {
		t.Error("the counter does not name the bank, so it cannot be attributed")
	}

	// And the dropped counter must stay clean: that one means a parse failure.
	for _, dp := range collectInt64Points(t, reader, "bankingsync_transactions_dropped_total") {
		if dp.Value != 0 {
			t.Errorf("dropped counter = %d, want 0: nothing failed to parse", dp.Value)
		}
	}
}

// collectInt64Points returns the int64 data points of a counter by name.
func collectInt64Points(t *testing.T, reader *sdkmetric.ManualReader, name string) []metricdata.DataPoint[int64] {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				return sum.DataPoints
			}
		}
	}
	return nil
}

func pendingTxnPayee(ref, date, amount, payee string) map[string]any {
	tx := bookedTxnPayee(ref, date, amount, payee)
	tx["status"] = "PDNG"
	return tx
}

// TestSync_unrelatedBookingDoesNotHijackAPendingRow is the reported defect driven
// through the whole sync, not just the matching unit.
//
// The amount tolerance was added so a hotel authorised at 120.00 and booked at
// 138.50 settles onto one row. It asked nothing about the payee, so any single
// open row within the tolerance was adopted whatever it was — and since an
// adopted row is rewritten with the booked amount, payee and reference, the
// pending transaction was not left unmatched but replaced by an unrelated one.
func TestSync_unrelatedBookingDoesNotHijackAPendingRow(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(6))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{
			pendingTxnPayee("auth-1", daysAgo(4), "120.00", "Hotel Berlin"),
		}})
		h.syncer.run()

		h.eb.setPages([][]map[string]any{{
			bookedTxnPayee("book-1", daysAgo(2), "138.50", "Elektromarkt Nord"),
		}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 2 {
			t.Fatalf("got %d transactions, want 2: an unrelated booking adopted the "+
				"authorisation instead of being imported on its own", len(txns))
		}

		byPayee := map[string]int64{}
		for _, tx := range txns {
			byPayee[tx.PayeeName] = tx.AmountCents
		}
		if got, ok := byPayee["Hotel Berlin"]; !ok || got != -12000 {
			t.Errorf("the authorisation is gone or was rewritten: payees %v", byPayee)
		}
		if got, ok := byPayee["Elektromarkt Nord"]; !ok || got != -13850 {
			t.Errorf("the booking did not arrive on its own: payees %v", byPayee)
		}
	})
}

// TestSync_settlingAuthorisationStillConfirms is the case the tolerance exists
// for, and the one this project was asked for in the first place: the same
// purchase arriving pending as "Hotel Berlin" and booked as "VISA Hotel Berlin",
// at a different amount, must end as one confirmed row.
func TestSync_settlingAuthorisationStillConfirms(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(6))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{
			pendingTxnPayee("auth-1", daysAgo(4), "120.00", "Hotel Berlin"),
		}})
		h.syncer.run()

		h.eb.setPages([][]map[string]any{{
			bookedTxnPayee("book-1", daysAgo(2), "138.50", "VISA Hotel Berlin"),
		}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 1 {
			t.Fatalf("got %d transactions, want 1: the settling authorisation must confirm "+
				"onto the row it authorised", len(txns))
		}
		if txns[0].AmountCents != -13850 {
			t.Errorf("amount = %d, want the booked -13850", txns[0].AmountCents)
		}
		if !txns[0].Cleared {
			t.Error("the adopted row was not confirmed")
		}
	})
}

// TestSync_truncatedPayeeStillConfirms is the reported case, driven through the
// whole sync.
//
// The bank prepends the card scheme and then cuts the tail to fit its field, so
// "Da Luigi Roma" comes back as "Visa Da Luigi". No equality rule joins those,
// and the rule that was in place refused them outright — correctly, given what
// it had to work with. What settles it is that the booked name is the pending
// one with a word removed rather than a word contradicted.
func TestSync_truncatedPayeeStillConfirms(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(6))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{
			pendingTxnPayee("auth-1", daysAgo(4), "120.00", "Da Luigi Roma"),
		}})
		h.syncer.run()

		h.eb.setPages([][]map[string]any{{
			bookedTxnPayee("book-1", daysAgo(2), "138.50", "Visa Da Luigi"),
		}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 1 {
			t.Fatalf("got %d transactions, want 1: the settlement must confirm onto the "+
				"authorisation it belongs to", len(txns))
		}
		if txns[0].AmountCents != -13850 {
			t.Errorf("amount = %d, want the booked -13850", txns[0].AmountCents)
		}
		if !txns[0].Cleared {
			t.Error("the adopted row was not confirmed")
		}
	})
}

// TestSync_anotherBranchOfTheSameChainDoesNotConfirm is the counterweight, and
// the reason a similarity score was not enough: "Visa Da Luigi Milano" shares
// just as many words with "Da Luigi Roma" as "Visa Da Luigi" does. What differs
// is that it contradicts one, and contradiction is not weak agreement.
func TestSync_anotherBranchOfTheSameChainDoesNotConfirm(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(6))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{
			pendingTxnPayee("auth-1", daysAgo(4), "120.00", "Da Luigi Roma"),
		}})
		h.syncer.run()

		h.eb.setPages([][]map[string]any{{
			bookedTxnPayee("book-1", daysAgo(2), "138.50", "Visa Da Luigi Milano"),
		}})
		h.syncer.run()

		if got := len(h.actualTxns(t)); got != 2 {
			t.Fatalf("got %d transactions, want 2: a different branch must not take the "+
				"authorisation", got)
		}
	})
}

// TestSync_uncertainMatchIsHeldNotGuessed is the middle band arriving in the
// sync loop.
//
// A pending Spotify charge and a Netflix booking at the same price on the same
// day: too much agreement to dismiss, too little to act on. Both available
// guesses are worse than asking — importing leaves a duplicate somebody has to
// find, adopting overwrites an authorisation nobody would notice was gone.
func TestSync_uncertainMatchIsHeldNotGuessed(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(6))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{
			pendingTxnPayee("auth-1", daysAgo(3), "9.99", "Spotify"),
		}})
		h.syncer.run()

		h.eb.setPages([][]map[string]any{{
			bookedTxnPayee("book-1", daysAgo(3), "9.99", "Netflix"),
		}})
		h.syncer.run()

		if got := len(h.actualTxns(t)); got != 1 {
			t.Fatalf("got %d transactions, want 1: the booking must not enter the budget "+
				"while the decision is open", got)
		}

		reviews, err := h.st.GetMatchReviews()
		if err != nil {
			t.Fatalf("GetMatchReviews: %v", err)
		}
		if len(reviews) != 1 {
			t.Fatalf("got %d held transactions, want 1 — the booking has vanished", len(reviews))
		}
		if reviews[0].Payee != "Netflix" {
			t.Errorf("held the wrong one: %q", reviews[0].Payee)
		}
		if reviews[0].AmountCents != -999 {
			t.Errorf("amount = %d, want -999: the import must be reconstructable", reviews[0].AmountCents)
		}
		if reviews[0].BestPayeeLevel == "" || reviews[0].BestAmountLevel == "" {
			t.Error("the evidence that produced the doubt was not recorded")
		}
	})
}

// TestSync_aHeldTransactionIsNotOfferedTwice keeps the queue from filling with
// the same decision on every cycle, and keeps the ordinary path from importing
// it behind the decision's back.
func TestSync_aHeldTransactionIsNotOfferedTwice(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(6))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("auth-1", daysAgo(3), "9.99", "Spotify"),
	}})
	h.syncer.run()

	booked := [][]map[string]any{{bookedTxnPayee("book-1", daysAgo(3), "9.99", "Netflix")}}
	h.eb.setPages(booked)
	h.syncer.run()
	h.eb.setPages(booked)
	h.syncer.run()

	reviews, err := h.st.GetMatchReviews()
	if err != nil {
		t.Fatalf("GetMatchReviews: %v", err)
	}
	if len(reviews) != 1 {
		t.Errorf("got %d held transactions after two syncs, want 1", len(reviews))
	}
	if got := len(h.actualTxns(t)); got != 1 {
		t.Errorf("got %d transactions, want 1: a held transaction slipped in on the "+
			"second pass", got)
	}
}

// TestSync_aHeldTransactionSurvivesItsCandidateDisappearing is what the held-key
// check is actually for, and the unique key in the database cannot do it.
//
// If the row that caused the doubt goes away — the user deletes it, or it is
// confirmed by something else — then nothing is close any more, and the ordinary
// path would import the held transaction as new. That would carry out one of the
// two options while the decision was still open, and the wrong one at that: the
// person may have been about to say "these are the same".
func TestSync_aHeldTransactionSurvivesItsCandidateDisappearing(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(6))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{
			pendingTxnPayee("auth-1", daysAgo(3), "9.99", "Spotify"),
		}})
		h.syncer.run()

		booked := [][]map[string]any{{bookedTxnPayee("book-1", daysAgo(3), "9.99", "Netflix")}}
		h.eb.setPages(booked)
		h.syncer.run()

		if n, _ := h.st.CountMatchReviews(); n != 1 {
			t.Fatalf("setup: %d held transactions, want 1", n)
		}

		// The candidate stops being adoptable: confirmed, and carrying a reference
		// that is not the held transaction's.
		existing := h.actualTxns(t)
		if len(existing) != 1 {
			t.Fatalf("setup: %d rows in the budget, want 1", len(existing))
		}
		h.seed.setCleared(t, existing[0].ID, true)

		h.eb.setPages(booked)
		h.syncer.run()

		if got := len(h.actualTxns(t)); got != 1 {
			t.Errorf("got %d transactions, want 1: the held one was imported while its "+
				"decision was still open", got)
		}
		if n, _ := h.st.CountMatchReviews(); n != 1 {
			t.Errorf("%d held transactions, want 1", n)
		}
	})
}

// holdOne drives a sync into the state the review queue exists for: a booking
// that is close enough to an open authorisation to be worth asking about, and
// not close enough to merge. It returns the one held transaction.
func holdOne(t *testing.T, h *harness) store.MatchReview {
	t.Helper()
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(6))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("auth-1", daysAgo(3), "9.99", "Spotify"),
	}})
	h.syncer.run()

	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("book-1", daysAgo(3), "9.99", "Netflix"),
	}})
	h.syncer.run()

	reviews, err := h.st.GetMatchReviews()
	if err != nil {
		t.Fatalf("GetMatchReviews: %v", err)
	}
	if len(reviews) != 1 {
		t.Fatalf("setup: %d held transactions, want 1", len(reviews))
	}
	return reviews[0]
}

func TestReviewQueue_offersTheCandidateThatCausedTheDoubt(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		held := holdOne(t, h)

		items, err := h.syncer.HeldTransactions(context.Background())
		if err != nil {
			t.Fatalf("HeldTransactions: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("got %d items, want 1", len(items))
		}
		it := items[0]
		if it.ID != held.ID || it.Payee != "Netflix" {
			t.Errorf("got id=%d payee=%q, want id=%d payee=Netflix", it.ID, it.Payee, held.ID)
		}
		if len(it.Candidates) != 1 {
			t.Fatalf("got %d candidates, want the open authorisation", len(it.Candidates))
		}
		c := it.Candidates[0]
		if c.PayeeName != "Spotify" {
			t.Errorf("candidate payee %q, want Spotify", c.PayeeName)
		}
		if c.Percent <= 0 || c.Percent >= 100 {
			t.Errorf("candidate probability %d%%; a held case is by definition neither", c.Percent)
		}
		// The number alone is not a decision aid. Whether the amounts agree is
		// the thing a person can actually check against their receipt.
		if !strings.Contains(c.Why, "amount") || !strings.Contains(c.Why, "payee") {
			t.Errorf("the reason %q does not name the fields it is made of", c.Why)
		}
	})
}

// TestReviewQueue_assigningMergesAndClearsTheQueue is the whole point of the
// feature: the person decides, the budget ends up with one row, and the
// transaction never comes back.
func TestReviewQueue_assigningMergesAndClearsTheQueue(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		holdOne(t, h)

		items, err := h.syncer.HeldTransactions(context.Background())
		if err != nil {
			t.Fatalf("HeldTransactions: %v", err)
		}
		c := items[0].Candidates[0]
		if err := h.syncer.ResolveHeld(context.Background(), items[0].ID, c.ID, c.Percent, h.syncer.matchPolicy("").Version()); err != nil {
			t.Fatalf("ResolveHeld: %v", err)
		}

		txns := h.actualTxns(t)
		if len(txns) != 1 {
			t.Fatalf("got %d transactions, want 1 merged row", len(txns))
		}
		if txns[0].AmountCents != -999 {
			t.Errorf("amount %d, want -999: the merged row must carry the booked figure",
				txns[0].AmountCents)
		}
		if !txns[0].Cleared {
			t.Error("the merged row is still unconfirmed although the booking settled it")
		}
		if n, _ := h.st.CountMatchReviews(); n != 0 {
			t.Errorf("%d decisions left in the queue", n)
		}

		// The bank keeps offering a booking for days. Once decided it must not
		// come back — neither into the queue nor into the budget.
		h.eb.setPages([][]map[string]any{{
			bookedTxnPayee("book-1", daysAgo(3), "9.99", "Netflix"),
		}})
		h.syncer.run()
		if got := len(h.actualTxns(t)); got != 1 {
			t.Errorf("got %d transactions after a further sync, want 1", got)
		}
		if n, _ := h.st.CountMatchReviews(); n != 0 {
			t.Errorf("%d decisions came back", n)
		}
	})
}

// TestReviewQueue_theAnswerIsFiledAgainstThePairThePersonChose is the label
// integrity finding, and it is the one that reaches furthest.
//
// A held decision is logged with the comparison levels of the model's best
// candidate, because at the time it is logged that is the only candidate the
// model has an opinion about. But the review page offers every candidate in the
// window, and the person is entitled to merge into any of them — that is the
// whole reason it is a review and not a confirmation.
//
// Filing "this was a match" against the stored row then attaches a positive label
// to a pair the person has just declined. The m probabilities are estimated from
// exactly these labels, so the level that loses reviews would come to look like
// the level that wins them, and the model would learn the opposite of what it was
// told.
func TestReviewQueue_theAnswerIsFiledAgainstThePairThePersonChose(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(8))
	h.reloadState(t)

	// Two authorisations in the window, agreeing with the booking to different
	// degrees, so that "the best candidate" and "the one a person picks" can come
	// apart.
	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("auth-1", daysAgo(3), "9.99", "Spotify"),
		pendingTxnPayee("auth-2", daysAgo(5), "9.49", "Spotify Family"),
	}})
	h.syncer.run()

	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("book-1", daysAgo(3), "9.99", "Netflix"),
	}})
	h.syncer.run()

	items, err := h.syncer.HeldTransactions(context.Background())
	if err != nil {
		t.Fatalf("HeldTransactions: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("setup: %d held transactions, want 1", len(items))
	}
	if len(items[0].Candidates) < 2 {
		t.Fatalf("setup: %d candidates offered, want at least 2 so that the best one and "+
			"the chosen one can differ", len(items[0].Candidates))
	}

	// Deliberately not the first. The page lists them best first.
	best, runnerUp := items[0].Candidates[0], items[0].Candidates[1]
	if runnerUp.ID == best.ID {
		t.Fatal("setup: the two candidates are the same row")
	}

	before, err := h.st.GetMatchDecisions(50)
	if err != nil {
		t.Fatalf("GetMatchDecisions: %v", err)
	}
	var logged store.MatchDecision
	for _, d := range before {
		if d.Outcome == "held" {
			logged = d
		}
	}
	if logged.PayeeLevel == "" {
		t.Fatalf("setup: no held decision was logged; got %+v", before)
	}

	if err := h.syncer.ResolveHeld(context.Background(), items[0].ID,
		runnerUp.ID, runnerUp.Percent, h.syncer.matchPolicy("").Version()); err != nil {
		t.Fatalf("ResolveHeld: %v", err)
	}

	after, err := h.st.GetMatchDecisions(50)
	if err != nil {
		t.Fatalf("GetMatchDecisions: %v", err)
	}
	var answered store.MatchDecision
	for _, d := range after {
		if d.ID == logged.ID {
			answered = d
		}
	}
	if answered.ID == 0 {
		t.Fatalf("the decision that was logged is gone; got %+v", after)
	}
	if answered.Truth == nil || !*answered.Truth {
		t.Fatalf("merging did not record a match: truth = %v", answered.Truth)
	}
	if answered.CandidateID != runnerUp.ID {
		t.Errorf("the answer is filed against candidate %q; the person merged into %q",
			answered.CandidateID, runnerUp.ID)
	}

	t.Logf("model asked about %s/%s/%s (candidate %q); person merged into %q, filed as %s/%s/%s",
		logged.PayeeLevel, logged.AmountLevel, logged.DateLevel, logged.CandidateID,
		runnerUp.ID, answered.PayeeLevel, answered.AmountLevel, answered.DateLevel)

	// The levels have to describe the pair the person settled on. The two
	// candidates here differ on payee and on date, so an unchanged triple means
	// the answer was filed against the row that was declined.
	if answered.PayeeLevel == logged.PayeeLevel &&
		answered.AmountLevel == logged.AmountLevel &&
		answered.DateLevel == logged.DateLevel {
		t.Errorf("the levels still describe the model's best candidate (%s/%s/%s) "+
			"although the person merged into a different row",
			answered.PayeeLevel, answered.AmountLevel, answered.DateLevel)
	}
}

func TestReviewQueue_callingItNewImportsItOnItsOwn(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		held := holdOne(t, h)

		if err := h.syncer.ResolveHeld(context.Background(), held.ID, "", 0, h.syncer.matchPolicy("").Version()); err != nil {
			t.Fatalf("ResolveHeld: %v", err)
		}

		txns := h.actualTxns(t)
		if len(txns) != 2 {
			t.Fatalf("got %d transactions, want 2: the authorisation and the new booking", len(txns))
		}
		if n, _ := h.st.CountMatchReviews(); n != 0 {
			t.Errorf("%d decisions left in the queue", n)
		}

		h.eb.setPages([][]map[string]any{{
			bookedTxnPayee("book-1", daysAgo(3), "9.99", "Netflix"),
		}})
		h.syncer.run()
		if got := len(h.actualTxns(t)); got != 2 {
			t.Errorf("got %d transactions after a further sync, want 2 — the decision "+
				"did not stop it being imported again", got)
		}
	})
}

// TestReviewQueue_refusesADecisionMadeOnAStalePage is the guard the opening
// balance already has for its expected figure, and it matters more here: the
// budget is editable by the user in another tab, and a probability shown before
// an edit can describe a row that no longer looks like that.
func TestReviewQueue_refusesADecisionMadeOnAStalePage(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		holdOne(t, h)

		items, err := h.syncer.HeldTransactions(context.Background())
		if err != nil {
			t.Fatalf("HeldTransactions: %v", err)
		}
		c := items[0].Candidates[0]

		err = h.syncer.ResolveHeld(context.Background(), items[0].ID, c.ID, c.Percent+7, h.syncer.matchPolicy("").Version())
		if err == nil {
			t.Fatal("a decision made against a figure the server never produced was applied")
		}
		if !strings.Contains(err.Error(), "%") {
			t.Errorf("the refusal %q does not say what the figure actually is", err)
		}
		if n, _ := h.st.CountMatchReviews(); n != 1 {
			t.Errorf("%d decisions in the queue; a refused decision must leave it alone", n)
		}
		if got := len(h.actualTxns(t)); got != 1 {
			t.Errorf("got %d transactions; a refused decision must write nothing", got)
		}
	})
}

// TestReviewQueue_refusesACandidateThatIsGone covers the other half of recomputing
// candidates rather than storing them. A stored candidate ID could name a row
// somebody deleted days ago, and merging into it would fail somewhere far less
// legible than here.
func TestReviewQueue_refusesACandidateThatIsGone(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		holdOne(t, h)

		items, err := h.syncer.HeldTransactions(context.Background())
		if err != nil {
			t.Fatalf("HeldTransactions: %v", err)
		}
		err = h.syncer.ResolveHeld(context.Background(), items[0].ID, "no-such-transaction", 71, h.syncer.matchPolicy("").Version())
		if err == nil {
			t.Fatal("a decision naming a transaction that is not there was accepted")
		}
		if n, _ := h.st.CountMatchReviews(); n != 1 {
			t.Errorf("%d decisions in the queue, want the refused one still there", n)
		}
	})
}

// TestReviewQueue_holdsBackWhileASyncIsRunning keeps two writers off the same
// backend. The syncer's run lock is what serialises them, and the queue reaches
// the backend from an HTTP handler rather than the sync loop.
func TestReviewQueue_holdsBackWhileASyncIsRunning(t *testing.T) {
	h := newHarness(t)
	holdOne(t, h)

	if !h.syncer.tryAcquire() {
		t.Fatal("setup: expected to acquire the run lock")
	}
	defer h.syncer.release()

	if _, err := h.syncer.HeldTransactions(context.Background()); err == nil {
		t.Error("the queue read the backend while a sync held the lock")
	}
	if err := h.syncer.ResolveHeld(context.Background(), 1, "", 0, h.syncer.matchPolicy("").Version()); err == nil {
		t.Error("a decision was applied while a sync held the lock")
	}
}

// TestReviewQueue_listsWhatItCannotOfferCandidatesFor is the guarantee the queue
// rests on, checked against the real listing rather than a fake.
//
// A held transaction is in neither the bank feed's future nor the budget. If the
// page silently dropped the ones it could not work out candidates for, the money
// would be nowhere at all and nothing would say so. Here the cause is a backend
// switch, where the stored candidate references are meaningless by construction.
func TestReviewQueue_listsWhatItCannotOfferCandidatesFor(t *testing.T) {
	h := newHarness(t)
	held := holdOne(t, h)

	if err := h.st.DeleteMatchReview(held.ID); err != nil {
		t.Fatalf("DeleteMatchReview: %v", err)
	}
	held.ID = 0
	held.Backend = "some-other-backend"
	if err := h.st.AddMatchReview(held); err != nil {
		t.Fatalf("AddMatchReview: %v", err)
	}

	items, err := h.syncer.HeldTransactions(context.Background())
	if err != nil {
		t.Fatalf("HeldTransactions: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want the held transaction listed anyway", len(items))
	}
	if items[0].Payee != "Netflix" {
		t.Errorf("payee %q, want Netflix", items[0].Payee)
	}
	if items[0].Unavailable == "" {
		t.Error("no reason given, so the row reads as having no candidates rather than " +
			"as one whose candidates cannot be worked out")
	}
	if len(items[0].Candidates) != 0 {
		t.Error("candidates were offered from a backend whose references do not carry across")
	}
}

// TestRemoveBankAccount_takesItsHeldTransactionsWithIt keeps the review queue
// from outliving what it points at.
//
// A held transaction names a budget account that is no longer being synced:
// nothing can be merged into it and nothing can be imported to it, so a row left
// behind is permanently undecidable — and it would keep the queue non-empty for
// good, which every counter and health check downstream reads as work outstanding.
func TestRemoveBankAccount_takesItsHeldTransactionsWithIt(t *testing.T) {
	h := newHarness(t)
	held := holdOne(t, h)

	accounts, err := h.st.GetAllBankAccounts()
	if err != nil {
		t.Fatalf("GetAllBankAccounts: %v", err)
	}
	if err := h.st.RemoveBankAccount(held.BankAccountID); err != nil {
		t.Fatalf("RemoveBankAccount: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("setup: %d accounts, want 1", len(accounts))
	}

	if n, _ := h.st.CountMatchReviews(); n != 0 {
		t.Errorf("%d held transactions survived the removal of their account, and none "+
			"of them can ever be decided", n)
	}
}

// TestSync_reviewGaugeIsActuallyObserved repeats the check that caught the drift
// gauge emitting nothing: an observable gauge with no registered callback exists
// in the code, in the README, and in no metrics backend anywhere.
func TestSync_reviewGaugeIsActuallyObserved(t *testing.T) {
	h := newHarness(t)
	h.syncer.backendName = "firefly"
	held := holdOne(t, h)

	var observed []int64
	var banks, backends []string
	if err := h.syncer.observeOpenReviews(recordingObserver{
		fn: func(v int64, opts []metric.ObserveOption) {
			observed = append(observed, v)
			set := metric.NewObserveConfig(opts).Attributes()
			for _, kv := range set.ToSlice() {
				switch kv.Key {
				case "bank":
					banks = append(banks, kv.Value.AsString())
				case "backend":
					backends = append(backends, kv.Value.AsString())
				}
			}
		},
	}); err != nil {
		t.Fatalf("observeOpenReviews: %v", err)
	}

	if len(observed) != 1 || observed[0] != 1 {
		t.Fatalf("observed %v, want a single 1", observed)
	}
	if len(banks) != 1 || banks[0] == "" {
		t.Errorf("bank label: got %v; without it the gauge cannot say which account is stuck", banks)
	}
	if len(backends) != 1 || backends[0] != "firefly" {
		t.Errorf("backend label: got %v, want [firefly]", backends)
	}
	_ = held
}

// TestSync_reviewGaugeReportsZeroForQuietAccounts is the opposite of the rule
// the drift gauge follows, and deliberately so.
//
// Drift skips accounts it has never compared, because zero there is
// indistinguishable from "agrees to the cent". Here zero is unambiguous —
// nothing is waiting — and reporting it is what lets an alert be written
// against the series instead of against its absence.
func TestSync_reviewGaugeReportsZeroForQuietAccounts(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	h.reloadState(t)

	var observed []int64
	if err := h.syncer.observeOpenReviews(recordingObserver{
		fn: func(v int64, _ []metric.ObserveOption) { observed = append(observed, v) },
	}); err != nil {
		t.Fatalf("observeOpenReviews: %v", err)
	}
	if len(observed) != 1 || observed[0] != 0 {
		t.Errorf("observed %v, want a single 0 — an absent series cannot be alerted on", observed)
	}
}

// TestSync_holdingDefersTheOpeningBalance is the irreversible case, and it has
// to be the run that holds: an opening balance already written is safe, because
// the held transaction came after it.
//
// The balance is written once and never revised. Written on a run that held
// something, it absorbs the held amount — and it stays absorbed after the
// decision is made, silently and for good. Deferring costs one sync.
func TestSync_holdingDefersTheOpeningBalance(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		id := autoBalanceAccount(t, h, "EUR")
		h.eb.setBalances([]map[string]any{{
			"balance_type":   "CLBD",
			"balance_amount": map[string]any{"amount": "100.00", "currency": "EUR"},
		}})
		// A row the user entered themselves, close enough to the incoming booking
		// to be worth asking about. It puts the doubt and the first balance-bearing
		// sync in the same run, which is the only combination that can go wrong.
		h.seed.foreignTransaction(t, h.acct("Checking"), time.Now().UTC().AddDate(0, 0, -3), "Spotify", "", -999)
		_ = h.st.SetLastSyncDate(daysAgo(6))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{
			bookedTxnPayee("book-1", daysAgo(3), "9.99", "Netflix"),
		}})
		h.syncer.run()

		if n, _ := h.st.CountMatchReviews(); n != 1 {
			t.Fatalf("setup: %d held transactions, want 1", n)
		}
		if got := accountRow(t, h, id).OpeningBalanceState; got == store.OpeningBalanceWritten {
			t.Error("an opening balance was written on a run that held a transaction back; " +
				"the held amount is now inside a figure that is never revised")
		}
	})
}

// TestSync_holdingSuppressesTheDriftAlarm follows the rule the incomplete-run
// guard already states. The budget is short by the held amount deliberately and
// for as long as the decision stays open, so a drift figure computed now
// measures the queue, not the account — and would email about it every sync.
func TestSync_holdingSuppressesTheDriftAlarm(t *testing.T) {
	h := newHarness(t)
	id := autoBalanceAccount(t, h, "EUR")
	h.eb.setBalances([]map[string]any{{
		"balance_type":   "CLBD",
		"balance_amount": map[string]any{"amount": "100.00", "currency": "EUR"},
	}})
	if err := h.st.SetOpeningBalance(id, 10000, daysAgo(7), "opening-ref"); err != nil {
		t.Fatalf("SetOpeningBalance: %v", err)
	}
	_ = h.st.SetLastSyncDate(daysAgo(6))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("auth-1", daysAgo(3), "9.99", "Spotify"),
	}})
	h.syncer.run()
	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("book-1", daysAgo(3), "9.99", "Netflix"),
	}})
	h.syncer.run()

	if n, _ := h.st.CountMatchReviews(); n != 1 {
		t.Fatalf("setup: %d held transactions, want 1", n)
	}
	if got := accountRow(t, h, id).DriftState; got != store.DriftIncomplete {
		t.Errorf("drift state %q, want %q: with a decision open the budget is short by "+
			"the held amount on purpose, so the figure measures the review queue",
			got, store.DriftIncomplete)
	}
}

// captureEmails swaps the notification sink for the duration of a test.
func captureEmails(t *testing.T) *[]string {
	t.Helper()
	real := sendEmail
	var sent []string
	sendEmail = func(_ context.Context, subject, body string) {
		sent = append(sent, subject+"\n"+body)
	}
	t.Cleanup(func() { sendEmail = real })
	return &sent
}

// TestSync_holdingRaisesOneEmailPerRun covers the last line of defence.
//
// A held transaction is in no budget and in no bank feed a later run will offer
// again, so an email may be the only thing that tells anyone it exists. One per
// run rather than one per transaction: a bank that changes its payee format
// holds a dozen at once, and a dozen emails get filtered — which would silence
// exactly the notification that matters most.
func TestSync_holdingRaisesOneEmailPerRun(t *testing.T) {
	h := newHarness(t)
	sent := captureEmails(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(6))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("auth-1", daysAgo(3), "9.99", "Spotify"),
		pendingTxnPayee("auth-2", daysAgo(3), "14.50", "Deezer"),
	}})
	h.syncer.run()
	*sent = nil

	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("book-1", daysAgo(3), "9.99", "Netflix"),
		bookedTxnPayee("book-2", daysAgo(3), "14.50", "Disney"),
	}})
	h.syncer.run()

	if n, _ := h.st.CountMatchReviews(); n != 2 {
		t.Fatalf("setup: %d held transactions, want 2", n)
	}
	if len(*sent) != 1 {
		t.Fatalf("got %d emails for one run holding two transactions, want 1: %v",
			len(*sent), *sent)
	}
	msg := (*sent)[0]
	if !strings.Contains(msg, "2 transaction(s)") {
		t.Errorf("the email does not say how many are waiting: %q", msg)
	}
	if !strings.Contains(msg, "/review") {
		t.Error("the email does not say where to go, which is the only thing it is for")
	}
	if !strings.Contains(msg, "NOT been imported") {
		t.Error("the email does not say the money is in no budget, which is the part " +
			"that makes it urgent rather than informational")
	}
}

// TestSync_aQuietRunRaisesNothing keeps the notification from becoming noise.
func TestSync_aQuietRunRaisesNothing(t *testing.T) {
	h := newHarness(t)
	sent := captureEmails(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(6))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{bookedTxn("ref-A", daysAgo(3), "-10.00")}})
	h.syncer.run()

	for _, m := range *sent {
		if strings.Contains(m, "need a decision") {
			t.Errorf("a run that held nothing raised a review email: %q", m)
		}
	}
}

// withMetrics gives a harness a real meter backed by a manual reader.
func withMetrics(t *testing.T, h *harness) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	h.syncer.met = newSyncMetrics(provider.Meter("bankingsync"))
	return reader
}

// TestSync_matchProbabilityIsRecordedForEveryOutcome is what makes the two
// thresholds settable.
//
// A person is asked to choose 90 and 50 with no view of where their bank's
// transactions actually fall. Without this histogram the only way to find out
// whether the review band is doing nothing or swallowing half the traffic is to
// change a number and wait a week.
func TestSync_matchProbabilityIsRecordedForEveryOutcome(t *testing.T) {
	h := newHarness(t)
	reader := withMetrics(t, h)
	h.syncer.backendName = "firefly"
	holdOne(t, h)

	// A booking with nothing in common, weighed against the same window. It is
	// created, and the figure it was created on is worth as much as the ones that
	// merged: a threshold is only choosable against the whole distribution.
	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("book-2", daysAgo(2), "412.30", "Stadtwerke"),
	}})
	h.syncer.run()

	sets := collectAttrSets(t, reader, "bankingsync_match_probability")
	if len(sets) == 0 {
		t.Fatal("no observations at all; the histogram exists in the code and nowhere else")
	}

	outcomes := map[string]int{}
	for _, set := range sets {
		outcomes[attrOf(set, "outcome")]++
		if attrOf(set, "bank") == "" {
			t.Error("no bank label; the distribution of one account cannot be told from another's")
		}
		if got := attrOf(set, "backend"); got != "firefly" {
			t.Errorf("backend label = %q, want firefly", got)
		}
	}
	for _, want := range []string{"held", "created"} {
		if outcomes[want] == 0 {
			t.Errorf("no observation for outcome %q; got %v", want, outcomes)
		}
	}
}

// TestSync_matchProbabilityIgnoresAnEmptyWindow keeps the distribution readable.
//
// A transaction with nothing to compare against has no probability, and
// recording zero for it would pile a spike at the bottom of the histogram that
// means "nothing was there" rather than "a very unlikely match" — which is
// exactly the confusion that would make the threshold unreadable off the chart.
func TestSync_matchProbabilityIgnoresAnEmptyWindow(t *testing.T) {
	h := newHarness(t)
	reader := withMetrics(t, h)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(6))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{bookedTxn("only-one", daysAgo(3), "-10.00")}})
	h.syncer.run()

	if sets := collectAttrSets(t, reader, "bankingsync_match_probability"); len(sets) != 0 {
		t.Errorf("got %d observations for a transaction with no candidates at all", len(sets))
	}
}

// TestSync_heldTransactionsAreCountedByBankAndReason closes two gaps at once.
//
// The bank label: every other per-transaction instrument carries one, and "which
// account is filling the queue" is the first question a full queue raises.
//
// The reason: under the shipped configuration a genuinely ambiguous pair is held
// rather than created, so bankingsync_near_miss_total{reason="ambiguous"} no
// longer fires for it. This is where that distinction survives, and it is the one
// that says whether moving a threshold would help — an ambiguous hold means two
// rows fit equally well and no threshold can separate them.
func TestSync_heldTransactionsAreCountedByBankAndReason(t *testing.T) {
	h := newHarness(t)
	reader := withMetrics(t, h)
	h.syncer.backendName = "firefly"
	holdOne(t, h)

	sets := collectAttrSets(t, reader, "bankingsync_match_reviews_total")
	if len(sets) != 1 {
		t.Fatalf("got %d series, want 1", len(sets))
	}
	if got := attrOf(sets[0], "outcome"); got != "queued" {
		t.Errorf("outcome = %q, want queued", got)
	}
	if got := attrOf(sets[0], "bank"); got == "" {
		t.Error("no bank label; the counter and the open-reviews gauge would disagree " +
			"about which dimensions the queue has")
	}
	if got := attrOf(sets[0], "reason"); got != "uncertain" && got != "ambiguous" {
		t.Errorf("reason = %q, want uncertain or ambiguous", got)
	}
}

// TestSync_holdingReachesTheLogPipelineWithItsReason keeps the diagnostic half.
func TestSync_holdingReachesTheLogPipelineWithItsReason(t *testing.T) {
	h := newHarness(t)
	rec := recordLogs(t)
	holdOne(t, h)

	got, ok := rec.find("match.held_for_review")
	if !ok {
		t.Fatalf("no structured record for a held transaction; got %v", rec.bodies())
	}
	if v := recordAttr(got, "reason"); v != "uncertain" && v != "ambiguous" {
		t.Errorf("reason = %q, want uncertain or ambiguous", v)
	}
	if recordAttr(got, "bank") == "" {
		t.Error("no bank attribute")
	}
}

// TestSync_ambiguityIsHeldRatherThanCounted pins the behaviour change that made
// bankingsync_near_miss_total{reason="ambiguous"} stop firing, so the README's
// account of it cannot drift back to the old meaning unnoticed.
//
// Two rows that fit equally well used to produce a duplicate and a counter tick.
// Now the pair is put to a person, which is better — but it means the near-miss
// counter is no longer where that case shows up, and a metric everyone believes
// in and nothing emits is the failure this project has already had once.
func TestSync_ambiguityIsHeldRatherThanCounted(t *testing.T) {
	h := newHarness(t)
	reader := withMetrics(t, h)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(6))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{
		pendingTxn("auth-1", daysAgo(3), "-120.00"),
		pendingTxn("auth-2", daysAgo(3), "-121.00"),
	}})
	h.syncer.run()

	h.eb.setPages([][]map[string]any{{bookedTxn("book-1", daysAgo(3), "-125.00")}})
	h.syncer.run()

	held, _ := h.st.CountMatchReviews()
	nearMisses := collectAttrSets(t, reader, "bankingsync_near_miss_total")

	if held == 0 {
		t.Error("two equally close rows produced no decision to make; the ambiguous " +
			"case now belongs to the review queue")
	}
	for _, set := range nearMisses {
		if attrOf(set, "reason") == "ambiguous" {
			t.Error("the ambiguous case was both held and counted as a near miss; " +
				"one transaction must not appear in two places as two different things")
		}
	}
}

// recordSpans installs an in-memory tracer so a test can assert what a run put
// on its spans, not only what it logged.
func recordSpans(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return exp
}

// spanAttr reads an attribute from the most recent span of that name.
//
// Most recent, not first: a scenario that reaches an interesting state usually
// takes more than one sync to get there, and the earlier runs are the setup. The
// first sync.run of a hold scenario is the one that created the row later held.
func spanAttr(t *testing.T, exp *tracetest.InMemoryExporter, span, key string) (attribute.Value, bool) {
	t.Helper()
	spans := exp.GetSpans()
	for i := len(spans) - 1; i >= 0; i-- {
		if spans[i].Name != span {
			continue
		}
		for _, kv := range spans[i].Attributes {
			if string(kv.Key) == key {
				return kv.Value, true
			}
		}
	}
	return attribute.Value{}, false
}

// TestSync_heldTransactionsAppearOnTheSpans closes the gap between the run log
// and the run trace.
//
// sync.finished has carried held_for_review since the queue existed, but the
// span did not — while tx_dropped and tx_zero_amount are on both. Anybody
// working from traces rather than logs, which is the whole point of having them,
// saw a run that added nothing and could not tell why.
func TestSync_heldTransactionsAppearOnTheSpans(t *testing.T) {
	h := newHarness(t)
	exp := recordSpans(t)
	holdOne(t, h)

	v, ok := spanAttr(t, exp, "sync.run", "tx_held")
	if !ok {
		t.Fatal("the sync.run span does not say anything was held, although the log does")
	}
	if v.AsInt64() != 1 {
		t.Errorf("sync.run tx_held = %d, want 1", v.AsInt64())
	}

	// Per account as well, or a multi-account instance can see that something was
	// held without seeing which bank it came from.
	v, ok = spanAttr(t, exp, "import.transactions_batch", "held")
	if !ok {
		t.Fatal("the per-account import span does not carry a held count")
	}
	if v.AsInt64() != 1 {
		t.Errorf("import.transactions_batch held = %d, want 1", v.AsInt64())
	}
}

// TestSync_nearMissCounterCarriesItsReason guards the label that is the whole
// diagnostic value of that counter.
//
// Without it the metric says only that some transactions were created next to
// something close, which nobody can act on — the reason is what says whether the
// amount rule, the payee levels or the date is the one costing you merges.
func TestSync_nearMissCounterCarriesItsReason(t *testing.T) {
	h := newHarness(t)
	reader := withMetrics(t, h)
	h.syncer.backendName = "firefly"

	h.syncer.nearMiss("TestBank", "payee", nil)

	sets := collectAttrSets(t, reader, "bankingsync_near_miss_total")
	if len(sets) != 1 {
		t.Fatalf("got %d series, want 1", len(sets))
	}
	if got := attrOf(sets[0], "reason"); got != "payee" {
		t.Errorf("reason = %q, want payee", got)
	}
	if got := attrOf(sets[0], "backend"); got != "firefly" {
		t.Errorf("backend = %q, want firefly", got)
	}
}

// TestReview_aFailedBookkeepingWriteIsReported covers the failure that used to
// go to stderr and nowhere else.
//
// The budget has already been written at this point. What fails is the note that
// stops the same bank transaction being offered again, so the program carries on
// as though nothing happened and the damage appears a run later — as a duplicate
// nobody can trace back to a cause.
func TestReview_aFailedBookkeepingWriteIsReported(t *testing.T) {
	h := newHarness(t)
	held := holdOne(t, h)
	rec := recordLogs(t)

	accounts, _ := h.st.GetAllBankAccounts()
	var acct store.BankAccount
	for _, a := range accounts {
		if a.ID == held.BankAccountID {
			acct = a
		}
	}

	// A store that cannot be written to is the shortest honest way to reach the
	// failure branch; what matters is that the branch reports rather than that
	// the database failed for this particular reason.
	_ = h.st.Close()

	held.Cleared = false
	_ = h.syncer.releaseHeld(context.Background(), held, acct, &budget.Transaction{ID: "t-1"}, true, nil, false)

	got, ok := rec.find("state.bookkeeping_failed")
	if !ok {
		t.Fatalf("a bookkeeping write failed after a decision and left no record; got %v", rec.bodies())
	}
	if v := recordAttr(got, "op"); v != "SetPending" {
		t.Errorf("op = %q, want SetPending", v)
	}
	if recordAttr(got, "bank") == "" {
		t.Error("no bank attribute, so the account it happened on is not recoverable from the record")
	}
	if recordAttr(got, "error") == "" {
		t.Error("no cause recorded")
	}
}

// TestBookkeepingFailed_saysWhetherTheNextRunCanRecover carries the one fact
// that decides how urgent this is.
//
// With a bank reference the next sync recognises the transaction on the
// external-reference fast path and nothing comes of it. Without one there is no
// such path, and the transaction is imported a second time.
func TestBookkeepingFailed_saysWhetherTheNextRunCanRecover(t *testing.T) {
	for name, tc := range map[string]struct {
		ref  string
		want string
	}{
		"with a bank reference":    {"book-1", "true"},
		"without a bank reference": {"", "false"},
	} {
		rec := recordLogs(t)
		bookkeepingFailed(context.Background(), "SetPending", "TestBank", tc.ref, errors.New("database is closed"))

		got, ok := rec.find("state.bookkeeping_failed")
		if !ok {
			t.Fatalf("%s: no record", name)
		}
		var recoverable string
		got.WalkAttributes(func(kv otellog.KeyValue) bool {
			if string(kv.Key) == "recoverable" {
				recoverable = kv.Value.String()
				return false
			}
			return true
		})
		if recoverable != tc.want {
			t.Errorf("%s: recoverable = %q, want %q", name, recoverable, tc.want)
		}
	}
}

// TestRepo_bookkeepingFailuresDoNotGoToStderrOnly keeps the fix from being
// undone by the next person adding a call site.
//
// Every one of these writes has the same shape — the budget changed, the record
// of it did not — and every one of them was originally a bare log.Printf. A new
// one written in the old style would be invisible again, and invisible is the
// whole problem: the failure has no effect until the next run.
func TestRepo_bookkeepingFailuresDoNotGoToStderrOnly(t *testing.T) {
	stderrOnly := regexp.MustCompile(`log\.Printf\("(SetPending|DeletePending|AddImportedRef|AddHeldKey)`)

	for _, name := range []string{"main.go", "review.go", "balance.go", "state.go"} {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range stderrOnly.FindAllString(string(body), -1) {
			t.Errorf("%s: %q writes a bookkeeping failure to stderr only; use "+
				"bookkeepingFailed so it reaches the log pipeline", name, m)
		}
	}
}

// TestSync_twoIdenticalReferencelessPendingRowsBothImport locates the layer the
// reported multiplicity loss actually lives in.
//
// Two purchases of the same amount on the same day, at a bank that supplies no
// entry reference for pending rows — which is the ordinary case, and the reason
// the import key has a fallback at all. Both are real purchases and both belong
// in the budget. If only one arrives, no matching model can be at fault: the
// second never reached the matcher.
func TestSync_twoIdenticalReferencelessPendingRowsBothImport(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(6))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{
			pendingTxnPayee("", daysAgo(3), "0.99", "App Store"),
			pendingTxnPayee("", daysAgo(3), "0.99", "App Store"),
		}})
		h.syncer.run()

		if got := len(h.actualTxns(t)); got != 2 {
			t.Errorf("got %d transactions, want 2: two separate purchases collapsed into "+
				"one before the matcher ever saw them", got)
		}
	})
}

// TestSync_referencelessRowsOfDifferentMerchantsDoNotCollide is the sharper half
// of the same defect, and the one that cannot be argued away as a duplicate.
//
// Two different shops, same day, same amount, no reference. Nothing about these
// two rows is the same except a date and a figure, yet the import key is built
// from exactly those two things.
func TestSync_referencelessRowsOfDifferentMerchantsDoNotCollide(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(6))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{
			pendingTxnPayee("", daysAgo(3), "0.99", "App Store"),
			pendingTxnPayee("", daysAgo(3), "0.99", "Kiosk Mueller"),
		}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 2 {
			t.Fatalf("got %d transactions, want 2: two different merchants shared an "+
				"import key built from date and amount alone", len(txns))
		}
		names := map[string]bool{}
		for _, x := range txns {
			names[x.PayeeName] = true
		}
		for _, want := range []string{"App Store", "Kiosk Mueller"} {
			if !names[want] {
				t.Errorf("%q is missing; got %v", want, names)
			}
		}
	})
}

// TestSync_aHeldRowDoesNotSwallowAnUnrelatedOne is the sharpest form of the
// import-key defect, because it loses a transaction that has nothing to do with
// the one under review.
//
// Before v3 the held-key check compared an identity built from date and amount
// alone. Once one row was held, every other row of the same size that day was
// dropped by that check — no counter, no log, no trace of it anywhere.
func TestSync_aHeldRowDoesNotSwallowAnUnrelatedOne(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(6))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{
			pendingTxnPayee("", daysAgo(3), "9.99", "Spotify"),
		}})
		h.syncer.run()

		h.eb.setPages([][]map[string]any{{
			bookedTxnPayee("", daysAgo(3), "9.99", "Netflix"),
		}})
		h.syncer.run()
		if n, _ := h.st.CountMatchReviews(); n != 1 {
			t.Fatalf("setup: %d held transactions, want 1", n)
		}

		// A different shop, same day, same amount, still no reference.
		h.eb.setPages([][]map[string]any{{
			bookedTxnPayee("", daysAgo(3), "9.99", "Netflix"),
			bookedTxnPayee("", daysAgo(3), "9.99", "Kiosk Mueller"),
		}})
		h.syncer.run()

		// It need not be imported — with an authorisation of the same size still
		// open, holding it is a defensible answer. What it must not be is gone.
		payees := presentPayees(t, h)
		if payees["Kiosk Mueller"] == 0 {
			t.Errorf("the unrelated purchase is in neither the budget nor the review "+
				"queue: it was swallowed by an open review of another transaction that "+
				"merely cost the same. Present: %v", payees)
		}
	})
}

// TestSync_theFeedOrderDoesNotChangeTheOutcome is the property the sort exists
// for. A result that depends on which page the bank returned a row on is a
// result nobody can reproduce, and it is what makes the assignment work in a
// later phase verifiable at all.
func TestSync_theFeedOrderDoesNotChangeTheOutcome(t *testing.T) {
	feed := []map[string]any{
		pendingTxnPayee("", daysAgo(3), "9.99", "Spotify"),
		bookedTxnPayee("", daysAgo(3), "9.99", "Spotify"),
		bookedTxnPayee("", daysAgo(3), "0.99", "App Store"),
		bookedTxnPayee("", daysAgo(3), "0.99", "App Store"),
		bookedTxnPayee("", daysAgo(2), "24.00", "Kiosk Mueller"),
	}
	reversed := make([]map[string]any, len(feed))
	for i, x := range feed {
		reversed[len(feed)-1-i] = x
	}

	shape := func(order []map[string]any) []string {
		var out []string
		h := newHarness(t)
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(6))
		h.reloadState(t)
		h.eb.setPages([][]map[string]any{order})
		h.syncer.run()
		for _, x := range h.actualTxns(t) {
			out = append(out, fmt.Sprintf("%s|%d|%v", x.PayeeName, x.AmountCents, x.Cleared))
		}
		sort.Strings(out)
		return out
	}

	forward, backward := shape(feed), shape(reversed)
	if len(forward) != len(backward) {
		t.Fatalf("got %d transactions forwards and %d backwards: %v vs %v",
			len(forward), len(backward), forward, backward)
	}
	for i := range forward {
		if forward[i] != backward[i] {
			t.Errorf("the same feed in a different order produced different budgets:\n  %v\n  %v",
				forward, backward)
			break
		}
	}
}

// TestSync_theSameFeedTwiceAddsNothing pins the assumption the occurrence index
// rests on: the bank returns a day's rows in a stable order. If it does not, the
// index shifts and the same purchase is imported under two identities.
//
// The failure direction is deliberate — a visible duplicate rather than a silent
// loss — but it still has to be tested, because it is the one thing this scheme
// trades away.
func TestSync_theSameFeedTwiceAddsNothing(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(6))
		h.reloadState(t)

		feed := [][]map[string]any{{
			pendingTxnPayee("", daysAgo(3), "0.99", "App Store"),
			pendingTxnPayee("", daysAgo(3), "0.99", "App Store"),
			pendingTxnPayee("", daysAgo(3), "0.99", "Kiosk Mueller"),
		}}
		h.eb.setPages(feed)
		h.syncer.run()
		first := len(h.actualTxns(t))
		if first != 3 {
			t.Fatalf("first run: got %d transactions, want 3", first)
		}

		h.eb.setPages(feed)
		h.syncer.run()
		if got := len(h.actualTxns(t)); got != first {
			t.Errorf("a second run over the same feed produced %d transactions, want %d",
				got, first)
		}
	})
}

// TestSync_aPendingRowFromBeforeV3IsStillRecognised covers the upgrade.
//
// A pending row imported by an older version is recorded under an identity built
// from date and amount alone. Its booking now computes a different one, and an
// authorisation that goes unrecognised is not merely unmatched — it is imported
// a second time, which turns an upgrade into a duplicate for every open
// authorisation in flight.
func TestSync_aPendingRowFromBeforeV3IsStillRecognised(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		id := h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(6))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{
			pendingTxnPayee("", daysAgo(3), "9.99", "Spotify"),
		}})
		h.syncer.run()

		// Rewrite the entry to the pre-v3 scheme, as an upgraded installation
		// would have it.
		pending := h.syncer.state.Pending(id)
		if len(pending) != 1 {
			t.Fatalf("setup: %d pending entries, want 1", len(pending))
		}
		var current, value string
		for k, v := range pending {
			current, value = k, v
		}
		txnID, _ := splitPendingVal(value)
		if err := h.st.DeletePending(id, current); err != nil {
			t.Fatalf("DeletePending: %v", err)
		}
		legacy := legacyImportKey(mustDay(t, daysAgo(3)), -999)
		if err := h.st.SetPending(id, legacy, value); err != nil {
			t.Fatalf("SetPending: %v", err)
		}
		h.reloadState(t)

		// The bank renames the payee on settlement — an aggregator relabelling, a
		// merchant descriptor change. The matcher alone would find this pair
		// uncertain and hold it; the pending map knows better, because it recorded
		// that this very bank row became that very budget row.
		h.eb.setPages([][]map[string]any{{
			bookedTxnPayee("", daysAgo(3), "9.99", "Netflix"),
		}})
		h.syncer.run()

		if n, _ := h.st.CountMatchReviews(); n != 0 {
			t.Errorf("%d transactions went to review: the upgrade lost track of an "+
				"authorisation recorded under the old identity", n)
		}
		txns := h.actualTxns(t)
		if len(txns) != 1 {
			t.Fatalf("got %d transactions, want 1: the upgrade did not recognise an "+
				"authorisation recorded under the old identity", len(txns))
		}
		if !txns[0].Cleared {
			t.Error("the row was not confirmed by its booking")
		}
		if txns[0].ID != txnID {
			t.Errorf("a different row survived: %q, want %q", txns[0].ID, txnID)
		}
	})
}

func mustDay(t *testing.T, s string) time.Time {
	if t != nil {
		t.Helper()
	}
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		if t == nil {
			panic(err)
		}
		t.Fatalf("parse %q: %v", s, err)
	}
	return d
}

func txnFor(status, ref, date, payee string, cents int64) enablebanking.Transaction {
	return enablebanking.Transaction{
		Status: status, EntryRef: ref, Date: mustDay(nil, date),
		AmountCents: cents, Payee: payee,
	}
}

// TestImportKeys_propertiesTheMatcherWouldOtherwiseMask pins the import key
// directly, and it does so because end-to-end it mostly cannot be pinned.
//
// Measured: with the payee, the status or the legacy fallback removed, the whole
// sync suite stays green — the matcher re-derives the right answer from the
// candidates and hides the damage. That is a good property of the matcher and a
// bad reason to leave the key untested, because the day the key is wrong and the
// matcher is uncertain is the day a transaction goes missing. Only the occurrence
// index is load-bearing end to end; the rest is held here.
func TestImportKeys_propertiesTheMatcherWouldOtherwiseMask(t *testing.T) {
	prefixes := []string{"VISA"}
	day := daysAgo(3)

	t.Run("a bank reference is the identity, verbatim", func(t *testing.T) {
		keys, _ := importKeys([]enablebanking.Transaction{
			txnFor("BOOK", "ref-1", day, "App Store", -99),
		}, prefixes)
		if keys[0] != "ref-1" {
			t.Errorf("got %q, want the reference unchanged", keys[0])
		}
	})

	t.Run("different merchants of the same size are different transactions", func(t *testing.T) {
		keys, _ := importKeys([]enablebanking.Transaction{
			txnFor("BOOK", "", day, "App Store", -99),
			txnFor("BOOK", "", day, "Kiosk Mueller", -99),
		}, prefixes)
		if keys[0] == keys[1] {
			t.Errorf("both merchants share the identity %q", keys[0])
		}
	})

	t.Run("two identical purchases are two transactions", func(t *testing.T) {
		keys, _ := importKeys([]enablebanking.Transaction{
			txnFor("BOOK", "", day, "App Store", -99),
			txnFor("BOOK", "", day, "App Store", -99),
		}, prefixes)
		if keys[0] == keys[1] {
			t.Errorf("both purchases share the identity %q", keys[0])
		}
	})

	t.Run("an authorisation and its booking share an identity", func(t *testing.T) {
		// Counted per status, or the booking would take the index after the
		// authorisation and the pending map would stop recognising the pair.
		keys, _ := importKeys([]enablebanking.Transaction{
			txnFor("PDNG", "", day, "Hotel Berlin", -12000),
			txnFor("BOOK", "", day, "VISA Hotel Berlin", -12000),
		}, prefixes)
		if keys[0] != keys[1] {
			t.Errorf("authorisation %q and booking %q do not share an identity, so the "+
				"pending map can never confirm one with the other", keys[0], keys[1])
		}
	})

	t.Run("the card scheme prefix does not create a second identity", func(t *testing.T) {
		keys, _ := importKeys([]enablebanking.Transaction{
			txnFor("BOOK", "", day, "Hotel Berlin", -12000),
			txnFor("BOOK", "", day, "VISA Hotel Berlin", -12000),
		}, prefixes)
		// Same payee once the prefix is discounted, so they are two occurrences
		// of one identity rather than two identities.
		if strings.TrimSuffix(keys[0], "|1") != strings.TrimSuffix(keys[1], "|2") {
			t.Errorf("prefix stripping did not apply: %q vs %q", keys[0], keys[1])
		}
	})

	t.Run("collisions under the old scheme are counted", func(t *testing.T) {
		_, collisions := importKeys([]enablebanking.Transaction{
			txnFor("BOOK", "", day, "App Store", -99),
			txnFor("BOOK", "", day, "Kiosk Mueller", -99),
			txnFor("BOOK", "", day, "Somewhere Else", -2400),
		}, prefixes)
		if collisions != 1 {
			t.Errorf("got %d collisions, want 1: two of these three would have shared "+
				"the pre-v3 identity", collisions)
		}
	})
}

// TestLessTxn_isATotalOrder covers the sort's tie-breaks, whose end-to-end effect
// only becomes visible once the assignment in a later phase replaces the greedy
// pass. Until then the property is the claim, and the property is testable.
func TestLessTxn_isATotalOrder(t *testing.T) {
	day := daysAgo(3)
	rows := []enablebanking.Transaction{
		txnFor("BOOK", "b", day, "Beta", -200),
		txnFor("PDNG", "a", day, "Alpha", -100),
		txnFor("BOOK", "c", daysAgo(1), "Gamma", -300),
		txnFor("BOOK", "a", day, "Alpha", -100),
	}

	// Antisymmetry and irreflexivity, which is what "total" needs to mean here.
	for i := range rows {
		if lessTxn(rows[i], rows[i]) {
			t.Errorf("row %d is less than itself", i)
		}
		for j := range rows {
			if i != j && lessTxn(rows[i], rows[j]) && lessTxn(rows[j], rows[i]) {
				t.Errorf("rows %d and %d are each less than the other", i, j)
			}
		}
	}

	// An authorisation is offered before the booking that settles it, so the
	// pending map is populated by the time the booking looks in it.
	pdng := txnFor("PDNG", "", day, "Alpha", -100)
	book := txnFor("BOOK", "", day, "Alpha", -100)
	if !lessTxn(pdng, book) || lessTxn(book, pdng) {
		t.Error("a booking sorts before the authorisation it settles")
	}
}

// TestSync_identityFollowsTheMerchantNotThePosition is where the payee in the
// import key actually earns its place.
//
// Within one batch the occurrence index already separates two rows, so removing
// the payee looks harmless. Across runs it is not: without the payee the index
// alone decides identity, and identity by position means that a feed which
// returns the same day's rows in a different order hands one purchase the other
// one's name. The first is then skipped as already imported and the second is
// imported again — a loss and a duplicate from the same cause.
func TestSync_identityFollowsTheMerchantNotThePosition(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(6))
		h.reloadState(t)

		a := pendingTxnPayee("", daysAgo(3), "0.99", "App Store")
		b := pendingTxnPayee("", daysAgo(3), "0.99", "Kiosk Mueller")

		h.eb.setPages([][]map[string]any{{a, b}})
		h.syncer.run()
		if got := len(h.actualTxns(t)); got != 2 {
			t.Fatalf("first run: got %d transactions, want 2", got)
		}

		// The same two rows, the other way round.
		h.eb.setPages([][]map[string]any{{b, a}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 2 {
			t.Fatalf("got %d transactions after the order changed, want 2: identity is "+
				"following the position in the feed rather than the merchant", len(txns))
		}
		names := map[string]int{}
		for _, x := range txns {
			names[x.PayeeName]++
		}
		for _, want := range []string{"App Store", "Kiosk Mueller"} {
			if names[want] != 1 {
				t.Errorf("%q appears %d times, want once; got %v", want, names[want], names)
			}
		}
	})
}

// TestSync_identityIsContentNotPosition is the case that decides whether the
// payee belongs in the import key at all.
//
// The occurrence index alone makes identity positional, and position is stable
// only while the set is. Let one purchase drop out of the window and a new one
// appear, and every index after the gap shifts by one: the newcomer inherits the
// identity of a row that is already imported and is skipped as a repeat of it.
// Nothing reports that, because as far as the importer is concerned it has seen
// this transaction before.
//
// With the payee in the key, identity is a property of the transaction rather
// than of its neighbours, and a shifted set shifts nothing.
func TestSync_identityIsContentNotPosition(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(6))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{
			pendingTxnPayee("", daysAgo(3), "0.99", "App Store"),
			pendingTxnPayee("", daysAgo(3), "0.99", "Kiosk Mueller"),
		}})
		h.syncer.run()
		if got := len(h.actualTxns(t)); got != 2 {
			t.Fatalf("first run: got %d transactions, want 2", got)
		}

		// The first purchase is no longer offered; a third one appears in its place.
		h.eb.setPages([][]map[string]any{{
			pendingTxnPayee("", daysAgo(3), "0.99", "Kiosk Mueller"),
			pendingTxnPayee("", daysAgo(3), "0.99", "Zeitschriften Weber"),
		}})
		h.syncer.run()

		// Held or imported is a matching decision and either is defensible with
		// two other authorisations of the same size open. Absent is not.
		names := presentPayees(t, h)
		if names["Zeitschriften Weber"] != 1 {
			t.Errorf("the new purchase was skipped as a repeat of one it merely stood "+
				"next to, and is in neither the budget nor the review queue: %v", names)
		}
		if names["Kiosk Mueller"] != 1 {
			t.Errorf("Kiosk Mueller appears %d times, want once: %v", names["Kiosk Mueller"], names)
		}
	})
}

// presentPayees is every payee the run accounted for, in the budget or in the
// review queue.
//
// The distinction between the two is a matching decision and varies with the
// scenario; what must never vary is that a transaction the bank offered is in
// one of them. A test about losing transactions asks this, not "was it imported".
func presentPayees(t *testing.T, h *harness) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, x := range h.actualTxns(t) {
		out[x.PayeeName]++
	}
	reviews, err := h.st.GetMatchReviews()
	if err != nil {
		t.Fatalf("GetMatchReviews: %v", err)
	}
	for _, r := range reviews {
		out[r.Payee]++
	}
	return out
}

// feedFrom renders a generated history in the shape the Enable Banking mock
// serves, so a synthetic history can be driven through the real importer rather
// than only through the model in isolation.
func feedFrom(rows []linkagegen.Row) [][]map[string]any {
	page := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		cents := r.AmountCents
		if cents < 0 {
			cents = -cents
		}
		page = append(page, map[string]any{
			"entry_reference":        r.EntryRef,
			"transaction_date":       r.Date.Format("2006-01-02"),
			"transaction_amount":     map[string]any{"amount": fmt.Sprintf("%d.%02d", cents/100, cents%100)},
			"credit_debit_indicator": "DBIT",
			"status":                 r.Status,
			"creditor":               map[string]any{"name": r.Payee},
		})
	}
	return [][]map[string]any{page}
}

// TestGeneratedHistory_reachesTheImporter is Phase G's acceptance criterion.
//
// A generator whose output the sync loop cannot consume is a generator that can
// only ever test the model in isolation, and the properties it was built for —
// order independence, multiplicity, assignment behaviour — are properties of the
// loop. This drives one synthetic history end to end and checks that every
// purchase the bank reported is accounted for somewhere.
func TestGeneratedHistory_reachesTheImporter(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		hist := linkagegen.Generate(linkagegen.Config{
			Seed: 42, Days: 10, Purchases: 40,
			Start:      time.Now().UTC().AddDate(0, 0, -12),
			FieldWidth: 16, Prefix: "VISA", References: true,
			SettleMinDays: 1, SettleMaxDays: 3, UnsettledChance: 0.2,
			UpliftChance: 0.15, UpliftMaxPercent: 20,
			BranchChance: 0.15, DuplicateChance: 0.15,
		})

		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(time.Now().UTC().AddDate(0, 0, -14).Format("2006-01-02"))
		h.reloadState(t)

		// Two runs over the same history, as a bank would serve it: first the
		// statement partway through, then the settled one. Anything else asks the
		// importer to merge two rows it was handed together, which is not a thing
		// a bank does.
		mid := time.Now().UTC().AddDate(0, 0, -6)
		h.eb.setPages(feedFrom(hist.FeedAsOf(mid)))
		h.syncer.run()
		h.eb.setPages(feedFrom(hist.FeedAsOf(time.Now().UTC().AddDate(0, 0, 30))))
		h.syncer.run()

		// Every purchase reached the budget or the queue. Which of the two is a
		// matching decision; being in neither is a loss.
		accounted := len(h.actualTxns(t))
		queued, err := h.st.CountMatchReviews()
		if err != nil {
			t.Fatalf("CountMatchReviews: %v", err)
		}
		if accounted+queued < len(hist.Purchases) {
			t.Errorf("%d purchases produced %d budget rows and %d held: %d are in neither",
				len(hist.Purchases), accounted, queued,
				len(hist.Purchases)-accounted-queued)
		}
		// And nothing was invented: a settled purchase is one row, not two.
		if accounted > len(hist.Purchases) {
			t.Errorf("%d purchases produced %d budget rows", len(hist.Purchases), accounted)
		}
	})
}

// TestPayeeFrequency_standsDownOnASmallSample is the guarantee for an account
// with little history. A frequency drawn from a handful of rows is a
// coincidence, not a distribution, and a correction built on one would penalise
// every payee equally and for no reason.
func TestPayeeFrequency_standsDownOnASmallSample(t *testing.T) {
	var few []enablebanking.Transaction
	for i := 0; i < minFrequencySample-1; i++ {
		few = append(few, txnFor("BOOK", "", daysAgo(3), "Edeka Sued", -1000))
	}
	if f := payeeFrequency(few, nil); f != nil {
		t.Errorf("a %d row sample produced a distribution", len(few))
	}
}

// TestPayeeFrequency_measuresTheAccountAndFloorsTheTail covers both directions
// of the correction against a history whose skew is known by construction.
func TestPayeeFrequency_measuresTheAccountAndFloorsTheTail(t *testing.T) {
	hist := linkagegen.Generate(linkagegen.Config{Seed: 21, Days: 30, Purchases: 400})

	var txns []enablebanking.Transaction
	for _, r := range hist.Rows {
		txns = append(txns, txnFor(r.Status, r.EntryRef, r.Date.Format("2006-01-02"), r.Payee, r.AmountCents))
	}
	freq := payeeFrequency(txns, nil)
	if freq == nil {
		t.Fatalf("a %d row history produced no distribution", len(txns))
	}

	counts := map[string]int{}
	for _, r := range hist.Rows {
		counts[strings.ToLower(r.Payee)]++
	}
	var top string
	for name, n := range counts {
		if top == "" || n > counts[top] {
			top = name
		}
	}
	if got := freq(top); got < 0.15 {
		t.Errorf("the commonest payee measures %.3f of the account; the generator draws "+
			"it far more often than that", got)
	}

	// A name the statement never mentioned is not evidence either way.
	if got := freq("nowhere gmbh"); got != 0 {
		t.Errorf("an unseen payee measured %.4f, want 0", got)
	}

}

// TestPayeeFrequency_floorsASingleSighting is the guard that keeps the
// correction from becoming a gate.
//
// A name seen once in a long statement has a frequency of 1/N, and log2 of the
// base u over that is worth more bits than every other field in the model put
// together — so a single coincidence would decide a merge on its own.
//
// The floor has to be a constant, and the test is written across two statement
// lengths because it used to be 2/N and that is what hid the defect. A floor that
// falls as the statement grows caps nothing: it took exactly one bit off at every
// size, and the payee term still reached +11.1 bits on a long statement against
// +6.544 for the whole rest of the model. Both lengths below must give the same
// answer for the same one-off name.
//
// It needs a history with a genuine one-off in it, which a drawn one does not
// reliably contain: the generator's pool repeats, so every name in it is common.
func TestPayeeFrequency_floorsASingleSighting(t *testing.T) {
	build := func(rows int) func(string) float64 {
		var txns []enablebanking.Transaction
		for i := 0; i < rows-1; i++ {
			txns = append(txns, txnFor("BOOK", "", daysAgo(3), "Edeka Sued", -1000))
		}
		txns = append(txns, txnFor("BOOK", "", daysAgo(3), "Ristorante Da Luigi Roma", -4200))
		freq := payeeFrequency(txns, nil)
		if freq == nil {
			t.Fatalf("a %d row sample produced no distribution", rows)
		}
		return freq
	}

	short, long := build(500), build(8000)

	oneOff := short("ristorante da luigi roma")
	if oneOff < minPayeeFrequency*0.999 {
		t.Errorf("a payee seen once measured %.6f, below the floor of %.6f: one sighting "+
			"would outweigh the rest of the model", oneOff, minPayeeFrequency)
	}
	if got := long("ristorante da luigi roma"); math.Abs(got-oneOff) > 1e-12 {
		t.Errorf("the same one-off name measured %.6f on a 500 row statement and %.6f on "+
			"an 8000 row one: the floor still depends on the statement length, so it "+
			"loosens as the account accumulates history", oneOff, got)
	}

	// What the floor is worth in the units that matter. The cap has to stay under
	// what the rest of the model can muster, or the correction decides alone.
	cap := math.Log2(budget.DefaultLinkage().PayeeU[budget.PayeeExact] / minPayeeFrequency)
	rest := math.Log2(0.70/0.02) + math.Log2(0.40/0.15) // amount exact + date same
	t.Logf("the correction caps at %+.3f bits; amount and date at their best are %+.3f", cap, rest)
	if cap >= rest {
		t.Errorf("the term-frequency correction caps at %+.3f bits and the rest of the "+
			"model musters %+.3f: a single rare name outweighs every other field", cap, rest)
	}

	// And the floor is a floor, not a replacement: the common name keeps its
	// real share.
	if common := short("edeka sued"); common < 0.9 {
		t.Errorf("the commonest payee measured %.3f, want its real share near 0.998", common)
	}
}

// TestMeasureFieldWidth_readsTheBankNotTheNames separates an institution that
// cuts merchant names at a fixed width from one whose longest name simply
// happens to be its longest.
func TestMeasureFieldWidth_readsTheBankNotTheNames(t *testing.T) {
	build := func(cfg linkagegen.Config) []enablebanking.Transaction {
		var out []enablebanking.Transaction
		for _, r := range linkagegen.Generate(cfg).Rows {
			out = append(out, txnFor(r.Status, r.EntryRef, r.Date.Format("2006-01-02"), r.Payee, r.AmountCents))
		}
		return out
	}

	// Names long enough for a 25 character field to bite. Real merchant strings
	// carry a town and often a country, which is why the field runs out.
	long := []string{
		"Zeitschriften Weber Nuernberg", "Elektromarkt Nord Hamburg",
		"Restaurant Da Luigi Roma Italia", "Tankstelle Am Stadtpark Koeln",
		"Buchhandlung Schmidt Muenchen", "Aldi",
	}
	cutting := linkagegen.Config{Seed: 31, Purchases: 300, FieldWidth: 25, Prefix: "VISA", Merchants: long}
	width, truncating := measureFieldWidth(build(cutting))
	if !truncating {
		t.Errorf("an institution cutting at 25 characters was not recognised as cutting "+
			"(longest name %d)", width)
	}
	if width != 25 {
		t.Errorf("measured width %d, want 25", width)
	}

	// The same bank without a field limit. Its longest name is a name, not a
	// boundary, and calling that truncation would make every institution look
	// like it cuts.
	whole := linkagegen.Config{Seed: 31, Purchases: 300, Merchants: long}
	if _, truncating := measureFieldWidth(build(whole)); truncating {
		t.Error("an institution that sends names whole was read as truncating")
	}
}

// TestSync_theBatchOrderDoesNotChangeTheBudget is the property the whole
// arrangement exists for, driven end to end.
//
// Before, two bookings competing for one authorisation were settled by whichever
// the loop reached first, so the same statement served in a different order
// produced a different budget. It is a property test rather than an example
// because the failure it guards against is a class, not a case.
func TestSync_theBatchOrderDoesNotChangeTheBudget(t *testing.T) {
	feed := []map[string]any{
		pendingTxnPayee("auth-1", daysAgo(5), "120.00", "Hotel Berlin"),
		bookedTxnPayee("book-1", daysAgo(3), "125.00", "VISA Hotel Berlin"),
		bookedTxnPayee("book-2", daysAgo(3), "127.00", "VISA Hotel Berlin"),
		bookedTxnPayee("book-3", daysAgo(2), "42.00", "Kiosk Mueller"),
		pendingTxnPayee("auth-2", daysAgo(4), "42.00", "Kiosk Mueller"),
	}

	shape := func(order []map[string]any) []string {
		h := newHarness(t)
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(9))
		h.reloadState(t)
		h.eb.setPages([][]map[string]any{order})
		h.syncer.run()

		var out []string
		for _, x := range h.actualTxns(t) {
			out = append(out, fmt.Sprintf("%s|%d|%v", x.PayeeName, x.AmountCents, x.Cleared))
		}
		reviews, err := h.st.GetMatchReviews()
		if err != nil {
			t.Fatalf("GetMatchReviews: %v", err)
		}
		for _, r := range reviews {
			out = append(out, fmt.Sprintf("HELD|%s|%d", r.Payee, r.AmountCents))
		}
		sort.Strings(out)
		return out
	}

	want := shape(feed)
	r := mrand.New(mrand.NewPCG(5, 9))
	for trial := 0; trial < 12; trial++ {
		shuffled := append([]map[string]any(nil), feed...)
		r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

		if got := shape(shuffled); !slicesEqual(got, want) {
			t.Fatalf("trial %d: the same statement in a different order produced a "+
				"different budget:\n  want %v\n  got  %v", trial, want, got)
		}
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSync_twoIdenticalSettlementsBothMerge is the reported multiplicity case,
// end to end and over both backends.
//
// Two purchases of the same amount at the same shop on the same day, each
// authorised and each settling. Judged pair by pair nothing can choose between
// them, so both went to a person; under the constraint there is exactly one
// arrangement that uses both authorisations, and the two rows that leave the
// same budget whichever way round are not a decision anybody needs to make.
func TestSync_twoIdenticalSettlementsBothMerge(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(9))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{
			pendingTxnPayee("auth-1", daysAgo(4), "0.99", "App Store"),
			pendingTxnPayee("auth-2", daysAgo(4), "0.99", "App Store"),
		}})
		h.syncer.run()
		if got := len(h.actualTxns(t)); got != 2 {
			t.Fatalf("setup: %d authorisations, want 2", got)
		}

		h.eb.setPages([][]map[string]any{{
			bookedTxnPayee("book-1", daysAgo(3), "0.99", "VISA App Store"),
			bookedTxnPayee("book-2", daysAgo(3), "0.99", "VISA App Store"),
		}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 2 {
			t.Errorf("got %d transactions, want 2: two settlements of two authorisations "+
				"must not become more or fewer rows", len(txns))
		}
		cleared := 0
		for _, x := range txns {
			if x.Cleared {
				cleared++
			}
		}
		if cleared != 2 {
			t.Errorf("%d of %d rows were confirmed, want both", cleared, len(txns))
		}
		if n, _ := h.st.CountMatchReviews(); n != 0 {
			t.Errorf("%d decisions were put to a person for a pair that leaves the same "+
				"budget either way round", n)
		}
	})
}

// TestSync_aChoiceBetweenDistinguishableRowsIsStillAsked is the boundary of the
// refinement above, and the reason it is drawn where it is.
//
// Two authorisations for the same amount at the same shop, but on different
// days. One settlement arrives. Settling onto one leaves a Monday authorisation
// open, onto the other a Tuesday one — two different budgets, so the choice is
// real and belongs to a person however alike the two rows look.
func TestSync_aChoiceBetweenDistinguishableRowsIsStillAsked(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("auth-1", daysAgo(4), "0.99", "App Store"),
		pendingTxnPayee("auth-2", daysAgo(3), "0.99", "App Store"),
	}})
	h.syncer.run()
	if got := len(h.actualTxns(t)); got != 2 {
		t.Fatalf("setup: %d authorisations, want 2", got)
	}

	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("book-1", daysAgo(3), "0.99", "VISA App Store"),
	}})
	h.syncer.run()

	n, _ := h.st.CountMatchReviews()
	if n != 1 {
		t.Errorf("%d decisions outstanding, want 1: the two authorisations are a day "+
			"apart, so which one settled is a question about the budget", n)
	}
}

// TestSync_theResumePointNeverPassesUnplacedWork covers the one way the two-pass
// import can lose a transaction.
//
// Transactions that need the model are set aside and placed at the end of the
// run. Anything handled inline after one has been set aside is finished earlier
// than something that came before it, so advancing the resume point on the
// inline one would carry it past work that has not happened. The next run then
// fetches from after the gap, and the transaction in it is never offered again.
func TestSync_theResumePointNeverPassesUnplacedWork(t *testing.T) {
	h := newHarness(t)
	id := h.addAccount(t, daysAgo(20))
	h.reloadState(t)

	// An authorisation, so that its settlement takes the inline path and writes.
	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("", daysAgo(3), "20.00", "Kiosk Mueller"),
	}})
	h.syncer.run()

	ctx, cancel := context.WithCancel(context.Background())
	h.syncer.ctx = ctx
	h.fake.afterWrite = func(int) { cancel() }

	// The older booking needs the model and is set aside; the newer one settles
	// the authorisation inline and writes, which ends the run.
	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("", daysAgo(5), "10.00", "Etwas Anderes"),
		bookedTxnPayee("", daysAgo(3), "20.00", "Kiosk Mueller"),
	}})
	h.syncer.run()

	accounts, err := h.st.GetAllBankAccounts()
	if err != nil {
		t.Fatalf("GetAllBankAccounts: %v", err)
	}
	var acct store.BankAccount
	for _, a := range accounts {
		if a.ID == id {
			acct = a
		}
	}
	if acct.StartSyncDate > daysAgo(5) {
		t.Errorf("resume point %q is past the transaction of %s, which was set aside and "+
			"never placed: the next run starts after it and it is offered no more",
			acct.StartSyncDate, daysAgo(5))
	}
}

// TestSync_everyDecisionIsRecordedNotJustTheDoubtfulOnes is what the log is for.
//
// The band a person sees is the smallest part of the traffic and the least
// interesting: the automatic band is where the expensive mistakes happen and
// where nobody is looking. A model later estimated on the doubtful cases alone
// and applied to all of them is biased by exactly the cases it never saw.
func TestSync_everyDecisionIsRecordedNotJustTheDoubtfulOnes(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("auth-1", daysAgo(5), "120.00", "Hotel Berlin"),
		bookedTxnPayee("book-9", daysAgo(4), "42.00", "Kiosk Mueller"),
	}})
	h.syncer.run()
	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("book-1", daysAgo(3), "125.00", "VISA Hotel Berlin"),
	}})
	h.syncer.run()

	decisions, err := h.st.GetMatchDecisions(50)
	if err != nil {
		t.Fatalf("GetMatchDecisions: %v", err)
	}
	byOutcome := map[string]int{}
	runs := map[string]bool{}
	for _, d := range decisions {
		byOutcome[d.Outcome]++
		runs[d.RunID] = true
		if d.ParamVersion == "" {
			t.Error("a decision was recorded without the parameters that made it; a " +
				"reading spanning a settings change would mix two models silently")
		}
	}
	for _, want := range []string{"created", "adopted"} {
		if byOutcome[want] == 0 {
			t.Errorf("no %q decision was recorded; got %v", want, byOutcome)
		}
	}
	if len(runs) != 2 {
		t.Errorf("%d run identities across two runs, want 2 — without one, a reading "+
			"cannot tell one pass over an account from the next", len(runs))
	}

	// The levels are the answer to "why did it not match", without anybody
	// having to send payees and amounts around.
	var withLevels int
	for _, d := range decisions {
		if d.PayeeLevel != "" && d.AmountLevel != "" && d.DateLevel != "" {
			withLevels++
		}
	}
	if withLevels == 0 {
		t.Error("no decision recorded the levels it was made on")
	}
}

// TestSync_aResolvedDecisionRecordsWhatItTurnedOutToBe closes the loop. A
// person's answer is the one observation available that does not come from the
// model, and it is the only reason the log is worth more than a diary.
func TestSync_aResolvedDecisionRecordsWhatItTurnedOutToBe(t *testing.T) {
	h := newHarness(t)
	held := holdOne(t, h)

	items, err := h.syncer.HeldTransactions(context.Background())
	if err != nil {
		t.Fatalf("HeldTransactions: %v", err)
	}
	if err := h.syncer.ResolveHeld(context.Background(), items[0].ID, "", 0,
		h.syncer.matchPolicy("").Version()); err != nil {
		t.Fatalf("ResolveHeld: %v", err)
	}

	decisions, err := h.st.GetMatchDecisions(50)
	if err != nil {
		t.Fatalf("GetMatchDecisions: %v", err)
	}
	var found bool
	for _, d := range decisions {
		if d.PendingKey != held.PendingKey {
			continue
		}
		found = true
		switch {
		case d.Truth == nil:
			t.Error("the decision was resolved and the record still says nothing about " +
				"what the answer was")
		case *d.Truth:
			t.Error("the person said the transaction was new, which refutes the pairing " +
				"the model offered; the record claims it was confirmed")
		}
	}
	if !found {
		t.Errorf("no decision on record for the transaction that was resolved")
	}
}

// TestReview_aSettingsChangeInvalidatesAnOpenPage covers the gap a probability
// check cannot reach.
//
// Thresholds do not enter the probability, so moving one changes what a page
// means without changing a single figure on it. And the "this is new" answer had
// no figure to check in the first place, so that path was until now checked for
// nothing at all.
func TestReview_aSettingsChangeInvalidatesAnOpenPage(t *testing.T) {
	h := newHarness(t)
	held := holdOne(t, h)
	stale := h.syncer.matchPolicy("").Version()

	if err := h.st.SetSetting(store.SettingAutoProbability, "95"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if fresh := h.syncer.matchPolicy("").Version(); fresh == stale {
		t.Fatal("moving a threshold did not change the parameter version")
	}

	// Both answers, because only one of them was ever checked.
	if err := h.syncer.ResolveHeld(context.Background(), held.ID, "", 0, stale); err == nil {
		t.Error(`"this is new" was applied against a page drawn under different settings`)
	}
	items, err := h.syncer.HeldTransactions(context.Background())
	if err != nil {
		t.Fatalf("HeldTransactions: %v", err)
	}
	c := items[0].Candidates[0]
	if err := h.syncer.ResolveHeld(context.Background(), held.ID, c.ID, c.Percent, stale); err == nil {
		t.Error("a merge was applied against a page drawn under different settings")
	}
	if n, _ := h.st.CountMatchReviews(); n != 1 {
		t.Errorf("%d decisions in the queue; a refused one must leave it alone", n)
	}
}

// TestSync_retentionIsActuallyCalled covers a policy that was written down and
// never applied.
//
// PruneMatchReviews existed, was documented, was tested against directly — and
// had no caller anywhere in the program. A retention window nothing invokes is a
// comment.
func TestSync_retentionIsActuallyCalled(t *testing.T) {
	h := newHarness(t)
	id := h.addAccount(t, "")
	h.reloadState(t)

	stale := time.Now().UTC().AddDate(0, 0, -400).Format("2006-01-02")
	if err := h.st.AddMatchReview(store.MatchReview{
		BankAccountID: id, Backend: "actual", PendingKey: "old-key",
		TxnDate: stale, AmountCents: -999, Payee: "Ancient",
	}); err != nil {
		t.Fatalf("AddMatchReview: %v", err)
	}
	if err := h.st.AddMatchDecision(store.MatchDecision{
		RunID: "old-run", BankAccountID: id, PendingKey: "old-key",
		Outcome: "created", ParamVersion: "v0", TxnDate: stale,
	}); err != nil {
		t.Fatalf("AddMatchDecision: %v", err)
	}

	h.syncer.run()

	if n, _ := h.st.CountMatchReviews(); n != 0 {
		t.Errorf("%d held transactions survived a run, %d days past the window", n, 400)
	}
	if n, _ := h.st.CountMatchDecisions(); n != 0 {
		t.Errorf("%d decisions survived a run, %d days past the window", n, 400)
	}
}

// TestSync_aClearSettlementIsNotHeldBecauseTheWindowIsBusy drives the prior fix
// through the whole sync, on the shape that made it visible.
//
// A payee agreeing exactly and an amount agreeing to the cent, four days after
// the authorisation, in a fortnight that also holds a dozen unrelated
// authorisations. That is not a doubtful case by any reading, and before the
// prior counted only plausible candidates it was put to a person.
func TestSync_aClearSettlementIsNotHeldBecauseTheWindowIsBusy(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(20))
		h.reloadState(t)

		page := []map[string]any{
			pendingTxnPayee("auth-1", daysAgo(9), "120.00", "Hotel Berlin"),
		}
		for i := 0; i < 12; i++ {
			page = append(page, pendingTxnPayee(
				fmt.Sprintf("noise-%d", i), daysAgo(8), fmt.Sprintf("%d.50", 30+i),
				fmt.Sprintf("Elektromarkt %d", i)))
		}
		h.eb.setPages([][]map[string]any{page})
		h.syncer.run()
		if got := len(h.actualTxns(t)); got != 13 {
			t.Fatalf("setup: %d authorisations, want 13", got)
		}

		h.eb.setPages([][]map[string]any{{
			bookedTxnPayee("book-1", daysAgo(5), "120.00", "VISA Hotel Berlin"),
		}})
		h.syncer.run()

		if n, _ := h.st.CountMatchReviews(); n != 0 {
			t.Errorf("%d decisions put to a person: a settlement matching on the payee and "+
				"to the cent was held because unrelated rows shared its fortnight", n)
		}
		if got := len(h.actualTxns(t)); got != 13 {
			t.Errorf("got %d transactions, want 13: the settlement was created beside its "+
				"own authorisation", got)
		}
		for _, x := range h.actualTxns(t) {
			if x.PayeeName == "Hotel Berlin" && !x.Cleared {
				t.Error("the authorisation was not confirmed by its settlement")
			}
		}
	})
}

// TestSync_theMarginAndMultiplicityAreCounted covers the two instruments that
// describe how the arrangement decided, rather than what it decided.
//
// The margin says whether there was a real alternative; the multiplicity counter
// says how often the choice was free because nothing could tell the candidates
// apart. That second one is the shape the reported defect had, so it is the one
// to watch if it ever comes back.
func TestSync_theMarginAndMultiplicityAreCounted(t *testing.T) {
	h := newHarness(t)
	reader := withMetrics(t, h)
	h.syncer.backendName = "firefly"
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("auth-1", daysAgo(4), "0.99", "App Store"),
		pendingTxnPayee("auth-2", daysAgo(4), "0.99", "App Store"),
		// Stays open, so that the unrelated booking below has a candidate to be
		// weighed against and rejected — which is where an infinite margin comes
		// from.
		pendingTxnPayee("auth-3", daysAgo(4), "50.00", "Kiosk Mueller"),
	}})
	h.syncer.run()
	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("book-1", daysAgo(3), "0.99", "VISA App Store"),
		bookedTxnPayee("book-2", daysAgo(3), "0.99", "VISA App Store"),
		bookedTxnPayee("book-3", daysAgo(3), "7.00", "Buchhandlung Schmidt"),
	}})
	h.syncer.run()

	margins := collectAttrSets(t, reader, "bankingsync_match_margin")
	if len(margins) == 0 {
		t.Error("no margins recorded; how close a call was is invisible")
	}
	// A transaction paired with nothing has an infinite margin, which is not a
	// measurement of how close the call was. One of them in the histogram makes
	// its sum infinite and every average drawn from it meaningless.
	if sum := histogramSum(t, reader, "bankingsync_match_margin"); math.IsInf(sum, 0) || math.IsNaN(sum) {
		t.Errorf("the margin histogram sums to %v; an unpaired transaction was recorded "+
			"as if it had been a close call", sum)
	}
	for _, set := range margins {
		if attrOf(set, "bank") == "" || attrOf(set, "outcome") == "" {
			t.Errorf("a margin was recorded without bank or outcome: %v", set.ToSlice())
		}
	}

	mult := collectAttrSets(t, reader, "bankingsync_match_multiplicity_total")
	if len(mult) == 0 {
		t.Error("two settlements onto two rows nothing could tell apart were not counted")
	}
	for _, set := range mult {
		if got := attrOf(set, "backend"); got != "firefly" {
			t.Errorf("backend = %q, want firefly", got)
		}
	}
}

// TestSync_theInheritedLabelGapsAreClosed covers two counters that named a
// reason or a duration without saying which account it belonged to. On a
// multi-account instance that is the difference between a number and an answer.
func TestSync_theInheritedLabelGapsAreClosed(t *testing.T) {
	h := newHarness(t)
	reader := withMetrics(t, h)
	h.addAccount(t, "")
	h.reloadState(t)

	h.syncer.nearMiss("TestBank", "payee", nil)
	h.syncer.run()

	for _, name := range []string{"bankingsync_near_miss_total", "bankingsync_fetch_duration_seconds"} {
		sets := collectAttrSets(t, reader, name)
		if len(sets) == 0 {
			t.Errorf("%s recorded nothing", name)
			continue
		}
		for _, set := range sets {
			if attrOf(set, "bank") == "" {
				t.Errorf("%s carries no bank label; on an instance with several accounts "+
					"it says something happened but not where", name)
			}
		}
	}
}

// TestSync_levelWeightsAreObservable makes a refit visible. The parameters are
// stated rather than estimated today, so the gauge is constant — which is the
// point: the day one moves, it moves on a dashboard and not only in a commit.
func TestSync_levelWeightsAreObservable(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	m := provider.Meter("bankingsync")
	_, err := m.Float64ObservableGauge("bankingsync_match_level_weight",
		metric.WithFloat64Callback(func(_ context.Context, obs metric.Float64Observer) error {
			for _, e := range budget.DefaultLinkage().LevelWeights() {
				obs.Observe(e.Bits, metric.WithAttributes(
					attribute.String("field", e.Field), attribute.String("level", e.Level)))
			}
			return nil
		}))
	if err != nil {
		t.Fatalf("register gauge: %v", err)
	}

	sets := collectAttrSets(t, reader, "bankingsync_match_level_weight")
	if len(sets) != 17 {
		t.Errorf("got %d level weights, want 17 — every level of every field", len(sets))
	}
	fields := map[string]bool{}
	for _, set := range sets {
		fields[attrOf(set, "field")] = true
		if attrOf(set, "level") == "" {
			t.Error("a weight was reported without naming its level")
		}
	}
	for _, want := range []string{"payee", "amount", "date"} {
		if !fields[want] {
			t.Errorf("no weights for the %s field", want)
		}
	}
}

// histogramSum totals one histogram across its series, which is how an infinity
// smuggled into it becomes visible.
func histogramSum(t *testing.T, reader *sdkmetric.ManualReader, name string) float64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var sum float64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			d, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is %T, not a histogram", name, m.Data)
			}
			for _, dp := range d.DataPoints {
				sum += dp.Sum
			}
		}
	}
	return sum
}

// TestSync_calibrationIsSilentUntilThereIsSomethingToSay is the guarantee for an
// installation that never accumulates labels — which is most of them.
//
// A Brier score over a dozen answers is noise with a decimal point. A missing
// series is honest; a wrong one gets alerted on.
func TestSync_calibrationIsSilentUntilThereIsSomethingToSay(t *testing.T) {
	h := newHarness(t)
	id := h.addAccount(t, "")
	h.reloadState(t)

	if got := h.syncer.labelledDecisions(); got != nil {
		t.Errorf("%d observations from an empty log", len(got))
	}

	day := time.Now().UTC().Format("2006-01-02")
	add := func(n int, match bool) {
		t.Helper()
		for i := 0; i < n; i++ {
			key := fmt.Sprintf("k-%v-%d", match, i)
			if err := h.st.AddMatchDecision(store.MatchDecision{
				RunID: "r", BankAccountID: id, PendingKey: key, Outcome: "held",
				ParamVersion: "v1", TxnDate: day, Weight: 3,
			}); err != nil {
				t.Fatalf("AddMatchDecision: %v", err)
			}
			if err := h.st.SetMatchDecisionTruth(id, key, match); err != nil {
				t.Fatalf("SetMatchDecisionTruth: %v", err)
			}
		}
	}

	add(minLabelledDecisions-1, true)
	if got := h.syncer.labelledDecisions(); got != nil {
		t.Errorf("%d observations below the threshold of %d", len(got), minLabelledDecisions)
	}

	add(30, false)
	got := h.syncer.labelledDecisions()
	if len(got) < minLabelledDecisions {
		t.Fatalf("%d observations above the threshold, want at least %d",
			len(got), minLabelledDecisions)
	}

	// And unsettled decisions are not counted as anything: a decision nobody has
	// answered is not evidence that the model was right.
	if err := h.st.AddMatchDecision(store.MatchDecision{
		RunID: "r", BankAccountID: id, PendingKey: "open", Outcome: "held",
		ParamVersion: "v1", TxnDate: day, Weight: 3,
	}); err != nil {
		t.Fatalf("AddMatchDecision: %v", err)
	}
	if after := h.syncer.labelledDecisions(); len(after) != len(got) {
		t.Errorf("an unanswered decision changed the count from %d to %d", len(got), len(after))
	}
}

// TestSync_aKeyConfirmationLabelsWhatTheModelWouldHaveSaid is the second source
// of labels, and the one that reaches where the first cannot.
//
// Review answers only describe the band a person was asked about. Above and
// below it the model decides alone and nobody ever finds out whether it was
// right — the same blind spot credit scoring calls reject inference. A pending
// entry confirmed by the bank's own key is a pair the bank says is one payment,
// with no opinion of the model's involved, so scoring the model against it
// measures exactly the merges it would have missed.
func TestSync_aKeyConfirmationLabelsWhatTheModelWouldHaveSaid(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("auth-1", daysAgo(3), "120.00", "Hotel Berlin"),
	}})
	h.syncer.run()
	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("auth-1", daysAgo(3), "120.00", "VISA Hotel Berlin"),
	}})
	h.syncer.run()

	if got := len(h.actualTxns(t)); got != 1 {
		t.Fatalf("setup: %d transactions, want 1 — the key did not confirm the pair", got)
	}

	decisions, err := h.st.GetMatchDecisions(50)
	if err != nil {
		t.Fatalf("GetMatchDecisions: %v", err)
	}
	var found bool
	for _, d := range decisions {
		if d.Outcome != "confirmed_by_reference" {
			continue
		}
		found = true
		if d.Truth == nil || !*d.Truth {
			t.Error("a pair the bank's key settled was not recorded as a match")
		}
		if d.PayeeLevel == "" || d.Weight == 0 {
			t.Errorf("the record says nothing about what the model would have made of "+
				"it: levels %q/%q/%q, weight %.3f",
				d.PayeeLevel, d.AmountLevel, d.DateLevel, d.Weight)
		}
		if d.CandidateID == "" {
			t.Error("the record does not name the row the pair was settled onto")
		}
	}
	if !found {
		t.Errorf("no reference label was recorded; the only measurement of merges the "+
			"matcher would have missed is missing. Got %d decisions", len(decisions))
	}
}

// TestSync_aReferenceLabelDescribesTheRowBeforeItWasMerged is the guard on the
// measurement chain.
//
// The label exists to say what the matcher would have made of the pair. Both
// backends end Update with budget.Apply, which patches the caller's transaction
// in place, so a row scored after the merge has already been given the incoming
// amount and scores as a perfect agreement with itself. A settlement that moved
// the amount must be recorded as having moved it.
func TestSync_aReferenceLabelDescribesTheRowBeforeItWasMerged(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("auth-1", daysAgo(3), "120.00", "Hotel Berlin"),
	}})
	h.syncer.run()
	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("auth-1", daysAgo(3), "125.00", "Hotel Berlin"),
	}})
	h.syncer.run()

	if got := len(h.actualTxns(t)); got != 1 {
		t.Fatalf("setup: %d transactions, want 1 — the key did not confirm the pair", got)
	}

	decisions, err := h.st.GetLabelledMatchDecisions(50)
	if err != nil {
		t.Fatalf("GetLabelledMatchDecisions: %v", err)
	}
	var found bool
	for _, d := range decisions {
		if d.Outcome != "confirmed_by_reference" {
			continue
		}
		found = true
		if d.AmountLevel != "higher_within" {
			t.Errorf("the settlement moved 120.00 to 125.00 but the label records %q; "+
				"scoring after the merge compares the merged row with itself",
				d.AmountLevel)
		}
		if d.Candidates != 0 {
			t.Errorf("Candidates = %d, want 0 — a one-element window reports a plausible "+
				"count of one, whose prior is half a bit of nothing, and the label would "+
				"later be re-scored under it", d.Candidates)
		}
	}
	if !found {
		t.Fatalf("no reference label was recorded. Got %d labelled decisions", len(decisions))
	}
}

// TestSync_aFallbackKeyConfirmationIsMeasuredButNotBelieved pins the line
// between the two things a pending-map hit can mean.
//
// With a bank reference the key is the bank's own identifier and the pair is
// evidence. Without one the key is date|amount|payee|n — every field the model
// compares — so the pair agrees on those fields because the key said so, and
// fitting m to it would ask the model to confirm the rule that selected it.
//
// The decision is still recorded, because a merge the reference caught and the
// matcher would have missed is worth counting either way. Its truth is left
// unset so that no estimator can reach it.
func TestSync_aFallbackKeyConfirmationIsMeasuredButNotBelieved(t *testing.T) {
	h := newHarness(t)
	reader := withMetrics(t, h)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("", daysAgo(3), "120.00", "Hotel Berlin"),
	}})
	h.syncer.run()
	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("", daysAgo(3), "120.00", "VISA Hotel Berlin"),
	}})
	h.syncer.run()

	if got := len(h.actualTxns(t)); got != 1 {
		t.Fatalf("setup: %d transactions, want 1 — the key did not confirm the pair", got)
	}

	decisions, err := h.st.GetMatchDecisions(50)
	if err != nil {
		t.Fatalf("GetMatchDecisions: %v", err)
	}
	var found bool
	for _, d := range decisions {
		if d.Outcome != "confirmed_by_fallback_key" {
			continue
		}
		found = true
		if d.Truth != nil {
			t.Error("a pair selected on the model's own comparison fields was recorded as " +
				"evidence; fitting m to it would be circular")
		}
		if d.PayeeLevel == "" || d.Weight == 0 {
			t.Errorf("the record says nothing about what the model would have made of it: "+
				"levels %q/%q/%q, weight %.3f",
				d.PayeeLevel, d.AmountLevel, d.DateLevel, d.Weight)
		}
	}
	if !found {
		t.Fatalf("no fallback-key confirmation was recorded, so the false-negative count "+
			"is lost for reference-less feeds. Got %d decisions", len(decisions))
	}

	labelled, err := h.st.GetLabelledMatchDecisions(50)
	if err != nil {
		t.Fatalf("GetLabelledMatchDecisions: %v", err)
	}
	if len(labelled) != 0 {
		t.Errorf("a reference-less feed produced %d labels; it must produce none", len(labelled))
	}

	var sawFallback bool
	for _, set := range collectAttrSets(t, reader, "bankingsync_match_labels_total") {
		if attrOf(set, "source") == "fallback_key" {
			sawFallback = true
		}
		if attrOf(set, "source") == "reference" {
			t.Error("a reference-less confirmation was counted as if the bank had vouched for it")
		}
	}
	if !sawFallback {
		t.Error("the confirmation was not counted at all, so the false-negative rate is blind")
	}
}

// TestSync_labelsAreCountedBySource keeps the two sources apart, because they
// are not equally trustworthy: a review answer is a person's judgement about a
// doubtful case, a key confirmation is the bank stating a fact.
func TestSync_labelsAreCountedBySource(t *testing.T) {
	h := newHarness(t)
	reader := withMetrics(t, h)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("auth-1", daysAgo(3), "120.00", "Hotel Berlin"),
	}})
	h.syncer.run()
	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("auth-1", daysAgo(3), "120.00", "VISA Hotel Berlin"),
	}})
	h.syncer.run()

	sources := map[string]bool{}
	for _, set := range collectAttrSets(t, reader, "bankingsync_match_labels_total") {
		sources[attrOf(set, "source")] = true
		if attrOf(set, "bank") == "" {
			t.Error("a label was counted without saying which account it came from")
		}
	}
	if !sources["reference"] {
		t.Errorf("no label from the bank's key; got %v", sources)
	}
	// And whether the model would have got there on its own, which is the
	// measurement worth having whether or not anything is ever fitted to it.
	if !sources["reference_model_agreed"] {
		t.Errorf("the model's own answer was not scored against the key; got %v", sources)
	}
}

// TestSync_levelCountsReadTheLogBackCorrectly is the bridge from what was
// recorded to what could be estimated, and it has one trap worth guarding: the
// log stores levels as names, so a level inserted in the middle of the constants
// must not silently rewrite what earlier runs meant.
func TestSync_levelCountsReadTheLogBackCorrectly(t *testing.T) {
	h := newHarness(t)
	id := h.addAccount(t, "")
	other := h.addAccount(t, "")
	h.reloadState(t)

	day := time.Now().UTC().Format("2006-01-02")
	yes, no := true, false
	add := func(acct int64, key, payee string, truth *bool) {
		t.Helper()
		d := store.MatchDecision{
			RunID: "r", BankAccountID: acct, PendingKey: key, Outcome: "held",
			ParamVersion: "v1", TxnDate: day, PayeeLevel: payee,
			AmountLevel: "exact", DateLevel: "same",
		}
		if err := h.st.AddMatchDecision(d); err != nil {
			t.Fatalf("AddMatchDecision: %v", err)
		}
		if truth != nil {
			if err := h.st.SetMatchDecisionTruth(acct, key, *truth); err != nil {
				t.Fatalf("SetMatchDecisionTruth: %v", err)
			}
		}
	}

	add(id, "a", "truncated", &yes)
	add(id, "b", "truncated", &yes)
	add(id, "c", "conflict", &no)
	add(id, "d", "exact", nil) // unanswered
	add(other, "e", "truncated", &yes)

	own := h.syncer.levelCounts(id)
	if own.PayeeM[budget.PayeeTruncated] != 2 {
		t.Errorf("truncated matches for this account = %d, want 2",
			own.PayeeM[budget.PayeeTruncated])
	}
	if own.PayeeU[budget.PayeeConflict] != 1 {
		t.Errorf("refuted conflicts = %d, want 1", own.PayeeU[budget.PayeeConflict])
	}
	if own.PayeeM[budget.PayeeExact] != 0 {
		t.Error("an unanswered decision was counted; nobody having looked is not evidence " +
			"that the model was right")
	}

	all := h.syncer.levelCounts(0)
	if all.PayeeM[budget.PayeeTruncated] != 3 {
		t.Errorf("truncated matches across the installation = %d, want 3 — the middle "+
			"step of the hierarchical estimate counts every account",
			all.PayeeM[budget.PayeeTruncated])
	}

	// And the level names round-trip: a table keyed by position would have made
	// every earlier record mean something else the day a level was inserted.
	for _, l := range []budget.PayeeLevel{budget.PayeeMissing, budget.PayeeNone,
		budget.PayeeConflict, budget.PayeeSubset, budget.PayeeTruncated,
		budget.PayeeFuzzy, budget.PayeeExact} {
		if got, ok := payeeLevelByName[l.String()]; !ok || got != l {
			t.Errorf("payee level %q does not read back as itself", l)
		}
	}
}

// askWhenUnsure turns Phase 6 on for a harness.
func askWhenUnsure(t *testing.T, h *harness) {
	t.Helper()
	if err := h.st.SetSetting(store.SettingAskWhenUnsure, "1"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
}

// settlingPair feeds an authorisation and then the booking that settles it, one
// run each, which is the ordinary shape of a decision the matcher makes on its
// own.
func settlingPair(t *testing.T, h *harness, amount, payee string) {
	t.Helper()
	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("", daysAgo(4), amount, payee),
	}})
	h.syncer.run()
	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("", daysAgo(3), amount, "VISA "+payee),
	}})
	h.syncer.run()
}

// TestSync_nobodyIsAskedAnythingUntilTheyAskToBe is the guarantee for every
// installation that never turns this on, which is the default and the majority.
//
// It is the only setting in the program that spends a person's attention rather
// than changing what happens to a transaction, so silence has to be what it does
// when nobody has said otherwise.
func TestSync_nobodyIsAskedAnythingUntilTheyAskToBe(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)

	settlingPair(t, h, "42.00", "Hotel Berlin")

	if _, ok, err := h.st.OpenInquiry(); err != nil || ok {
		t.Fatalf("a question was asked without the setting being turned on (ok=%v, err=%v)", ok, err)
	}
}

// TestSync_oneAutomaticDecisionIsPutUpForConfirmation is the feature itself: with
// the setting on, a run that decided something alone asks about one of them.
func TestSync_oneAutomaticDecisionIsPutUpForConfirmation(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)
	askWhenUnsure(t, h)

	settlingPair(t, h, "42.00", "Hotel Berlin")

	q, ok, err := h.st.OpenInquiry()
	if err != nil || !ok {
		t.Fatalf("no confirmation was raised (ok=%v, err=%v)", ok, err)
	}
	if q.Gain <= 0 {
		t.Errorf("a question worth nothing was asked (%.6g bits)", q.Gain)
	}
	if q.CandidatePayee == "" || q.Payee == "" {
		t.Errorf("the question does not name both rows: %q against %q", q.Payee, q.CandidatePayee)
	}
	if q.ParamVersion == "" {
		t.Error("the question carries no parameter version, so a settings change cannot invalidate it")
	}
}

// TestSync_aConfirmationDoesNotHoldUpTheBudget is the distinction from the review
// queue, and the reason this asks afterwards rather than before.
//
// A question about a decision the matcher was confident enough to act on must
// not be the reason money is missing from a budget. Holding it would turn a
// request for a label into a delay, and the whole point is that the label is
// cheap.
func TestSync_aConfirmationDoesNotHoldUpTheBudget(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)
	askWhenUnsure(t, h)

	settlingPair(t, h, "42.00", "Hotel Berlin")

	if _, ok, _ := h.st.OpenInquiry(); !ok {
		t.Fatal("no confirmation was raised, so this proves nothing")
	}
	if got := len(h.actualTxns(t)); got != 1 {
		t.Fatalf("the settled pair should be one transaction in the budget, got %d", got)
	}
	reviews, err := h.st.GetMatchReviews()
	if err != nil {
		t.Fatalf("GetMatchReviews: %v", err)
	}
	if len(reviews) != 0 {
		t.Errorf("the confirmation put %d transaction(s) in the review queue", len(reviews))
	}
}

// TestSync_onlyOneQuestionIsOutstandingAtATime is what keeps this from becoming
// something people click past.
//
// A nightly sync would otherwise stack up a month of questions, and a queue of
// unanswered questions produces worse labels than no questions at all: the ones
// that do get answered are the ones somebody clicked through quickly.
func TestSync_onlyOneQuestionIsOutstandingAtATime(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)
	askWhenUnsure(t, h)

	settlingPair(t, h, "42.00", "Hotel Berlin")
	first, ok, _ := h.st.OpenInquiry()
	if !ok {
		t.Fatal("no confirmation was raised")
	}

	settlingPair(t, h, "17.50", "Bahn Ticket")
	settlingPair(t, h, "9.99", "Kino Astor")

	second, ok, err := h.st.OpenInquiry()
	if err != nil || !ok {
		t.Fatalf("the open question disappeared (ok=%v, err=%v)", ok, err)
	}
	if second.ID != first.ID {
		t.Errorf("a second question was asked while the first was unanswered: %d then %d",
			first.ID, second.ID)
	}
}

// TestSync_answeringAConfirmationTeachesTheModel is the whole purpose: the answer
// has to arrive where the estimators look, which is the decision log, and it has
// to arrive as an observation of the levels that decision actually compared.
func TestSync_answeringAConfirmationTeachesTheModel(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)
	askWhenUnsure(t, h)

	settlingPair(t, h, "42.00", "Hotel Berlin")
	q, ok, _ := h.st.OpenInquiry()
	if !ok {
		t.Fatal("no confirmation was raised")
	}

	before := h.syncer.levelCounts(0)
	yes := true
	if err := h.syncer.AnswerInquiry(context.Background(), q.ID, &yes, q.ParamVersion); err != nil {
		t.Fatalf("AnswerInquiry: %v", err)
	}

	after := h.syncer.levelCounts(0)
	if total(after) != total(before)+3 {
		t.Fatalf("one answer should add one observation per field, got %d against %d",
			total(after), total(before))
	}
	if open, _ := h.st.HasOpenInquiry(); open {
		t.Error("the question stayed open after being answered")
	}
}

// TestSync_notKnowingIsAnAnswerAndIsNotALabel keeps a guess out of the counts.
//
// The alternative is a page with only yes and no on it, which does not stop
// people who cannot remember a transaction from pressing one of them. A label
// invented that way is worse than none: it is about to be weighed against a
// prior that at least had an argument behind it.
func TestSync_notKnowingIsAnAnswerAndIsNotALabel(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)
	askWhenUnsure(t, h)

	settlingPair(t, h, "42.00", "Hotel Berlin")
	q, ok, _ := h.st.OpenInquiry()
	if !ok {
		t.Fatal("no confirmation was raised")
	}

	before := total(h.syncer.levelCounts(0))
	if err := h.syncer.AnswerInquiry(context.Background(), q.ID, nil, q.ParamVersion); err != nil {
		t.Fatalf("AnswerInquiry: %v", err)
	}

	if after := total(h.syncer.levelCounts(0)); after != before {
		t.Errorf("not knowing became %d observation(s)", after-before)
	}
	if open, _ := h.st.HasOpenInquiry(); open {
		t.Error("not knowing left the question open, so it will be asked again forever")
	}
}

// TestSync_aConfirmationAnsweredAgainstChangedSettingsIsRefused is the same guard
// the queue has, for the same reason: an answer about a decision the program
// would no longer make teaches it about a version of itself that no longer runs.
func TestSync_aConfirmationAnsweredAgainstChangedSettingsIsRefused(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)
	askWhenUnsure(t, h)

	settlingPair(t, h, "42.00", "Hotel Berlin")
	q, ok, _ := h.st.OpenInquiry()
	if !ok {
		t.Fatal("no confirmation was raised")
	}

	yes := true
	if err := h.syncer.AnswerInquiry(context.Background(), q.ID, &yes, "some-other-version"); err == nil {
		t.Fatal("an answer given under different parameters was accepted")
	}
	if open, _ := h.st.HasOpenInquiry(); !open {
		t.Error("the refused answer closed the question anyway")
	}
	if got := total(h.syncer.levelCounts(0)); got != 0 {
		t.Errorf("the refused answer left %d observation(s) behind", got)
	}
}

// TestSync_aHeldTransactionIsNotAlsoAskedAbout keeps the two channels apart. A
// transaction in the queue is already on its way to a person, and asking about
// it here would be asking the same thing twice and counting the answer once.
//
// The run carries a settled pair alongside the doubtful one so that there is
// something else to ask about: a run in which the sampler simply found nothing
// would pass this without proving anything.
func TestSync_aHeldTransactionIsNotAlsoAskedAbout(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)
	askWhenUnsure(t, h)

	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("auth-1", daysAgo(3), "9.99", "Spotify"),
		pendingTxnPayee("auth-2", daysAgo(4), "42.00", "Hotel Berlin"),
	}})
	h.syncer.run()
	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("book-1", daysAgo(3), "9.99", "Netflix"),
		bookedTxnPayee("book-2", daysAgo(3), "42.00", "VISA Hotel Berlin"),
	}})
	h.syncer.run()

	reviews, err := h.st.GetMatchReviews()
	if err != nil {
		t.Fatalf("GetMatchReviews: %v", err)
	}
	if len(reviews) != 1 {
		t.Fatalf("got %d held transactions, want 1 — this test needs one to rule out", len(reviews))
	}
	q, ok, err := h.st.OpenInquiry()
	if err != nil || !ok {
		t.Fatalf("no confirmation was raised, so the exclusion is untested (ok=%v, err=%v)", ok, err)
	}
	if q.PendingKey == reviews[0].PendingKey {
		t.Fatalf("the held transaction %q was also put up for confirmation", q.PendingKey)
	}
}

// total counts every observation in a set of level counts.
func total(c budget.LevelCounts) int {
	n := 0
	for _, m := range []map[budget.PayeeLevel]int{c.PayeeM, c.PayeeU} {
		for _, v := range m {
			n += v
		}
	}
	for _, m := range []map[budget.AmountLevel]int{c.AmountM, c.AmountU} {
		for _, v := range m {
			n += v
		}
	}
	for _, m := range []map[budget.DateLevel]int{c.DateM, c.DateU} {
		for _, v := range m {
			n += v
		}
	}
	return n
}

// TestSync_theAnswerGoesToTheDecisionItWasAbout guards the label against the
// transaction being decided again between the question and the answer.
//
// Every other label in this program is filed against the newest record for a
// transaction, and for the review queue that is right: a held row is re-offered
// every run, and the last record is the state a person has now settled. A
// question is not like that. It showed one comparison, at one moment, and if a
// later run has since compared the same transaction against something else the
// answer would be filed against levels nobody was ever shown — which is a wrong
// label written silently, in the one table this whole phase exists to fill.
func TestSync_theAnswerGoesToTheDecisionItWasAbout(t *testing.T) {
	h := newHarness(t)
	id := h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)
	askWhenUnsure(t, h)

	settlingPair(t, h, "42.00", "Hotel Berlin")
	q, ok, _ := h.st.OpenInquiry()
	if !ok {
		t.Fatal("no confirmation was raised")
	}
	if q.DecisionID == 0 {
		t.Fatal("the question does not name the decision it is about")
	}

	// A later run decides the same transaction again, on different levels.
	if err := h.st.AddMatchDecision(store.MatchDecision{
		RunID: "later", BankAccountID: id, PendingKey: q.PendingKey,
		Outcome: "created", ParamVersion: q.ParamVersion,
		TxnDate:    time.Now().UTC().Format("2006-01-02"),
		PayeeLevel: "conflict", AmountLevel: "outside_lower", DateLevel: "before_far",
	}); err != nil {
		t.Fatalf("AddMatchDecision: %v", err)
	}

	yes := true
	if err := h.syncer.AnswerInquiry(context.Background(), q.ID, &yes, q.ParamVersion); err != nil {
		t.Fatalf("AnswerInquiry: %v", err)
	}

	decisions, err := h.st.GetMatchDecisions(100)
	if err != nil {
		t.Fatalf("GetMatchDecisions: %v", err)
	}
	for _, d := range decisions {
		switch {
		case d.ID == q.DecisionID && d.Truth == nil:
			t.Error("the answer did not reach the decision the question was about")
		case d.RunID == "later" && d.Truth != nil:
			t.Errorf("the answer was filed against a later decision on levels %q/%q/%q, "+
				"which nobody was shown", d.PayeeLevel, d.AmountLevel, d.DateLevel)
		}
	}
}

// settleDecisions writes n settled decisions on the given levels, which is the
// evidence a refit is estimated from.
func settleDecisions(t *testing.T, h *harness, id int64, prefix string, n int,
	payee, amount, date string, match bool) {
	t.Helper()
	day := time.Now().UTC().Format("2006-01-02")
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("%s-%d", prefix, i)
		if err := h.st.AddMatchDecision(store.MatchDecision{
			RunID: "r", BankAccountID: id, Bank: "TestBank", PendingKey: key,
			Outcome: "adopted", ParamVersion: "v1", TxnDate: day, Candidates: 2,
			PayeeLevel: payee, AmountLevel: amount, DateLevel: date,
		}); err != nil {
			t.Fatalf("AddMatchDecision: %v", err)
		}
		if err := h.st.SetMatchDecisionTruth(id, key, match); err != nil {
			t.Fatalf("SetMatchDecisionTruth: %v", err)
		}
	}
}

// evidence writes settled decisions sampled from a stated truth about how a bank
// behaves.
//
// Sampled from a linkage rather than assembled by hand, and that is not a
// shortcut. The refit estimates the marginal distribution of each level given
// match and given non-match, so it can only be judged on a corpus that is a
// sample of pairs. A hand-picked list of interesting cases has whatever marginal
// distribution the list happens to have, and refitting to it measures the
// author's choice of examples rather than anything about a bank.
func evidence(t *testing.T, h *harness, id int64, truth budget.Linkage, baseRate float64, n int, seed int64) {
	t.Helper()
	rng := mrand.New(mrand.NewPCG(uint64(seed), 0x9E3779B9))

	payees := []budget.PayeeLevel{budget.PayeeMissing, budget.PayeeNone, budget.PayeeConflict,
		budget.PayeeSubset, budget.PayeeTruncated, budget.PayeeFuzzy, budget.PayeeExact}
	amounts := []budget.AmountLevel{budget.AmountOutsideLower, budget.AmountOutsideHigher,
		budget.AmountLowerWithin, budget.AmountHigherWithin, budget.AmountExact}
	dates := []budget.DateLevel{budget.DateBeforeFar, budget.DateAfterFar, budget.DateBeforeNear,
		budget.DateAfterNear, budget.DateSame}

	day := time.Now().UTC().Format("2006-01-02")
	for i := 0; i < n; i++ {
		match := rng.Float64() < baseRate
		pick := func(u float64, weights func(int) float64, count int) int {
			var acc float64
			for k := 0; k < count; k++ {
				acc += weights(k)
				if u < acc {
					return k
				}
			}
			return count - 1
		}
		pm, pu := truth.PayeeM, truth.PayeeU
		am, au := truth.AmountM, truth.AmountU
		dm, du := truth.DateM, truth.DateU

		p := payees[pick(rng.Float64(), func(k int) float64 {
			if match {
				return pm[payees[k]]
			}
			return pu[payees[k]]
		}, len(payees))]
		a := amounts[pick(rng.Float64(), func(k int) float64 {
			if match {
				return am[amounts[k]]
			}
			return au[amounts[k]]
		}, len(amounts))]
		d := dates[pick(rng.Float64(), func(k int) float64 {
			if match {
				return dm[dates[k]]
			}
			return du[dates[k]]
		}, len(dates))]

		key := fmt.Sprintf("ev-%d", i)
		if err := h.st.AddMatchDecision(store.MatchDecision{
			RunID: "r", BankAccountID: id, Bank: "TestBank", PendingKey: key,
			Outcome: "adopted", ParamVersion: "v1", TxnDate: day, Candidates: 2,
			PayeeLevel: p.String(), AmountLevel: a.String(), DateLevel: d.String(),
		}); err != nil {
			t.Fatalf("AddMatchDecision: %v", err)
		}
		if err := h.st.SetMatchDecisionTruth(id, key, match); err != nil {
			t.Fatalf("SetMatchDecisionTruth: %v", err)
		}
	}
}

// divergentBank is how an institution the shipped parameters are wrong about
// actually behaves: it truncates the payee far more often than the defaults
// expect, and settles away from the authorised amount more often.
//
// A divergence in degree, which is the only kind a refit should be allowed to
// fix. A bank that reversed the ordering of the levels would be a different
// model rather than a rescaled one, and the anchors would refuse it however well
// it scored — which is the constraint working, not failing.
func divergentBank() budget.Linkage {
	l := budget.DefaultLinkage()
	l.PayeeM = map[budget.PayeeLevel]float64{
		budget.PayeeExact: 0.34, budget.PayeeTruncated: 0.30, budget.PayeeFuzzy: 0.05,
		budget.PayeeSubset: 0.13, budget.PayeeConflict: 0.02, budget.PayeeNone: 0.02,
		budget.PayeeMissing: 0.14,
	}
	l.AmountM = map[budget.AmountLevel]float64{
		budget.AmountExact: 0.55, budget.AmountHigherWithin: 0.20,
		budget.AmountLowerWithin: 0.18, budget.AmountOutsideHigher: 0.035,
		budget.AmountOutsideLower: 0.035,
	}
	return l
}

// evidenced builds an installation with enough settled decisions for a candidate
// parameter set to be proposed, judged and promoted.
func evidenced(t *testing.T) (*harness, int64) {
	t.Helper()
	h := newHarness(t)
	id := h.addAccount(t, "")
	h.reloadState(t)
	evidence(t, h, id, divergentBank(), 0.45, 3000, 7)
	return h, id
}

// TestPromotion_anInstallationWithoutEvidenceIsToldSo keeps the page honest for
// the majority, who will never have a settled decision at all.
func TestPromotion_anInstallationWithoutEvidenceIsToldSo(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	h.reloadState(t)

	got, err := h.syncer.PromotionStatus(context.Background())
	if err != nil {
		t.Fatalf("PromotionStatus: %v", err)
	}
	if got.Labelled != 0 || got.Candidate != "" {
		t.Errorf("a candidate was proposed from nothing: %+v", got)
	}
	if got.InForce != h.syncer.matchPolicy("").Version() {
		t.Errorf("the version in force is %q, want %q", got.InForce, h.syncer.matchPolicy("").Version())
	}
}

// TestPromotion_nothingIsPutIntoForceWithoutHavingBeenWatched is the gate's
// central rule.
//
// Promoting a candidate nobody has watched installs parameters with no record of
// what they would have changed — which is the one finding the whole page exists
// to put in front of somebody, and the only one the program refuses to judge on
// their behalf.
func TestPromotion_nothingIsPutIntoForceWithoutHavingBeenWatched(t *testing.T) {
	h, _ := evidenced(t)
	ctx := context.Background()

	got, err := h.syncer.PromotionStatus(ctx)
	if err != nil {
		t.Fatalf("PromotionStatus: %v", err)
	}
	if got.Candidate == "" {
		t.Fatal("no candidate was proposed, so this proves nothing")
	}
	if err := h.syncer.PromoteTrial(ctx, got.Candidate); err == nil {
		t.Fatal("a candidate nobody had watched was put into force")
	}
	if now := h.syncer.matchPolicy("").Version(); now != got.InForce {
		t.Errorf("the refused promotion changed the parameters anyway: %q", now)
	}
}

// TestPromotion_watchingChangesNothing is the guarantee that makes watching safe
// to switch on.
func TestPromotion_watchingChangesNothing(t *testing.T) {
	h, _ := evidenced(t)
	ctx := context.Background()
	before := h.syncer.matchPolicy("").Version()

	got, _ := h.syncer.PromotionStatus(ctx)
	if err := h.syncer.WatchTrial(ctx, got.Candidate); err != nil {
		t.Fatalf("WatchTrial: %v", err)
	}

	pol := h.syncer.matchPolicy("")
	if pol.Version() != before {
		t.Errorf("watching changed the parameters in force: %q became %q", before, pol.Version())
	}
	if pol.Trial == nil {
		t.Fatal("watching did not attach the candidate to the policy")
	}
	if pol.Trial.Version(pol) != got.Candidate {
		t.Errorf("a different candidate is being watched: %q", pol.Trial.Version(pol))
	}
}

// TestPromotion_aWatchedCandidateIsRecordedAgainstEveryDecision is what fills the
// tally the person is asked to look at.
func TestPromotion_aWatchedCandidateIsRecordedAgainstEveryDecision(t *testing.T) {
	h, _ := evidenced(t)
	ctx := context.Background()
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)

	got, _ := h.syncer.PromotionStatus(ctx)
	if err := h.syncer.WatchTrial(ctx, got.Candidate); err != nil {
		t.Fatalf("WatchTrial: %v", err)
	}

	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("", daysAgo(4), "42.00", "Hotel Berlin"),
	}})
	h.syncer.run()
	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("", daysAgo(3), "42.00", "VISA Hotel Berlin"),
	}})
	h.syncer.run()

	tally, err := h.st.CountShadowOutcomes(got.Candidate)
	if err != nil {
		t.Fatalf("CountShadowOutcomes: %v", err)
	}
	if tally.Total == 0 {
		t.Fatal("no decision was recorded against the watched candidate")
	}
	if tally.Differing > tally.Total {
		t.Errorf("%d of %d decisions differed, which is more than were made",
			tally.Differing, tally.Total)
	}
}

// TestPromotion_aPromotedSetIsWhatDecides closes the loop: after promotion the
// matcher really is running the fitted parameters, and the metrics say so.
func TestPromotion_aPromotedSetIsWhatDecides(t *testing.T) {
	h, _ := evidenced(t)
	ctx := context.Background()
	shipped := h.syncer.matchPolicy("").Version()

	got, _ := h.syncer.PromotionStatus(ctx)
	if err := h.syncer.WatchTrial(ctx, got.Candidate); err != nil {
		t.Fatalf("WatchTrial: %v", err)
	}
	if err := h.syncer.PromoteTrial(ctx, got.Candidate); err != nil {
		for _, c := range got.Verdict.Checks {
			t.Logf("  %-18s %-12s %s", c.Name, c.Status, c.Detail)
		}
		t.Fatalf("PromoteTrial: %v", err)
	}

	pol := h.syncer.matchPolicy("")
	if pol.Version() != got.Candidate {
		t.Fatalf("the promoted parameters are not in force: %q, want %q",
			pol.Version(), got.Candidate)
	}
	if pol.Version() == shipped {
		t.Error("promotion produced the parameters that were already in force")
	}
	if pol.Trial != nil {
		t.Error("the promoted candidate is still being watched against itself")
	}

	// And the way back exists, because a change to how money is matched that
	// cannot be undone is one nobody should be encouraged to make.
	if err := h.syncer.RevertParameters(ctx); err != nil {
		t.Fatalf("RevertParameters: %v", err)
	}
	if now := h.syncer.matchPolicy("").Version(); now != shipped {
		t.Errorf("reverting left %q in force, want the shipped %q", now, shipped)
	}
}

// TestPromotion_aStaleCandidateIsRefused stops a page left open overnight from
// installing parameters other than the ones it described.
func TestPromotion_aStaleCandidateIsRefused(t *testing.T) {
	h, id := evidenced(t)
	ctx := context.Background()

	got, _ := h.syncer.PromotionStatus(ctx)
	if err := h.syncer.WatchTrial(ctx, got.Candidate); err != nil {
		t.Fatalf("WatchTrial: %v", err)
	}

	// More evidence arrives, so the candidate the evidence supports is no longer
	// the one being watched.
	settleDecisions(t, h, id, "later", 300, "conflict", "lower_within", "before_far", false)

	after, _ := h.syncer.PromotionStatus(ctx)
	if after.Candidate == got.Candidate {
		t.Fatal("three hundred more decisions did not move the candidate, so this proves nothing")
	}
	if err := h.syncer.PromoteTrial(ctx, got.Candidate); err == nil {
		t.Error("parameters other than the ones the evidence supports were put into force")
	}
	if err := h.syncer.PromoteTrial(ctx, after.Candidate); err == nil {
		t.Error("a candidate that had never been watched was put into force")
	}
}

// TestPromotion_unreadableStoredParametersDoNotStopASync keeps an optimisation
// from taking the budget down with it. The shipped parameters are always a valid
// answer, so a damaged file falls back to them rather than failing.
func TestPromotion_unreadableStoredParametersDoNotStopASync(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(6))
	h.reloadState(t)
	shipped := h.syncer.matchPolicy("").Version()

	if err := h.st.SetSetting(store.SettingPromotedTrial, `{"payee_m":{"exact":1}}`); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if now := h.syncer.matchPolicy("").Version(); now != shipped {
		t.Errorf("a damaged parameter file was used: %q, want the shipped %q", now, shipped)
	}

	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("b-1", daysAgo(3), "42.00", "Hotel Berlin"),
	}})
	h.syncer.run()
	if got := len(h.actualTxns(t)); got != 1 {
		t.Errorf("the sync imported %d transactions, want 1", got)
	}
}

// TestPromotion_narrowEvidenceIsRefusedHoweverWellItScores is the anchor check
// earning its place, and it was found by accident before it was written.
//
// An installation whose settled decisions cover only a few level combinations
// produces a refit that reads the silence as impossibility: a level never seen
// on a match gets a probability near zero, which is not "rare" but "cannot
// happen", and the level's weight goes to an extreme in one direction. Such a
// set can score genuinely and significantly better than the parameters in force
// on the narrow evidence it came from, and still stop the matcher doing the
// things this program is documented to do.
//
// No score buys that back, which is why the anchors are a separate check and not
// a term in one.
//
// The corpus below is an installation at a bank where a truncated payee means
// nothing — the field is cut so short that unrelated merchants collide in it, so
// every pair agreeing only on a truncation turns out to be two payments. The
// shipped parameters call those merges at better than 0.99 and are wrong every
// time, so a refit that learns it is a large, real improvement in Brier score and
// passes the calibration check on its own merits. It also stops the matcher
// recognising a truncating bank at all, which is a documented promise, and the
// anchors are what notice.
func TestPromotion_narrowEvidenceIsRefusedHoweverWellItScores(t *testing.T) {
	h := newHarness(t)
	id := h.addAccount(t, "")
	h.reloadState(t)

	// Three level combinations and hundreds of each, one of which contradicts the
	// parameters in force outright.
	settleDecisions(t, h, id, "m", 220, "exact", "exact", "same", true)
	settleDecisions(t, h, id, "u", 160, "none", "outside_higher", "after_far", false)
	settleDecisions(t, h, id, "t", 90, "truncated", "exact", "after_near", false)

	got, err := h.syncer.PromotionStatus(context.Background())
	if err != nil {
		t.Fatalf("PromotionStatus: %v", err)
	}
	if got.Verdict.Promotable() {
		t.Fatal("parameters fitted to three level combinations were promotable")
	}

	var calibration, anchors budget.Check
	for _, c := range got.Verdict.Checks {
		switch c.Name {
		case "calibration":
			calibration = c
		case "anchor cases":
			anchors = c
		}
	}
	if calibration.Status != budget.CheckPassed {
		t.Fatalf("the calibration check reported %q, so the anchors are not what refused this",
			calibration.Status)
	}
	if anchors.Status != budget.CheckFailed {
		t.Fatalf("the anchor check reported %q", anchors.Status)
	}
	t.Logf("refused on anchors while scoring well: %s", anchors.Detail)
}

// TestPromotion_aWatchedCandidateStillHasToPassItsChecks closes the gap between
// the two guards.
//
// Watching is a prerequisite for promotion, not a substitute for the checks.
// Without this, a candidate that fails its anchors could be promoted simply by
// having been watched first, and every finding on the page would be advice.
func TestPromotion_aWatchedCandidateStillHasToPassItsChecks(t *testing.T) {
	h := newHarness(t)
	id := h.addAccount(t, "")
	h.reloadState(t)
	ctx := context.Background()
	shipped := h.syncer.matchPolicy("").Version()

	settleDecisions(t, h, id, "m", 220, "exact", "exact", "same", true)
	settleDecisions(t, h, id, "u", 160, "none", "outside_higher", "after_far", false)
	settleDecisions(t, h, id, "t", 90, "truncated", "exact", "after_near", true)

	got, err := h.syncer.PromotionStatus(ctx)
	if err != nil {
		t.Fatalf("PromotionStatus: %v", err)
	}
	if got.Verdict.Promotable() {
		t.Fatal("this candidate was expected to fail its checks")
	}
	// Watching a candidate is allowed whatever its checks say — seeing what a bad
	// idea would have done is exactly what watching is for.
	if err := h.syncer.WatchTrial(ctx, got.Candidate); err != nil {
		t.Fatalf("WatchTrial: %v", err)
	}
	if err := h.syncer.PromoteTrial(ctx, got.Candidate); err == nil {
		t.Fatal("a watched candidate was promoted although its checks had failed")
	}
	if now := h.syncer.matchPolicy("").Version(); now != shipped {
		t.Errorf("the refused promotion changed the parameters anyway: %q", now)
	}
}

// TestSync_aSettledAuthorisationReachesTheRuleRunner covers the one thing the
// import loop decides and nothing else checks: which transactions are handed to
// the budget's own rule engine after a run.
//
// The list is built branch by branch as transactions are placed, and every
// branch had to be right for a user's categorisation rules to fire. Nothing
// asserted that until now — the gap was found by damaging the branch and
// watching the whole suite stay green.
//
// A settled authorisation is the case that matters. The row already existed, so
// it is not "added" and is easy to mistake for a row that needs no further
// attention; but its payee and amount have just been rewritten with the booked
// values, which is exactly when a rule matching on the payee should run.
func TestSync_aSettledAuthorisationReachesTheRuleRunner(t *testing.T) {
	h, _ := newRealHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)

	// Matches only the booked spelling, so it cannot fire on the authorisation.
	h.addRule(t, "rule-settled",
		`[{"field":"imported_payee","op":"contains","value":"VISA","type":"string"}]`,
		`[{"field":"notes","op":"set","value":"ruled","type":"string"}]`)

	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("auth-1", daysAgo(4), "42.00", "Hotel Berlin"),
	}})
	h.syncer.run()
	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("book-1", daysAgo(3), "42.00", "VISA Hotel Berlin"),
	}})
	h.syncer.run()

	txns := h.actualTxns(t)
	if len(txns) != 1 {
		t.Fatalf("got %d transactions, want 1 — the pair did not settle", len(txns))
	}
	if txns[0].Notes != "ruled" {
		t.Errorf("the settled row carries notes %q: it never reached the rule runner, so a "+
			"user's categorisation rules would not have fired on it", txns[0].Notes)
	}
}

// TestSync_anUnchangedRowIsNotSentThroughTheRulesAgain is the other direction.
//
// A row the bank re-offers unchanged has nothing new about it, and running the
// rules over it again is work that grows with the fetch window rather than with
// what arrived. It also risks re-applying an action a user has since undone by
// hand.
//
// The guard is the deduplication short circuit rather than anything in settle:
// a row already in the pending map is counted as skipped and never reaches the
// model at all. Sabotaging settle leaves this test green, which is correct — it
// is the earlier mechanism this pins.
func TestSync_anUnchangedRowIsNotSentThroughTheRulesAgain(t *testing.T) {
	h, _ := newRealHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("auth-1", daysAgo(3), "42.00", "Hotel Berlin"),
	}})
	h.syncer.run()

	// Added only now, so it can only fire on a later run — and only if that run
	// hands the unchanged row over again.
	h.addRule(t, "rule-late",
		`[{"field":"imported_payee","op":"contains","value":"Hotel","type":"string"}]`,
		`[{"field":"notes","op":"set","value":"ruled-again","type":"string"}]`)

	// The same authorisation, offered again exactly as before.
	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("auth-1", daysAgo(3), "42.00", "Hotel Berlin"),
	}})
	h.syncer.run()

	txns := h.actualTxns(t)
	if len(txns) != 1 {
		t.Fatalf("got %d transactions, want 1", len(txns))
	}
	if txns[0].Notes == "ruled-again" {
		t.Error("a row the bank re-offered unchanged was put through the rules a second time")
	}
}

// TestSync_gathersTheNonMatchSampleWithoutBeingAsked is the point of the whole
// mechanism.
//
// The u probabilities describe pairs that are not the same payment, and the two
// label sources in service can barely produce one: a bank reference only ever
// says "these two are one payment", and reviews only ever describe the narrow
// band. Left to those the u side stays on its stated priors however long an
// installation runs. Every candidate a transaction was weighed against and not
// paired with is a non-match, and counting those needs nobody to answer
// anything.
func TestSync_gathersTheNonMatchSampleWithoutBeingAsked(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)
	version := h.syncer.matchPolicy("").ClassificationVersion()

	if got, _ := h.st.LevelObservations(version); len(got) != 0 {
		t.Fatalf("a fresh installation already holds %d observations", len(got))
	}

	// Two authorisations that settle in the same run. A bank withdraws an
	// authorisation when it books it and goes on sending the ones still open, so
	// both of these leave the feed together and neither is spoken for — each
	// booking is then weighed against both, and the one it is not paired with is
	// a non-match.
	//
	// That withdrawal is the whole mechanism, and getting it wrong is how an
	// earlier version of this test passed against a feed no bank produces.
	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("", daysAgo(6), "42.00", "Hotel Berlin"),
		pendingTxnPayee("", daysAgo(6), "17.50", "Bahn Ticket"),
	}})
	h.syncer.run()
	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("", daysAgo(4), "43.50", "VISA Hotel Berlin"),
		bookedTxnPayee("", daysAgo(4), "17.50", "VISA Bahn Ticket"),
	}})
	h.syncer.run()

	if got := len(h.actualTxns(t)); got != 2 {
		t.Fatalf("got %d budget rows for two purchases: the pairs did not settle, so "+
			"whatever was sampled came from a scenario that did not work", got)
	}

	got, err := h.st.LevelObservations(version)
	if err != nil {
		t.Fatalf("LevelObservations: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no non-match pairs were sampled, so the u side can never move")
	}

	fields := map[string]int{}
	for _, o := range got {
		fields[o.Field] += o.Count
	}
	for _, f := range []string{"payee", "amount", "date"} {
		if fields[f] == 0 {
			t.Errorf("nothing was sampled for the %s field", f)
		}
	}
	// All three fields see the same pairs, so their totals have to agree.
	if fields["payee"] != fields["amount"] || fields["amount"] != fields["date"] {
		t.Errorf("the three fields disagree on how many pairs were seen: %v", fields)
	}
}

// TestSync_theSampleSurvivesRunsAndDiesWithItsParameters pins both halves of the
// tally's lifetime: it accumulates while the classification stands, and it is
// discarded when the rules that produced it change.
func TestSync_theSampleSurvivesRunsAndDiesWithItsParameters(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)
	version := h.syncer.matchPolicy("").ClassificationVersion()

	// Two authorisations, then both settling together — the shape that leaves an
	// unpaired candidate behind. Repeated, because the point is that the tally
	// adds up across runs rather than being rewritten by the latest one.
	settle := func(dayOpen, dayBook int, a, b string) {
		h.eb.setPages([][]map[string]any{{
			pendingTxnPayee("", daysAgo(dayOpen), "42.00", a),
			pendingTxnPayee("", daysAgo(dayOpen), "17.50", b),
		}})
		h.syncer.run()
		h.eb.setPages([][]map[string]any{{
			bookedTxnPayee("", daysAgo(dayBook), "43.50", "VISA "+a),
			bookedTxnPayee("", daysAgo(dayBook), "17.50", "VISA "+b),
		}})
		h.syncer.run()
	}
	settle(9, 7, "Hotel Berlin", "Bahn Ticket")
	after2 := totalObservations(t, h, version)
	settle(5, 3, "Kino Astor", "Baecker Sued")
	after3 := totalObservations(t, h, version)

	if !(after3 > after2) {
		t.Fatalf("the tally did not grow across runs: %d then %d", after2, after3)
	}

	// Changing the amount tolerance changes which level a pair reaches, so what
	// was counted is no longer what would be counted now.
	if err := h.st.SetSetting(store.SettingTolerancePercent, "5"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	moved := h.syncer.matchPolicy("").ClassificationVersion()
	if moved == version {
		t.Fatal("the tolerance change did not move the classification")
	}
	settle(2, 1, "Apotheke", "Tankstelle")

	if old := totalObservations(t, h, version); old != 0 {
		t.Errorf("%d observations under the retired classification survived", old)
	}
	if fresh := totalObservations(t, h, moved); fresh == 0 {
		t.Error("nothing was sampled under the new parameters")
	}
}

func totalObservations(t *testing.T, h *harness, version string) int {
	t.Helper()
	obs, err := h.st.LevelObservations(version)
	if err != nil {
		t.Fatalf("LevelObservations: %v", err)
	}
	n := 0
	for _, o := range obs {
		n += o.Count
	}
	return n
}

// promoteFittedParameters puts a parameter set into force the way the promotion
// page's last step does, and returns it.
func promoteFittedParameters(t *testing.T, h *harness) budget.Linkage {
	t.Helper()
	l := budget.DefaultLinkage()
	l.PayeeM = map[budget.PayeeLevel]float64{
		budget.PayeeExact: 0.34, budget.PayeeTruncated: 0.30, budget.PayeeFuzzy: 0.05,
		budget.PayeeSubset: 0.13, budget.PayeeConflict: 0.02, budget.PayeeNone: 0.02,
		budget.PayeeMissing: 0.14,
	}
	raw, err := budget.MarshalTrial(budget.Trial{Linkage: l, Calibration: budget.Identity()})
	if err != nil {
		t.Fatalf("MarshalTrial: %v", err)
	}
	if err := h.st.SetSetting(store.SettingPromotedTrial, string(raw)); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	return l
}

// TestSync_theLevelWeightGaugeFollowsAPromotion is the one thing that gauge
// exists for, and it did not do it.
//
// It reported log2(m/u) from the shipped parameters unconditionally, so an
// installation running a promoted set saw the weights of a model that was not
// deciding anything. The metric's whole justification is that it is constant
// until a refit moves it — a dashboard watching for that moment would have
// watched forever.
//
// Read through the real registration in initTelemetry and a real SDK reader, so
// this covers the wiring and not a reimplementation of it.
func TestSync_theLevelWeightGaugeFollowsAPromotion(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(previous) })

	h := newHarness(t)
	h.addAccount(t, "")
	h.reloadState(t)
	t.Cleanup(initTelemetry(h.syncer))

	weights := func() map[string]float64 {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		out := map[string]float64{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "bankingsync_match_level_weight" {
					continue
				}
				g, ok := m.Data.(metricdata.Gauge[float64])
				if !ok {
					t.Fatalf("the gauge came back as %T", m.Data)
				}
				for _, dp := range g.DataPoints {
					field, _ := dp.Attributes.Value(attribute.Key("field"))
					level, _ := dp.Attributes.Value(attribute.Key("level"))
					out[field.AsString()+"/"+level.AsString()] = dp.Value
				}
			}
		}
		return out
	}

	before := weights()
	if len(before) == 0 {
		t.Fatal("the gauge reported nothing at all")
	}
	shipped := budget.DefaultLinkage().LevelWeights()
	for _, e := range shipped {
		if got, ok := before[e.Field+"/"+e.Level]; !ok || got != e.Bits {
			t.Fatalf("before any promotion %s/%s reported %v, want the shipped %.4f",
				e.Field, e.Level, got, e.Bits)
		}
	}

	fitted := promoteFittedParameters(t, h)
	after := weights()

	if after["payee/truncated"] == before["payee/truncated"] {
		t.Fatalf("the gauge still reports %.4f bits for a truncated payee after a promotion "+
			"that moved it — a dashboard would never see the change",
			after["payee/truncated"])
	}
	for _, e := range fitted.LevelWeights() {
		if got := after[e.Field+"/"+e.Level]; got != e.Bits {
			t.Errorf("%s/%s reported %.4f, want %.4f from the set in force",
				e.Field, e.Level, got, e.Bits)
		}
	}
}

// TestSync_aPromotionKeepsTheNonMatchSample is the other half.
//
// The sample was scoped to the whole parameter version, and a promotion changes
// that version — so promoting discarded the very evidence that made the
// promotion assessable, and the next candidate could not be judged until it had
// rebuilt. Nothing about a promotion changes which comparison level a pair
// reaches, so nothing about it should touch the tally.
func TestSync_aPromotionKeepsTheNonMatchSample(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(9))
	h.reloadState(t)
	classification := h.syncer.matchPolicy("").ClassificationVersion()

	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("", daysAgo(4), "42.00", "Hotel Berlin"),
		pendingTxnPayee("", daysAgo(4), "17.50", "Bahn Ticket"),
		pendingTxnPayee("", daysAgo(4), "9.99", "Kino Astor"),
	}})
	h.syncer.run()
	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("", daysAgo(3), "42.00", "VISA Hotel Berlin"),
	}})
	h.syncer.run()

	gathered := totalObservations(t, h, classification)
	if gathered == 0 {
		t.Fatal("nothing was sampled, so this proves nothing")
	}

	before := h.syncer.matchPolicy("").Version()
	promoteFittedParameters(t, h)
	if h.syncer.matchPolicy("").Version() == before {
		t.Fatal("the promotion did not move the parameter version")
	}
	if moved := h.syncer.matchPolicy("").ClassificationVersion(); moved != classification {
		t.Fatalf("the promotion moved the classification from %s to %s; it changes what a "+
			"level is worth, not which level a pair falls into", classification, moved)
	}

	// A sync runs the retention, which is where the sample was being lost. The
	// feed is empty on purpose: a run that also gathered would hide a deletion
	// behind its own harvest, and the tally has to be exactly what it was.
	h.eb.setPages([][]map[string]any{{}})
	h.syncer.run()

	kept := totalObservations(t, h, classification)
	if kept != gathered {
		t.Errorf("the tally went from %d to %d across a promotion; nothing about a promotion "+
			"changes which level a pair reaches, so it must not touch the sample that made "+
			"the promotion assessable in the first place", gathered, kept)
	}
}

// TestSync_aStableBankReferenceLeavesNothingToSample is the counterpart, and it
// documents a limit rather than a feature.
//
// Where a bank keeps one reference from authorisation to booking, the pair is
// settled by that key before the model is consulted, and the only row ever
// withdrawn from the feed is the one being settled. Every window therefore holds
// exactly one candidate, and removing the one that was chosen leaves nothing.
//
// So an installation on such a feed can never estimate u. That is not a gap in
// the sampling: u describes how often two unrelated rows in a window agree by
// chance, and an account whose windows never hold two rows has no answer to give.
// It is also the account that needs one least — nothing there is ambiguous.
func TestSync_aStableBankReferenceLeavesNothingToSample(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(20))
	h.reloadState(t)
	classification := h.syncer.matchPolicy("").ClassificationVersion()

	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("auth-a", daysAgo(6), "42.00", "Hotel Berlin"),
		pendingTxnPayee("auth-b", daysAgo(6), "17.50", "Bahn Ticket"),
	}})
	h.syncer.run()
	// The same references on the bookings, which is what "stable" means.
	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("auth-a", daysAgo(4), "43.50", "VISA Hotel Berlin"),
		bookedTxnPayee("auth-b", daysAgo(4), "17.50", "VISA Bahn Ticket"),
	}})
	h.syncer.run()

	if got := len(h.actualTxns(t)); got != 2 {
		t.Fatalf("got %d budget rows for two purchases; the references should have settled "+
			"both without any guessing", got)
	}
	if got := totalObservations(t, h, classification); got != 0 {
		t.Errorf("sampled %d pairs from a feed whose references settle everything; every "+
			"window holds one candidate and it is the one that gets chosen", got)
	}
}

// TestSync_theModelIsMeasuredEvenWhenItIsNotConsulted is the answer to a
// question an operator on a well-behaved feed will ask: how is the matcher
// doing, when the bank's own key settles nearly everything before it is asked?
//
// match_probability is empty there by construction — it records what the
// thresholds cut through, and these pairs never reach a threshold. What the
// model would have said is computed anyway, against a pairing the bank has
// already confirmed, which is ground truth the model did not provide. That
// distribution is the only view of its accuracy such an installation gets.
func TestSync_theModelIsMeasuredEvenWhenItIsNotConsulted(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(previous) })

	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(20))
	h.reloadState(t)
	t.Cleanup(initTelemetry(h.syncer))

	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("auth-a", daysAgo(6), "42.00", "Hotel Berlin"),
	}})
	h.syncer.run()
	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("auth-a", daysAgo(4), "43.50", "VISA Hotel Berlin"),
	}})
	h.syncer.run()

	counts := histogramCounts(t, reader, "bankingsync_match_reference_probability")
	if len(counts) == 0 {
		t.Fatal("a pair the bank's reference settled produced no measurement of what the " +
			"model would have said; on this kind of feed there is no other source")
	}
	total := int64(0)
	for _, n := range counts {
		total += n
	}
	if total != 1 {
		t.Errorf("recorded %d observations for one settled pair", total)
	}
	if len(histogramCounts(t, reader, "bankingsync_match_probability")) != 0 {
		t.Error("the pair reached match_probability as well; that histogram is the " +
			"distribution the thresholds cut through, and this pair never met one")
	}
}

// TestSync_aWatchedCandidateIsCountedAsItGoes puts the promotion page's tally on
// a time axis.
//
// The page reports how many decisions a candidate would have changed. Whether
// those were spread evenly or fell in one unusual week is a different question
// and a different answer, and only a series can tell them apart.
func TestSync_aWatchedCandidateIsCountedAsItGoes(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(previous) })

	h := newHarness(t)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(20))
	h.reloadState(t)
	t.Cleanup(initTelemetry(h.syncer))

	if counted := counterTotal(t, reader, "bankingsync_match_shadow_decisions_total"); counted != 0 {
		t.Fatalf("%d decisions counted against a candidate before one was watched", counted)
	}

	watched := watchFittedParameters(t, h)

	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("", daysAgo(6), "42.00", "Hotel Berlin"),
		pendingTxnPayee("", daysAgo(6), "17.50", "Bahn Ticket"),
	}})
	h.syncer.run()
	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("", daysAgo(4), "43.50", "VISA Hotel Berlin"),
		bookedTxnPayee("", daysAgo(4), "17.50", "VISA Bahn Ticket"),
	}})
	h.syncer.run()

	byAgreement := counterByAttribute(t, reader, "bankingsync_match_shadow_decisions_total", "agreement")
	if len(byAgreement) == 0 {
		t.Fatal("nothing was counted against the watched candidate, so the page's tally has " +
			"no series behind it")
	}
	// The candidate is deliberately one that merges on a payee agreement the
	// shipped parameters treat as evidence against, so it has to disagree
	// somewhere. A counter that only ever says "same" is reporting that a
	// candidate is safe without having compared anything.
	if byAgreement["different"] == 0 {
		t.Errorf("every decision was counted as agreeing with a candidate built to disagree: %v",
			byAgreement)
	}
	if byAgreement["same"] == 0 {
		t.Errorf("no decision was counted as agreeing, which a candidate this close to the "+
			"shipped parameters cannot manage: %v", byAgreement)
	}

	// The candidate is named on every point. A counter does not reset when the
	// watch moves on, so without this two candidates' tallies would add into one
	// line that describes neither.
	byCandidate := counterByAttribute(t, reader, "bankingsync_match_shadow_decisions_total", "candidate")
	if byCandidate[watched] == 0 {
		t.Errorf("nothing is counted against the watched candidate %s: %v", watched, byCandidate)
	}
	if len(byCandidate) != 1 {
		t.Errorf("the tally is split across %d candidates while one was watched: %v",
			len(byCandidate), byCandidate)
	}
}

// watchFittedParameters starts a shadow evaluation the way the matching page's
// watch button does.
func watchFittedParameters(t *testing.T, h *harness) string {
	t.Helper()
	l := budget.DefaultLinkage()
	l.PayeeM = map[budget.PayeeLevel]float64{
		budget.PayeeExact: 0.20, budget.PayeeTruncated: 0.08, budget.PayeeFuzzy: 0.05,
		budget.PayeeSubset: 0.13, budget.PayeeConflict: 0.02, budget.PayeeNone: 0.37,
		budget.PayeeMissing: 0.15,
	}
	l.PayeeU = map[budget.PayeeLevel]float64{
		budget.PayeeExact: 0.050, budget.PayeeTruncated: 0.004, budget.PayeeFuzzy: 0.006,
		budget.PayeeSubset: 0.020, budget.PayeeConflict: 0.120, budget.PayeeNone: 0.001,
		budget.PayeeMissing: 0.799,
	}
	raw, err := budget.MarshalTrial(budget.Trial{Linkage: l, Calibration: budget.Identity()})
	if err != nil {
		t.Fatalf("MarshalTrial: %v", err)
	}
	if err := h.st.SetSetting(store.SettingShadowTrial, string(raw)); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	pol := h.syncer.matchPolicy("")
	if pol.Trial == nil {
		t.Fatal("the candidate was stored but the policy is not watching it")
	}
	return pol.Trial.Version(pol)
}

func counterByAttribute(t *testing.T, r *sdkmetric.ManualReader, name, key string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s came back as %T", name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				v, _ := dp.Attributes.Value(attribute.Key(key))
				out[v.AsString()] += dp.Value
			}
		}
	}
	return out
}

func histogramCounts(t *testing.T, r *sdkmetric.ManualReader, name string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s came back as %T", name, m.Data)
			}
			for _, dp := range h.DataPoints {
				out[dp.Attributes.Encoded(attribute.DefaultEncoder())] += int64(dp.Count)
			}
		}
	}
	return out
}

func counterTotal(t *testing.T, r *sdkmetric.ManualReader, name string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	total := int64(0)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s came back as %T", name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
		}
	}
	return total
}

// withObservers binds the observable gauges to a manual reader, which is the
// only way they can be exercised at all: a gauge is its callback, and a callback
// nothing collects from is indistinguishable from one that was never registered.
func withObservers(t *testing.T, h *harness) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	meter := provider.Meter("bankingsync")
	h.syncer.met = newSyncMetrics(meter)
	h.syncer.registerObservers(meter)
	return reader
}

// TestObservability_brierReportsEveryComponentItNeedsToAddUp is the check that
// the decomposition on a dashboard is the decomposition in the code.
//
// Murphy's three-way identity holds only when you stratify on the distinct
// forecast values. This bins by width and uses each bin's mean, so two further
// terms appear and reliability − resolution + uncertainty is out by an amount
// that depends on nothing but the bin count. A panel plotting the parts against
// the whole has to be able to close, and it can only do that if the parts are
// all published.
func TestObservability_brierReportsEveryComponentItNeedsToAddUp(t *testing.T) {
	h := newHarness(t)
	id := h.addAccount(t, "")
	h.reloadState(t)
	// A spread of weights, so that several fall in each bin and the within-bin
	// terms are not zero. See settleWeighted for why that is the whole point.
	var matched, unmatched []float64
	for i := 0; i < 90; i++ {
		matched = append(matched, -1.5+float64(i)*0.09)
	}
	for i := 0; i < 70; i++ {
		unmatched = append(unmatched, -4.0+float64(i)*0.08)
	}
	settleWeighted(t, h, id, "m", matched, true)
	settleWeighted(t, h, id, "u", unmatched, false)

	reader := withObservers(t, h)
	got := collectValues(t, reader, "bankingsync_match_brier_score", "component")
	if len(got) == 0 {
		t.Fatal("no Brier components at all")
	}
	for _, want := range []string{"score", "reliability", "resolution", "uncertainty",
		"within_bin_variance", "within_bin_covariance", "generalised_resolution"} {
		if _, ok := got[want]; !ok {
			t.Errorf("no %q component; got %v", want, keysOf(got))
		}
	}

	three := got["reliability"] - got["resolution"] + got["uncertainty"]
	five := three + got["within_bin_variance"] - got["within_bin_covariance"]
	viaGRES := got["reliability"] - got["generalised_resolution"] + got["uncertainty"]
	t.Logf("score %.6f, three-way %.6f, five-way %.6f, via generalised resolution %.6f",
		got["score"], three, five, viaGRES)

	if math.Abs(five-got["score"]) > 1e-9 {
		t.Errorf("the five published components sum to %.9f and the score is %.9f",
			five, got["score"])
	}
	if math.Abs(viaGRES-got["score"]) > 1e-9 {
		t.Errorf("reliability − generalised_resolution + uncertainty is %.9f and the "+
			"score is %.9f", viaGRES, got["score"])
	}
}

// TestObservability_theGateIsReportedWithoutBeingRun is the property that keeps
// this series affordable.
//
// Reaching a verdict refits the linkage and fits a Platt calibration twice, runs
// six anchors through the real decision function and takes a significance test.
// That is fine on a page load and would not be fine on every collection, so the
// gauge reads what the last real evaluation concluded. Before there has been one
// it reports nothing, which is honest: a zero p-value would read as overwhelming
// significance.
func TestObservability_theGateIsReportedWithoutBeingRun(t *testing.T) {
	h, _ := evidenced(t)
	reader := withObservers(t, h)

	if got := collectValues(t, reader, "bankingsync_match_gate", "figure"); len(got) != 0 {
		t.Errorf("the gate reported %v before anything had evaluated it", got)
	}

	if _, err := h.syncer.PromotionStatus(context.Background()); err != nil {
		t.Fatalf("PromotionStatus: %v", err)
	}

	got := collectValues(t, reader, "bankingsync_match_gate", "figure")
	if len(got) == 0 {
		t.Fatal("the gate ran and reported nothing")
	}
	for _, want := range []string{"labelled", "holdout", "base_brier", "trial_brier",
		"skill_percent", "p_value", "significance_level", "statistic"} {
		if _, ok := got[want]; !ok {
			t.Errorf("no %q figure; got %v", want, keysOf(got))
		}
	}
	t.Logf("skill %+.2f%%, p = %.4f against a bar of %.4f, on %.0f of %.0f decisions",
		got["skill_percent"], got["p_value"], got["significance_level"],
		got["holdout"], got["labelled"])

	if got["p_value"] < 0 || got["p_value"] > 1 {
		t.Errorf("p = %v, which is not a probability", got["p_value"])
	}
	if got["significance_level"] <= 0 || got["significance_level"] > 0.05 {
		t.Errorf("the bar is %v; it must be positive and no looser than the stated level",
			got["significance_level"])
	}

	// And each check separately, because they fail for unrelated reasons.
	checks := collectValues(t, reader, "bankingsync_match_gate_check", "check")
	for _, want := range []string{"anchor cases", "calibration", "changed decisions", "overall"} {
		if _, ok := checks[want]; !ok {
			t.Errorf("no standing published for check %q; got %v", want, keysOf(checks))
		}
	}
}

// TestObservability_levelObservationsSayWhatRestsOnEvidence is the series that
// separates a parameter from a guess.
//
// match_level_weight says what a level is worth. It says the same thing on an
// installation that has settled a thousand decisions and on one that has settled
// none, because until a promotion the weights are the shipped ones either way.
// This is the series that tells those two apart.
func TestObservability_levelObservationsSayWhatRestsOnEvidence(t *testing.T) {
	h := newHarness(t)
	id := h.addAccount(t, "")
	h.reloadState(t)
	reader := withObservers(t, h)

	for k, v := range collectBy(t, reader, "bankingsync_match_level_observations",
		"field", "side", "level") {
		if v != 0 {
			t.Fatalf("a fresh installation reported %v observations at %s", v, k)
		}
	}

	settleDecisions(t, h, id, "m", 60, "truncated", "exact", "after_near", true)

	got := collectBy(t, reader, "bankingsync_match_level_observations", "field", "side", "level")
	for key, want := range map[string]float64{
		"payee/m/truncated": 60,
		"amount/m/exact":    60,
		"date/m/after_near": 60,
	} {
		if got[key] != want {
			t.Errorf("%s reports %v observations, want %v", key, got[key], want)
		}
	}
	// A level nobody has seen has to be reported as nothing rather than left out,
	// or "no evidence" and "no series" become the same picture. So must the whole
	// u side, which these labels say nothing about.
	for _, key := range []string{"payee/m/fuzzy", "payee/u/truncated", "date/m/same"} {
		v, ok := got[key]
		if !ok || v != 0 {
			t.Errorf("%s reports %v (present=%v); it has to say zero", key, v, ok)
		}
	}
}

// TestObservability_thePolicyInForceIsASeries closes the gap that made every
// other matching series hard to read: a step in any of them is most often a
// threshold move, and without this it has to be correlated against a log line.
func TestObservability_thePolicyInForceIsASeries(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	h.reloadState(t)
	reader := withObservers(t, h)

	got := collectValues(t, reader, "bankingsync_match_policy", "setting")
	for want, expect := range map[string]float64{
		"auto_probability":   float64(store.DefaultAutoProbability) / 100,
		"review_probability": float64(store.DefaultReviewProbability) / 100,
		"overlap":            float64(store.DefaultMatchOverlap) / 100,
	} {
		if math.Abs(got[want]-expect) > 1e-9 {
			t.Errorf("%s reports %v, want %v", want, got[want], expect)
		}
	}

	if err := h.st.SetSetting(store.SettingMatchOverlap, "65"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if got := collectValues(t, reader, "bankingsync_match_policy", "setting"); math.Abs(got["overlap"]-0.65) > 1e-9 {
		t.Errorf("after the setting moved the series reports %v, want 0.65", got["overlap"])
	}
}

// TestObservability_theCalibrationInForceIsVisible covers the failure that used
// to be silent by construction.
//
// A Platt fit that gives up returns the identity, and an identity is exactly what
// an installation that never fitted one is running. Without this series the two
// are the same picture, which is the state the solver rewrite was meant to end.
func TestObservability_theCalibrationInForceIsVisible(t *testing.T) {
	h := newHarness(t)
	h.addAccount(t, "")
	h.reloadState(t)
	reader := withObservers(t, h)

	got := collectValues(t, reader, "bankingsync_match_calibration_coefficient", "coefficient")
	if got["slope"] != 1 || got["intercept"] != 0 {
		t.Errorf("an unfitted installation reports A=%v B=%v, want the identity",
			got["slope"], got["intercept"])
	}

	raw, err := budget.MarshalTrial(budget.Trial{
		Linkage:     budget.DefaultLinkage(),
		Calibration: budget.Calibration{A: 0.42, B: -1.3},
	})
	if err != nil {
		t.Fatalf("MarshalTrial: %v", err)
	}
	if err := h.st.SetSetting(store.SettingPromotedTrial, string(raw)); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	got = collectValues(t, reader, "bankingsync_match_calibration_coefficient", "coefficient")
	if math.Abs(got["slope"]-0.42) > 1e-9 || math.Abs(got["intercept"]+1.3) > 1e-9 {
		t.Errorf("a promoted calibration reports A=%v B=%v, want 0.42 and -1.3",
			got["slope"], got["intercept"])
	}
}

// collectBy is collectValues with a compound key, for series whose identity
// needs more than one attribute — a level means nothing without the field and
// the side it belongs to.
func collectBy(t *testing.T, reader *sdkmetric.ManualReader, name string, keys ...string) map[string]float64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	out := map[string]float64{}
	join := func(set attribute.Set) string {
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = attrOf(set, k)
		}
		return strings.Join(parts, "/")
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Gauge[int64]:
				for _, dp := range d.DataPoints {
					out[join(dp.Attributes)] = float64(dp.Value)
				}
			case metricdata.Gauge[float64]:
				for _, dp := range d.DataPoints {
					out[join(dp.Attributes)] = dp.Value
				}
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					out[join(dp.Attributes)] += float64(dp.Value)
				}
			default:
				t.Fatalf("%s is %T, which this helper does not know", name, m.Data)
			}
		}
	}
	return out
}

// settleWeighted writes settled decisions carrying the weight the model actually
// gave them, which settleDecisions does not.
//
// It matters here and only here. Every other test reads levels back; this one
// reads probabilities, and a corpus of identical weights makes every forecast a
// half — under which the within-bin terms are zero and the three-component Brier
// decomposition is accidentally exact. A test built on that would pass whether
// the extra components were published or not.
func settleWeighted(t *testing.T, h *harness, id int64, prefix string,
	weights []float64, match bool) {
	t.Helper()
	day := time.Now().UTC().Format("2006-01-02")
	for i, w := range weights {
		key := fmt.Sprintf("%s-%d", prefix, i)
		if err := h.st.AddMatchDecision(store.MatchDecision{
			RunID: "r", BankAccountID: id, Bank: "TestBank", PendingKey: key,
			Outcome: "adopted", ParamVersion: "v1", TxnDate: day, Candidates: 2,
			PayeeLevel: "exact", AmountLevel: "exact", DateLevel: "same",
			Weight: w, Probability: budget.Probability(w),
		}); err != nil {
			t.Fatalf("AddMatchDecision: %v", err)
		}
		if err := h.st.SetMatchDecisionTruth(id, key, match); err != nil {
			t.Fatalf("SetMatchDecisionTruth: %v", err)
		}
	}
}

func keysOf(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
