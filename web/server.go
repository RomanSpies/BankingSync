package web

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bankingsync/enablebanking"
	"bankingsync/logs"
	"bankingsync/store"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const maxPEMUploadBytes = 64 << 10

func validatePrivateKeyPEM(data []byte) error {
	block, _ := pem.Decode(data)
	if block == nil {
		return fmt.Errorf("no PEM block found — expected a private key")
	}
	if !strings.Contains(block.Type, "PRIVATE KEY") {
		return fmt.Errorf("expected a private key, got %q", block.Type)
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		if _, err := x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
			return fmt.Errorf("unreadable RSA key: %w", err)
		}
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return fmt.Errorf("unreadable key: %w", err)
		}
		if _, ok := key.(*rsa.PrivateKey); !ok {
			return fmt.Errorf("expected an RSA key — Enable Banking does not accept other key types")
		}
	default:
		return fmt.Errorf("unsupported PEM type %q", block.Type)
	}
	return nil
}

// AppVersion is set by the main package at startup.
var AppVersion string

var olog = logs.Get("bankingsync/web")

// SBOMPath is the path to the CycloneDX SBOM file generated during the Docker build.
var SBOMPath = "/app/sbom.cdx.json"

// SyncTriggerFunc is called when the user requests a manual sync from the web UI.
// It reports whether the sync actually ran; a scheduled sync already in flight
// causes the trigger to be skipped.
type SyncTriggerFunc func() bool

// CycloneDX types for SBOM parsing.
type cdxBOM struct {
	BOMFormat   string         `json:"bomFormat"`
	SpecVersion string         `json:"specVersion"`
	Version     int            `json:"version"`
	Components  []cdxComponent `json:"components"`
}

type cdxComponent struct {
	Type     string       `json:"type"`
	Name     string       `json:"name"`
	Version  string       `json:"version"`
	PURL     string       `json:"purl"`
	Licenses []cdxLicense `json:"licenses"`
}

type cdxLicenseEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type cdxLicense struct {
	License cdxLicenseEntry `json:"license"`
}

// BackendStatus describes the budget backend for the health endpoint. Operators
// need to see which backend is active and whether it answered, because a
// misconfigured or unreachable backend is the most likely failure in practice.
type BackendStatus struct {
	Name      string
	Version   string
	Reachable bool
	CheckedAt time.Time
}

// Server is the embedded web UI and health endpoint server.
type Server struct {
	st         *store.Store
	eb         *enablebanking.Client
	trigger    SyncTriggerFunc
	testEmail  func(context.Context) error
	templateFS fs.FS

	mu          sync.Mutex
	syncRunning bool

	reviewProblems metric.Int64Counter

	backendStatus  func() BackendStatus
	openingBalance func(context.Context, int64, bool, *int64) (OpeningBalancePreview, error)
	review         ReviewQueue
	promotions     Promotions

	mux *http.ServeMux
	srv *http.Server
}

// NewFromDir creates the Server using templates loaded from the "web/templates"
// subdirectory on disk. It is the standard constructor for production use.
func NewFromDir(st *store.Store, eb *enablebanking.Client, trigger SyncTriggerFunc, testEmail func(context.Context) error) (*Server, error) {
	return New(st, eb, trigger, testEmail, os.DirFS("web"))
}

// New creates the Server, registers all routes, and validates templates from templateFS.
func New(st *store.Store, eb *enablebanking.Client, trigger SyncTriggerFunc, testEmail func(context.Context) error, templateFS fs.FS) (*Server, error) {
	// Validate all templates at startup to catch authoring errors early.
	funcs := template.FuncMap{"version": func() string { return AppVersion }}
	if _, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html"); err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	s := &Server{
		st:         st,
		eb:         eb,
		trigger:    trigger,
		testEmail:  testEmail,
		templateFS: templateFS,
		mux:        http.NewServeMux(),
	}

	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/setup", s.handleSetup)
	s.mux.HandleFunc("/connect", s.handleConnect)
	s.mux.HandleFunc("/refresh-banks", s.handleRefreshBanks)
	s.mux.HandleFunc("/callback", s.handleCallback)
	s.mux.HandleFunc("/pick-account", s.handlePickAccount)
	s.mux.HandleFunc("/status", s.handleStatus)
	s.mux.HandleFunc("/renew", s.handleRenew)
	s.mux.HandleFunc("/remove-account", s.handleRemoveAccount)
	s.mux.HandleFunc("/reset-sync", s.handleResetSync)
	s.mux.HandleFunc("/sync/now", s.handleSyncNow)
	s.mux.HandleFunc("/test-email", s.handleTestEmail)
	s.mux.HandleFunc("/settings", s.handleSettings)
	s.mux.HandleFunc("/opening-balance", s.handleOpeningBalance)
	s.mux.HandleFunc("/review", s.handleReview)
	s.mux.HandleFunc("/review/resolve", s.handleReviewResolve)
	s.mux.HandleFunc("/review/confirm", s.handleReviewConfirm)
	s.mux.HandleFunc("/matching", s.handleMatching)
	s.mux.HandleFunc("/matching/apply", s.handleMatchingApply)
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/sbom", s.handleSBOM)
	s.mux.HandleFunc("/sbom.json", s.handleSBOMJSON)

	return s, nil
}

// BackendLabel names the active backend for the UI. It falls back to the value
// persisted at startup so the templates never say "Actual" to a Firefly user.
func (s *Server) BackendLabel() string {
	if s.backendStatus != nil {
		if n := s.backendStatus().Name; n != "" {
			return BackendDisplayName(n)
		}
	}
	if v, _ := s.st.GetSetting("budget_backend"); v != "" {
		return BackendDisplayName(v)
	}
	return "your budget"
}

func (s *Server) backendVersionOrEmpty() string {
	if s.backendStatus == nil {
		return ""
	}
	return s.backendStatus().Version
}

// BackendDisplayName turns a stored backend identifier into the name a user
// recognises. It is exported because the same mapping is needed wherever a
// backend is named to a person, and two spellings of "Actual Budget" would be
// one too many.
func BackendDisplayName(name string) string {
	switch name {
	case "actual":
		return "Actual Budget"
	case "firefly":
		return "Firefly III"
	default:
		return name
	}
}

// SetBackendStatus wires a provider for the backend section of /health.
func (s *Server) SetBackendStatus(fn func() BackendStatus) {
	s.backendStatus = fn
}

// Mux returns the underlying ServeMux so callers can register additional routes.
func (s *Server) Mux() *http.ServeMux { return s.mux }

// StartTLS begins listening with TLS using the provided cert and key files.
// It blocks until the server stops.
func (s *Server) StartTLS(addr, certFile, keyFile string) error {
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           traceMiddleware(sameOriginMiddleware(s.mux)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("Web UI → https://localhost%s", addr)
	if err := s.srv.ListenAndServeTLS(certFile, keyFile); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// statusWriter captures the HTTP status code for trace attributes.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func sameOriginMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		if !requestIsSameOrigin(r) {
			olog.Warn(r.Context(), "http.crossorigin.rejected",
				logs.String("path", r.URL.Path),
				logs.String("origin", r.Header.Get("Origin")),
				logs.String("referer", r.Header.Get("Referer")),
			)
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requestIsSameOrigin(r *http.Request) bool {
	if r.Host == "" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		return originHostMatches(origin, r.Host)
	}
	if referer := r.Header.Get("Referer"); referer != "" {
		return originHostMatches(referer, r.Host)
	}
	return true
}

func originHostMatches(rawURL, host string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, host)
}

// traceMiddleware wraps an http.Handler to create an OTel span per request,
// enabling Pyroscope profile correlation with web handler traces.
func traceMiddleware(next http.Handler) http.Handler {
	tracer := otel.Tracer("bankingsync/web")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), fmt.Sprintf("%s %s", r.Method, r.URL.Path),
			trace.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.path", r.URL.Path),
			),
		)
		defer span.End()

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r.WithContext(ctx))
		span.SetAttributes(attribute.Int("http.status_code", sw.status))
		if sw.status >= 400 {
			olog.Warn(ctx, "http.request.error",
				logs.String("method", r.Method),
				logs.String("path", r.URL.Path),
				logs.Int("status", sw.status),
			)
		}
	})
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	funcs := template.FuncMap{"version": func() string { return AppVersion }}
	tpl, err := template.New("").Funcs(funcs).ParseFS(s.templateFS, "templates/base.html", "templates/"+name)
	if err != nil {
		log.Printf("parse template %s: %v", name, err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template %s: %v", name, err)
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// isSetup returns true when the Enable Banking private key is available —
// either stored in the DB via the setup UI or present as a file in /data/,
// matching the resolution order of DefaultPEMSource.
func (s *Server) isSetup() bool {
	if v, _ := s.st.GetSetting("eb_pem_content"); v != "" {
		return true
	}
	if _, err := os.Stat("/data/private.pem"); err == nil {
		return true
	}
	matches, _ := filepath.Glob("/data/*.pem")
	return len(matches) > 0
}

func (s *Server) isConnected() bool {
	accounts, err := s.st.GetAllBankAccounts()
	return err == nil && len(accounts) > 0
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !s.isSetup() {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	if !s.isConnected() {
		http.Redirect(w, r, "/connect", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/status", http.StatusFound)
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	type setupData struct {
		Title       string
		Error       string
		AppID       string
		PEMReady    bool
		AppIDLocked bool
	}

	appIDEnv := os.Getenv("EB_APPLICATION_ID")
	pemReady := s.isSetup()
	savedAppID, _ := s.st.GetSetting("eb_app_id")
	if appIDEnv != "" {
		savedAppID = appIDEnv
	}

	if r.Method == http.MethodGet {
		s.render(w, "setup.html", setupData{
			Title:       "Setup",
			AppID:       savedAppID,
			PEMReady:    pemReady,
			AppIDLocked: appIDEnv != "",
		})
		return
	}

	// Accept both multipart (with file upload) and URL-encoded (app_id only) forms.
	// ParseMultipartForm also calls ParseForm, so r.FormValue works either way.
	r.Body = http.MaxBytesReader(w, r.Body, maxPEMUploadBytes)
	if err := r.ParseMultipartForm(maxPEMUploadBytes); err != nil && isRequestTooLarge(err) {
		s.render(w, "setup.html", setupData{
			Title: "Setup",
			Error: fmt.Sprintf("Upload rejected: a private key must be smaller than %d KiB.", maxPEMUploadBytes>>10),
		})
		return
	}

	if r.MultipartForm != nil {
		file, _, err := r.FormFile("pem_file")
		if err != nil && err != http.ErrMissingFile {
			s.render(w, "setup.html", setupData{Title: "Setup", Error: "PEM read error: " + err.Error()})
			return
		}
		if file != nil {
			defer file.Close()
			pemBytes, err := io.ReadAll(file)
			if err != nil {
				s.render(w, "setup.html", setupData{Title: "Setup", Error: "PEM read error: " + err.Error()})
				return
			}
			if err := validatePrivateKeyPEM(pemBytes); err != nil {
				s.render(w, "setup.html", setupData{Title: "Setup", Error: "Invalid PEM file: " + err.Error()})
				return
			}
			if err := s.st.SetSetting("eb_pem_content", string(pemBytes)); err != nil {
				s.render(w, "setup.html", setupData{Title: "Setup", Error: "Failed to store PEM: " + err.Error()})
				return
			}
			pemReady = true
		}
	}

	if appIDEnv == "" {
		appID := strings.TrimSpace(r.FormValue("app_id"))
		if appID == "" {
			s.render(w, "setup.html", setupData{Title: "Setup", Error: "Application ID is required.", PEMReady: pemReady})
			return
		}
		if err := s.st.SetSetting("eb_app_id", appID); err != nil {
			s.render(w, "setup.html", setupData{Title: "Setup", Error: "Failed to store App ID: " + err.Error(), PEMReady: pemReady})
			return
		}
		savedAppID = appID
	}

	if !pemReady {
		s.render(w, "setup.html", setupData{Title: "Setup", Error: "Please upload a PEM file.", AppID: savedAppID, AppIDLocked: appIDEnv != ""})
		return
	}

	if s.isConnected() {
		http.Redirect(w, r, "/status", http.StatusFound)
	} else {
		http.Redirect(w, r, "/connect", http.StatusFound)
	}
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	type connectData struct {
		Title     string
		Error     string
		AuthURL   string
		Banks     []enablebanking.ASPSP
		Countries []string
		Accounts  []store.BankAccount
		CacheAge  string
	}

	accounts, _ := s.st.GetAllBankAccounts()

	// Show pending auth URL if present (from /renew redirect)
	if r.Method == http.MethodGet {
		authURL, _ := s.st.GetSetting("pending_auth_url")
		banks, err := s.getASPSPs()
		errStr := r.URL.Query().Get("error")
		if err != nil && errStr == "" {
			errStr = "Failed to load bank list: " + err.Error()
		}
		s.render(w, "connect.html", connectData{
			Title:     "Connect Bank",
			Error:     errStr,
			AuthURL:   authURL,
			Banks:     banks,
			Countries: uniqueCountries(banks),
			Accounts:  accounts,
			CacheAge:  s.aspspCacheAge(),
		})
		return
	}

	bankName := r.FormValue("bank_name")
	bankCountry := r.FormValue("bank_country")
	psuType := r.FormValue("psu_type")
	if psuType == "" {
		psuType = "personal"
	}

	stateUUID := uuid.New().String()
	_ = s.st.SetSetting("pending_session_state", stateUUID)
	_ = s.st.SetSetting("pending_bank_name", bankName)
	_ = s.st.SetSetting("pending_bank_country", bankCountry)
	_ = s.st.SetSetting("pending_auth_url", "")

	appBaseURL := detectBaseURL(r, s.st)
	authURL, expiresAt, err := s.eb.StartAuth(r.Context(), bankName, bankCountry, psuType, stateUUID, appBaseURL,
		s.consentValidity(bankName, bankCountry))
	if err != nil {
		errMsg := "Failed to start authorisation: " + err.Error()
		if isWrongASPSPError(err) {
			s.invalidateASPSPCache()
			errMsg = fmt.Sprintf("Enable Banking rejected %q (WRONG_ASPSP_PROVIDED). "+
				"The bank list was refreshed from Enable Banking — please select the bank again. "+
				"If it keeps failing, that bank may not yet be enabled for your application.", bankName)
		}
		banks, _ := s.getASPSPs()
		s.render(w, "connect.html", connectData{
			Title:     "Connect Bank",
			Error:     errMsg,
			Banks:     banks,
			Countries: uniqueCountries(banks),
			Accounts:  accounts,
			CacheAge:  s.aspspCacheAge(),
		})
		return
	}

	_ = s.st.SetSetting("pending_session_expiry", expiresAt.Format(time.RFC3339))

	banks, _ := s.getASPSPs()
	s.render(w, "connect.html", connectData{
		Title:     "Connect Bank",
		AuthURL:   authURL,
		Banks:     banks,
		Countries: uniqueCountries(banks),
		Accounts:  accounts,
		CacheAge:  s.aspspCacheAge(),
	})
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	rawState := r.URL.Query().Get("state")

	if code == "" || rawState == "" {
		http.Redirect(w, r, "/connect?error=Missing+callback+parameters", http.StatusFound)
		return
	}

	expectedUUID, _ := s.st.GetSetting("pending_session_state")
	if rawState == "" || rawState != expectedUUID {
		http.Redirect(w, r, "/connect?error=Invalid+session+state", http.StatusFound)
		return
	}

	sr, err := s.eb.CompleteAuth(r.Context(), code, rawState)
	if err != nil {
		http.Redirect(w, r, "/connect?error="+urlEncode("Auth failed: "+err.Error()), http.StatusFound)
		return
	}
	if len(sr.Accounts) == 0 {
		http.Redirect(w, r, "/connect?error=No+accounts+returned", http.StatusFound)
		return
	}

	bankName, _ := s.st.GetSetting("pending_bank_name")
	bankCountry, _ := s.st.GetSetting("pending_bank_country")
	expiry, _ := s.st.GetSetting("pending_session_expiry")
	if expiry == "" {
		expiry = time.Now().UTC().Add(enablebanking.DefaultConsentValidity).Format(time.RFC3339)
	}
	renewAccountID, _ := s.st.GetSetting("pending_renew_account_id")

	_ = s.st.SetSetting("pending_session_state", "")
	_ = s.st.SetSetting("pending_bank_name", "")
	_ = s.st.SetSetting("pending_bank_country", "")
	_ = s.st.SetSetting("pending_renew_account_id", "")
	_ = s.st.SetSetting("pending_auth_url", "")

	if renewAccountID != "" && len(sr.Accounts) == 1 {
		id, _ := strconv.ParseInt(renewAccountID, 10, 64)
		_ = s.st.UpdateBankAccountSession(id, sr.SessionID, expiry)
		http.Redirect(w, r, "/status", http.StatusFound)
		return
	}

	accountsJSON, _ := json.Marshal(sr.Accounts)
	_ = s.st.SetSetting("pending_auth_session_id", sr.SessionID)
	_ = s.st.SetSetting("pending_auth_accounts", string(accountsJSON))
	_ = s.st.SetSetting("pending_auth_expiry", expiry)
	_ = s.st.SetSetting("pending_auth_bank_name", bankName)
	_ = s.st.SetSetting("pending_auth_bank_country", bankCountry)

	http.Redirect(w, r, "/pick-account", http.StatusFound)
}

// defaultBudgetAccount resolves the prefilled target account name for the active
// backend, so a Firefly user is not offered an ACTUAL_ACCOUNT default.
func (s *Server) defaultBudgetAccount() string {
	backend, _ := s.st.GetSetting("budget_backend")
	if backend == "firefly" {
		return strings.TrimSpace(os.Getenv("FIREFLY_ACCOUNT"))
	}
	return strings.TrimSpace(os.Getenv("ACTUAL_ACCOUNT"))
}

// suggestedNameFor derives a target account name for the selected bank account.
// It is the server-side half of the picker's suggestion: with scripting off the
// field arrives empty, and falling back to a shared constant would funnel every
// connected bank into a single budget account.
func suggestedNameFor(accounts []enablebanking.SessionAccount, uid, bankName string) string {
	for _, a := range accounts {
		if a.EffectiveUID() == uid {
			return a.SuggestedAccountName(bankName)
		}
	}
	return ""
}

// renameSafeBackend reports whether the backend re-finds an account by something
// other than its name, which decides whether the picker may promise that a later
// rename is harmless. Firefly matches on IBAN; Actual has only the name.
func (s *Server) renameSafeBackend() bool {
	backend, _ := s.st.GetSetting("budget_backend")
	return backend == "firefly"
}

// knownBudgetAccounts lists target account names already in use, so the picker
// can offer them instead of relying on the user retyping one exactly. A typo
// silently creates a second account and splits the balance history.
func (s *Server) knownBudgetAccounts() []string {
	accounts, err := s.st.GetAllBankAccounts()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, a := range accounts {
		if a.ActualAccount == "" || seen[a.ActualAccount] {
			continue
		}
		seen[a.ActualAccount] = true
		out = append(out, a.ActualAccount)
	}
	sort.Strings(out)
	return out
}

func accountDetails(accounts []enablebanking.SessionAccount, uid string) (string, string) {
	for _, a := range accounts {
		if a.EffectiveUID() == uid {
			return a.IBAN, a.Currency
		}
	}
	return "", ""
}

func (s *Server) handlePickAccount(w http.ResponseWriter, r *http.Request) {
	type pickData struct {
		Title            string
		SessionID        string
		BankName         string
		BankCountry      string
		Expiry           string
		Accounts         any
		DefaultAccount   string
		DefaultStartDate string
		BackendLabel     string
		KnownAccounts    []string
		RenameSafe       bool
		Error            string
	}

	sessionID, _ := s.st.GetSetting("pending_auth_session_id")
	accountsJSON, _ := s.st.GetSetting("pending_auth_accounts")
	expiry, _ := s.st.GetSetting("pending_auth_expiry")
	bankName, _ := s.st.GetSetting("pending_auth_bank_name")
	bankCountry, _ := s.st.GetSetting("pending_auth_bank_country")
	defaultAccount := s.defaultBudgetAccount()
	defaultStart := time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02")

	var accounts []enablebanking.SessionAccount
	_ = json.Unmarshal([]byte(accountsJSON), &accounts)

	type pickable struct {
		enablebanking.SessionAccount
		Suggested string
	}
	pickables := make([]pickable, 0, len(accounts))
	for _, a := range accounts {
		pickables = append(pickables, pickable{SessionAccount: a, Suggested: a.SuggestedAccountName(bankName)})
	}

	if r.Method == http.MethodGet {
		s.render(w, "pick_account.html", pickData{
			Title:            "Select Account",
			SessionID:        sessionID,
			BankName:         bankName,
			BankCountry:      bankCountry,
			Expiry:           expiry,
			Accounts:         pickables,
			DefaultAccount:   defaultAccount,
			DefaultStartDate: defaultStart,
			BackendLabel:     s.BackendLabel(),
			KnownAccounts:    s.knownBudgetAccounts(),
			RenameSafe:       s.renameSafeBackend(),
		})
		return
	}

	uid := r.FormValue("account_uid")
	actualAccount := strings.TrimSpace(r.FormValue("actual_account"))
	startDate := strings.TrimSpace(r.FormValue("start_sync_date"))
	if uid == "" {
		s.render(w, "pick_account.html", pickData{
			Title:            "Select Account",
			Accounts:         pickables,
			DefaultAccount:   defaultAccount,
			DefaultStartDate: defaultStart,
			BackendLabel:     s.BackendLabel(),
			KnownAccounts:    s.knownBudgetAccounts(),
			RenameSafe:       s.renameSafeBackend(),
			Error:            "Please select an account.",
		})
		return
	}
	if actualAccount == "" {
		actualAccount = defaultAccount
	}
	if actualAccount == "" {
		actualAccount = suggestedNameFor(accounts, uid, bankName)
	}
	if actualAccount == "" {
		actualAccount = bankName
	}
	if startDate == "" {
		startDate = defaultStart
	}

	iban, currency := accountDetails(accounts, uid)

	if _, err := s.st.AddBankAccount(store.NewBankAccount{
		SessionID:     sessionID,
		AccountUID:    uid,
		BankName:      bankName,
		BankCountry:   bankCountry,
		ActualAccount: actualAccount,
		StartSyncDate: startDate,
		SessionExpiry: expiry,
		IBAN:          iban,
		Currency:      currency,
	}); err != nil {
		http.Error(w, "Failed to save: "+err.Error(), http.StatusInternalServerError)
		return
	}

	_ = s.st.SetSetting("pending_auth_session_id", "")
	_ = s.st.SetSetting("pending_auth_accounts", "")
	_ = s.st.SetSetting("pending_auth_expiry", "")
	_ = s.st.SetSetting("pending_auth_bank_name", "")
	_ = s.st.SetSetting("pending_auth_bank_country", "")

	http.Redirect(w, r, "/status", http.StatusFound)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	type accountRow struct {
		store.BankAccount
		DaysLeft   int
		MaskedIBAN string
	}
	type statusData struct {
		Title           string
		Accounts        []accountRow
		LastSync        string
		SyncRunning     bool
		SyncLogs        []store.SyncLog
		UpdateAvailable string
		BackendLabel    string
		BackendVersion  string
		ReviewsOpen     int
	}

	accounts, _ := s.st.GetAllBankAccounts()
	rows := make([]accountRow, 0, len(accounts))
	for _, a := range accounts {
		row := accountRow{BankAccount: a}
		row.MaskedIBAN = enablebanking.SessionAccount{IBAN: a.IBAN}.MaskedIBAN()
		if t, err := time.Parse(time.RFC3339, a.SessionExpiry); err == nil {
			row.DaysLeft = int(time.Until(t).Hours() / 24)
		}
		rows = append(rows, row)
	}
	syncLogs, _ := s.st.GetSyncLogs(20)
	updateAvail, _ := s.st.GetSetting("update_available")

	lastSync, _ := s.st.GetLastSyncDate()
	reviewsOpen, _ := s.st.CountMatchReviews()

	s.mu.Lock()
	running := s.syncRunning
	s.mu.Unlock()

	s.render(w, "status.html", statusData{
		Title:           "Status",
		Accounts:        rows,
		LastSync:        lastSync,
		SyncRunning:     running,
		SyncLogs:        syncLogs,
		UpdateAvailable: updateAvail,
		BackendLabel:    s.BackendLabel(),
		BackendVersion:  s.backendVersionOrEmpty(),
		ReviewsOpen:     reviewsOpen,
	})
}

func (s *Server) handleRenew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	idStr := r.FormValue("account_id")
	id, _ := strconv.ParseInt(idStr, 10, 64)

	accounts, _ := s.st.GetAllBankAccounts()
	var bankName, bankCountry string
	var found bool
	for _, a := range accounts {
		if a.ID == id {
			bankName = a.BankName
			bankCountry = a.BankCountry
			found = true
			break
		}
	}
	if !found {
		http.Redirect(w, r, "/status", http.StatusFound)
		return
	}
	if bankName == "" {
		// Migrated account has no bank name stored — user must re-select their bank.
		http.Redirect(w, r, "/connect?error="+urlEncode("Your account was migrated without bank details. Please re-connect your bank to renew access."), http.StatusFound)
		return
	}

	stateUUID := uuid.New().String()
	_ = s.st.SetSetting("pending_session_state", stateUUID)
	_ = s.st.SetSetting("pending_bank_name", bankName)
	_ = s.st.SetSetting("pending_bank_country", bankCountry)
	_ = s.st.SetSetting("pending_renew_account_id", idStr)

	appBaseURL := detectBaseURL(r, s.st)
	authURL, expiresAt, err := s.eb.StartAuth(r.Context(), bankName, bankCountry, "personal", stateUUID, appBaseURL,
		s.consentValidity(bankName, bankCountry))
	if err != nil {
		http.Redirect(w, r, "/connect?error="+urlEncode("Failed to start renewal: "+err.Error()), http.StatusFound)
		return
	}
	_ = s.st.SetSetting("pending_session_expiry", expiresAt.Format(time.RFC3339))

	_ = s.st.SetSetting("pending_auth_url", authURL)
	http.Redirect(w, r, "/connect", http.StatusFound)
}

func (s *Server) handleRemoveAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	id, _ := strconv.ParseInt(r.FormValue("account_id"), 10, 64)
	_ = s.st.RemoveBankAccount(id)
	http.Redirect(w, r, "/status", http.StatusFound)
}

func (s *Server) handleResetSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	id, _ := strconv.ParseInt(r.FormValue("account_id"), 10, 64)
	startDate := strings.TrimSpace(r.FormValue("start_date"))
	if startDate == "" {
		startDate = time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02")
	}
	_ = s.st.UpdateBankAccountStartDate(id, startDate)
	http.Redirect(w, r, "/status", http.StatusFound)
}

func (s *Server) handleSyncNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	if s.syncRunning {
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":false,"reason":"already running"}`)
		return
	}
	s.syncRunning = true
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			s.syncRunning = false
			s.mu.Unlock()
		}()
		if !s.trigger() {
			log.Printf("Manual sync skipped — a scheduled sync is already running")
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"ok":true}`)
}

func (s *Server) handleTestEmail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if s.testEmail == nil {
		fmt.Fprint(w, `{"ok":false,"error":"test email not configured"}`)
		return
	}
	if err := s.testEmail(r.Context()); err != nil {
		fmt.Fprintf(w, `{"ok":false,"error":%q}`, err.Error())
		return
	}
	fmt.Fprint(w, `{"ok":true}`)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	accounts, _ := s.st.GetAllBankAccounts()
	lastSync, _ := s.st.GetLastSyncDate()
	lastLog, _ := s.st.GetLatestSyncLog()

	status := "ok"
	httpCode := http.StatusOK

	if len(accounts) == 0 || lastSync == "" {
		status = "unhealthy"
		httpCode = http.StatusServiceUnavailable
	}

	expiring := 0
	for _, a := range accounts {
		if t, err := time.Parse(time.RFC3339, a.SessionExpiry); err == nil {
			if int(time.Until(t).Hours()/24) < 7 {
				expiring++
			}
		}
	}
	if expiring > 0 && status == "ok" {
		status = "degraded"
	}
	if lastLog != nil && lastLog.Status != "success" && status == "ok" {
		status = "degraded"
	}

	backend := map[string]any{"name": "unknown"}
	if s.backendStatus != nil {
		b := s.backendStatus()
		backend = map[string]any{"name": b.Name, "reachable": b.Reachable}
		if b.Version != "" {
			backend["version"] = b.Version
		}
		if !b.CheckedAt.IsZero() {
			backend["checked_at"] = b.CheckedAt.Format(time.RFC3339)
		}
		if !b.Reachable && status == "ok" {
			status = "degraded"
		}
	}

	opening := map[string]int{}
	drifting, checked := 0, 0
	var maxAbs int64
	worst := ""
	lastChecked := ""
	for _, a := range accounts {
		state := a.OpeningBalanceState
		if state == "" {
			state = "not_set"
		}
		opening[state]++
		if state == store.OpeningBalanceDenied || state == store.OpeningBalanceUnavailable {
			if status == "ok" {
				status = "degraded"
			}
		}
		if a.DriftState == store.DriftOK || a.DriftState == store.DriftAlert {
			checked++
		}
		if a.DriftState == store.DriftAlert {
			drifting++
			if status == "ok" {
				status = "degraded"
			}
			abs := a.DriftCents
			if abs < 0 {
				abs = -abs
			}
			if abs > maxAbs {
				maxAbs, worst = abs, a.BankName
			}
		}
		if a.DriftCheckedAt > lastChecked {
			lastChecked = a.DriftCheckedAt
		}
	}

	drift := map[string]any{
		"accounts_checked":  checked,
		"accounts_drifting": drifting,
		"max_abs_cents":     maxAbs,
	}
	if worst != "" {
		drift["worst_account"] = worst
	}
	if lastChecked != "" {
		drift["checked_at"] = lastChecked
	}

	// A held transaction is in no budget at all, so an instance with an open
	// decision is not doing the job it was installed for — degraded is the
	// honest word, and it is what makes a forgotten queue visible to monitoring
	// rather than only to whoever opens the page.
	openReviews, _ := s.st.CountMatchReviews()
	if openReviews > 0 && status == "ok" {
		status = "degraded"
	}

	resp := map[string]any{
		"status":             status,
		"version":            AppVersion,
		"connected_accounts": len(accounts),
		"expiring_sessions":  expiring,
		"backend":            backend,
		"opening_balances":   opening,
		"drift":              drift,
		"reviews_open":       openReviews,
	}
	if lastSync != "" {
		resp["last_sync"] = lastSync
		if d, err := time.Parse("2006-01-02", lastSync); err == nil {
			resp["hours_since_sync"] = int(time.Since(d).Hours())
		}
	}
	if lastLog != nil {
		resp["last_sync_status"] = lastLog.Status
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpCode)
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleSBOM(w http.ResponseWriter, r *http.Request) {
	type sbomRow struct {
		Name    string
		Version string
		License string
	}
	type sbomData struct {
		Title      string
		Error      string
		Format     string
		GoModules  []sbomRow
		OSPackages []sbomRow
		Total      int
	}

	data, err := os.ReadFile(SBOMPath)
	if err != nil {
		s.render(w, "sbom.html", sbomData{
			Title: "SBOM",
			Error: "SBOM file not available. It is generated during the Docker build.",
		})
		return
	}

	var bom cdxBOM
	if err := json.Unmarshal(data, &bom); err != nil {
		s.render(w, "sbom.html", sbomData{
			Title: "SBOM",
			Error: "Failed to parse SBOM: " + err.Error(),
		})
		return
	}

	var goMods, osPkgs []sbomRow
	for _, c := range bom.Components {
		row := sbomRow{
			Name:    c.Name,
			Version: c.Version,
			License: componentLicense(c),
		}
		switch {
		case strings.HasPrefix(c.PURL, "pkg:golang/"):
			goMods = append(goMods, row)
		case strings.HasPrefix(c.PURL, "pkg:apk/"):
			osPkgs = append(osPkgs, row)
		}
	}

	sort.Slice(goMods, func(i, j int) bool { return goMods[i].Name < goMods[j].Name })
	sort.Slice(osPkgs, func(i, j int) bool { return osPkgs[i].Name < osPkgs[j].Name })

	s.render(w, "sbom.html", sbomData{
		Title:      "SBOM",
		Format:     bom.BOMFormat + " " + bom.SpecVersion,
		GoModules:  goMods,
		OSPackages: osPkgs,
		Total:      len(goMods) + len(osPkgs),
	})
}

func (s *Server) handleSBOMJSON(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(SBOMPath)
	if err != nil {
		http.Error(w, "SBOM not available", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="sbom.cdx.json"`)
	w.Write(data)
}

func componentLicense(c cdxComponent) string {
	if len(c.Licenses) == 0 {
		return ""
	}
	l := c.Licenses[0].License
	if l.ID != "" {
		return l.ID
	}
	return l.Name
}

func (s *Server) handleRefreshBanks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	s.invalidateASPSPCache()
	http.Redirect(w, r, "/connect", http.StatusFound)
}

func (s *Server) invalidateASPSPCache() {
	_ = s.st.SetSetting("aspsp_cache", "")
	_ = s.st.SetSetting("aspsp_cache_at", "")
}

func (s *Server) aspspCacheAge() string {
	cachedAt, _ := s.st.GetSetting("aspsp_cache_at")
	if cachedAt == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, cachedAt)
	if err != nil {
		return ""
	}
	return humanizeAge(time.Since(t))
}

func humanizeAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	default:
		return plural(int(d.Hours()/24), "day") + " ago"
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func isWrongASPSPError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "WRONG_ASPSP_PROVIDED")
}

func (s *Server) getASPSPs() ([]enablebanking.ASPSP, error) {
	cachedJSON, _ := s.st.GetSetting("aspsp_cache")
	cachedAt, _ := s.st.GetSetting("aspsp_cache_at")

	if cachedJSON != "" && cachedAt != "" {
		if t, err := time.Parse(time.RFC3339, cachedAt); err == nil && time.Since(t) < 24*time.Hour {
			var banks []enablebanking.ASPSP
			if err := json.Unmarshal([]byte(cachedJSON), &banks); err == nil {
				return banks, nil
			}
		}
	}

	banks, err := s.eb.GetASPSPs(context.Background())
	if err != nil {
		return nil, err
	}
	if data, err := json.Marshal(banks); err == nil {
		_ = s.st.SetSetting("aspsp_cache", string(data))
		_ = s.st.SetSetting("aspsp_cache_at", time.Now().UTC().Format(time.RFC3339))
	}
	return banks, nil
}

func trustProxyHeaders() bool {
	return strings.EqualFold(os.Getenv("TRUSTED_PROXY"), "true")
}

func detectBaseURL(r *http.Request, st *store.Store) string {
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	host := r.Host

	if trustProxyHeaders() {
		if v := r.Header.Get("X-Forwarded-Proto"); v == "http" || v == "https" {
			scheme = v
		}
		if v := r.Header.Get("X-Forwarded-Host"); isValidHost(v) {
			host = v
		}
	}

	if isValidHost(host) {
		base := scheme + "://" + host
		_ = st.SetSetting("eb_base_url", base)
		return base
	}
	if stored, _ := st.GetSetting("eb_base_url"); stored != "" {
		return stored
	}
	return "https://localhost:8443"
}

func isValidHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for _, c := range host {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '-' || c == ':' || c == '[' || c == ']':
		default:
			return false
		}
	}
	return true
}

func uniqueCountries(banks []enablebanking.ASPSP) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, b := range banks {
		if _, ok := seen[b.Country]; !ok {
			seen[b.Country] = struct{}{}
			out = append(out, b.Country)
		}
	}
	return out
}

func urlEncode(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteRune(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func (s *Server) consentValidity(bankName, bankCountry string) time.Duration {
	banks, err := s.getASPSPs()
	if err != nil {
		return enablebanking.DefaultConsentValidity
	}
	for _, b := range banks {
		if b.Name == bankName && b.Country == bankCountry {
			return b.ConsentValidity()
		}
	}
	return enablebanking.DefaultConsentValidity
}

func isRequestTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

// OpeningBalancePreview is what the confirmation page shows before anything is
// written, and what the apply path returns afterwards.
type OpeningBalancePreview struct {
	AccountID     int64
	BankName      string
	BudgetAccount string
	Currency      string
	BalanceType   string
	ReferenceDate string
	BankCents     int64
	ImportedCents int64
	OpeningCents  int64
	OpeningDate   string
	// AvailableBalance marks a figure derived from an available balance rather
	// than a booked one, which carries the overdraft caveat.
	AvailableBalance bool
	Applied          bool
	Refusal          string
}

// SetOpeningBalanceFunc injects the balance work, which belongs to the syncer:
// it owns the backend connection and the run lock. The web package stays free of
// both, matching how SetBackendStatus is wired.
func (s *Server) SetOpeningBalanceFunc(
	fn func(ctx context.Context, accountID int64, apply bool, expected *int64) (OpeningBalancePreview, error),
) {
	s.openingBalance = fn
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	type settingsData struct {
		Title             string
		DriftNotifyCents  string
		ToleranceCents    string
		TolerancePercent  string
		PayeePrefixes     string
		AutoProbability   string
		ReviewProbability string
		MatchOverlap      string
		AskWhenUnsure     bool
		Saved             bool
		Error             string
	}

	render := func(msg string, saved bool) {
		t := s.st.Tunables()
		s.render(w, "settings.html", settingsData{
			Title:             "Settings",
			DriftNotifyCents:  strconv.FormatInt(t.DriftNotifyCents, 10),
			ToleranceCents:    strconv.FormatInt(t.ToleranceCents, 10),
			TolerancePercent:  strconv.Itoa(t.TolerancePercent),
			PayeePrefixes:     strings.Join(t.PayeePrefixes, ","),
			AutoProbability:   strconv.Itoa(t.AutoProbabilityPct),
			ReviewProbability: strconv.Itoa(t.ReviewProbabilityPct),
			MatchOverlap:      strconv.Itoa(t.OverlapPct),
			AskWhenUnsure:     t.AskWhenUnsure,
			Saved:             saved,
			Error:             msg,
		})
	}

	if r.Method != http.MethodPost {
		render("", false)
		return
	}

	keys := []string{
		store.SettingDriftNotifyCents,
		store.SettingToleranceCents,
		store.SettingTolerancePercent,
		store.SettingPayeePrefixes,
		store.SettingAutoProbability,
		store.SettingReviewProbability,
		store.SettingMatchOverlap,
		store.SettingAskWhenUnsure,
	}

	// Validated in full before anything is written. The two match thresholds
	// constrain each other, so a form rejected halfway through would leave the
	// pair in a state the user never asked for and the read path would then have
	// to repair.
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		value, err := store.ValidateTunable(key, r.FormValue(key))
		if err != nil {
			render(err.Error(), false)
			return
		}
		values[key] = value
	}
	if err := store.ValidateTunableSet(values); err != nil {
		render(err.Error(), false)
		return
	}

	// What changed, and from what. Two of these keys are matching policy: moving
	// the automatic threshold changes what gets merged without anybody being
	// asked, and without a record there is no way to tell afterwards from when.
	before := s.st.Tunables()
	changed := make([]logs.KV, 0, len(keys))
	for _, key := range keys {
		if err := s.st.SetSetting(key, values[key]); err != nil {
			render("Failed to store "+key+": "+err.Error(), false)
			return
		}
	}
	after := s.st.Tunables()
	for name, pair := range map[string][2]string{
		"amount_tolerance_pct":   {strconv.Itoa(before.TolerancePercent), strconv.Itoa(after.TolerancePercent)},
		"amount_tolerance_cap":   {strconv.FormatInt(before.ToleranceCents, 10), strconv.FormatInt(after.ToleranceCents, 10)},
		"payee_prefixes":         {strings.Join(before.PayeePrefixes, ","), strings.Join(after.PayeePrefixes, ",")},
		"auto_probability_pct":   {strconv.Itoa(before.AutoProbabilityPct), strconv.Itoa(after.AutoProbabilityPct)},
		"review_probability_pct": {strconv.Itoa(before.ReviewProbabilityPct), strconv.Itoa(after.ReviewProbabilityPct)},
		"match_overlap_pct":      {strconv.Itoa(before.OverlapPct), strconv.Itoa(after.OverlapPct)},
		"drift_notify_cents":     {strconv.FormatInt(before.DriftNotifyCents, 10), strconv.FormatInt(after.DriftNotifyCents, 10)},
		"ask_when_unsure":        {strconv.FormatBool(before.AskWhenUnsure), strconv.FormatBool(after.AskWhenUnsure)},
	} {
		if pair[0] != pair[1] {
			changed = append(changed, logs.String(name, pair[0]+" -> "+pair[1]))
		}
	}
	if len(changed) > 0 {
		olog.Info(r.Context(), "settings.changed", changed...)
	}
	render("", true)
}

func (s *Server) handleOpeningBalance(w http.ResponseWriter, r *http.Request) {
	type openingData struct {
		Title   string
		Preview OpeningBalancePreview
		Error   string
	}

	if s.openingBalance == nil {
		http.Error(w, "opening balances are not available", http.StatusServiceUnavailable)
		return
	}
	id, err := strconv.ParseInt(r.FormValue("account_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid account", http.StatusBadRequest)
		return
	}

	apply := r.Method == http.MethodPost
	var expected *int64
	if apply {
		if v, err := strconv.ParseInt(r.FormValue("expected_cents"), 10, 64); err == nil {
			expected = &v
		}
	}

	preview, err := s.openingBalance(r.Context(), id, apply, expected)
	if err != nil {
		s.render(w, "opening_balance.html", openingData{Title: "Opening Balance", Error: err.Error()})
		return
	}
	if preview.Applied {
		http.Redirect(w, r, "/status", http.StatusFound)
		return
	}
	s.render(w, "opening_balance.html", openingData{Title: "Opening Balance", Preview: preview})
}

// ReviewCandidate is one existing budget row offered as the counterpart of a
// held transaction.
//
// Candidates are recomputed whenever the page is drawn rather than stored with
// the held transaction. A row saved days ago may since have been split, edited
// or deleted, so a stored candidate could name something that no longer exists;
// and on the Actual backend the listing call is also what warms the in-process
// map that a subsequent update depends on.
type ReviewCandidate struct {
	ID   string
	Date string
	// Amount arrives already formatted. The syncer owns the money formatter and
	// this package is a presentation layer; a second implementation of the same
	// rounding is how two screens end up disagreeing about a figure.
	Amount    string
	PayeeName string

	// Percent is the match probability as shown to the user, rounded the way the
	// page prints it. The resolve step is checked against this figure, so it has
	// to be the rounded one rather than the exact value behind it.
	Percent int

	// Why names the comparison levels that produced the figure — "amount exact,
	// payee truncated, same day". The model gives this away for nothing, since
	// every field contributes its own term, and a reason is what makes the
	// number possible to disagree with.
	Why string
}

// ReviewItem is one held transaction together with what it might belong to.
type ReviewItem struct {
	ID int64
	// ParamVersion identifies the parameters the candidates below were worked
	// out with. It travels back with the answer, because a threshold can move
	// between the page being drawn and the button being pressed without moving
	// any probability at all — and then the figure the page showed is right and
	// the decision it belongs to is not.
	ParamVersion  string
	BankName      string
	BudgetAccount string
	Date          string
	Amount        string
	Currency      string
	Payee         string
	HeldAt        string
	Candidates    []ReviewCandidate

	// Unavailable explains why this row has no candidates to offer, when the
	// reason is a failure rather than an empty window. A held transaction whose
	// account cannot be reached still has to be visible: the whole point of the
	// queue is that nothing disappears quietly.
	Unavailable string
}

// InquiryItem is one decision the matcher made on its own that the user is being
// asked to confirm.
//
// Everything on it is a snapshot taken when the decision was made. The budget
// row may have been edited since — on a merge it certainly has been, by the
// merge itself — and showing its present state would be showing the question its
// own answer.
type InquiryItem struct {
	ID       int64
	BankName string

	// Merged says which way the matcher went, and is only used to describe what
	// happened. The question itself is the same either way: are these two rows
	// one payment? Asking "was this right?" instead would make a yes mean
	// opposite things on a merge and on an import, which is the reliable way to
	// collect wrong answers.
	Merged  bool
	Percent int

	ParamVersion string
	AskedAt      string

	Date     string
	Amount   string
	Currency string
	Payee    string

	CandidateDate   string
	CandidateAmount string
	CandidatePayee  string
	Why             string
}

// PromotionCheck is one finding about a candidate parameter set.
type PromotionCheck struct {
	Name   string
	Status string
	Detail string
}

// PromotionView is the state of the parameter-promotion page.
type PromotionView struct {
	// InForce and Candidate are parameter versions — the identity of a whole
	// effective parameter set, and the label the decisions in the metrics carry.
	InForce   string
	Candidate string

	// Labelled is how many settled decisions the candidate rests on.
	Labelled int

	// Watching means the candidate is being evaluated alongside the one in
	// force, without acting on it. Fitted means the parameters currently in
	// force are themselves a promotion rather than the shipped ones.
	Watching bool
	Fitted   bool

	// Promotable is whether the automatic checks leave the decision open to a
	// person. It is never on its own a reason to promote.
	Promotable bool

	Checks []PromotionCheck
}

// Promotions is the parameter-promotion work, which belongs to the syncer for
// the same reason the review queue does: it needs the matching policy and the
// decision log, and the web layer holds neither.
type Promotions interface {
	// PromotionPage reports what could be promoted and what is known about it.
	PromotionPage(ctx context.Context) (PromotionView, error)

	// WatchTrial starts evaluating the named candidate without acting on it,
	// StopWatching drops it.
	WatchTrial(ctx context.Context, version string) error
	StopWatching(ctx context.Context) error

	// PromoteTrial puts the watched candidate into force; RevertParameters puts
	// the shipped ones back.
	PromoteTrial(ctx context.Context, version string) error
	RevertParameters(ctx context.Context) error
}

// SetPromotions injects the promotion work.
func (s *Server) SetPromotions(p Promotions) { s.promotions = p }

// ReviewQueue is the held-transaction work, which belongs to the syncer: it owns
// the backend connection and the run lock, and this package holds neither.
//
// It is an interface rather than a pair of injected functions because the two
// halves are not independent — resolving a decision means recomputing the same
// candidates the listing showed, and an implementation that got one right and
// the other wrong would act on something the user never saw.
type ReviewQueue interface {
	// HeldTransactions lists everything awaiting a decision, with its candidates.
	HeldTransactions(ctx context.Context) ([]ReviewItem, error)

	// ResolveHeld carries out one decision. An empty candidateID means "this is
	// a new transaction"; otherwise the held transaction is merged into that
	// candidate. shownPercent is the figure the user was looking at and
	// paramVersion the parameters that produced it, both of which the
	// implementation must check against freshly computed ones.
	ResolveHeld(ctx context.Context, id int64, candidateID string, shownPercent int, paramVersion string) error

	// PendingInquiry returns the one decision the matcher settled alone that the
	// user is being asked to confirm, or nil. Unlike the queue it blocks
	// nothing: the transaction is already in the budget whichever way the
	// answer goes.
	PendingInquiry(ctx context.Context) (*InquiryItem, error)

	// AnswerInquiry records the reply. A nil answer is "I do not know", which
	// closes the question without labelling the decision — the one outcome that
	// must stay available, because a guessed label is worse than none.
	AnswerInquiry(ctx context.Context, id int64, answer *bool, paramVersion string) error
}

// SetReviewQueue injects the queue work, matching how SetOpeningBalanceFunc and
// SetBackendStatus are wired.
func (s *Server) SetReviewQueue(q ReviewQueue) { s.review = q }

// refusal marks an error as one the user can act on — a stale page, a candidate
// that is gone, a sync holding the lock — as opposed to a fault in the program
// or an unreachable backend.
//
// The distinction exists for the severity of what gets recorded, not for the
// page: both render the same way. Without it every refusal would either be
// logged as an error, which makes a working guard look like a defect, or as a
// warning, which buries a backend outage among them.
type refusal struct{ err error }

func (r refusal) Error() string { return r.err.Error() }
func (r refusal) Unwrap() error { return r.err }

// Refuse wraps an error the user is expected to see and act on.
func Refuse(format string, a ...any) error { return refusal{fmt.Errorf(format, a...)} }

// IsRefusal reports whether an error is the user's to resolve.
func IsRefusal(err error) bool {
	var r refusal
	return errors.As(err, &r)
}

type matchingData struct {
	Title string
	View  PromotionView
	Saved string
	Error string
}

func (s *Server) handleMatching(w http.ResponseWriter, r *http.Request) {
	s.renderMatching(w, r, "", "")
}

func (s *Server) renderMatching(w http.ResponseWriter, r *http.Request, saved, msg string) {
	if s.promotions == nil {
		http.Error(w, "parameter promotion is not available", http.StatusServiceUnavailable)
		return
	}
	view, err := s.promotions.PromotionPage(r.Context())
	if err != nil {
		s.recordReviewProblem(r.Context(), "matching", err)
		if msg == "" {
			msg = err.Error()
		}
	}
	s.render(w, "matching.html", matchingData{
		Title: "Matching parameters", View: view, Saved: saved, Error: msg})
}

// handleMatchingApply carries out one of the four things a person can do to the
// matching parameters.
//
// All four are a person's decision and none of them happens on its own. The
// program can say that a candidate passes its checks; it cannot say that
// changing how money is matched is a good idea today.
func (s *Server) handleMatchingApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if s.promotions == nil {
		http.Error(w, "parameter promotion is not available", http.StatusServiceUnavailable)
		return
	}

	version := r.FormValue("param_version")
	var err error
	var saved string
	switch action := r.FormValue("action"); action {
	case "watch":
		err, saved = s.promotions.WatchTrial(r.Context(), version),
			"Now watching these parameters. Nothing has changed — the next syncs will "+
				"record what they would have decided differently."
	case "stop":
		err, saved = s.promotions.StopWatching(r.Context()), "Stopped watching."
	case "promote":
		err, saved = s.promotions.PromoteTrial(r.Context(), version),
			"These parameters are now in force."
	case "revert":
		err, saved = s.promotions.RevertParameters(r.Context()),
			"The shipped parameters are back in force."
	default:
		s.recordReviewProblem(r.Context(), "matching", Refuse("unknown action %q", action))
		s.renderMatching(w, r, "", "That was not one of the choices.")
		return
	}
	if err != nil {
		s.recordReviewProblem(r.Context(), "matching", err)
		s.renderMatching(w, r, "", err.Error())
		return
	}
	s.renderMatching(w, r, saved, "")
}

// recordReviewProblem puts a failed review interaction where an operator can
// find it.
//
// Both outcomes render as a 200 with the reason on the page, which is right for
// a form the user should correct in place — but it means the request-level
// warning on 4xx and 5xx never fires. Left at that, a person failing repeatedly
// to resolve a decision would produce nothing at all in logs, traces or metrics,
// which is the same silence the queue itself was built to end.
func (s *Server) recordReviewProblem(ctx context.Context, op string, err error) {
	span := trace.SpanFromContext(ctx)
	if IsRefusal(err) {
		span.SetAttributes(
			attribute.String("review.op", op),
			attribute.String("review.refused", err.Error()))
		olog.Warn(ctx, "review.refused",
			logs.String("op", op),
			logs.String("reason", err.Error()))
		s.countReviewProblem(ctx, op, "refused")
		return
	}
	s.countReviewProblem(ctx, op, "failed")
	span.RecordError(err)
	span.SetStatus(codes.Error, "review "+op+" failed")
	olog.Error(ctx, "review.failed",
		logs.String("op", op),
		logs.String("error", err.Error()))
}

type reviewData struct {
	Title   string
	Items   []ReviewItem
	Inquiry *InquiryItem
	Error   string
}

func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	s.renderReview(w, r, "")
}

func (s *Server) renderReview(w http.ResponseWriter, r *http.Request, msg string) {
	if s.review == nil {
		http.Error(w, "the review queue is not available", http.StatusServiceUnavailable)
		return
	}
	items, err := s.review.HeldTransactions(r.Context())
	if err != nil {
		s.recordReviewProblem(r.Context(), "list", err)
		if msg == "" {
			msg = err.Error()
		}
		items = nil
	}
	// A confirmation that cannot be read is not worth an error on this page.
	// The queue is the part somebody came here for, and it is still usable.
	inquiry, err := s.review.PendingInquiry(r.Context())
	if err != nil {
		s.recordReviewProblem(r.Context(), "inquiry", err)
		inquiry = nil
	}
	s.render(w, "review.html", reviewData{
		Title: "Review", Items: items, Inquiry: inquiry, Error: msg})
}

// handleReviewConfirm takes the answer to a confirmation request.
func (s *Server) handleReviewConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if s.review == nil {
		http.Error(w, "the review queue is not available", http.StatusServiceUnavailable)
		return
	}

	id, err := strconv.ParseInt(r.FormValue("inquiry_id"), 10, 64)
	if err != nil {
		s.recordReviewProblem(r.Context(), "confirm", Refuse("unreadable confirmation id %q", r.FormValue("inquiry_id")))
		s.renderReview(w, r, "That question no longer exists.")
		return
	}

	// Three answers, not two. "I do not know" has to be a button rather than the
	// absence of one, or the only way out of the question is to guess at it.
	var answer *bool
	switch v := r.FormValue("answer"); v {
	case "yes":
		t := true
		answer = &t
	case "no":
		f := false
		answer = &f
	case "unknown":
	default:
		s.recordReviewProblem(r.Context(), "confirm", Refuse("unreadable answer %q", v))
		s.renderReview(w, r, "That answer was not one of the choices.")
		return
	}

	if err := s.review.AnswerInquiry(r.Context(), id, answer, r.FormValue("param_version")); err != nil {
		s.recordReviewProblem(r.Context(), "confirm", err)
		s.renderReview(w, r, err.Error())
		return
	}
	http.Redirect(w, r, "/review", http.StatusFound)
}

func (s *Server) handleReviewResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if s.review == nil {
		http.Error(w, "the review queue is not available", http.StatusServiceUnavailable)
		return
	}

	id, err := strconv.ParseInt(r.FormValue("review_id"), 10, 64)
	if err != nil {
		s.recordReviewProblem(r.Context(), "resolve", Refuse("unreadable review id %q", r.FormValue("review_id")))
		s.renderReview(w, r, "That decision no longer exists.")
		return
	}
	// Absent rather than malformed means "this is new", so an unparseable figure
	// is only an error when a candidate was actually chosen.
	candidateID := r.FormValue("candidate_id")
	shown := 0
	if candidateID != "" {
		n, err := strconv.Atoi(r.FormValue("shown_percent"))
		if err != nil {
			s.recordReviewProblem(r.Context(), "resolve", Refuse("no probability submitted with candidate %q", candidateID))
			s.renderReview(w, r, "The page was out of date. Look at it again.")
			return
		}
		shown = n
	}

	if err := s.review.ResolveHeld(r.Context(), id, candidateID, shown, r.FormValue("param_version")); err != nil {
		s.recordReviewProblem(r.Context(), "resolve", err)
		s.renderReview(w, r, err.Error())
		return
	}
	http.Redirect(w, r, "/review", http.StatusFound)
}
