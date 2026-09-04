package enablebanking

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"bankingsync/logs"
)

var olog = logs.Get("bankingsync/enablebanking")

// ASPSP represents one bank from the Enable Banking /aspsps endpoint.
type ASPSP struct {
	Name                   string `json:"name"`
	Country                string `json:"country"`
	MaximumConsentValidity int    `json:"maximum_consent_validity"`
}

// DefaultConsentValidity is the access window requested when an ASPSP does not
// advertise a shorter maximum.
const DefaultConsentValidity = 180 * 24 * time.Hour

// ConsentValidity returns the window to request for this ASPSP, clamped to the
// bank's advertised maximum when it publishes one.
func (a ASPSP) ConsentValidity() time.Duration {
	if a.MaximumConsentValidity <= 0 {
		return DefaultConsentValidity
	}
	d := time.Duration(a.MaximumConsentValidity) * time.Second
	if d > DefaultConsentValidity {
		return DefaultConsentValidity
	}
	return d
}

// SessionAccount is a single account entry within a completed OAuth session.
//
// The JSON tags describe how bankingsync stores the value in pending_auth_accounts.
// They do not describe what Enable Banking sends — see UnmarshalJSON, which reads
// the wire format and is deliberately tolerant about where each value sits.
type SessionAccount struct {
	UID             string `json:"uid"`
	AccountUID      string `json:"account_uid"`
	ResourceID      string `json:"resource_id"`
	IBAN            string `json:"iban"`
	OwnerName       string `json:"owner_name"`
	Currency        string `json:"currency"`
	Name            string `json:"name"`
	Product         string `json:"product"`
	CashAccountType string `json:"cash_account_type"`
}

// UnmarshalJSON reads an account entry without assuming a single wire layout.
//
// Enable Banking wraps account identifiers in an object rather than exposing a
// top-level "iban" — the same shape the transaction parser already handles for
// creditor_account and debtor_account. Reading only the flat field leaves every
// IBAN empty, which is invisible: the picker still renders, just with nothing
// but a UUID to tell two accounts apart, and every IBAN-keyed feature
// downstream silently stops matching.
//
// Decoding goes through a map so that one unexpected field type cannot fail the
// whole entry, and every known location is probed in turn.
func (a *SessionAccount) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	a.UID = jsonString(raw, "uid")
	a.AccountUID = jsonString(raw, "account_uid")
	a.ResourceID = jsonString(raw, "resource_id")
	a.Currency = jsonString(raw, "currency")
	a.Name = jsonString(raw, "name")
	a.Product = jsonString(raw, "product")
	a.CashAccountType = jsonString(raw, "cash_account_type")
	a.OwnerName = firstNonEmpty(
		jsonString(raw, "owner_name"),
		jsonString(raw, "account_holder_name"),
		jsonString(raw, "psu_name"),
	)
	a.IBAN = normaliseIBAN(firstNonEmpty(jsonString(raw, "iban"), nestedIBAN(raw)))
	return nil
}

func jsonString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// nestedIBAN looks for the IBAN in the container shapes Enable Banking uses.
func nestedIBAN(raw map[string]any) string {
	for _, key := range []string{"account_id", "account_identification", "identification"} {
		if obj, ok := raw[key].(map[string]any); ok {
			if v := jsonString(obj, "iban"); v != "" {
				return v
			}
		}
	}
	for _, key := range []string{"all_account_ids", "account_ids"} {
		list, ok := raw[key].([]any)
		if !ok {
			continue
		}
		for _, item := range list {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if v := jsonString(obj, "iban"); v != "" {
				return v
			}
			if strings.EqualFold(jsonString(obj, "identification"), "IBAN") {
				if v := jsonString(obj, "identifier"); v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// EffectiveUID returns the best-available account UID across the three possible field names.
func (a SessionAccount) EffectiveUID() string {
	if a.UID != "" {
		return a.UID
	}
	if a.AccountUID != "" {
		return a.AccountUID
	}
	return a.ResourceID
}

// Label is what the account picker shows as the primary line. It never returns
// an empty string, and falls back to the UID only when the bank supplied nothing
// a human could recognise.
func (a SessionAccount) Label() string {
	if a.IBAN != "" {
		return a.MaskedIBAN()
	}
	return firstNonEmpty(a.Name, a.Product, a.CashAccountType, a.EffectiveUID())
}

// SuggestedAccountName is the target account name proposed when the operator has
// not configured one. It has to identify the bank account on sight, because with
// several accounts connected a shared default silently funnels all of them into
// one budget account.
//
// Renaming the result later in the budget backend is safe for Firefly, which
// re-finds the account by IBAN. Under Actual the name is the only handle there
// is, so a rename there still has to be mirrored in the web UI.
func (a SessionAccount) SuggestedAccountName(bankName string) string {
	bankName = strings.TrimSpace(bankName)
	detail := firstNonEmpty(a.Name, a.Product, a.MaskedIBAN(), a.CashAccountType)

	switch {
	case bankName != "" && detail != "" && !strings.EqualFold(bankName, detail):
		return bankName + " " + detail
	case bankName != "":
		return bankName
	case detail != "":
		return detail
	default:
		return ""
	}
}

// Describe is the secondary line of the picker: everything known about the
// account that is not already the label.
func (a SessionAccount) Describe() string {
	parts := make([]string, 0, 4)
	for _, v := range []string{a.OwnerName, a.Name, a.Product, a.Currency} {
		if v == "" || v == a.Label() {
			continue
		}
		if !slices.Contains(parts, v) {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, " · ")
}

// MaskedIBAN returns the IBAN with the middle portion replaced by asterisks.
func (a SessionAccount) MaskedIBAN() string {
	if len(a.IBAN) <= 8 {
		return a.IBAN
	}
	return a.IBAN[:4] + strings.Repeat("*", len(a.IBAN)-8) + a.IBAN[len(a.IBAN)-4:]
}

// SessionResponse is the decoded reply from POST /sessions.
type SessionResponse struct {
	SessionID string           `json:"session_id"`
	Accounts  []SessionAccount `json:"accounts"`
}

// GetASPSPs fetches the list of supported banks from the Enable Banking API.
func (c *Client) GetASPSPs(ctx context.Context) ([]ASPSP, error) {
	tracer := otel.Tracer("bankingsync/enablebanking")
	ctx, span := tracer.Start(ctx, "enablebanking.get_aspsps")
	defer span.End()

	headers, err := c.makeHeaders()
	if err != nil {
		return nil, fmt.Errorf("makeHeaders: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/aspsps", nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("GET /aspsps: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("GET /aspsps HTTP %d: %s", resp.StatusCode, body)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	var data struct {
		ASPSPs []ASPSP `json:"aspsps"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("decode /aspsps: %w", err)
	}
	span.SetAttributes(attribute.Int("bank_count", len(data.ASPSPs)))
	olog.Info(ctx, "enablebanking.get_aspsps.completed", logs.Int("bank_count", len(data.ASPSPs)))
	return data.ASPSPs, nil
}

// StartAuth initiates the OAuth authorisation flow and returns the redirect URL
// that the user must open in their browser. Enable Banking will redirect the user
// to appBaseURL+"/callback" with code and state query parameters on completion.
func (c *Client) StartAuth(ctx context.Context, bankName, bankCountry, psuType, stateUUID, appBaseURL string, validity ...time.Duration) (string, time.Time, error) {
	tracer := otel.Tracer("bankingsync/enablebanking")
	ctx, span := tracer.Start(ctx, "enablebanking.start_auth",
		trace.WithAttributes(
			attribute.String("bank", bankName),
			attribute.String("country", bankCountry),
		),
	)
	defer span.End()

	headers, err := c.makeHeaders()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("makeHeaders: %w", err)
	}

	window := DefaultConsentValidity
	if len(validity) > 0 && validity[0] > 0 {
		window = validity[0]
	}
	expiresAt := time.Now().UTC().Add(window).Truncate(time.Second)
	validUntil := expiresAt.Format("2006-01-02T15:04:05Z")

	// balances and transactions are separate consent scopes and are fixed at
	// authorisation time — Enable Banking offers no way to widen them afterwards.
	// Requesting balances explicitly is what makes opening balances and drift
	// detection possible; an existing connection keeps whatever it was granted
	// until the user renews it.
	access := map[string]any{
		"valid_until":  validUntil,
		"transactions": true,
		"balances":     RequestBalanceAccess(),
	}

	payload := map[string]any{
		"access":       access,
		"aspsp":        map[string]string{"name": bankName, "country": bankCountry},
		"state":        stateUUID,
		"redirect_url": appBaseURL + "/callback",
		"psu_type":     psuType,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/auth", bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", time.Time{}, fmt.Errorf("POST /auth: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("POST /auth HTTP %d: %s", resp.StatusCode, raw)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", time.Time{}, err
	}
	var result struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", time.Time{}, fmt.Errorf("decode /auth: %w", err)
	}
	olog.Info(ctx, "enablebanking.start_auth.completed",
		logs.String("bank", bankName),
		logs.String("country", bankCountry),
		logs.String("valid_until", validUntil),
	)
	return result.URL, expiresAt, nil
}

// CompleteAuth finalises the OAuth flow using the code and state received from
// the callback and returns the session with its associated accounts.
func (c *Client) CompleteAuth(ctx context.Context, code, state string) (*SessionResponse, error) {
	tracer := otel.Tracer("bankingsync/enablebanking")
	ctx, span := tracer.Start(ctx, "enablebanking.complete_auth")
	defer span.End()

	headers, err := c.makeHeaders()
	if err != nil {
		return nil, fmt.Errorf("makeHeaders: %w", err)
	}

	payload := map[string]string{"code": code, "state": state}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/sessions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("POST /sessions: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("POST /sessions HTTP %d: %s", resp.StatusCode, raw)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	captureResponse("session", "session response", raw)

	var sr SessionResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		return nil, fmt.Errorf("decode /sessions: %w", err)
	}
	span.SetAttributes(attribute.Int("account_count", len(sr.Accounts)))
	olog.Info(ctx, "enablebanking.complete_auth.completed", logs.Int("account_count", len(sr.Accounts)))
	return &sr, nil
}
