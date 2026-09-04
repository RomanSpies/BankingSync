package firefly

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestEncodeSplitID_roundTrip(t *testing.T) {
	id, err := EncodeID("1234", "5678")
	if err != nil {
		t.Fatalf("EncodeID: %v", err)
	}
	if id != "1234:5678" {
		t.Errorf("got %q, want 1234:5678", id)
	}

	group, journal, err := SplitID(id)
	if err != nil {
		t.Fatalf("SplitID: %v", err)
	}
	if group != "1234" || journal != "5678" {
		t.Errorf("got %q/%q, want 1234/5678", group, journal)
	}
}

func TestEncodeID_rejectsReservedCharacters(t *testing.T) {
	cases := []struct{ group, journal string }{
		{"12|34", "5678"},
		{"1234", "56|78"},
		{"12:34", "5678"},
		{"", "5678"},
		{"1234", ""},
	}
	for _, tc := range cases {
		if _, err := EncodeID(tc.group, tc.journal); err == nil {
			t.Errorf("EncodeID(%q, %q) must fail: a reserved character would corrupt the pending map value",
				tc.group, tc.journal)
		}
	}
}

func TestSplitID_rejectsMalformed(t *testing.T) {
	for _, in := range []string{"1234", "", ":", "1234:"} {
		if _, _, err := SplitID(in); err == nil {
			t.Errorf("SplitID(%q) must fail", in)
		}
	}
}

// The offset is the whole point: with one, Firefly reads an instant and renders
// it in the server's timezone, which moves the calendar day for every server
// west of UTC. Without one it is wall-clock time and the day survives.
func TestFormatDate_carriesNoOffset(t *testing.T) {
	d := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	got := FormatDate(d)
	if got != "2026-07-01T00:00:00" {
		t.Errorf("got %q, want %q", got, "2026-07-01T00:00:00")
	}
	if strings.ContainsAny(got, "+Z") {
		t.Errorf("got %q — an offset turns the value into an instant, and a "+
			"Firefly server west of UTC would store the previous day", got)
	}
}

func TestParseDate_keepsTheLocalCalendarDay(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
	}{
		{"2026-07-01T00:00:00+02:00", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
		{"2026-07-01T00:00:00-05:00", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
		{"2026-07-01T00:00:00+00:00", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
		{"2026-07-01T23:30:00+02:00", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
		{"2026-07-01", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		got, err := ParseDate(tc.in)
		if err != nil {
			t.Fatalf("ParseDate(%q): %v", tc.in, err)
		}
		if !got.Equal(tc.want) {
			t.Errorf("ParseDate(%q) = %v, want %v — converting to UTC would shift the day and move the match window",
				tc.in, got, tc.want)
		}
	}
}

func TestParseDate_rejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "   ", "not-a-date"} {
		if _, err := ParseDate(in); err == nil {
			t.Errorf("ParseDate(%q) must fail", in)
		}
	}
}

func TestTypeForAmount(t *testing.T) {
	if got, err := TypeForAmount(-1); err != nil || got != typeWithdrawal {
		t.Errorf("negative: got %q, %v", got, err)
	}
	if got, err := TypeForAmount(1); err != nil || got != typeDeposit {
		t.Errorf("positive: got %q, %v", got, err)
	}
	if _, err := TypeForAmount(0); err == nil {
		t.Error("a zero amount has no direction and must be rejected before it reaches the API")
	}
}

func TestFormatAmount_isAlwaysPositive(t *testing.T) {
	cases := []struct {
		cents int64
		want  string
	}{
		{-1250, "12.50"}, {1250, "12.50"}, {-5, "0.05"}, {100, "1.00"}, {-100000, "1000.00"},
	}
	for _, tc := range cases {
		if got := FormatAmount(tc.cents); got != tc.want {
			t.Errorf("FormatAmount(%d) = %q, want %q", tc.cents, got, tc.want)
		}
	}
}

func TestSignedCents_orientsOnTheAssetSide(t *testing.T) {
	const asset = "7"

	got, err := SignedCents("12.50", asset, "99", asset)
	if err != nil {
		t.Fatalf("outgoing: %v", err)
	}
	if got != -1250 {
		t.Errorf("money leaving the asset account must be negative, got %d", got)
	}

	got, err = SignedCents("12.50", "99", asset, asset)
	if err != nil {
		t.Fatalf("incoming: %v", err)
	}
	if got != 1250 {
		t.Errorf("money arriving must be positive, got %d", got)
	}
}

func TestSignedCents_normalisesBeforeApplyingDirection(t *testing.T) {
	const asset = "7"
	got, err := SignedCents("-12.50", "99", asset, asset)
	if err != nil {
		t.Fatalf("SignedCents: %v", err)
	}
	if got != 1250 {
		t.Errorf("a negative payload must not double-invert an incoming amount, got %d", got)
	}
}

func TestSignedCents_transferInBothDirections(t *testing.T) {
	const ours, theirs = "7", "8"

	out, err := SignedCents("40.00", ours, theirs, ours)
	if err != nil {
		t.Fatalf("outgoing transfer: %v", err)
	}
	in, err := SignedCents("40.00", theirs, ours, ours)
	if err != nil {
		t.Fatalf("incoming transfer: %v", err)
	}
	if out != -4000 || in != 4000 {
		t.Errorf("a transfer must be signed from our side: got %d and %d", out, in)
	}
}

func TestSignedCents_rejectsForeignTransaction(t *testing.T) {
	if _, err := SignedCents("10.00", "1", "2", "7"); err == nil {
		t.Error("a transaction touching neither side of our account must not be signed at all")
	}
	if _, err := SignedCents("10.00", "1", "2", ""); err == nil {
		t.Error("without an asset account there is no direction to derive")
	}
}

func TestRouteTemplate_collapsesIdentifiers(t *testing.T) {
	for in, want := range map[string]string{
		"/api/v1/transactions/161":        "/api/v1/transactions/{id}",
		"/api/v1/accounts/9":              "/api/v1/accounts/{id}",
		"/api/v1/accounts/9/transactions": "/api/v1/accounts/{id}/transactions",
		"/api/v1/accounts":                "/api/v1/accounts",
		"/api/v1/search/transactions":     "/api/v1/search/transactions",
		"/api/v1/about":                   "/api/v1/about",
	} {
		if got := routeTemplate(in); got != want {
			t.Errorf("routeTemplate(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRouteTemplate_leavesWordsAlone guards the other direction. Collapsing a
// path segment that is not an identifier would merge unrelated endpoints into one
// series and one span name, which hides exactly what the label is for.
func TestRouteTemplate_leavesWordsAlone(t *testing.T) {
	for _, s := range []string{
		"/api/v1/transactions",
		"/api/v1/accounts/summary",
		"/api/v1/v2",
	} {
		if got := routeTemplate(s); got != s {
			t.Errorf("routeTemplate(%q) = %q, want it unchanged", s, got)
		}
	}
}

// TestIsDuplicateRejection_separatesADuplicateFromAValidationError is the whole
// point of matching on the message rather than on the status.
//
// 422 is what Firefly answers for a duplicate and for every validation failure
// alike — a rejected IBAN, a currency code that is too short, a date range it
// dislikes. Counting all of them as duplicates would make the metric say that
// bankingsync is importing the same rows twice when it is not.
func TestIsDuplicateRejection_separatesADuplicateFromAValidationError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"the duplicate itself", &APIError{
			Status: http.StatusUnprocessableEntity,
			Body:   `{"message":"Duplicate of transaction #1."}`,
		}, true},
		{"case does not matter", &APIError{
			Status: http.StatusUnprocessableEntity,
			Body:   `{"message":"duplicate of transaction #17."}`,
		}, true},
		{"a rejected IBAN", &APIError{
			Status: http.StatusUnprocessableEntity,
			Body:   `{"message":"The iban is not a valid IBAN."}`,
		}, false},
		{"a currency that is too short", &APIError{
			Status: http.StatusUnprocessableEntity,
			Body:   `{"message":"The currency code must be at least 3 characters."}`,
		}, false},
		{"a date range Firefly dislikes", &APIError{
			Status: http.StatusUnprocessableEntity,
			Body:   `{"message":"The start must be a date before end."}`,
		}, false},
		{"the same words under another status", &APIError{
			Status: http.StatusInternalServerError,
			Body:   `{"message":"Duplicate of transaction #1."}`,
		}, false},
		{"not an API error at all", errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDuplicateRejection(tc.err); got != tc.want {
				t.Errorf("isDuplicateRejection() = %v, want %v", got, tc.want)
			}
		})
	}
}
