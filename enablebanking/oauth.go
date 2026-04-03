package enablebanking

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ASPSP represents one bank from the Enable Banking /aspsps endpoint.
type ASPSP struct {
	Name    string `json:"name"`
	Country string `json:"country"`
}

// SessionAccount is a single account entry within a completed OAuth session.
type SessionAccount struct {
	UID        string `json:"uid"`
	AccountUID string `json:"account_uid"`
	ResourceID string `json:"resource_id"`
	IBAN       string `json:"iban"`
	OwnerName  string `json:"owner_name"`
	Currency   string `json:"currency"`
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
func (c *Client) GetASPSPs() ([]ASPSP, error) {
	headers, err := c.makeHeaders()
	if err != nil {
		return nil, fmt.Errorf("makeHeaders: %w", err)
	}
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/aspsps", nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /aspsps: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /aspsps HTTP %d: %s", resp.StatusCode, body)
	}
	var data struct {
		ASPSPs []ASPSP `json:"aspsps"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("decode /aspsps: %w", err)
	}
	return data.ASPSPs, nil
}

// StartAuth initiates the OAuth authorisation flow and returns the redirect URL
// that the user must open in their browser. Enable Banking will redirect the user
// to appBaseURL+"/callback" with code and state query parameters on completion.
func (c *Client) StartAuth(bankName, bankCountry, psuType, stateUUID, appBaseURL string) (string, error) {
	headers, err := c.makeHeaders()
	if err != nil {
		return "", fmt.Errorf("makeHeaders: %w", err)
	}

	validUntil := time.Now().UTC().Add(180 * 24 * time.Hour).Format("2006-01-02T15:04:05Z")

	payload := map[string]any{
		"access":       map[string]string{"valid_until": validUntil},
		"aspsp":        map[string]string{"name": bankName, "country": bankCountry},
		"state":        stateUUID,
		"redirect_url": appBaseURL + "/callback",
		"psu_type":     psuType,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/auth", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST /auth: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("POST /auth HTTP %d: %s", resp.StatusCode, raw)
	}
	var result struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("decode /auth: %w", err)
	}
	return result.URL, nil
}

// CompleteAuth finalises the OAuth flow using the code and state received from
// the callback and returns the session with its associated accounts.
func (c *Client) CompleteAuth(code, state string) (*SessionResponse, error) {
	headers, err := c.makeHeaders()
	if err != nil {
		return nil, fmt.Errorf("makeHeaders: %w", err)
	}

	payload := map[string]string{"code": code, "state": state}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/sessions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST /sessions: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("POST /sessions HTTP %d: %s", resp.StatusCode, raw)
	}
	var sr SessionResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		return nil, fmt.Errorf("decode /sessions: %w", err)
	}
	return &sr, nil
}
