package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const samplePage = `{
  "transactions": [
    {
      "entry_reference": "REAL-REF-9981",
      "transaction_date": "2026-07-01",
      "status": "BOOK",
      "credit_debit_indicator": "DBIT",
      "transaction_amount": { "amount": "42.17", "currency": "EUR" },
      "creditor": { "name": "Real Person GmbH" },
      "creditor_account": { "iban": "DE89370400440532013000" },
      "remittance_information": ["Invoice for real thing"]
    }
  ],
  "continuation_key": ""
}`

func scrubbed(t *testing.T, in string) map[string]any {
	t.Helper()
	out, err := scrubBytes([]byte(in))
	if err != nil {
		t.Fatalf("scrubBytes: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("scrubbed output is not valid JSON: %v", err)
	}
	return decoded
}

func firstTxn(t *testing.T, page map[string]any) map[string]any {
	t.Helper()
	txns, ok := page["transactions"].([]any)
	if !ok || len(txns) == 0 {
		t.Fatal("no transactions in scrubbed output")
	}
	txn, ok := txns[0].(map[string]any)
	if !ok {
		t.Fatal("transaction is not an object")
	}
	return txn
}

func TestScrub_preservesStructuralEnums(t *testing.T) {
	txn := firstTxn(t, scrubbed(t, samplePage))
	if got := txn["status"]; got != "BOOK" {
		t.Errorf("status: got %v, want BOOK", got)
	}
	if got := txn["credit_debit_indicator"]; got != "DBIT" {
		t.Errorf("credit_debit_indicator: got %v, want DBIT", got)
	}
	if got := txn["transaction_date"]; got != "2026-07-01" {
		t.Errorf("transaction_date: got %v, want 2026-07-01", got)
	}
	amt := txn["transaction_amount"].(map[string]any)
	if got := amt["currency"]; got != "EUR" {
		t.Errorf("currency: got %v, want EUR", got)
	}
}

func TestScrub_removesIdentifyingValues(t *testing.T) {
	raw := samplePage
	txn := firstTxn(t, scrubbed(t, raw))

	for _, secret := range []string{"REAL-REF-9981", "Real Person GmbH", "DE89370400440532013000", "Invoice for real thing", "42.17"} {
		encoded, _ := json.Marshal(txn)
		if strings.Contains(string(encoded), secret) {
			t.Errorf("scrubbed output still contains %q", secret)
		}
	}
}

func TestScrub_preservesKeysAndNesting(t *testing.T) {
	txn := firstTxn(t, scrubbed(t, samplePage))
	for _, key := range []string{
		"entry_reference", "transaction_date", "status", "credit_debit_indicator",
		"transaction_amount", "creditor", "creditor_account", "remittance_information",
	} {
		if _, ok := txn[key]; !ok {
			t.Errorf("scrubbing dropped key %q", key)
		}
	}
	if _, ok := txn["creditor"].(map[string]any)["name"]; !ok {
		t.Error("scrubbing flattened creditor.name")
	}
	if _, ok := txn["remittance_information"].([]any); !ok {
		t.Error("remittance_information should stay an array")
	}
}

func TestScrub_preservesJSONTypes(t *testing.T) {
	page := scrubbed(t, `{"transactions":[{"transaction_amount":{"amount":25.5},"entry_reference":"X"}]}`)
	txn := firstTxn(t, page)
	amt := txn["transaction_amount"].(map[string]any)
	if _, ok := amt["amount"].(float64); !ok {
		t.Errorf("numeric amount became %T, want float64", amt["amount"])
	}
}

func TestScrub_isDeterministic(t *testing.T) {
	first := firstTxn(t, scrubbed(t, samplePage))
	second := firstTxn(t, scrubbed(t, samplePage))
	if first["entry_reference"] != second["entry_reference"] {
		t.Error("entry_reference scrubbing is not deterministic")
	}
	if first["creditor"].(map[string]any)["name"] != second["creditor"].(map[string]any)["name"] {
		t.Error("name scrubbing is not deterministic")
	}
}

func TestScrub_sameInputMapsToSameSyntheticValue(t *testing.T) {
	page := scrubbed(t, `{"transactions":[
      {"entry_reference":"SAME","creditor":{"name":"Shared Payee"}},
      {"entry_reference":"SAME","creditor":{"name":"Shared Payee"}},
      {"entry_reference":"OTHER","creditor":{"name":"Different Payee"}}
    ]}`)
	txns := page["transactions"].([]any)
	a := txns[0].(map[string]any)
	b := txns[1].(map[string]any)
	c := txns[2].(map[string]any)

	if a["entry_reference"] != b["entry_reference"] {
		t.Error("identical refs should scrub to the same synthetic value")
	}
	if a["entry_reference"] == c["entry_reference"] {
		t.Error("different refs should scrub to different synthetic values")
	}
	if a["creditor"].(map[string]any)["name"] != b["creditor"].(map[string]any)["name"] {
		t.Error("identical payees should scrub to the same synthetic name")
	}
}

func TestScrub_emptyStringsStayEmpty(t *testing.T) {
	txn := firstTxn(t, scrubbed(t, `{"transactions":[{"transaction_date":"","entry_reference":""}]}`))
	if got := txn["entry_reference"]; got != "" {
		t.Errorf("empty ref became %q; the empty/non-empty distinction drives parser branches", got)
	}
}

func TestScrub_rejectsInvalidJSON(t *testing.T) {
	if _, err := scrubBytes([]byte("not json")); err == nil {
		t.Fatal("expected an error for invalid JSON")
	}
}

func TestScrubFile_roundTrip(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "page-0001.json")
	out := filepath.Join(dir, "out", "ing_booked.json")
	if err := os.WriteFile(in, []byte(samplePage), 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if err := scrubFile(in, out); err != nil {
		t.Fatalf("scrubFile: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if strings.Contains(string(raw), "Real Person GmbH") {
		t.Error("scrubbed file still contains the real payee")
	}
}

func TestRun_scrubsDirectory(t *testing.T) {
	inDir := t.TempDir()
	outDir := filepath.Join(t.TempDir(), "fixtures")
	for _, name := range []string{"page-0001.json", "page-0002.json"} {
		if err := os.WriteFile(filepath.Join(inDir, name), []byte(samplePage), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(inDir, "notes.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatalf("write txt: %v", err)
	}

	if err := run(inDir, outDir); err != nil {
		t.Fatalf("run: %v", err)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read out dir: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("got %d output files, want 2 (non-JSON must be skipped)", len(entries))
	}
}

func TestScrub_replacesContinuationKey(t *testing.T) {
	page := scrubbed(t, `{"transactions":[{"entry_reference":"X"}],"continuation_key":"abc+def/ghi="}`)
	got, _ := page["continuation_key"].(string)
	if got == "abc+def/ghi=" {
		t.Error("continuation_key was preserved; it may encode account identifiers")
	}
	if got == "" {
		t.Error("continuation_key should be replaced, not dropped")
	}
}

func TestScrub_continuationKeysStayDistinct(t *testing.T) {
	one := scrubbed(t, `{"transactions":[{"entry_reference":"X"}],"continuation_key":"key-one"}`)
	two := scrubbed(t, `{"transactions":[{"entry_reference":"X"}],"continuation_key":"key-two"}`)
	if one["continuation_key"] == two["continuation_key"] {
		t.Error("distinct continuation keys collapsed to the same value; pagination fixtures need distinct keys")
	}
}
