package enablebanking

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseDecimalCents_exact(t *testing.T) {
	ok := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"0.00", 0},
		{"1", 100},
		{"1.5", 150},
		{"1.05", 105},
		{"-0.01", -1},
		{"+12.34", 1234},
		{" 42.17 ", 4217},
		{"1234567890123.45", 123456789012345},
		{"9.9900", 999},
	}
	for _, tc := range ok {
		got, err := ParseDecimalCents(tc.in)
		if err != nil {
			t.Errorf("ParseDecimalCents(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseDecimalCents(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}

	// Every one of these is a value a float-based parser would accept and get
	// subtly wrong, which is why they must be errors rather than best guesses.
	bad := []string{
		"", " ", "-", "+",
		"1,23",
		"1e3",
		"0.005",
		"12.",
		".5",
		"1.2.3",
		"abc",
		"1 2",
		"99999999999999999999.00",
	}
	for _, in := range bad {
		if got, err := ParseDecimalCents(in); err == nil {
			t.Errorf("ParseDecimalCents(%q) = %d, want an error", in, got)
		}
	}
}

func bal(typ, amount, currency string) Balance {
	c, err := ParseDecimalCents(amount)
	if err != nil {
		panic(err)
	}
	return Balance{Type: typ, AmountCents: c, Currency: currency}
}

func TestSelectBalance_prefersClosingBooked(t *testing.T) {
	got, err := SelectBalance([]Balance{
		bal("ITAV", "1500.00", "EUR"),
		bal("ITBD", "1200.00", "EUR"),
		bal("CLBD", "1000.00", "EUR"),
	}, "EUR")
	if err != nil {
		t.Fatalf("SelectBalance: %v", err)
	}
	if got.Type != "CLBD" {
		t.Errorf("type: got %s, want CLBD", got.Type)
	}
}

func TestSelectBalance_fallsBackToInterimBooked(t *testing.T) {
	got, err := SelectBalance([]Balance{
		bal("CLAV", "1500.00", "EUR"),
		bal("ITBD", "1200.00", "EUR"),
	}, "EUR")
	if err != nil {
		t.Fatalf("SelectBalance: %v", err)
	}
	if got.Type != "ITBD" {
		t.Errorf("type: got %s, want ITBD", got.Type)
	}
}

// Some banks report no booked type at all — Revolut answers with ITAV only.
// Refusing that would leave those accounts without an opening balance forever,
// so an available type is accepted as a last resort and flagged as containing
// pending entries.
func TestSelectBalance_fallsBackToAvailableWhenNothingIsBooked(t *testing.T) {
	got, err := SelectBalance([]Balance{bal("ITAV", "1500.00", "EUR")}, "EUR")
	if err != nil {
		t.Fatalf("SelectBalance: %v", err)
	}
	if got.Type != "ITAV" {
		t.Errorf("type: got %s, want ITAV", got.Type)
	}
	if !got.IncludesPending() {
		t.Error("an available balance must report that it already covers pending " +
			"entries, or they are subtracted from a figure that never held them")
	}
}

// Booked always wins, so the fallback never degrades a bank that offers both.
func TestSelectBalance_bookedBeatsAvailable(t *testing.T) {
	got, err := SelectBalance([]Balance{
		bal("ITAV", "1500.00", "EUR"),
		bal("CLBD", "1000.00", "EUR"),
	}, "EUR")
	if err != nil {
		t.Fatalf("SelectBalance: %v", err)
	}
	if got.Type != "CLBD" {
		t.Errorf("type: got %s, want CLBD", got.Type)
	}
	if got.IncludesPending() {
		t.Error("a booked balance must not claim to include pending entries")
	}
}

// The types that are wrong in kind stay refused: a period-start balance and a
// forward-looking one cannot be reconciled with the window we import.
func TestSelectBalance_refusesTypesThatAreWrongInKind(t *testing.T) {
	_, err := SelectBalance([]Balance{
		bal("XPCD", "1400.00", "EUR"),
		bal("FWAV", "1400.00", "EUR"),
		bal("OPBD", "900.00", "EUR"),
		bal("VALU", "900.00", "EUR"),
	}, "EUR")
	if !errors.Is(err, ErrNoBookedBalance) {
		t.Fatalf("error: got %v, want ErrNoBookedBalance", err)
	}
	for _, want := range []string{"XPCD", "OPBD"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the reported type %s", err, want)
		}
	}
}

func TestSelectBalance_refusesAmbiguousDuplicateType(t *testing.T) {
	_, err := SelectBalance([]Balance{
		bal("CLBD", "1000.00", "EUR"),
		bal("CLBD", "2000.00", "EUR"),
	}, "EUR")
	if !errors.Is(err, ErrAmbiguousBalance) {
		t.Fatalf("error: got %v, want ErrAmbiguousBalance", err)
	}
}

func TestSelectBalance_filtersByAccountCurrency(t *testing.T) {
	got, err := SelectBalance([]Balance{
		bal("CLBD", "1100.00", "USD"),
		bal("CLBD", "1000.00", "EUR"),
	}, "EUR")
	if err != nil {
		t.Fatalf("SelectBalance: %v", err)
	}
	if got.AmountCents != 100000 {
		t.Errorf("picked the wrong currency: got %d cents", got.AmountCents)
	}
}

func TestSelectBalance_refusesMixedCurrencyWhenAccountCurrencyUnknown(t *testing.T) {
	_, err := SelectBalance([]Balance{
		bal("CLBD", "1000.00", "EUR"),
		bal("ITBD", "1100.00", "USD"),
	}, "")
	if !errors.Is(err, ErrAmbiguousBalance) {
		t.Fatalf("error: got %v, want ErrAmbiguousBalance", err)
	}
}

func TestSelectBalance_prcdRequiresReferenceDate(t *testing.T) {
	undated := bal("PRCD", "1000.00", "EUR")
	if _, err := SelectBalance([]Balance{undated}, "EUR"); !errors.Is(err, ErrNoBookedBalance) {
		t.Fatalf("undated PRCD: got %v, want ErrNoBookedBalance", err)
	}

	dated := undated
	dated.ReferenceDate = time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	got, err := SelectBalance([]Balance{dated}, "EUR")
	if err != nil {
		t.Fatalf("dated PRCD: %v", err)
	}
	if !got.HasReferenceDate() {
		t.Error("selected PRCD lost its reference date")
	}
}

func TestParseBalance_readsTheWireShape(t *testing.T) {
	raw := map[string]any{
		"name":                       "Booked balance",
		"balance_type":               "clbd",
		"reference_date":             "2026-08-15",
		"last_committed_transaction": "ref-9",
		"balance_amount": map[string]any{
			"amount":   "-1234.56",
			"currency": "eur",
		},
	}
	b, err := parseBalance(raw)
	if err != nil {
		t.Fatalf("parseBalance: %v", err)
	}
	if b.Type != "CLBD" {
		t.Errorf("type: got %q, want CLBD (upper-cased)", b.Type)
	}
	if b.Currency != "EUR" {
		t.Errorf("currency: got %q, want EUR (upper-cased)", b.Currency)
	}
	if b.AmountCents != -123456 {
		t.Errorf("amount: got %d, want -123456", b.AmountCents)
	}
	if !b.ReferenceDate.Equal(time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("reference_date: got %v", b.ReferenceDate)
	}
	if b.LastCommitted != "ref-9" {
		t.Errorf("last_committed_transaction: got %q", b.LastCommitted)
	}
}

func TestParseBalance_rejectsIncomplete(t *testing.T) {
	cases := map[string]map[string]any{
		"no type":     {"balance_amount": map[string]any{"amount": "1.00", "currency": "EUR"}},
		"no amount":   {"balance_type": "CLBD"},
		"bad amount":  {"balance_type": "CLBD", "balance_amount": map[string]any{"amount": "1,00"}},
		"bad refdate": {"balance_type": "CLBD", "balance_amount": map[string]any{"amount": "1.00"}, "reference_date": "15.08.2026"},
	}
	for name, raw := range cases {
		if _, err := parseBalance(raw); err == nil {
			t.Errorf("%s: want an error, got nil", name)
		}
	}
}

// allDigits duplicates what strconv.ParseInt would reject anyway; it exists so
// the two failures stay distinguishable. Without it a thousands separator is
// reported as a range problem, which sends the reader looking in the wrong
// place.
func TestParseDecimalCents_distinguishesSyntaxFromRange(t *testing.T) {
	_, err := ParseDecimalCents("1,23")
	if err == nil {
		t.Fatal("want an error for a thousands separator")
	}
	if strings.Contains(err.Error(), "out of range") {
		t.Errorf("syntax error reported as a range error: %v", err)
	}

	_, err = ParseDecimalCents("99999999999999999999.00")
	if err == nil {
		t.Fatal("want an error for an overflowing amount")
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Errorf("range error not reported as one: %v", err)
	}
}

func balanceServer(t *testing.T, status int, body string) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/accounts/acct-1/balances", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	return newTestClientWith(t, mux)
}

func TestFetchBalances_decodesTheEnvelope(t *testing.T) {
	c := balanceServer(t, http.StatusOK, `{"balances":[
		{"name":"Booked","balance_type":"CLBD","reference_date":"2026-08-15",
		 "balance_amount":{"amount":"1234.56","currency":"EUR"}},
		{"name":"Available","balance_type":"ITAV",
		 "balance_amount":{"amount":"3234.56","currency":"EUR"}}]}`)

	got, err := c.FetchBalances(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("FetchBalances: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("balances: got %d, want 2", len(got))
	}

	picked, err := SelectBalance(got, "EUR")
	if err != nil {
		t.Fatalf("SelectBalance: %v", err)
	}
	if picked.AmountCents != 123456 {
		t.Errorf("picked %d cents, want 123456 — the available balance won, which "+
			"would fold the overdraft into the opening balance", picked.AmountCents)
	}
}

// A denied balances scope must not fail the sync: transactions still import,
// only the balance features go dark.
func TestFetchBalances_classifiesRefusals(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrBalancesNotPermitted},
		{http.StatusForbidden, ErrBalancesNotPermitted},
		{http.StatusNotFound, ErrBalancesUnsupported},
	} {
		c := balanceServer(t, tc.status, `{"error":"nope"}`)
		_, err := c.FetchBalances(context.Background(), "acct-1")
		if !errors.Is(err, tc.want) {
			t.Errorf("HTTP %d: got %v, want %v", tc.status, err, tc.want)
		}
	}

	c := balanceServer(t, http.StatusInternalServerError, `boom`)
	_, err := c.FetchBalances(context.Background(), "acct-1")
	if err == nil {
		t.Fatal("HTTP 500: want an error")
	}
	if errors.Is(err, ErrBalancesNotPermitted) || errors.Is(err, ErrBalancesUnsupported) {
		t.Errorf("HTTP 500 was classified as a permanent refusal: %v", err)
	}
}

// One unusable entry must not take the usable ones down with it.
func TestFetchBalances_skipsMalformedEntries(t *testing.T) {
	c := balanceServer(t, http.StatusOK, `{"balances":[
		{"balance_type":"OTHR"},
		{"balance_type":"CLBD","balance_amount":{"amount":"10.00","currency":"EUR"}}]}`)

	got, err := c.FetchBalances(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("FetchBalances: %v", err)
	}
	if len(got) != 1 || got[0].Type != "CLBD" {
		t.Fatalf("got %#v, want just the CLBD entry", got)
	}
}
