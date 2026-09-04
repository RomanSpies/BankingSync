package enablebanking

import "testing"

func TestParseCurrency(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want string
	}{
		{"plain", map[string]any{"transaction_amount": map[string]any{"amount": "1.00", "currency": "EUR"}}, "EUR"},
		{"lowercase is normalised", map[string]any{"transaction_amount": map[string]any{"amount": "1.00", "currency": "eur"}}, "EUR"},
		{"padded", map[string]any{"transaction_amount": map[string]any{"amount": "1.00", "currency": " CZK "}}, "CZK"},
		{"absent", map[string]any{"transaction_amount": map[string]any{"amount": "1.00"}}, ""},
		{"no amount map", map[string]any{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseCurrency(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseCounterpartyIBAN_picksSideByDirection(t *testing.T) {
	debit := map[string]any{
		"transaction_amount":     map[string]any{"amount": "10.00", "currency": "EUR"},
		"credit_debit_indicator": "DBIT",
		"creditor_account":       map[string]any{"iban": "DE63111111111111111111"},
		"debtor_account":         map[string]any{"iban": "DE90222222222222222222"},
	}
	if got := parseCounterpartyIBAN(debit); got != "DE63111111111111111111" {
		t.Errorf("debit must use the creditor side: got %q", got)
	}

	credit := map[string]any{
		"transaction_amount":     map[string]any{"amount": "10.00", "currency": "EUR"},
		"credit_debit_indicator": "CRDT",
		"creditor_account":       map[string]any{"iban": "DE63111111111111111111"},
		"debtor_account":         map[string]any{"iban": "DE90222222222222222222"},
	}
	if got := parseCounterpartyIBAN(credit); got != "DE90222222222222222222" {
		t.Errorf("credit must use the debtor side: got %q", got)
	}
}

func TestParseCounterpartyIBAN_normalises(t *testing.T) {
	in := map[string]any{
		"transaction_amount":     map[string]any{"amount": "10.00", "currency": "EUR"},
		"credit_debit_indicator": "DBIT",
		"creditor_account":       map[string]any{"iban": "de97 1234 5678 9000 0000 01"},
	}
	const want = "DE97123456789000000001"
	if got := parseCounterpartyIBAN(in); got != want {
		t.Errorf("got %q, want %q — spacing and case must not fork account matching", got, want)
	}
}

func TestParseCounterpartyIBAN_absent(t *testing.T) {
	in := map[string]any{
		"transaction_amount":     map[string]any{"amount": "10.00", "currency": "EUR"},
		"credit_debit_indicator": "DBIT",
	}
	if got := parseCounterpartyIBAN(in); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestParseSEPA_extractsRefsAndKeepsPurpose(t *testing.T) {
	const raw = "EREF+2026070100123456 MREF+M-4711 CRED+DE98ZZZ09999999999 SVWZ+Miete Januar 2026"
	purpose, refs := parseSEPA(raw)

	if purpose != "Miete Januar 2026" {
		t.Errorf("purpose: got %q, want %q", purpose, "Miete Januar 2026")
	}
	if refs.EndToEnd != "2026070100123456" {
		t.Errorf("EREF: got %q", refs.EndToEnd)
	}
	if refs.Mandate != "M-4711" {
		t.Errorf("MREF: got %q", refs.Mandate)
	}
	if refs.CreditorID != "DE98ZZZ09999999999" {
		t.Errorf("CRED: got %q", refs.CreditorID)
	}
}

func TestParseSEPA_noTagsYieldsNoRefs(t *testing.T) {
	const raw = "KARTENZAHLUNG REWE SAGT DANKE"
	purpose, refs := parseSEPA(raw)
	if purpose != raw {
		t.Errorf("purpose: got %q, want the input unchanged", purpose)
	}
	if !refs.isZero() {
		t.Errorf("expected no refs, got %+v", refs)
	}
}

func TestParseNotesAndSEPA_collectsRefsAcrossJoinedLines(t *testing.T) {
	raw := map[string]any{
		"remittance_information": []any{"EREF+ABC SVWZ+Stromabschlag", "MREF+M2 SVWZ+Kundennr 55123"},
	}
	notes, refs := parseNotesAndSEPA(raw)

	if notes != "Stromabschlag Kundennr 55123" {
		t.Errorf("notes changed: got %q", notes)
	}
	if refs.EndToEnd != "ABC" {
		t.Errorf("EREF from line 1: got %q", refs.EndToEnd)
	}
	if refs.Mandate != "M2" {
		t.Errorf("MREF from line 2 must survive the join: got %q", refs.Mandate)
	}
}

func TestParseTransaction_carriesNewFields(t *testing.T) {
	c := newTestClient()
	got, err := c.parseTransaction(map[string]any{
		"entry_reference":                     "ref-1",
		"transaction_date":                    "2026-07-10",
		"transaction_amount":                  map[string]any{"amount": "84.50", "currency": "EUR"},
		"credit_debit_indicator":              "DBIT",
		"status":                              "BOOK",
		"creditor":                            map[string]any{"name": "Vermieter"},
		"creditor_account":                    map[string]any{"iban": "DE70123456789000000002"},
		"remittance_information_unstructured": "EREF+E1 MREF+M1 CRED+C1 SVWZ+Miete",
	})
	if err != nil {
		t.Fatalf("parseTransaction: %v", err)
	}
	if got.Currency != "EUR" {
		t.Errorf("Currency: got %q", got.Currency)
	}
	if got.CounterpartyIBAN != "DE70123456789000000002" {
		t.Errorf("CounterpartyIBAN: got %q", got.CounterpartyIBAN)
	}
	if got.SEPA.EndToEnd != "E1" || got.SEPA.Mandate != "M1" || got.SEPA.CreditorID != "C1" {
		t.Errorf("SEPA: got %+v", got.SEPA)
	}
	if got.Notes != "Miete" {
		t.Errorf("Notes: got %q, want Miete", got.Notes)
	}
}
