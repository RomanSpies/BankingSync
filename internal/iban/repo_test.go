package iban_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"bankingsync/internal/iban"
)

// ibanShaped is deliberately looser than an IBAN. The point is to catch the
// strings a reader would take for one, including the ones that are not.
var ibanShaped = regexp.MustCompile(`\b[A-Z]{2}[0-9]{2}[A-Z0-9]{11,30}\b`)

// otherScheme lists IBAN-shaped literals that are not IBANs at all. Nothing is
// asserted about them: a SEPA creditor identifier has its own layout and is
// under no obligation to satisfy mod-97 — one of the two below happens to, which
// is precisely why the two cases cannot share a list.
var otherScheme = map[string]string{
	"DE98ZZZ09999999999": "SEPA creditor identifier",
	"DE12ZZZ00000000000": "SEPA creditor identifier in a remittance-information fixture",
}

// deliberatelyBroken lists literals that must stay invalid, because being
// invalid is what they are for. Asserting that keeps a well-meaning fix from
// quietly removing the counterexample a test depends on.
var deliberatelyBroken = map[string]string{
	"DE31123456789005193987EXTRA": "the substring hit Store.GetOrCreateAccount must refuse to adopt; " +
		"27 characters, so a real Firefly could never hold it",
	"DE11111111111111111111": "the literal TestStoreLive_refusesAnInventedIBAN hands to a real " +
		"instance to establish that it validates; it has to stay wrong to mean anything",
}

// TestRepoIBANLiteralsAreValid keeps hand-written IBANs arithmetically correct.
//
// It exists because a real Firefly III instance validates what it is handed and
// the fixture does not. Eleven of the twelve IBAN-shaped literals in this
// repository were invented and failed mod-97, which stayed invisible for as long
// as no test reached a real instance. The rule is cheap to hold and the failure
// it prevents — a live test rejected with a 422 that names a field rather than a
// cause — is expensive to diagnose.
//
// It skips its own package, which necessarily holds wrong check digits as test
// input.
func TestRepoIBANLiteralsAreValid(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolving the repository root: %v", err)
	}

	seen := map[string]string{}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".cache", "node_modules":
				return filepath.SkipDir
			case "iban":
				if filepath.Dir(path) == filepath.Join(root, "internal") {
					return filepath.SkipDir
				}
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".html", ".md":
		default:
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, m := range ibanShaped.FindAllString(string(body), -1) {
			if _, ok := seen[m]; !ok {
				seen[m] = rel
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if len(seen) == 0 {
		t.Fatal("no IBAN-shaped literal found anywhere; the scan is not looking at the repository")
	}

	for literal, where := range seen {
		if _, other := otherScheme[literal]; other {
			continue
		}
		if reason, broken := deliberatelyBroken[literal]; broken {
			if iban.Valid(literal) {
				t.Errorf("%s: %q is listed as a counterexample (%s) but passes mod-97; "+
					"the test that relies on it is no longer testing what it says",
					where, literal, reason)
			}
			continue
		}
		if !iban.Valid(literal) {
			t.Errorf("%s: %q fails its own check digits. A live Firefly rejects it. "+
				"Fix the two check digits, use iban.GenerateDE, or list it in otherScheme/deliberatelyBroken with a reason.",
				where, literal)
		}
	}
}

// TestRepoIBANExemptionsAreUsed keeps the list from outliving its entries. An
// exemption nobody needs is an exemption nobody rechecks.
func TestRepoIBANExemptionsAreUsed(t *testing.T) {
	root, _ := filepath.Abs("../..")
	var body strings.Builder
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && (d.Name() == ".git" || d.Name() == ".cache") {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".html", ".md":
			b, err := os.ReadFile(path)
			if err == nil {
				body.Write(b)
			}
		}
		return nil
	})
	text := body.String()
	for _, list := range []map[string]string{otherScheme, deliberatelyBroken} {
		for literal := range list {
			// Two occurrences: the entry in the list itself, plus at least one real use.
			if strings.Count(text, literal) < 2 {
				t.Errorf("%q is exempted but no longer appears in the repository; remove the entry", literal)
			}
		}
	}
}
