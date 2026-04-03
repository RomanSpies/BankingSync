package enablebanking

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
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
	banks, err := c.GetASPSPs()
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
	_, err := c.GetASPSPs()
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
	banks, err := c.GetASPSPs()
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
	url, err := c.StartAuth("TestBank", "DE", "personal", "state-uuid", "http://localhost:8080")
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
	_, _ = c.StartAuth("TestBank", "DE", "personal", "uuid-123", "http://myapp:8080")

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
	_, err := c.StartAuth("Bank", "DE", "personal", "uuid", "http://localhost:8080")
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
	sr, err := c.CompleteAuth("code-xyz", "state-uuid")
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
	sr, err := c.CompleteAuth("code", "state")
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
	_, err := c.CompleteAuth("bad-code", "state")
	if err == nil {
		t.Error("expected error on HTTP 401")
	}
}
