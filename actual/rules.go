package actual

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"regexp"
	"strings"
	"unicode"
)

// RuleSet holds all active rules loaded from the Actual Budget database.
type RuleSet struct {
	Rules []*Rule
}

// Rule is a single Actual Budget rule consisting of a set of conditions and the
// actions to apply when all (or any) conditions match.
type Rule struct {
	Stage      string
	CondOp     string
	Conditions []Condition
	Actions    []Action
}

// Condition is a single predicate within a Rule.
type Condition struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value"`
	Type  string `json:"type"`

	compiledRegex *regexp.Regexp
}

// Action describes a mutation to apply to a matching transaction.
type Action struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value"`
	Type  string `json:"type"`
}

// LoadRules reads all non-deleted rules from the database and returns them as a RuleSet.
func (d *DB) LoadRules() (*RuleSet, error) {
	rows, err := d.sql.Query(`
		SELECT stage, conditions_op, conditions, actions
		FROM rules
		WHERE tombstone = 0
	`)
	if err != nil {
		return nil, fmt.Errorf("query rules: %w", err)
	}
	defer rows.Close()

	var rs RuleSet
	for rows.Next() {
		var (
			stage, condOp     sql.NullString
			condJSON, actJSON string
		)
		if err := rows.Scan(&stage, &condOp, &condJSON, &actJSON); err != nil {
			log.Printf("Scan rule row: %v", err)
			continue
		}
		var conds []Condition
		var acts []Action
		if err := json.Unmarshal([]byte(condJSON), &conds); err != nil {
			log.Printf("Parse rule conditions: %v", err)
			continue
		}
		for i := range conds {
			if conds[i].Op == "matches" {
				pattern := normalise(toString(conds[i].Value))
				if re, err := regexp.Compile(pattern); err != nil {
					log.Printf("WARN rule %q: 'matches' pattern %q is not valid regex: %v", stage.String, pattern, err)
				} else {
					conds[i].compiledRegex = re
				}
			}
		}
		if err := json.Unmarshal([]byte(actJSON), &acts); err != nil {
			log.Printf("Parse rule actions: %v", err)
			continue
		}
		op := condOp.String
		if op == "" {
			op = "and"
		}
		rs.Rules = append(rs.Rules, &Rule{
			Stage:      stage.String,
			CondOp:     op,
			Conditions: conds,
			Actions:    acts,
		})
	}
	return &rs, rows.Err()
}

// Match returns all rules in the set whose conditions are satisfied by t.
func (rs *RuleSet) Match(t *Transaction) []*Rule {
	var matched []*Rule
	for _, r := range rs.Rules {
		if r.evaluate(t) {
			matched = append(matched, r)
		}
	}
	return matched
}

// Apply executes all actions of the rule against transaction t, writing changes
// to the database and updating the in-memory fields.
func (r *Rule) Apply(d *DB, t *Transaction) {
	for _, a := range r.Actions {
		if err := applyAction(d, t, a); err != nil {
			log.Printf("Rule action error (%s=%v): %v", a.Field, a.Value, err)
		}
	}
}

func (r *Rule) evaluate(t *Transaction) bool {
	results := make([]bool, 0, len(r.Conditions))
	for _, c := range r.Conditions {
		results = append(results, matchCondition(t, c))
	}
	if r.CondOp == "or" {
		for _, v := range results {
			if v {
				return true
			}
		}
		return false
	}

	for _, v := range results {
		if !v {
			return false
		}
	}
	return true
}

func matchCondition(t *Transaction, c Condition) bool {
	fieldVal := getField(t, c.Field)
	switch c.Type {
	case "string", "imported_payee":
		if c.Op == "matches" {
			re := c.compiledRegex
			if re == nil {
				var err error
				re, err = regexp.Compile(normalise(toString(c.Value)))
				if err != nil {
					return false
				}
			}
			return re.MatchString(normalise(fieldVal))
		}
		if list, ok := c.Value.([]any); ok {
			return matchStringList(fieldVal, c.Op, list)
		}
		return matchString(fieldVal, c.Op, toString(c.Value))
	case "id":
		return matchID(fieldVal, c.Op, c.Value)
	case "number":
		return matchNumber(t.AmountCents, c.Op, c.Value)
	case "boolean":
		return matchBool(fieldVal, c.Op, c.Value)
	default:
		return false
	}
}

func getField(t *Transaction, field string) string {
	switch field {
	case "description", "payee":
		return t.PayeeID
	case "payee_name":
		return t.PayeeName
	case "imported_description", "imported_payee":
		return t.ImportedPayee
	case "notes":
		return t.Notes
	case "acct", "account":
		return t.AccountID
	case "category":
		return t.CategoryID
	default:
		return ""
	}
}

func matchString(fieldVal, op, pattern string) bool {
	fv := normalise(fieldVal)
	pv := normalise(pattern)
	switch op {
	case "is":
		return fv == pv
	case "isNot":
		return fv != pv
	case "contains":
		return strings.Contains(fv, pv)
	case "doesNotContain":
		return !strings.Contains(fv, pv)
	case "oneOf":

		return fv == pv
	case "notOneOf":
		return fv != pv
	case "matches":
		re, err := regexp.Compile(pv)
		if err != nil {
			return false
		}
		return re.MatchString(fv)
	}
	return false
}

func matchStringList(fieldVal, op string, list []any) bool {
	fv := normalise(fieldVal)
	for _, item := range list {
		if normalise(toString(item)) == fv {
			return op == "oneOf"
		}
	}
	return op == "notOneOf"
}

func matchID(fieldVal string, op string, value any) bool {
	switch v := value.(type) {
	case string:
		return matchString(fieldVal, op, v)
	case []any:
		return matchStringList(fieldVal, op, v)
	}
	return false
}

func matchNumber(amountCents int64, op string, value any) bool {
	f := toFloat(value)

	targetCents := int64(math.Round(f))
	switch op {
	case "is":
		return amountCents == targetCents
	case "isapprox":
		diff := amountCents - targetCents
		if diff < 0 {
			diff = -diff
		}
		tc := targetCents
		if tc < 0 {
			tc = -tc
		}
		interval := int64(math.Round(float64(tc) * 0.075))
		return diff <= interval
	case "gt":
		return amountCents > targetCents
	case "gte":
		return amountCents >= targetCents
	case "lt":
		return amountCents < targetCents
	case "lte":
		return amountCents <= targetCents
	case "isbetween":
		m, ok := value.(map[string]any)
		if !ok {
			return false
		}
		lo := int64(math.Round(toFloat(m["num1"])))
		hi := int64(math.Round(toFloat(m["num2"])))
		if lo > hi {
			lo, hi = hi, lo
		}
		return amountCents >= lo && amountCents <= hi
	}
	return false
}

func matchBool(fieldVal, op string, value any) bool {
	bv := fieldVal != "" && fieldVal != "0" && fieldVal != "false"
	target := toBool(value)
	switch op {
	case "is":
		return bv == target
	case "isNot":
		return bv != target
	}
	return false
}

func applyAction(d *DB, t *Transaction, a Action) error {
	switch a.Field {
	case "category":
		catID := toString(a.Value)
		if catID == "" {
			return nil
		}
		_, err := d.sql.Exec(
			`UPDATE transactions SET category = ? WHERE id = ?`, catID, t.ID,
		)
		if err != nil {
			return err
		}
		d.track("transactions", t.ID, "category", catID)
		t.CategoryID = catID

	case "description", "payee":
		payeeID := toString(a.Value)
		if payeeID == "" {
			return nil
		}
		_, err := d.sql.Exec(
			`UPDATE transactions SET description = ? WHERE id = ?`, payeeID, t.ID,
		)
		if err != nil {
			return err
		}
		d.track("transactions", t.ID, "description", payeeID)
		t.PayeeID = payeeID
		t.PayeeName = d.resolvePayeeName(payeeID)

	case "notes":
		notes := toString(a.Value)
		if a.Op == "prepend-notes" {
			notes = notes + t.Notes
		} else if a.Op == "append-notes" {
			notes = t.Notes + notes
		}
		notes = strings.TrimSpace(notes)
		_, err := d.sql.Exec(
			`UPDATE transactions SET notes = ? WHERE id = ?`, notes, t.ID,
		)
		if err != nil {
			return err
		}
		d.track("transactions", t.ID, "notes", notes)
		t.Notes = notes

	case "cleared":
		v := btoi(toBool(a.Value))
		_, err := d.sql.Exec(
			`UPDATE transactions SET cleared = ? WHERE id = ?`, v, t.ID,
		)
		if err != nil {
			return err
		}
		d.track("transactions", t.ID, "cleared", v)
		t.Cleared = v != 0
	}
	return nil
}

func normalise(s string) string {

	return strings.Map(func(r rune) rune {
		return unicode.ToLower(r)
	}, s)
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return fmt.Sprintf("%g", x)
	default:
		return fmt.Sprintf("%v", x)
	}
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case string:
		f := 0.0
		_, _ = fmt.Sscanf(x, "%f", &f)
		return f
	}
	return 0
}

func toBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != "" && x != "0" && x != "false"
	}
	return false
}
