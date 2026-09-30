package main

import "testing"

func TestSync_skippedTransactionsCarryTheirReason(t *testing.T) {
	for reason, feeds := range map[string][][]map[string]any{
		"reference": {
			{bookedTxnPayee("r1", daysAgo(3), "12.00", "Kiosk")},
			{bookedTxnPayee("r1", daysAgo(3), "12.00", "Kiosk")},
		},
		"transaction_id": {
			{bookedTxnPayee("T1", daysAgo(3), "12.00", "Kiosk")},
			{onlyTransactionID(bookedTxnPayee("", daysAgo(3), "12.00", "Kiosk"), "T1")},
		},
		"content": {
			{bookedTxnPayee("", daysAgo(3), "12.00", "Kiosk")},
			{bookedTxnPayee("", daysAgo(3), "12.00", "Kiosk")},
		},
		"authorisation_seen": {
			{pendingTxnPayee("", daysAgo(3), "12.00", "Kiosk")},
			{pendingTxnPayee("", daysAgo(3), "12.00", "Kiosk")},
		},
		"authorisation_settled": {
			{pendingTxnPayee("a1", daysAgo(3), "12.00", "Kiosk")},
			{bookedTxnPayee("a1", daysAgo(3), "12.00", "Kiosk")},
			{pendingTxnPayee("a1", daysAgo(3), "12.00", "Kiosk")},
		},
		"adopted": {
			{pendingTxnPayee("", daysAgo(3), "120.00", "Hotel Berlin")},
			{pendingTxnPayee("", daysAgo(3), "120.00", "Hotel Berlin GmbH")},
		},
	} {
		t.Run(reason, func(t *testing.T) {
			h := newHarness(t)
			reader := withMetrics(t, h)
			h.addAccount(t, "")
			_ = h.st.SetLastSyncDate(daysAgo(8))
			h.reloadState(t)
			for _, feed := range feeds {
				h.eb.setPages([][]map[string]any{feed})
				h.syncer.run()
			}

			got := collectBy(t, reader, "bankingsync_transactions_skipped_total", "reason")
			if len(got) != 1 || got[reason] != 1 {
				t.Errorf("skipped by reason: got %v, want one %q", got, reason)
			}
		})
	}
}
