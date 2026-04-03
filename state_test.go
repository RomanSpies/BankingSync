package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bankingsync/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestGetSession_valid_RFC3339(t *testing.T) {
	s := &State{
		EBSessionID:     "sess-123",
		EBAccountUID:    "acct-456",
		EBSessionExpiry: "2025-12-31T23:59:59Z",
	}
	sid, uid, expiry, err := s.GetSession()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sid != "sess-123" {
		t.Errorf("sessionID: got %q, want sess-123", sid)
	}
	if uid != "acct-456" {
		t.Errorf("accountUID: got %q, want acct-456", uid)
	}
	if expiry.IsZero() {
		t.Error("expected non-zero expiry")
	}
}

func TestGetSession_valid_naiveISO(t *testing.T) {
	s := &State{
		EBSessionID:     "sess-1",
		EBAccountUID:    "acct-1",
		EBSessionExpiry: "2025-06-15T10:30:00",
	}
	_, _, expiry, err := s.GetSession()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC)
	if !expiry.Equal(want) {
		t.Errorf("expiry: got %v, want %v", expiry, want)
	}
}

func TestGetSession_noSession(t *testing.T) {
	s := &State{}
	_, _, _, err := s.GetSession()
	if err == nil {
		t.Error("expected error when no session")
	}
}

func TestGetSession_missingSessionID(t *testing.T) {
	s := &State{EBAccountUID: "acct-1", EBSessionExpiry: "2025-01-01T00:00:00Z"}
	_, _, _, err := s.GetSession()
	if err == nil {
		t.Error("expected error when session ID is missing")
	}
}

func TestGetSession_missingAccountUID(t *testing.T) {
	s := &State{EBSessionID: "sess-1", EBSessionExpiry: "2025-01-01T00:00:00Z"}
	_, _, _, err := s.GetSession()
	if err == nil {
		t.Error("expected error when account UID is missing")
	}
}

func TestGetSession_invalidExpiry(t *testing.T) {
	s := &State{
		EBSessionID:     "sess-1",
		EBAccountUID:    "acct-1",
		EBSessionExpiry: "not-a-date",
	}
	_, _, _, err := s.GetSession()
	if err == nil {
		t.Error("expected error for invalid expiry format")
	}
}

func TestEarliestPendingDate_empty(t *testing.T) {
	s := &State{PendingMap: map[string]string{}}
	_, ok := s.EarliestPendingDate()
	if ok {
		t.Error("expected ok=false for empty pending map")
	}
}

func TestEarliestPendingDate_single(t *testing.T) {
	s := &State{PendingMap: map[string]string{
		"REF-1": "txn-1|2024-06-15",
	}}
	d, ok := s.EarliestPendingDate()
	if !ok {
		t.Fatal("expected ok=true")
	}
	want := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	if !d.Equal(want) {
		t.Errorf("got %v, want %v", d, want)
	}
}

func TestEarliestPendingDate_multiple(t *testing.T) {
	s := &State{PendingMap: map[string]string{
		"REF-1": "txn-1|2024-06-15",
		"REF-2": "txn-2|2024-04-01",
		"REF-3": "txn-3|2024-08-20",
	}}
	d, ok := s.EarliestPendingDate()
	if !ok {
		t.Fatal("expected ok=true")
	}
	want := time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)
	if !d.Equal(want) {
		t.Errorf("got %v, want %v (should be earliest)", d, want)
	}
}

func TestEarliestPendingDate_malformedValueSkipped(t *testing.T) {
	s := &State{PendingMap: map[string]string{
		"REF-bad":  "txn-bad",
		"REF-good": "txn-good|2024-09-01",
	}}
	d, ok := s.EarliestPendingDate()
	if !ok {
		t.Fatal("expected ok=true even with malformed value")
	}
	want := time.Date(2024, 9, 1, 0, 0, 0, 0, time.UTC)
	if !d.Equal(want) {
		t.Errorf("got %v, want %v", d, want)
	}
}

func TestEarliestPendingDate_fallbackKey(t *testing.T) {
	s := &State{PendingMap: map[string]string{
		"REF-A":            "txn-1|2024-06-15",
		"2024-03-10|-1200": "txn-2|2024-03-10",
	}}
	d, ok := s.EarliestPendingDate()
	if !ok {
		t.Fatal("expected ok=true")
	}
	want := time.Date(2024, 3, 10, 0, 0, 0, 0, time.UTC)
	if !d.Equal(want) {
		t.Errorf("got %v, want %v (should be earliest)", d, want)
	}
}

func TestPendingKey_refDisambiguatesSameAmountAndDate(t *testing.T) {
	s := &State{PendingMap: make(map[string]string)}
	st := newTestStore(t)

	if err := s.SetPending("REF-A", "txn-1", "2024-06-15", st); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPending("REF-B", "txn-2", "2024-06-15", st); err != nil {
		t.Fatal(err)
	}

	if len(s.PendingMap) != 2 {
		t.Fatalf("expected 2 pending entries, got %d", len(s.PendingMap))
	}

	idA, _ := splitPendingVal(s.PendingMap["REF-A"])
	idB, _ := splitPendingVal(s.PendingMap["REF-B"])
	if idA != "txn-1" {
		t.Errorf("REF-A: got %q, want txn-1", idA)
	}
	if idB != "txn-2" {
		t.Errorf("REF-B: got %q, want txn-2", idB)
	}

	if err := s.DeletePending("REF-A", st); err != nil {
		t.Fatal(err)
	}
	if _, exists := s.PendingMap["REF-A"]; exists {
		t.Error("REF-A should be deleted")
	}
	idB2, _ := splitPendingVal(s.PendingMap["REF-B"])
	if idB2 != "txn-2" {
		t.Error("REF-B should still exist after deleting REF-A")
	}
}

func TestPendingKey_emptyRefCollides(t *testing.T) {
	s := &State{PendingMap: make(map[string]string)}
	st := newTestStore(t)

	key := "2024-06-15|-50.00"

	if err := s.SetPending(key, "txn-1", "2024-06-15", st); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPending(key, "txn-2", "2024-06-15", st); err != nil {
		t.Fatal(err)
	}

	if len(s.PendingMap) != 1 {
		t.Fatalf("expected 1 pending entry (collision), got %d", len(s.PendingMap))
	}
	id, _ := splitPendingVal(s.PendingMap[key])
	if id != "txn-2" {
		t.Errorf("got %q, want txn-2 (last write wins)", id)
	}
}

func TestSplitPendingVal(t *testing.T) {
	id, date := splitPendingVal("txn-abc|2024-06-15")
	if id != "txn-abc" {
		t.Errorf("id: got %q, want txn-abc", id)
	}
	if date != "2024-06-15" {
		t.Errorf("date: got %q, want 2024-06-15", date)
	}
}

func TestSplitPendingVal_noSeparator(t *testing.T) {
	id, date := splitPendingVal("txn-abc")
	if id != "txn-abc" {
		t.Errorf("id: got %q, want txn-abc", id)
	}
	if date != "" {
		t.Errorf("date: got %q, want empty", date)
	}
}

func TestPruneImportedRefs_keepsRecent(t *testing.T) {
	recent := time.Now().UTC().AddDate(0, 0, -5).Format("2006-01-02")
	refs := map[string]string{"ref-1": recent}
	pruned := pruneImportedRefs(refs)
	if _, ok := pruned["ref-1"]; !ok {
		t.Error("expected recent ref to be kept")
	}
}

func TestPruneImportedRefs_removesOld(t *testing.T) {
	old := time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02")
	refs := map[string]string{"ref-old": old}
	pruned := pruneImportedRefs(refs)
	if _, ok := pruned["ref-old"]; ok {
		t.Error("expected old ref to be pruned")
	}
}

func TestPruneImportedRefs_boundary(t *testing.T) {

	exactly21 := time.Now().UTC().AddDate(0, 0, -21).Format("2006-01-02")
	refs := map[string]string{"ref-21": exactly21}
	pruned := pruneImportedRefs(refs)
	if _, ok := pruned["ref-21"]; !ok {
		t.Error("expected exactly-21-day-old ref to be kept (on boundary)")
	}
}

func TestPruneImportedRefs_mixed(t *testing.T) {
	now := time.Now().UTC()
	refs := map[string]string{
		"ref-old":    now.AddDate(0, 0, -25).Format("2006-01-02"),
		"ref-recent": now.AddDate(0, 0, -10).Format("2006-01-02"),
		"ref-today":  now.Format("2006-01-02"),
	}
	pruned := pruneImportedRefs(refs)
	if _, ok := pruned["ref-old"]; ok {
		t.Error("ref-old should be pruned")
	}
	if _, ok := pruned["ref-recent"]; !ok {
		t.Error("ref-recent should be kept")
	}
	if _, ok := pruned["ref-today"]; !ok {
		t.Error("ref-today should be kept")
	}
}

func TestPruneImportedRefs_empty(t *testing.T) {
	pruned := pruneImportedRefs(map[string]string{})
	if len(pruned) != 0 {
		t.Errorf("expected empty map, got %v", pruned)
	}
}

func TestLoadFromStore_empty(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	s, err := LoadFromStore(st)
	if err != nil {
		t.Fatalf("unexpected error on empty store: %v", err)
	}
	if s.PendingMap == nil {
		t.Error("PendingMap should be initialised")
	}
	if s.ImportedRefs == nil {
		t.Error("ImportedRefs should be initialised")
	}
}

func TestSaveAndLoadState_roundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	original := &State{
		EBSessionID:     "sid",
		EBAccountUID:    "uid",
		EBSessionExpiry: "2025-01-01T00:00:00Z",
		LastSyncDate:    "2024-06-01",
		PendingMap:      map[string]string{"REF-ABC": "txn-abc|2024-06-01"},
		ImportedRefs:    map[string]string{"REF-001": "2024-06-01"},
	}

	data, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var loaded State
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if loaded.EBSessionID != original.EBSessionID {
		t.Errorf("EBSessionID: got %q, want %q", loaded.EBSessionID, original.EBSessionID)
	}
	if loaded.LastSyncDate != original.LastSyncDate {
		t.Errorf("LastSyncDate: got %q, want %q", loaded.LastSyncDate, original.LastSyncDate)
	}
	if len(loaded.PendingMap) != 1 {
		t.Errorf("PendingMap: expected 1 entry, got %d", len(loaded.PendingMap))
	}
	if len(loaded.ImportedRefs) != 1 {
		t.Errorf("ImportedRefs: expected 1 entry, got %d", len(loaded.ImportedRefs))
	}
}

func TestLoadState_badJSON(t *testing.T) {

	bad := []byte("{not valid json")
	var s State
	err := json.Unmarshal(bad, &s)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestLoadState_initialisesNilMaps(t *testing.T) {

	raw := []byte(`{"eb_session_id":"x","eb_account_uid":"y","eb_session_expiry":"2025-01-01T00:00:00Z"}`)
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if s.PendingMap == nil {
		s.PendingMap = make(map[string]string)
	}
	if s.ImportedRefs == nil {
		s.ImportedRefs = make(map[string]string)
	}
	if s.PendingMap == nil || s.ImportedRefs == nil {
		t.Error("maps should be initialised")
	}
}

// --- write-through methods ---------------------------------------------------

func openStateStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestState_SetPending_writesThrough(t *testing.T) {
	st := openStateStore(t)
	s := &State{PendingMap: make(map[string]string)}
	if err := s.SetPending("k1", "txn-abc", "2024-06-15", st); err != nil {
		t.Fatalf("SetPending: %v", err)
	}
	id, date := splitPendingVal(s.PendingMap["k1"])
	if id != "txn-abc" {
		t.Errorf("memory id: got %q, want txn-abc", id)
	}
	if date != "2024-06-15" {
		t.Errorf("memory date: got %q, want 2024-06-15", date)
	}
	dbVal, ok, err := st.GetPendingTxnID("k1")
	if err != nil {
		t.Fatalf("GetPendingTxnID: %v", err)
	}
	if !ok {
		t.Error("expected found in DB")
	}
	if dbVal != "txn-abc|2024-06-15" {
		t.Errorf("DB: got %q, want txn-abc|2024-06-15", dbVal)
	}
}

func TestState_DeletePending_writesThrough(t *testing.T) {
	st := openStateStore(t)
	s := &State{PendingMap: map[string]string{"k1": "txn-abc|2024-06-15"}}
	_ = st.SetPending("k1", "txn-abc|2024-06-15")
	if err := s.DeletePending("k1", st); err != nil {
		t.Fatalf("DeletePending: %v", err)
	}
	if _, exists := s.PendingMap["k1"]; exists {
		t.Error("expected removed from memory")
	}
	_, ok, _ := st.GetPendingTxnID("k1")
	if ok {
		t.Error("expected removed from DB")
	}
}

func TestState_AddImportedRef_writesThrough(t *testing.T) {
	st := openStateStore(t)
	s := &State{ImportedRefs: make(map[string]string)}
	if err := s.AddImportedRef("REF-1", "2024-06-01", st); err != nil {
		t.Fatalf("AddImportedRef: %v", err)
	}
	if s.ImportedRefs["REF-1"] != "2024-06-01" {
		t.Error("expected in memory")
	}
	has, _ := st.HasImportedRef("REF-1")
	if !has {
		t.Error("expected in DB")
	}
}

func TestState_SetLastSyncDate_writesThrough(t *testing.T) {
	st := openStateStore(t)
	s := &State{}
	if err := s.SetLastSyncDate("2024-06-01", st); err != nil {
		t.Fatalf("SetLastSyncDate: %v", err)
	}
	if s.LastSyncDate != "2024-06-01" {
		t.Errorf("memory: got %q", s.LastSyncDate)
	}
	d, _ := st.GetLastSyncDate()
	if d != "2024-06-01" {
		t.Errorf("DB: got %q", d)
	}
}

func TestState_PruneImportedRefs_writesThrough(t *testing.T) {
	st := openStateStore(t)
	now := time.Now().UTC()
	oldDate := now.AddDate(0, 0, -30).Format("2006-01-02")
	newDate := now.Format("2006-01-02")
	_ = st.AddImportedRef("old", oldDate)
	_ = st.AddImportedRef("new", newDate)
	s := &State{ImportedRefs: map[string]string{"old": oldDate, "new": newDate}}
	if err := s.PruneImportedRefs(st); err != nil {
		t.Fatalf("PruneImportedRefs: %v", err)
	}
	if _, ok := s.ImportedRefs["old"]; ok {
		t.Error("expected old removed from memory")
	}
	if _, ok := s.ImportedRefs["new"]; !ok {
		t.Error("expected new kept in memory")
	}
}

func TestState_Reload_updatesSession(t *testing.T) {
	st := openStateStore(t)
	_, _ = st.AddBankAccount("sess-new", "acct-new", "Bank", "DE", "", "", "2025-01-01T00:00:00Z")
	s := &State{}
	if err := s.Reload(st); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if s.EBSessionID != "sess-new" {
		t.Errorf("EBSessionID: got %q, want sess-new", s.EBSessionID)
	}
	if s.EBAccountUID != "acct-new" {
		t.Errorf("EBAccountUID: got %q, want acct-new", s.EBAccountUID)
	}
}

func TestLoadFromStore_withData(t *testing.T) {
	st := openStateStore(t)
	_, _ = st.AddBankAccount("sess-1", "acct-1", "Bank", "DE", "", "", "2025-12-31T00:00:00Z")
	_ = st.SetLastSyncDate("2024-06-01")
	_ = st.SetPending("REF-1-P", "txn-1|2024-06-01")
	_ = st.AddImportedRef("REF-1", "2024-06-01")
	s, err := LoadFromStore(st)
	if err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}
	if s.EBSessionID != "sess-1" {
		t.Errorf("EBSessionID: got %q", s.EBSessionID)
	}
	if s.EBAccountUID != "acct-1" {
		t.Errorf("EBAccountUID: got %q", s.EBAccountUID)
	}
	if s.LastSyncDate != "2024-06-01" {
		t.Errorf("LastSyncDate: got %q", s.LastSyncDate)
	}
	if s.PendingMap["REF-1-P"] != "txn-1|2024-06-01" {
		t.Errorf("PendingMap[REF-1-P]: got %q", s.PendingMap["REF-1-P"])
	}
	if s.ImportedRefs["REF-1"] != "2024-06-01" {
		t.Errorf("ImportedRefs[REF-1]: got %q", s.ImportedRefs["REF-1"])
	}
}
