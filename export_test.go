package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"bankingsync/store"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	real := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = real
	return <-done
}

func seedForExport(t *testing.T) *store.Store {
	t.Helper()
	h := newHarness(t)
	id := h.addAccount(t, "")
	h.reloadState(t)

	day := time.Now().UTC().Format("2006-01-02")
	yes, no := true, false
	for i, d := range []store.MatchDecision{
		{PendingKey: "k1", PayeeLevel: "exact", AmountLevel: "exact", DateLevel: "same",
			Outcome: "adopted", Weight: 9, Probability: 0.99, Truth: &yes},
		{PendingKey: "k2", PayeeLevel: "conflict", AmountLevel: "higher_within",
			DateLevel: "after_near", Outcome: "created", Weight: -2, Probability: 0.2, Truth: &no},
		{PendingKey: "k3", PayeeLevel: "truncated", AmountLevel: "exact", DateLevel: "after_far",
			Outcome: "held", Weight: 3, Probability: 0.9},
	} {
		d.RunID, d.BankAccountID, d.Bank, d.ParamVersion, d.TxnDate = "r", id, "TestBank", "v1", day
		if err := h.st.AddMatchDecision(d); err != nil {
			t.Fatalf("decision %d: %v", i, err)
		}
		if d.Truth != nil {
			if err := h.st.SetMatchDecisionTruth(id, d.PendingKey, *d.Truth); err != nil {
				t.Fatalf("truth %d: %v", i, err)
			}
		}
	}
	return h.st
}

// TestExport_saysHowTheBankBehavesWithoutSayingWhatWasBought is the whole
// argument for this file existing.
//
// It is the only route by which an institution that truncates its payee field
// can improve the shipped parameters, because the developer's own bank does not
// truncate and measuring against it would dress one bank's habits up as a
// finding. That route only stays open if the file is one a person is willing to
// attach to a public issue.
func TestExport_saysHowTheBankBehavesWithoutSayingWhatWasBought(t *testing.T) {
	st := seedForExport(t)

	out := captureStdout(t, func() {
		if err := exportLinkageStats(st, "actual", "abc123"); err != nil {
			t.Fatalf("export: %v", err)
		}
	})

	var got LinkageStats
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if len(got.Accounts) != 1 {
		t.Fatalf("got %d accounts, want 1", len(got.Accounts))
	}
	a := got.Accounts[0]
	if a.Decisions != 3 || a.Labelled != 2 {
		t.Errorf("decisions=%d settled=%d, want 3 and 2", a.Decisions, a.Labelled)
	}
	if c := a.Levels["payee.exact"]; c.Seen != 1 || c.Matched != 1 {
		t.Errorf("payee.exact = %+v, want one seen and one matched", c)
	}
	if c := a.Levels["payee.conflict"]; c.Refuted != 1 {
		t.Errorf("payee.conflict = %+v, want one refuted", c)
	}
	if c := a.Levels["payee.truncated"]; c.Seen != 1 || c.Matched != 0 || c.Refuted != 0 {
		t.Errorf("payee.truncated = %+v: an unanswered decision is neither", c)
	}

	// The counts are the m and u the parameters would be estimated from, so an
	// export with none of them is a file with nothing in it.
	if len(a.Levels) == 0 {
		t.Error("no level counts at all")
	}
}

// TestExport_carriesNothingIdentifying is the property that decides whether
// anybody will ever send one. A file that names the bank, the account or the
// installation is one people are right to refuse.
func TestExport_carriesNothingIdentifying(t *testing.T) {
	st := seedForExport(t)
	out := captureStdout(t, func() {
		if err := exportLinkageStats(st, "actual", "abc123"); err != nil {
			t.Fatalf("export: %v", err)
		}
	})

	for _, forbidden := range []string{"TestBank", "Checking", "k1", "k2", "k3", "9.99", "999"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("the export contains %q; it is meant to describe how a bank behaves, "+
				"not what anybody bought", forbidden)
		}
	}
	// Dates no finer than a month, or a small account's traffic becomes a diary.
	if got := time.Now().UTC().Format("2006-01-02"); strings.Contains(out, got) {
		t.Errorf("the export contains a full date (%s); a month is as fine as this goes", got)
	}
	if want := time.Now().UTC().Format("2006-01"); !strings.Contains(out, want) {
		t.Errorf("the export names no month at all")
	}
}
