package enablebanking

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// testRSAKeyPEM generates a fresh RSA private key encoded as PKCS1 PEM.
func testRSAKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// newTestClientWith creates a Client whose HTTP calls are served by mux.
func newTestClientWith(t *testing.T, mux *http.ServeMux) *Client {
	t.Helper()
	keyPEM := testRSAKeyPEM(t)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	c := NewClient(
		func() (string, error) { return "test-app-id", nil },
		func() ([]byte, error) { return keyPEM, nil },
		nil,
	)
	c.baseURL = ts.URL
	return c
}

// --- EffectiveUID -----------------------------------------------------------

func TestEffectiveUID_uid(t *testing.T) {
	a := SessionAccount{UID: "u", AccountUID: "au", ResourceID: "r"}
	if got := a.EffectiveUID(); got != "u" {
		t.Errorf("got %q, want u", got)
	}
}

func TestEffectiveUID_accountUID_whenUIDEmpty(t *testing.T) {
	a := SessionAccount{AccountUID: "au", ResourceID: "r"}
	if got := a.EffectiveUID(); got != "au" {
		t.Errorf("got %q, want au", got)
	}
}

func TestEffectiveUID_resourceID_whenOthersEmpty(t *testing.T) {
	a := SessionAccount{ResourceID: "r"}
	if got := a.EffectiveUID(); got != "r" {
		t.Errorf("got %q, want r", got)
	}
}

func TestEffectiveUID_allEmpty(t *testing.T) {
	a := SessionAccount{}
	if got := a.EffectiveUID(); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestMaskedIBAN_normal(t *testing.T) {
	a := SessionAccount{IBAN: "DE89370400440532013000"}
	got := a.MaskedIBAN()
	want := "DE89**************3000"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMaskedIBAN_short(t *testing.T) {
	a := SessionAccount{IBAN: "DE123456"}
	if got := a.MaskedIBAN(); got != "DE123456" {
		t.Errorf("got %q, want DE123456 (no masking for 8 chars)", got)
	}
}

func TestMaskedIBAN_empty(t *testing.T) {
	a := SessionAccount{}
	if got := a.MaskedIBAN(); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// --- GetASPSPs --------------------------------------------------------------

func TestGetASPSPs_success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/aspsps", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"aspsps": []map[string]any{
				{"name": "TestBank", "country": "DE"},
				{"name": "AnotherBank", "country": "FR"},
			},
		})
	})
	c := newTestClientWith(t, mux)
	banks, err := c.GetASPSPs(context.Background())
	if err != nil {
		t.Fatalf("GetASPSPs: %v", err)
	}
	if len(banks) != 2 {
		t.Fatalf("expected 2 banks, got %d", len(banks))
	}
	if banks[0].Name != "TestBank" || banks[0].Country != "DE" {
		t.Errorf("banks[0]: got %+v", banks[0])
	}
	if banks[1].Name != "AnotherBank" || banks[1].Country != "FR" {
		t.Errorf("banks[1]: got %+v", banks[1])
	}
}

func TestGetASPSPs_httpError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/aspsps", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	})
	c := newTestClientWith(t, mux)
	_, err := c.GetASPSPs(context.Background())
	if err == nil {
		t.Error("expected error on HTTP 500")
	}
}

func TestGetASPSPs_emptyList(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/aspsps", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"aspsps": []any{}})
	})
	c := newTestClientWith(t, mux)
	banks, err := c.GetASPSPs(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(banks) != 0 {
		t.Errorf("expected empty slice, got %d banks", len(banks))
	}
}

// --- StartAuth --------------------------------------------------------------

func TestStartAuth_success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"url": "https://bank.example.com/auth?token=abc",
		})
	})
	c := newTestClientWith(t, mux)
	url, _, err := c.StartAuth(context.Background(), "TestBank", "DE", "personal", "state-uuid", "http://localhost:8080")
	if err != nil {
		t.Fatalf("StartAuth: %v", err)
	}
	if url != "https://bank.example.com/auth?token=abc" {
		t.Errorf("got %q", url)
	}
}

func TestStartAuth_setsRedirectURLAndState(t *testing.T) {
	var capturedBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		_ = json.NewEncoder(w).Encode(map[string]string{"url": "https://bank.example.com"})
	})
	c := newTestClientWith(t, mux)
	_, _, _ = c.StartAuth(context.Background(), "TestBank", "DE", "personal", "uuid-123", "http://myapp:8080")

	if capturedBody["redirect_url"] != "http://myapp:8080/callback" {
		t.Errorf("redirect_url: got %q, want http://myapp:8080/callback", capturedBody["redirect_url"])
	}
	if capturedBody["state"] != "uuid-123" {
		t.Errorf("state: got %q, want uuid-123", capturedBody["state"])
	}
	if capturedBody["psu_type"] != "personal" {
		t.Errorf("psu_type: got %q, want personal", capturedBody["psu_type"])
	}
}

func TestStartAuth_httpError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	})
	c := newTestClientWith(t, mux)
	_, _, err := c.StartAuth(context.Background(), "Bank", "DE", "personal", "uuid", "http://localhost:8080")
	if err == nil {
		t.Error("expected error on HTTP 400")
	}
}

// --- CompleteAuth -----------------------------------------------------------

func TestCompleteAuth_success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewEncoder(w).Encode(SessionResponse{
			SessionID: "sess-abc",
			Accounts:  []SessionAccount{{UID: "acct-1"}},
		})
	})
	c := newTestClientWith(t, mux)
	sr, err := c.CompleteAuth(context.Background(), "code-xyz", "state-uuid")
	if err != nil {
		t.Fatalf("CompleteAuth: %v", err)
	}
	if sr.SessionID != "sess-abc" {
		t.Errorf("SessionID: got %q, want sess-abc", sr.SessionID)
	}
	if len(sr.Accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(sr.Accounts))
	}
	if sr.Accounts[0].EffectiveUID() != "acct-1" {
		t.Errorf("EffectiveUID: got %q", sr.Accounts[0].EffectiveUID())
	}
}

func TestCompleteAuth_multipleAccounts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/sessions", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(SessionResponse{
			SessionID: "sess-multi",
			Accounts: []SessionAccount{
				{UID: "acct-1"},
				{AccountUID: "acct-2"},
				{ResourceID: "acct-3"},
			},
		})
	})
	c := newTestClientWith(t, mux)
	sr, err := c.CompleteAuth(context.Background(), "code", "state")
	if err != nil {
		t.Fatalf("CompleteAuth: %v", err)
	}
	if len(sr.Accounts) != 3 {
		t.Fatalf("expected 3 accounts, got %d", len(sr.Accounts))
	}
	if sr.Accounts[0].EffectiveUID() != "acct-1" {
		t.Errorf("accounts[0]: got %q", sr.Accounts[0].EffectiveUID())
	}
	if sr.Accounts[1].EffectiveUID() != "acct-2" {
		t.Errorf("accounts[1]: got %q", sr.Accounts[1].EffectiveUID())
	}
	if sr.Accounts[2].EffectiveUID() != "acct-3" {
		t.Errorf("accounts[2]: got %q", sr.Accounts[2].EffectiveUID())
	}
}

func TestCompleteAuth_httpError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/sessions", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	c := newTestClientWith(t, mux)
	_, err := c.CompleteAuth(context.Background(), "bad-code", "state")
	if err == nil {
		t.Error("expected error on HTTP 401")
	}
}

func TestASPSP_ConsentValidity(t *testing.T) {
	cases := []struct {
		name string
		max  int
		want time.Duration
	}{
		{"unset falls back to default", 0, DefaultConsentValidity},
		{"negative falls back to default", -1, DefaultConsentValidity},
		{"90 days is honoured", 90 * 24 * 3600, 90 * 24 * time.Hour},
		{"longer than default is clamped", 365 * 24 * 3600, DefaultConsentValidity},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := ASPSP{Name: "X", Country: "DE", MaximumConsentValidity: c.max}
			if got := a.ConsentValidity(); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestGetASPSPs_parsesMaximumConsentValidity(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/aspsps", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"aspsps":[{"name":"ShortBank","country":"DE","maximum_consent_validity":7776000}]}`))
	})
	c := newTestClientWith(t, mux)

	banks, err := c.GetASPSPs(context.Background())
	if err != nil {
		t.Fatalf("GetASPSPs: %v", err)
	}
	if len(banks) != 1 {
		t.Fatalf("got %d banks, want 1", len(banks))
	}
	if banks[0].MaximumConsentValidity != 7776000 {
		t.Errorf("maximum_consent_validity: got %d, want 7776000", banks[0].MaximumConsentValidity)
	}
	if got := banks[0].ConsentValidity(); got != 90*24*time.Hour {
		t.Errorf("ConsentValidity: got %v, want 90 days", got)
	}
}

func TestStartAuth_requestsClampedValidityAndReturnsIt(t *testing.T) {
	var body map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"url":"https://bank.example/auth"}`))
	})
	c := newTestClientWith(t, mux)

	before := time.Now().UTC()
	_, expiresAt, err := c.StartAuth(context.Background(), "ShortBank", "DE", "personal", "s", "http://localhost:8443",
		90*24*time.Hour)
	if err != nil {
		t.Fatalf("StartAuth: %v", err)
	}

	access, _ := body["access"].(map[string]any)
	validUntil, _ := access["valid_until"].(string)
	sent, err := time.Parse("2006-01-02T15:04:05Z", validUntil)
	if err != nil {
		t.Fatalf("valid_until %q: %v", validUntil, err)
	}
	days := sent.Sub(before).Hours() / 24
	if days < 89 || days > 91 {
		t.Errorf("requested validity: got %.1f days, want ~90", days)
	}
	if !expiresAt.Equal(sent) {
		t.Errorf("returned expiry %v does not match the requested valid_until %v", expiresAt, sent)
	}
}

func TestStartAuth_defaultValidityWhenUnspecified(t *testing.T) {
	var body map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"url":"https://bank.example/auth"}`))
	})
	c := newTestClientWith(t, mux)

	before := time.Now().UTC()
	_, expiresAt, err := c.StartAuth(context.Background(), "Bank", "DE", "personal", "s", "http://localhost:8443")
	if err != nil {
		t.Fatalf("StartAuth: %v", err)
	}
	if days := expiresAt.Sub(before).Hours() / 24; days < 179 || days > 181 {
		t.Errorf("default validity: got %.1f days, want ~180", days)
	}
}

// The balances scope cannot be widened after authorisation, so it has to be in
// the very first request or the feature is unavailable until the user renews.
func TestStartAuth_requestsBalanceAndTransactionAccess(t *testing.T) {
	var captured map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_ = json.NewEncoder(w).Encode(map[string]string{"url": "https://bank.example.com/auth"})
	})
	c := newTestClientWith(t, mux)
	if _, _, err := c.StartAuth(context.Background(), "TestBank", "DE", "personal", "s", "http://localhost:8080"); err != nil {
		t.Fatalf("StartAuth: %v", err)
	}

	access, ok := captured["access"].(map[string]any)
	if !ok {
		t.Fatalf("access is not an object: %#v", captured["access"])
	}
	for _, scope := range []string{"balances", "transactions"} {
		if v, _ := access[scope].(bool); !v {
			t.Errorf("access.%s = %#v, want true", scope, access[scope])
		}
	}
	if _, ok := access["valid_until"].(string); !ok {
		t.Errorf("valid_until went missing: %#v", access["valid_until"])
	}
}

func TestStartAuth_balanceAccessCanBeOptedOut(t *testing.T) {
	t.Setenv(BalanceAccessEnv, "false")

	var captured map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_ = json.NewEncoder(w).Encode(map[string]string{"url": "https://bank.example.com/auth"})
	})
	c := newTestClientWith(t, mux)
	if _, _, err := c.StartAuth(context.Background(), "TestBank", "DE", "personal", "s", "http://localhost:8080"); err != nil {
		t.Fatalf("StartAuth: %v", err)
	}

	access, _ := captured["access"].(map[string]any)
	if v, _ := access["balances"].(bool); v {
		t.Error("balances was still requested despite the opt-out")
	}
	if v, _ := access["transactions"].(bool); !v {
		t.Error("opting out of balances must not disable transactions")
	}
}
