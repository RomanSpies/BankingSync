package main

import (
	"context"
	"testing"

	"bankingsync/enablebanking"
	"bankingsync/web"
)

func TestTxnIdentities_numbersIdenticalRecords(t *testing.T) {
	got := txnIdentities([]enablebanking.Transaction{
		{Status: "BOOK", ContentKey: "2026-09-30|aaaa"},
		{Status: "BOOK", ContentKey: "2026-09-30|aaaa"},
		{Status: "BOOK", ContentKey: "2026-09-30|bbbb"},
		{Status: "PDNG"},
	})
	want := []string{"2026-09-30|aaaa|1", "2026-09-30|aaaa|2", "2026-09-30|bbbb|1", ""}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("identity %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func identityPointsTo(t *testing.T, h *harness, acct int64, rowID string) {
	t.Helper()
	check := func(when string) {
		for _, id := range h.syncer.state.Identities[acct] {
			if id == rowID {
				return
			}
		}
		t.Errorf("%s: no booking identity points at the row %s it landed in: %v", when, rowID, h.syncer.state.Identities[acct])
	}
	check("in memory")
	h.reloadState(t)
	check("after a restart")
}

func bookedRowID(t *testing.T, h *harness) string {
	t.Helper()
	for _, tx := range h.actualTxns(t) {
		if tx.Cleared {
			return tx.ID
		}
	}
	t.Fatal("no booked row in the budget")
	return ""
}

func TestSync_everyWayABookingLandsRecordsItsIdentity(t *testing.T) {
	for name, feeds := range map[string][][]map[string]any{
		"created": {
			{bookedTxnPayee("", daysAgo(2), "3.50", "Cafe Sonne")},
		},
		"confirmed through its pending entry": {
			{pendingTxnPayee("", daysAgo(3), "9.99", "Spotify")},
			{bookedTxnPayee("", daysAgo(3), "9.99", "Spotify")},
		},
		"adopted by the model": {
			{pendingTxnPayee("", daysAgo(5), "120.00", "Hotel Berlin")},
			{bookedTxnPayee("", daysAgo(3), "138.50", "VISA Hotel Berlin")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, h *harness) {
				acct := h.addAccount(t, "")
				_ = h.st.SetLastSyncDate(daysAgo(8))
				h.reloadState(t)
				for _, feed := range feeds {
					h.eb.setPages([][]map[string]any{feed})
					h.syncer.run()
				}
				identityPointsTo(t, h, acct, bookedRowID(t, h))
			})
		})
	}

	t.Run("settling a pending entry whose row is gone", func(t *testing.T) {
		forEachBackend(t, func(t *testing.T, h *harness) {
			acct := h.addAccount(t, "")
			_ = h.st.SetLastSyncDate(daysAgo(8))
			h.reloadState(t)
			h.eb.setPages([][]map[string]any{{pendingTxnPayee("", daysAgo(3), "9.99", "Spotify")}})
			h.syncer.run()
			for key := range h.syncer.state.Pending(acct) {
				if err := h.syncer.state.SetPending(acct, key, "gone", daysAgo(3), h.st); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}

			h.eb.setPages([][]map[string]any{{bookedTxnPayee("", daysAgo(3), "9.99", "Spotify")}})
			h.syncer.run()

			identityPointsTo(t, h, acct, bookedRowID(t, h))
		})
	})
}

func TestReviewQueue_aDecidedBookingRecordsItsIdentity(t *testing.T) {
	t.Run("imported as new", func(t *testing.T) {
		forEachBackend(t, func(t *testing.T, h *harness) {
			held := holdOne(t, h)
			items, _ := h.syncer.HeldTransactions(context.Background())
			if err := h.syncer.ResolveHeld(context.Background(), items[0].ID, "", 0,
				h.syncer.matchPolicy("").Version()); err != nil {
				t.Fatalf("ResolveHeld: %v", err)
			}
			for _, tx := range h.actualTxns(t) {
				if tx.PayeeName == "Netflix" {
					identityPointsTo(t, h, held.BankAccountID, tx.ID)
				}
			}
		})
	})
	t.Run("merged into a row", func(t *testing.T) {
		forEachBackend(t, func(t *testing.T, h *harness) {
			held := holdOne(t, h)
			items, _ := h.syncer.HeldTransactions(context.Background())
			c := items[0].Candidates[0]
			if err := h.syncer.ResolveHeld(context.Background(), items[0].ID, c.ID, c.Percent,
				h.syncer.matchPolicy("").Version()); err != nil {
				t.Fatalf("ResolveHeld: %v", err)
			}
			identityPointsTo(t, h, held.BankAccountID, c.ID)
		})
	})
	t.Run("settling a held authorisation", func(t *testing.T) {
		forEachBackend(t, func(t *testing.T, h *harness) {
			auth, b, _ := holdDriftedPair(t, h)
			items, _ := h.syncer.HeldTransactions(context.Background())
			var pick web.ReviewCandidate
			for _, c := range items[b].Candidates {
				if c.Held {
					pick = c
				}
			}
			if err := h.syncer.ResolveHeld(context.Background(), items[b].ID, pick.ID, pick.Percent,
				h.syncer.matchPolicy("").Version()); err != nil {
				t.Fatalf("ResolveHeld: %v", err)
			}
			for _, tx := range h.actualTxns(t) {
				if tx.AmountCents == -2490 {
					identityPointsTo(t, h, auth.BankAccountID, tx.ID)
				}
			}
		})
	})
}

func TestSync_aBookingWhoseTransactionIDChangedIsNotImportedTwice(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(8))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{onlyTransactionID(bookedTxnPayee("", daysAgo(2), "4.00", "Kiosk"), "tid-1")}})
		h.syncer.run()
		h.eb.setPages([][]map[string]any{{onlyTransactionID(bookedTxnPayee("", daysAgo(2), "4.00", "Kiosk"), "tid-2")}})
		h.syncer.run()

		if n := len(h.actualTxns(t)); n != 1 {
			t.Fatalf("%d rows; the bank changed the transaction_id of a record it had already delivered", n)
		}
	})
}

func TestSync_aRedeliveredBookingBesideItsTwinIsNotHeld(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(8))
		h.reloadState(t)
		monday := bookedTxnPayee("", daysAgo(5), "3.50", "Cafe Sonne")
		wednesday := bookedTxnPayee("", daysAgo(3), "3.50", "Cafe Sonne")

		h.eb.setPages([][]map[string]any{{monday}})
		h.syncer.run()
		h.eb.setPages([][]map[string]any{{monday, wednesday}})
		h.syncer.run()

		if n := len(h.actualTxns(t)); n != 2 {
			t.Fatalf("%d rows, want Monday's and Wednesday's purchase", n)
		}
		if n, _ := h.st.CountMatchReviews(); n != 0 {
			t.Fatalf("%d reviews; Monday's booking was delivered again and is a lookup, not a question", n)
		}
	})
}

func TestSync_aRedeliveredBookingIsSkippedWithoutConsultingTheModel(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(8))
		h.reloadState(t)
		monday := bookedTxnPayee("", daysAgo(5), "3.50", "Cafe Sonne")

		h.eb.setPages([][]map[string]any{{monday}})
		h.syncer.run()
		before, _ := h.st.CountMatchDecisions()
		h.eb.setPages([][]map[string]any{{monday}})
		h.syncer.run()
		after, _ := h.st.CountMatchDecisions()

		if n := len(h.actualTxns(t)); n != 1 {
			t.Fatalf("%d rows after the same booking was delivered twice, want 1", n)
		}
		if after != before {
			t.Fatalf("%d decisions recorded for a booking already imported; its own row is not evidence about the model", after-before)
		}
	})
}

func TestSync_twoIdenticalBookingsOnOneDayStayTwoAcrossRuns(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(8))
		h.reloadState(t)
		coffee := bookedTxnPayee("", daysAgo(2), "3.50", "Cafe Sonne")

		for _, feed := range [][]map[string]any{{coffee}, {coffee, coffee}, {coffee, coffee}} {
			h.eb.setPages([][]map[string]any{feed})
			h.syncer.run()
		}

		if n := len(h.actualTxns(t)); n != 2 {
			t.Fatalf("%d rows, want the two purchases the bank reported", n)
		}
		if n, _ := h.st.CountMatchReviews(); n != 0 {
			t.Fatalf("%d reviews, want none", n)
		}
	})
}

func TestSync_aSecondIdenticalPurchaseOnALaterDayIsNotAbsorbed(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(8))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{bookedTxnPayee("", daysAgo(5), "3.50", "Cafe Sonne")}})
		h.syncer.run()
		h.eb.setPages([][]map[string]any{{bookedTxnPayee("", daysAgo(3), "3.50", "Cafe Sonne")}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 2 {
			t.Fatalf("%d rows; Wednesday's purchase was merged into Monday's and 3.50 went missing", len(txns))
		}
		for _, tx := range txns {
			if !tx.Cleared {
				t.Errorf("row %+v is not booked", tx)
			}
		}
		if n, _ := h.st.CountMatchReviews(); n != 0 {
			t.Fatalf("%d reviews, want none", n)
		}
	})
}

func TestSync_aDriftedRedeliveryWithinReachStillAdoptsItsOwnRow(t *testing.T) {
	h := newHarness(t)
	reader := withMetrics(t, h)
	acct := h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(8))
	h.reloadState(t)
	monday := bookedTxnPayee("", daysAgo(5), "3.50", "Cafe Sonne")
	h.eb.setPages([][]map[string]any{{monday}})
	h.syncer.run()

	drifted := bookedTxnPayee("", daysAgo(5), "3.50", "Cafe Sonne")
	drifted["note"] = "enriched later by the bank"
	_ = h.st.SetBankAccountLastSyncDate(acct, daysAgo(6))
	h.reloadState(t)
	h.eb.setPages([][]map[string]any{{drifted}})
	h.syncer.run()

	if n := len(h.actualTxns(t)); n != 1 {
		t.Fatalf("%d rows; a record the bank changed while it was still in reach must settle onto its own row", n)
	}
	if got := collectBy(t, reader, "bankingsync_booking_identity_changed_total", "bank"); len(got) != 1 {
		t.Fatalf("booking_identity_changed_total = %v; the drift must be counted", got)
	}
}

func onlyTransactionID(tx map[string]any, id string) map[string]any {
	delete(tx, "entry_reference")
	tx["transaction_id"] = id
	return tx
}

func TestSync_aBookingImportedUnderItsTransactionIDIsNotImportedAgain(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(8))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{bookedTxnPayee("T1", daysAgo(3), "12.00", "Kiosk")}})
		h.syncer.run()
		h.eb.setPages([][]map[string]any{{onlyTransactionID(bookedTxnPayee("", daysAgo(3), "12.00", "Kiosk"), "T1")}})
		h.syncer.run()

		if n := len(h.actualTxns(t)); n != 1 {
			t.Fatalf("%d rows; a booking imported under its transaction_id before this version must still be recognised by it", n)
		}
	})
}

func TestSync_anAuthorisationKeyedByItsTransactionIDIsStillSettled(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(8))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{pendingTxnPayee("T9", daysAgo(4), "30.00", "Hotel Berlin")}})
		h.syncer.run()
		h.eb.setPages([][]map[string]any{{onlyTransactionID(bookedTxnPayee("", daysAgo(2), "34.50", "HBM Hospitality GmbH"), "T9")}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 1 || !txns[0].Cleared || txns[0].AmountCents != -3450 {
			t.Fatalf("rows %+v; an authorisation keyed by its transaction_id before this version must still be settled by its booking", txns)
		}
	})
}
