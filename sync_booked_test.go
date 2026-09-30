package main

import (
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
