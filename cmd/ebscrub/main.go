package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
)

var preserveKeys = map[string]bool{
	"status":                 true,
	"credit_debit_indicator": true,
	"credit_debit_indic":     true,
	"currency":               true,
	"transaction_date":       true,
	"booking_date":           true,
	"value_date":             true,
}

var tokenKeys = map[string]bool{
	"continuation_key": true,
}

var identityKeys = map[string]bool{
	"entry_reference": true,
	"transaction_id":  true,
	"resource_id":     true,
	"uid":             true,
	"account_uid":     true,
	"session_id":      true,
}

var nameKeys = map[string]bool{
	"name":          true,
	"creditor_name": true,
	"debtor_name":   true,
	"owner_name":    true,
}

var ibanKeys = map[string]bool{
	"iban":   true,
	"bban":   true,
	"pan":    true,
	"masked": true,
}

var amountKeys = map[string]bool{
	"amount": true,
}

var payees = []string{
	"Acme GmbH", "Beispiel AG", "Contoso Ltd", "Muster Handel",
	"Nordwind Energie", "Beispielmarkt", "Testverlag", "Rhein Logistik",
}

var narratives = []string{
	"Rechnung 1001", "Dauerauftrag", "Kartenzahlung", "Lastschrift Beitrag",
	"Ueberweisung", "Abonnement", "Erstattung", "Gutschrift",
}

func hashOf(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

func pick(list []string, seed string) string {
	return list[hashOf(seed)%uint64(len(list))]
}

func syntheticID(seed string) string {
	return fmt.Sprintf("ref-%012x", hashOf(seed)%0xffffffffffff)
}

func syntheticIBAN(seed string) string {
	return fmt.Sprintf("DE%02d1234567890%08d", hashOf(seed)%90+10, hashOf(seed)%100000000)
}

func syntheticAmount(seed string) string {
	cents := hashOf(seed)%99000 + 100
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

func scrubString(key, value string) string {
	if value == "" {
		return ""
	}
	switch {
	case tokenKeys[key]:
		return fmt.Sprintf("ck-%016x", hashOf(value))
	case identityKeys[key]:
		return syntheticID(value)
	case nameKeys[key]:
		return pick(payees, value)
	case ibanKeys[key]:
		return syntheticIBAN(value)
	case amountKeys[key]:
		return syntheticAmount(value)
	case strings.HasPrefix(key, "remittance_information"):
		return pick(narratives, value)
	default:
		return pick(narratives, value)
	}
}

func scrub(key string, node any) any {
	switch v := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, child := range v {
			out[k] = scrub(k, child)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = scrub(key, child)
		}
		return out
	case string:
		if preserveKeys[key] {
			return v
		}
		return scrubString(key, v)
	case float64:
		if amountKeys[key] {
			cents := hashOf(fmt.Sprint(v))%99000 + 100
			return float64(cents) / 100
		}
		return v
	default:
		return v
	}
}

func scrubBytes(in []byte) ([]byte, error) {
	var decoded any
	if err := json.Unmarshal(in, &decoded); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	out, err := json.MarshalIndent(scrub("", decoded), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func scrubFile(inPath, outPath string) error {
	raw, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	clean, err := scrubBytes(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", inPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(outPath, clean, 0o644)
}

func run(in, out string) error {
	info, err := os.Stat(in)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return scrubFile(in, out)
	}

	entries, err := os.ReadDir(in)
	if err != nil {
		return err
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if err := scrubFile(filepath.Join(in, e.Name()), filepath.Join(out, e.Name())); err != nil {
			return err
		}
		count++
	}
	fmt.Fprintf(os.Stderr, "scrubbed %d file(s) into %s\n", count, out)
	return nil
}

func main() {
	in := flag.String("in", "", "input file or directory of captured responses")
	out := flag.String("out", "", "output file or directory for scrubbed fixtures")
	flag.Parse()

	if *in == "" || *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*in, *out); err != nil {
		fmt.Fprintf(os.Stderr, "ebscrub: %v\n", err)
		os.Exit(1)
	}
}
