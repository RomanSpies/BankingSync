package main

import (
	"strings"
	"testing"

	"bankingsync/enablebanking"
)

const ingTransferLine = "mandatereference:,creditorid:,remittanceinformation:Umbuchung Extra-Konto"

func ingTransfer(tx map[string]any) map[string]any {
	delete(tx, "creditor")
	tx["remittance_information"] = []any{ingTransferLine}
	return tx
}

func TestSync_anINGAuthorisationStillFindsItsBookingAcrossTheUpgrade(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		acct := h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(8))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{ingTransfer(pendingTxnPayee("", daysAgo(3), "500.00", ""))}})
		h.syncer.run()

		pending := h.syncer.state.Pending(acct)
		if len(pending) != 1 {
			t.Fatalf("setup: %d pending entries, want 1", len(pending))
		}
		written, _ := importKeys([]enablebanking.Transaction{
			txnFor("PDNG", "", daysAgo(3), ingTransferLine, -50000),
		}, h.syncer.matchPolicy("").PayeePrefixes)
		for key, val := range pending {
			txnID, _ := splitPendingVal(val)
			if err := h.syncer.state.DeletePending(acct, key, h.st); err != nil {
				t.Fatalf("setup: %v", err)
			}
			if err := h.syncer.state.SetPending(acct, written[0], txnID, daysAgo(3), h.st); err != nil {
				t.Fatalf("setup: %v", err)
			}
		}

		h.eb.setPages([][]map[string]any{{ingTransfer(bookedTxnPayee("", daysAgo(3), "500.00", ""))}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 1 || !txns[0].Cleared {
			t.Fatalf("rows %+v, want the authorisation booked", txns)
		}
		if len(h.syncer.state.Pending(acct)) != 0 {
			t.Error("the pending entry written by the previous version was not consumed by its booking")
		}
		decisions, err := h.st.GetMatchDecisions(50)
		if err != nil {
			t.Fatalf("GetMatchDecisions: %v", err)
		}
		var byKey bool
		for _, d := range decisions {
			byKey = byKey || d.Outcome == "confirmed_by_fallback_key"
		}
		if !byKey {
			t.Error("the booking did not find the authorisation by the key the previous version wrote")
		}
		if row := txns[0]; strings.Contains(row.Notes, "remittanceinformation") || strings.Contains(row.PayeeName, "remittanceinformation") {
			t.Errorf("payee %q, notes %q still carry ING's labels", row.PayeeName, row.Notes)
		}
	})
}
