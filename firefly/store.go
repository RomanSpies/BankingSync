package firefly

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"bankingsync/budget"
)

const (
	accountTypeAsset = "asset"
	defaultAssetRole = "defaultAsset"
)

// Store implements budget.Store against Firefly III.
//
// Firefly is write-through, so it deliberately implements neither budget.Flusher
// nor budget.RuleRunner: there is nothing to flush, and rules run in Firefly's
// own engine, triggered per request through apply_rules.
type Store struct {
	c   *Client
	cfg Config
}

type Config struct {
	// PendingTag marks transactions the bank has not booked yet. It is a tag
	// rather than the reconciled flag, because reconciled locks the amount and
	// a pending authorisation regularly books at a different value.
	PendingTag string

	// ApplyRules lets Firefly's own rule engine run. It is deliberately off
	// while a transaction is still pending, because a rule may strip the
	// pending tag before the booking ever confirms it.
	ApplyRules bool

	FireWebhooks bool
}

func NewStore(c *Client, cfg Config) *Store {
	if cfg.PendingTag == "" {
		cfg.PendingTag = "pending"
	}
	return &Store{c: c, cfg: cfg}
}

var (
	_ budget.Store                = (*Store)(nil)
	_ budget.Describer            = (*Store)(nil)
	_ budget.OpeningBalanceWriter = (*Store)(nil)
	_ budget.BalanceReader        = (*Store)(nil)
)

func (s *Store) Ping(ctx context.Context) error {
	_, err := s.c.About(ctx)
	return err
}

func (s *Store) Close() {
	// Nothing to release. The Actual backend holds a SQLite handle and a websocket
	// that have to be torn down; Firefly is reached over stateless HTTP through a
	// shared http.Client whose connection pool outlives any single Store. Closing
	// it here would break the next Store built on the same client.
}

func (s *Store) BackendVersion(ctx context.Context) (string, error) {
	return s.c.About(ctx)
}

// GetOrCreateAccount resolves the asset account by IBAN first and by name only
// as a fallback.
//
// The IBAN is what makes renaming safe. Firefly enforces that an asset account's
// IBAN is unique per user (app/Rules/UniqueIban.php: asset and liability IBANs
// may occur zero times elsewhere), so it is a stable identity that survives any
// number of renames in the Firefly UI. The name is not: matching on it alone
// means a rename looks like a missing account, and the next sync silently opens
// a second one beside it.
//
// Both lookups are re-verified here because Firefly's account search is
// `whereLike('accounts.iban', '%query%')` (app/Support/Search/AccountSearch.php)
// for iban and name alike. Taking the first hit would let "Checking" adopt
// "Old Checking" — a wrong but entirely plausible account.
func (s *Store) GetOrCreateAccount(ctx context.Context, spec budget.AccountSpec) (acct *budget.Account, err error) {
	ctx, span := tracer().Start(ctx, "firefly.get_or_create_account",
		trace.WithAttributes(
			attribute.Bool("firefly.has_iban", spec.IBAN != ""),
			attribute.String("firefly.currency", spec.Currency),
		))
	defer func() { endSpan(span, err) }()

	if spec.IBAN != "" {
		found, err := s.searchAccounts(ctx, spec.IBAN, "iban")
		if err != nil {
			return nil, err
		}
		if a := pickAssetByIBAN(found, spec.IBAN); a != nil {
			return s.confirmCurrency(ctx, a, spec)
		}
	}

	found, err := s.searchAccounts(ctx, spec.Name, "name")
	if err != nil {
		return nil, err
	}
	if a := pickAssetByName(found, spec.Name); a != nil {
		if spec.IBAN != "" && a.IBAN == "" {
			s.adoptIBAN(ctx, a, spec.IBAN)
		}
		return s.confirmCurrency(ctx, a, spec)
	}

	// Optional fields are omitted rather than sent empty. Firefly validates
	// currency_code with `min:3` and rejects "" outright, so a bank account whose
	// currency the bank never reported could not be created at all — with the
	// empty string left in, that is a 422 on every sync. Left out, Firefly falls
	// back to the user's default currency, which is the right answer when we do
	// not know better.
	payload := map[string]any{
		"name":         spec.Name,
		"type":         accountTypeAsset,
		"account_role": defaultAssetRole,
	}
	if spec.Currency != "" {
		payload["currency_code"] = spec.Currency
	}
	if spec.IBAN != "" {
		payload["iban"] = spec.IBAN
	}

	body, err := s.c.Post(ctx, "/api/v1/accounts", payload)
	if err != nil {
		return nil, fmt.Errorf("create asset account %q: %w", spec.Name, err)
	}
	raw, err := Unwrap(body)
	if err != nil {
		return nil, err
	}
	created, err := decodeAccount(raw)
	if err != nil {
		return nil, err
	}
	return &budget.Account{ID: created.ID, Name: created.Name, Currency: created.Currency}, nil
}

// confirmCurrency refuses an account whose currency differs from the bank's.
// Firefly silently overrules a submitted currency_code with the account's own,
// so importing anyway would denominate every amount wrongly and never complain.
func (s *Store) confirmCurrency(ctx context.Context, a *account, spec budget.AccountSpec) (*budget.Account, error) {
	if spec.Currency != "" && a.Currency != "" && !strings.EqualFold(spec.Currency, a.Currency) {
		s.c.obs.recordConflict(ctx, ConflictCurrencyMismatch)
		return nil, fmt.Errorf(
			"account %q is denominated in %s but the bank reports %s; Firefly would silently "+
				"convert every amount, so the import stops here",
			a.Name, a.Currency, spec.Currency)
	}
	return &budget.Account{ID: a.ID, Name: a.Name, Currency: a.Currency}, nil
}

// ListTransactions returns single-split transactions in [from, to). Firefly's
// range is inclusive on both ends, so the upper bound is shifted back by a day.
// Groups holding more than one split are skipped: they are user-made splits, and
// adopting one would mean a later update destroys the rest.
func (s *Store) ListTransactions(ctx context.Context, accountID string, from, to time.Time) (out []*budget.Transaction, err error) {
	ctx, span := tracer().Start(ctx, "firefly.list_transactions",
		trace.WithAttributes(
			attribute.String("firefly.account_id", accountID),
			attribute.String("firefly.window_start", from.Format("2006-01-02")),
			attribute.String("firefly.window_end", to.Format("2006-01-02")),
		))
	defer func() {
		span.SetAttributes(attribute.Int("firefly.result_count", len(out)))
		endSpan(span, err)
	}()

	// An empty half-open window has no rows in it, and asking anyway would send
	// Firefly a range it rejects.
	if !to.After(from) {
		return nil, nil
	}

	// Firefly wants start strictly before end and answers 422 otherwise — not a
	// documented rule, an observed one. A one-day half-open window lands exactly
	// on it, because the inclusive end is a day back from the exclusive one. The
	// production window is fifteen days wide so it never gets there, but a caller
	// asking for a single day is asking something reasonable and should not get a
	// validation error for it.
	//
	// So the range is widened by a day and the surplus dropped below. Widening is
	// the only option available: there is no way to express a single day to an API
	// that insists on two.
	endIncl := to.AddDate(0, 0, -1)
	widened := !endIncl.After(from)
	if widened {
		endIncl = from.AddDate(0, 0, 1)
	}

	q := url.Values{
		"start": {from.Format("2006-01-02")},
		"end":   {endIncl.Format("2006-01-02")},
	}
	raws, err := s.c.GetPaged(ctx, "/api/v1/accounts/"+accountID+"/transactions", q)
	if err != nil {
		return nil, err
	}

	out = make([]*budget.Transaction, 0, len(raws))
	for _, raw := range raws {
		g, err := decodeGroup(raw)
		if err != nil {
			return nil, err
		}
		if len(g.Splits) != 1 {
			continue
		}
		// Unreachable by contract — the endpoint above is already scoped to the
		// account — and deliberately kept anyway. toBudget derives the amount's
		// sign from the asset side and errors when it finds neither, which would
		// abort the whole sync for this account over one surprising row. Skipping
		// is the better failure. No test can exercise this; that is the point.
		if !g.touches(accountID) {
			continue
		}
		t, err := g.toBudget(accountID, s.cfg.PendingTag)
		if err != nil {
			return nil, fmt.Errorf("transaction %s: %w", g.ID, err)
		}
		// Only when the range was widened above. ParseDate anchors the stated day
		// at UTC midnight and the callers' bounds are UTC midnights too, so this
		// is a plain calendar-day comparison rather than an instant one.
		if widened && (t.Date.Before(from) || !t.Date.Before(to)) {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// FindByExternalRef looks the reference up through Firefly's search, which is
// global. The result must be filtered against the account here, or a reference
// that two banks happen to share would be adopted across accounts.
func (s *Store) FindByExternalRef(ctx context.Context, accountID, ref string) (found *budget.Transaction, err error) {
	ctx, span := tracer().Start(ctx, "firefly.find_by_external_ref",
		trace.WithAttributes(attribute.String("firefly.account_id", accountID)))
	defer func() {
		span.SetAttributes(attribute.Bool("firefly.hit", found != nil))
		endSpan(span, err)
	}()

	if ref == "" {
		return nil, nil
	}
	q := url.Values{"query": {`external_id_is:"` + escapeQuery(ref) + `"`}}
	raws, err := s.c.GetPaged(ctx, "/api/v1/search/transactions", q)
	if err != nil {
		return nil, err
	}

	for _, raw := range raws {
		g, err := decodeGroup(raw)
		if err != nil {
			return nil, err
		}
		if len(g.Splits) != 1 {
			continue
		}
		// The search is global, so the account has to be checked here. Relying
		// on the amount orientation to fail would make this an accident.
		if !g.touches(accountID) {
			continue
		}
		// The search operator is a DSL, not an API contract. Verify the value
		// rather than trusting a silent fallback to a substring match.
		if g.Splits[0].ExternalID != ref {
			continue
		}
		t, err := g.toBudget(accountID, s.cfg.PendingTag)
		if err != nil {
			return nil, fmt.Errorf("transaction %s: %w", g.ID, err)
		}
		return t, nil
	}
	return nil, nil
}

func (s *Store) Create(ctx context.Context, accountID string, in budget.ImportedFields) (created *budget.Transaction, err error) {
	ctx, span := tracer().Start(ctx, "firefly.create_transaction",
		trace.WithAttributes(
			attribute.String("firefly.account_id", accountID),
			attribute.Bool("firefly.cleared", in.Cleared),
			attribute.Bool("firefly.has_external_ref", in.ExternalRef != ""),
		))
	defer func() { endSpan(span, err) }()

	split, err := s.splitPayload(ctx, accountID, in)
	if err != nil {
		return nil, err
	}

	payload := map[string]any{
		"apply_rules":   s.cfg.ApplyRules && in.Cleared,
		"fire_webhooks": s.cfg.FireWebhooks,
		"transactions":  []map[string]any{split},
	}
	// The duplicate hash covers the whole submitted row. Without an external
	// reference two same-day, same-amount transactions are byte-identical, and
	// the rejection could not be resolved by looking the reference up.
	if in.ExternalRef != "" {
		payload["error_if_duplicate_hash"] = true
	}

	body, err := s.c.Post(ctx, "/api/v1/transactions", payload)
	if err != nil {
		if isDuplicateRejection(err) {
			s.c.obs.recordConflict(ctx, ConflictDuplicate)
		}
		return nil, err
	}
	raw, err := Unwrap(body)
	if err != nil {
		return nil, err
	}
	g, err := decodeGroup(raw)
	if err != nil {
		return nil, err
	}
	if len(g.Splits) != 1 {
		return nil, fmt.Errorf("created group %s came back with %d splits", g.ID, len(g.Splits))
	}
	return g.toBudget(accountID, s.cfg.PendingTag)
}

// isDuplicateRejection recognises the 422 Firefly answers when
// error_if_duplicate_hash catches a row it already holds.
//
// It matches on the message text, which is not an API contract — the same footing
// the external_id_is lookup stands on. The consequence of Firefly rephrasing it is
// that the counter under-reports; nothing else changes, because the error is
// returned either way. A plain check for 422 would be worse: validation failures
// share that status, and a rejected IBAN is not a duplicate.
func isDuplicateRejection(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnprocessableEntity {
		return false
	}
	return strings.Contains(strings.ToLower(apiErr.Body), "duplicate of transaction")
}

// Update rewrites one split. It reads the group first and refuses to touch a
// group holding more than one split: Firefly deletes every journal missing from
// the submitted array, so a partial write would destroy the splits the user
// created.
func (s *Store) Update(ctx context.Context, t *budget.Transaction, p budget.Patch) (err error) {
	ctx, span := tracer().Start(ctx, "firefly.update_transaction",
		trace.WithAttributes(attribute.String("firefly.account_id", t.AccountID)))
	defer func() { endSpan(span, err) }()

	if p.IsEmpty() {
		return nil
	}
	groupID, journalID, err := SplitID(t.ID)
	if err != nil {
		return err
	}

	current, err := s.group(ctx, groupID)
	if err != nil {
		return err
	}
	if len(current.Splits) != 1 {
		s.c.obs.recordConflict(ctx, ConflictUserSplitGroup)
		return fmt.Errorf(
			"transaction group %s now holds %d splits — the user split it, and updating "+
				"would delete their work", groupID, len(current.Splits))
	}
	split := current.Splits[0]
	if split.JournalID != journalID {
		return fmt.Errorf("group %s no longer contains journal %s", groupID, journalID)
	}
	if split.Reconciled {
		return nil
	}

	update := map[string]any{"transaction_journal_id": split.JournalID}
	if p.AmountCents != nil {
		update["amount"] = FormatAmount(*p.AmountCents)
	}
	if p.Notes != nil {
		update["notes"] = *p.Notes
	}
	if p.ExternalRef != nil {
		update["external_id"] = *p.ExternalRef
	}
	if p.ImportedPayee != nil {
		update["internal_reference"] = *p.ImportedPayee
	}
	if p.PayeeName != nil {
		update["description"] = *p.PayeeName
	}
	if p.Cleared != nil && *p.Cleared {
		// Tags replace rather than merge, so the desired set has to be built
		// from what is there minus our marker, or the user loses their tags.
		update["tags"] = withoutTag(split.Tags, s.cfg.PendingTag)
	}

	_, err = s.c.Put(ctx, "/api/v1/transactions/"+groupID, map[string]any{
		"apply_rules":   s.cfg.ApplyRules,
		"fire_webhooks": s.cfg.FireWebhooks,
		"transactions":  []map[string]any{update},
	})
	if err != nil {
		return err
	}
	budget.Apply(t, p)
	return nil
}

// splitPayload builds the split body, including the transfer case: when the
// counterparty IBAN belongs to an asset account the user already holds, this is
// a movement between their own accounts, not a payment to an invented one.
func (s *Store) splitPayload(ctx context.Context, accountID string, in budget.ImportedFields) (map[string]any, error) {
	txType, err := TypeForAmount(in.AmountCents)
	if err != nil {
		return nil, err
	}

	description := strings.TrimSpace(in.PayeeName)
	if description == "" {
		description = "Unknown"
	}

	split := map[string]any{
		"type":        txType,
		"date":        FormatDate(in.Date),
		"amount":      FormatAmount(in.AmountCents),
		"description": description,
	}
	if in.Currency != "" {
		split["currency_code"] = in.Currency
	}
	if in.ExternalRef != "" {
		split["external_id"] = in.ExternalRef
	}
	if in.ImportedPayee != "" {
		split["internal_reference"] = in.ImportedPayee
	}
	if in.Notes != "" {
		split["notes"] = in.Notes
	}
	if !in.Cleared {
		split["tags"] = []string{s.cfg.PendingTag}
	}
	addSEPA(split, in.SEPA)

	counterpartID, err := s.ownAssetByIBAN(ctx, in.CounterpartyIBAN)
	if err != nil {
		return nil, err
	}
	if counterpartID != "" && counterpartID != accountID {
		split["type"] = typeTransfer
		if in.AmountCents < 0 {
			split["source_id"] = accountID
			split["destination_id"] = counterpartID
		} else {
			split["source_id"] = counterpartID
			split["destination_id"] = accountID
		}
		return split, nil
	}

	if in.AmountCents < 0 {
		split["source_id"] = accountID
		split["destination_name"] = description
	} else {
		split["source_name"] = description
		split["destination_id"] = accountID
	}
	return split, nil
}

func (s *Store) ownAssetByIBAN(ctx context.Context, iban string) (string, error) {
	if iban == "" {
		return "", nil
	}
	found, err := s.searchAccounts(ctx, iban, "iban")
	if err != nil {
		return "", err
	}
	if a := pickAssetByIBAN(found, iban); a != nil {
		return a.ID, nil
	}
	return "", nil
}

func (s *Store) group(ctx context.Context, groupID string) (*group, error) {
	body, err := s.c.Get(ctx, "/api/v1/transactions/"+groupID, nil)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && s.isGone(ctx, apiErr) {
			s.c.obs.recordConflict(ctx, ConflictGone)
			return nil, fmt.Errorf("transaction group %s no longer exists: %w", groupID, budget.ErrGone)
		}
		return nil, err
	}
	raw, err := Unwrap(body)
	if err != nil {
		return nil, err
	}
	return decodeGroup(raw)
}

// isGone decides whether a failed read means the transaction is no longer there.
//
// A real Firefly answers a missing transaction with 401 and "Unauthenticated",
// not 404 — verified against 6.6.6 for a deleted group, an id that never existed
// and an id that is not a number. Its route model binding cannot resolve the
// object, so the ownership check fails before anything gets as far as reporting
// that it is absent.
//
// That makes 401 ambiguous: an expired token says exactly the same thing. The
// difference is settled by asking whether the token still works at all, which
// costs one request and only on the error path. Guessing instead would mean
// either reporting "Unauthenticated" for a transaction the user deleted — which
// sends them looking at their credentials — or reporting a dead token as a
// vanished transaction, which is worse.
func (s *Store) isGone(ctx context.Context, apiErr *APIError) bool {
	switch apiErr.Status {
	case http.StatusNotFound:
		return true
	case http.StatusUnauthorized:
		_, err := s.c.Get(ctx, "/api/v1/about", nil)
		return err == nil
	default:
		return false
	}
}

func (s *Store) searchAccounts(ctx context.Context, query, field string) ([]*account, error) {
	if query == "" {
		return nil, nil
	}
	q := url.Values{
		"query": {query},
		"field": {field},
		"type":  {accountTypeAsset},
	}
	raws, err := s.c.GetPaged(ctx, "/api/v1/search/accounts", q)
	if err != nil {
		return nil, err
	}
	out := make([]*account, 0, len(raws))
	for _, raw := range raws {
		a, err := decodeAccount(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// pickAssetByIBAN accepts a search hit only when its IBAN is exactly the one
// asked for. Firefly's search is a substring match, so a hit proves nothing.
func pickAssetByIBAN(accounts []*account, iban string) *account {
	want := NormaliseIBAN(iban)
	for _, a := range accounts {
		if a.Type == accountTypeAsset && NormaliseIBAN(a.IBAN) == want {
			return a
		}
	}
	return nil
}

// pickAssetByName accepts a search hit only on an exact (case-insensitive) name.
func pickAssetByName(accounts []*account, name string) *account {
	want := strings.TrimSpace(name)
	for _, a := range accounts {
		if a.Type == accountTypeAsset && strings.EqualFold(strings.TrimSpace(a.Name), want) {
			return a
		}
	}
	return nil
}

// NormaliseIBAN matches how Firefly stores IBANs: spaces stripped, upper case.
func NormaliseIBAN(s string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
}

// adoptIBAN writes our IBAN onto an account that was matched by name and has
// none of its own, so that the next rename does not orphan it. It never
// overwrites an existing IBAN, and a failure is not fatal: the account is
// usable for this run either way, it just stays rename-fragile.
func (s *Store) adoptIBAN(ctx context.Context, a *account, iban string) {
	if _, err := s.c.Put(ctx, "/api/v1/accounts/"+a.ID, map[string]any{"iban": iban}); err != nil {
		log.Printf("Firefly: could not record IBAN on account %q (%s): %v — "+
			"renaming it in Firefly will make the next sync create a second account",
			a.Name, a.ID, err)
		return
	}
	a.IBAN = iban
}

func withoutTag(tags []string, drop string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if !strings.EqualFold(t, drop) {
			out = append(out, t)
		}
	}
	return out
}

func addSEPA(split map[string]any, refs budget.SEPARefs) {
	if refs.EndToEnd != "" {
		split["sepa_ct_id"] = refs.EndToEnd
	}
	if refs.Mandate != "" {
		split["sepa_db"] = refs.Mandate
	}
	if refs.CreditorID != "" {
		split["sepa_ci"] = refs.CreditorID
	}
}

func escapeQuery(v string) string {
	return strings.ReplaceAll(v, `"`, "")
}

func decodeAccount(raw json.RawMessage) (*account, error) {
	var a struct {
		ID         string `json:"id"`
		Attributes struct {
			Name               string `json:"name"`
			Type               string `json:"type"`
			CurrencyCode       string `json:"currency_code"`
			IBAN               string `json:"iban"`
			OpeningBalance     string `json:"opening_balance"`
			OpeningBalanceDate string `json:"opening_balance_date"`
			CurrentBalance     string `json:"current_balance"`
		} `json:"attributes"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("decode account: %w", err)
	}
	return &account{
		ID: a.ID, Name: a.Attributes.Name, Type: a.Attributes.Type,
		Currency: a.Attributes.CurrencyCode, IBAN: a.Attributes.IBAN,
		OpeningBalance:     a.Attributes.OpeningBalance,
		OpeningBalanceDate: a.Attributes.OpeningBalanceDate,
		CurrentBalance:     a.Attributes.CurrentBalance,
	}, nil
}

// SetOpeningBalance records an opening balance on the asset account.
//
// Firefly models it as an account property rather than a transaction, so this
// is a PUT on the account. It reads the account first and refuses when an
// opening balance is already present: a second PUT would *replace* the value,
// and the caller's contract is that an opening balance is written once and
// never re-adjusted.
func (s *Store) SetOpeningBalance(ctx context.Context, accountID string, ob budget.OpeningBalance) (written bool, err error) {
	ctx, span := tracer().Start(ctx, "firefly.set_opening_balance",
		trace.WithAttributes(attribute.String("firefly.account_id", accountID)))
	defer func() {
		span.SetAttributes(attribute.Bool("firefly.written", written))
		endSpan(span, err)
	}()

	current, err := s.readAccount(ctx, accountID)
	if err != nil {
		return false, err
	}
	if cents, err := ParseSignedAmount(current.OpeningBalance); err == nil && cents != 0 {
		return false, nil
	}

	// Both fields or neither: Firefly validates them with a mutual
	// required_with, so sending one alone is a 422.
	_, err = s.c.Put(ctx, "/api/v1/accounts/"+accountID, map[string]any{
		"opening_balance":      FormatSignedAmount(ob.AmountCents),
		"opening_balance_date": ob.Date.Format("2006-01-02"),
	})
	if err != nil {
		return false, fmt.Errorf("set opening balance on account %s: %w", accountID, err)
	}
	return true, nil
}

// AccountBalance reports the account total Firefly shows, opening balance
// included.
func (s *Store) AccountBalance(ctx context.Context, accountID string) (cents int64, err error) {
	ctx, span := tracer().Start(ctx, "firefly.account_balance",
		trace.WithAttributes(attribute.String("firefly.account_id", accountID)))
	defer func() { endSpan(span, err) }()

	a, err := s.readAccount(ctx, accountID)
	if err != nil {
		return 0, err
	}
	cents, err = ParseSignedAmount(a.CurrentBalance)
	if err != nil {
		return 0, fmt.Errorf("account %s balance %q: %w", accountID, a.CurrentBalance, err)
	}
	return cents, nil
}

func (s *Store) readAccount(ctx context.Context, accountID string) (*account, error) {
	body, err := s.c.Get(ctx, "/api/v1/accounts/"+accountID, nil)
	if err != nil {
		return nil, fmt.Errorf("read account %s: %w", accountID, err)
	}
	raw, err := Unwrap(body)
	if err != nil {
		return nil, err
	}
	return decodeAccount(raw)
}
