package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
	srv, err := New(st, noopEB(), func() bool { return true }, nil, TemplateFS)
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
	srv, st := newTestServer(t)
	_, _ = st.AddBankAccount(store.NewBankAccount{SessionID: "sess", AccountUID: "acct", BankName: "Bank", BankCountry: "DE", SessionExpiry: "2027-01-01T00:00:00Z"})
	_ = st.SetLastSyncDate("2026-04-01")
	w := get(t, srv, "/health")
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Errorf("expected status ok, got %s", w.Body.String())
	}
}

func TestHandleHealth_noAccounts_returns503(t *testing.T) {
	srv, _ := newTestServer(t)
	w := get(t, srv, "/health")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503", w.Code)
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
	_, _ = st.AddBankAccount(store.NewBankAccount{SessionID: "sess", AccountUID: "acct", BankName: "Bank", BankCountry: "DE", SessionExpiry: "2025-01-01T00:00:00Z"})
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
	_, _ = st.AddBankAccount(store.NewBankAccount{SessionID: "sess", AccountUID: "acct", BankName: "TestBank", BankCountry: "DE", SessionExpiry: "2026-01-01T00:00:00Z"})
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

func TestHandlePickAccount_POST_savesActualAccount(t *testing.T) {
	srv, st := newTestServer(t)
	accts := []enablebanking.SessionAccount{{UID: "uid-1"}}
	data, _ := json.Marshal(accts)
	_ = st.SetSetting("pending_auth_session_id", "sess-1")
	_ = st.SetSetting("pending_auth_accounts", string(data))
	_ = st.SetSetting("pending_auth_expiry", "2027-01-01T00:00:00Z")
	_ = st.SetSetting("pending_auth_bank_name", "TestBank")
	_ = st.SetSetting("pending_auth_bank_country", "DE")

	w := post(t, srv, "/pick-account", url.Values{
		"account_uid":    {"uid-1"},
		"actual_account": {"MyChecking"},
	})
	if w.Code != http.StatusFound {
		t.Fatalf("got %d, want 302", w.Code)
	}
	accounts, _ := st.GetAllBankAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accounts))
	}
	if accounts[0].ActualAccount != "MyChecking" {
		t.Errorf("ActualAccount: got %q, want MyChecking", accounts[0].ActualAccount)
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
	id, _ := st.AddBankAccount(store.NewBankAccount{SessionID: "sess", AccountUID: "acct", BankName: "Bank", BankCountry: "DE", SessionExpiry: "2025-01-01T00:00:00Z"})
	w := post(t, srv, "/remove-account", url.Values{"account_id": {fmt.Sprintf("%d", id)}})
	if w.Code != http.StatusFound {
		t.Errorf("got %d, want 302", w.Code)
	}
	accounts, _ := st.GetAllBankAccounts()
	if len(accounts) != 0 {
		t.Errorf("expected 0 accounts after remove, got %d", len(accounts))
	}
}

// --- handleResetSync --------------------------------------------------------

func TestHandleResetSync_GET_returns404(t *testing.T) {
	srv, _ := newTestServer(t)
	w := get(t, srv, "/reset-sync")
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", w.Code)
	}
}

func TestHandleResetSync_POST_updatesStartDate(t *testing.T) {
	srv, st := newTestServer(t)
	id, _ := st.AddBankAccount(store.NewBankAccount{SessionID: "sess", AccountUID: "acct", BankName: "Bank", BankCountry: "DE", SessionExpiry: "2025-01-01T00:00:00Z"})
	w := post(t, srv, "/reset-sync", url.Values{
		"account_id": {fmt.Sprintf("%d", id)},
		"start_date": {"2025-06-01"},
	})
	if w.Code != http.StatusFound {
		t.Errorf("got %d, want 302", w.Code)
	}
	accounts, _ := st.GetAllBankAccounts()
	if accounts[0].StartSyncDate != "2025-06-01" {
		t.Errorf("StartSyncDate: got %q, want 2025-06-01", accounts[0].StartSyncDate)
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
	srv, err := New(st, noopEB(), func() bool { triggered <- struct{}{}; return true }, nil, TemplateFS)
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
	srv, _ := New(st, noopEB(), func() bool { <-block; return true }, nil, TemplateFS)

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

// --- handleTestEmail --------------------------------------------------------

func TestHandleTestEmail_GET_returns404(t *testing.T) {
	srv, _ := newTestServer(t)
	w := get(t, srv, "/test-email")
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", w.Code)
	}
}

func TestHandleTestEmail_POST_nilFunc_returnsError(t *testing.T) {
	srv, _ := newTestServer(t)
	w := post(t, srv, "/test-email", nil)
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Errorf("expected ok:false, got %s", w.Body.String())
	}
}

func TestHandleTestEmail_POST_success(t *testing.T) {
	st := openTestStore(t)
	srv, _ := New(st, noopEB(), func() bool { return true }, func(context.Context) error { return nil }, TemplateFS)
	w := post(t, srv, "/test-email", nil)
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("expected ok:true, got %s", w.Body.String())
	}
}

func TestHandleTestEmail_POST_failure(t *testing.T) {
	st := openTestStore(t)
	srv, _ := New(st, noopEB(), func() bool { return true }, func(context.Context) error { return fmt.Errorf("smtp down") }, TemplateFS)
	w := post(t, srv, "/test-email", nil)
	if !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Errorf("expected ok:false, got %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "smtp down") {
		t.Errorf("expected error message in response, got %s", w.Body.String())
	}
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

// --- handleSBOM -------------------------------------------------------------

const testSBOMJSON = `{
	"bomFormat": "CycloneDX",
	"specVersion": "1.5",
	"version": 1,
	"components": [
		{"type":"library","name":"github.com/example/lib","version":"v1.2.3","purl":"pkg:golang/github.com/example/lib@v1.2.3","licenses":[{"license":{"id":"MIT"}}]},
		{"type":"library","name":"github.com/other/pkg","version":"v0.4.0","purl":"pkg:golang/github.com/other/pkg@v0.4.0"},
		{"type":"library","name":"ca-certificates","version":"20240226-r0","purl":"pkg:apk/alpine/ca-certificates@20240226-r0","licenses":[{"license":{"name":"MPL-2.0"}}]},
		{"type":"library","name":"tzdata","version":"2024a-r0","purl":"pkg:apk/alpine/tzdata@2024a-r0"}
	]
}`

func withSBOMFile(t *testing.T, content string) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "sbom.cdx.json")
	if err := os.WriteFile(f, []byte(content), 0644); err != nil {
		t.Fatalf("write sbom: %v", err)
	}
	old := SBOMPath
	SBOMPath = f
	t.Cleanup(func() { SBOMPath = old })
}

func TestHandleSBOM_GET_validFile(t *testing.T) {
	srv, _ := newTestServer(t)
	withSBOMFile(t, testSBOMJSON)

	w := get(t, srv, "/sbom")
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "github.com/example/lib") {
		t.Error("expected Go module name in output")
	}
	if !strings.Contains(body, "ca-certificates") {
		t.Error("expected OS package name in output")
	}
	if !strings.Contains(body, "MIT") {
		t.Error("expected license in output")
	}
	if !strings.Contains(body, "CycloneDX 1.5") {
		t.Error("expected format string in output")
	}
}

func TestHandleSBOM_GET_missingFile(t *testing.T) {
	srv, _ := newTestServer(t)
	old := SBOMPath
	SBOMPath = filepath.Join(t.TempDir(), "nonexistent.json")
	t.Cleanup(func() { SBOMPath = old })

	w := get(t, srv, "/sbom")
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200 (renders info message)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "not available") {
		t.Error("expected 'not available' message")
	}
}

func TestHandleSBOM_GET_invalidJSON(t *testing.T) {
	srv, _ := newTestServer(t)
	withSBOMFile(t, "not valid json")

	w := get(t, srv, "/sbom")
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200 (renders error)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Failed to parse") {
		t.Error("expected parse error message")
	}
}

func TestHandleSBOM_GET_emptyComponents(t *testing.T) {
	srv, _ := newTestServer(t)
	withSBOMFile(t, `{"bomFormat":"CycloneDX","specVersion":"1.5","version":1,"components":[]}`)

	w := get(t, srv, "/sbom")
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "not available") {
		t.Error("should not show error for empty but valid SBOM")
	}
	if !strings.Contains(body, "CycloneDX 1.5") {
		t.Error("expected format string even with no components")
	}
}

func TestHandleSBOM_GET_categorisation(t *testing.T) {
	srv, _ := newTestServer(t)
	withSBOMFile(t, testSBOMJSON)

	w := get(t, srv, "/sbom")
	body := w.Body.String()
	if !strings.Contains(body, "Go Dependencies") {
		t.Error("expected Go Dependencies section")
	}
	if !strings.Contains(body, "OS Packages") {
		t.Error("expected OS Packages section")
	}
}

// --- handleSBOMJSON ---------------------------------------------------------

func TestHandleSBOMJSON_validFile(t *testing.T) {
	srv, _ := newTestServer(t)
	withSBOMFile(t, testSBOMJSON)

	w := get(t, srv, "/sbom.json")
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "sbom.cdx.json") {
		t.Errorf("Content-Disposition: got %q, want attachment with sbom.cdx.json", cd)
	}
	if !strings.Contains(w.Body.String(), "CycloneDX") {
		t.Error("expected raw JSON content in response")
	}
}

func TestHandleSBOMJSON_missingFile(t *testing.T) {
	srv, _ := newTestServer(t)
	old := SBOMPath
	SBOMPath = filepath.Join(t.TempDir(), "nonexistent.json")
	t.Cleanup(func() { SBOMPath = old })

	w := get(t, srv, "/sbom.json")
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", w.Code)
	}
}

// --- componentLicense -------------------------------------------------------

func TestComponentLicense_id(t *testing.T) {
	c := cdxComponent{Licenses: []cdxLicense{{License: cdxLicenseEntry{ID: "MIT"}}}}
	if got := componentLicense(c); got != "MIT" {
		t.Errorf("got %q, want MIT", got)
	}
}

func TestComponentLicense_name(t *testing.T) {
	c := cdxComponent{Licenses: []cdxLicense{{License: cdxLicenseEntry{Name: "Apache License 2.0"}}}}
	if got := componentLicense(c); got != "Apache License 2.0" {
		t.Errorf("got %q, want Apache License 2.0", got)
	}
}

func TestComponentLicense_idPreferred(t *testing.T) {
	c := cdxComponent{Licenses: []cdxLicense{{License: cdxLicenseEntry{ID: "Apache-2.0", Name: "Apache License 2.0"}}}}
	if got := componentLicense(c); got != "Apache-2.0" {
		t.Errorf("got %q, want Apache-2.0 (ID preferred over Name)", got)
	}
}

func TestComponentLicense_empty(t *testing.T) {
	c := cdxComponent{}
	if got := componentLicense(c); got != "" {
		t.Errorf("got %q, want empty", got)
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

func TestDetectBaseURL_forwardedHeadersHonouredBehindTrustedProxy(t *testing.T) {
	t.Setenv("TRUSTED_PROXY", "true")
	st := openTestStore(t)
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Host = "internal:8443"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "myapp.example.com")
	got := detectBaseURL(req, st)
	if got != "https://myapp.example.com" {
		t.Errorf("got %q, want https://myapp.example.com", got)
	}
	stored, _ := st.GetSetting("eb_base_url")
	if stored != "https://myapp.example.com" {
		t.Errorf("stored: got %q", stored)
	}
}

func TestDetectBaseURL_forwardedHeadersIgnoredByDefault(t *testing.T) {
	t.Setenv("TRUSTED_PROXY", "")
	st := openTestStore(t)
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Host = "192.168.1.50:8443"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "bankingsync-renew.evil")
	got := detectBaseURL(req, st)
	if strings.Contains(got, "evil") {
		t.Errorf("got %q — an untrusted X-Forwarded-Host must never reach the session expiry email", got)
	}
	if got != "http://192.168.1.50:8443" {
		t.Errorf("got %q, want the real request host", got)
	}
	stored, _ := st.GetSetting("eb_base_url")
	if strings.Contains(stored, "evil") {
		t.Errorf("attacker-controlled host was persisted: %q", stored)
	}
}

func TestDetectBaseURL_rejectsMalformedForwardedHost(t *testing.T) {
	t.Setenv("TRUSTED_PROXY", "true")
	st := openTestStore(t)
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Host = "internal:8443"
	req.Header.Set("X-Forwarded-Host", "evil.example/path?x=1")
	if got := detectBaseURL(req, st); got != "http://internal:8443" {
		t.Errorf("got %q, want the request host when the forwarded host is malformed", got)
	}
}

func TestDetectBaseURL_rejectsNonHTTPScheme(t *testing.T) {
	t.Setenv("TRUSTED_PROXY", "true")
	st := openTestStore(t)
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Host = "internal:8443"
	req.Header.Set("X-Forwarded-Proto", "javascript")
	got := detectBaseURL(req, st)
	if strings.HasPrefix(got, "javascript") {
		t.Errorf("got %q — only http and https are acceptable schemes", got)
	}
}

func TestSameOrigin_allowsMatchingOrigin(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/sync/now", nil)
	req.Host = "nas.local:8443"
	req.Header.Set("Origin", "https://nas.local:8443")
	if !requestIsSameOrigin(req) {
		t.Error("a same-origin POST must be allowed")
	}
}

func TestSameOrigin_rejectsForeignOrigin(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/remove-account", nil)
	req.Host = "nas.local:8443"
	req.Header.Set("Origin", "https://evil.example")
	if requestIsSameOrigin(req) {
		t.Error("a cross-origin POST must be rejected")
	}
}

func TestSameOrigin_rejectsForeignReferer(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/setup", nil)
	req.Host = "nas.local:8443"
	req.Header.Set("Referer", "https://evil.example/drive-by.html")
	if requestIsSameOrigin(req) {
		t.Error("a cross-origin referer must be rejected")
	}
}

func TestSameOrigin_allowsHeaderlessRequest(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/sync/now", nil)
	req.Host = "nas.local:8443"
	if !requestIsSameOrigin(req) {
		t.Error("a curl-style POST with no Origin or Referer must still work")
	}
}

func TestSameOriginMiddleware_blocksCrossOriginPost(t *testing.T) {
	called := false
	h := sameOriginMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))

	req, _ := http.NewRequest(http.MethodPost, "/remove-account", nil)
	req.Host = "nas.local:8443"
	req.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if called {
		t.Error("the handler must not run for a cross-origin POST")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("got %d, want 403", w.Code)
	}
}

func TestSameOriginMiddleware_allowsGet(t *testing.T) {
	called := false
	h := sameOriginMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))

	req, _ := http.NewRequest(http.MethodGet, "/status", nil)
	req.Host = "nas.local:8443"
	req.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !called {
		t.Error("GET requests must pass through unaffected")
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
	_, _ = st.AddBankAccount(store.NewBankAccount{SessionID: "sess", AccountUID: "acct", BankName: "Bank", BankCountry: "DE", SessionExpiry: "2025-01-01T00:00:00Z"})
	if !srv.isConnected() {
		t.Error("expected true when account exists")
	}
}

// --- ASPSP cache refresh / auto-invalidation --------------------------------

// testRSAKeyPEM generates a fresh RSA private key encoded as PKCS1 PEM, so the
// Enable Banking client can sign its JWT auth headers in tests.
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

func TestIsWrongASPSPError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", fmt.Errorf("connection refused"), false},
		{"other http error", fmt.Errorf("POST /auth HTTP 500: internal"), false},
		{"machine code", fmt.Errorf(`POST /auth HTTP 422: {"error":"WRONG_ASPSP_PROVIDED"}`), true},
		{"full body", fmt.Errorf(`POST /auth HTTP 422: {"code":422,"message":"Wrong ASPSP name provided","error":"WRONG_ASPSP_PROVIDED","detail":null}`), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isWrongASPSPError(c.err); got != c.want {
				t.Errorf("isWrongASPSPError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestHumanizeAge(t *testing.T) {
	cases := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"future/skew", -5 * time.Minute, "just now"},
		{"seconds", 30 * time.Second, "just now"},
		{"one minute", time.Minute, "1 minute ago"},
		{"minutes", 42 * time.Minute, "42 minutes ago"},
		{"one hour", time.Hour, "1 hour ago"},
		{"hours", 6 * time.Hour, "6 hours ago"},
		{"one day", 24 * time.Hour, "1 day ago"},
		{"days", 3 * 24 * time.Hour, "3 days ago"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := humanizeAge(c.d); got != c.want {
				t.Errorf("humanizeAge(%v) = %q, want %q", c.d, got, c.want)
			}
		})
	}
}

func TestASPSPCacheAge(t *testing.T) {
	srv, st := newTestServer(t)

	// No timestamp → empty.
	if got := srv.aspspCacheAge(); got != "" {
		t.Errorf("no cache: got %q, want empty", got)
	}

	// Unparseable timestamp → empty.
	_ = st.SetSetting("aspsp_cache_at", "not-a-time")
	if got := srv.aspspCacheAge(); got != "" {
		t.Errorf("bad timestamp: got %q, want empty", got)
	}

	// A timestamp ~3 hours old → "3 hours ago".
	_ = st.SetSetting("aspsp_cache_at", time.Now().UTC().Add(-3*time.Hour).Format(time.RFC3339))
	if got := srv.aspspCacheAge(); got != "3 hours ago" {
		t.Errorf("3h old: got %q, want %q", got, "3 hours ago")
	}
}

func TestInvalidateASPSPCache_clearsKeys(t *testing.T) {
	srv, st := newTestServer(t)
	_ = st.SetSetting("aspsp_cache", `[{"name":"X","country":"DE"}]`)
	_ = st.SetSetting("aspsp_cache_at", "2099-01-01T00:00:00Z")

	srv.invalidateASPSPCache()

	if v, _ := st.GetSetting("aspsp_cache"); v != "" {
		t.Errorf("aspsp_cache: got %q, want empty", v)
	}
	if v, _ := st.GetSetting("aspsp_cache_at"); v != "" {
		t.Errorf("aspsp_cache_at: got %q, want empty", v)
	}
}

func TestHandleRefreshBanks_GET_returns404(t *testing.T) {
	srv, _ := newTestServer(t)
	w := get(t, srv, "/refresh-banks")
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", w.Code)
	}
}

func TestHandleRefreshBanks_POST_clearsCacheAndRedirects(t *testing.T) {
	srv, st := newTestServer(t)
	_ = st.SetSetting("aspsp_cache", `[{"name":"StaleBank","country":"DE"}]`)
	_ = st.SetSetting("aspsp_cache_at", "2099-01-01T00:00:00Z")

	w := post(t, srv, "/refresh-banks", nil)

	if w.Code != http.StatusFound {
		t.Fatalf("got %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/connect" {
		t.Errorf("Location: got %q, want /connect", loc)
	}
	if v, _ := st.GetSetting("aspsp_cache"); v != "" {
		t.Errorf("aspsp_cache not cleared: got %q", v)
	}
	if v, _ := st.GetSetting("aspsp_cache_at"); v != "" {
		t.Errorf("aspsp_cache_at not cleared: got %q", v)
	}
}

func TestHandleConnect_GET_showsRefreshButtonAndCacheAge(t *testing.T) {
	srv, st := newTestServer(t)
	banks := []enablebanking.ASPSP{{Name: "TestBank", Country: "DE"}}
	bj, _ := json.Marshal(banks)
	_ = st.SetSetting("aspsp_cache", string(bj))
	// Recent enough to stay valid (<24h) and to render a concrete age.
	_ = st.SetSetting("aspsp_cache_at", time.Now().UTC().Add(-2*time.Hour).Format(time.RFC3339))
	_ = st.SetSetting("eb_pem_content", "pem")

	body := get(t, srv, "/connect").Body.String()
	if !strings.Contains(body, `action="/refresh-banks"`) {
		t.Error("expected a refresh-banks form on the connect page")
	}
	if !strings.Contains(body, "Refresh list") {
		t.Error("expected a 'Refresh list' button on the connect page")
	}
	if !strings.Contains(body, "Bank list updated 2 hours ago") {
		t.Error("expected the cache age to be surfaced on the connect page")
	}
}

// TestHandleConnect_POST_wrongASPSP_invalidatesCache is an end-to-end test: a
// POST to /connect drives the real Enable Banking client against a mock server
// that returns 422 WRONG_ASPSP_PROVIDED on /auth. The handler must then bust the
// stale cache and re-fetch the live list before re-rendering.
func TestHandleConnect_POST_wrongASPSP_invalidatesCache(t *testing.T) {
	st := openTestStore(t)

	// Seed a far-future cache holding a stale bank the live catalog rejects.
	staleJSON, _ := json.Marshal([]enablebanking.ASPSP{{Name: "StaleBank", Country: "DE"}})
	_ = st.SetSetting("aspsp_cache", string(staleJSON))
	_ = st.SetSetting("aspsp_cache_at", "2099-01-01T00:00:00Z")
	_ = st.SetSetting("eb_pem_content", "pem")

	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"code":422,"message":"Wrong ASPSP name provided","error":"WRONG_ASPSP_PROVIDED","detail":null}`))
	})
	mux.HandleFunc("/aspsps", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"aspsps":[{"name":"FreshBank","country":"DE"}]}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	keyPEM := testRSAKeyPEM(t)
	eb := enablebanking.NewClient(
		func() (string, error) { return "test-app-id", nil },
		func() ([]byte, error) { return keyPEM, nil },
		nil,
		enablebanking.WithBaseURL(ts.URL),
	)
	srv, err := New(st, eb, func() bool { return true }, nil, TemplateFS)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	w := post(t, srv, "/connect", url.Values{
		"bank_name":    {"StaleBank"},
		"bank_country": {"DE"},
		"psu_type":     {"personal"},
	})

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}

	// Cache must have been invalidated and then re-fetched live → now FreshBank.
	cache, _ := st.GetSetting("aspsp_cache")
	if !strings.Contains(cache, "FreshBank") {
		t.Errorf("cache not refreshed from live catalog: got %q", cache)
	}
	if strings.Contains(cache, "StaleBank") {
		t.Errorf("stale bank still cached: %q", cache)
	}

	body := w.Body.String()
	if !strings.Contains(body, "FreshBank") {
		t.Error("expected refreshed bank list in re-rendered page")
	}
	if !strings.Contains(body, "WRONG_ASPSP_PROVIDED") {
		t.Error("expected a WRONG_ASPSP_PROVIDED explanation in the error message")
	}
}

func TestValidatePrivateKeyPEM_acceptsRSAKey(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := validatePrivateKeyPEM(data); err != nil {
		t.Errorf("a valid RSA key must be accepted: %v", err)
	}
}

func TestValidatePrivateKeyPEM_rejectsPublicKey(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	data := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	if err := validatePrivateKeyPEM(data); err == nil {
		t.Error("uploading public.pem by mistake must be rejected at upload time")
	}
}

func TestValidatePrivateKeyPEM_rejectsCorruptBody(t *testing.T) {
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("not a key")})
	if err := validatePrivateKeyPEM(data); err == nil {
		t.Error("a well-framed but unparseable key must be rejected, not stored and failed hours later")
	}
}

func TestValidatePrivateKeyPEM_rejectsNonPEM(t *testing.T) {
	if err := validatePrivateKeyPEM([]byte("just some bytes")); err == nil {
		t.Error("non-PEM input must be rejected")
	}
}

func TestValidatePrivateKeyPEM_rejectsPaddedGarbage(t *testing.T) {
	data := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: make([]byte, 4096)})
	if err := validatePrivateKeyPEM(data); err == nil {
		t.Error("a PEM-framed blob that is not a key must be rejected")
	}
}

func TestHandleSetup_POST_oversizedUploadIsRejectedClearly(t *testing.T) {
	srv, _ := newTestServer(t)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("pem_file", "private.pem")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(bytes.Repeat([]byte("A"), maxPEMUploadBytes+1024)); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost, "/setup", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "smaller than") {
		t.Errorf("an oversized upload must say so, got: %s", w.Body.String())
	}
}

func TestIsRequestTooLarge(t *testing.T) {
	if !isRequestTooLarge(&http.MaxBytesError{Limit: 10}) {
		t.Error("a MaxBytesError must be recognised")
	}
	if isRequestTooLarge(fmt.Errorf("something else")) {
		t.Error("an unrelated error must not be treated as an oversized request")
	}
}

func TestHandlePickAccount_POST_persistsIBANAndCurrency(t *testing.T) {
	srv, st := newTestServer(t)
	accts := []enablebanking.SessionAccount{
		{UID: "uid-1", IBAN: "DE63111111111111111111", Currency: "EUR"},
		{UID: "uid-2", IBAN: "DE90222222222222222222", Currency: "CZK"},
	}
	data, _ := json.Marshal(accts)
	_ = st.SetSetting("pending_auth_session_id", "sess-1")
	_ = st.SetSetting("pending_auth_accounts", string(data))
	_ = st.SetSetting("pending_auth_expiry", "2027-01-01T00:00:00Z")
	_ = st.SetSetting("pending_auth_bank_name", "TestBank")
	_ = st.SetSetting("pending_auth_bank_country", "DE")

	w := post(t, srv, "/pick-account", url.Values{
		"account_uid":    {"uid-2"},
		"actual_account": {"MyChecking"},
	})
	if w.Code != http.StatusFound {
		t.Fatalf("got %d, want 302", w.Code)
	}

	accounts, _ := st.GetAllBankAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accounts))
	}
	if got, want := accounts[0].IBAN, "DE90222222222222222222"; got != want {
		t.Errorf("IBAN: got %q, want %q — the picked account's IBAN, not the first one", got, want)
	}
	if got, want := accounts[0].Currency, "CZK"; got != want {
		t.Errorf("Currency: got %q, want %q", got, want)
	}
}

func TestHandlePickAccount_POST_missingIBANIsNotFatal(t *testing.T) {
	srv, st := newTestServer(t)
	accts := []enablebanking.SessionAccount{{UID: "uid-1"}}
	data, _ := json.Marshal(accts)
	_ = st.SetSetting("pending_auth_session_id", "sess-1")
	_ = st.SetSetting("pending_auth_accounts", string(data))
	_ = st.SetSetting("pending_auth_expiry", "2027-01-01T00:00:00Z")

	w := post(t, srv, "/pick-account", url.Values{"account_uid": {"uid-1"}})
	if w.Code != http.StatusFound {
		t.Fatalf("banks that supply no IBAN must still be connectable: got %d", w.Code)
	}
	accounts, _ := st.GetAllBankAccounts()
	if len(accounts) != 1 || accounts[0].IBAN != "" {
		t.Errorf("expected one account with an empty IBAN, got %+v", accounts)
	}
}

func TestAccountDetails_matchesViaEffectiveUID(t *testing.T) {
	accounts := []enablebanking.SessionAccount{
		{AccountUID: "acct-uid", IBAN: "DE20333333333333333333", Currency: "EUR"},
		{ResourceID: "res-id", IBAN: "DE47444444444444444444", Currency: "USD"},
	}
	if iban, cur := accountDetails(accounts, "acct-uid"); iban != "DE20333333333333333333" || cur != "EUR" {
		t.Errorf("account_uid fallback: got %q / %q", iban, cur)
	}
	if iban, cur := accountDetails(accounts, "res-id"); iban != "DE47444444444444444444" || cur != "USD" {
		t.Errorf("resource_id fallback: got %q / %q", iban, cur)
	}
	if iban, cur := accountDetails(accounts, "unknown"); iban != "" || cur != "" {
		t.Errorf("unknown uid must yield empty, got %q / %q", iban, cur)
	}
}

func TestHandleHealth_reportsTheBackend(t *testing.T) {
	srv, st := newTestServer(t)
	_, _ = st.AddBankAccount(store.NewBankAccount{
		SessionID: "sess", AccountUID: "acct", BankName: "Bank", BankCountry: "DE",
		SessionExpiry: "2027-01-01T00:00:00Z",
	})
	_ = st.SetLastSyncDate("2026-08-01")

	srv.SetBackendStatus(func() BackendStatus {
		return BackendStatus{Name: "firefly", Version: "6.6.6", Reachable: true, CheckedAt: time.Now().UTC()}
	})

	w := get(t, srv, "/health")
	body := w.Body.String()
	if !strings.Contains(body, `"name":"firefly"`) {
		t.Errorf("the active backend must be visible, got %s", body)
	}
	if !strings.Contains(body, `"version":"6.6.6"`) {
		t.Errorf("the backend version belongs in health, got %s", body)
	}
}

func TestHandleHealth_unreachableBackendDegrades(t *testing.T) {
	srv, st := newTestServer(t)
	_, _ = st.AddBankAccount(store.NewBankAccount{
		SessionID: "sess", AccountUID: "acct", BankName: "Bank", BankCountry: "DE",
		SessionExpiry: "2027-01-01T00:00:00Z",
	})
	_ = st.SetLastSyncDate("2026-08-01")

	srv.SetBackendStatus(func() BackendStatus {
		return BackendStatus{Name: "firefly", Reachable: false, CheckedAt: time.Now().UTC()}
	})

	w := get(t, srv, "/health")
	if !strings.Contains(w.Body.String(), `"status":"degraded"`) {
		t.Errorf("an unreachable backend must degrade health, got %s", w.Body.String())
	}
}

func TestHandleHealth_withoutProviderStillAnswers(t *testing.T) {
	srv, st := newTestServer(t)
	_, _ = st.AddBankAccount(store.NewBankAccount{
		SessionID: "sess", AccountUID: "acct", BankName: "Bank", BankCountry: "DE",
		SessionExpiry: "2027-01-01T00:00:00Z",
	})
	_ = st.SetLastSyncDate("2026-08-01")

	w := get(t, srv, "/health")
	if w.Code != http.StatusOK {
		t.Fatalf("health must not depend on a backend provider being wired, got %d", w.Code)
	}
}

func TestStatusPage_namesTheActiveBackend(t *testing.T) {
	srv, st := newTestServer(t)
	_ = st.SetSetting("budget_backend", "firefly")

	w := get(t, srv, "/status")
	if !strings.Contains(w.Body.String(), "Firefly III") {
		t.Errorf("a Firefly user must not be told about Actual Budget:\n%s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "Actual Budget") {
		t.Error("the status page still mentions Actual Budget")
	}
}

func TestPickAccount_offersKnownAccountsAndNamesTheBackend(t *testing.T) {
	srv, st := newTestServer(t)
	_ = st.SetSetting("budget_backend", "firefly")
	_, _ = st.AddBankAccount(store.NewBankAccount{
		SessionID: "s", AccountUID: "a", BankName: "Bank", BankCountry: "DE",
		ActualAccount: "Girokonto", SessionExpiry: "2027-01-01T00:00:00Z",
	})
	_ = st.SetSetting("pending_auth_session_id", "sess-1")
	_ = st.SetSetting("pending_auth_accounts", `[{"uid":"uid-1"}]`)

	w := get(t, srv, "/pick-account")
	body := w.Body.String()
	if !strings.Contains(body, "Firefly III Account") {
		t.Errorf("the field label must name the active backend:\n%s", body)
	}
	if !strings.Contains(body, `<option value="Girokonto">`) {
		t.Error("existing target accounts must be offered, a retyped name silently creates a second account")
	}
}

func TestBackendLabel_fallsBackWithoutProvider(t *testing.T) {
	srv, st := newTestServer(t)
	if got := srv.BackendLabel(); got != "your budget" {
		t.Errorf("unknown backend: got %q", got)
	}
	_ = st.SetSetting("budget_backend", "actual")
	if got := srv.BackendLabel(); got != "Actual Budget" {
		t.Errorf("got %q, want Actual Budget", got)
	}
}

func TestBackendLabel_prefersTheLiveProvider(t *testing.T) {
	srv, st := newTestServer(t)
	_ = st.SetSetting("budget_backend", "actual")
	srv.SetBackendStatus(func() BackendStatus { return BackendStatus{Name: "firefly"} })

	if got := srv.BackendLabel(); got != "Firefly III" {
		t.Errorf("the live backend must win over the persisted setting, got %q", got)
	}
}

func TestSettings_savesAndRejects(t *testing.T) {
	srv, st := newTestServer(t)

	good := url.Values{
		store.SettingTolerancePercent:  {"30"},
		store.SettingToleranceCents:    {"7500"},
		store.SettingDriftNotifyCents:  {"250"},
		store.SettingPayeePrefixes:     {"VISA, VPAY"},
		store.SettingAutoProbability:   {"90"},
		store.SettingReviewProbability: {"50"},
		store.SettingMatchOverlap:      {"50"},
	}
	if w := post(t, srv, "/settings", good); w.Code != http.StatusOK {
		t.Fatalf("save: got %d, want 200", w.Code)
	}
	tun := st.Tunables()
	if tun.TolerancePercent != 30 || tun.ToleranceCents != 7500 || tun.DriftNotifyCents != 250 {
		t.Errorf("settings not stored: %+v", tun)
	}

	bad := url.Values{
		store.SettingTolerancePercent:  {"500"},
		store.SettingToleranceCents:    {"7500"},
		store.SettingDriftNotifyCents:  {"250"},
		store.SettingPayeePrefixes:     {"VISA"},
		store.SettingAutoProbability:   {"90"},
		store.SettingReviewProbability: {"50"},
		store.SettingMatchOverlap:      {"50"},
	}
	w := post(t, srv, "/settings", bad)
	if !strings.Contains(w.Body.String(), "percentage") {
		t.Error("an out-of-range percentage was not rejected with an explanation")
	}
	if got := st.Tunables().TolerancePercent; got != 30 {
		t.Errorf("a rejected form still changed the stored value to %d", got)
	}
}

func TestOpeningBalance_doubleSubmitAppliesOnce(t *testing.T) {
	srv, _ := newTestServer(t)

	var applies int
	srv.SetOpeningBalanceFunc(func(_ context.Context, id int64, apply bool, _ *int64) (OpeningBalancePreview, error) {
		if apply {
			applies++
		}
		return OpeningBalancePreview{AccountID: id, OpeningCents: 1000, Applied: apply}, nil
	})

	form := url.Values{"account_id": {"1"}, "expected_cents": {"1000"}}
	for i := 0; i < 3; i++ {
		if w := post(t, srv, "/opening-balance", form); w.Code != http.StatusFound {
			t.Fatalf("submit %d: got %d, want a redirect", i, w.Code)
		}
	}
	if applies != 3 {
		t.Fatalf("apply calls: got %d, want 3 — the handler must pass every submit "+
			"through; the idempotency lives in the store claim, not here", applies)
	}
}

func TestOpeningBalance_showsTheRefusalInsteadOfWriting(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetOpeningBalanceFunc(func(_ context.Context, id int64, _ bool, _ *int64) (OpeningBalancePreview, error) {
		return OpeningBalancePreview{AccountID: id, Refusal: "no booked balance type available"}, nil
	})

	w := get(t, srv, "/opening-balance?account_id=1")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no booked balance type") {
		t.Error("the refusal reason was not shown to the user")
	}
	if strings.Contains(w.Body.String(), "Write it") {
		t.Error("the apply button was offered despite a refusal")
	}
}

func TestOpeningBalance_passesTheExpectedValueThrough(t *testing.T) {
	srv, _ := newTestServer(t)
	var seen *int64
	srv.SetOpeningBalanceFunc(func(_ context.Context, id int64, _ bool, expected *int64) (OpeningBalancePreview, error) {
		seen = expected
		return OpeningBalancePreview{AccountID: id, Applied: true}, nil
	})

	post(t, srv, "/opening-balance", url.Values{"account_id": {"1"}, "expected_cents": {"4242"}})
	if seen == nil || *seen != 4242 {
		t.Fatalf("expected_cents: got %v, want 4242 — without it a stale confirmation "+
			"page writes an outdated figure", seen)
	}
}

func TestHealth_reportsDriftAndDegrades(t *testing.T) {
	srv, st := newTestServer(t)
	id, err := st.AddBankAccount(store.NewBankAccount{
		SessionID: "s", AccountUID: "u", BankName: "TestBank", BankCountry: "DE",
		SessionExpiry: time.Now().UTC().AddDate(0, 0, 90).Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("AddBankAccount: %v", err)
	}
	if err := st.SetAccountDrift(id, -4250, store.DriftAlert); err != nil {
		t.Fatalf("SetAccountDrift: %v", err)
	}
	// Without a successful sync the endpoint is already unhealthy, which would
	// mask the escalation this test is about.
	if _, err := st.AddSyncLog("success", 1, 0, 0, 0.1, ""); err != nil {
		t.Fatalf("AddSyncLog: %v", err)
	}
	if err := st.SetLastSyncDate(time.Now().UTC().Format("2006-01-02")); err != nil {
		t.Fatalf("SetLastSyncDate: %v", err)
	}

	w := get(t, srv, "/health")
	var body struct {
		Status string `json:"status"`
		Drift  struct {
			AccountsDrifting int    `json:"accounts_drifting"`
			MaxAbsCents      int64  `json:"max_abs_cents"`
			WorstAccount     string `json:"worst_account"`
		} `json:"drift"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /health: %v", err)
	}
	if body.Drift.AccountsDrifting != 1 || body.Drift.MaxAbsCents != 4250 {
		t.Errorf("drift block: %+v", body.Drift)
	}
	if body.Drift.WorstAccount != "TestBank" {
		t.Errorf("worst_account: got %q", body.Drift.WorstAccount)
	}
	if body.Status != "degraded" {
		t.Errorf("status: got %q, want degraded when an account is drifting", body.Status)
	}
}

// The account table grew two columns for the balance features. With
// overflow:hidden on the wrapper the extra width clipped the right-hand edge and
// took the row actions with it, leaving no way to renew or remove an account.
func TestStatusTable_actionsCannotBeClippedOff(t *testing.T) {
	srv, st := newTestServer(t)
	if _, err := st.AddBankAccount(store.NewBankAccount{
		SessionID: "s", AccountUID: "u", BankName: "TestBank", BankCountry: "DE",
		ActualAccount: "Checking", IBAN: "DE31123456789005193987", Currency: "EUR",
		SessionExpiry: time.Now().UTC().AddDate(0, 0, 90).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("AddBankAccount: %v", err)
	}

	body := get(t, srv, "/status").Body.String()

	for _, want := range []string{"/renew", "/reset-sync", "/remove-account"} {
		if !strings.Contains(body, want) {
			t.Errorf("the %s action is missing from the account row", want)
		}
	}
	if !strings.Contains(body, `<td class="actions-cell">`) {
		t.Error("the action cell does not carry actions-cell, so the table squeezes " +
			"it instead of the text columns")
	}
	if !strings.Contains(body, "td.actions-cell,th.actions-cell{width:1%") {
		t.Error("the stylesheet does not reserve the action column's natural width, " +
			"so the table squeezes the buttons rather than the text columns")
	}
	if strings.Contains(body, ".tbl-wrap{border:1px solid var(--border);border-radius:var(--r);overflow:hidden") {
		t.Error("the table wrapper still clips overflow; wide tables lose their " +
			"right-hand column instead of scrolling")
	}
	if !strings.Contains(body, "overflow-x:auto") {
		t.Error("the table wrapper does not allow horizontal overflow")
	}
}

// fakeReviewQueue records what the handler asked for, so the tests can pin the
// contract between the page and the syncer rather than the syncer itself.
type fakeReviewQueue struct {
	items []ReviewItem
	err   error

	gotID      int64
	gotCand    string
	gotPercent int
	gotVersion string
	calls      int
	resolveErr error

	inquiry    *InquiryItem
	inquiryErr error

	answerID      int64
	answer        *bool
	answerVersion string
	answerCalls   int
	answerErr     error
}

func (q *fakeReviewQueue) HeldTransactions(context.Context) ([]ReviewItem, error) {
	return q.items, q.err
}

func (q *fakeReviewQueue) ResolveHeld(
	_ context.Context, id int64, cand string, pct int, version string,
) error {
	q.calls++
	q.gotID, q.gotCand, q.gotPercent, q.gotVersion = id, cand, pct, version
	return q.resolveErr
}

func (q *fakeReviewQueue) PendingInquiry(context.Context) (*InquiryItem, error) {
	return q.inquiry, q.inquiryErr
}

func (q *fakeReviewQueue) AnswerInquiry(
	_ context.Context, id int64, answer *bool, version string,
) error {
	q.answerCalls++
	q.answerID, q.answer, q.answerVersion = id, answer, version
	return q.answerErr
}

func TestReview_listsHeldTransactionsWithTheirReasons(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetReviewQueue(&fakeReviewQueue{items: []ReviewItem{{
		ID: 7, BankName: "TestBank", Date: "2026-08-25", Amount: "-9.99",
		Currency: "EUR", Payee: "Visa Da Luigi",
		Candidates: []ReviewCandidate{{
			ID: "txn-1", Date: "2026-08-23", Amount: "-8.99", PayeeName: "Da Luigi Roma",
			Percent: 71, Why: "amount within tolerance · payee cut short · 2 days apart",
		}},
	}}})

	w := get(t, srv, "/review")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Visa Da Luigi", "Da Luigi Roma", "71", "payee cut short",
		`value="txn-1"`, `name="review_id" value="7"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}
	// Without this the page can only merge, and a transaction that is genuinely
	// new could never leave the queue.
	if !strings.Contains(body, `name="candidate_id" value=""`) {
		t.Error("the page offers no way to say the transaction is new")
	}
}

func TestReview_saysSoWhenNothingIsWaiting(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetReviewQueue(&fakeReviewQueue{})

	w := get(t, srv, "/review")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Nothing waiting") {
		t.Error("an empty queue must say so rather than render a blank page")
	}
}

// TestReview_stillListsWhatItCannotOfferCandidatesFor is the guarantee the whole
// queue rests on: a held transaction is not in the budget, so a page that quietly
// omitted it would leave the money nowhere at all.
func TestReview_stillListsWhatItCannotOfferCandidatesFor(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetReviewQueue(&fakeReviewQueue{items: []ReviewItem{{
		ID: 3, BankName: "TestBank", Date: "2026-08-25", Amount: "-9.99", Payee: "Netflix",
		Unavailable: "the budget backend is unreachable",
	}}})

	body := get(t, srv, "/review").Body.String()
	if !strings.Contains(body, "Netflix") {
		t.Error("a held transaction disappeared from the page because its candidates could not be listed")
	}
	if !strings.Contains(body, "the budget backend is unreachable") {
		t.Error("the reason is not shown, so the row looks like a matcher failure rather than an outage")
	}
}

func TestReview_passesTheChoiceAndTheFigureItWasMadeOn(t *testing.T) {
	srv, _ := newTestServer(t)
	q := &fakeReviewQueue{}
	srv.SetReviewQueue(q)

	w := post(t, srv, "/review/resolve", url.Values{
		"review_id": {"7"}, "candidate_id": {"txn-1"}, "shown_percent": {"71"},
	})
	if w.Code != http.StatusFound {
		t.Fatalf("got %d, want a redirect", w.Code)
	}
	if q.gotID != 7 || q.gotCand != "txn-1" || q.gotPercent != 71 {
		t.Errorf("handler passed id=%d candidate=%q percent=%d", q.gotID, q.gotCand, q.gotPercent)
	}
}

// TestReview_anEmptyChoiceMeansNewAndNeedsNoFigure separates the two cases the
// hidden field cannot distinguish on its own. "It is new" is a decision about
// nothing in the budget, so there is no probability to have gone stale, and
// demanding one would make the commonest answer the one that fails.
func TestReview_anEmptyChoiceMeansNewAndNeedsNoFigure(t *testing.T) {
	srv, _ := newTestServer(t)
	q := &fakeReviewQueue{}
	srv.SetReviewQueue(q)

	w := post(t, srv, "/review/resolve", url.Values{
		"review_id": {"7"}, "candidate_id": {""}, "shown_percent": {""},
	})
	if w.Code != http.StatusFound {
		t.Fatalf("got %d, want a redirect", w.Code)
	}
	if q.calls != 1 || q.gotCand != "" {
		t.Errorf("calls=%d candidate=%q; choosing \"new\" must reach the queue", q.calls, q.gotCand)
	}
}

func TestReview_showsARefusalInsteadOfSwallowingIt(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetReviewQueue(&fakeReviewQueue{
		resolveErr: errors.New("that match is now 55%, not 71%"),
		items:      []ReviewItem{{ID: 7, Payee: "Netflix", Amount: "-9.99"}},
	})

	w := post(t, srv, "/review/resolve", url.Values{
		"review_id": {"7"}, "candidate_id": {"txn-1"}, "shown_percent": {"71"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want the page back with the reason", w.Code)
	}
	if !strings.Contains(w.Body.String(), "that match is now 55%, not 71%") {
		t.Error("the refusal is not shown, so the user would think the decision was applied")
	}
}

// TestReview_resolveIsPostOnly matches the rest of the app: a GET to an action
// route is a 404, not a 405. Anything that changes the budget must not be
// reachable by following a link.
func TestReview_resolveIsPostOnly(t *testing.T) {
	srv, _ := newTestServer(t)
	q := &fakeReviewQueue{}
	srv.SetReviewQueue(q)

	if w := get(t, srv, "/review/resolve?review_id=7&candidate_id=txn-1"); w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", w.Code)
	}
	if q.calls != 0 {
		t.Error("a GET reached the queue")
	}
}

func TestReview_isUnavailableRatherThanBlankWithoutAQueue(t *testing.T) {
	srv, _ := newTestServer(t)

	if w := get(t, srv, "/review"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503", w.Code)
	}
	if w := post(t, srv, "/review/resolve", url.Values{"review_id": {"7"}}); w.Code != http.StatusServiceUnavailable {
		t.Errorf("resolve: got %d, want 503", w.Code)
	}
}

func TestSettings_storesTheDecisionThresholds(t *testing.T) {
	srv, st := newTestServer(t)

	w := post(t, srv, "/settings", url.Values{
		"balance_drift_notify_cents":       {"1000"},
		"match_amount_tolerance_max_cents": {"5000"},
		"match_amount_tolerance_pct":       {"25"},
		"match_payee_prefixes":             {"VISA"},
		"match_auto_probability":           {"93"},
		"match_review_probability":         {"45"},
		"match_overlap_pct":                {"50"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	if got := st.Tunables(); got.AutoProbabilityPct != 93 || got.ReviewProbabilityPct != 45 {
		t.Errorf("auto %d, review %d", got.AutoProbabilityPct, got.ReviewProbabilityPct)
	}
}

// TestSettings_writesNothingWhenTheFormIsRejected is why the handler validates
// everything before it writes anything.
//
// The two thresholds constrain each other. Writing them one at a time means a
// rejected form can still have moved the first one, leaving a policy that is
// neither the old one nor the one asked for — and the field that survived is
// whichever happened to come first in a loop.
func TestSettings_writesNothingWhenTheFormIsRejected(t *testing.T) {
	srv, st := newTestServer(t)
	before := st.Tunables()

	w := post(t, srv, "/settings", url.Values{
		"balance_drift_notify_cents":       {"2500"},
		"match_amount_tolerance_max_cents": {"5000"},
		"match_amount_tolerance_pct":       {"25"},
		"match_payee_prefixes":             {"VISA"},
		"match_auto_probability":           {"60"},
		"match_review_probability":         {"80"},
		"match_overlap_pct":                {"50"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "must not be above") {
		t.Error("the crossed pair was accepted")
	}

	after := st.Tunables()
	if after.DriftNotifyCents != before.DriftNotifyCents {
		t.Errorf("an unrelated field was written anyway: %d, was %d",
			after.DriftNotifyCents, before.DriftNotifyCents)
	}
	if after.AutoProbabilityPct != before.AutoProbabilityPct {
		t.Errorf("the auto threshold was written before the form was refused: %d, was %d",
			after.AutoProbabilityPct, before.AutoProbabilityPct)
	}
}

// TestSettings_theFormIsAllOrNothing states the contract a partial POST runs
// into, because the failure it produces is otherwise baffling: adding a setting
// to the page makes every form that omits it fail whole.
//
// The single form carries every field, so this only bites a hand-made request —
// and refusing one is better than the alternative, where the missing key is
// read as an empty value and silently resets the setting.
func TestSettings_theFormIsAllOrNothing(t *testing.T) {
	srv, st := newTestServer(t)
	before := st.Tunables()

	w := post(t, srv, "/settings", url.Values{
		store.SettingTolerancePercent: {"30"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	if got := st.Tunables(); got.TolerancePercent != before.TolerancePercent {
		t.Errorf("a partial form was applied: tolerance is now %d, was %d",
			got.TolerancePercent, before.TolerancePercent)
	}
}

// healthyAccount sets up the minimum for /health to report "ok", so a test about
// one escalation is not masked by the endpoint already being unhealthy.
func healthyAccount(t *testing.T, st *store.Store) int64 {
	t.Helper()
	id, err := st.AddBankAccount(store.NewBankAccount{
		SessionID: "s", AccountUID: "u", BankName: "TestBank", BankCountry: "DE",
		ActualAccount: "Checking", Currency: "EUR",
		SessionExpiry: time.Now().UTC().AddDate(0, 0, 90).Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("AddBankAccount: %v", err)
	}
	if _, err := st.AddSyncLog("success", 1, 0, 0, 0.1, ""); err != nil {
		t.Fatalf("AddSyncLog: %v", err)
	}
	if err := st.SetLastSyncDate(time.Now().UTC().Format("2006-01-02")); err != nil {
		t.Fatalf("SetLastSyncDate: %v", err)
	}
	return id
}

func heldRow(t *testing.T, st *store.Store, acctID int64, key string) {
	t.Helper()
	if err := st.AddMatchReview(store.MatchReview{
		BankAccountID: acctID, Backend: "actual", ExternalRef: "book-" + key,
		PendingKey: key, TxnDate: "2026-08-25", AmountCents: -999,
		Currency: "EUR", Payee: "Netflix", Cleared: true, BestProbability: 0.71,
	}); err != nil {
		t.Fatalf("AddMatchReview: %v", err)
	}
}

// TestHealth_degradesWhileADecisionIsOpen makes the queue visible to monitoring
// rather than only to whoever opens the page.
//
// A held transaction is in no budget at all, so an instance with one outstanding
// is not doing the job it was installed for. Left at "ok", a queue nobody looked
// at would be indistinguishable from an empty one for as long as it went unread.
func TestHealth_degradesWhileADecisionIsOpen(t *testing.T) {
	srv, st := newTestServer(t)
	id := healthyAccount(t, st)

	var body struct {
		Status      string `json:"status"`
		ReviewsOpen int    `json:"reviews_open"`
	}
	if err := json.Unmarshal(get(t, srv, "/health").Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /health: %v", err)
	}
	if body.Status != "ok" || body.ReviewsOpen != 0 {
		t.Fatalf("setup: status %q with %d open; want a healthy instance to start from",
			body.Status, body.ReviewsOpen)
	}

	heldRow(t, st, id, "book-1")

	if err := json.Unmarshal(get(t, srv, "/health").Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /health: %v", err)
	}
	if body.ReviewsOpen != 1 {
		t.Errorf("reviews_open: got %d, want 1", body.ReviewsOpen)
	}
	if body.Status != "degraded" {
		t.Errorf("status: got %q, want degraded — a transaction in no budget is not "+
			"a healthy instance", body.Status)
	}
}

func TestStatus_pointsAtAnOpenDecision(t *testing.T) {
	srv, st := newTestServer(t)
	id := healthyAccount(t, st)

	if strings.Contains(get(t, srv, "/status").Body.String(), "waiting for a decision") {
		t.Fatal("setup: the dashboard announces a decision with an empty queue")
	}

	heldRow(t, st, id, "book-1")

	body := get(t, srv, "/status").Body.String()
	if !strings.Contains(body, "waiting for a decision") {
		t.Error("the dashboard does not mention the open decision, so the only place " +
			"it appears is a page nobody has a reason to open")
	}
	if !strings.Contains(body, `href="/review"`) {
		t.Error("no link to the queue from the page the user actually lands on")
	}
}

// logRecorder captures the structured log pipeline, so a test can assert that a
// failure was reported and not only that it was rendered.
type logRecorder struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (r *logRecorder) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }

func (r *logRecorder) OnEmit(_ context.Context, rec *sdklog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, *rec)
	return nil
}

func (r *logRecorder) Shutdown(context.Context) error   { return nil }
func (r *logRecorder) ForceFlush(context.Context) error { return nil }

func (r *logRecorder) find(body string) (sdklog.Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.records {
		if rec.Body().AsString() == body {
			return rec, true
		}
	}
	return sdklog.Record{}, false
}

func (r *logRecorder) bodies() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, rec := range r.records {
		out = append(out, rec.Body().AsString())
	}
	return out
}

func recordLogs(t *testing.T) *logRecorder {
	t.Helper()
	rec := &logRecorder{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(rec))
	prev := global.GetLoggerProvider()
	global.SetLoggerProvider(lp)
	t.Cleanup(func() { global.SetLoggerProvider(prev) })
	return rec
}

func recordAttr(rec sdklog.Record, key string) string {
	var out string
	rec.WalkAttributes(func(kv otellog.KeyValue) bool {
		if string(kv.Key) == key {
			out = kv.Value.AsString()
			return false
		}
		return true
	})
	return out
}

// TestReview_aRefusedDecisionIsReported closes the gap that made the whole
// feature's failure mode invisible.
//
// A refusal renders as a 200 with the reason on the page, which is right for a
// form the user should correct in place — but it means the request-level warning
// on 4xx and 5xx never fires. Left at that, somebody failing repeatedly to
// resolve a decision produced nothing in logs, traces or metrics at all.
func TestReview_aRefusedDecisionIsReported(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := recordLogs(t)
	srv.SetReviewQueue(&fakeReviewQueue{
		resolveErr: Refuse("that match is now 55%%, not 71%%"),
	})

	if w := post(t, srv, "/review/resolve", url.Values{
		"review_id": {"7"}, "candidate_id": {"txn-1"}, "shown_percent": {"71"},
	}); w.Code != http.StatusOK {
		t.Fatalf("got %d, want the page back", w.Code)
	}

	got, ok := rec.find("review.refused")
	if !ok {
		t.Fatalf("a refused decision left no record; got %v", rec.bodies())
	}
	if v := recordAttr(got, "op"); v != "resolve" {
		t.Errorf("op = %q, want resolve", v)
	}
	if !strings.Contains(recordAttr(got, "reason"), "55%") {
		t.Errorf("the reason does not say what was refused: %q", recordAttr(got, "reason"))
	}
	if _, wrong := rec.find("review.failed"); wrong {
		t.Error("a guard doing its job was recorded as a program failure, which is how " +
			"a working refusal ends up looking like an outage")
	}
}

// TestReview_aBrokenBackendIsReportedAsAFailure is the other half of the split.
// A refusal is the user's to fix; an unreachable backend is not, and burying one
// among the other is what makes an outage take a day to notice.
func TestReview_aBrokenBackendIsReportedAsAFailure(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := recordLogs(t)
	srv.SetReviewQueue(&fakeReviewQueue{err: errors.New("connect to the budget backend: no route to host")})

	if w := get(t, srv, "/review"); w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}

	got, ok := rec.find("review.failed")
	if !ok {
		t.Fatalf("an unreachable backend left no record; got %v", rec.bodies())
	}
	if v := recordAttr(got, "op"); v != "list" {
		t.Errorf("op = %q, want list", v)
	}
	if !strings.Contains(recordAttr(got, "error"), "no route to host") {
		t.Errorf("the cause is not recorded: %q", recordAttr(got, "error"))
	}
}

// TestRefuse_separatesWhoHasToActOnIt pins the distinction the severities rest on.
func TestRefuse_separatesWhoHasToActOnIt(t *testing.T) {
	if !IsRefusal(Refuse("look at it again")) {
		t.Error("a refusal is not recognised as one")
	}
	if IsRefusal(errors.New("no route to host")) {
		t.Error("a plain failure was taken for a refusal, which would log an outage as a warning")
	}
	if !IsRefusal(fmt.Errorf("wrapped: %w", Refuse("look again"))) {
		t.Error("a wrapped refusal is no longer recognised; the classification must survive " +
			"the error travelling up through a caller that adds context")
	}
}

// TestSettings_aPolicyChangeIsRecorded matters because two of these settings are
// matching policy. Moving the automatic threshold changes what gets merged
// without anybody being asked, and with no record there is no way to tell
// afterwards when the behaviour changed or what it changed from.
func TestSettings_aPolicyChangeIsRecorded(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := recordLogs(t)

	form := url.Values{
		store.SettingDriftNotifyCents:  {"1000"},
		store.SettingToleranceCents:    {"5000"},
		store.SettingTolerancePercent:  {"25"},
		store.SettingPayeePrefixes:     {"VISA"},
		store.SettingAutoProbability:   {"93"},
		store.SettingReviewProbability: {"50"},
		store.SettingMatchOverlap:      {"50"},
	}
	if w := post(t, srv, "/settings", form); w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}

	got, ok := rec.find("settings.changed")
	if !ok {
		t.Fatalf("a threshold moved and nothing recorded it; got %v", rec.bodies())
	}
	if v := recordAttr(got, "auto_probability_pct"); v != "90 -> 93" {
		t.Errorf("auto_probability_pct = %q, want %q — the old value is the half that "+
			"makes the record useful", v, "90 -> 93")
	}
	// The drift threshold was submitted unchanged. Recording it anyway would make
	// every save look like a policy change and the record worth ignoring.
	if v := recordAttr(got, "drift_notify_cents"); v != "" {
		t.Errorf("an unchanged setting was recorded as changed: %q", v)
	}
}

func TestSettings_savingNothingNewIsNotAnEvent(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := recordLogs(t)

	form := url.Values{
		store.SettingDriftNotifyCents:  {"1000"},
		store.SettingToleranceCents:    {"5000"},
		store.SettingTolerancePercent:  {"25"},
		store.SettingPayeePrefixes:     {"VISA,MASTERCARD,MC,MAESTRO,DEBIT,KARTENZAHLUNG,POS"},
		store.SettingAutoProbability:   {"90"},
		store.SettingReviewProbability: {"50"},
		store.SettingMatchOverlap:      {"50"},
	}
	if w := post(t, srv, "/settings", form); w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	if _, found := rec.find("settings.changed"); found {
		t.Error("saving the form unchanged was recorded as a policy change")
	}
}

func sampleInquiry() *InquiryItem {
	return &InquiryItem{
		ID: 12, BankName: "TestBank", Merged: true, Percent: 94,
		ParamVersion: "v-abc", AskedAt: "today",
		Date: "2026-08-25", Amount: "-42.00", Currency: "EUR", Payee: "VISA Hotel Berlin",
		CandidateDate: "2026-08-23", CandidateAmount: "-42.00", CandidatePayee: "Hotel Berlin",
		Why: "amount to the cent, two days apart",
	}
}

// TestReview_showsTheConfirmationAndSaysItChangesNothing is the difference the
// page has to carry between its two halves. The queue is money not yet in the
// budget; the confirmation is a question about money already there. A user who
// reads the second as the first will answer it under pressure that does not
// exist, and answering under pressure is how the labels go wrong.
func TestReview_showsTheConfirmationAndSaysItChangesNothing(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetReviewQueue(&fakeReviewQueue{inquiry: sampleInquiry()})

	w := get(t, srv, "/review")
	body := w.Body.String()

	for _, want := range []string{"VISA Hotel Berlin", "Hotel Berlin", "v-abc",
		"amount to the cent", "already up to date"} {
		if !strings.Contains(body, want) {
			t.Errorf("the confirmation does not show %q", want)
		}
	}
	if strings.Contains(body, "Nothing waiting") {
		t.Error("the page claims nothing is waiting while it is asking a question")
	}
}

// TestReview_theConfirmationOffersAWayOutOfGuessing keeps the third button on the
// page. With only yes and no, somebody who cannot remember a transaction still
// has to press one of them, and the label that produces is worse than the
// silence it replaced.
func TestReview_theConfirmationOffersAWayOutOfGuessing(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetReviewQueue(&fakeReviewQueue{inquiry: sampleInquiry()})

	body := get(t, srv, "/review").Body.String()
	for _, want := range []string{`value="yes"`, `value="no"`, `value="unknown"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the confirmation has no %s button", want)
		}
	}
}

// TestReview_carriesEachAnswerThroughAsItself pins the mapping from button to
// label. Three answers go in and three distinct things must come out, in
// particular a nil for "cannot say" rather than a false.
func TestReview_carriesEachAnswerThroughAsItself(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		button string
		want   *bool
	}{
		{"yes", &yes},
		{"no", &no},
		{"unknown", nil},
	} {
		t.Run(tc.button, func(t *testing.T) {
			srv, _ := newTestServer(t)
			q := &fakeReviewQueue{inquiry: sampleInquiry()}
			srv.SetReviewQueue(q)

			w := post(t, srv, "/review/confirm", url.Values{
				"inquiry_id": {"12"}, "param_version": {"v-abc"}, "answer": {tc.button},
			})
			if w.Code != http.StatusFound {
				t.Fatalf("got %d, want a redirect", w.Code)
			}
			if q.answerCalls != 1 || q.answerID != 12 || q.answerVersion != "v-abc" {
				t.Fatalf("handler passed calls=%d id=%d version=%q",
					q.answerCalls, q.answerID, q.answerVersion)
			}
			switch {
			case tc.want == nil && q.answer != nil:
				t.Errorf("%q became the label %v instead of no label at all", tc.button, *q.answer)
			case tc.want != nil && q.answer == nil:
				t.Errorf("%q became no label instead of %v", tc.button, *tc.want)
			case tc.want != nil && *q.answer != *tc.want:
				t.Errorf("%q became %v", tc.button, *q.answer)
			}
		})
	}
}

// TestReview_anAnswerThatIsNotOneOfTheChoicesIsNotALabel keeps a hand-made
// request from writing something nobody clicked. The failure mode this rules out
// is silent: an unrecognised value falling through to a default of false would
// record a wrong label with nothing to show for it.
func TestReview_anAnswerThatIsNotOneOfTheChoicesIsNotALabel(t *testing.T) {
	srv, _ := newTestServer(t)
	q := &fakeReviewQueue{inquiry: sampleInquiry()}
	srv.SetReviewQueue(q)

	w := post(t, srv, "/review/confirm", url.Values{
		"inquiry_id": {"12"}, "param_version": {"v-abc"}, "answer": {"maybe"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want the page back with the reason", w.Code)
	}
	if q.answerCalls != 0 {
		t.Errorf("an answer of %q reached the queue", "maybe")
	}
}

// TestReview_aRefusedConfirmationSaysWhy keeps a rejected answer from looking
// like an accepted one.
func TestReview_aRefusedConfirmationSaysWhy(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetReviewQueue(&fakeReviewQueue{
		inquiry:   sampleInquiry(),
		answerErr: errors.New("the matching settings changed while this page was open"),
	})

	w := post(t, srv, "/review/confirm", url.Values{
		"inquiry_id": {"12"}, "param_version": {"stale"}, "answer": {"yes"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want the page back with the reason", w.Code)
	}
	if !strings.Contains(w.Body.String(), "the matching settings changed") {
		t.Error("the refusal is not shown, so the user would think the answer was recorded")
	}
}

// TestReview_aBrokenConfirmationDoesNotTakeTheQueueDownWithIt keeps the part
// somebody came to this page for working when the optional part cannot be read.
func TestReview_aBrokenConfirmationDoesNotTakeTheQueueDownWithIt(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetReviewQueue(&fakeReviewQueue{
		items:      []ReviewItem{{ID: 7, Payee: "Netflix", Amount: "-9.99"}},
		inquiryErr: errors.New("the decision log is unreadable"),
	})

	w := get(t, srv, "/review")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want the page", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Netflix") {
		t.Error("an unreadable confirmation took the review queue off the page with it")
	}
}

type fakePromotions struct {
	view PromotionView
	err  error

	action     string
	gotVersion string
	calls      int
	actionErr  error
}

func (p *fakePromotions) PromotionPage(context.Context) (PromotionView, error) {
	return p.view, p.err
}

func (p *fakePromotions) WatchTrial(_ context.Context, v string) error {
	p.calls++
	p.action, p.gotVersion = "watch", v
	return p.actionErr
}

func (p *fakePromotions) StopWatching(context.Context) error {
	p.calls++
	p.action = "stop"
	return p.actionErr
}

func (p *fakePromotions) PromoteTrial(_ context.Context, v string) error {
	p.calls++
	p.action, p.gotVersion = "promote", v
	return p.actionErr
}

func (p *fakePromotions) RevertParameters(context.Context) error {
	p.calls++
	p.action = "revert"
	return p.actionErr
}

func promotableView() PromotionView {
	return PromotionView{
		InForce: "aaaa1111", Candidate: "bbbb2222", Labelled: 900,
		Watching: true, Promotable: true,
		Checks: []PromotionCheck{
			{Name: "anchor cases", Status: "passed", Detail: "all 6 still decided as documented"},
			{Name: "calibration", Status: "passed", Detail: "Brier 0.0385 against 0.0418 in force"},
			{Name: "changed decisions", Status: "for a person", Detail: "12 of 400 decisions would have gone differently (3.0%)"},
		},
	}
}

// TestMatching_showsBothVersionsAndEveryFinding is what the page is for. A
// promotion changes how money is matched, and the person pressing the button has
// to be able to see what they are replacing and what the checks actually said.
func TestMatching_showsBothVersionsAndEveryFinding(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetPromotions(&fakePromotions{view: promotableView()})

	body := get(t, srv, "/matching").Body.String()
	for _, want := range []string{"aaaa1111", "bbbb2222", "anchor cases", "calibration",
		"changed decisions", "3.0%", "900"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}
}

// TestMatching_anInstallationWithNoEvidenceIsNotOfferedAChoice keeps the page
// honest for the majority. There is nothing to fit to and there never may be,
// and that is not a fault to be reported as one.
func TestMatching_anInstallationWithNoEvidenceIsNotOfferedAChoice(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetPromotions(&fakePromotions{view: PromotionView{InForce: "aaaa1111"}})

	body := get(t, srv, "/matching").Body.String()
	if !strings.Contains(body, "Nothing to fit to") {
		t.Error("an installation with no settled decisions is not told why there is no candidate")
	}
	if strings.Contains(body, `value="promote"`) {
		t.Error("a promotion button was offered with nothing to promote")
	}
}

// TestMatching_promotionIsNotOfferedBeforeWatching pins the rule in the markup as
// well as in the syncer. A button that is there to be pressed and then refused
// teaches people the page is unreliable.
func TestMatching_promotionIsNotOfferedBeforeWatching(t *testing.T) {
	view := promotableView()
	view.Watching = false
	srv, _ := newTestServer(t)
	srv.SetPromotions(&fakePromotions{view: view})

	body := get(t, srv, "/matching").Body.String()
	if strings.Contains(body, `value="promote"`) {
		t.Error("a candidate nobody is watching was offered for promotion")
	}
	if !strings.Contains(body, `value="watch"`) {
		t.Error("there is no way to start watching")
	}
}

// TestMatching_aFailedGateDoesNotOfferTheButton keeps the checks meaningful in
// the interface and not only in the log.
func TestMatching_aFailedGateDoesNotOfferTheButton(t *testing.T) {
	view := promotableView()
	view.Promotable = false
	view.Checks[0] = PromotionCheck{Name: "anchor cases", Status: "failed",
		Detail: "1 of 6 moved"}
	srv, _ := newTestServer(t)
	srv.SetPromotions(&fakePromotions{view: view})

	body := get(t, srv, "/matching").Body.String()
	if !strings.Contains(body, "disabled") {
		t.Error("a candidate that failed its checks was offered for promotion anyway")
	}
	if !strings.Contains(body, "1 of 6 moved") {
		t.Error("the failure is not shown, so there is no way to know why")
	}
}

// TestMatching_carriesEachActionThroughAsItself pins the four things a person can
// do, including that each one carries the version it was looking at.
func TestMatching_carriesEachActionThroughAsItself(t *testing.T) {
	for _, action := range []string{"watch", "stop", "promote", "revert"} {
		t.Run(action, func(t *testing.T) {
			srv, _ := newTestServer(t)
			p := &fakePromotions{view: promotableView()}
			srv.SetPromotions(p)

			w := post(t, srv, "/matching/apply", url.Values{
				"action": {action}, "param_version": {"bbbb2222"},
			})
			if w.Code != http.StatusOK {
				t.Fatalf("got %d", w.Code)
			}
			if p.calls != 1 || p.action != action {
				t.Fatalf("calls=%d action=%q", p.calls, p.action)
			}
			if (action == "watch" || action == "promote") && p.gotVersion != "bbbb2222" {
				t.Errorf("the version on the page did not travel with the action: %q", p.gotVersion)
			}
		})
	}
}

// TestMatching_anUnknownActionChangesNothing keeps a hand-made request from
// falling through to whichever branch happens to be last.
func TestMatching_anUnknownActionChangesNothing(t *testing.T) {
	srv, _ := newTestServer(t)
	p := &fakePromotions{view: promotableView()}
	srv.SetPromotions(p)

	w := post(t, srv, "/matching/apply", url.Values{"action": {"install"}})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	if p.calls != 0 {
		t.Errorf("an unknown action reached the syncer as %q", p.action)
	}
}

// TestMatching_aRefusalIsShown keeps a rejected promotion from looking like a
// completed one.
func TestMatching_aRefusalIsShown(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetPromotions(&fakePromotions{
		view:      promotableView(),
		actionErr: errors.New("nothing is being watched, so there is no record"),
	})

	w := post(t, srv, "/matching/apply", url.Values{
		"action": {"promote"}, "param_version": {"bbbb2222"},
	})
	if !strings.Contains(w.Body.String(), "nothing is being watched") {
		t.Error("the refusal is not shown, so the user would think it had been applied")
	}
}

// TestReview_countsWhatDidNotGoThrough covers the one thing a refused review
// interaction previously left no trace of outside a log line.
//
// Every refusal is a person who tried to settle a transaction and was told no. A
// run of them means the page is being drawn from state that keeps moving
// underneath it, and no other series in the program would show that. Refusals
// and failures share the counter because the ratio is the interesting figure: a
// refusal is the program working, a failure is it not working.
//
// The real instrument is read through a manual reader rather than a stub, so
// this fails if the counter is never registered as well as if it is never
// incremented.
func TestReview_countsWhatDidNotGoThrough(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(previous) })

	srv, _ := newTestServer(t)
	srv.InitTelemetry()

	ctx := context.Background()
	srv.recordReviewProblem(ctx, "resolve", Refuse("that decision is no longer in the queue"))
	srv.recordReviewProblem(ctx, "resolve", Refuse("the matching settings changed"))
	srv.recordReviewProblem(ctx, "confirm", errors.New("the database is unreachable"))

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "bankingsync_review_problems_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("the counter came back as %T", m.Data)
			}
			for _, dp := range sum.DataPoints {
				op, _ := dp.Attributes.Value(attribute.Key("op"))
				outcome, _ := dp.Attributes.Value(attribute.Key("outcome"))
				got[op.AsString()+"/"+outcome.AsString()] += dp.Value
			}
		}
	}

	if len(got) == 0 {
		t.Fatal("the counter was never registered or never incremented")
	}
	for key, want := range map[string]int64{"resolve/refused": 2, "confirm/failed": 1} {
		if got[key] != want {
			t.Errorf("%s = %d, want %d — full tally %v", key, got[key], want, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("unexpected series: %v", got)
	}
}
