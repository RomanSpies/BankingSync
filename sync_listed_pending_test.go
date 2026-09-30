package main

import (
	"fmt"
	"sort"
	"testing"
)

func withBatchSize(t *testing.T, n int) {
	t.Helper()
	previous := assignBatchSize
	assignBatchSize = n
	t.Cleanup(func() { assignBatchSize = previous })
}

func TestSync_aListedPendingStaysApartFromItsKeyTwinAtAnyBatchSize(t *testing.T) {
	for _, size := range []int{1, 200} {
		t.Run(fmt.Sprintf("batch %d", size), func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, h *harness) {
				withBatchSize(t, size)
				acct := h.addAccount(t, "")
				_ = h.st.SetLastSyncDate(daysAgo(4))
				h.reloadState(t)

				h.eb.setPages([][]map[string]any{{
					pendingTxnPayee("", daysAgo(1), "3.50", "Cafe Sonne"),
					bookedTxnPayee("", daysAgo(1), "3.50", "Cafe Sonne"),
				}})
				h.syncer.run()

				if n := len(h.actualTxns(t)); n != 2 {
					t.Fatalf("%d rows; an authorisation the bank still lists is not the one its twin booked", n)
				}
				if n := len(h.syncer.state.Pending(acct)); n != 1 {
					t.Errorf("%d pending entries, want the listed authorisation kept open", n)
				}
				if n, _ := h.st.CountMatchReviews(); n != 0 {
					t.Errorf("%d reviews, want none", n)
				}
			})
		})
	}
}

func TestSync_aKnownPendingListedBesideItsKeyTwinIsNotConfirmedByIt(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		acct := h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(4))
		h.reloadState(t)
		pending := pendingTxnPayee("", daysAgo(1), "3.50", "Cafe Sonne")

		h.eb.setPages([][]map[string]any{{pending}})
		h.syncer.run()
		h.eb.setPages([][]map[string]any{{pending, bookedTxnPayee("", daysAgo(1), "3.50", "Cafe Sonne")}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 2 {
			t.Fatalf("%d rows; the bank still lists the authorisation, so the booking is a second purchase", len(txns))
		}
		uncleared := 0
		for _, tx := range txns {
			if !tx.Cleared {
				uncleared++
			}
		}
		if uncleared != 1 {
			t.Errorf("%d uncleared rows, want the listed authorisation left open", uncleared)
		}
		if n := len(h.syncer.state.Pending(acct)); n != 1 {
			t.Errorf("%d pending entries, want the listed authorisation's entry kept", n)
		}
	})
}

func TestSync_aListedPendingSharingTheBookingsReferenceIsStillConfirmed(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(4))
		h.reloadState(t)
		pending := pendingTxnPayee("r-1", daysAgo(2), "3.50", "Cafe Sonne")

		h.eb.setPages([][]map[string]any{{pending}})
		h.syncer.run()
		h.eb.setPages([][]map[string]any{{pending, bookedTxnPayee("r-1", daysAgo(1), "3.50", "Cafe Sonne")}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 1 || !txns[0].Cleared {
			t.Fatalf("rows %+v; a shared bank reference makes them one purchase, listed or not", txns)
		}
		labelled, err := h.st.GetLabelledMatchDecisions(10)
		if err != nil {
			t.Fatalf("GetLabelledMatchDecisions: %v", err)
		}
		confirmed := false
		for _, d := range labelled {
			confirmed = confirmed || d.Outcome == "confirmed_by_reference"
		}
		if !confirmed {
			t.Error("the pair was not confirmed by its reference; the bank's own identifier is the one label nobody has to answer for")
		}
	})
}

func TestSync_theBatchSizeDoesNotChangeTheOutcome(t *testing.T) {
	feed := []map[string]any{
		pendingTxnPayee("", daysAgo(3), "9.99", "Spotify"),
		bookedTxnPayee("", daysAgo(3), "9.99", "Spotify"),
		bookedTxnPayee("", daysAgo(3), "0.99", "App Store"),
		bookedTxnPayee("", daysAgo(3), "0.99", "App Store"),
		bookedTxnPayee("", daysAgo(2), "24.00", "Kiosk Mueller"),
	}
	shape := func(size int) []string {
		h := newHarness(t)
		previous := assignBatchSize
		assignBatchSize = size
		defer func() { assignBatchSize = previous }()
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(6))
		h.reloadState(t)
		h.eb.setPages([][]map[string]any{feed})
		h.syncer.run()
		var out []string
		for _, x := range h.actualTxns(t) {
			out = append(out, fmt.Sprintf("%s|%d|%v", x.PayeeName, x.AmountCents, x.Cleared))
		}
		sort.Strings(out)
		return out
	}

	whole := fmt.Sprint(shape(200))
	for _, size := range []int{1, 2, 3} {
		if got := fmt.Sprint(shape(size)); got != whole {
			t.Errorf("batch size %d produced a different budget:\n  %s\nthan one batch:\n  %s", size, got, whole)
		}
	}
}

func TestSync_aDeclinedFastPathIsCounted(t *testing.T) {
	h := newHarness(t)
	reader := withMetrics(t, h)
	withBatchSize(t, 1)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(4))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{
		pendingTxnPayee("", daysAgo(1), "3.50", "Cafe Sonne"),
		bookedTxnPayee("", daysAgo(1), "3.50", "Cafe Sonne"),
	}})
	h.syncer.run()

	if got := collectBy(t, reader, "bankingsync_listed_pending_twins_total", "bank"); len(got) != 1 {
		t.Fatalf("listed_pending_twins_total = %v; the declined confirmation must be visible", got)
	}
}
