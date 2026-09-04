package fireflytest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	wantType := r.URL.Query().Get("type")

	s.mu.Lock()
	var matching []*Account
	for _, a := range s.accounts {
		if wantType != "" && wantType != "all" && a.Type != wantType {
			continue
		}
		matching = append(matching, a)
	}
	current, limit := page(r, s.perPage)
	if limit > s.perPage {
		limit = s.perPage
	}
	slice, meta := paginate(matching, current, limit)
	data := make([]map[string]any, 0, len(slice))
	for _, a := range slice {
		data = append(data, a.toJSON(s.balanceOfLocked(a)))
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"data": data,
		"meta": map[string]any{"pagination": meta},
	})
}

// handleSearchAccounts implements the field filter Firefly offers on account
// search, which is what makes IBAN-based matching possible.
func (s *Server) handleSearchAccounts(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("query"))
	field := r.URL.Query().Get("field")
	wantType := r.URL.Query().Get("type")

	s.mu.Lock()
	var matching []*Account
	for _, a := range s.accounts {
		if wantType != "" && wantType != "all" && a.Type != wantType {
			continue
		}
		// Firefly's account search is whereLike('%query%') for iban and name,
		// and an exact comparison for id and number
		// (app/Support/Search/AccountSearch.php). A fixture that matched exactly
		// everywhere would hide every client that trusts a hit without checking it.
		var hay string
		exact := false
		switch field {
		case "iban":
			hay = a.IBAN
		case "number":
			hay, exact = "", true
		case "id":
			hay, exact = a.ID, true
		default:
			hay = a.Name
		}
		if hay == "" {
			continue
		}
		hit := strings.Contains(strings.ToLower(hay), strings.ToLower(q))
		if exact {
			hit = hay == q
		}
		if hit {
			matching = append(matching, a)
		}
	}
	current, limit := page(r, s.perPage)
	if limit > s.perPage {
		limit = s.perPage
	}
	slice, meta := paginate(matching, current, limit)
	data := make([]map[string]any, 0, len(slice))
	for _, a := range slice {
		data = append(data, a.toJSON(s.balanceOfLocked(a)))
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"data": data,
		"meta": map[string]any{"pagination": meta},
	})
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	if fail, _ := s.consumeWriteBudget(); fail {
		writeJSON(w, http.StatusInternalServerError, errorBody("injected failure"))
		return
	}

	var body struct {
		Name               string `json:"name"`
		Type               string `json:"type"`
		Role               string `json:"account_role"`
		CurrencyCode       string `json:"currency_code"`
		IBAN               string `json:"iban"`
		OpeningBalance     string `json:"opening_balance"`
		OpeningBalanceDate string `json:"opening_balance_date"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("bad request"))
		return
	}
	if body.Name == "" || body.Type == "" {
		writeJSON(w, http.StatusUnprocessableEntity, errorBody("name and type are required"))
		return
	}
	if msg := validateOpeningBalance(body.OpeningBalance, body.OpeningBalanceDate); msg != "" {
		writeJSON(w, http.StatusUnprocessableEntity, errorBody(msg))
		return
	}

	s.mu.Lock()
	for _, a := range s.accounts {
		if a.Type == body.Type && strings.EqualFold(a.Name, body.Name) {
			s.mu.Unlock()
			writeJSON(w, http.StatusUnprocessableEntity,
				errorBody("This account name is already in use."))
			return
		}
	}
	a := s.addAccountLocked(body.Name, body.Type, body.CurrencyCode, body.IBAN)
	if body.Role != "" {
		a.Role = body.Role
	}
	out := a.toJSON(s.balanceOfLocked(a))
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// handleAccountTransactions honours start and end inclusively on both ends,
// which is where Firefly differs from the half-open window the matcher uses.
func (s *Server) handleAccountTransactions(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	accountID := parts[len(parts)-2]

	from, okFrom := parseDayParam(r.URL.Query().Get("start"))
	to, okTo := parseDayParam(r.URL.Query().Get("end"))

	// Firefly wants start strictly before end and answers 422 otherwise. It is an
	// observed rule rather than a documented one — verified against 6.6.6 — and it
	// is the reason a one-day half-open window has to be widened before it is
	// asked for. Without it here, the fixture would happily answer a range the
	// real thing refuses, and the widening would look unnecessary.
	if okFrom && okTo && !to.After(from) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"message": "The start must be a date before end. (and 1 more error)",
			"errors": map[string]any{
				"start": []string{"The start must be a date before end."},
				"end":   []string{"The end must be a date after start."},
			},
		})
		return
	}

	s.mu.Lock()
	var matching []*Group
	for _, g := range s.groups {
		if !groupTouchesAccount(g, accountID) {
			continue
		}
		day, err := groupDay(g)
		if err == nil {
			if okFrom && day.Before(from) {
				continue
			}
			if okTo && day.After(to) {
				continue
			}
		}
		matching = append(matching, g)
	}
	current, limit := page(r, s.perPage)
	if limit > s.perPage {
		limit = s.perPage
	}
	slice, meta := paginate(matching, current, limit)
	data := make([]map[string]any, 0, len(slice))
	for _, g := range slice {
		data = append(data, g.toJSON(s))
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"data": data,
		"meta": map[string]any{"pagination": meta},
	})
}

// handleSearchTransactions implements external_id_is as an exact match and
// deliberately offers no account scoping, because the real endpoint has none.
// A client that forgets to filter by account will pass here and fail in
// production, so the fixture must not paper over it.
func (s *Server) handleSearchTransactions(w http.ResponseWriter, r *http.Request) {
	wantRef, hasRef := externalIDIs(r.URL.Query().Get("query"))

	s.mu.Lock()
	var matching []*Group
	if hasRef {
		for _, g := range s.groups {
			for _, sp := range g.Splits {
				hit := sp.ExternalID == wantRef
				if s.searchContains {
					hit = sp.ExternalID != "" && strings.Contains(sp.ExternalID, wantRef)
				}
				if hit {
					matching = append(matching, g)
					break
				}
			}
		}
	}
	current, limit := page(r, s.perPage)
	if limit > s.perPage {
		limit = s.perPage
	}
	slice, meta := paginate(matching, current, limit)
	data := make([]map[string]any, 0, len(slice))
	for _, g := range slice {
		data = append(data, g.toJSON(s))
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"data": data,
		"meta": map[string]any{"pagination": meta},
	})
}

func (s *Server) handleGetTransaction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	groupID := parts[len(parts)-1]

	s.mu.Lock()
	g := s.groupLocked(groupID)
	if g == nil {
		s.mu.Unlock()
		writeMissingTransaction(w)
		return
	}
	out := g.toJSON(s)
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) handleCreateTransaction(w http.ResponseWriter, r *http.Request) {
	fail, drop := s.consumeWriteBudget()
	if fail {
		writeJSON(w, http.StatusInternalServerError, errorBody("injected failure"))
		return
	}

	var body struct {
		ErrorIfDuplicateHash bool             `json:"error_if_duplicate_hash"`
		ApplyRules           bool             `json:"apply_rules"`
		FireWebhooks         bool             `json:"fire_webhooks"`
		GroupTitle           string           `json:"group_title"`
		Transactions         []map[string]any `json:"transactions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("bad request"))
		return
	}
	if len(body.Transactions) == 0 {
		writeJSON(w, http.StatusUnprocessableEntity, errorBody("transactions is required"))
		return
	}

	var splits []*Split
	for _, raw := range body.Transactions {
		sp, err := splitFromJSON(raw)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, errorBody(err.Error()))
			return
		}
		splits = append(splits, sp)
	}

	s.mu.Lock()
	for _, sp := range splits {
		s.resolveSidesLocked(sp)
		sp.Date = s.storeDateLocked(sp.Date)
	}
	if body.ErrorIfDuplicateHash {
		for _, sp := range splits {
			if g := s.findByHashLocked(sp.hash()); g != nil {
				s.mu.Unlock()
				writeJSON(w, http.StatusUnprocessableEntity, errorBody(duplicateMessage(g.ID)))
				return
			}
		}
	}
	g := s.addGroupLocked(splits)
	g.Title = body.GroupTitle
	out := g.toJSON(s)
	s.mu.Unlock()

	if drop {
		hijack(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// handleUpdateTransaction reproduces Firefly's destructive update: every journal
// missing from the submitted array is deleted.
func (s *Server) handleUpdateTransaction(w http.ResponseWriter, r *http.Request) {
	fail, drop := s.consumeWriteBudget()
	if fail {
		writeJSON(w, http.StatusInternalServerError, errorBody("injected failure"))
		return
	}

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	groupID := parts[len(parts)-1]

	var body struct {
		GroupTitle   string           `json:"group_title"`
		Transactions []map[string]any `json:"transactions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("bad request"))
		return
	}

	s.mu.Lock()
	g := s.groupLocked(groupID)
	if g == nil {
		s.mu.Unlock()
		writeMissingTransaction(w)
		return
	}

	var kept []*Split
	for _, raw := range body.Transactions {
		journalID, _ := raw["transaction_journal_id"].(string)
		target := findSplit(g, journalID)
		if target == nil {
			s.mu.Unlock()
			writeJSON(w, http.StatusUnprocessableEntity, errorBody("unknown transaction_journal_id"))
			return
		}
		if err := applyUpdate(target, raw); err != nil {
			s.mu.Unlock()
			writeJSON(w, http.StatusUnprocessableEntity, errorBody(err.Error()))
			return
		}
		s.resolveSidesLocked(target)
		target.Date = s.storeDateLocked(target.Date)
		kept = append(kept, target)
	}

	if len(kept) > 0 {
		g.Splits = kept
	}
	if body.GroupTitle != "" {
		g.Title = body.GroupTitle
	}
	out := g.toJSON(s)
	s.mu.Unlock()

	if drop {
		hijack(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// resolveSidesLocked auto-creates the expense or revenue account a name refers
// to, exactly as Firefly does, which is the source of account pollution.
func (s *Server) resolveSidesLocked(sp *Split) {
	if sp.DestinationID == "" && sp.DestinationName != "" {
		sp.DestinationID = s.ensureNamedAccountLocked(sp.DestinationName, counterpartyType(sp.Type, false))
	}
	if sp.SourceID == "" && sp.SourceName != "" {
		sp.SourceID = s.ensureNamedAccountLocked(sp.SourceName, counterpartyType(sp.Type, true))
	}
	sp.SourceName = s.accountName(sp.SourceID, sp.SourceName)
	sp.DestinationName = s.accountName(sp.DestinationID, sp.DestinationName)

	// The account's currency overrules whatever the client submitted. The asset
	// side is asked first; the counterparty only decides when the source has no
	// currency of its own.
	if cur := s.currencyOfFirstLocked(sp.SourceID, sp.DestinationID); cur != "" {
		sp.CurrencyCode = cur
	}
}

func (s *Server) currencyOfFirstLocked(ids ...string) string {
	for _, id := range ids {
		if cur := s.accountCurrency(id); cur != "" {
			return cur
		}
	}
	return ""
}

func (s *Server) ensureNamedAccountLocked(name, accountType string) string {
	for _, a := range s.accounts {
		if a.Type == accountType && strings.EqualFold(a.Name, name) {
			return a.ID
		}
	}
	return s.addAccountLocked(name, accountType, "", "").ID
}

func (s *Server) accountName(id, fallback string) string {
	for _, a := range s.accounts {
		if a.ID == id {
			return a.Name
		}
	}
	return fallback
}

func (s *Server) accountCurrency(id string) string {
	for _, a := range s.accounts {
		if a.ID == id && a.Type == "asset" {
			return a.CurrencyCode
		}
	}
	return ""
}

func (s *Server) findByHashLocked(h string) *Group {
	for _, g := range s.groups {
		for _, sp := range g.Splits {
			if sp.hash() == h {
				return g
			}
		}
	}
	return nil
}

func counterpartyType(txType string, isSource bool) string {
	switch txType {
	case "deposit":
		if isSource {
			return "revenue"
		}
		return "asset"
	default:
		if isSource {
			return "asset"
		}
		return "expense"
	}
}

func hijack(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	conn, _, err := hj.Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func groupTouchesAccount(g *Group, accountID string) bool {
	for _, sp := range g.Splits {
		if sp.SourceID == accountID || sp.DestinationID == accountID {
			return true
		}
	}
	return false
}

func groupDay(g *Group) (time.Time, error) {
	return parseAnyDay(g.Splits[0].Date)
}

func parseDayParam(v string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	d, err := parseAnyDay(v)
	if err != nil {
		return time.Time{}, false
	}
	return d, true
}

func parseAnyDay(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC), nil
	}
	return time.Parse("2006-01-02", v)
}

func externalIDIs(query string) (string, bool) {
	for _, token := range splitQuery(query) {
		op, val, found := strings.Cut(token, ":")
		if !found || op != "external_id_is" {
			continue
		}
		return strings.Trim(val, `"`), true
	}
	return "", false
}

func splitQuery(q string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range q {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ' ' && !inQuote:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func findSplit(g *Group, journalID string) *Split {
	for _, sp := range g.Splits {
		if sp.JournalID == journalID {
			return sp
		}
	}
	return nil
}

// handleUpdateAccount applies a partial account update. It enforces Firefly's
// UniqueIban rule: an asset account's IBAN may not occur on any other asset or
// liability account of the same user (app/Rules/UniqueIban.php).
func (s *Server) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/accounts/"), "/")

	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, errorBody("bad body"))
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var target *Account
	for _, a := range s.accounts {
		if a.ID == id {
			target = a
		}
	}
	if target == nil {
		writeJSON(w, http.StatusNotFound, errorBody("Account not found"))
		return
	}

	if iban, ok := payload["iban"].(string); ok && iban != "" {
		for _, a := range s.accounts {
			if a.ID != target.ID && a.IBAN == iban && (a.Type == "asset" || a.Type == "liabilities") {
				writeJSON(w, http.StatusUnprocessableEntity,
					errorBody("It looks like this IBAN is already in use."))
				return
			}
		}
		target.IBAN = iban
	}
	if name, ok := payload["name"].(string); ok && name != "" {
		target.Name = name
	}

	ob, hasOB := payload["opening_balance"].(string)
	obDate, hasOBDate := payload["opening_balance_date"].(string)
	if hasOB || hasOBDate {
		if msg := validateOpeningBalance(ob, obDate); msg != "" {
			writeJSON(w, http.StatusUnprocessableEntity, errorBody(msg))
			return
		}
		target.OpeningBalance = ob
		target.OpeningBalanceDate = obDate
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": target.toJSON(s.balanceOfLocked(target))})
}

// balanceOfLocked totals an account the way Firefly reports current_balance:
// the opening balance plus every split that touches the account, signed by the
// side the account sits on. Callers hold s.mu.
func (s *Server) balanceOfLocked(a *Account) string {
	cents := int64(0)
	if a.OpeningBalance != "" {
		if v, err := decimalCents(a.OpeningBalance); err == nil {
			cents += v
		}
	}
	for _, g := range s.groups {
		for _, sp := range g.Splits {
			v, err := decimalCents(sp.Amount)
			if err != nil {
				continue
			}
			switch a.ID {
			case sp.SourceID:
				cents -= v
			case sp.DestinationID:
				cents += v
			}
		}
	}
	return centsToDecimal(cents)
}

func decimalCents(s string) (int64, error) {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
	intPart, frac, _ := strings.Cut(s, ".")
	for len(frac) < 2 {
		frac += "0"
	}
	v, err := strconv.ParseInt(intPart+frac[:2], 10, 64)
	if err != nil {
		return 0, err
	}
	if neg {
		v = -v
	}
	return v, nil
}

func centsToDecimal(c int64) string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%s%d.%02d", sign, c/100, c%100)
}

// validateOpeningBalance reproduces Firefly's bidirectional rule:
//
//	'opening_balance'      => ['numeric', 'required_with:opening_balance_date'],
//	'opening_balance_date' => ['date',    'required_with:opening_balance'],
//
// Sending either field alone is a 422. A fixture that accepted one would let a
// client that forgets the other look correct here and fail against a real
// instance.
func validateOpeningBalance(amount, date string) string {
	if amount == "" && date == "" {
		return ""
	}
	if amount == "" {
		return "The opening balance field is required when opening balance date is present."
	}
	if date == "" {
		return "The opening balance date field is required when opening balance is present."
	}
	if _, err := decimalCents(amount); err != nil {
		return "The opening balance must be a number."
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return "The opening balance date is not a valid date."
	}
	return ""
}

func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/accounts/"), "/")

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.ID == id {
			writeJSON(w, http.StatusOK, map[string]any{"data": a.toJSON(s.balanceOfLocked(a))})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, errorBody("Account not found"))
}
