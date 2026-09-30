package main

import (
	"context"
	"strconv"
	"testing"

	"bankingsync/store"
	"bankingsync/web"
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

func holdDriftedPair(t *testing.T, h *harness) (auth store.MatchReview, booking, authorisation int) {
	t.Helper()
	auth = holdAuthorisationBesideAnother(t, h)
	h.eb.setPages([][]map[string]any{{bookedTxnPayee("", daysAgo(1), "24.90", "EDEKA AKTIV MARKT Gelnhausen")}})
	h.syncer.run()

	items, err := h.syncer.HeldTransactions(context.Background())
	if err != nil || len(items) != 2 {
		t.Fatalf("setup: review page %d items, %v; want the authorisation and its drifted booking", len(items), err)
	}
	booking, authorisation = -1, -1
	for i, it := range items {
		if it.ID == auth.ID {
			authorisation = i
		} else {
			booking = i
		}
	}
	if booking < 0 || authorisation < 0 {
		t.Fatalf("setup: could not tell the booking from the authorisation on the page")
	}
	return auth, booking, authorisation
}

func TestReviewQueue_aHeldBookingOffersItsHeldAuthorisation(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		auth, b, a := holdDriftedPair(t, h)
		items, _ := h.syncer.HeldTransactions(context.Background())

		offered := false
		for _, c := range items[b].Candidates {
			if c.Held {
				offered = true
				if c.ID != heldCandidatePrefix+strconv.FormatInt(auth.ID, 10) {
					t.Errorf("the held candidate is %s, want the authorisation %d", c.ID, auth.ID)
				}
			}
		}
		if !offered {
			t.Fatalf("the booking's review does not offer its held authorisation: %+v", items[b].Candidates)
		}
		for _, c := range items[a].Candidates {
			if c.Held {
				t.Errorf("the authorisation's review offers a held item %s; only a booking settles an authorisation", c.ID)
			}
		}
		if !items[a].BookingWaiting {
			t.Error("the authorisation's review does not say its booking is waiting too")
		}
		if items[b].BookingWaiting {
			t.Error("the booking's review claims a booking is waiting for it")
		}
	})
}

func TestReviewQueue_choosingTheHeldAuthorisationImportsOneRowAndClearsBoth(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		auth, b, _ := holdDriftedPair(t, h)
		items, _ := h.syncer.HeldTransactions(context.Background())
		var pick *web.ReviewCandidate
		for i := range items[b].Candidates {
			if items[b].Candidates[i].Held {
				pick = &items[b].Candidates[i]
			}
		}
		if pick == nil {
			t.Fatal("setup: no held candidate to choose")
		}

		if err := h.syncer.ResolveHeld(context.Background(), items[b].ID, pick.ID, pick.Percent,
			h.syncer.matchPolicy("").Version()); err != nil {
			t.Fatalf("ResolveHeld: %v", err)
		}

		if n, _ := h.st.CountMatchReviews(); n != 0 {
			t.Fatalf("%d reviews left; choosing the authorisation answers both", n)
		}
		var pair string
		for _, tx := range h.actualTxns(t) {
			if tx.AmountCents == -2490 {
				pair = tx.ID
				if !tx.Cleared || tx.Date.Format("2006-01-02") != daysAgo(8) {
					t.Errorf("the pair row %+v; want booked, dated like the authorisation", tx)
				}
			}
			if tx.AmountCents == -2143 {
				t.Errorf("the authorisation was imported on its own as well")
			}
		}
		if pair == "" {
			t.Fatal("no row at the booking's amount")
		}

		labelled, err := h.st.GetLabelledMatchDecisions(100)
		if err != nil {
			t.Fatalf("GetLabelledMatchDecisions: %v", err)
		}
		var sawBooking, sawAuth bool
		for _, d := range labelled {
			switch d.PendingKey {
			case auth.PendingKey:
				sawAuth = true
				if *d.Truth {
					t.Error("the authorisation's own decision was confirmed; its best candidate was the other EDEKA row, which the answer refutes")
				}
			default:
				sawBooking = true
				if !*d.Truth || d.CandidateID != pair {
					t.Errorf("the booking's decision: truth %v candidate %q; want confirmed against the new row %q", *d.Truth, d.CandidateID, pair)
				}
				if d.PayeeLevel != "truncated" {
					t.Errorf("the booking's decision carries payee level %q; want the pair's own, truncated", d.PayeeLevel)
				}
			}
		}
		if !sawBooking || !sawAuth {
			t.Fatalf("labels: booking %v authorisation %v; one answer settles both decisions", sawBooking, sawAuth)
		}
		if !h.syncer.state.Consumed(auth.BankAccountID, auth.PendingKey) {
			t.Error("the settled authorisation was not recorded as consumed by the booking")
		}
	})
}

func TestReviewQueue_aHeldAuthorisationAlreadyDecidedIsRefused(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		_, b, a := holdDriftedPair(t, h)
		items, _ := h.syncer.HeldTransactions(context.Background())
		var pick web.ReviewCandidate
		for _, c := range items[b].Candidates {
			if c.Held {
				pick = c
			}
		}
		version := h.syncer.matchPolicy("").Version()
		if err := h.syncer.ResolveHeld(context.Background(), items[a].ID, "", 0, version); err != nil {
			t.Fatalf("setup: resolving the authorisation as new: %v", err)
		}
		before := len(h.actualTxns(t))

		err := h.syncer.ResolveHeld(context.Background(), items[b].ID, pick.ID, pick.Percent, version)
		if err == nil {
			t.Fatal("a page drawn before the authorisation was decided settled it a second time")
		}
		if after := len(h.actualTxns(t)); after != before {
			t.Fatalf("%d rows after a refused answer, want %d", after, before)
		}
	})
}

func TestSync_aPendingIsNotHeldAgainstARowTheBankAlreadyBooked(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(14))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{bookedTxnPayee("", daysAgo(12), "20.00", "EDEKA AKTIV MARKT")}})
		h.syncer.run()
		h.eb.setPages([][]map[string]any{{pendingTxnPayee("", daysAgo(8), "21.43", "Visa Edeka Aktiv Markt")}})
		h.syncer.run()
		if n, _ := h.st.CountMatchReviews(); n != 0 {
			t.Fatalf("%d reviews; an authorisation was held against a purchase the bank had already booked", n)
		}

		h.eb.setPages([][]map[string]any{{bookedTxnPayee("", daysAgo(1), "21.43", "EDEKA AKTIV MARKT Gelnhausen")}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 2 {
			t.Fatalf("%d rows, want the earlier purchase and the settled authorisation", len(txns))
		}
		for _, tx := range txns {
			if tx.AmountCents == -2000 && tx.Date.Format("2006-01-02") != daysAgo(12) {
				t.Errorf("the earlier booked purchase was rewritten: %+v", tx)
			}
			if tx.AmountCents == -2143 && !tx.Cleared {
				t.Errorf("the authorisation was not settled by its booking: %+v", tx)
			}
		}
		if n, _ := h.st.CountMatchReviews(); n != 0 {
			t.Fatalf("%d reviews after the booking arrived, want none", n)
		}
	})
}

func TestReviewQueue_aHeldAuthorisationDoesNotOfferARowAlreadyBooked(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(14))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{pendingTxnPayee("", daysAgo(12), "20.00", "EDEKA AKTIV MARKT")}})
		h.syncer.run()
		h.eb.setPages([][]map[string]any{{bookedTxnPayee("", daysAgo(9), "30.00", "Rewe")}})
		h.syncer.run()
		h.eb.setPages([][]map[string]any{{pendingTxnPayee("", daysAgo(8), "21.43", "Visa Edeka Aktiv Markt")}})
		h.syncer.run()

		items, err := h.syncer.HeldTransactions(context.Background())
		if err != nil || len(items) != 1 {
			t.Fatalf("setup: %d held, %v; want the Visa authorisation held against the EDEKA one", len(items), err)
		}
		for _, c := range items[0].Candidates {
			if c.PayeeName == "Rewe" {
				t.Fatalf("the held authorisation is offered the booked Rewe purchase %s", c.ID)
			}
		}
	})
}
