package fireflytest

import (
	"fmt"
	"strconv"
	"strings"
)

func splitFromJSON(raw map[string]any) (*Split, error) {
	sp := &Split{
		Type:              str(raw, "type"),
		Date:              str(raw, "date"),
		Amount:            str(raw, "amount"),
		CurrencyCode:      str(raw, "currency_code"),
		Description:       str(raw, "description"),
		SourceID:          str(raw, "source_id"),
		SourceName:        str(raw, "source_name"),
		DestinationID:     str(raw, "destination_id"),
		DestinationName:   str(raw, "destination_name"),
		ExternalID:        str(raw, "external_id"),
		InternalReference: str(raw, "internal_reference"),
		Notes:             str(raw, "notes"),
		SepaCtID:          str(raw, "sepa_ct_id"),
		SepaDB:            str(raw, "sepa_db"),
		SepaCI:            str(raw, "sepa_ci"),
	}
	if v, ok := raw["reconciled"].(bool); ok {
		sp.Reconciled = v
	}
	if tags, ok := raw["tags"]; ok {
		sp.Tags = toStrings(tags)
	}
	return sp, validate(sp)
}

// applyUpdate mirrors Firefly's partial update: only the keys present in the
// payload are touched. Tags are the exception — supplying the key replaces the
// whole set, and omitting it leaves the existing tags alone.
func applyUpdate(sp *Split, raw map[string]any) error {
	if _, ok := raw["type"]; ok {
		sp.Type = str(raw, "type")
	}
	if _, ok := raw["date"]; ok {
		sp.Date = str(raw, "date")
	}
	if _, ok := raw["amount"]; ok {
		if sp.Reconciled {
			return fmt.Errorf("the amount of a reconciled transaction cannot be edited")
		}
		sp.Amount = str(raw, "amount")
	}
	if _, ok := raw["description"]; ok {
		sp.Description = str(raw, "description")
	}
	if _, ok := raw["notes"]; ok {
		sp.Notes = str(raw, "notes")
	}
	if _, ok := raw["external_id"]; ok {
		sp.ExternalID = str(raw, "external_id")
	}
	if _, ok := raw["internal_reference"]; ok {
		sp.InternalReference = str(raw, "internal_reference")
	}
	if _, ok := raw["source_id"]; ok {
		sp.SourceID = str(raw, "source_id")
	}
	if _, ok := raw["source_name"]; ok {
		sp.SourceName = str(raw, "source_name")
		sp.SourceID = ""
	}
	if _, ok := raw["destination_id"]; ok {
		sp.DestinationID = str(raw, "destination_id")
	}
	if _, ok := raw["destination_name"]; ok {
		sp.DestinationName = str(raw, "destination_name")
		sp.DestinationID = ""
	}
	if v, ok := raw["reconciled"].(bool); ok {
		sp.Reconciled = v
	}
	if tags, ok := raw["tags"]; ok {
		sp.Tags = toStrings(tags)
	}
	return validate(sp)
}

func validate(sp *Split) error {
	if sp.Type == "" {
		return fmt.Errorf("type is required")
	}
	if sp.Date == "" {
		return fmt.Errorf("date is required")
	}
	if strings.TrimSpace(sp.Description) == "" {
		return fmt.Errorf("description is required")
	}
	amount, err := strconv.ParseFloat(strings.TrimSpace(sp.Amount), 64)
	if err != nil {
		return fmt.Errorf("amount %q is not a number", sp.Amount)
	}
	if amount <= 0 {
		return fmt.Errorf("amount must be greater than zero")
	}
	return nil
}

func str(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func toStrings(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
