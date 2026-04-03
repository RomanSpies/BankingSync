package web

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"bankingsync/enablebanking"
	"bankingsync/store"

	"github.com/google/uuid"
)

// AppVersion is set by the main package at startup.
var AppVersion string

// SyncTriggerFunc is called when the user requests a manual sync from the web UI.
type SyncTriggerFunc func()

// Server is the embedded web UI and health endpoint server.
type Server struct {
	st         *store.Store
	eb         *enablebanking.Client
	trigger    SyncTriggerFunc
	testEmail  func() error
	templateFS fs.FS

	mu          sync.Mutex
	syncRunning bool

	mux *http.ServeMux
	srv *http.Server
}

// NewFromDir creates the Server using templates loaded from the "web/templates"
// subdirectory on disk. It is the standard constructor for production use.
func NewFromDir(st *store.Store, eb *enablebanking.Client, trigger SyncTriggerFunc, testEmail func() error) (*Server, error) {
	return New(st, eb, trigger, testEmail, os.DirFS("web"))
}

// New creates the Server, registers all routes, and validates templates from templateFS.
func New(st *store.Store, eb *enablebanking.Client, trigger SyncTriggerFunc, testEmail func() error, templateFS fs.FS) (*Server, error) {
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
	s.mux.HandleFunc("/callback", s.handleCallback)
	s.mux.HandleFunc("/pick-account", s.handlePickAccount)
	s.mux.HandleFunc("/status", s.handleStatus)
	s.mux.HandleFunc("/renew", s.handleRenew)
	s.mux.HandleFunc("/remove-account", s.handleRemoveAccount)
	s.mux.HandleFunc("/reset-sync", s.handleResetSync)
	s.mux.HandleFunc("/sync/now", s.handleSyncNow)
	s.mux.HandleFunc("/test-email", s.handleTestEmail)
	s.mux.HandleFunc("/health", s.handleHealth)

	return s, nil
}

// Mux returns the underlying ServeMux so callers can register additional routes.
func (s *Server) Mux() *http.ServeMux { return s.mux }

// StartTLS begins listening with TLS using the provided cert and key files.
// It blocks until the server stops.
func (s *Server) StartTLS(addr, certFile, keyFile string) error {
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("Web UI → https://localhost%s", addr)
	if err := s.srv.ListenAndServeTLS(certFile, keyFile); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
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
	_ = r.ParseMultipartForm(4 << 20)

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
			block, _ := pem.Decode(pemBytes)
			if block == nil || !strings.Contains(block.Type, "PRIVATE KEY") {
				s.render(w, "setup.html", setupData{Title: "Setup", Error: "Invalid PEM file — expected a private key."})
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
	authURL, err := s.eb.StartAuth(bankName, bankCountry, psuType, stateUUID, appBaseURL)
	if err != nil {
		banks, _ := s.getASPSPs()
		s.render(w, "connect.html", connectData{
			Title:     "Connect Bank",
			Error:     "Failed to start authorisation: " + err.Error(),
			Banks:     banks,
			Countries: uniqueCountries(banks),
			Accounts:  accounts,
		})
		return
	}

	banks, _ := s.getASPSPs()
	s.render(w, "connect.html", connectData{
		Title:     "Connect Bank",
		AuthURL:   authURL,
		Banks:     banks,
		Countries: uniqueCountries(banks),
		Accounts:  accounts,
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

	sr, err := s.eb.CompleteAuth(code, rawState)
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
	expiry := time.Now().UTC().Add(180 * 24 * time.Hour).Format(time.RFC3339)
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

func (s *Server) handlePickAccount(w http.ResponseWriter, r *http.Request) {
	type pickData struct {
		Title            string
		SessionID        string
		BankName         string
		BankCountry      string
		Expiry           string
		Accounts         []enablebanking.SessionAccount
		DefaultAccount   string
		DefaultStartDate string
		Error            string
	}

	sessionID, _ := s.st.GetSetting("pending_auth_session_id")
	accountsJSON, _ := s.st.GetSetting("pending_auth_accounts")
	expiry, _ := s.st.GetSetting("pending_auth_expiry")
	bankName, _ := s.st.GetSetting("pending_auth_bank_name")
	bankCountry, _ := s.st.GetSetting("pending_auth_bank_country")
	defaultAccount := os.Getenv("ACTUAL_ACCOUNT")
	if defaultAccount == "" {
		defaultAccount = "Revolut"
	}
	defaultStart := time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02")

	var accounts []enablebanking.SessionAccount
	_ = json.Unmarshal([]byte(accountsJSON), &accounts)

	if r.Method == http.MethodGet {
		s.render(w, "pick_account.html", pickData{
			Title:            "Select Account",
			SessionID:        sessionID,
			BankName:         bankName,
			BankCountry:      bankCountry,
			Expiry:           expiry,
			Accounts:         accounts,
			DefaultAccount:   defaultAccount,
			DefaultStartDate: defaultStart,
		})
		return
	}

	uid := r.FormValue("account_uid")
	actualAccount := strings.TrimSpace(r.FormValue("actual_account"))
	startDate := strings.TrimSpace(r.FormValue("start_sync_date"))
	if uid == "" {
		s.render(w, "pick_account.html", pickData{
			Title:            "Select Account",
			Accounts:         accounts,
			DefaultAccount:   defaultAccount,
			DefaultStartDate: defaultStart,
			Error:            "Please select an account.",
		})
		return
	}
	if actualAccount == "" {
		actualAccount = defaultAccount
	}
	if startDate == "" {
		startDate = defaultStart
	}

	if _, err := s.st.AddBankAccount(sessionID, uid, bankName, bankCountry, actualAccount, startDate, expiry); err != nil {
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
		DaysLeft int
	}
	type statusData struct {
		Title           string
		Accounts        []accountRow
		LastSync        string
		SyncRunning     bool
		SyncLogs        []store.SyncLog
		UpdateAvailable string
	}

	accounts, _ := s.st.GetAllBankAccounts()
	rows := make([]accountRow, 0, len(accounts))
	for _, a := range accounts {
		row := accountRow{BankAccount: a}
		if t, err := time.Parse(time.RFC3339, a.SessionExpiry); err == nil {
			row.DaysLeft = int(time.Until(t).Hours() / 24)
		}
		rows = append(rows, row)
	}
	syncLogs, _ := s.st.GetSyncLogs(20)
	updateAvail, _ := s.st.GetSetting("update_available")

	lastSync, _ := s.st.GetLastSyncDate()

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
	authURL, err := s.eb.StartAuth(bankName, bankCountry, "personal", stateUUID, appBaseURL)
	if err != nil {
		http.Redirect(w, r, "/connect?error="+urlEncode("Failed to start renewal: "+err.Error()), http.StatusFound)
		return
	}

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
		s.trigger()
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
	if err := s.testEmail(); err != nil {
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

	resp := map[string]any{
		"status":             status,
		"version":            AppVersion,
		"connected_accounts": len(accounts),
		"expiring_sessions":  expiring,
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

	banks, err := s.eb.GetASPSPs()
	if err != nil {
		return nil, err
	}
	if data, err := json.Marshal(banks); err == nil {
		_ = s.st.SetSetting("aspsp_cache", string(data))
		_ = s.st.SetSetting("aspsp_cache_at", time.Now().UTC().Format(time.RFC3339))
	}
	return banks, nil
}

func detectBaseURL(r *http.Request, st *store.Store) string {
	scheme := "https"
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = v
	} else if r.TLS == nil {
		scheme = "http"
	}
	host := r.Host
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		host = v
	}
	if host != "" {
		base := scheme + "://" + host
		_ = st.SetSetting("eb_base_url", base)
		return base
	}
	if stored, _ := st.GetSetting("eb_base_url"); stored != "" {
		return stored
	}
	return "https://localhost:8443"
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
