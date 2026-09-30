package enablebanking

import (
	"encoding/json"
	"strings"
	"testing"
)

func record(t *testing.T, text string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return m
}

func parsed(t *testing.T, raw map[string]any) Transaction {
	t.Helper()
	txn, err := newTestClient().parseTransaction(raw)
	if err != nil {
		t.Fatalf("parseTransaction: %v", err)
	}
	return txn
}

const booked = `{"status":"BOOK","transaction_date":"2026-09-28","booking_date":"2026-09-29",
	"transaction_amount":{"amount":"3.50","currency":"EUR"},"credit_debit_indicator":"DBIT",
	"creditor":{"name":"Cafe Sonne","postal_address":null},
	"remittance_information":["NR XXXX 2681 09:14"],"exchange_rate":null}`

func TestContentKey_ignoresTheTransactionID(t *testing.T) {
	a := record(t, booked)
	b := record(t, booked)
	a["transaction_id"] = "first-fetch"
	b["transaction_id"] = "second-fetch"

	if ka, kb := parsed(t, a).ContentKey, parsed(t, b).ContentKey; ka == "" || ka != kb {
		t.Fatalf("keys %q and %q; a transaction_id the bank changes between fetches must not change the record's identity", ka, kb)
	}
}

func TestContentKey_ignoresNullFields(t *testing.T) {
	a := record(t, booked)
	b := record(t, booked)
	b["new_field"] = nil
	b["creditor"].(map[string]any)["contact_details"] = nil
	delete(b, "exchange_rate")

	if ka, kb := parsed(t, a).ContentKey, parsed(t, b).ContentKey; ka != kb {
		t.Fatalf("keys %q and %q; a null field is an absent field, and a schema that grows by a null must not re-identify every record", ka, kb)
	}
}

func TestContentKey_isIndependentOfKeyOrder(t *testing.T) {
	a := record(t, `{"status":"BOOK","transaction_date":"2026-09-28","transaction_amount":{"amount":"3.50","currency":"EUR"},"creditor":{"name":"Cafe Sonne"}}`)
	b := record(t, `{"creditor":{"name":"Cafe Sonne"},"transaction_amount":{"currency":"EUR","amount":"3.50"},"transaction_date":"2026-09-28","status":"BOOK"}`)

	if ka, kb := parsed(t, a).ContentKey, parsed(t, b).ContentKey; ka != kb {
		t.Fatalf("keys %q and %q for the same record sent in a different field order", ka, kb)
	}
}

func TestContentKey_changesWithAnyNonNullField(t *testing.T) {
	base := parsed(t, record(t, booked)).ContentKey
	for name, change := range map[string]func(map[string]any){
		"remittance": func(m map[string]any) { m["remittance_information"] = []any{"NR XXXX 2681 16:02"} },
		"amount":     func(m map[string]any) { m["transaction_amount"].(map[string]any)["amount"] = "3.60" },
		"payee":      func(m map[string]any) { m["creditor"].(map[string]any)["name"] = "Cafe Mond" },
	} {
		m := record(t, booked)
		change(m)
		if got := parsed(t, m).ContentKey; got == base {
			t.Errorf("%s: a different record has the same identity %q", name, got)
		}
	}
}

func TestContentKey_isEmptyForAuthorisations(t *testing.T) {
	m := record(t, booked)
	m["status"] = "PDNG"
	if got := parsed(t, m).ContentKey; got != "" {
		t.Fatalf("an authorisation got the identity %q; it may still change before it books", got)
	}
}

func TestContentKey_isDatedByTheRecordsLatestDate(t *testing.T) {
	m := record(t, booked)
	m["value_date"] = "2026-10-01"
	txn := parsed(t, m)
	if !strings.HasPrefix(txn.ContentKey, "2026-10-01|") {
		t.Fatalf("key %q; the reach of a record is its latest date, whichever field the bank filters on", txn.ContentKey)
	}
	if got := txn.Date.Format("2006-01-02"); got != "2026-09-28" {
		t.Errorf("the transaction date became %s; the reach date must not leak into it", got)
	}
}

func TestParseTransaction_leavesTheRecordIntact(t *testing.T) {
	m := record(t, booked)
	m["transaction_id"] = "t-1"
	_ = parsed(t, m)

	if m["transaction_id"] != "t-1" {
		t.Error("parsing removed the transaction_id from the bank's record, which the failure log still prints")
	}
	if _, ok := m["exchange_rate"]; !ok {
		t.Error("parsing removed a null field from the bank's record")
	}
}
