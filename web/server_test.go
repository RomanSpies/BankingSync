package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"bankingsync/enablebanking"
	"bankingsync/store"
)

// --- helpers ----------------------------------------------------------------

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func noopEB() *enablebanking.Client {
	return enablebanking.NewClient(
		func() (string, error) { return "test-app-id", nil },
		func() ([]byte, error) { return nil, fmt.Errorf("no PEM in tests") },
		nil,
	)
}

func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st := openTestStore(t)
	srv, err := New(st, noopEB(), func() {}, TemplateFS)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, st
}

func get(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

func post(t *testing.T, srv *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

// --- handleHealth -----------------------------------------------------------

func TestHandleHealth_returns200(t *testing.T) {
	srv, _ := newTestServer(t)
	w := get(t, srv, "/health")
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
}

// --- handleIndex ------------------------------------------------------------

func TestHandleIndex_noSetup_redirectsToSetup(t *testing.T) {
	srv, _ := newTestServer(t)
	w := get(t, srv, "/")
	if w.Code != http.StatusFound {
		t.Errorf("got %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/setup" {
		t.Errorf("Location: got %q, want /setup", loc)
	}
}

func TestHandleIndex_setupDone_noAccounts_redirectsToConnect(t *testing.T) {
	srv, st := newTestServer(t)
	_ = st.SetSetting("eb_pem_content", "pem-data")
	w := get(t, srv, "/")
	if w.Code != http.StatusFound {
		t.Errorf("got %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/connect" {
		t.Errorf("Location: got %q, want /connect", loc)
	}
}

func TestHandleIndex_connected_redirectsToStatus(t *testing.T) {
	srv, st := newTestServer(t)
	_ = st.SetSetting("eb_pem_content", "pem-data")
	_, _ = st.AddBankAccount("sess", "acct", "Bank", "DE", "2025-01-01T00:00:00Z")
	w := get(t, srv, "/")
	if w.Code != http.StatusFound {
		t.Errorf("got %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/status" {
		t.Errorf("Location: got %q, want /status", loc)
	}
}

func TestHandleIndex_unknownPath_404(t *testing.T) {
	srv, _ := newTestServer(t)
	w := get(t, srv, "/not-a-real-path")
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", w.Code)
	}
}

// --- handleSetup ------------------------------------------------------------

func TestHandleSetup_GET_renders(t *testing.T) {
	srv, _ := newTestServer(t)
	w := get(t, srv, "/setup")
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Errorf("Content-Type: got %q", w.Header().Get("Content-Type"))
	}
}

func TestHandleSetup_GET_pemAlreadyStoredShowsMessage(t *testing.T) {
	srv, st := newTestServer(t)
	_ = st.SetSetting("eb_pem_content", "some-pem")
	w := get(t, srv, "/setup")
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "already stored") {
		t.Error("expected 'already stored' message when PEM is in DB")
	}
}

func TestHandleSetup_POST_missingPEM_rendersError(t *testing.T) {
	srv, _ := newTestServer(t)
	w := post(t, srv, "/setup", url.Values{"app_id": {"test-uuid"}})
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200 (re-render with error)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Please upload a PEM file") {
		t.Error("expected PEM error message in response")
	}
}

// --- handleConnect ----------------------------------------------------------

func TestHandleConnect_GET_renders(t *testing.T) {
	srv, st := newTestServer(t)
	_ = st.SetSetting("eb_pem_content", "pem")
	w := get(t, srv, "/connect")
	// getASPSPs will fail (no real EB API), but handler renders with error — still 200.
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Errorf("Content-Type: got %q", w.Header().Get("Content-Type"))
	}
}

func TestHandleConnect_GET_withCachedBanks(t *testing.T) {
	srv, st := newTestServer(t)
	banks := []enablebanking.ASPSP{
		{Name: "TestBank", Country: "DE"},
		{Name: "OtherBank", Country: "FR"},
	}
	banksJSON, _ := json.Marshal(banks)
	_ = st.SetSetting("aspsp_cache", string(banksJSON))
	_ = st.SetSetting("aspsp_cache_at", "2099-01-01T00:00:00Z") // far future
	_ = st.SetSetting("eb_pem_content", "pem")

	w := get(t, srv, "/connect")
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "TestBank") {
		t.Error("expected TestBank in connect page")
	}
	if !strings.Contains(body, "OtherBank") {
		t.Error("expected OtherBank in connect page")
	}
}

// --- handleStatus -----------------------------------------------------------

func TestHandleStatus_GET_noAccounts(t *testing.T) {
	srv, _ := newTestServer(t)
	w := get(t, srv, "/status")
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "No bank accounts connected") {
		t.Error("expected 'No bank accounts connected' message")
	}
}

func TestHandleStatus_GET_withAccount(t *testing.T) {
	srv, st := newTestServer(t)
	_, _ = st.AddBankAccount("sess", "acct", "TestBank", "DE", "2026-01-01T00:00:00Z")
	w := get(t, srv, "/status")
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "TestBank") {
		t.Error("expected TestBank in status page")
	}
}

func TestHandleStatus_GET_showsLastSync(t *testing.T) {
	srv, st := newTestServer(t)
	_ = st.SetLastSyncDate("2024-06-01")
	w := get(t, srv, "/status")
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "2024-06-01") {
		t.Error("expected last sync date in status page")
	}
}

// --- handleCallback ---------------------------------------------------------

func TestHandleCallback_missingCode_redirectsToConnect(t *testing.T) {
	srv, _ := newTestServer(t)
	w := get(t, srv, "/callback?state=uuid")
	if w.Code != http.StatusFound {
		t.Errorf("got %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/connect") {
		t.Errorf("Location: got %q, want /connect?...", loc)
	}
}

func TestHandleCallback_missingState_redirectsToConnect(t *testing.T) {
	srv, _ := newTestServer(t)
	w := get(t, srv, "/callback?code=abc")
	if w.Code != http.StatusFound {
		t.Errorf("got %d, want 302", w.Code)
	}
	if !strings.HasPrefix(w.Header().Get("Location"), "/connect") {
		t.Errorf("Location: got %q", w.Header().Get("Location"))
	}
}

func TestHandleCallback_wrongState_redirectsToConnect(t *testing.T) {
	srv, st := newTestServer(t)
	_ = st.SetSetting("pending_session_state", "correct-uuid")
	w := get(t, srv, "/callback?code=abc&state=wrong-uuid")
	if w.Code != http.StatusFound {
		t.Errorf("got %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/connect") {
		t.Errorf("Location: got %q, want /connect?...", loc)
	}
}

// --- handlePickAccount ------------------------------------------------------

func TestHandlePickAccount_GET_renders(t *testing.T) {
	srv, _ := newTestServer(t)
	w := get(t, srv, "/pick-account")
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
}

// --- handleRemoveAccount ----------------------------------------------------

func TestHandleRemoveAccount_GET_returns404(t *testing.T) {
	srv, _ := newTestServer(t)
	w := get(t, srv, "/remove-account")
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", w.Code)
	}
}

func TestHandleRemoveAccount_POST_removesAccount(t *testing.T) {
	srv, st := newTestServer(t)
	id, _ := st.AddBankAccount("sess", "acct", "Bank", "DE", "2025-01-01T00:00:00Z")
	w := post(t, srv, "/remove-account", url.Values{"account_id": {fmt.Sprintf("%d", id)}})
	if w.Code != http.StatusFound {
		t.Errorf("got %d, want 302", w.Code)
	}
	accounts, _ := st.GetAllBankAccounts()
	if len(accounts) != 0 {
		t.Errorf("expected 0 accounts after remove, got %d", len(accounts))
	}
}

// --- handleSyncNow ----------------------------------------------------------

func TestHandleSyncNow_GET_returns404(t *testing.T) {
	srv, _ := newTestServer(t)
	w := get(t, srv, "/sync/now")
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", w.Code)
	}
}

func TestHandleSyncNow_POST_returnsOK(t *testing.T) {
	st := openTestStore(t)
	triggered := make(chan struct{}, 1)
	srv, err := New(st, noopEB(), func() { triggered <- struct{}{} }, TemplateFS)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	w := post(t, srv, "/sync/now", nil)
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("expected ok:true in response, got %s", w.Body.String())
	}
}

func TestHandleSyncNow_POST_alreadyRunning_returnsNotOK(t *testing.T) {
	st := openTestStore(t)
	block := make(chan struct{})
	srv, _ := New(st, noopEB(), func() { <-block }, TemplateFS)

	// Start first sync (goroutine will block on channel)
	w1 := post(t, srv, "/sync/now", nil)
	if !strings.Contains(w1.Body.String(), `"ok":true`) {
		t.Fatalf("first sync should succeed, got %s", w1.Body.String())
	}

	// Second request while first is running
	w2 := post(t, srv, "/sync/now", nil)
	if !strings.Contains(w2.Body.String(), `"ok":false`) {
		t.Errorf("second sync should be rejected, got %s", w2.Body.String())
	}

	close(block) // unblock the goroutine
}

// --- handleRenew ------------------------------------------------------------

func TestHandleRenew_GET_returns404(t *testing.T) {
	srv, _ := newTestServer(t)
	w := get(t, srv, "/renew")
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", w.Code)
	}
}

func TestHandleRenew_POST_unknownAccount_redirectsToStatus(t *testing.T) {
	srv, _ := newTestServer(t)
	w := post(t, srv, "/renew", url.Values{"account_id": {"999"}})
	if w.Code != http.StatusFound {
		t.Errorf("got %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/status" {
		t.Errorf("Location: got %q, want /status", loc)
	}
}

// --- pure helpers -----------------------------------------------------------

func TestUniqueCountries_deduplicates(t *testing.T) {
	banks := []enablebanking.ASPSP{
		{Name: "A", Country: "DE"},
		{Name: "B", Country: "FR"},
		{Name: "C", Country: "DE"},
	}
	got := uniqueCountries(banks)
	if len(got) != 2 {
		t.Errorf("expected 2 unique countries, got %d: %v", len(got), got)
	}
}

func TestUniqueCountries_empty(t *testing.T) {
	got := uniqueCountries(nil)
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestUniqueCountries_preservesOrder(t *testing.T) {
	banks := []enablebanking.ASPSP{
		{Country: "DE"},
		{Country: "FR"},
		{Country: "AT"},
	}
	got := uniqueCountries(banks)
	if got[0] != "DE" || got[1] != "FR" || got[2] != "AT" {
		t.Errorf("order not preserved: %v", got)
	}
}

func TestURLEncode_safechars(t *testing.T) {
	got := urlEncode("hello-world_123")
	if got != "hello-world_123" {
		t.Errorf("got %q, want hello-world_123", got)
	}
}

func TestURLEncode_specialChars(t *testing.T) {
	got := urlEncode("hello world")
	if got != "hello%20world" {
		t.Errorf("got %q, want hello%%20world", got)
	}
}

func TestURLEncode_colon(t *testing.T) {
	got := urlEncode("err: bad")
	if !strings.Contains(got, "%3A") && !strings.Contains(got, "%3a") {
		t.Errorf("colon should be encoded, got %q", got)
	}
}

func TestDetectBaseURL_fromForwardedHeaders(t *testing.T) {
	st := openTestStore(t)
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "myapp.example.com")
	got := detectBaseURL(req, st)
	if got != "https://myapp.example.com" {
		t.Errorf("got %q, want https://myapp.example.com", got)
	}
	// Should also be stored in DB
	stored, _ := st.GetSetting("eb_base_url")
	if stored != "https://myapp.example.com" {
		t.Errorf("stored: got %q", stored)
	}
}

func TestDetectBaseURL_fromRequestHost(t *testing.T) {
	st := openTestStore(t)
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Host = "192.168.1.50:8080"
	got := detectBaseURL(req, st)
	if got != "http://192.168.1.50:8080" {
		t.Errorf("got %q", got)
	}
}

func TestDetectBaseURL_fallbackToStored(t *testing.T) {
	st := openTestStore(t)
	_ = st.SetSetting("eb_base_url", "https://saved.example.com")
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	// No host header
	got := detectBaseURL(req, st)
	if got != "https://saved.example.com" {
		t.Errorf("got %q, want https://saved.example.com", got)
	}
}

func TestDetectBaseURL_fallbackToLocalhost(t *testing.T) {
	st := openTestStore(t)
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	got := detectBaseURL(req, st)
	if got != "https://localhost:8443" {
		t.Errorf("got %q, want https://localhost:8443", got)
	}
}

// --- isSetup / isConnected --------------------------------------------------

func TestIsSetup_falseWhenEmpty(t *testing.T) {
	srv, _ := newTestServer(t)
	if srv.isSetup() {
		t.Error("expected false on fresh store with no /data/*.pem")
	}
}

func TestIsSetup_trueWhenPEMInDB(t *testing.T) {
	srv, st := newTestServer(t)
	_ = st.SetSetting("eb_pem_content", "pem-data")
	if !srv.isSetup() {
		t.Error("expected true when eb_pem_content is in DB")
	}
}

func TestIsConnected_falseWhenNoAccounts(t *testing.T) {
	srv, _ := newTestServer(t)
	if srv.isConnected() {
		t.Error("expected false with no accounts")
	}
}

func TestIsConnected_trueWhenAccountExists(t *testing.T) {
	srv, st := newTestServer(t)
	_, _ = st.AddBankAccount("sess", "acct", "Bank", "DE", "2025-01-01T00:00:00Z")
	if !srv.isConnected() {
		t.Error("expected true when account exists")
	}
}
