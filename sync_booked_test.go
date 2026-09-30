package main

import (
	"context"
	"testing"
)

func TestSync_aConfirmedAuthorisationIsRecordedAsConsumed(t *testing.T) {
	for name, feed := range map[string]struct {
		pending, booked map[string]any
	}{
		"settled by the bank's reference": {
			pendingTxnPayee("r-1", daysAgo(3), "9.99", "Spotify"),
			bookedTxnPayee("r-1", daysAgo(2), "9.99", "Spotify"),
		},
		"settled by the model": {
			pendingTxnPayee("", daysAgo(5), "120.00", "Hotel Berlin"),
			bookedTxnPayee("", daysAgo(3), "138.50", "VISA Hotel Berlin"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, h *harness) {
				acct := h.addAccount(t, "")
				_ = h.st.SetLastSyncDate(daysAgo(8))
				h.reloadState(t)

				h.eb.setPages([][]map[string]any{{feed.pending}})
				h.syncer.run()
				pendingKeys := make([]string, 0, 1)
				for k := range h.syncer.state.Pending(acct) {
					pendingKeys = append(pendingKeys, k)
				}
				if len(pendingKeys) != 1 {
					t.Fatalf("setup: %d pending entries, want 1", len(pendingKeys))
				}

				h.eb.setPages([][]map[string]any{{feed.booked}})
				h.syncer.run()

				txns := h.actualTxns(t)
				if len(txns) != 1 {
					t.Fatalf("%d rows in the budget, want the authorisation settled into one", len(txns))
				}
				if len(h.syncer.state.Pending(acct)) != 0 {
					t.Errorf("the settled authorisation is still in the pending map")
				}
				if !h.syncer.state.Booked(acct)(txns[0].ID) {
					t.Errorf("the booked row %s was not recorded as booked", txns[0].ID)
				}
				if !h.syncer.state.Consumed(acct, pendingKeys[0]) {
					t.Errorf("the authorisation's key %q was not recorded as consumed by its booking", pendingKeys[0])
				}

				h.reloadState(t)
				if !h.syncer.state.Consumed(acct, pendingKeys[0]) {
					t.Errorf("the consumed key did not survive a restart")
				}
			})
		})
	}
}

func TestSync_aPendingCarryingAnImportedReferenceLeavesTheBookedRowAlone(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(8))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{bookedTxnPayee("r-1", daysAgo(3), "138.50", "VISA Hotel Berlin")}})
		h.syncer.run()
		h.eb.setPages([][]map[string]any{{pendingTxnPayee("r-1", daysAgo(4), "120.00", "Hotel Berlin")}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 1 {
			t.Fatalf("%d rows, want the booking alone", len(txns))
		}
		if txns[0].AmountCents != -13850 {
			t.Fatalf("the booked row now reads %d cents; an authorisation delivered after its booking rewrote it", txns[0].AmountCents)
		}
	})
}

func TestSync_aReferencelessAuthorisationSeenAfterItsBookingIsNotImportedAgain(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(8))
		h.reloadState(t)

		pending := pendingTxnPayee("", daysAgo(5), "120.00", "Hotel Berlin")
		booked := bookedTxnPayee("", daysAgo(3), "138.50", "VISA Hotel Berlin")
		h.eb.setPages([][]map[string]any{{pending}})
		h.syncer.run()
		h.eb.setPages([][]map[string]any{{booked}})
		h.syncer.run()
		h.eb.setPages([][]map[string]any{{pending, booked}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 1 {
			t.Fatalf("%d rows, want one: the authorisation was already settled by its booking", len(txns))
		}
		if txns[0].AmountCents != -13850 {
			t.Fatalf("the booked row now reads %d cents; the stale authorisation rewrote it", txns[0].AmountCents)
		}
	})
}

func TestReviewQueue_assigningABookingToAPendingRowReleasesItsPendingEntry(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		held := holdOne(t, h)
		acct := held.BankAccountID
		if len(h.syncer.state.Pending(acct)) != 1 {
			t.Fatalf("setup: %d pending entries, want the Spotify authorisation", len(h.syncer.state.Pending(acct)))
		}

		items, err := h.syncer.HeldTransactions(context.Background())
		if err != nil || len(items) != 1 || len(items[0].Candidates) == 0 {
			t.Fatalf("setup: review page %v, %v", items, err)
		}
		c := items[0].Candidates[0]
		if err := h.syncer.ResolveHeld(context.Background(), items[0].ID, c.ID, c.Percent,
			h.syncer.matchPolicy("").Version()); err != nil {
			t.Fatalf("ResolveHeld: %v", err)
		}

		if n := len(h.syncer.state.Pending(acct)); n != 0 {
			t.Errorf("%d pending entries left; the booking a person assigned settled the authorisation", n)
		}
		if !h.syncer.state.Booked(acct)(c.ID) {
			t.Errorf("the row %s a person settled was not recorded as booked", c.ID)
		}
		if !h.syncer.state.Consumed(acct, "auth-1") {
			t.Errorf("the authorisation auth-1 was not recorded as consumed by the booking")
		}
	})
}

func TestReviewQueue_aHeldBookingImportedAsNewIsRecordedAsBooked(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		held := holdOne(t, h)
		items, err := h.syncer.HeldTransactions(context.Background())
		if err != nil || len(items) != 1 {
			t.Fatalf("setup: review page %v, %v", items, err)
		}
		if err := h.syncer.ResolveHeld(context.Background(), items[0].ID, "", 0,
			h.syncer.matchPolicy("").Version()); err != nil {
			t.Fatalf("ResolveHeld: %v", err)
		}

		var created string
		for _, tx := range h.actualTxns(t) {
			if tx.PayeeName == "Netflix" {
				created = tx.ID
			}
		}
		if created == "" {
			t.Fatal("the booking a person called new was not imported")
		}
		if !h.syncer.state.Booked(held.BankAccountID)(created) {
			t.Errorf("the booking imported from the review queue was not recorded as booked")
		}
	})
}

func TestSync_referenceSourcesAreCountedByStatus(t *testing.T) {
	h := newHarness(t)
	reader := withMetrics(t, h)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(8))
	h.reloadState(t)

	byID := bookedTxnPayee("", daysAgo(2), "4.00", "Kiosk")
	byID["transaction_id"] = "tid-1"
	h.eb.setPages([][]map[string]any{{
		bookedTxnPayee("e-1", daysAgo(3), "9.99", "Spotify"),
		pendingTxnPayee("e-2", daysAgo(2), "5.00", "Bakery"),
		byID,
		bookedTxnPayee("", daysAgo(1), "3.50", "Cafe Sonne"),
	}})
	h.syncer.run()

	got := collectBy(t, reader, "bankingsync_reference_source_total", "status", "source")
	want := map[string]float64{
		"BOOK/entry_reference": 1,
		"PDNG/entry_reference": 1,
		"BOOK/transaction_id":  1,
		"BOOK/none":            1,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("reference_source_total{%s} = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
}
