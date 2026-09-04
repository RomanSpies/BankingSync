package enablebanking

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files from current parser output")

type goldenTxn struct {
	Status           string `json:"status"`
	Date             string `json:"date"`
	AmountCents      int64  `json:"amount_cents"`
	Currency         string `json:"currency"`
	Payee            string `json:"payee"`
	Notes            string `json:"notes"`
	EntryRef         string `json:"entry_ref"`
	CounterpartyIBAN string `json:"counterparty_iban"`
	SEPAEndToEnd     string `json:"sepa_end_to_end"`
	SEPAMandate      string `json:"sepa_mandate"`
	SEPACreditorID   string `json:"sepa_creditor_id"`
}

func toGolden(t Transaction) goldenTxn {
	return goldenTxn{
		Status:           t.Status,
		Date:             t.Date.Format("2006-01-02"),
		AmountCents:      t.AmountCents,
		Currency:         t.Currency,
		Payee:            t.Payee,
		Notes:            t.Notes,
		EntryRef:         t.EntryRef,
		CounterpartyIBAN: t.CounterpartyIBAN,
		SEPAEndToEnd:     t.SEPA.EndToEnd,
		SEPAMandate:      t.SEPA.Mandate,
		SEPACreditorID:   t.SEPA.CreditorID,
	}
}

type fixturePage struct {
	Transactions    []map[string]any `json:"transactions"`
	ContinuationKey string           `json:"continuation_key"`
}

func fixtureFiles(t *testing.T) []string {
	t.Helper()
	all, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatalf("glob testdata: %v", err)
	}
	var out []string
	for _, f := range all {
		if strings.HasSuffix(f, ".golden.json") {
			continue
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		t.Fatal("no fixtures found in testdata/")
	}
	sort.Strings(out)
	return out
}

func loadFixture(t *testing.T, path string) fixturePage {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var page fixturePage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return page
}

func goldenPath(fixture string) string {
	return strings.TrimSuffix(fixture, ".json") + ".golden.json"
}

func TestFixtures_parseToGolden(t *testing.T) {
	c := newTestClient()

	for _, fixture := range fixtureFiles(t) {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			page := loadFixture(t, fixture)
			if len(page.Transactions) == 0 {
				t.Fatal("fixture contains no transactions")
			}

			got := make([]goldenTxn, 0, len(page.Transactions))
			for i, raw := range page.Transactions {
				parsed, err := c.parseTransaction(raw)
				if err != nil {
					t.Fatalf("record %d failed to parse (a real payload must never drop): %v", i, err)
				}
				got = append(got, toGolden(parsed))
			}

			encoded, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatalf("encode golden: %v", err)
			}
			encoded = append(encoded, '\n')

			path := goldenPath(fixture)
			if *updateGolden {
				if err := os.WriteFile(path, encoded, 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				t.Logf("updated %s", path)
				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s (run: go test ./enablebanking -update): %v", path, err)
			}
			if string(encoded) != string(want) {
				t.Errorf("parsed output drifted from golden\n--- got ---\n%s\n--- want ---\n%s", encoded, want)
			}
		})
	}
}

func TestFixtures_noDroppedRecords(t *testing.T) {
	c := newTestClient()
	for _, fixture := range fixtureFiles(t) {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			page := loadFixture(t, fixture)
			dropped := 0
			for _, raw := range page.Transactions {
				if _, err := c.parseTransaction(raw); err != nil {
					dropped++
				}
			}
			if dropped != 0 {
				t.Errorf("%d record(s) dropped, want 0", dropped)
			}
		})
	}
}

var knownTransactionFields = map[string]bool{
	"entry_reference":                     true,
	"transaction_id":                      true,
	"transaction_date":                    true,
	"booking_date":                        true,
	"value_date":                          true,
	"status":                              true,
	"credit_debit_indicator":              true,
	"credit_debit_indic":                  true,
	"transaction_amount":                  true,
	"creditor":                            true,
	"creditor_name":                       true,
	"creditor_account":                    true,
	"debtor":                              true,
	"debtor_name":                         true,
	"debtor_account":                      true,
	"remittance_information":              true,
	"remittance_information_unstructured": true,

	"balance_after_transaction":                  true,
	"bank_transaction_code":                      true,
	"creditor_account_additional_identification": true,
	"creditor_agent":                             true,
	"debtor_account_additional_identification":   true,
	"debtor_agent":                               true,
	"exchange_rate":                              true,
	"merchant_category_code":                     true,
	"note":                                       true,
	"reference_number":                           true,
	"reference_number_schema":                    true,
}

func TestFixtures_shapeDriftCanary(t *testing.T) {
	seen := make(map[string]map[string]bool)

	for _, fixture := range fixtureFiles(t) {
		page := loadFixture(t, fixture)
		for _, raw := range page.Transactions {
			for key := range raw {
				if knownTransactionFields[key] {
					continue
				}
				if seen[key] == nil {
					seen[key] = make(map[string]bool)
				}
				seen[key][filepath.Base(fixture)] = true
			}
		}
	}

	if len(seen) == 0 {
		return
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		files := make([]string, 0, len(seen[k]))
		for f := range seen[k] {
			files = append(files, f)
		}
		sort.Strings(files)
		t.Errorf("unreviewed transaction field %q appears in %v — review it, then add it to knownTransactionFields", k, files)
	}
}

func TestFixtures_parserReadFieldsArePresent(t *testing.T) {
	required := []string{
		"entry_reference",
		"transaction_id",
		"transaction_date",
		"booking_date",
		"value_date",
		"credit_debit_indicator",
		"credit_debit_indic",
		"transaction_amount",
		"creditor",
		"creditor_name",
		"debtor_name",
		"remittance_information",
		"remittance_information_unstructured",
	}

	present := make(map[string]bool)
	for _, fixture := range fixtureFiles(t) {
		page := loadFixture(t, fixture)
		for _, raw := range page.Transactions {
			for key := range raw {
				present[key] = true
			}
		}
	}

	for _, field := range required {
		if !present[field] {
			t.Errorf("no fixture exercises %q — the parser branch reading it is untested", field)
		}
	}
}
