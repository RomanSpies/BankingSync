package enablebanking

import (
	"testing"
	"time"
)

func newTestClient(ownNames ...string) *Client {
	names := make(map[string]struct{})
	for _, n := range ownNames {
		names[n] = struct{}{}
	}
	return NewClient(
		func() (string, error) { return "test-app-id", nil },
		func() ([]byte, error) { return nil, nil },
		names,
	)
}

func mustDate(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParseDate_transactionDate(t *testing.T) {
	raw := map[string]any{"transaction_date": "2024-06-15"}
	got, err := parseDate(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Equal(mustDate("2024-06-15")) {
		t.Errorf("got %v, want 2024-06-15", got)
	}
}

func TestParseDate_bookingDate_fallback(t *testing.T) {
	raw := map[string]any{"booking_date": "2024-03-01"}
	got, err := parseDate(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Equal(mustDate("2024-03-01")) {
		t.Errorf("got %v, want 2024-03-01", got)
	}
}

func TestParseDate_valueDate_fallback(t *testing.T) {
	raw := map[string]any{"value_date": "2024-07-20"}
	got, err := parseDate(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Equal(mustDate("2024-07-20")) {
		t.Errorf("got %v, want 2024-07-20", got)
	}
}

func TestParseDate_preferTransactionDate(t *testing.T) {

	raw := map[string]any{
		"transaction_date": "2024-06-10",
		"booking_date":     "2024-06-09",
	}
	got, err := parseDate(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Equal(mustDate("2024-06-10")) {
		t.Errorf("got %v, want 2024-06-10 (transaction_date wins)", got)
	}
}

func TestParseDate_truncatesDateTime(t *testing.T) {

	raw := map[string]any{"transaction_date": "2024-06-15T10:30:00"}
	got, err := parseDate(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Equal(mustDate("2024-06-15")) {
		t.Errorf("got %v, want 2024-06-15", got)
	}
}

func TestParseDate_noFieldReturnsError(t *testing.T) {
	_, err := parseDate(map[string]any{})
	if err == nil {
		t.Error("expected error when no date field is present")
	}
}

func TestParseDate_emptyStringSkipped(t *testing.T) {
	raw := map[string]any{
		"transaction_date": "",
		"booking_date":     "2024-01-02",
	}
	got, err := parseDate(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Equal(mustDate("2024-01-02")) {
		t.Errorf("got %v, want 2024-01-02", got)
	}
}

func TestParseAmountCents_creditString(t *testing.T) {
	raw := map[string]any{
		"transaction_amount":     map[string]any{"amount": "12.34"},
		"credit_debit_indicator": "CRDT",
	}
	got, err := parseAmountCents(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 1234 {
		t.Errorf("got %d, want 1234", got)
	}
}

func TestParseAmountCents_debitString(t *testing.T) {
	raw := map[string]any{
		"transaction_amount":     map[string]any{"amount": "50.00"},
		"credit_debit_indicator": "DBIT",
	}
	got, err := parseAmountCents(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != -5000 {
		t.Errorf("got %d, want -5000", got)
	}
}

func TestParseAmountCents_debitAlreadyNegative(t *testing.T) {

	raw := map[string]any{
		"transaction_amount":     map[string]any{"amount": "-50.00"},
		"credit_debit_indicator": "DBIT",
	}
	got, err := parseAmountCents(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != -5000 {
		t.Errorf("got %d, want -5000", got)
	}
}

func TestParseAmountCents_creditNegative(t *testing.T) {

	raw := map[string]any{
		"transaction_amount":     map[string]any{"amount": "-10.00"},
		"credit_debit_indicator": "CRDT",
	}
	got, err := parseAmountCents(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 1000 {
		t.Errorf("got %d, want 1000 (CRDT flips negative)", got)
	}
}

func TestParseAmountCents_alternativeIndicatorField(t *testing.T) {
	raw := map[string]any{
		"transaction_amount": map[string]any{"amount": "7.50"},
		"credit_debit_indic": "DBIT",
	}
	got, err := parseAmountCents(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != -750 {
		t.Errorf("got %d, want -750", got)
	}
}

func TestParseAmountCents_floatAmountField(t *testing.T) {
	raw := map[string]any{
		"transaction_amount":     map[string]any{"amount": float64(25.5)},
		"credit_debit_indicator": "CRDT",
	}
	got, err := parseAmountCents(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 2550 {
		t.Errorf("got %d, want 2550", got)
	}
}

func TestParseAmountCents_missingAmountIsAnError(t *testing.T) {
	raw := map[string]any{
		"transaction_amount":     map[string]any{},
		"credit_debit_indicator": "CRDT",
	}
	if _, err := parseAmountCents(raw); err == nil {
		t.Fatal("a missing amount must be reported as a parse failure, not silently imported as 0.00")
	}
}

func TestParseAmountCents_missingIndicatorKeepsNegativeSign(t *testing.T) {
	raw := map[string]any{
		"transaction_amount": map[string]any{"amount": "-50.00"},
	}
	got, err := parseAmountCents(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != -5000 {
		t.Errorf("got %d, want -5000 (a debit signalled by sign must not become income)", got)
	}
}

func TestParseAmountCents_missingIndicatorKeepsPositiveSign(t *testing.T) {
	raw := map[string]any{
		"transaction_amount": map[string]any{"amount": "50.00"},
	}
	got, err := parseAmountCents(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 5000 {
		t.Errorf("got %d, want 5000", got)
	}
}

func TestParseAmountCents_missingIndicatorZeroAmountIsAnError(t *testing.T) {
	raw := map[string]any{
		"transaction_amount": map[string]any{"amount": "0.00"},
	}
	if _, err := parseAmountCents(raw); err == nil {
		t.Fatal("a zero amount with no indicator has no determinable direction and must be reported")
	}
}

func TestParseAmountCents_roundingCents(t *testing.T) {

	raw := map[string]any{
		"transaction_amount":     map[string]any{"amount": "9.999"},
		"credit_debit_indicator": "CRDT",
	}
	got, err := parseAmountCents(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 1000 {
		t.Errorf("got %d, want 1000 (9.999 rounds to 1000 cents)", got)
	}
}

func TestParsePayee_debitCreditorName(t *testing.T) {
	c := newTestClient()
	raw := map[string]any{
		"credit_debit_indicator": "DBIT",
		"creditor":               map[string]any{"name": "Shop A"},
	}
	got := c.parsePayee(raw)
	if got != "Shop A" {
		t.Errorf("got %q, want Shop A", got)
	}
}

func TestParsePayee_debitCreditorNameFlatField(t *testing.T) {
	c := newTestClient()
	raw := map[string]any{
		"credit_debit_indicator": "DBIT",
		"creditor_name":          "Shop B",
	}
	got := c.parsePayee(raw)
	if got != "Shop B" {
		t.Errorf("got %q, want Shop B", got)
	}
}

func TestParsePayee_debitFallsBackToRemittance(t *testing.T) {
	c := newTestClient()
	raw := map[string]any{
		"credit_debit_indicator": "DBIT",
		"remittance_information": []any{"Card Payment to Store"},
	}
	got := c.parsePayee(raw)
	if got != "Card Payment to Store" {
		t.Errorf("got %q, want 'Card Payment to Store'", got)
	}
}

func TestParsePayee_creditDebtorName(t *testing.T) {
	c := newTestClient()
	raw := map[string]any{
		"credit_debit_indicator": "CRDT",
		"debtor":                 map[string]any{"name": "Payer Co"},
	}
	got := c.parsePayee(raw)
	if got != "Payer Co" {
		t.Errorf("got %q, want Payer Co", got)
	}
}

func TestParsePayee_creditDebtorIsOwnName_fallsBackToRemittance(t *testing.T) {
	c := newTestClient("john doe")
	raw := map[string]any{
		"credit_debit_indicator": "CRDT",
		"debtor":                 map[string]any{"name": "John Doe"},
		"remittance_information": []any{"Refund from Store"},
	}
	got := c.parsePayee(raw)
	if got != "Refund from Store" {
		t.Errorf("got %q, want 'Refund from Store'", got)
	}
}

func TestParsePayee_noNameReturnsUnknown(t *testing.T) {
	c := newTestClient()
	raw := map[string]any{"credit_debit_indicator": "DBIT"}
	got := c.parsePayee(raw)
	if got != "Unknown" {
		t.Errorf("got %q, want Unknown", got)
	}
}

func TestParsePayee_debitorNameFlatField(t *testing.T) {
	c := newTestClient()
	raw := map[string]any{
		"credit_debit_indicator": "CRDT",
		"debtor_name":            "Flat Debtor",
	}
	got := c.parsePayee(raw)
	if got != "Flat Debtor" {
		t.Errorf("got %q, want Flat Debtor", got)
	}
}

func TestParseNotes_unstructured(t *testing.T) {
	raw := map[string]any{
		"remittance_information_unstructured": "Invoice 12345",
	}
	got := parseNotes(raw)
	if got != "Invoice 12345" {
		t.Errorf("got %q, want 'Invoice 12345'", got)
	}
}

func TestParseNotes_structuredList(t *testing.T) {
	raw := map[string]any{
		"remittance_information": []any{"Line 1", "Line 2"},
	}
	got := parseNotes(raw)
	if got != "Line 1 Line 2" {
		t.Errorf("got %q, want 'Line 1 Line 2'", got)
	}
}

func TestParseNotes_structuredString(t *testing.T) {
	raw := map[string]any{
		"remittance_information": "Single string ref",
	}
	got := parseNotes(raw)
	if got != "Single string ref" {
		t.Errorf("got %q, want 'Single string ref'", got)
	}
}

func TestParseNotes_empty(t *testing.T) {
	got := parseNotes(map[string]any{})
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestParseNotes_unstructuredPreferred(t *testing.T) {

	raw := map[string]any{
		"remittance_information_unstructured": "Priority",
		"remittance_information":              []any{"Not this"},
	}
	got := parseNotes(raw)
	if got != "Priority" {
		t.Errorf("got %q, want Priority", got)
	}
}

func TestGetEntryRef_entryReference(t *testing.T) {
	raw := map[string]any{
		"entry_reference": "REF-001",
		"transaction_id":  "TXN-001",
	}
	got := getEntryRef(raw)
	if got != "REF-001" {
		t.Errorf("got %q, want REF-001 (entry_reference wins)", got)
	}
}

func TestGetEntryRef_fallbackToTransactionID(t *testing.T) {
	raw := map[string]any{"transaction_id": "TXN-002"}
	got := getEntryRef(raw)
	if got != "TXN-002" {
		t.Errorf("got %q, want TXN-002", got)
	}
}

func TestGetEntryRef_empty(t *testing.T) {
	got := getEntryRef(map[string]any{})
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestIndicString(t *testing.T) {
	cases := []struct {
		raw  map[string]any
		want string
	}{
		{map[string]any{"credit_debit_indicator": "dbit"}, "DBIT"},
		{map[string]any{"credit_debit_indicator": "CRDT"}, "CRDT"},
		{map[string]any{"credit_debit_indic": "dbit"}, "DBIT"},
		{map[string]any{}, ""},
	}
	for _, tc := range cases {
		got := indicString(tc.raw)
		if got != tc.want {
			t.Errorf("indicString(%v) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestFirstRemittanceLine_list(t *testing.T) {
	raw := map[string]any{"remittance_information": []any{"First", "Second"}}
	got := firstRemittanceLine(raw)
	if got != "First" {
		t.Errorf("got %q, want First", got)
	}
}

func TestFirstRemittanceLine_string(t *testing.T) {
	raw := map[string]any{"remittance_information": "The only line"}
	got := firstRemittanceLine(raw)
	if got != "The only line" {
		t.Errorf("got %q, want 'The only line'", got)
	}
}

func TestFirstRemittanceLine_emptyList(t *testing.T) {
	raw := map[string]any{"remittance_information": []any{}}
	got := firstRemittanceLine(raw)
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestFirstRemittanceLine_absent(t *testing.T) {
	got := firstRemittanceLine(map[string]any{})
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestJoinRemittance_list(t *testing.T) {
	raw := map[string]any{"remittance_information": []any{"Part1", "Part2", "Part3"}}
	got := joinRemittance(raw)
	if got != "Part1 Part2 Part3" {
		t.Errorf("got %q, want 'Part1 Part2 Part3'", got)
	}
}

func TestJoinRemittance_string(t *testing.T) {
	raw := map[string]any{"remittance_information": "Just one"}
	got := joinRemittance(raw)
	if got != "Just one" {
		t.Errorf("got %q, want 'Just one'", got)
	}
}

func TestJoinRemittance_absent(t *testing.T) {
	got := joinRemittance(map[string]any{})
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestIsOwnName(t *testing.T) {
	c := newTestClient("alice smith", "a. smith")
	if !c.isOwnName("Alice Smith") {
		t.Error("expected own name match for 'Alice Smith'")
	}
	if !c.isOwnName("A. Smith") {
		t.Error("expected own name match for 'A. Smith'")
	}
	if c.isOwnName("Bob Jones") {
		t.Error("expected no match for 'Bob Jones'")
	}
}

func TestIsOwnName_emptySet(t *testing.T) {
	c := newTestClient()
	if c.isOwnName("Anyone") {
		t.Error("expected false when ownNames is empty")
	}
}

func TestParseTransaction_booked(t *testing.T) {
	c := newTestClient()
	raw := map[string]any{
		"status":                              "BOOK",
		"transaction_date":                    "2024-06-01",
		"transaction_amount":                  map[string]any{"amount": "42.50"},
		"credit_debit_indicator":              "DBIT",
		"creditor":                            map[string]any{"name": "Landlord"},
		"remittance_information_unstructured": "June rent",
		"entry_reference":                     "REF-JUNE",
	}
	txn, err := c.parseTransaction(raw)
	if err != nil {
		t.Fatalf("parseTransaction error: %v", err)
	}
	if txn.Status != "BOOK" {
		t.Errorf("Status: got %q, want BOOK", txn.Status)
	}
	if !txn.Date.Equal(mustDate("2024-06-01")) {
		t.Errorf("Date: got %v, want 2024-06-01", txn.Date)
	}
	if txn.AmountCents != -4250 {
		t.Errorf("AmountCents: got %d, want -4250", txn.AmountCents)
	}
	if txn.Payee != "Landlord" {
		t.Errorf("Payee: got %q, want Landlord", txn.Payee)
	}
	if txn.Notes != "June rent" {
		t.Errorf("Notes: got %q, want 'June rent'", txn.Notes)
	}
	if txn.EntryRef != "REF-JUNE" {
		t.Errorf("EntryRef: got %q, want REF-JUNE", txn.EntryRef)
	}
}

func TestParseTransaction_pending(t *testing.T) {
	c := newTestClient()
	raw := map[string]any{
		"status":                 "PDNG",
		"transaction_date":       "2024-06-15",
		"transaction_amount":     map[string]any{"amount": "5.00"},
		"credit_debit_indicator": "CRDT",
		"debtor":                 map[string]any{"name": "Employer"},
	}
	txn, err := c.parseTransaction(raw)
	if err != nil {
		t.Fatalf("parseTransaction error: %v", err)
	}
	if txn.Status != "PDNG" {
		t.Errorf("Status: got %q, want PDNG", txn.Status)
	}
	if txn.AmountCents != 500 {
		t.Errorf("AmountCents: got %d, want 500", txn.AmountCents)
	}
}

func TestParseTransaction_defaultsStatusToBook(t *testing.T) {
	c := newTestClient()
	raw := map[string]any{
		"transaction_date":       "2024-01-01",
		"transaction_amount":     map[string]any{"amount": "1.00"},
		"credit_debit_indicator": "CRDT",
	}
	txn, err := c.parseTransaction(raw)
	if err != nil {
		t.Fatalf("parseTransaction error: %v", err)
	}
	if txn.Status != "BOOK" {
		t.Errorf("Status: got %q, want BOOK (default)", txn.Status)
	}
}

func TestParseTransaction_missingDate_returnsError(t *testing.T) {
	c := newTestClient()
	raw := map[string]any{
		"transaction_amount":     map[string]any{"amount": "1.00"},
		"credit_debit_indicator": "CRDT",
	}
	_, err := c.parseTransaction(raw)
	if err == nil {
		t.Error("expected error when no date field present")
	}
}

// A bank date names a calendar day, not an instant. The parser therefore cuts
// any offset off instead of interpreting it — an offset-bearing value would
// otherwise resolve to a different day in UTC, and that day would flow into both
// budget backends at once.
//
// The equivalent mistake was live in the Firefly adapter until it was caught
// against a real instance: dates were sent as instants, so every transaction
// landed one day early on any server west of UTC. Nothing in the test suite
// noticed. This pins the shared half of that class.
func TestParseDate_takesTheStatedDayNotTheInstant(t *testing.T) {
	cases := map[string]string{
		"bare date":                    "2026-08-01",
		"offset east, midnight":        "2026-08-01T00:00:00+02:00",
		"offset west, late in the day": "2026-08-01T23:59:00-11:00",
		"offset east, late in the day": "2026-08-01T23:59:00+13:00",
		"zulu":                         "2026-08-01T00:00:00Z",
		"fractional seconds":           "2026-08-01T12:00:00.123456+05:30",
	}
	want := mustDate("2026-08-01")

	for name, raw := range cases {
		got, err := parseDate(map[string]any{"transaction_date": raw})
		if err != nil {
			t.Errorf("%s (%q): %v", name, raw, err)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("%s (%q): got %s, want %s — the offset was interpreted instead "+
				"of discarded", name, raw, got.Format("2006-01-02"), want.Format("2006-01-02"))
		}
		if got.Location() != time.UTC {
			t.Errorf("%s (%q): anchored in %s, want UTC — a non-UTC anchor makes "+
				"dateToInt read a different calendar day", name, raw, got.Location())
		}
	}
}
