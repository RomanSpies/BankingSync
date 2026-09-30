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

func TestSync_aBookingWhoseTransactionIDChangedIsCounted(t *testing.T) {
	h := newHarness(t)
	reader := withMetrics(t, h)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(8))
	h.reloadState(t)
	first := bookedTxnPayee("", daysAgo(2), "4.00", "Kiosk")
	first["transaction_id"] = "tid-1"
	again := bookedTxnPayee("", daysAgo(2), "4.00", "Kiosk")
	again["transaction_id"] = "tid-2"

	h.eb.setPages([][]map[string]any{{first}})
	h.syncer.run()
	h.eb.setPages([][]map[string]any{{again}})
	h.syncer.run()

	if got := collectBy(t, reader, "bankingsync_reference_changed_total", "bank"); len(got) != 1 {
		t.Fatalf("reference_changed_total = %v; the same record under a new transaction_id must be counted", got)
	}
}
