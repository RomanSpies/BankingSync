package main

import (
	"context"
	"testing"
	"time"

	"bankingsync/store"
)

func withStatus(tx map[string]any, status string) map[string]any {
	tx["status"] = status
	return tx
}

func daysAhead(n int) string { return time.Now().UTC().AddDate(0, 0, n).Format("2006-01-02") }

func TestSync_aScheduledTransactionIsNotImportedUntilItBooks(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(8))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{withStatus(bookedTxnPayee("s1", daysAhead(3), "850.00", "Vermieter"), "SCHD")}})
		h.syncer.run()
		if n := len(h.actualTxns(t)); n != 0 {
			t.Fatalf("%d rows after a scheduled transfer, want none: the bank has not booked it", n)
		}

		h.eb.setPages([][]map[string]any{{bookedTxnPayee("s1", today(), "850.00", "Vermieter")}})
		h.syncer.run()
		txns := h.actualTxns(t)
		if len(txns) != 1 || !txns[0].Cleared {
			t.Fatalf("rows %+v, want the booking once", txns)
		}
	})
}

func TestSync_aCancelledOrRejectedTransactionIsNeverImported(t *testing.T) {
	for _, status := range []string{"CNCL", "RJCT"} {
		t.Run(status, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, h *harness) {
				h.addAccount(t, "")
				_ = h.st.SetLastSyncDate(daysAgo(8))
				h.reloadState(t)

				h.eb.setPages([][]map[string]any{{
					withStatus(bookedTxnPayee("c1", daysAgo(2), "40.00", "Kiosk"), status),
					withStatus(bookedTxnPayee("", daysAgo(2), "12.00", "Bäckerei"), status),
				}})
				h.syncer.run()
				if n := len(h.actualTxns(t)); n != 0 {
					t.Errorf("%d rows, want none", n)
				}
			})
		})
	}
}

func TestSync_aCancelledAuthorisationIsNotTurnedIntoABooking(t *testing.T) {
	for name, ref := range map[string]string{"with reference": "auth-7", "without reference": ""} {
		t.Run(name, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, h *harness) {
				reader := withMetrics(t, h)
				acct := h.addAccount(t, "")
				_ = h.st.SetLastSyncDate(daysAgo(8))
				h.reloadState(t)

				h.eb.setPages([][]map[string]any{{pendingTxnPayee(ref, daysAgo(3), "200.00", "Hotel Berlin")}})
				h.syncer.run()
				h.eb.setPages([][]map[string]any{{withStatus(bookedTxnPayee(ref, daysAgo(3), "200.00", "Hotel Berlin"), "CNCL")}})
				h.syncer.run()

				txns := h.actualTxns(t)
				if len(txns) != 1 || txns[0].Cleared {
					t.Fatalf("rows %+v, want the authorisation left uncleared", txns)
				}
				if n := len(h.syncer.state.Pending(acct)); n != 0 {
					t.Errorf("%d pending entries; a cancelled authorisation must not wait for a booking", n)
				}
				got := collectBy(t, reader, "bankingsync_authorisations_withdrawn_total", "status")
				if got["CNCL"] != 1 {
					t.Errorf("withdrawn: got %v, want one CNCL", got)
				}
			})
		})
	}
}

func TestSync_aCancelledAuthorisationBesideItsBookingIsSettledByIt(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(8))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{pendingTxnPayee("", daysAgo(3), "200.00", "Hotel Berlin")}})
		h.syncer.run()
		h.eb.setPages([][]map[string]any{{
			withStatus(bookedTxnPayee("", daysAgo(3), "200.00", "Hotel Berlin"), "CNCL"),
			bookedTxnPayee("", daysAgo(3), "200.00", "Hotel Berlin"),
		}})
		h.syncer.run()

		txns := h.actualTxns(t)
		if len(txns) != 1 || !txns[0].Cleared {
			t.Fatalf("rows %+v, want the authorisation booked", txns)
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
			t.Error("the booking did not settle the authorisation by its key; the cancellation released it first")
		}
	})
}

func TestSync_anAccountHoldIsImportedAsPendingAndSettledByItsBooking(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h *harness) {
		h.addAccount(t, "")
		_ = h.st.SetLastSyncDate(daysAgo(8))
		h.reloadState(t)

		h.eb.setPages([][]map[string]any{{withStatus(bookedTxnPayee("h1", daysAgo(4), "150.00", "Autovermietung"), "HOLD")}})
		h.syncer.run()
		txns := h.actualTxns(t)
		if len(txns) != 1 || txns[0].Cleared {
			t.Fatalf("rows %+v, want the hold as one uncleared row", txns)
		}

		h.eb.setPages([][]map[string]any{{bookedTxnPayee("h1", daysAgo(2), "150.00", "Autovermietung")}})
		h.syncer.run()
		txns = h.actualTxns(t)
		if len(txns) != 1 || !txns[0].Cleared {
			t.Fatalf("rows %+v, want the hold booked in place", txns)
		}
	})
}

func TestSync_excludedTransactionsAreCounted(t *testing.T) {
	h := newHarness(t)
	reader := withMetrics(t, h)
	h.addAccount(t, "")
	_ = h.st.SetLastSyncDate(daysAgo(8))
	h.reloadState(t)

	h.eb.setPages([][]map[string]any{{
		withStatus(bookedTxnPayee("x1", daysAhead(2), "10.00", "A"), "SCHD"),
		withStatus(bookedTxnPayee("x2", daysAgo(2), "11.00", "B"), "CNCL"),
		withStatus(bookedTxnPayee("x3", daysAgo(2), "12.00", "C"), "RJCT"),
		withStatus(bookedTxnPayee("x4", daysAgo(2), "13.00", "D"), "OTHR"),
		bookedTxnPayee("x5", daysAgo(2), "14.00", "E"),
	}})
	h.syncer.run()

	got := collectBy(t, reader, "bankingsync_transactions_excluded_total", "status")
	want := map[string]float64{"SCHD": 1, "CNCL": 1, "RJCT": 1}
	if len(got) != len(want) {
		t.Errorf("excluded: got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("excluded[%s]: got %v, want %v", k, got[k], v)
		}
	}
	if n := len(h.actualTxns(t)); n != 2 {
		t.Errorf("%d rows, want the booking and the OTHR row", n)
	}
}

func TestSync_aScheduledTransactionDoesNotMoveTheOpeningBalance(t *testing.T) {
	h := newHarness(t)
	id := autoBalanceAccount(t, h, "EUR")
	h.eb.setBalances(bookedBalance("1000.00"))
	h.eb.setPages([][]map[string]any{{
		bookedTxn("r1", daysAgo(3), "-25.00"),
		withStatus(bookedTxn("r2", daysAhead(5), "-500.00"), "SCHD"),
	}})
	h.reloadState(t)

	h.syncer.run()

	a := accountRow(t, h, id)
	if a.OpeningBalanceState != store.OpeningBalanceWritten {
		t.Fatalf("state: got %q, want %q — a scheduled payment is not a reason to wait", a.OpeningBalanceState, store.OpeningBalanceWritten)
	}
	if a.OpeningBalanceCents != 102500 {
		t.Errorf("opening: got %d, want 102500; the scheduled payment is not in the balance", a.OpeningBalanceCents)
	}
}

func TestOpeningBalancePreview_ignoresExcludedTransactions(t *testing.T) {
	h := newHarness(t)
	id := autoBalanceAccount(t, h, "EUR")
	h.eb.setBalances(bookedBalance("1000.00"))
	h.eb.setPages([][]map[string]any{{
		bookedTxn("r1", daysAgo(3), "-25.00"),
		withStatus(bookedTxn("r2", daysAhead(5), "-500.00"), "SCHD"),
		withStatus(bookedTxn("r3", daysAgo(2), "-70.00"), "RJCT"),
	}})
	h.reloadState(t)

	out, err := h.syncer.OpeningBalancePreview(context.Background(), id, false, nil)
	if err != nil {
		t.Fatalf("OpeningBalancePreview: %v", err)
	}
	if out.Refusal != "" {
		t.Fatalf("refused: %s", out.Refusal)
	}
	if out.OpeningCents != 102500 {
		t.Errorf("opening: got %d, want 102500", out.OpeningCents)
	}
}
