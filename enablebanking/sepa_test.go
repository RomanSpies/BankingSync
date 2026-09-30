package enablebanking

import (
	"strings"
	"testing"
)

func TestStripSEPAPrefixes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text untouched", "Rewe Sagt Danke", "Rewe Sagt Danke"},
		{"empty", "", ""},
		{"svwz only", "SVWZ+Miete Januar", "Miete Januar"},
		{"svwz after metadata", "EREF+ABC123 MREF+M-99 SVWZ+Miete Januar 2026", "Miete Januar 2026"},
		{"metadata only", "EREF+ABC123 MREF+M-99 CRED+DE12ZZZ00000000000", ""},
		{"abwa kept when no svwz", "EREF+X ABWA+Max Mustermann", "Max Mustermann"},
		{"leading text before tags", "Kartenzahlung EREF+X SVWZ+Supermarkt", "Supermarkt"},
		{"leading text kept when no svwz", "Kartenzahlung EREF+X", "Kartenzahlung"},
		{"plus sign in normal text", "Bonus + Zulage", "Bonus + Zulage"},
		{"lowercase tag not stripped", "svwz+kleingeschrieben", "svwz+kleingeschrieben"},
		{"svwz with trailing metadata", "SVWZ+Rechnung 42 ABWA+Firma GmbH", "Rechnung 42"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripSEPAPrefixes(c.in); got != c.want {
				t.Errorf("stripSEPAPrefixes(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestParsePayee_stripsSEPAPrefixFromFallback(t *testing.T) {
	c := newTestClient()
	raw := map[string]any{
		"credit_debit_indicator": "DBIT",
		"remittance_information": []any{"EREF+ABC MREF+M1 SVWZ+Miete Januar"},
	}
	if got := c.parsePayee(raw); got != "Miete Januar" {
		t.Errorf("payee: got %q, want Miete Januar — clearing metadata must never become a payee", got)
	}
}

func TestParseNotes_stripsSEPAPrefixes(t *testing.T) {
	raw := map[string]any{
		"remittance_information": []any{"EREF+ABC SVWZ+Stromabschlag", "MREF+M2 SVWZ+Kundennr 55123"},
	}
	if got := parseNotes(raw); got != "Stromabschlag Kundennr 55123" {
		t.Errorf("notes: got %q", got)
	}
}

func TestParseNotes_stripsUnstructuredPrefix(t *testing.T) {
	raw := map[string]any{
		"remittance_information_unstructured": "EREF+9981 SVWZ+Abonnement",
	}
	if got := parseNotes(raw); got != "Abonnement" {
		t.Errorf("notes: got %q, want Abonnement", got)
	}
}

func TestParseINGRemittance_takesTheTextAfterItsLabels(t *testing.T) {
	text, refs, ok := parseINGRemittance("mandatereference:,creditorid:,remittanceinformation:VISA REWE MARKT 1234")
	if !ok {
		t.Fatal("ING's remittance format was not recognised")
	}
	if text != "VISA REWE MARKT 1234" {
		t.Errorf("text: got %q, want the remittance information alone", text)
	}
	if !refs.isZero() {
		t.Errorf("empty labels produced references: %+v", refs)
	}
}

func TestParseINGRemittance_carriesMandateAndCreditorID(t *testing.T) {
	text, refs, ok := parseINGRemittance("mandatereference:M-0815,creditorid:DE98ZZZ09999999999,remittanceinformation:Beitrag Oktober")
	if !ok {
		t.Fatal("ING's remittance format was not recognised")
	}
	if text != "Beitrag Oktober" {
		t.Errorf("text: got %q", text)
	}
	if refs.Mandate != "M-0815" || refs.CreditorID != "DE98ZZZ09999999999" || refs.EndToEnd != "" {
		t.Errorf("refs: got %+v", refs)
	}
}

func TestParseINGRemittance_keepsCommasInTheText(t *testing.T) {
	text, _, _ := parseINGRemittance("mandatereference:,creditorid:,remittanceinformation:PAYPAL (EUROPE) S.A.R.L. ET CIE, S.C.A., 1043872")
	if text != "PAYPAL (EUROPE) S.A.R.L. ET CIE, S.C.A., 1043872" {
		t.Errorf("text: got %q, want everything after the last label", text)
	}
}

func TestParseINGRemittance_isCaseInsensitive(t *testing.T) {
	text, refs, ok := parseINGRemittance("MandateReference:M1,CreditorId:C1,RemittanceInformation:Miete")
	if !ok || text != "Miete" || refs.Mandate != "M1" || refs.CreditorID != "C1" {
		t.Errorf("got %q %+v %v", text, refs, ok)
	}
}

func TestParseINGRemittance_leavesOtherTextAlone(t *testing.T) {
	for _, in := range []string{
		"",
		"Rewe Sagt Danke",
		"EREF+ABC SVWZ+Miete",
		"Hinweis remittanceinformation:Text",
		"Kauf mandatereference:M1,creditorid:C1,remittanceinformation:Text",
		"mandatereference:M1,remittanceinformation:Text",
	} {
		text, refs, ok := parseINGRemittance(in)
		if ok || text != in || !refs.isZero() {
			t.Errorf("parseINGRemittance(%q) = %q %+v %v, want the input untouched", in, text, refs, ok)
		}
	}
}

func TestParseSEPA_readsTagsInsideINGText(t *testing.T) {
	purpose, refs := parseSEPA("mandatereference:M1,creditorid:C1,remittanceinformation:EREF+E1 MREF+M2 SVWZ+Miete")
	if purpose != "Miete" {
		t.Errorf("purpose: got %q", purpose)
	}
	if refs.EndToEnd != "E1" || refs.Mandate != "M1" || refs.CreditorID != "C1" {
		t.Errorf("refs: got %+v, want ING's own labels to win and the tag to fill the gap", refs)
	}
}

func TestParseNotesAndSEPA_readsINGFromEitherRemittanceField(t *testing.T) {
	const ing = "mandatereference:M1,creditorid:C1,remittanceinformation:Beitrag Oktober"
	for name, raw := range map[string]map[string]any{
		"unstructured": {"remittance_information_unstructured": ing},
		"array":        {"remittance_information": []any{ing, "Mitglied 42"}},
	} {
		t.Run(name, func(t *testing.T) {
			notes, refs := parseNotesAndSEPA(raw)
			if strings.Contains(notes, "remittanceinformation") || !strings.HasPrefix(notes, "Beitrag Oktober") {
				t.Errorf("notes: got %q", notes)
			}
			if refs.Mandate != "M1" || refs.CreditorID != "C1" {
				t.Errorf("refs: got %+v", refs)
			}
		})
	}
}

func TestParsePayee_fallbackDropsTheINGLabels(t *testing.T) {
	c := newTestClient()
	raw := map[string]any{
		"credit_debit_indicator": "CRDT",
		"remittance_information": []any{"mandatereference:,creditorid:,remittanceinformation:Umbuchung Tagesgeld"},
	}
	if got := c.parsePayee(raw); got != "Umbuchung Tagesgeld" {
		t.Errorf("payee: got %q, want Umbuchung Tagesgeld — clearing metadata must never become a payee", got)
	}
}

func TestParseTransaction_keyPayeeIsTheUncleanedPayee(t *testing.T) {
	const line = "mandatereference:,creditorid:,remittanceinformation:Umbuchung Tagesgeld"
	got, err := newTestClient().parseTransaction(map[string]any{
		"transaction_date":       "2026-09-10",
		"transaction_amount":     map[string]any{"amount": "500.00", "currency": "EUR"},
		"credit_debit_indicator": "CRDT",
		"remittance_information": []any{line},
	})
	if err != nil {
		t.Fatalf("parseTransaction: %v", err)
	}
	if got.Payee != "Umbuchung Tagesgeld" {
		t.Errorf("Payee: got %q", got.Payee)
	}
	if got.KeyPayee != line {
		t.Errorf("KeyPayee: got %q, want %q — the import key of a pending row written before "+
			"ING's format was read must still match its booking", got.KeyPayee, line)
	}
}

func TestParseTransaction_keyPayeeIsTheCounterpartyName(t *testing.T) {
	got, err := newTestClient().parseTransaction(map[string]any{
		"transaction_date":       "2026-09-10",
		"transaction_amount":     map[string]any{"amount": "12.00", "currency": "EUR"},
		"credit_debit_indicator": "DBIT",
		"creditor":               map[string]any{"name": "Kiosk Mueller"},
		"remittance_information": []any{"mandatereference:,creditorid:,remittanceinformation:Zeitung"},
	})
	if err != nil {
		t.Fatalf("parseTransaction: %v", err)
	}
	if got.Payee != "Kiosk Mueller" || got.KeyPayee != "Kiosk Mueller" {
		t.Errorf("Payee %q, KeyPayee %q, want the counterparty name for both", got.Payee, got.KeyPayee)
	}
}
