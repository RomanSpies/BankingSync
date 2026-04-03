package store_test

import (
	"path/filepath"
	"testing"
	"time"

	"bankingsync/store"
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
	id, err := st.AddBankAccount("sess-1", "acct-1", "TestBank", "DE", "", "", "2025-01-01T00:00:00Z")
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
	_, _ = st.AddBankAccount("s1", "a1", "Bank1", "DE", "", "", "2025-01-01T00:00:00Z")
	_, _ = st.AddBankAccount("s2", "a2", "Bank2", "FR", "", "", "2025-01-01T00:00:00Z")
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
	id, _ := st.AddBankAccount("old-sess", "acct", "Bank", "DE", "", "", "2025-01-01T00:00:00Z")
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
	id, _ := st.AddBankAccount("sess", "acct", "Bank", "DE", "", "", "2025-01-01T00:00:00Z")
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
	has, err := st.HasImportedRef("REF-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if has {
		t.Error("expected false for empty DB")
	}
}

func TestAddImportedRef_andHas(t *testing.T) {
	st := openTestStore(t)
	if err := st.AddImportedRef("REF-1", "2024-06-01"); err != nil {
		t.Fatalf("AddImportedRef: %v", err)
	}
	has, err := st.HasImportedRef("REF-1")
	if err != nil {
		t.Fatalf("HasImportedRef: %v", err)
	}
	if !has {
		t.Error("expected true after add")
	}
}

func TestAddImportedRef_upsert(t *testing.T) {
	st := openTestStore(t)
	_ = st.AddImportedRef("REF-1", "2024-01-01")
	if err := st.AddImportedRef("REF-1", "2024-02-01"); err != nil {
		t.Fatalf("duplicate add should not error: %v", err)
	}
	refs, _ := st.AllImportedRefs()
	if refs["REF-1"] != "2024-02-01" {
		t.Errorf("expected updated date, got %q", refs["REF-1"])
	}
}

func TestAllImportedRefs(t *testing.T) {
	st := openTestStore(t)
	_ = st.AddImportedRef("A", "2024-01-01")
	_ = st.AddImportedRef("B", "2024-02-01")
	refs, err := st.AllImportedRefs()
	if err != nil {
		t.Fatalf("AllImportedRefs: %v", err)
	}
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
	_ = st.AddImportedRef("recent", recent)
	updated, err := st.PruneImportedRefs()
	if err != nil {
		t.Fatalf("PruneImportedRefs: %v", err)
	}
	if _, ok := updated["recent"]; !ok {
		t.Error("expected recent ref to be kept")
	}
}

func TestPruneImportedRefs_removesOld(t *testing.T) {
	st := openTestStore(t)
	old := time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02")
	_ = st.AddImportedRef("old", old)
	updated, err := st.PruneImportedRefs()
	if err != nil {
		t.Fatalf("PruneImportedRefs: %v", err)
	}
	if _, ok := updated["old"]; ok {
		t.Error("expected old ref to be pruned")
	}
}

func TestPruneImportedRefs_boundary21Days(t *testing.T) {
	st := openTestStore(t)
	exactly21 := time.Now().UTC().AddDate(0, 0, -21).Format("2006-01-02")
	_ = st.AddImportedRef("boundary", exactly21)
	updated, err := st.PruneImportedRefs()
	if err != nil {
		t.Fatalf("PruneImportedRefs: %v", err)
	}
	if _, ok := updated["boundary"]; !ok {
		t.Error("expected 21-day-old ref to be kept (inclusive boundary)")
	}
}

func TestPruneImportedRefs_mixed(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC()
	_ = st.AddImportedRef("old", now.AddDate(0, 0, -25).Format("2006-01-02"))
	_ = st.AddImportedRef("recent", now.AddDate(0, 0, -5).Format("2006-01-02"))
	updated, err := st.PruneImportedRefs()
	if err != nil {
		t.Fatalf("PruneImportedRefs: %v", err)
	}
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
	_, ok, err := st.GetPendingTxnID("missing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected not found")
	}
}

func TestSetPending_andGet(t *testing.T) {
	st := openTestStore(t)
	if err := st.SetPending("key-1", "txn-abc"); err != nil {
		t.Fatalf("SetPending: %v", err)
	}
	id, ok, err := st.GetPendingTxnID("key-1")
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
	_ = st.SetPending("k", "v1")
	_ = st.SetPending("k", "v2")
	id, _, _ := st.GetPendingTxnID("k")
	if id != "v2" {
		t.Errorf("got %q, want v2 after upsert", id)
	}
}

func TestDeletePending(t *testing.T) {
	st := openTestStore(t)
	_ = st.SetPending("k", "v")
	if err := st.DeletePending("k"); err != nil {
		t.Fatalf("DeletePending: %v", err)
	}
	_, ok, _ := st.GetPendingTxnID("k")
	if ok {
		t.Error("expected not found after delete")
	}
}

func TestDeletePending_nonexistent(t *testing.T) {
	st := openTestStore(t)
	if err := st.DeletePending("ghost"); err != nil {
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
	_ = st.SetPending("k1", "v1")
	_ = st.SetPending("k2", "v2")
	m, err := st.AllPendingMap()
	if err != nil {
		t.Fatalf("AllPendingMap: %v", err)
	}
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
	_, _ = st.AddBankAccount("sess", "acct", "Bank", "DE", "MyChecking", "", "2025-01-01T00:00:00Z")
	accounts, _ := st.GetAllBankAccounts()
	if accounts[0].ActualAccount != "MyChecking" {
		t.Errorf("ActualAccount: got %q, want MyChecking", accounts[0].ActualAccount)
	}
}

func TestAddBankAccount_startSyncDateRoundtrip(t *testing.T) {
	st := openTestStore(t)
	_, _ = st.AddBankAccount("sess", "acct", "Bank", "DE", "", "2025-03-01", "2025-01-01T00:00:00Z")
	accounts, _ := st.GetAllBankAccounts()
	if accounts[0].StartSyncDate != "2025-03-01" {
		t.Errorf("StartSyncDate: got %q, want 2025-03-01", accounts[0].StartSyncDate)
	}
}

func TestUpdateBankAccountStartDate(t *testing.T) {
	st := openTestStore(t)
	id, _ := st.AddBankAccount("sess", "acct", "Bank", "DE", "", "", "2025-01-01T00:00:00Z")
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
