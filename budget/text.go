package budget

import (
	"strings"
	"unicode"
)

// TitleCase collapses whitespace and lowercases all but the first letter of each
// word, but only when the input is entirely upper case. Mixed-case input is
// returned with whitespace collapsed and nothing else changed.
func TitleCase(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	if !isAllUpper(s) {
		return s
	}
	words := strings.Split(s, " ")
	for i, w := range words {
		r := []rune(w)
		if len(r) == 0 {
			continue
		}
		for j := 1; j < len(r); j++ {
			r[j] = unicode.ToLower(r[j])
		}
		words[i] = string(r)
	}
	return strings.Join(words, " ")
}

func isAllUpper(s string) bool {
	hasLetter := false
	for _, r := range s {
		if unicode.IsLetter(r) {
			hasLetter = true
			if unicode.IsLower(r) {
				return false
			}
		}
	}
	return hasLetter
}

// PayeeWasRenamedByUser reports whether the payee on t differs from what the
// bank supplied, which means a user edited it and the importer must not
// overwrite it.
func PayeeWasRenamedByUser(t *Transaction) bool {
	if t.PayeeName == "" {
		return false
	}
	if t.ImportedPayee == "" {
		return true
	}
	return t.PayeeName != TitleCase(t.ImportedPayee)
}
