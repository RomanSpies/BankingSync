package main

import (
	"testing"

	"bankingsync/store"
)

func holdAuthorisationBesideAnother(t *testing.T, h *harness) store.MatchReview {
	t.Helper()
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(14))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{pendingTxnPayee("", daysAgo(12), "20.00", "EDEKA AKTIV MARKT")}})
	h.syncer.run()
	h.eb.setPages([][]map[string]any{{pendingTxnPayee("", daysAgo(8), "21.43", "Visa Edeka Aktiv Markt")}})
	h.syncer.run()

	reviews, err := h.st.GetMatchReviews()
	if err != nil || len(reviews) != 1 || reviews[0].Cleared {
		t.Fatalf("setup: want the Visa authorisation held against the earlier EDEKA one, got %+v (%v)", reviews, err)
	}
	return reviews[0]
}

func TestSync_aBookingSettlesItsHeldAuthorisation(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		_ = holdAuthorisationBesideAnother(t, h)

		h.eb.setPages([][]map[string]any{{bookedTxnPayee("", daysAgo(1), "21.43", "EDEKA AKTIV MARKT Gelnhausen")}})
		h.syncer.run()

		if n, _ := h.st.CountMatchReviews(); n != 0 {
			t.Fatalf("%d reviews left; the booking should have settled its held authorisation", n)
		}
		txns := h.actualTxns(t)
		if len(txns) != 2 {
			t.Fatalf("%d rows, want the earlier EDEKA authorisation and one row for the settled pair", len(txns))
		}
		found := false
		for _, tx := range txns {
			if tx.AmountCents != -2143 {
				continue
			}
			found = true
			if !tx.Cleared {
				t.Errorf("the settled pair is not booked")
			}
			if got := tx.Date.Format("2006-01-02"); got != daysAgo(8) {
				t.Errorf("the settled pair is dated %s, want the authorisation's day %s", got, daysAgo(8))
			}
		}
		if !found {
			t.Fatal("no row at 21.43 for the settled pair")
		}
		for acct, keys := range h.syncer.state.HeldKeys {
			if len(keys) != 0 {
				t.Errorf("account %d still has held keys %v", acct, keys)
			}
		}
	})
}

func TestSync_aSettledHeldAuthorisationSeenAgainIsNotImported(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		auth := holdAuthorisationBesideAnother(t, h)
		booking := bookedTxnPayee("", daysAgo(1), "21.43", "EDEKA AKTIV MARKT Gelnhausen")
		h.eb.setPages([][]map[string]any{{booking}})
		h.syncer.run()
		if !h.syncer.state.Consumed(auth.BankAccountID, auth.PendingKey) {
			t.Fatalf("the settled authorisation's key %q was not recorded as consumed by its booking", auth.PendingKey)
		}

		h.eb.setPages([][]map[string]any{{
			pendingTxnPayee("", daysAgo(8), "21.43", "Visa Edeka Aktiv Markt"),
			booking,
		}})
		h.syncer.run()

		if n := len(h.actualTxns(t)); n != 2 {
			t.Fatalf("%d rows after the feed repeated itself, want 2", n)
		}
		if n, _ := h.st.CountMatchReviews(); n != 0 {
			t.Fatalf("%d reviews; the repeated authorisation was held again", n)
		}
	})
}

func TestSync_aDriftedBookingIsHeldBesideItsHeldAuthorisation(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		_ = holdAuthorisationBesideAnother(t, h)

		h.eb.setPages([][]map[string]any{{bookedTxnPayee("", daysAgo(1), "24.90", "EDEKA AKTIV MARKT Gelnhausen")}})
		h.syncer.run()

		if n, _ := h.st.CountMatchReviews(); n != 2 {
			t.Fatalf("%d reviews; a drifted booking a week out is a question, and its authorisation waits with it", n)
		}
		if n := len(h.actualTxns(t)); n != 1 {
			t.Fatalf("%d rows; nothing of the pair may be written while it is in question", n)
		}
	})
}
