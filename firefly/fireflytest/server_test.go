package fireflytest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"bankingsync/firefly"
	"bankingsync/firefly/fireflytest"
)

func newPair(t *testing.T) (*fireflytest.Server, *firefly.Client) {
	t.Helper()
	s := fireflytest.New(t)
	c := firefly.New(s.URL, s.Token(),
		firefly.WithHTTPClient(s.Client()),
		firefly.WithBackoffBase(time.Millisecond))
	return s, c
}

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func seedAsset(t *testing.T, s *fireflytest.Server) *fireflytest.Account {
	t.Helper()
	return s.AddAccount("Checking", "asset", "EUR", "DE63111111111111111111")
}

func seedTxn(s *fireflytest.Server, asset *fireflytest.Account, d time.Time, amount, ref, desc string) *fireflytest.Group {
	return s.AddGroup(fireflytest.Split{
		Type: "withdrawal", Date: s.FormatDate(d), Amount: amount,
		Description: desc, SourceID: asset.ID, DestinationName: "Shop",
		ExternalID: ref, CurrencyCode: "EUR",
	})
}

func TestServer_requiresBearer(t *testing.T) {
	s := fireflytest.New(t)

	resp, err := s.Client().Get(s.URL + "/api/v1/about")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an unauthenticated request must be rejected, got %d", resp.StatusCode)
	}
}

func TestServer_paginatesAtTwoPerPage(t *testing.T) {
	s, c := newPair(t)
	asset := seedAsset(t, s)
	for i := 0; i < 5; i++ {
		seedTxn(s, asset, day(2026, time.July, 10+i), "10.00", "", "Txn")
	}

	got, err := c.GetPaged(context.Background(), "/api/v1/accounts/"+asset.ID+"/transactions", nil)
	if err != nil {
		t.Fatalf("GetPaged: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d transactions, want 5 — the client must follow every page", len(got))
	}
}

func TestServer_startAndEndAreInclusive(t *testing.T) {
	s, c := newPair(t)
	asset := seedAsset(t, s)
	seedTxn(s, asset, day(2026, time.July, 8), "10.00", "lo", "Lower edge")
	seedTxn(s, asset, day(2026, time.July, 22), "10.00", "hi", "Upper edge")
	seedTxn(s, asset, day(2026, time.July, 7), "10.00", "below", "Below")
	seedTxn(s, asset, day(2026, time.July, 23), "10.00", "above", "Above")

	q := url.Values{"start": {"2026-07-08"}, "end": {"2026-07-22"}}
	got, err := c.GetPaged(context.Background(), "/api/v1/accounts/"+asset.ID+"/transactions", q)
	if err != nil {
		t.Fatalf("GetPaged: %v", err)
	}
	refs := externalIDs(t, got)
	if !refs["lo"] || !refs["hi"] {
		t.Errorf("both edges must be included, got %v", keys(refs))
	}
	if refs["below"] || refs["above"] {
		t.Errorf("outside days must be excluded, got %v", keys(refs))
	}
}

func TestServer_externalIDIsIsExactAndUnscoped(t *testing.T) {
	s, c := newPair(t)
	assetA := s.AddAccount("A", "asset", "EUR", "DE1")
	assetB := s.AddAccount("B", "asset", "EUR", "DE2")
	seedTxn(s, assetA, day(2026, time.July, 10), "10.00", "ref-1", "On A")
	seedTxn(s, assetB, day(2026, time.July, 10), "10.00", "ref-1", "On B")
	seedTxn(s, assetA, day(2026, time.July, 10), "10.00", "xxref-1yy", "Substring")

	q := url.Values{"query": {`external_id_is:"ref-1"`}}
	got, err := c.GetPaged(context.Background(), "/api/v1/search/transactions", q)
	if err != nil {
		t.Fatalf("GetPaged: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d hits, want 2: the operator is exact (no substring) and global (both accounts)", len(got))
	}
}

func TestServer_putWithPartialSplitsDestroysTheOthers(t *testing.T) {
	s, c := newPair(t)
	asset := seedAsset(t, s)
	g := seedTxn(s, asset, day(2026, time.July, 10), "10.00", "ref-1", "Ours")
	if err := s.SplitGroup(g.ID); err != nil {
		t.Fatalf("SplitGroup: %v", err)
	}
	if got := len(s.Groups()[0].Splits); got != 2 {
		t.Fatalf("setup: got %d splits, want 2", got)
	}
	ours := s.Groups()[0].Splits[0].JournalID

	_, err := c.Put(context.Background(), "/api/v1/transactions/"+g.ID, map[string]any{
		"transactions": []map[string]any{{
			"transaction_journal_id": ours,
			"description":            "Updated",
		}},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if got := len(s.Groups()[0].Splits); got != 1 {
		t.Fatalf("got %d splits, want 1 — Firefly deletes journals missing from the array, "+
			"and the fixture must reproduce that or the guard is never tested", got)
	}
}

func TestServer_tagsAreReplacedNotMerged(t *testing.T) {
	s, c := newPair(t)
	asset := seedAsset(t, s)
	g := s.AddGroup(fireflytest.Split{
		Type: "withdrawal", Date: s.FormatDate(day(2026, time.July, 10)), Amount: "10.00",
		Description: "Ours", SourceID: asset.ID, DestinationName: "Shop",
		ExternalID: "ref-1", Tags: []string{"pending", "urlaub"},
	})
	journal := g.Splits[0].JournalID

	if _, err := c.Put(context.Background(), "/api/v1/transactions/"+g.ID, map[string]any{
		"transactions": []map[string]any{{"transaction_journal_id": journal, "description": "Untouched tags"}},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := s.Groups()[0].Splits[0].Tags; len(got) != 2 {
		t.Errorf("omitting the tags key must leave them alone, got %v", got)
	}

	if _, err := c.Put(context.Background(), "/api/v1/transactions/"+g.ID, map[string]any{
		"transactions": []map[string]any{{"transaction_journal_id": journal, "tags": []string{"urlaub"}}},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got := s.Groups()[0].Splits[0].Tags
	if len(got) != 1 || got[0] != "urlaub" {
		t.Errorf("supplying tags must replace the whole set, got %v", got)
	}
}

func TestServer_reconciledBlocksAmountEdit(t *testing.T) {
	s, c := newPair(t)
	asset := seedAsset(t, s)
	g := s.AddGroup(fireflytest.Split{
		Type: "withdrawal", Date: s.FormatDate(day(2026, time.July, 10)), Amount: "10.00",
		Description: "Reconciled", SourceID: asset.ID, DestinationName: "Shop", Reconciled: true,
	})

	_, err := c.Put(context.Background(), "/api/v1/transactions/"+g.ID, map[string]any{
		"transactions": []map[string]any{{
			"transaction_journal_id": g.Splits[0].JournalID,
			"amount":                 "12.50",
		}},
	})
	if err == nil {
		t.Fatal("Firefly refuses to change the amount of a reconciled transaction")
	}
}

func TestServer_rejectsZeroAmountAndEmptyDescription(t *testing.T) {
	s, c := newPair(t)
	asset := seedAsset(t, s)

	_, err := c.Post(context.Background(), "/api/v1/transactions", map[string]any{
		"transactions": []map[string]any{{
			"type": "withdrawal", "date": "2026-07-10T00:00:00+00:00",
			"amount": "0", "description": "Zero", "source_id": asset.ID, "destination_name": "Shop",
		}},
	})
	assertStatus(t, err, http.StatusUnprocessableEntity, "a zero amount")

	_, err = c.Post(context.Background(), "/api/v1/transactions", map[string]any{
		"transactions": []map[string]any{{
			"type": "withdrawal", "date": "2026-07-10T00:00:00+00:00",
			"amount": "10.00", "description": "", "source_id": asset.ID, "destination_name": "Shop",
		}},
	})
	assertStatus(t, err, http.StatusUnprocessableEntity, "an empty description")
}

// TestServer_unknownGroupIsUnauthenticated pins the status a real Firefly gives,
// which is not the one anybody would predict. Verified against 6.6.6: a deleted
// group, an id that never existed and an id that is not a number all come back
// 401, never 404.
func TestServer_unknownGroupIsUnauthenticated(t *testing.T) {
	_, c := newPair(t)
	_, err := c.Put(context.Background(), "/api/v1/transactions/9999", map[string]any{
		"transactions": []map[string]any{{"transaction_journal_id": "1"}},
	})
	assertStatus(t, err, http.StatusUnauthorized, "an unknown group")
}

func TestServer_duplicateHashDependsOnExternalID(t *testing.T) {
	s, c := newPair(t)
	asset := seedAsset(t, s)

	post := func(ref string) error {
		_, err := c.Post(context.Background(), "/api/v1/transactions", map[string]any{
			"error_if_duplicate_hash": true,
			"transactions": []map[string]any{{
				"type": "withdrawal", "date": "2026-07-10T00:00:00+00:00",
				"amount": "10.00", "description": "Kaffee",
				"source_id": asset.ID, "destination_name": "Cafe", "external_id": ref,
			}},
		})
		return err
	}

	if err := post("ref-1"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := post("ref-2"); err != nil {
		t.Fatalf("a different external id must not collide: %v", err)
	}
	if err := post("ref-1"); err == nil {
		t.Fatal("the same external id and payload must be rejected as a duplicate")
	}

	if err := post(""); err != nil {
		t.Fatalf("first reference-less: %v", err)
	}
	err := post("")
	assertStatus(t, err, http.StatusUnprocessableEntity,
		"two reference-less twins are byte-identical and do collide")
}

func TestServer_returnsDatesWithNonUTCOffset(t *testing.T) {
	s, c := newPair(t)
	asset := seedAsset(t, s)
	seedTxn(s, asset, day(2026, time.July, 1), "10.00", "ref-1", "Txn")

	got, err := c.GetPaged(context.Background(), "/api/v1/accounts/"+asset.ID+"/transactions", nil)
	if err != nil {
		t.Fatalf("GetPaged: %v", err)
	}
	raw := firstSplit(t, got[0])
	date, _ := raw["date"].(string)
	if strings.HasSuffix(date, "+00:00") || strings.HasSuffix(date, "Z") {
		t.Fatalf("date %q is UTC — the fixture must serve a shifted offset by default, "+
			"otherwise the timezone bug it exists to expose can never fire", date)
	}
	if !strings.Contains(date, "+") && !strings.Contains(date[10:], "-") {
		t.Fatalf("date %q carries no offset at all", date)
	}

	parsed, err := firefly.ParseDate(date)
	if err != nil {
		t.Fatalf("ParseDate: %v", err)
	}
	if !parsed.Equal(day(2026, time.July, 1)) {
		t.Errorf("got %v, want 2026-07-01 — the calendar day must survive the offset", parsed)
	}
}

func TestServer_autoCreatesCounterpartyAccount(t *testing.T) {
	s, c := newPair(t)
	asset := seedAsset(t, s)
	before := len(s.Accounts())

	if _, err := c.Post(context.Background(), "/api/v1/transactions", map[string]any{
		"transactions": []map[string]any{{
			"type": "withdrawal", "date": "2026-07-10T00:00:00+00:00",
			"amount": "10.00", "description": "Kaffee",
			"source_id": asset.ID, "destination_name": "Ein Ganz Neuer Laden",
		}},
	}); err != nil {
		t.Fatalf("Post: %v", err)
	}

	if got := len(s.Accounts()); got != before+1 {
		t.Fatalf("an unknown destination name must create an expense account: %d -> %d", before, got)
	}
}

func TestServer_accountCurrencyOverrulesSubmitted(t *testing.T) {
	s, c := newPair(t)
	asset := s.AddAccount("Checking", "asset", "EUR", "DE1")

	if _, err := c.Post(context.Background(), "/api/v1/transactions", map[string]any{
		"transactions": []map[string]any{{
			"type": "withdrawal", "date": "2026-07-10T00:00:00+00:00",
			"amount": "10.00", "description": "Prag", "currency_code": "CZK",
			"source_id": asset.ID, "destination_name": "Shop",
		}},
	}); err != nil {
		t.Fatalf("Post: %v", err)
	}

	if got := s.Groups()[0].Splits[0].CurrencyCode; got != "EUR" {
		t.Errorf("the asset account's currency must overrule the submitted one, got %q — "+
			"this is why a mismatch has to fail before the write", got)
	}
}

func TestServer_deleteGroupKnob(t *testing.T) {
	s, c := newPair(t)
	asset := seedAsset(t, s)
	g := seedTxn(s, asset, day(2026, time.July, 10), "10.00", "ref-1", "Txn")

	s.DeleteGroup(g.ID)

	_, err := c.Put(context.Background(), "/api/v1/transactions/"+g.ID, map[string]any{
		"transactions": []map[string]any{{"transaction_journal_id": g.Splits[0].JournalID}},
	})
	assertStatus(t, err, http.StatusUnauthorized, "a group the user deleted")
}

func TestServer_failAndRateLimitKnobs(t *testing.T) {
	s, c := newPair(t)
	asset := seedAsset(t, s)

	s.FailNextWrites(1)
	_, err := c.Post(context.Background(), "/api/v1/transactions", map[string]any{
		"transactions": []map[string]any{{
			"type": "withdrawal", "date": "2026-07-10T00:00:00+00:00",
			"amount": "10.00", "description": "Fails", "source_id": asset.ID, "destination_name": "Shop",
		}},
	})
	assertStatus(t, err, http.StatusInternalServerError, "an injected write failure")

	s.RateLimitNext(1)
	if _, err := c.About(context.Background()); err != nil {
		t.Fatalf("a single 429 must be absorbed by the retry: %v", err)
	}
}

func assertStatus(t *testing.T, err error, want int, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s must be rejected", what)
	}
	apiErr, ok := err.(*firefly.APIError)
	if !ok {
		t.Fatalf("%s: want *APIError, got %T: %v", what, err, err)
	}
	if apiErr.Status != want {
		t.Errorf("%s: got status %d, want %d", what, apiErr.Status, want)
	}
}

func firstSplit(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var g struct {
		Attributes struct {
			Transactions []map[string]any `json:"transactions"`
		} `json:"attributes"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("decode group: %v", err)
	}
	if len(g.Attributes.Transactions) == 0 {
		t.Fatal("group has no splits")
	}
	return g.Attributes.Transactions[0]
}

func externalIDs(t *testing.T, groups []json.RawMessage) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, raw := range groups {
		sp := firstSplit(t, raw)
		if id, _ := sp["external_id"].(string); id != "" {
			out[id] = true
		}
	}
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Firefly's account search is whereLike('%query%') for iban and name
// (app/Support/Search/AccountSearch.php). Pinning it here keeps the fixture from
// drifting back to an exact match, which would make every client that trusts a
// search hit look correct.
func TestSearchAccounts_isASubstringMatchForNameAndIBAN(t *testing.T) {
	s := fireflytest.New(t)
	s.AddAccount("Old Checking", "asset", "EUR", "DE31123456789005193987EXTRA")

	for _, tc := range []struct {
		field, query string
	}{
		{"name", "Checking"},
		{"iban", "DE31123456789005193987"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, s.URL+"/api/v1/search/accounts?type=asset&field="+
				tc.field+"&query="+url.QueryEscape(tc.query), nil)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+s.Token())
			res, err := s.Client().Do(req)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			defer res.Body.Close()

			var body struct {
				Data []json.RawMessage `json:"data"`
			}
			if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(body.Data) != 1 {
				t.Fatalf("field=%s query=%q returned %d accounts, want 1 — the fixture "+
					"is matching exactly where Firefly matches on a substring",
					tc.field, tc.query, len(body.Data))
			}
		})
	}
}

// Firefly validates opening_balance and opening_balance_date with a mutual
// required_with, so either one alone is a 422. Pinning it here is what stops a
// client that forgets the second field from passing against this fixture and
// then failing against a real instance.
func TestUpdateAccount_openingBalanceFieldsAreMutuallyRequired(t *testing.T) {
	s := fireflytest.New(t)
	acct := s.AddAccount("Giro", "asset", "EUR", "")

	cases := []struct {
		name   string
		body   map[string]any
		status int
	}{
		{"amount only", map[string]any{"opening_balance": "100.00"}, http.StatusUnprocessableEntity},
		{"date only", map[string]any{"opening_balance_date": "2026-07-14"}, http.StatusUnprocessableEntity},
		{"both", map[string]any{"opening_balance": "100.00", "opening_balance_date": "2026-07-14"}, http.StatusOK},
		{"neither", map[string]any{"name": "Giro neu"}, http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, _ := json.Marshal(tc.body)
			req, err := http.NewRequest(http.MethodPut, s.URL+"/api/v1/accounts/"+acct.ID,
				bytes.NewReader(payload))
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+s.Token())
			res, err := s.Client().Do(req)
			if err != nil {
				t.Fatalf("put: %v", err)
			}
			defer res.Body.Close()

			if res.StatusCode != tc.status {
				t.Errorf("status: got %d, want %d", res.StatusCode, tc.status)
			}
		})
	}
}
