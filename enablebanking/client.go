package enablebanking

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"bankingsync/logs"
	"bankingsync/tracing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

const apiBase = "https://api.enablebanking.com"

const (
	maxPages      = 100
	maxRateRetry  = 3
	rateRetryBase = 2 * time.Second
	maxRetryAfter = 60 * time.Second
)

// Transaction is a normalised transaction record returned from the Enable Banking API.
type Transaction struct {
	Status string
	Date   time.Time

	AmountCents int64
	Currency    string
	Payee       string
	Notes       string
	EntryRef    string

	CounterpartyIBAN string
	SEPA             SEPARefs
}

// Client is an Enable Banking API client that fetches transactions using JWT
// authentication signed with an RSA private key.
type Client struct {
	appID    AppIDResolver
	pemSrc   PEMSource
	ownNames map[string]struct{}
	http     *http.Client
	baseURL  string
}

type Option func(*Client)

func WithBaseURL(baseURL string) Option {
	return func(c *Client) { c.baseURL = baseURL }
}

// NewClient returns a Client using the provided resolvers for App ID and PEM key.
// ownNames is a lowercase set of account-holder names used to filter out
// self-transfer payees on credit transactions.
func NewClient(appID AppIDResolver, pemSrc PEMSource, ownNames map[string]struct{}, opts ...Option) *Client {
	c := &Client{
		appID:    appID,
		pemSrc:   pemSrc,
		ownNames: ownNames,
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: tracing.NewTransport(nil, "bankingsync/enablebanking"),
		},
		baseURL: apiBase,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// FetchTransactions retrieves all transactions for accountUID from dateFrom
// through today, following continuation keys to page through the full result set.
func (c *Client) FetchTransactions(ctx context.Context, accountUID string, dateFrom time.Time) ([]Transaction, int, error) {
	headers, err := c.makeHeaders()
	if err != nil {
		return nil, 0, fmt.Errorf("makeHeaders: %w", err)
	}

	dateParams := fmt.Sprintf("date_from=%s&date_to=%s",
		dateFrom.Format("2006-01-02"),
		time.Now().UTC().Format("2006-01-02"),
	)

	var txns []Transaction
	dropped := 0
	txnURL := fmt.Sprintf("%s/accounts/%s/transactions", c.baseURL, accountUID)
	reqURL := txnURL + "?" + dateParams
	seenKeys := make(map[string]struct{})

	for page := 1; reqURL != ""; page++ {
		if page > maxPages {
			return nil, dropped, fmt.Errorf(
				"pagination exceeded %d pages for account %s — aborting to avoid an unbounded loop",
				maxPages, accountUID)
		}
		if err := ctx.Err(); err != nil {
			return nil, dropped, fmt.Errorf("fetch cancelled after %d pages: %w", page-1, err)
		}

		raw, ck, err := c.fetchPage(ctx, reqURL, headers)
		if err != nil {
			return nil, dropped, err
		}
		for _, r := range raw {
			t, err := c.parseTransaction(r)
			if err != nil {
				dropped++
				log.Printf("Skipping malformed transaction: %v | %v", err, r)
				olog.Warn(ctx, "transaction.parse.failed",
					logs.String("account_uid", accountUID),
					logs.String("error", err.Error()),
					logs.String("fields", strings.Join(sortedKeys(r), ",")),
				)
				continue
			}
			txns = append(txns, t)
		}
		if ck == "" {
			reqURL = ""
			continue
		}
		if _, repeated := seenKeys[ck]; repeated {
			return nil, dropped, fmt.Errorf(
				"repeated continuation key from Enable Banking for account %s — aborting to avoid an unbounded loop",
				accountUID)
		}
		seenKeys[ck] = struct{}{}
		reqURL = txnURL + "?" + dateParams + "&continuation_key=" + url.QueryEscape(ck)
	}

	log.Printf("Fetched %d transactions from Enable Banking (%d dropped)", len(txns), dropped)
	if dropped > 0 {
		olog.Error(ctx, "transactions.dropped",
			logs.String("account_uid", accountUID),
			logs.Int("dropped", dropped),
			logs.Int("parsed", len(txns)),
		)
	}
	return txns, dropped, nil
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (c *Client) fetchPage(ctx context.Context, url string, headers map[string]string) ([]map[string]any, string, error) {
	tracer := otel.Tracer("bankingsync/enablebanking")
	_, span := tracer.Start(ctx, "enablebanking.fetch_page")
	defer span.End()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	body, status, err := c.doWithRateLimitRetry(ctx, req, headers)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, "", err
	}
	if status != http.StatusOK {
		err := fmt.Errorf("unexpected HTTP %d from Enable Banking: %s", status, body)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, "", err
	}

	captureResponse("page", "response", body)

	var data struct {
		Transactions    []map[string]any `json:"transactions"`
		ContinuationKey string           `json:"continuation_key"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, "", fmt.Errorf("decode response: %w", err)
	}
	span.SetAttributes(
		attribute.Int("txn_count", len(data.Transactions)),
		attribute.Bool("has_more", data.ContinuationKey != ""),
	)
	return data.Transactions, data.ContinuationKey, nil
}

func (c *Client) makeHeaders() (map[string]string, error) {
	appID, err := c.appID()
	if err != nil {
		return nil, fmt.Errorf("resolve app ID: %w", err)
	}
	keyData, err := c.pemSrc()
	if err != nil {
		return nil, fmt.Errorf("resolve PEM: %w", err)
	}
	block, _ := pem.Decode(keyData)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in private.pem")
	}
	var rsaKey *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		rsaKey, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, e := x509.ParsePKCS8PrivateKey(block.Bytes)
		if e != nil {
			return nil, fmt.Errorf("parse PKCS8 key: %w", e)
		}
		var ok bool
		rsaKey, ok = key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private key is not RSA")
		}
	default:
		return nil, fmt.Errorf("unsupported PEM type: %s", block.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	now := time.Now().Unix()
	claims := jwt.MapClaims{
		"iss": "enablebanking.com",
		"aud": "api.enablebanking.com",
		"iat": now,
		"exp": now + 3600,
		"jti": uuid.New().String(),
		"sub": appID,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = appID
	signed, err := token.SignedString(rsaKey)
	if err != nil {
		return nil, fmt.Errorf("sign JWT: %w", err)
	}
	return map[string]string{
		"Authorization": "Bearer " + signed,
		"Content-Type":  "application/json",
	}, nil
}

func (c *Client) parseTransaction(t map[string]any) (Transaction, error) {
	date, err := parseDate(t)
	if err != nil {
		return Transaction{}, fmt.Errorf("parseDate: %w", err)
	}
	amountCents, err := parseAmountCents(t)
	if err != nil {
		return Transaction{}, fmt.Errorf("parseAmount: %w", err)
	}
	payee := c.parsePayee(t)
	notes, sepa := parseNotesAndSEPA(t)
	ref := getEntryRef(t)
	status, _ := t["status"].(string)
	if status == "" {
		status = "BOOK"
	}
	return Transaction{
		Status:           status,
		Date:             date,
		AmountCents:      amountCents,
		Currency:         parseCurrency(t),
		Payee:            payee,
		Notes:            notes,
		EntryRef:         ref,
		CounterpartyIBAN: parseCounterpartyIBAN(t),
		SEPA:             sepa,
	}, nil
}

func parseCurrency(t map[string]any) string {
	amtMap, _ := t["transaction_amount"].(map[string]any)
	if amtMap == nil {
		return ""
	}
	cur, _ := amtMap["currency"].(string)
	return strings.ToUpper(strings.TrimSpace(cur))
}

func parseCounterpartyIBAN(t map[string]any) string {
	field := "debtor_account"
	if transactionIsDebit(t) {
		field = "creditor_account"
	}
	acct, _ := t[field].(map[string]any)
	if acct == nil {
		return ""
	}
	iban, _ := acct["iban"].(string)
	return normaliseIBAN(iban)
}

func normaliseIBAN(s string) string {
	return strings.ToUpper(strings.ReplaceAll(s, " ", ""))
}

func parseDate(t map[string]any) (time.Time, error) {
	for _, field := range []string{"transaction_date", "booking_date", "value_date"} {
		raw, _ := t[field].(string)
		if raw == "" {
			continue
		}
		if len(raw) > 10 {
			raw = raw[:10]
		}
		d, err := time.Parse("2006-01-02", raw)
		if err == nil {
			return d, nil
		}
	}
	return time.Time{}, fmt.Errorf("no date field found")
}

func parseAmountCents(t map[string]any) (int64, error) {
	amtMap, _ := t["transaction_amount"].(map[string]any)
	rawAmt := ""
	if amtMap != nil {
		rawAmt, _ = amtMap["amount"].(string)
		if rawAmt == "" {
			if v, ok := amtMap["amount"].(float64); ok {
				rawAmt = strconv.FormatFloat(v, 'f', -1, 64)
			}
		}
	}
	if rawAmt == "" {
		return 0, fmt.Errorf("no transaction_amount.amount field")
	}
	f, err := strconv.ParseFloat(rawAmt, 64)
	if err != nil {
		return 0, fmt.Errorf("parse amount %q: %w", rawAmt, err)
	}
	cents := int64(math.Round(f * 100))

	switch indicString(t) {
	case "DBIT":
		if cents > 0 {
			cents = -cents
		}
	case "CRDT":
		if cents < 0 {
			cents = -cents
		}
	default:
		if cents == 0 {
			return 0, fmt.Errorf("no credit_debit_indicator and a zero amount: direction is undeterminable")
		}
	}
	return cents, nil
}

func transactionIsDebit(t map[string]any) bool {
	switch indicString(t) {
	case "DBIT":
		return true
	case "CRDT":
		return false
	}
	cents, err := parseAmountCents(t)
	return err == nil && cents < 0
}

func (c *Client) parsePayee(t map[string]any) string {
	var name string
	if transactionIsDebit(t) {

		if cred, ok := t["creditor"].(map[string]any); ok {
			name, _ = cred["name"].(string)
		}
		if name == "" {
			name, _ = t["creditor_name"].(string)
		}

		if name == "" {
			name = firstRemittanceLine(t)
		}
	} else {

		if deb, ok := t["debtor"].(map[string]any); ok {
			name, _ = deb["name"].(string)
		}
		if name == "" {
			name, _ = t["debtor_name"].(string)
		}

		if name == "" || c.isOwnName(name) {
			name = firstRemittanceLine(t)
		}
	}
	if name == "" {
		return "Unknown"
	}
	return name
}

func parseNotes(t map[string]any) string {
	notes, _ := parseNotesAndSEPA(t)
	return notes
}

func parseNotesAndSEPA(t map[string]any) (string, SEPARefs) {
	if ref, ok := t["remittance_information_unstructured"].(string); ok && ref != "" {
		return parseSEPA(ref)
	}
	return joinRemittanceAndSEPA(t)
}

func getEntryRef(t map[string]any) string {
	if v, ok := t["entry_reference"].(string); ok && v != "" {
		return v
	}
	v, _ := t["transaction_id"].(string)
	return v
}

func indicString(t map[string]any) string {
	s, _ := t["credit_debit_indicator"].(string)
	if s == "" {
		s, _ = t["credit_debit_indic"].(string)
	}
	return strings.ToUpper(s)
}

func firstRemittanceLine(t map[string]any) string {
	ri := t["remittance_information"]
	switch v := ri.(type) {
	case []any:
		if len(v) > 0 {
			s, _ := v[0].(string)
			return stripSEPAPrefixes(s)
		}
	case string:
		return stripSEPAPrefixes(v)
	}
	return ""
}

func joinRemittance(t map[string]any) string {
	joined, _ := joinRemittanceAndSEPA(t)
	return joined
}

func joinRemittanceAndSEPA(t map[string]any) (string, SEPARefs) {
	var refs SEPARefs
	ri := t["remittance_information"]
	switch v := ri.(type) {
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				continue
			}
			cleaned, lineRefs := parseSEPA(s)
			refs.merge(lineRefs)
			if cleaned != "" {
				parts = append(parts, cleaned)
			}
		}
		return strings.Join(parts, " "), refs
	case string:
		return parseSEPA(v)
	}
	return "", refs
}

func (c *Client) isOwnName(name string) bool {
	if len(c.ownNames) == 0 {
		return false
	}
	_, ok := c.ownNames[strings.ToLower(name)]
	return ok
}

func (c *Client) doWithRateLimitRetry(ctx context.Context, req *http.Request, headers map[string]string) ([]byte, int, error) {
	for attempt := 0; ; attempt++ {
		attemptReq := req.Clone(ctx)
		for k, v := range headers {
			attemptReq.Header.Set(k, v)
		}

		resp, err := c.http.Do(attemptReq)
		if err != nil {
			return nil, 0, fmt.Errorf("GET %s: %w", req.URL.String(), err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusTooManyRequests || attempt >= maxRateRetry {
			return body, resp.StatusCode, nil
		}

		wait := retryAfterDelay(resp.Header.Get("Retry-After"), attempt)
		log.Printf("Enable Banking rate limited (attempt %d/%d), waiting %s", attempt+1, maxRateRetry, wait)
		select {
		case <-ctx.Done():
			return nil, 0, fmt.Errorf("rate limit backoff cancelled: %w", ctx.Err())
		case <-time.After(wait):
		}
	}
}

func retryAfterDelay(header string, attempt int) time.Duration {
	if header != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && secs >= 0 {
			d := time.Duration(secs) * time.Second
			if d > maxRetryAfter {
				return maxRetryAfter
			}
			return d
		}
	}
	d := rateRetryBase << attempt
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}
