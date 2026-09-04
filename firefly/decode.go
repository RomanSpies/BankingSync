package firefly

import (
	"encoding/json"
	"fmt"
	"strings"

	"bankingsync/budget"
)

type account struct {
	ID                 string
	Name               string
	Type               string
	Currency           string
	IBAN               string
	OpeningBalance     string
	OpeningBalanceDate string
	CurrentBalance     string
}

type split struct {
	JournalID         string   `json:"transaction_journal_id"`
	Type              string   `json:"type"`
	Date              string   `json:"date"`
	Amount            string   `json:"amount"`
	CurrencyCode      string   `json:"currency_code"`
	Description       string   `json:"description"`
	SourceID          string   `json:"source_id"`
	SourceName        string   `json:"source_name"`
	DestinationID     string   `json:"destination_id"`
	DestinationName   string   `json:"destination_name"`
	ExternalID        string   `json:"external_id"`
	InternalReference string   `json:"internal_reference"`
	Notes             string   `json:"notes"`
	Tags              []string `json:"tags"`
	Reconciled        bool     `json:"reconciled"`
}

type group struct {
	ID     string
	Title  string
	Splits []split
}

func decodeGroup(raw json.RawMessage) (*group, error) {
	var g struct {
		ID         string `json:"id"`
		Attributes struct {
			GroupTitle   string  `json:"group_title"`
			Transactions []split `json:"transactions"`
		} `json:"attributes"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, fmt.Errorf("decode transaction group: %w", err)
	}
	return &group{ID: g.ID, Title: g.Attributes.GroupTitle, Splits: g.Attributes.Transactions}, nil
}

// touches reports whether the group has the given account on either side.
func (g *group) touches(accountID string) bool {
	for _, sp := range g.Splits {
		if sp.SourceID == accountID || sp.DestinationID == accountID {
			return true
		}
	}
	return false
}

// toBudget converts a single-split group into the neutral model, oriented on the
// asset account the caller is syncing.
func (g *group) toBudget(assetAccountID, pendingTag string) (*budget.Transaction, error) {
	if len(g.Splits) != 1 {
		return nil, fmt.Errorf("group %s holds %d splits", g.ID, len(g.Splits))
	}
	sp := g.Splits[0]

	id, err := EncodeID(g.ID, sp.JournalID)
	if err != nil {
		return nil, err
	}
	cents, err := SignedCents(sp.Amount, sp.SourceID, sp.DestinationID, assetAccountID)
	if err != nil {
		return nil, err
	}
	date, err := ParseDate(sp.Date)
	if err != nil {
		return nil, err
	}

	return &budget.Transaction{
		ID:            id,
		AccountID:     assetAccountID,
		Date:          date,
		AmountCents:   cents,
		Currency:      sp.CurrencyCode,
		PayeeName:     sp.Description,
		Notes:         sp.Notes,
		ExternalRef:   sp.ExternalID,
		ImportedPayee: sp.InternalReference,
		Cleared:       !hasTag(sp.Tags, pendingTag),
		Reconciled:    sp.Reconciled,
		Tags:          append([]string(nil), sp.Tags...),
	}, nil
}

// hasTag reports whether the pending marker is present. Firefly has no cleared
// column, so the absence of the tag is what "the bank has booked it" means here.
func hasTag(tags []string, name string) bool {
	if name == "" {
		return false
	}
	for _, t := range tags {
		if strings.EqualFold(t, name) {
			return true
		}
	}
	return false
}
