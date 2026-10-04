package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"bankingsync/store"
)

func pendingPick(st *store.Store, accounts string) {
	_ = st.SetSetting("pending_auth_session_id", "sess-1")
	_ = st.SetSetting("pending_auth_accounts", accounts)
	_ = st.SetSetting("pending_auth_expiry", "2027-01-01T00:00:00Z")
	_ = st.SetSetting("pending_auth_bank_name", "TestBank")
	_ = st.SetSetting("pending_auth_bank_country", "DE")
}

func TestPickAccount_connectsSeveralAccountsWithOneAuthorisation(t *testing.T) {
	srv, st := newTestServer(t)
	pendingPick(st, `[{"uid":"checking","name":"Checking","iban":"DE63111111111111111111"},{"uid":"savings","name":"Savings","iban":"DE90222222222222222222"},{"uid":"loan","name":"Loan"}]`)

	w := post(t, srv, "/pick-account", url.Values{
		"account_uid":              {"checking", "savings"},
		"actual_account_checking":  {" Daily expenses "},
		"start_sync_date_checking": {"2026-01-01"},
		"actual_account_savings":   {"Emergency fund"},
		"start_sync_date_savings":  {" "},
		"actual_account_loan":      {"Not wanted"},
	})
	if w.Code != http.StatusFound {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}

	accounts, _ := st.GetAllBankAccounts()
	if len(accounts) != 2 {
		t.Fatalf("%d accounts, want the two selected", len(accounts))
	}
	defaultStart := time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02")
	want := map[string]store.BankAccount{
		"checking": {ActualAccount: "Daily expenses", StartSyncDate: "2026-01-01", IBAN: "DE63111111111111111111"},
		"savings":  {ActualAccount: "Emergency fund", StartSyncDate: defaultStart, IBAN: "DE90222222222222222222"},
	}
	for _, a := range accounts {
		w, ok := want[a.AccountUID]
		if !ok {
			t.Errorf("unexpected account %q", a.AccountUID)
			continue
		}
		if a.SessionID != "sess-1" {
			t.Errorf("%s on session %q; both must share the one authorisation", a.AccountUID, a.SessionID)
		}
		if a.ActualAccount != w.ActualAccount || a.StartSyncDate != w.StartSyncDate || a.IBAN != w.IBAN {
			t.Errorf("%s: %q from %q with %q, want %q from %q with %q",
				a.AccountUID, a.ActualAccount, a.StartSyncDate, a.IBAN, w.ActualAccount, w.StartSyncDate, w.IBAN)
		}
	}
}

func TestPickAccount_needsAtLeastOneAccount(t *testing.T) {
	srv, st := newTestServer(t)
	pendingPick(st, `[{"uid":"checking"},{"uid":"savings"}]`)

	w := post(t, srv, "/pick-account", url.Values{"actual_account_checking": {"Daily"}})

	if !strings.Contains(w.Body.String(), "Please select an account.") {
		t.Errorf("an empty selection was not refused:\n%s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `value="Daily"`) {
		t.Error("the name already typed was lost on the refused submit")
	}
	if accounts, _ := st.GetAllBankAccounts(); len(accounts) != 0 {
		t.Errorf("%d accounts created from an empty selection", len(accounts))
	}
}

func TestPickAccount_refusesAnAccountAlreadyConnectedOnThisSession(t *testing.T) {
	srv, st := newTestServer(t)
	_, _ = st.AddBankAccount(store.NewBankAccount{
		SessionID: "sess-1", AccountUID: "checking", BankName: "TestBank", BankCountry: "DE",
		SessionExpiry: "2027-01-01T00:00:00Z",
	})
	pendingPick(st, `[{"uid":"checking"}]`)

	w := post(t, srv, "/pick-account", url.Values{"account_uid": {"checking"}})

	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400 for an account this authorisation already connected", w.Code)
	}
	if accounts, _ := st.GetAllBankAccounts(); len(accounts) != 1 {
		t.Errorf("%d accounts, want the one already there", len(accounts))
	}
}

func TestPickAccount_severalOfferedAccountsAreNotAllNamedAfterTheDefault(t *testing.T) {
	t.Setenv("ACTUAL_ACCOUNT", "Main")

	t.Run("several offered", func(t *testing.T) {
		srv, st := newTestServer(t)
		pendingPick(st, `[{"uid":"checking","name":"Checking"},{"uid":"savings","name":"Savings"}]`)

		page := get(t, srv, "/pick-account").Body.String()
		if strings.Contains(page, `value="Main"`) {
			t.Error("every account is pre-filled with the default; submitted as is, all of them import into one budget account")
		}

		post(t, srv, "/pick-account", url.Values{"account_uid": {"checking", "savings"}})
		accounts, _ := st.GetAllBankAccounts()
		names := map[string]bool{}
		for _, a := range accounts {
			names[a.ActualAccount] = true
		}
		if len(accounts) != 2 || len(names) != 2 || names["Main"] {
			t.Errorf("accounts named %v, want one name each and none of them the default", names)
		}
	})

	t.Run("one offered", func(t *testing.T) {
		srv, st := newTestServer(t)
		pendingPick(st, `[{"uid":"checking","name":"Checking"}]`)

		post(t, srv, "/pick-account", url.Values{"account_uid": {"checking"}})
		accounts, _ := st.GetAllBankAccounts()
		if len(accounts) != 1 || accounts[0].ActualAccount != "Main" {
			t.Errorf("a single account no longer imports into the configured default: %+v", accounts)
		}
	})
}
