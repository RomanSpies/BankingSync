package enablebanking

import "testing"

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
