package web

import (
	"encoding/json"
	"strconv"
	"testing"

	"bankingsync/enablebanking"
	"bankingsync/store"
)

func addRevolut(t *testing.T, st *store.Store, a store.NewBankAccount) int64 {
	t.Helper()
	if a.BankName == "" {
		a.BankName, a.BankCountry = "Revolut", "LT"
	}
	if a.SessionID == "" {
		a.SessionID = "old-sess"
	}
	a.SessionExpiry = "2026-09-20T00:00:00Z"
	id, err := st.AddBankAccount(a)
	if err != nil {
		t.Fatalf("AddBankAccount: %v", err)
	}
	return id
}

func authorisingAt(st *store.Store, bank, country string) {
	_ = st.SetSetting("pending_bank_name", bank)
	_ = st.SetSetting("pending_bank_country", country)
	_ = st.SetSetting("pending_session_expiry", "2027-03-25T00:00:00Z")
}

func renewingAt(st *store.Store, id int64) {
	authorisingAt(st, "Revolut", "LT")
	_ = st.SetSetting("pending_renew_account_id", strconv.FormatInt(id, 10))
}

func accountByID(t *testing.T, st *store.Store, id int64) store.BankAccount {
	t.Helper()
	accounts, _ := st.GetAllBankAccounts()
	for _, a := range accounts {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("no account %d", id)
	return store.BankAccount{}
}

func offeredForPicking(t *testing.T, st *store.Store) []string {
	t.Helper()
	raw, _ := st.GetSetting("pending_auth_accounts")
	var accounts []enablebanking.SessionAccount
	if err := json.Unmarshal([]byte(raw), &accounts); err != nil {
		t.Fatalf("pending_auth_accounts %q: %v", raw, err)
	}
	uids := make([]string, 0, len(accounts))
	for _, a := range accounts {
		uids = append(uids, a.EffectiveUID())
	}
	return uids
}

func TestFinishAuth_aNewAuthorisationCarriesTheBanksConnectedAccounts(t *testing.T) {
	srv, st := newTestServer(t)
	id := addRevolut(t, st, store.NewBankAccount{AccountUID: "old-a", IdentificationHash: "h-a"})
	authorisingAt(st, "Revolut", "LT")

	w := finish(t, srv, &enablebanking.SessionResponse{SessionID: "new-sess", Accounts: []enablebanking.SessionAccount{
		{UID: "new-a", IdentificationHash: "h-a"},
		{UID: "new-b", IdentificationHash: "h-b"},
	}})

	if loc := w.Header().Get("Location"); loc != "/pick-account" {
		t.Fatalf("redirected to %q, want /pick-account for the account not yet connected", loc)
	}
	if a := accountByID(t, st, id); a.SessionID != "new-sess" || a.AccountUID != "new-a" {
		t.Errorf("connected account left on session %q as %q; the bank has just ended that session", a.SessionID, a.AccountUID)
	}
	if got := offeredForPicking(t, st); len(got) != 1 || got[0] != "new-b" {
		t.Errorf("picker offers %v, want only new-b: new-a is already connected", got)
	}
}

func TestFinishAuth_anotherBanksAccountsStayWhereTheyAre(t *testing.T) {
	srv, st := newTestServer(t)
	id := addRevolut(t, st, store.NewBankAccount{BankName: "N26", BankCountry: "DE", AccountUID: "old-a", IdentificationHash: "h-a"})
	authorisingAt(st, "Revolut", "LT")

	finish(t, srv, &enablebanking.SessionResponse{SessionID: "new-sess", Accounts: []enablebanking.SessionAccount{
		{UID: "new-a", IdentificationHash: "h-a"},
	}})

	if a := accountByID(t, st, id); a.SessionID != "old-sess" {
		t.Errorf("an N26 account was moved onto a Revolut authorisation")
	}
}

func TestFinishAuth_anAccountWithoutIdentifiersIsNotMovedByGuess(t *testing.T) {
	srv, st := newTestServer(t)
	id := addRevolut(t, st, store.NewBankAccount{AccountUID: "old-a"})
	authorisingAt(st, "Revolut", "LT")

	w := finish(t, srv, &enablebanking.SessionResponse{SessionID: "new-sess", Accounts: []enablebanking.SessionAccount{
		{UID: "new-b"},
	}})

	if a := accountByID(t, st, id); a.SessionID != "old-sess" {
		t.Errorf("an account with nothing to recognise it by was moved onto the only account offered")
	}
	if loc := w.Header().Get("Location"); loc != "/pick-account" {
		t.Errorf("redirected to %q, want /pick-account", loc)
	}
}

func TestFinishAuth_renewingOneAccountRenewsItsSiblings(t *testing.T) {
	srv, st := newTestServer(t)
	x := addRevolut(t, st, store.NewBankAccount{AccountUID: "old-x", IdentificationHash: "h-x"})
	y := addRevolut(t, st, store.NewBankAccount{AccountUID: "old-y", IdentificationHash: "h-y"})
	renewingAt(st, x)

	w := finish(t, srv, &enablebanking.SessionResponse{SessionID: "new-sess", Accounts: []enablebanking.SessionAccount{
		{UID: "new-x", IdentificationHash: "h-x"},
		{UID: "new-y", IdentificationHash: "h-y"},
	}})

	if loc := w.Header().Get("Location"); loc != "/status" {
		t.Fatalf("redirected to %q, want /status", loc)
	}
	for id, uid := range map[int64]string{x: "new-x", y: "new-y"} {
		if a := accountByID(t, st, id); a.SessionID != "new-sess" || a.AccountUID != uid {
			t.Errorf("account %d on session %q as %q, want new-sess as %s", id, a.SessionID, a.AccountUID, uid)
		}
	}
}

func TestFinishAuth_aSiblingsAccountIsNotOfferedForTheRenewal(t *testing.T) {
	t.Run("the one account left is the renewed one", func(t *testing.T) {
		srv, st := newTestServer(t)
		x := addRevolut(t, st, store.NewBankAccount{AccountUID: "old-x"})
		y := addRevolut(t, st, store.NewBankAccount{AccountUID: "old-y", IdentificationHash: "h-y"})
		renewingAt(st, x)

		w := finish(t, srv, &enablebanking.SessionResponse{SessionID: "new-sess", Accounts: []enablebanking.SessionAccount{
			{UID: "new-y", IdentificationHash: "h-y"},
			{UID: "new-z", IdentificationHash: "h-z"},
		}})

		if loc := w.Header().Get("Location"); loc != "/status" {
			t.Fatalf("redirected to %q, want /status", loc)
		}
		if a := accountByID(t, st, x); a.AccountUID != "new-z" {
			t.Errorf("renewed account is %q, want new-z: new-y belongs to its sibling", a.AccountUID)
		}
		if a := accountByID(t, st, y); a.AccountUID != "new-y" {
			t.Errorf("sibling is %q, want new-y", a.AccountUID)
		}
	})

	t.Run("the picker leaves the sibling's account out", func(t *testing.T) {
		srv, st := newTestServer(t)
		x := addRevolut(t, st, store.NewBankAccount{AccountUID: "old-x"})
		addRevolut(t, st, store.NewBankAccount{AccountUID: "old-y", IdentificationHash: "h-y"})
		renewingAt(st, x)

		w := finish(t, srv, &enablebanking.SessionResponse{SessionID: "new-sess", Accounts: []enablebanking.SessionAccount{
			{UID: "new-y", IdentificationHash: "h-y"},
			{UID: "new-z1", IdentificationHash: "h-z1"},
			{UID: "new-z2", IdentificationHash: "h-z2"},
		}})

		if loc := w.Header().Get("Location"); loc != "/pick-account" {
			t.Fatalf("redirected to %q, want /pick-account", loc)
		}
		got := offeredForPicking(t, st)
		if len(got) != 2 || got[0] != "new-z1" || got[1] != "new-z2" {
			t.Errorf("renewal picker offers %v, want new-z1 and new-z2: choosing new-y would put two accounts on one", got)
		}
		if a := accountByID(t, st, x); a.SessionID != "old-sess" {
			t.Errorf("the account being renewed was changed before anybody chose")
		}
	})
}

func TestFinishAuth_anAuthorisationOfOnlyConnectedAccountsGoesToStatus(t *testing.T) {
	srv, st := newTestServer(t)
	id := addRevolut(t, st, store.NewBankAccount{AccountUID: "old-a", IdentificationHash: "h-a"})
	authorisingAt(st, "Revolut", "LT")

	w := finish(t, srv, &enablebanking.SessionResponse{SessionID: "new-sess", Accounts: []enablebanking.SessionAccount{
		{UID: "new-a", IdentificationHash: "h-a"},
	}})

	if loc := w.Header().Get("Location"); loc != "/status" {
		t.Errorf("redirected to %q, want /status: there is nothing left to pick", loc)
	}
	if a := accountByID(t, st, id); a.SessionID != "new-sess" {
		t.Errorf("connected account not moved")
	}
	if v, _ := st.GetSetting("pending_auth_session_id"); v != "" {
		t.Errorf("a picker was prepared for nothing: pending_auth_session_id %q", v)
	}
}
