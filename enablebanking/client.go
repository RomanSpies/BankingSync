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
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

const apiBase = "https://api.enablebanking.com"

// Transaction is a normalised transaction record returned from the Enable Banking API.
type Transaction struct {
	Status string
	Date   time.Time

	AmountCents int64
	Payee       string
	Notes       string
	EntryRef    string
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

// NewClient returns a Client using the provided resolvers for App ID and PEM key.
// ownNames is a lowercase set of account-holder names used to filter out
// self-transfer payees on credit transactions.
func NewClient(appID AppIDResolver, pemSrc PEMSource, ownNames map[string]struct{}) *Client {
	return &Client{
		appID:    appID,
		pemSrc:   pemSrc,
		ownNames: ownNames,
		http:     &http.Client{Timeout: 30 * time.Second},
		baseURL:  apiBase,
	}
}

// FetchTransactions retrieves all transactions for accountUID from dateFrom
// through today, following continuation keys to page through the full result set.
func (c *Client) FetchTransactions(ctx context.Context, accountUID string, dateFrom time.Time) ([]Transaction, error) {
	headers, err := c.makeHeaders()
	if err != nil {
		return nil, fmt.Errorf("makeHeaders: %w", err)
	}

	params := fmt.Sprintf("date_from=%s&date_to=%s",
		dateFrom.Format("2006-01-02"),
		time.Now().UTC().Format("2006-01-02"),
	)

	var txns []Transaction
	txnURL := fmt.Sprintf("%s/accounts/%s/transactions", c.baseURL, accountUID)
	reqURL := txnURL + "?" + params

	for reqURL != "" {
		raw, ck, err := c.fetchPage(ctx, reqURL, headers)
		if err != nil {
			return nil, err
		}
		for _, r := range raw {
			t, err := c.parseTransaction(r)
			if err != nil {
				log.Printf("Skipping malformed transaction: %v | %v", err, r)
				continue
			}
			txns = append(txns, t)
		}
		if ck != "" {
			reqURL = txnURL + "?continuation_key=" + ck
		} else {
			reqURL = ""
		}

		params = ""
	}

	log.Printf("Fetched %d transactions from Enable Banking", len(txns))
	return txns, nil
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

	resp, err := c.http.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, "", fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("Enable Banking error %d: %s", resp.StatusCode, body)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, "", err
	}

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
	notes := parseNotes(t)
	ref := getEntryRef(t)
	status, _ := t["status"].(string)
	if status == "" {
		status = "BOOK"
	}
	return Transaction{
		Status:      status,
		Date:        date,
		AmountCents: amountCents,
		Payee:       payee,
		Notes:       notes,
		EntryRef:    ref,
	}, nil
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
		rawAmt = "0"
	}
	f, err := strconv.ParseFloat(rawAmt, 64)
	if err != nil {
		return 0, fmt.Errorf("parse amount %q: %w", rawAmt, err)
	}
	cents := int64(math.Round(f * 100))

	indic := indicString(t)
	if indic == "DBIT" {
		if cents > 0 {
			cents = -cents
		}
	} else {
		if cents < 0 {
			cents = -cents
		}
	}
	return cents, nil
}

func (c *Client) parsePayee(t map[string]any) string {
	indic := indicString(t)
	var name string
	if indic == "DBIT" {

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
	if ref, ok := t["remittance_information_unstructured"].(string); ok && ref != "" {
		return ref
	}
	return joinRemittance(t)
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
			return s
		}
	case string:
		return v
	}
	return ""
}

func joinRemittance(t map[string]any) string {
	ri := t["remittance_information"]
	switch v := ri.(type) {
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	case string:
		return v
	}
	return ""
}

func (c *Client) isOwnName(name string) bool {
	if len(c.ownNames) == 0 {
		return false
	}
	_, ok := c.ownNames[strings.ToLower(name)]
	return ok
}
