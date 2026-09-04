package store_test

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bankingsync/store"

	_ "modernc.org/sqlite"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// --- Open -------------------------------------------------------------------

func TestOpen_idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st1, err := store.Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	_ = st1.Close()
	st2, err := store.Open(path)
	if err != nil {
		t.Fatalf("second open (idempotent): %v", err)
	}
	_ = st2.Close()
}

// --- Settings ---------------------------------------------------------------

func TestGetSetting_missingKey(t *testing.T) {
	st := openTestStore(t)
	v, err := st.GetSetting("nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "" {
		t.Errorf("expected empty string, got %q", v)
	}
}

func TestSetSetting_andGet(t *testing.T) {
	st := openTestStore(t)
	if err := st.SetSetting("foo", "bar"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	got, err := st.GetSetting("foo")
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if got != "bar" {
		t.Errorf("got %q, want bar", got)
	}
}

func TestSetSetting_overwrite(t *testing.T) {
	st := openTestStore(t)
	_ = st.SetSetting("k", "first")
	_ = st.SetSetting("k", "second")
	got, _ := st.GetSetting("k")
	if got != "second" {
		t.Errorf("got %q, want second", got)
	}
}

// --- LastSyncDate -----------------------------------------------------------

func TestGetLastSyncDate_emptyOnNew(t *testing.T) {
	st := openTestStore(t)
	d, err := st.GetLastSyncDate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != "" {
		t.Errorf("expected empty, got %q", d)
	}
}

func TestSetLastSyncDate_andGet(t *testing.T) {
	st := openTestStore(t)
	if err := st.SetLastSyncDate("2024-06-01"); err != nil {
		t.Fatalf("SetLastSyncDate: %v", err)
	}
	got, err := st.GetLastSyncDate()
	if err != nil {
		t.Fatalf("GetLastSyncDate: %v", err)
	}
	if got != "2024-06-01" {
		t.Errorf("got %q, want 2024-06-01", got)
	}
}

// --- BankAccounts -----------------------------------------------------------

func TestGetAllBankAccounts_emptyOnNew(t *testing.T) {
	st := openTestStore(t)
	accounts, err := st.GetAllBankAccounts()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(accounts) != 0 {
		t.Errorf("expected 0 accounts, got %d", len(accounts))
	}
}

func TestAddBankAccount_andGetAll(t *testing.T) {
	st := openTestStore(t)
	id, err := st.AddBankAccount(store.NewBankAccount{SessionID: "sess-1", AccountUID: "acct-1", BankName: "TestBank", BankCountry: "DE", SessionExpiry: "2025-01-01T00:00:00Z"})
	if err != nil {
		t.Fatalf("AddBankAccount: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero ID")
	}
	accounts, err := st.GetAllBankAccounts()
	if err != nil {
		t.Fatalf("GetAllBankAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("expected 1, got %d", len(accounts))
	}
	a := accounts[0]
	if a.ID != id {
		t.Errorf("ID: got %d, want %d", a.ID, id)
	}
	if a.SessionID != "sess-1" {
		t.Errorf("SessionID: got %q", a.SessionID)
	}
	if a.AccountUID != "acct-1" {
		t.Errorf("AccountUID: got %q", a.AccountUID)
	}
	if a.BankName != "TestBank" {
		t.Errorf("BankName: got %q", a.BankName)
	}
	if a.BankCountry != "DE" {
		t.Errorf("BankCountry: got %q", a.BankCountry)
	}
	if a.SessionExpiry != "2025-01-01T00:00:00Z" {
		t.Errorf("SessionExpiry: got %q", a.SessionExpiry)
	}
}

func TestGetAllBankAccounts_orderedByCreation(t *testing.T) {
	st := openTestStore(t)
	_, _ = st.AddBankAccount(store.NewBankAccount{SessionID: "s1", AccountUID: "a1", BankName: "Bank1", BankCountry: "DE", SessionExpiry: "2025-01-01T00:00:00Z"})
	_, _ = st.AddBankAccount(store.NewBankAccount{SessionID: "s2", AccountUID: "a2", BankName: "Bank2", BankCountry: "FR", SessionExpiry: "2025-01-01T00:00:00Z"})
	accounts, _ := st.GetAllBankAccounts()
	if len(accounts) != 2 {
		t.Fatalf("expected 2, got %d", len(accounts))
	}
	if accounts[0].BankName != "Bank1" {
		t.Errorf("first should be Bank1, got %q", accounts[0].BankName)
	}
	if accounts[1].BankName != "Bank2" {
		t.Errorf("second should be Bank2, got %q", accounts[1].BankName)
	}
}

func TestUpdateBankAccountSession(t *testing.T) {
	st := openTestStore(t)
	id, _ := st.AddBankAccount(store.NewBankAccount{SessionID: "old-sess", AccountUID: "acct", BankName: "Bank", BankCountry: "DE", SessionExpiry: "2025-01-01T00:00:00Z"})
	if err := st.UpdateBankAccountSession(id, "new-sess", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("UpdateBankAccountSession: %v", err)
	}
	accounts, _ := st.GetAllBankAccounts()
	if accounts[0].SessionID != "new-sess" {
		t.Errorf("SessionID: got %q, want new-sess", accounts[0].SessionID)
	}
	if accounts[0].SessionExpiry != "2026-01-01T00:00:00Z" {
		t.Errorf("SessionExpiry: got %q", accounts[0].SessionExpiry)
	}
}

func TestRemoveBankAccount(t *testing.T) {
	st := openTestStore(t)
	id, _ := st.AddBankAccount(store.NewBankAccount{SessionID: "sess", AccountUID: "acct", BankName: "Bank", BankCountry: "DE", SessionExpiry: "2025-01-01T00:00:00Z"})
	if err := st.RemoveBankAccount(id); err != nil {
		t.Fatalf("RemoveBankAccount: %v", err)
	}
	accounts, _ := st.GetAllBankAccounts()
	if len(accounts) != 0 {
		t.Errorf("expected 0 after remove, got %d", len(accounts))
	}
}

// --- ImportedRefs -----------------------------------------------------------

func TestHasImportedRef_falseOnNew(t *testing.T) {
	st := openTestStore(t)
	has, err := st.HasImportedRef(1, "REF-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if has {
		t.Error("expected false for empty DB")
	}
}

func TestAddImportedRef_andHas(t *testing.T) {
	st := openTestStore(t)
	if err := st.AddImportedRef(1, "REF-1", "2024-06-01"); err != nil {
		t.Fatalf("AddImportedRef: %v", err)
	}
	has, err := st.HasImportedRef(1, "REF-1")
	if err != nil {
		t.Fatalf("HasImportedRef: %v", err)
	}
	if !has {
		t.Error("expected true after add")
	}
}

func TestAddImportedRef_upsert(t *testing.T) {
	st := openTestStore(t)
	_ = st.AddImportedRef(1, "REF-1", "2024-01-01")
	if err := st.AddImportedRef(1, "REF-1", "2024-02-01"); err != nil {
		t.Fatalf("duplicate add should not error: %v", err)
	}
	all, _ := st.AllImportedRefs()
	refs := all[1]
	if refs["REF-1"] != "2024-02-01" {
		t.Errorf("expected updated date, got %q", refs["REF-1"])
	}
}

func TestAllImportedRefs(t *testing.T) {
	st := openTestStore(t)
	_ = st.AddImportedRef(1, "A", "2024-01-01")
	_ = st.AddImportedRef(1, "B", "2024-02-01")
	refsAll, err := st.AllImportedRefs()
	if err != nil {
		t.Fatalf("AllImportedRefs: %v", err)
	}
	refs := refsAll[1]
	if len(refs) != 2 {
		t.Errorf("expected 2, got %d", len(refs))
	}
	if refs["A"] != "2024-01-01" {
		t.Errorf("A: got %q", refs["A"])
	}
	if refs["B"] != "2024-02-01" {
		t.Errorf("B: got %q", refs["B"])
	}
}

func TestPruneImportedRefs_keepsRecent(t *testing.T) {
	st := openTestStore(t)
	recent := time.Now().UTC().AddDate(0, 0, -5).Format("2006-01-02")
	_ = st.AddImportedRef(1, "recent", recent)
	updatedAll, err := st.PruneImportedRefs()
	if err != nil {
		t.Fatalf("PruneImportedRefs: %v", err)
	}
	updated := updatedAll[1]
	if _, ok := updated["recent"]; !ok {
		t.Error("expected recent ref to be kept")
	}
}

func TestPruneImportedRefs_removesOld(t *testing.T) {
	st := openTestStore(t)
	old := time.Now().UTC().AddDate(0, 0, -(store.RetentionDays + 5)).Format("2006-01-02")
	_ = st.AddImportedRef(1, "old", old)
	updatedAll, err := st.PruneImportedRefs()
	if err != nil {
		t.Fatalf("PruneImportedRefs: %v", err)
	}
	updated := updatedAll[1]
	if _, ok := updated["old"]; ok {
		t.Error("expected old ref to be pruned")
	}
}

func TestPruneImportedRefs_boundaryIsInclusive(t *testing.T) {
	st := openTestStore(t)
	exactly := time.Now().UTC().AddDate(0, 0, -store.RetentionDays).Format("2006-01-02")
	_ = st.AddImportedRef(1, "boundary", exactly)
	updatedAll, err := st.PruneImportedRefs()
	if err != nil {
		t.Fatalf("PruneImportedRefs: %v", err)
	}
	updated := updatedAll[1]
	if _, ok := updated["boundary"]; !ok {
		t.Error("a ref exactly at the retention boundary must be kept")
	}
}

func TestPruneImportedRefs_mixed(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC()
	_ = st.AddImportedRef(1, "old", now.AddDate(0, 0, -(store.RetentionDays+5)).Format("2006-01-02"))
	_ = st.AddImportedRef(1, "recent", now.AddDate(0, 0, -5).Format("2006-01-02"))
	updatedAll, err := st.PruneImportedRefs()
	if err != nil {
		t.Fatalf("PruneImportedRefs: %v", err)
	}
	updated := updatedAll[1]
	if _, ok := updated["old"]; ok {
		t.Error("old should be pruned")
	}
	if _, ok := updated["recent"]; !ok {
		t.Error("recent should be kept")
	}
}

// --- PendingMap -------------------------------------------------------------

func TestGetPendingTxnID_notFound(t *testing.T) {
	st := openTestStore(t)
	_, ok, err := st.GetPendingTxnID(1, "missing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected not found")
	}
}

func TestSetPending_andGet(t *testing.T) {
	st := openTestStore(t)
	if err := st.SetPending(1, "key-1", "txn-abc"); err != nil {
		t.Fatalf("SetPending: %v", err)
	}
	id, ok, err := st.GetPendingTxnID(1, "key-1")
	if err != nil {
		t.Fatalf("GetPendingTxnID: %v", err)
	}
	if !ok {
		t.Error("expected found=true")
	}
	if id != "txn-abc" {
		t.Errorf("got %q, want txn-abc", id)
	}
}

func TestSetPending_upsert(t *testing.T) {
	st := openTestStore(t)
	_ = st.SetPending(1, "k", "v1")
	_ = st.SetPending(1, "k", "v2")
	id, _, _ := st.GetPendingTxnID(1, "k")
	if id != "v2" {
		t.Errorf("got %q, want v2 after upsert", id)
	}
}

func TestDeletePending(t *testing.T) {
	st := openTestStore(t)
	_ = st.SetPending(1, "k", "v")
	if err := st.DeletePending(1, "k"); err != nil {
		t.Fatalf("DeletePending: %v", err)
	}
	_, ok, _ := st.GetPendingTxnID(1, "k")
	if ok {
		t.Error("expected not found after delete")
	}
}

func TestDeletePending_nonexistent(t *testing.T) {
	st := openTestStore(t)
	if err := st.DeletePending(1, "ghost"); err != nil {
		t.Fatalf("deleting nonexistent key should not error: %v", err)
	}
}

func TestAllPendingMap_empty(t *testing.T) {
	st := openTestStore(t)
	m, err := st.AllPendingMap()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("expected empty map, got %v", m)
	}
}

func TestAllPendingMap(t *testing.T) {
	st := openTestStore(t)
	_ = st.SetPending(1, "k1", "v1")
	_ = st.SetPending(1, "k2", "v2")
	mAll, err := st.AllPendingMap()
	if err != nil {
		t.Fatalf("AllPendingMap: %v", err)
	}
	m := mAll[1]
	if len(m) != 2 {
		t.Errorf("expected 2, got %d", len(m))
	}
	if m["k1"] != "v1" {
		t.Errorf("k1: got %q", m["k1"])
	}
	if m["k2"] != "v2" {
		t.Errorf("k2: got %q", m["k2"])
	}
}

func TestAddBankAccount_actualAccountRoundtrip(t *testing.T) {
	st := openTestStore(t)
	_, _ = st.AddBankAccount(store.NewBankAccount{SessionID: "sess", AccountUID: "acct", BankName: "Bank", BankCountry: "DE", ActualAccount: "MyChecking", SessionExpiry: "2025-01-01T00:00:00Z"})
	accounts, _ := st.GetAllBankAccounts()
	if accounts[0].ActualAccount != "MyChecking" {
		t.Errorf("ActualAccount: got %q, want MyChecking", accounts[0].ActualAccount)
	}
}

func TestAddBankAccount_startSyncDateRoundtrip(t *testing.T) {
	st := openTestStore(t)
	_, _ = st.AddBankAccount(store.NewBankAccount{SessionID: "sess", AccountUID: "acct", BankName: "Bank", BankCountry: "DE", StartSyncDate: "2025-03-01", SessionExpiry: "2025-01-01T00:00:00Z"})
	accounts, _ := st.GetAllBankAccounts()
	if accounts[0].StartSyncDate != "2025-03-01" {
		t.Errorf("StartSyncDate: got %q, want 2025-03-01", accounts[0].StartSyncDate)
	}
}

func TestUpdateBankAccountStartDate(t *testing.T) {
	st := openTestStore(t)
	id, _ := st.AddBankAccount(store.NewBankAccount{SessionID: "sess", AccountUID: "acct", BankName: "Bank", BankCountry: "DE", SessionExpiry: "2025-01-01T00:00:00Z"})
	if err := st.UpdateBankAccountStartDate(id, "2025-06-15"); err != nil {
		t.Fatalf("UpdateBankAccountStartDate: %v", err)
	}
	accounts, _ := st.GetAllBankAccounts()
	if accounts[0].StartSyncDate != "2025-06-15" {
		t.Errorf("StartSyncDate: got %q, want 2025-06-15", accounts[0].StartSyncDate)
	}
}

func TestAddSyncLog_andGetLogs(t *testing.T) {
	st := openTestStore(t)
	id, err := st.AddSyncLog("success", 5, 2, 3, 1.5, "")
	if err != nil {
		t.Fatalf("AddSyncLog: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero ID")
	}
	logs, err := st.GetSyncLogs(10)
	if err != nil {
		t.Fatalf("GetSyncLogs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1, got %d", len(logs))
	}
	if logs[0].Status != "success" {
		t.Errorf("Status: got %q", logs[0].Status)
	}
	if logs[0].TxAdded != 5 {
		t.Errorf("TxAdded: got %d", logs[0].TxAdded)
	}
	if logs[0].TxConfirmed != 2 {
		t.Errorf("TxConfirmed: got %d", logs[0].TxConfirmed)
	}
}

func TestGetSyncLogs_respectsLimit(t *testing.T) {
	st := openTestStore(t)
	for i := 0; i < 5; i++ {
		_, _ = st.AddSyncLog("success", i, 0, 0, 0.1, "")
	}
	logs, _ := st.GetSyncLogs(3)
	if len(logs) != 3 {
		t.Errorf("expected 3, got %d", len(logs))
	}
}

func TestGetSyncLogs_emptyOnNew(t *testing.T) {
	st := openTestStore(t)
	logs, err := st.GetSyncLogs(10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(logs) != 0 {
		t.Errorf("expected 0, got %d", len(logs))
	}
}

func TestGetLatestSyncLog_returnsNewest(t *testing.T) {
	st := openTestStore(t)
	_, _ = st.AddSyncLog("success", 1, 0, 0, 0.1, "first")
	_, _ = st.AddSyncLog("error", 0, 0, 0, 0.2, "second")
	l, err := st.GetLatestSyncLog()
	if err != nil {
		t.Fatalf("GetLatestSyncLog: %v", err)
	}
	if l == nil {
		t.Fatal("expected non-nil")
	}
	if l.Status != "error" {
		t.Errorf("Status: got %q, want error", l.Status)
	}
	if l.Message != "second" {
		t.Errorf("Message: got %q, want second", l.Message)
	}
}

func TestGetLatestSyncLog_nilOnEmpty(t *testing.T) {
	st := openTestStore(t)
	l, err := st.GetLatestSyncLog()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if l != nil {
		t.Errorf("expected nil, got %+v", l)
	}
}

func TestMigration_legacyUnscopedTablesAreAdoptedByOldestAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err = raw.Exec(`
		CREATE TABLE bank_accounts (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id     TEXT NOT NULL,
			account_uid    TEXT NOT NULL,
			bank_name      TEXT NOT NULL,
			bank_country   TEXT NOT NULL,
			actual_account  TEXT NOT NULL DEFAULT '',
			start_sync_date TEXT NOT NULL DEFAULT '',
			session_expiry  TEXT NOT NULL DEFAULT '',
			created_at     TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE imported_refs (ref TEXT PRIMARY KEY, date TEXT NOT NULL);
		CREATE TABLE pending_map (key TEXT PRIMARY KEY, txn_id TEXT NOT NULL);
		INSERT INTO bank_accounts (session_id, account_uid, bank_name, bank_country, created_at)
			VALUES ('s1', 'uid-old', 'Revolut', 'DE', '2026-03-01 10:00:00');
		INSERT INTO bank_accounts (session_id, account_uid, bank_name, bank_country, created_at)
			VALUES ('s2', 'uid-new', 'ING', 'DE', '2026-07-01 10:00:00');
		INSERT INTO imported_refs (ref, date) VALUES ('legacy-ref', '2026-07-20');
		INSERT INTO pending_map (key, txn_id) VALUES ('legacy-key', 'txn-legacy|2026-07-20');
	`)
	if err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open must migrate the legacy schema: %v", err)
	}
	defer st.Close()

	accounts, err := st.GetAllBankAccounts()
	if err != nil {
		t.Fatalf("GetAllBankAccounts: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("got %d accounts, want 2", len(accounts))
	}
	oldest := accounts[0].ID
	if accounts[0].BankName != "Revolut" {
		t.Fatalf("oldest account: got %q, want Revolut", accounts[0].BankName)
	}

	refs, err := st.AllImportedRefs()
	if err != nil {
		t.Fatalf("AllImportedRefs: %v", err)
	}
	if refs[oldest]["legacy-ref"] != "2026-07-20" {
		t.Errorf("legacy ref not adopted by the oldest account: %v", refs)
	}

	pending, err := st.AllPendingMap()
	if err != nil {
		t.Fatalf("AllPendingMap: %v", err)
	}
	if pending[oldest]["legacy-key"] != "txn-legacy|2026-07-20" {
		t.Errorf("legacy pending not adopted by the oldest account: %v", pending)
	}

	if err := st.AddImportedRef(accounts[1].ID, "legacy-ref", "2026-07-21"); err != nil {
		t.Fatalf("the same ref must be insertable for a second account: %v", err)
	}
}

func TestMigration_isIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idem.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := st.AddImportedRef(1, "r1", "2026-07-20"); err != nil {
		t.Fatalf("add ref: %v", err)
	}
	_ = st.Close()

	st2, err := store.Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer st2.Close()

	refs, _ := st2.AllImportedRefs()
	if refs[1]["r1"] != "2026-07-20" {
		t.Errorf("data lost on re-open: %v", refs)
	}
}

func TestRemoveBankAccount_clearsItsScopedSyncState(t *testing.T) {
	st := openTestStore(t)
	keep, err := st.AddBankAccount(store.NewBankAccount{SessionID: "s1", AccountUID: "uid-a", BankName: "BankA", BankCountry: "DE", ActualAccount: "Checking", SessionExpiry: "2027-01-01T00:00:00Z"})
	if err != nil {
		t.Fatalf("add account A: %v", err)
	}
	drop, err := st.AddBankAccount(store.NewBankAccount{SessionID: "s2", AccountUID: "uid-b", BankName: "BankB", BankCountry: "DE", ActualAccount: "Savings", SessionExpiry: "2027-01-01T00:00:00Z"})
	if err != nil {
		t.Fatalf("add account B: %v", err)
	}

	if err := st.AddImportedRef(keep, "ref-keep", "2026-07-20"); err != nil {
		t.Fatalf("ref keep: %v", err)
	}
	if err := st.AddImportedRef(drop, "ref-drop", "2026-07-20"); err != nil {
		t.Fatalf("ref drop: %v", err)
	}
	if err := st.SetPending(drop, "pend-drop", "txn-x|2026-07-20"); err != nil {
		t.Fatalf("pending drop: %v", err)
	}
	if err := st.SetPending(keep, "pend-keep", "txn-y|2026-07-20"); err != nil {
		t.Fatalf("pending keep: %v", err)
	}

	if err := st.RemoveBankAccount(drop); err != nil {
		t.Fatalf("RemoveBankAccount: %v", err)
	}

	refs, _ := st.AllImportedRefs()
	if len(refs[drop]) != 0 {
		t.Errorf("removed account left orphaned refs: %v", refs[drop])
	}
	if refs[keep]["ref-keep"] != "2026-07-20" {
		t.Error("removing one account must not touch another account's refs")
	}

	pending, _ := st.AllPendingMap()
	if len(pending[drop]) != 0 {
		t.Errorf("removed account left orphaned pending entries: %v", pending[drop])
	}
	if pending[keep]["pend-keep"] == "" {
		t.Error("removing one account must not touch another account's pending entries")
	}
}

func TestAddBankAccount_ibanAndCurrencyRoundtrip(t *testing.T) {
	st := openTestStore(t)
	_, err := st.AddBankAccount(store.NewBankAccount{
		SessionID: "s", AccountUID: "a", BankName: "Bank", BankCountry: "DE",
		SessionExpiry: "2027-01-01T00:00:00Z",
		IBAN:          "de97 1234 5678 9000 0000 01", Currency: "eur",
	})
	if err != nil {
		t.Fatalf("AddBankAccount: %v", err)
	}

	accounts, err := st.GetAllBankAccounts()
	if err != nil {
		t.Fatalf("GetAllBankAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts, want 1", len(accounts))
	}
	if got, want := accounts[0].IBAN, "DE97123456789000000001"; got != want {
		t.Errorf("IBAN: got %q, want %q — spacing and case must not fork account matching", got, want)
	}
	if got, want := accounts[0].Currency, "EUR"; got != want {
		t.Errorf("Currency: got %q, want %q", got, want)
	}
}

func TestAddBankAccount_ibanAndCurrencyOptional(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.AddBankAccount(store.NewBankAccount{
		SessionID: "s", AccountUID: "a", BankName: "Bank", BankCountry: "DE",
		SessionExpiry: "2027-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("AddBankAccount: %v", err)
	}
	accounts, _ := st.GetAllBankAccounts()
	if accounts[0].IBAN != "" || accounts[0].Currency != "" {
		t.Errorf("banks that supply neither must round-trip empty, got %q / %q",
			accounts[0].IBAN, accounts[0].Currency)
	}
}

func TestMigration_addsIbanAndCurrencyToExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE bank_accounts (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id     TEXT NOT NULL,
			account_uid    TEXT NOT NULL,
			bank_name      TEXT NOT NULL,
			bank_country   TEXT NOT NULL,
			actual_account  TEXT NOT NULL DEFAULT '',
			start_sync_date TEXT NOT NULL DEFAULT '',
			session_expiry  TEXT NOT NULL DEFAULT '',
			created_at     TEXT NOT NULL DEFAULT (datetime('now'))
		);
		INSERT INTO bank_accounts (session_id, account_uid, bank_name, bank_country)
		VALUES ('s', 'a', 'Bank', 'DE');
	`); err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	_ = raw.Close()

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open must migrate an existing DB: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	accounts, err := st.GetAllBankAccounts()
	if err != nil {
		t.Fatalf("GetAllBankAccounts after migration: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts, want 1", len(accounts))
	}
	if accounts[0].IBAN != "" || accounts[0].Currency != "" {
		t.Errorf("migrated rows must default to empty, got %q / %q", accounts[0].IBAN, accounts[0].Currency)
	}
}

func TestRetentionOutlivesTheFetchWindow(t *testing.T) {
	// The sync loop refetches a rolling 30 days and matches within seven days on
	// either side. If deduplication state expired sooner, a transaction the user
	// deleted would drop out of the state while still being refetched, and every
	// sync would import it again.
	const rollingFetchWindow = 30
	const matchWindow = 7

	if store.RetentionDays <= rollingFetchWindow+matchWindow {
		t.Fatalf("RetentionDays is %d, which does not outlive a %d-day fetch window widened by %d days: "+
			"deleted transactions would resurrect on the next sync",
			store.RetentionDays, rollingFetchWindow, matchWindow)
	}
}

func addTestAccount(t *testing.T, st *store.Store, state string) int64 {
	t.Helper()
	id, err := st.AddBankAccount(store.NewBankAccount{
		SessionID: "sess", AccountUID: "uid", BankName: "TestBank", BankCountry: "DE",
		ActualAccount: "Checking", Currency: "EUR", IBAN: "DE31123456789005193987",
		OpeningBalanceState: state,
	})
	if err != nil {
		t.Fatalf("AddBankAccount: %v", err)
	}
	return id
}

func onlyAccount(t *testing.T, st *store.Store) store.BankAccount {
	t.Helper()
	accounts, err := st.GetAllBankAccounts()
	if err != nil {
		t.Fatalf("GetAllBankAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts: got %d, want 1", len(accounts))
	}
	return accounts[0]
}

func TestOpeningBalance_roundTripsThroughTheStore(t *testing.T) {
	st := openTestStore(t)
	id := addTestAccount(t, st, store.OpeningBalanceAuto)

	if got := onlyAccount(t, st).OpeningBalanceState; got != store.OpeningBalanceAuto {
		t.Fatalf("state after insert: got %q, want %q", got, store.OpeningBalanceAuto)
	}

	if err := st.SetOpeningBalance(id, -123456, "2026-07-14", "bankingsync-opening-DE77"); err != nil {
		t.Fatalf("SetOpeningBalance: %v", err)
	}

	a := onlyAccount(t, st)
	if a.OpeningBalanceCents != -123456 {
		t.Errorf("cents: got %d, want -123456 — a negative opening balance is an "+
			"overdraft and must survive storage intact", a.OpeningBalanceCents)
	}
	if a.OpeningBalanceState != store.OpeningBalanceWritten {
		t.Errorf("state: got %q, want %q", a.OpeningBalanceState, store.OpeningBalanceWritten)
	}
	if a.OpeningBalanceDate != "2026-07-14" || a.OpeningBalanceRef != "bankingsync-opening-DE77" {
		t.Errorf("date/ref: got %q / %q", a.OpeningBalanceDate, a.OpeningBalanceRef)
	}
	if a.OpeningBalanceWrittenAt == "" {
		t.Error("written_at was not stamped")
	}
}

// A row that predates this feature must never be filled in unattended.
func TestOpeningBalance_legacyRowsAreDistinguishableFromNewOnes(t *testing.T) {
	st := openTestStore(t)
	addTestAccount(t, st, store.OpeningBalanceLegacy)

	if got := onlyAccount(t, st).OpeningBalanceState; got != store.OpeningBalanceLegacy {
		t.Errorf("state: got %q, want the empty legacy marker", got)
	}
}

func TestClaimOpeningBalance_onlyOneCallerWins(t *testing.T) {
	st := openTestStore(t)
	id := addTestAccount(t, st, store.OpeningBalanceAuto)

	const callers = 16
	results := make(chan bool, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := st.ClaimOpeningBalance(id)
			if err != nil {
				t.Errorf("ClaimOpeningBalance: %v", err)
			}
			results <- ok
		}()
	}
	wg.Wait()
	close(results)

	won := 0
	for ok := range results {
		if ok {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d of %d callers claimed the account — a double click would write "+
			"two opening balances", won, callers)
	}
}

func TestClaimOpeningBalance_refusesAnAlreadyWrittenAccount(t *testing.T) {
	st := openTestStore(t)
	id := addTestAccount(t, st, store.OpeningBalanceAuto)

	if err := st.SetOpeningBalance(id, 1000, "2026-07-14", "ref"); err != nil {
		t.Fatalf("SetOpeningBalance: %v", err)
	}
	ok, err := st.ClaimOpeningBalance(id)
	if err != nil {
		t.Fatalf("ClaimOpeningBalance: %v", err)
	}
	if ok {
		t.Error("an account with a written opening balance was claimed again")
	}
}

func TestSetAccountDrift_recordsStateAndStamp(t *testing.T) {
	st := openTestStore(t)
	id := addTestAccount(t, st, store.OpeningBalanceAuto)

	if err := st.SetAccountDrift(id, -4250, store.DriftAlert); err != nil {
		t.Fatalf("SetAccountDrift: %v", err)
	}
	a := onlyAccount(t, st)
	if a.DriftCents != -4250 || a.DriftState != store.DriftAlert {
		t.Errorf("drift: got %d / %q, want -4250 / %q", a.DriftCents, a.DriftState, store.DriftAlert)
	}
	if a.DriftCheckedAt == "" {
		t.Error("drift_checked_at was not stamped")
	}
}

func TestSetBalancesAccess_records(t *testing.T) {
	st := openTestStore(t)
	id := addTestAccount(t, st, store.OpeningBalanceAuto)

	if err := st.SetBalancesAccess(id, "denied"); err != nil {
		t.Fatalf("SetBalancesAccess: %v", err)
	}
	if got := onlyAccount(t, st).BalancesAccess; got != "denied" {
		t.Errorf("balances_access: got %q, want denied", got)
	}
}

// The additive migrations must run against a database created before the
// opening-balance columns existed, which is every existing installation.
func TestMigration_addsOpeningBalanceColumnsToALegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	_, err = raw.Exec(`
		CREATE TABLE bank_accounts (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id     TEXT NOT NULL,
			account_uid    TEXT NOT NULL,
			bank_name      TEXT NOT NULL,
			bank_country   TEXT NOT NULL,
			actual_account  TEXT NOT NULL DEFAULT '',
			start_sync_date TEXT NOT NULL DEFAULT '',
			session_expiry  TEXT NOT NULL DEFAULT '',
			created_at     TEXT NOT NULL DEFAULT (datetime('now'))
		);
		INSERT INTO bank_accounts (session_id, account_uid, bank_name, bank_country)
		VALUES ('sess', 'uid', 'OldBank', 'DE');`)
	if err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	_ = raw.Close()

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open on a legacy database: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	accounts, err := st.GetAllBankAccounts()
	if err != nil {
		t.Fatalf("GetAllBankAccounts after migration: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts: got %d, want 1", len(accounts))
	}
	if got := accounts[0].OpeningBalanceState; got != store.OpeningBalanceLegacy {
		t.Errorf("a pre-existing account came out as %q — it must stay in the legacy "+
			"state so nothing is written to it unattended", got)
	}
	if accounts[0].BankName != "OldBank" {
		t.Errorf("existing data was lost: %#v", accounts[0])
	}
}

func TestMatchReviews_roundTripAndReset(t *testing.T) {
	s := openTestStore(t)

	r := store.MatchReview{
		BankAccountID: 1, Backend: "firefly", ExternalRef: "book-1", PendingKey: "book-1",
		TxnDate: time.Now().UTC().Format("2006-01-02"), AmountCents: -999, Currency: "EUR",
		Payee: "Netflix", BestProbability: 0.74, BestPayeeLevel: "none",
		BestAmountLevel: "exact", BestDateLevel: "same",
	}
	if err := s.AddMatchReview(r); err != nil {
		t.Fatalf("AddMatchReview: %v", err)
	}
	// Offering the same transaction again must not queue a second decision.
	if err := s.AddMatchReview(r); err != nil {
		t.Fatalf("AddMatchReview twice: %v", err)
	}

	got, err := s.GetMatchReviews()
	if err != nil {
		t.Fatalf("GetMatchReviews: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].Payee != "Netflix" || got[0].AmountCents != -999 || got[0].BestPayeeLevel != "none" {
		t.Errorf("round trip lost something: %+v", got[0])
	}

	keys, err := s.AllHeldKeys()
	if err != nil {
		t.Fatalf("AllHeldKeys: %v", err)
	}
	if !keys[1]["book-1"] {
		t.Error("the held key is not reported, so the sync loop would offer it again")
	}

	// A reset declares that nothing has been imported. An entry left behind would
	// point at candidates the reset assumes are gone, and would keep its own
	// transaction from ever being fetched again.
	if _, _, err := s.ResetImportState(); err != nil {
		t.Fatalf("ResetImportState: %v", err)
	}
	if n, _ := s.CountMatchReviews(); n != 0 {
		t.Errorf("%d held transactions survived a reset", n)
	}
}

func TestMatchReviews_pruneFollowsRetention(t *testing.T) {
	s := openTestStore(t)

	fresh := time.Now().UTC().Format("2006-01-02")
	stale := time.Now().UTC().AddDate(0, 0, -store.RetentionDays-1).Format("2006-01-02")

	for i, d := range []string{fresh, stale} {
		if err := s.AddMatchReview(store.MatchReview{
			BankAccountID: 1, PendingKey: fmt.Sprintf("k%d", i), TxnDate: d,
		}); err != nil {
			t.Fatalf("AddMatchReview: %v", err)
		}
	}
	if err := s.PruneMatchReviews(); err != nil {
		t.Fatalf("PruneMatchReviews: %v", err)
	}

	got, _ := s.GetMatchReviews()
	if len(got) != 1 {
		t.Fatalf("got %d rows, want only the fresh one", len(got))
	}
	if got[0].TxnDate != fresh {
		t.Errorf("kept %s, want %s — past retention the bank will not offer it again "+
			"either, so the row describes an import that can no longer happen", got[0].TxnDate, fresh)
	}
}

// TestMatchDecisions_survivesNothingItShouldNot pins the two purges and the
// retention window, all three of which have to name the new table by hand.
func TestMatchDecisions_survivesNothingItShouldNot(t *testing.T) {
	st := openTestStore(t)
	id, err := st.AddBankAccount(store.NewBankAccount{
		SessionID: "s", AccountUID: "u", BankName: "TestBank", Currency: "EUR",
	})
	if err != nil {
		t.Fatalf("AddBankAccount: %v", err)
	}

	add := func(key, day string) {
		t.Helper()
		if err := st.AddMatchDecision(store.MatchDecision{
			RunID: "run-1", BankAccountID: id, Bank: "TestBank", PendingKey: key,
			Outcome: "created", ParamVersion: "abc123", TxnDate: day,
		}); err != nil {
			t.Fatalf("AddMatchDecision: %v", err)
		}
	}
	recent := time.Now().UTC().Format("2006-01-02")
	old := time.Now().UTC().AddDate(0, 0, -400).Format("2006-01-02")

	add("k-recent", recent)
	add("k-old", old)
	if n, _ := st.CountMatchDecisions(); n != 2 {
		t.Fatalf("setup: %d decisions, want 2", n)
	}

	// Retention drops what the bank will not offer again, and keeps the rest.
	if err := st.PruneMatchDecisions(); err != nil {
		t.Fatalf("PruneMatchDecisions: %v", err)
	}
	left, err := st.GetMatchDecisions(10)
	if err != nil {
		t.Fatalf("GetMatchDecisions: %v", err)
	}
	if len(left) != 1 {
		t.Fatalf("%d decisions after retention, want 1", len(left))
	}
	// Which one survived is the whole assertion: a count alone passes just as
	// happily when the window is the wrong way round.
	if left[0].PendingKey != "k-recent" {
		t.Errorf("retention kept %q and dropped the recent one; the window is inverted",
			left[0].PendingKey)
	}

	// A reset declares the imports never to have happened, so the record of
	// deciding them describes a history that no longer exists.
	if _, _, err := st.ResetImportState(); err != nil {
		t.Fatalf("ResetImportState: %v", err)
	}
	if n, _ := st.CountMatchDecisions(); n != 0 {
		t.Errorf("%d decisions survived a reset", n)
	}

	add("k-again", recent)
	if err := st.RemoveBankAccount(id); err != nil {
		t.Fatalf("RemoveBankAccount: %v", err)
	}
	if n, _ := st.CountMatchDecisions(); n != 0 {
		t.Errorf("%d decisions survived the removal of their account", n)
	}
}

// TestMatchDecisions_truthLandsOnTheLatestRecord matters because a transaction
// offered over several runs leaves one record per run, and only the last of them
// describes the state a person has now settled.
func TestMatchDecisions_truthLandsOnTheLatestRecord(t *testing.T) {
	st := openTestStore(t)
	id, err := st.AddBankAccount(store.NewBankAccount{
		SessionID: "s", AccountUID: "u", BankName: "TestBank", Currency: "EUR",
	})
	if err != nil {
		t.Fatalf("AddBankAccount: %v", err)
	}
	day := time.Now().UTC().Format("2006-01-02")
	for _, run := range []string{"run-1", "run-2"} {
		if err := st.AddMatchDecision(store.MatchDecision{
			RunID: run, BankAccountID: id, PendingKey: "key-1",
			Outcome: "held", ParamVersion: "v1", TxnDate: day,
		}); err != nil {
			t.Fatalf("AddMatchDecision: %v", err)
		}
	}

	if err := st.SetMatchDecisionTruth(id, "key-1", true); err != nil {
		t.Fatalf("SetMatchDecisionTruth: %v", err)
	}
	decisions, err := st.GetMatchDecisions(10)
	if err != nil {
		t.Fatalf("GetMatchDecisions: %v", err)
	}
	if len(decisions) != 2 {
		t.Fatalf("got %d decisions, want 2", len(decisions))
	}
	// Newest first.
	if decisions[0].Truth == nil || !*decisions[0].Truth {
		t.Error("the answer did not land on the most recent record")
	}
	if decisions[1].Truth != nil {
		t.Error("an earlier record was overwritten; it describes a different run and " +
			"a state that was true at the time")
	}
}

// TestMatchInquiries_onlyOneIsOpenAtATime pins the invariant every caller
// relies on, and the one that keeps a request for labels from becoming a queue
// people learn to click past.
func TestMatchInquiries_onlyOneIsOpenAtATime(t *testing.T) {
	st := openTestStore(t)
	id, err := st.AddBankAccount(store.NewBankAccount{
		SessionID: "s", AccountUID: "u", BankName: "TestBank", Currency: "EUR",
	})
	if err != nil {
		t.Fatalf("AddBankAccount: %v", err)
	}
	day := time.Now().UTC().Format("2006-01-02")

	if open, err := st.HasOpenInquiry(); err != nil || open {
		t.Fatalf("an empty store reports an open question (open=%v, err=%v)", open, err)
	}
	if _, ok, err := st.OpenInquiry(); err != nil || ok {
		t.Fatalf("an empty store returned a question (ok=%v, err=%v)", ok, err)
	}

	add := func(key string) {
		t.Helper()
		if err := st.AddMatchInquiry(store.MatchInquiry{
			BankAccountID: id, Bank: "TestBank", PendingKey: key,
			ParamVersion: "v1", Outcome: "adopted", Probability: 0.94, Gain: 0.03,
			TxnDate: day, AmountCents: -4200, Currency: "EUR", Payee: "Hotel Berlin",
		}); err != nil {
			t.Fatalf("AddMatchInquiry: %v", err)
		}
	}

	add("k-1")
	first, ok, err := st.OpenInquiry()
	if err != nil || !ok {
		t.Fatalf("the question was not returned (ok=%v, err=%v)", ok, err)
	}
	if first.Payee != "Hotel Berlin" || first.AmountCents != -4200 || first.Gain != 0.03 {
		t.Errorf("the snapshot did not survive storage: %+v", first)
	}

	if err := st.CloseInquiry(first.ID); err != nil {
		t.Fatalf("CloseInquiry: %v", err)
	}
	if open, _ := st.HasOpenInquiry(); open {
		t.Fatal("a closed question is still open")
	}
	// Both readers have to agree, and they are separate queries: one counts and
	// one fetches, so a filter dropped from either is invisible to the other.
	if got, ok, err := st.OpenInquiry(); err != nil || ok {
		t.Fatalf("OpenInquiry returned the answered question back (id=%d, ok=%v, err=%v)",
			got.ID, ok, err)
	}
	// Twice is a bug in the caller, not a second answer.
	if err := st.CloseInquiry(first.ID); err == nil {
		t.Error("closing an already answered question was accepted")
	}

	add("k-2")
	second, _, _ := st.OpenInquiry()
	if second.ID == first.ID {
		t.Fatal("the new question reused the answered one")
	}
}

// TestMatchInquiries_survivesNothingItShouldNot pins the three places that have
// to name the table by hand, and the fourth thing that is easy to get wrong:
// an unanswered question must age out too, or it blocks every later one for as
// long as nobody answers it.
func TestMatchInquiries_survivesNothingItShouldNot(t *testing.T) {
	st := openTestStore(t)
	id, err := st.AddBankAccount(store.NewBankAccount{
		SessionID: "s", AccountUID: "u", BankName: "TestBank", Currency: "EUR",
	})
	if err != nil {
		t.Fatalf("AddBankAccount: %v", err)
	}
	add := func(key, day string) {
		t.Helper()
		if err := st.AddMatchInquiry(store.MatchInquiry{
			BankAccountID: id, PendingKey: key, TxnDate: day, Outcome: "adopted",
		}); err != nil {
			t.Fatalf("AddMatchInquiry: %v", err)
		}
	}
	recent := time.Now().UTC().Format("2006-01-02")
	stale := time.Now().UTC().AddDate(0, 0, -400).Format("2006-01-02")

	add("k-old", stale)
	if err := st.PruneMatchInquiries(); err != nil {
		t.Fatalf("PruneMatchInquiries: %v", err)
	}
	if open, _ := st.HasOpenInquiry(); open {
		t.Error("an unanswered question past retention still blocks the next one")
	}

	add("k-recent", recent)
	if err := st.PruneMatchInquiries(); err != nil {
		t.Fatalf("PruneMatchInquiries: %v", err)
	}
	if open, _ := st.HasOpenInquiry(); !open {
		t.Fatal("retention took a question about a transaction still inside the window")
	}

	if _, _, err := st.ResetImportState(); err != nil {
		t.Fatalf("ResetImportState: %v", err)
	}
	if open, _ := st.HasOpenInquiry(); open {
		t.Error("a reset left a question about an import it just declared never happened")
	}

	add("k-again", recent)
	if err := st.RemoveBankAccount(id); err != nil {
		t.Fatalf("RemoveBankAccount: %v", err)
	}
	if open, _ := st.HasOpenInquiry(); open {
		t.Error("removing the account left a question nobody can answer")
	}
}

// TestShadowOutcomes_areCountedAgainstOneCandidate pins the scoping. A tally
// that ignored which parameter set a shadow came from would credit a new
// candidate with the record of whatever was being watched last week.
func TestShadowOutcomes_areCountedAgainstOneCandidate(t *testing.T) {
	st := openTestStore(t)
	id, err := st.AddBankAccount(store.NewBankAccount{
		SessionID: "s", AccountUID: "u", BankName: "TestBank", Currency: "EUR",
	})
	if err != nil {
		t.Fatalf("AddBankAccount: %v", err)
	}
	day := time.Now().UTC().Format("2006-01-02")
	add := func(key, version, live, shadow string) {
		t.Helper()
		if err := st.AddMatchDecision(store.MatchDecision{
			RunID: "r", BankAccountID: id, PendingKey: key, TxnDate: day,
			Outcome: live, ParamVersion: "v1",
			ShadowVersion: version, ShadowOutcome: shadow,
		}); err != nil {
			t.Fatalf("AddMatchDecision: %v", err)
		}
	}

	add("a", "cand-1", "adopted", "adopted")
	add("b", "cand-1", "adopted", "created")
	add("c", "cand-1", "held", "held")
	add("d", "cand-2", "adopted", "created")
	add("e", "", "adopted", "")

	got, err := st.CountShadowOutcomes("cand-1")
	if err != nil {
		t.Fatalf("CountShadowOutcomes: %v", err)
	}
	if got.Total != 3 || got.Differing != 1 {
		t.Errorf("got %d of %d differing, want 1 of 3 — %+v", got.Differing, got.Total, got)
	}

	// An empty version is not a wildcard. Decisions recorded while nothing was
	// being watched belong to no candidate at all.
	if empty, err := st.CountShadowOutcomes(""); err != nil || empty.Total != 0 {
		t.Errorf("an empty version matched %d decisions (err %v)", empty.Total, err)
	}
	// And a candidate nothing has been recorded against reports nothing rather
	// than failing on the null a SUM over an empty set produces.
	if none, err := st.CountShadowOutcomes("cand-3"); err != nil || none.Total != 0 || none.Differing != 0 {
		t.Errorf("an unwatched candidate reported %+v (err %v)", none, err)
	}
}

// TestLevelObservations_accumulateAndAreScopedToTheirParameters covers the two
// things the non-match sample depends on: that repeated runs add up rather than
// overwrite, and that a sample gathered under different classification rules is
// not merged with one gathered under the current ones.
func TestLevelObservations_accumulateAndAreScopedToTheirParameters(t *testing.T) {
	st := openTestStore(t)
	id, err := st.AddBankAccount(store.NewBankAccount{
		SessionID: "s", AccountUID: "u", BankName: "TestBank", Currency: "EUR",
	})
	if err != nil {
		t.Fatalf("AddBankAccount: %v", err)
	}
	add := func(version string, n int) {
		t.Helper()
		if err := st.AddLevelObservations(id, version, []store.LevelObservation{
			{Field: "payee", Level: "none", Count: n},
			{Field: "amount", Level: "outside_higher", Count: n},
		}); err != nil {
			t.Fatalf("AddLevelObservations: %v", err)
		}
	}

	add("v1", 40)
	add("v1", 60)
	got, err := st.LevelObservations("v1")
	if err != nil {
		t.Fatalf("LevelObservations: %v", err)
	}
	total := 0
	for _, o := range got {
		if o.Field == "payee" && o.Level == "none" {
			total = o.Count
		}
	}
	if total != 100 {
		t.Errorf("two runs of 40 and 60 gave %d, want 100 — the tally overwrites rather than adds", total)
	}

	// A different parameter version is a different classification, not more of
	// the same one.
	add("v2", 5)
	if got, _ := st.LevelObservations("v1"); len(got) == 0 {
		t.Fatal("the earlier version's sample disappeared")
	}
	for _, o := range got {
		if o.Field == "payee" && o.Level == "none" && o.Count != 100 {
			t.Errorf("a sample under v2 leaked into v1: %d", o.Count)
		}
	}

	// Retention keeps only what the parameters in force produced.
	if err := st.PruneLevelObservations("v2"); err != nil {
		t.Fatalf("PruneLevelObservations: %v", err)
	}
	if old, _ := st.LevelObservations("v1"); len(old) != 0 {
		t.Errorf("a sample under retired parameters survived the prune: %+v", old)
	}
	if cur, _ := st.LevelObservations("v2"); len(cur) == 0 {
		t.Error("the prune took the sample in force with it")
	}

	// And it goes with the account and with a reset, like everything else
	// derived from imported data.
	if _, _, err := st.ResetImportState(); err != nil {
		t.Fatalf("ResetImportState: %v", err)
	}
	if got, _ := st.LevelObservations("v2"); len(got) != 0 {
		t.Errorf("a reset left %d observation rows behind", len(got))
	}
	add("v2", 7)
	if err := st.RemoveBankAccount(id); err != nil {
		t.Fatalf("RemoveBankAccount: %v", err)
	}
	if got, _ := st.LevelObservations("v2"); len(got) != 0 {
		t.Errorf("removing the account left %d observation rows behind", len(got))
	}
}

// TestMatchDecisions_keepEvidenceAndForgetWhoItWasAbout covers the split the
// decision log needs and did not have.
//
// An unanswered decision is a diagnostic: once the bank stops offering the
// transaction nobody can check it, so it goes with the rest of the deduplication
// state. An answered one is evidence, and evidence does not expire when the
// transaction does — but everything saying WHICH transaction it was stops being
// needed at exactly that moment, and a pending key carries a payee and an amount.
func TestMatchDecisions_keepEvidenceAndForgetWhoItWasAbout(t *testing.T) {
	st := openTestStore(t)
	id, err := st.AddBankAccount(store.NewBankAccount{
		SessionID: "s", AccountUID: "u", BankName: "TestBank", Currency: "EUR",
	})
	if err != nil {
		t.Fatalf("AddBankAccount: %v", err)
	}
	day := func(n int) string {
		return time.Now().UTC().AddDate(0, 0, -n).Format("2006-01-02")
	}
	add := func(key, txnDate string, truth *bool) {
		t.Helper()
		if err := st.AddMatchDecision(store.MatchDecision{
			RunID: "r", BankAccountID: id, Bank: "TestBank", PendingKey: key,
			IncomingRef: "ref-" + key, CandidateID: "txn-" + key,
			PayeeLevel: "exact", AmountLevel: "exact", DateLevel: "same",
			Candidates: 2, Weight: 9.5, Probability: 0.99,
			Outcome: "adopted", ParamVersion: "v1", TxnDate: txnDate, Truth: truth,
		}); err != nil {
			t.Fatalf("AddMatchDecision: %v", err)
		}
	}
	yes := true

	add("fresh-open", day(3), nil)
	add("stale-open", day(60), nil)
	add("fresh-settled", day(3), &yes)
	add("stale-settled", day(60), &yes)
	add("ancient-settled", day(500), &yes)

	if err := st.PruneMatchDecisions(); err != nil {
		t.Fatalf("PruneMatchDecisions: %v", err)
	}
	got, err := st.GetMatchDecisions(100)
	if err != nil {
		t.Fatalf("GetMatchDecisions: %v", err)
	}
	byKey := map[string]store.MatchDecision{}
	for _, d := range got {
		byKey[d.PendingKey] = d
	}

	if _, ok := byKey["stale-open"]; ok {
		t.Error("an unanswered decision past the window survived: nobody can check it any more")
	}
	if _, ok := byKey["fresh-open"]; !ok {
		t.Error("an unanswered decision inside the window was taken")
	}
	if d, ok := byKey["fresh-settled"]; !ok {
		t.Error("a settled decision inside the window was taken")
	} else if d.IncomingRef == "" || d.CandidateID == "" {
		t.Error("a decision inside the window was redacted early")
	}

	// The stale settled one is still there, but nothing on it says which
	// transaction it was.
	var redacted *store.MatchDecision
	for i, d := range got {
		if d.PayeeLevel == "exact" && d.PendingKey == "" && d.Truth != nil {
			redacted = &got[i]
		}
	}
	if redacted == nil {
		t.Fatal("the settled decision past the window was deleted rather than redacted")
	}
	for name, v := range map[string]string{
		"pending_key":  redacted.PendingKey,
		"incoming_ref": redacted.IncomingRef,
		"candidate_id": redacted.CandidateID,
		"run_id":       redacted.RunID,
	} {
		if v != "" {
			t.Errorf("%s survived redaction as %q — it names the purchase", name, v)
		}
	}
	if !strings.HasSuffix(redacted.TxnDate, "-01") {
		t.Errorf("the date was kept to the day as %q; the export emits nothing finer than "+
			"a month and neither should this", redacted.TxnDate)
	}
	// The decision timestamp matters more than the transaction date here. A
	// decision is made within hours of the transaction reaching the feed, so a
	// timestamp to the second pins the purchase down more precisely than the
	// date ever did, and coarsening one without the other achieves nothing.
	if len(redacted.DecidedAt) != len("2006-01-01") || !strings.HasSuffix(redacted.DecidedAt, "-01") {
		t.Errorf("the decision timestamp survived as %q, which locates the purchase to the "+
			"second and undoes the coarsening of the transaction date", redacted.DecidedAt)
	}
	// And what the estimators actually consume is untouched.
	if redacted.PayeeLevel != "exact" || redacted.AmountLevel != "exact" ||
		redacted.DateLevel != "same" || redacted.Candidates != 2 || redacted.Weight != 9.5 {
		t.Errorf("redaction took the evidence with it: %+v", *redacted)
	}
	if redacted.Truth == nil || !*redacted.Truth {
		t.Error("redaction lost the answer, which is the only reason the row was kept")
	}

	// "ancient-settled" is about a transaction from 500 days ago but the decision
	// about it was made just now, and that is the clock the evidence window runs
	// on. Deleting by transaction date would discard what this run just learned,
	// and — worse — would never reach a row the bank forward-dated, because such
	// a row is newer than every backward-looking cutoff. decided_at is written
	// here, so it is always real and always in the past.
	var settledPast int
	for _, d := range got {
		if d.PendingKey == "" && d.Truth != nil {
			settledPast++
		}
	}
	if settledPast != 2 {
		t.Errorf("%d redacted settled decisions survived, want 2 — the one from 60 days ago "+
			"and the one from 500 days ago, whose decision was made just now", settledPast)
	}
	for _, d := range got {
		if d.DecidedAt < time.Now().UTC().AddDate(0, 0, -store.EvidenceRetentionDays).Format("2006-01-02") {
			t.Errorf("a decision made on %s outlived the evidence window", d.DecidedAt)
		}
	}
}

// TestOpen_discardsParametersFittedOnTheOldCorpus covers the one-shot upgrade
// step, in both directions.
//
// A promoted parameter set is the fitted numbers themselves and is loaded into
// the live policy on every run, so one fitted on the contaminated decision log
// would go on deciding real merges forever. Nothing records which corpus made a
// set, so the only safe move is to drop what exists at upgrade and refit.
//
// The other direction matters just as much: it must run once. A set promoted
// after the upgrade, under the corrected log, has to survive the next open.
func TestOpen_discardsParametersFittedOnTheOldCorpus(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bank.db")

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, k := range []string{store.SettingPromotedTrial, store.SettingShadowTrial} {
		if err := st.SetSetting(k, `{"fitted":"on the old corpus"}`); err != nil {
			t.Fatalf("SetSetting %s: %v", k, err)
		}
	}
	// Undo the marker the first open wrote, which is what an upgrade looks like:
	// a database carrying promoted parameters and no record of the discard.
	if err := st.SetSetting("match_corpus_v3_discarded", ""); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st, err = store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for _, k := range []string{store.SettingPromotedTrial, store.SettingShadowTrial} {
		if v, err := st.GetSetting(k); err != nil {
			t.Fatalf("GetSetting %s: %v", k, err)
		} else if v != "" {
			t.Errorf("%s survived the upgrade as %q; it was fitted on labels the model "+
				"scored against itself and it decides real merges", k, v)
		}
	}

	if err := st.SetSetting(store.SettingPromotedTrial, `{"fitted":"on the new one"}`); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	st, err = store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if v, _ := st.GetSetting(store.SettingPromotedTrial); v == "" {
		t.Error("a set promoted after the upgrade was discarded too; the step must run once")
	}
}

// TestMatchDecisions_anAnswerThatLandsNowhereSaysSo covers the two ways an
// answer can fail to reach the decision it is about.
//
// Both used to be silent. The setters discarded the sql.Result, so an update
// that touched no rows was indistinguishable from one that did, and a queue
// entry outliving its decision looked exactly like a filed answer.
//
// The empty key is the worse of the two and is not a rows-affected problem at
// all. Redaction blanks pending_key, so every aged row in an account shares the
// empty key; a lookup by it finds the newest of them and writes the answer onto
// a stranger's label — updating exactly one row, successfully, and corrupting
// the only evidence the estimators have.
func TestMatchDecisions_anAnswerThatLandsNowhereSaysSo(t *testing.T) {
	st := openTestStore(t)
	id, err := st.AddBankAccount(store.NewBankAccount{
		SessionID: "s", AccountUID: "u", BankName: "TestBank", Currency: "EUR",
	})
	if err != nil {
		t.Fatalf("AddBankAccount: %v", err)
	}
	yes := true
	if err := st.AddMatchDecision(store.MatchDecision{
		RunID: "r", BankAccountID: id, Bank: "TestBank", PendingKey: "old",
		IncomingRef: "ref-old", CandidateID: "txn-old",
		PayeeLevel: "exact", AmountLevel: "exact", DateLevel: "same",
		Outcome: "adopted", ParamVersion: "v1",
		TxnDate: time.Now().UTC().AddDate(0, 0, -60).Format("2006-01-02"), Truth: &yes,
	}); err != nil {
		t.Fatalf("AddMatchDecision: %v", err)
	}
	if err := st.PruneMatchDecisions(); err != nil {
		t.Fatalf("PruneMatchDecisions: %v", err)
	}

	if err := st.SetMatchDecisionTruth(id, "", false); !errors.Is(err, store.ErrNoSuchDecision) {
		t.Errorf("SetMatchDecisionTruth with an empty key returned %v, want ErrNoSuchDecision; "+
			"an empty key is what redaction leaves behind, not a transaction", err)
	}
	got, err := st.GetMatchDecisions(10)
	if err != nil {
		t.Fatalf("GetMatchDecisions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("setup: %d decisions, want the one redacted row", len(got))
	}
	if got[0].Truth == nil || !*got[0].Truth {
		t.Error("an answer aimed at nothing overwrote the answer on a redacted decision")
	}

	if err := st.SetMatchDecisionTruth(id, "never-existed", true); !errors.Is(err, store.ErrNoSuchDecision) {
		t.Errorf("SetMatchDecisionTruth for an unknown key returned %v, want ErrNoSuchDecision", err)
	}
	if err := st.SetMatchDecisionTruthByID(9999, true); !errors.Is(err, store.ErrNoSuchDecision) {
		t.Errorf("SetMatchDecisionTruthByID for an unknown id returned %v, want ErrNoSuchDecision", err)
	}
	if id, err := st.LatestMatchDecisionID(id, ""); id != 0 || err != nil {
		t.Errorf("LatestMatchDecisionID with an empty key returned (%d, %v), want (0, nil); "+
			"it would otherwise pin a question to a redacted stranger", id, err)
	}
}

// TestMatchDecisions_aForwardDatedRowStillAges closes the gap that a
// backward-looking cutoff leaves open.
//
// Retention compares the transaction date against a date in the past. A bank
// reporting a value date rather than a booking date can put that date in the
// future, and such a row is newer than every cutoff: it was never redacted and
// never deleted, so it kept a payee, an amount and a second-resolution timestamp
// for as long as the database existed.
func TestMatchDecisions_aForwardDatedRowStillAges(t *testing.T) {
	st := openTestStore(t)
	id, err := st.AddBankAccount(store.NewBankAccount{
		SessionID: "s", AccountUID: "u", BankName: "TestBank", Currency: "EUR",
	})
	if err != nil {
		t.Fatalf("AddBankAccount: %v", err)
	}
	ahead := time.Now().UTC().AddDate(0, 0, store.ForwardDatingDays+30).Format("2006-01-02")
	yes := true
	for _, tc := range []struct {
		key   string
		truth *bool
	}{{"scheduled-open", nil}, {"scheduled-settled", &yes}} {
		if err := st.AddMatchDecision(store.MatchDecision{
			RunID: "r", BankAccountID: id, Bank: "TestBank", PendingKey: tc.key,
			IncomingRef: "ref-" + tc.key, CandidateID: "txn-" + tc.key,
			PayeeLevel: "exact", AmountLevel: "exact", DateLevel: "same",
			Candidates: 2, Weight: 9.5, Probability: 0.99,
			Outcome: "adopted", ParamVersion: "v1", TxnDate: ahead, Truth: tc.truth,
		}); err != nil {
			t.Fatalf("AddMatchDecision: %v", err)
		}
	}

	if err := st.PruneMatchDecisions(); err != nil {
		t.Fatalf("PruneMatchDecisions: %v", err)
	}
	got, err := st.GetMatchDecisions(100)
	if err != nil {
		t.Fatalf("GetMatchDecisions: %v", err)
	}
	for _, d := range got {
		if d.Truth == nil {
			t.Error("an unanswered decision dated beyond the horizon survived; nobody can " +
				"check it and nothing will ever remove it")
		}
	}
	if len(got) != 1 {
		t.Fatalf("%d decisions survived, want 1: the unanswered one goes, the settled one "+
			"is redacted and kept", len(got))
	}
	d := got[0]
	if d.Truth == nil {
		t.Fatal("the wrong row survived: the unanswered one should have been deleted")
	}
	if d.IncomingRef != "" || d.CandidateID != "" || d.PendingKey != "" || d.RunID != "" {
		t.Errorf("a decision dated beyond the horizon kept what names the purchase: %+v", d)
	}
	if len(d.DecidedAt) != len("2006-01-01") {
		t.Errorf("the decision timestamp survived as %q, to the second, on a row no "+
			"backward-looking cutoff can reach", d.DecidedAt)
	}
}

// TestMatchDecisions_redactionIsIdempotent keeps a run of the prune from
// undoing the previous one's date coarsening or churning rows for nothing.
func TestMatchDecisions_redactionIsIdempotent(t *testing.T) {
	st := openTestStore(t)
	id, err := st.AddBankAccount(store.NewBankAccount{
		SessionID: "s", AccountUID: "u", BankName: "TestBank", Currency: "EUR",
	})
	if err != nil {
		t.Fatalf("AddBankAccount: %v", err)
	}
	yes := true
	if err := st.AddMatchDecision(store.MatchDecision{
		RunID: "r", BankAccountID: id, PendingKey: "k", IncomingRef: "r", CandidateID: "c",
		PayeeLevel: "exact", AmountLevel: "exact", DateLevel: "same",
		Outcome: "adopted", ParamVersion: "v1",
		TxnDate: time.Now().UTC().AddDate(0, 0, -60).Format("2006-01-02"), Truth: &yes,
	}); err != nil {
		t.Fatalf("AddMatchDecision: %v", err)
	}

	if err := st.PruneMatchDecisions(); err != nil {
		t.Fatalf("first prune: %v", err)
	}
	first, _ := st.GetMatchDecisions(10)
	if err := st.PruneMatchDecisions(); err != nil {
		t.Fatalf("second prune: %v", err)
	}
	second, _ := st.GetMatchDecisions(10)

	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("row counts %d then %d, want 1 each", len(first), len(second))
	}
	if first[0].TxnDate != second[0].TxnDate {
		t.Errorf("a second prune moved the date again: %q became %q",
			first[0].TxnDate, second[0].TxnDate)
	}
}

// TestLevelObservations_migratesAnExistingSample covers the one thing a schema
// rename can get wrong without anybody noticing: an installation that already
// has the table.
//
// The sample was originally scoped to the whole parameter version and the column
// said so. It is now scoped to what actually decides a comparison level, which
// is a smaller thing, and the column had to be renamed to stop saying something
// untrue. A fresh database gets the new name from the CREATE; an existing one
// gets it from the migration, and only opening a real database proves that.
func TestLevelObservations_migratesAnExistingSample(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE level_observations (
			bank_account_id INTEGER NOT NULL,
			param_version   TEXT NOT NULL,
			field           TEXT NOT NULL,
			level           TEXT NOT NULL,
			observations    INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (bank_account_id, param_version, field, level)
		) WITHOUT ROWID;
		INSERT INTO level_observations VALUES (1, 'old-scope', 'payee', 'none', 42);`); err != nil {
		t.Fatalf("build the old schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open the store over an existing database: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	got, err := st.LevelObservations("old-scope")
	if err != nil {
		t.Fatalf("LevelObservations: %v", err)
	}
	if len(got) != 1 || got[0].Count != 42 {
		t.Fatalf("the existing sample did not survive the rename: %+v", got)
	}

	// And the renamed column still accumulates.
	if err := st.AddLevelObservations(1, "old-scope", []store.LevelObservation{
		{Field: "payee", Level: "none", Count: 8},
	}); err != nil {
		t.Fatalf("AddLevelObservations: %v", err)
	}
	if after, _ := st.LevelObservations("old-scope"); len(after) != 1 || after[0].Count != 50 {
		t.Errorf("the upsert did not find the renamed column: %+v", after)
	}
}
