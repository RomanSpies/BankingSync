package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"bankingsync/store"
)

func backendTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedDedupeState(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.AddImportedRef(1, "ref-1", "2026-07-01"); err != nil {
		t.Fatalf("AddImportedRef: %v", err)
	}
	if err := st.AddImportedRef(1, "ref-2", "2026-07-02"); err != nil {
		t.Fatalf("AddImportedRef: %v", err)
	}
	if err := st.SetPending(1, "key-1", "txn-1|2026-07-01"); err != nil {
		t.Fatalf("SetPending: %v", err)
	}
}

func dedupeStateSize(t *testing.T, st *store.Store) (int, int) {
	t.Helper()
	refs, err := st.AllImportedRefs()
	if err != nil {
		t.Fatalf("AllImportedRefs: %v", err)
	}
	pending, err := st.AllPendingMap()
	if err != nil {
		t.Fatalf("AllPendingMap: %v", err)
	}
	return len(refs[1]), len(pending[1])
}

func TestResolveBackend_firstRunPersistsChoice(t *testing.T) {
	t.Setenv("BUDGET_BACKEND", "actual")
	st := backendTestStore(t)

	got, err := resolveBackend(st)
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if got != backendActual {
		t.Errorf("got %q, want %q", got, backendActual)
	}
	if v, _ := st.GetSetting(backendSettingKey); v != backendActual {
		t.Errorf("setting %s: got %q, want %q", backendSettingKey, v, backendActual)
	}
}

func TestResolveBackend_unchangedKeepsState(t *testing.T) {
	t.Setenv("BUDGET_BACKEND", "actual")
	st := backendTestStore(t)
	if err := st.SetSetting(backendSettingKey, backendActual); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	seedDedupeState(t, st)

	if _, err := resolveBackend(st); err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}

	refs, pending := dedupeStateSize(t, st)
	if refs != 2 || pending != 1 {
		t.Errorf("state was touched without a backend change: %d refs, %d pending", refs, pending)
	}
}

func TestResolveBackend_changeWithoutMigrateFlagRefuses(t *testing.T) {
	t.Setenv("BUDGET_BACKEND", "actual")
	t.Setenv("BUDGET_BACKEND_MIGRATE", "")
	st := backendTestStore(t)
	if err := st.SetSetting(backendSettingKey, "firefly"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	seedDedupeState(t, st)

	_, err := resolveBackend(st)
	if err == nil {
		t.Fatal("a silent purge on backend change would lose the dedupe state; startup must refuse")
	}
	if !strings.Contains(err.Error(), "BUDGET_BACKEND_MIGRATE") {
		t.Errorf("the error must name the opt-out: %v", err)
	}

	refs, pending := dedupeStateSize(t, st)
	if refs != 2 || pending != 1 {
		t.Errorf("state must survive a refused start: %d refs, %d pending", refs, pending)
	}
	if v, _ := st.GetSetting(backendSettingKey); v != "firefly" {
		t.Errorf("the stored backend must not move on a refused start, got %q", v)
	}
}

func TestResolveBackend_changeWithMigrateFlagPurges(t *testing.T) {
	t.Setenv("BUDGET_BACKEND", "actual")
	t.Setenv("BUDGET_BACKEND_MIGRATE", "true")
	st := backendTestStore(t)
	if err := st.SetSetting(backendSettingKey, "firefly"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	seedDedupeState(t, st)

	got, err := resolveBackend(st)
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if got != backendActual {
		t.Errorf("got %q, want %q", got, backendActual)
	}

	refs, pending := dedupeStateSize(t, st)
	if refs != 0 || pending != 0 {
		t.Errorf("dedupe state must be purged: %d refs, %d pending", refs, pending)
	}
	if v, _ := st.GetSetting(backendSettingKey); v != backendActual {
		t.Errorf("setting %s: got %q, want %q", backendSettingKey, v, backendActual)
	}
}

func TestResolveBackend_rejectsUnknown(t *testing.T) {
	for _, value := range []string{"ynab", "", "actualbudget"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("BUDGET_BACKEND", value)
			st := backendTestStore(t)

			if value == "" {
				if _, err := resolveBackend(st); err != nil {
					t.Fatalf("an unset variable must default to actual, got %v", err)
				}
				return
			}
			_, err := resolveBackend(st)
			if err == nil {
				t.Fatalf("BUDGET_BACKEND=%q must be rejected at startup", value)
			}
			if !strings.Contains(err.Error(), "not a known backend") {
				t.Errorf("error %q should name the problem", err.Error())
			}
		})
	}
}

func TestResolveBackend_acceptsFirefly(t *testing.T) {
	t.Setenv("BUDGET_BACKEND", "firefly")
	st := backendTestStore(t)

	got, err := resolveBackend(st)
	if err != nil {
		t.Fatalf("firefly is implemented and must be selectable: %v", err)
	}
	if got != backendFirefly {
		t.Errorf("got %q, want %q", got, backendFirefly)
	}
}

func TestDialFirefly_requiresURLAndToken(t *testing.T) {
	t.Setenv("BUDGET_BACKEND", "firefly")
	t.Setenv("FIREFLY_URL", "")
	t.Setenv("FIREFLY_TOKEN", "")

	if _, err := dialBackend(context.Background()); err == nil {
		t.Fatal("missing credentials must fail at connect time, not silently produce a broken client")
	}

	t.Setenv("FIREFLY_URL", "http://firefly.invalid")
	if _, err := dialBackend(context.Background()); err == nil {
		t.Fatal("a missing token must still fail")
	}

	t.Setenv("FIREFLY_TOKEN", "tok")
	if _, err := dialBackend(context.Background()); err != nil {
		t.Fatalf("with both set the backend must build: %v", err)
	}
}

func TestResolveBackend_normalisesValue(t *testing.T) {
	t.Setenv("BUDGET_BACKEND", "  ACTUAL  ")
	st := backendTestStore(t)

	got, err := resolveBackend(st)
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if got != backendActual {
		t.Errorf("got %q, want %q", got, backendActual)
	}
}

func TestResetImportState_reportsCounts(t *testing.T) {
	st := backendTestStore(t)
	seedDedupeState(t, st)

	refs, pending, err := st.ResetImportState()
	if err != nil {
		t.Fatalf("PurgeDedupeState: %v", err)
	}
	if refs != 2 {
		t.Errorf("refs: got %d, want 2", refs)
	}
	if pending != 1 {
		t.Errorf("pending: got %d, want 1", pending)
	}
}

func TestResetImportState_clearsSyncWatermarks(t *testing.T) {
	st := backendTestStore(t)
	seedDedupeState(t, st)

	id, err := st.AddBankAccount(store.NewBankAccount{
		SessionID: "sess", AccountUID: "uid", BankName: "Bank", BankCountry: "DE",
		ActualAccount: "Checking",
	})
	if err != nil {
		t.Fatalf("AddBankAccount: %v", err)
	}
	if err := st.SetBankAccountLastSyncDate(id, "2026-08-13"); err != nil {
		t.Fatalf("SetBankAccountLastSyncDate: %v", err)
	}
	if err := st.SetLastSyncDate("2026-08-13"); err != nil {
		t.Fatalf("SetLastSyncDate: %v", err)
	}

	if _, _, err := st.ResetImportState(); err != nil {
		t.Fatalf("ResetImportState: %v", err)
	}

	accounts, err := st.GetAllBankAccounts()
	if err != nil {
		t.Fatalf("GetAllBankAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts: got %d, want 1", len(accounts))
	}
	if accounts[0].LastSyncDate != "" {
		t.Errorf("account watermark survived the reset: %q — the new backend would "+
			"only receive transactions newer than the old backend's last sync",
			accounts[0].LastSyncDate)
	}

	global, err := st.GetLastSyncDate()
	if err != nil {
		t.Fatalf("GetLastSyncDate: %v", err)
	}
	if global != "" {
		t.Errorf("global watermark survived the reset: %q", global)
	}
}

func TestDialActual_errorNamesTheBackendInsteadOfKillingTheProcess(t *testing.T) {
	t.Setenv("ACTUAL_URL", "")
	t.Setenv("ACTUAL_PASSWORD", "")
	t.Setenv("ACTUAL_SYNC_ID", "")

	_, err := dialActual(context.Background())
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	for _, want := range []string{"ACTUAL_URL", "BUDGET_BACKEND=actual"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q — an operator who set "+
				"BUDGET_BACKEND=firefly cannot tell why Actual was dialled at all",
				err, want)
		}
	}
}

func TestDialFirefly_errorNamesTheBackend(t *testing.T) {
	t.Setenv("FIREFLY_URL", "")

	_, err := dialFirefly(context.Background())
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !strings.Contains(err.Error(), "BUDGET_BACKEND=firefly") {
		t.Errorf("error %q does not name the backend", err)
	}
}

func TestDialBackend_fireflyNeverReachesActual(t *testing.T) {
	t.Setenv("BUDGET_BACKEND", "firefly")
	t.Setenv("ACTUAL_URL", "")
	t.Setenv("FIREFLY_URL", "")

	_, err := dialBackend(context.Background())
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if strings.Contains(err.Error(), "ACTUAL_") {
		t.Errorf("BUDGET_BACKEND=firefly still complained about an Actual variable: %v", err)
	}
}
