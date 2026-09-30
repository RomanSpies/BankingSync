package enablebanking

import "testing"

func TestRefSource_namesWhereTheReferenceCameFrom(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  map[string]any
		want string
	}{
		"both":                   {map[string]any{"entry_reference": "e", "transaction_id": "t"}, RefSourceEntryReference},
		"entry reference only":   {map[string]any{"entry_reference": "e"}, RefSourceEntryReference},
		"transaction id only":    {map[string]any{"transaction_id": "t"}, RefSourceTransactionID},
		"neither":                {map[string]any{}, RefSourceNone},
		"empty strings":          {map[string]any{"entry_reference": "", "transaction_id": ""}, RefSourceNone},
		"empty entry, id filled": {map[string]any{"entry_reference": "", "transaction_id": "t"}, RefSourceTransactionID},
		"null values":            {map[string]any{"entry_reference": nil, "transaction_id": nil}, RefSourceNone},
	} {
		if got := refSource(tc.raw); got != tc.want {
			t.Errorf("%s: refSource = %q, want %q", name, got, tc.want)
		}
	}
}
