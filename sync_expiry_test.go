package main

import (
	"strings"
	"testing"
	"time"

	"bankingsync/store"
)

func TestSync_anExpiringSessionIsAnnouncedOnceForAllItsAccounts(t *testing.T) {
	h := newHarness(t)
	sent := captureEmails(t)
	expiry := time.Now().UTC().AddDate(0, 0, 5).Add(12 * time.Hour).Format(time.RFC3339)
	for _, a := range []struct{ session, uid, name string }{
		{"shared", "checking", "Checking"},
		{"shared", "savings", "Savings"},
		{"other", "card", "Card"},
	} {
		if _, err := h.st.AddBankAccount(store.NewBankAccount{
			SessionID: a.session, AccountUID: a.uid, BankName: "TestBank", BankCountry: "DE",
			ActualAccount: h.acct(a.name), SessionExpiry: expiry,
		}); err != nil {
			t.Fatalf("add bank account: %v", err)
		}
	}
	h.reloadState(t)

	h.syncer.run()
	if n := countSubjects(*sent, "session expires in"); n != 2 {
		t.Errorf("%d expiry emails, want 2: one per session, not one per account", n)
	}

	h.syncer.run()
	if n := countSubjects(*sent, "session expires in"); n != 2 {
		t.Errorf("%d expiry emails after a second run, want still 2", n)
	}
}

func countSubjects(sent []string, part string) int {
	n := 0
	for _, m := range sent {
		if subject, _, _ := strings.Cut(m, "\n"); strings.Contains(subject, part) {
			n++
		}
	}
	return n
}
