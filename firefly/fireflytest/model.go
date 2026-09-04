package fireflytest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Account mirrors the fields of a Firefly III account that this project touches.
type Account struct {
	ID           string
	Name         string
	Type         string
	Role         string
	CurrencyCode string
	IBAN         string

	OpeningBalance     string
	OpeningBalanceDate string
}

// Split mirrors one journal inside a transaction group.
type Split struct {
	JournalID         string
	Type              string
	Date              string
	Amount            string
	CurrencyCode      string
	Description       string
	SourceID          string
	SourceName        string
	DestinationID     string
	DestinationName   string
	ExternalID        string
	InternalReference string
	Notes             string
	Tags              []string
	Reconciled        bool

	SepaCtID string
	SepaDB   string
	SepaCI   string
}

// Group is a transaction group. Firefly models every transaction as a group of
// one or more splits, which is why a partial update is destructive.
type Group struct {
	ID     string
	Title  string
	Splits []*Split
}

// hash mirrors Firefly's import_hash_v2: a digest over the submitted row. The
// external id is part of it, so two transactions carrying different bank
// references never collide, while two reference-less twins do.
func (s *Split) hash() string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		s.Type, s.Date, s.Amount, s.Description,
		s.SourceID, s.SourceName, s.DestinationID, s.DestinationName,
		s.ExternalID, s.InternalReference, s.Notes,
	}, "\x00")))
	return hex.EncodeToString(h[:])
}

func (s *Split) clone() *Split {
	out := *s
	out.Tags = append([]string(nil), s.Tags...)
	return &out
}

func (a *Account) toJSON(currentBalance string) map[string]any {
	return map[string]any{
		"type": "accounts",
		"id":   a.ID,
		"attributes": map[string]any{
			"name":                 a.Name,
			"type":                 a.Type,
			"account_role":         a.Role,
			"currency_code":        a.CurrencyCode,
			"iban":                 a.IBAN,
			"active":               true,
			"opening_balance":      a.OpeningBalance,
			"opening_balance_date": a.OpeningBalanceDate,
			"current_balance":      currentBalance,
		},
	}
}

func (g *Group) toJSON(s *Server) map[string]any {
	splits := make([]map[string]any, 0, len(g.Splits))
	for _, sp := range g.Splits {
		splits = append(splits, map[string]any{
			"transaction_journal_id": sp.JournalID,
			"type":                   sp.Type,
			"date":                   sp.Date,
			"amount":                 sp.Amount,
			"currency_code":          sp.CurrencyCode,
			"description":            sp.Description,
			"source_id":              sp.SourceID,
			"source_name":            sp.SourceName,
			"source_type":            s.accountType(sp.SourceID),
			"destination_id":         sp.DestinationID,
			"destination_name":       sp.DestinationName,
			"destination_type":       s.accountType(sp.DestinationID),
			"external_id":            sp.ExternalID,
			"internal_reference":     sp.InternalReference,
			"notes":                  sp.Notes,
			"tags":                   append([]string(nil), sp.Tags...),
			"reconciled":             sp.Reconciled,
			"sepa_ct_id":             sp.SepaCtID,
			"sepa_db":                sp.SepaDB,
			"sepa_ci":                sp.SepaCI,
			"import_hash_v2":         sp.hash(),
		})
	}
	return map[string]any{
		"type": "transactions",
		"id":   g.ID,
		"attributes": map[string]any{
			"group_title":  g.Title,
			"transactions": splits,
		},
	}
}

func errorBody(msg string) map[string]any {
	return map[string]any{"message": msg, "errors": map[string]any{}}
}

func duplicateMessage(groupID string) string {
	return fmt.Sprintf("Duplicate of transaction #%s.", groupID)
}
