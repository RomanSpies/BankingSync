package enablebanking

import (
	"encoding/json"
	"strings"
	"testing"
)

// The existing web tests build SessionAccount values in Go and marshal them, so
// they only ever prove the struct round-trips through its own tags. These decode
// the container shapes Enable Banking actually uses.
func TestSessionAccount_decodesIBANFromEveryKnownShape(t *testing.T) {
	const want = "DE31123456789005193987"

	cases := []struct {
		name string
		body string
	}{
		{"nested account_id", `{"uid":"u1","account_id":{"iban":"DE31123456789005193987","other":null}}`},
		{"flat iban", `{"uid":"u1","iban":"DE31123456789005193987"}`},
		{"all_account_ids identification pair", `{"uid":"u1","all_account_ids":[
			{"identification":"BBAN","identifier":"0519398700"},
			{"identification":"IBAN","identifier":"DE31123456789005193987"}]}`},
		{"all_account_ids iban key", `{"uid":"u1","all_account_ids":[{"iban":"DE31123456789005193987"}]}`},
		{"account_identification wrapper", `{"uid":"u1","account_identification":{"iban":"DE31123456789005193987"}}`},
		{"spaced and lowercase", `{"uid":"u1","account_id":{"iban":"de31 1234 5678 9005 1939 87"}}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var a SessionAccount
			if err := json.Unmarshal([]byte(tc.body), &a); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if a.IBAN != want {
				t.Errorf("IBAN: got %q, want %q", a.IBAN, want)
			}
		})
	}
}

func TestSessionAccount_survivesUnexpectedFieldTypes(t *testing.T) {
	body := `{"uid":"u1","name":{"unexpected":"object"},"product":123,
		"currency":"EUR","account_id":{"iban":"DE31123456789005193987"}}`

	var a SessionAccount
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatalf("a single odd field type must not fail the whole entry: %v", err)
	}
	if a.IBAN == "" {
		t.Error("IBAN was dropped because another field had an unexpected type")
	}
	if a.Currency != "EUR" {
		t.Errorf("Currency: got %q, want EUR", a.Currency)
	}
}

func TestSessionAccount_labelNeverFallsBackToUIDWhenAnythingElseIsKnown(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"iban wins", `{"uid":"6f2a-uuid","name":"Girokonto","account_id":{"iban":"DE31123456789005193987"}}`, "DE31**************3987"},
		{"name when no iban", `{"uid":"6f2a-uuid","name":"Girokonto"}`, "Girokonto"},
		{"product when no name", `{"uid":"6f2a-uuid","product":"Current Account"}`, "Current Account"},
		{"cash type when no product", `{"uid":"6f2a-uuid","cash_account_type":"CACC"}`, "CACC"},
		{"uid only as last resort", `{"uid":"6f2a-uuid"}`, "6f2a-uuid"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var a SessionAccount
			if err := json.Unmarshal([]byte(tc.body), &a); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := a.Label(); got != tc.want {
				t.Errorf("Label: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSessionAccount_twoAccountsOfOneBankAreDistinguishable(t *testing.T) {
	body := `[{"uid":"aaaa-1111","account_id":{"iban":"DE31123456789005193987"},"name":"Girokonto","currency":"EUR"},
		{"uid":"bbbb-2222","account_id":{"iban":"DE56123456789000111222"},"name":"Tagesgeld","currency":"EUR"}]`

	var accounts []SessionAccount
	if err := json.Unmarshal([]byte(body), &accounts); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("accounts: got %d, want 2", len(accounts))
	}
	if accounts[0].Label() == accounts[1].Label() {
		t.Fatalf("both accounts render as %q — the picker cannot tell them apart", accounts[0].Label())
	}
	for _, a := range accounts {
		if strings.Contains(a.Label(), a.EffectiveUID()) {
			t.Errorf("account %s falls back to its UUID despite carrying an IBAN", a.EffectiveUID())
		}
	}
}

// The picker re-reads what was written to pending_auth_accounts, so the struct
// has to survive its own round-trip as well as the wire format.
func TestSessionAccount_survivesTheSettingsRoundTrip(t *testing.T) {
	var decoded []SessionAccount
	wire := `[{"uid":"aaaa-1111","account_id":{"iban":"DE31 1234 5678 9005 1939 87"},
		"name":"Girokonto","currency":"EUR","cash_account_type":"CACC"}]`
	if err := json.Unmarshal([]byte(wire), &decoded); err != nil {
		t.Fatalf("decode wire: %v", err)
	}

	stored, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var reread []SessionAccount
	if err := json.Unmarshal(stored, &reread); err != nil {
		t.Fatalf("decode stored: %v", err)
	}
	if reread[0].IBAN != "DE31123456789005193987" {
		t.Errorf("IBAN lost in the settings round-trip: got %q", reread[0].IBAN)
	}
	if reread[0].Label() != decoded[0].Label() {
		t.Errorf("label changed across the round-trip: %q -> %q", decoded[0].Label(), reread[0].Label())
	}
}

func TestSuggestedAccountName_distinguishesTwoAccountsOfOneBank(t *testing.T) {
	var accounts []SessionAccount
	wire := `[{"uid":"a","account_id":{"iban":"DE31123456789005193987"},"name":"Girokonto"},
		{"uid":"b","account_id":{"iban":"DE56123456789000111222"},"name":"Tagesgeld"}]`
	if err := json.Unmarshal([]byte(wire), &accounts); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	first := accounts[0].SuggestedAccountName("Sparkasse")
	second := accounts[1].SuggestedAccountName("Sparkasse")
	if first == second {
		t.Fatalf("both accounts suggest %q — connecting both would merge them into "+
			"one budget account", first)
	}
	for _, got := range []string{first, second} {
		if !strings.Contains(got, "Sparkasse") {
			t.Errorf("suggestion %q does not name the bank", got)
		}
	}
}

func TestSuggestedAccountName_fallsBackToTheIBANWhenTheBankNamesNothing(t *testing.T) {
	var a SessionAccount
	if err := json.Unmarshal([]byte(`{"uid":"a","account_id":{"iban":"DE31123456789005193987"}}`), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	got := a.SuggestedAccountName("Sparkasse")
	if !strings.Contains(got, a.MaskedIBAN()) {
		t.Errorf("suggestion %q carries neither an account name nor the IBAN", got)
	}
	if strings.Contains(got, a.IBAN) {
		t.Errorf("suggestion %q contains the full IBAN; it should be masked", got)
	}
}

func TestSuggestedAccountName_neverEmptyWhenAnythingIsKnown(t *testing.T) {
	for _, tc := range []struct{ name, wire, bank string }{
		{"bank only", `{"uid":"a"}`, "Sparkasse"},
		{"account only", `{"uid":"a","name":"Girokonto"}`, ""},
		{"iban only", `{"uid":"a","account_id":{"iban":"DE31123456789005193987"}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var a SessionAccount
			if err := json.Unmarshal([]byte(tc.wire), &a); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := a.SuggestedAccountName(tc.bank); got == "" {
				t.Error("suggestion is empty although the account is identifiable")
			}
		})
	}
}
