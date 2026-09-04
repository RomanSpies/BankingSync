// Package fireflytest provides a stateful in-memory stand-in for the Firefly III
// v1 API.
//
// It reproduces the behaviours that make the real API dangerous to write
// against: a PUT that omits a split destroys it, a tags field replaces the whole
// set, dates come back with the server's own offset, and the transaction search
// is global rather than account-scoped. A fixture that got any of these wrong
// would let a broken client look correct.
package fireflytest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// DefaultToken is the bearer token the server accepts.
	DefaultToken = "test-token"

	// DefaultPerPage is deliberately tiny so every list call paginates.
	DefaultPerPage = 2

	// DefaultOffset is deliberately not UTC so date handling is exercised.
	DefaultOffset = "+02:00"

	// DefaultVersion is what /about reports.
	DefaultVersion = "6.6.6"
)

// Server is a stateful Firefly III stand-in.
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	token    string
	perPage  int
	offset   string
	version  string
	accounts []*Account
	groups   []*Group

	nextAccount int
	nextGroup   int
	nextJournal int

	failWrites  int
	rateLimits  int
	requestLog  []string
	writeCount  int
	dropResults int

	searchContains bool
}

// New starts a server preloaded with nothing. Callers seed it through the
// exported helpers.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		token:   DefaultToken,
		perPage: DefaultPerPage,
		offset:  DefaultOffset,
		version: DefaultVersion,
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.route))
	t.Cleanup(s.Close)
	return s
}

// Token returns the bearer token the server expects.
func (s *Server) Token() string { return s.token }

// SetPerPage changes the page size the server reports and honours.
func (s *Server) SetPerPage(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.perPage = n
}

// SetOffset changes the timezone offset dates are rendered with.
func (s *Server) SetOffset(off string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offset = off
}

// FailNextWrites makes the next n write requests answer 500.
func (s *Server) FailNextWrites(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failWrites = n
}

// RateLimitNext makes the next n requests answer 429 with Retry-After: 0.
func (s *Server) RateLimitNext(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimits = n
}

// DropNextResponses applies the next n writes and then closes the connection
// without answering, reproducing a lost response over a durable change.
func (s *Server) DropNextResponses(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropResults = n
}

// SetSearchContains degrades external_id_is to a substring match, reproducing a
// silent regression of Firefly's search DSL. A client that trusts the operator
// without verifying the returned value adopts the wrong transaction.
func (s *Server) SetSearchContains(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.searchContains = v
}

// Requests returns the method and path of every request served so far.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requestLog...)
}

// AddAccount seeds an account and returns it.
func (s *Server) AddAccount(name, accountType, currency, iban string) *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addAccountLocked(name, accountType, currency, iban)
}

func (s *Server) addAccountLocked(name, accountType, currency, iban string) *Account {
	s.nextAccount++
	a := &Account{
		ID:           strconv.Itoa(s.nextAccount),
		Name:         name,
		Type:         accountType,
		CurrencyCode: currency,
		IBAN:         iban,
	}
	if accountType == "asset" {
		a.Role = "defaultAsset"
	}
	s.accounts = append(s.accounts, a)
	return a
}

// AddGroup seeds a single-split transaction group and returns it.
func (s *Server) AddGroup(sp Split) *Group {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addGroupLocked([]*Split{&sp})
}

func (s *Server) addGroupLocked(splits []*Split) *Group {
	s.nextGroup++
	g := &Group{ID: strconv.Itoa(s.nextGroup)}
	for _, sp := range splits {
		s.nextJournal++
		sp.JournalID = strconv.Itoa(s.nextJournal)
		if sp.Date == "" {
			sp.Date = s.renderDateLocked(time.Now().UTC())
		}
		g.Splits = append(g.Splits, sp)
	}
	s.groups = append(s.groups, g)
	return g
}

// Groups returns a snapshot of the stored transaction groups.
func (s *Server) Groups() []*Group {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Group, 0, len(s.groups))
	for _, g := range s.groups {
		cp := &Group{ID: g.ID, Title: g.Title}
		for _, sp := range g.Splits {
			cp.Splits = append(cp.Splits, sp.clone())
		}
		out = append(out, cp)
	}
	return out
}

// Accounts returns a snapshot of the stored accounts.
func (s *Server) Accounts() []*Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		cp := *a
		out = append(out, &cp)
	}
	return out
}

// RenameAccount reproduces a user renaming an account in the Firefly UI.
//
// It exists because Accounts() hands out copies: a test that renamed one of those
// would mutate nothing and then pass for the wrong reason.
func (s *Server) RenameAccount(id, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.ID == id {
			a.Name = name
			return nil
		}
	}
	return fmt.Errorf("unknown account %s", id)
}

// SplitGroup reproduces a user splitting our transaction in the UI: the original
// journal stays and a second one appears beside it.
func (s *Server) SplitGroup(groupID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.groupLocked(groupID)
	if g == nil {
		return fmt.Errorf("unknown group %s", groupID)
	}
	extra := g.Splits[0].clone()
	s.nextJournal++
	extra.JournalID = strconv.Itoa(s.nextJournal)
	extra.ExternalID = ""
	extra.Description = g.Splits[0].Description + " (split)"
	g.Splits = append(g.Splits, extra)
	return nil
}

// DeleteGroup reproduces a user deleting the transaction.
func (s *Server) DeleteGroup(groupID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.groups[:0]
	for _, g := range s.groups {
		if g.ID != groupID {
			out = append(out, g)
		}
	}
	s.groups = out
}

// FormatDate renders a day the way this server does, for use in expectations.
func (s *Server) FormatDate(d time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renderDateLocked(d)
}

// storeDateLocked mirrors how Firefly treats an incoming date. A value carrying
// an offset is an *instant* and comes back rendered in the server's own offset;
// one without an offset is wall-clock time and keeps its calendar day. The
// distinction is invisible on a UTC server and shifts the day by one on any
// server west of UTC — which is exactly why storing the string verbatim, as this
// fixture used to, could never surface that class of bug.
func (s *Server) storeDateLocked(raw string) string {
	loc := s.zoneLocked()
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.In(loc).Format(wireDateLayout)
	}
	if t, err := time.Parse("2006-01-02T15:04:05", raw); err == nil {
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc).
			Format(wireDateLayout)
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc).Format(wireDateLayout)
	}
	return raw
}

const wireDateLayout = "2006-01-02T15:04:05-07:00"

func (s *Server) zoneLocked() *time.Location {
	t, err := time.Parse("-07:00", s.offset)
	if err != nil {
		return time.UTC
	}
	_, secs := t.Zone()
	return time.FixedZone(s.offset, secs)
}

func (s *Server) renderDateLocked(d time.Time) string {
	y, m, day := d.Date()
	return fmt.Sprintf("%04d-%02d-%02dT00:00:00%s", y, int(m), day, s.offset)
}

func (s *Server) accountType(id string) string {
	for _, a := range s.accounts {
		if a.ID == id {
			return a.Type
		}
	}
	return ""
}

func (s *Server) groupLocked(id string) *Group {
	for _, g := range s.groups {
		if g.ID == id {
			return g
		}
	}
	return nil
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requestLog = append(s.requestLog, r.Method+" "+r.URL.Path)
	if s.rateLimits > 0 {
		s.rateLimits--
		s.mu.Unlock()
		w.Header().Set("Retry-After", "0")
		writeJSON(w, http.StatusTooManyRequests, errorBody("rate limited"))
		return
	}
	s.mu.Unlock()

	if r.Header.Get("Authorization") != "Bearer "+s.token {
		writeJSON(w, http.StatusUnauthorized, errorBody("Unauthenticated."))
		return
	}

	path := r.URL.Path
	switch {
	case path == "/api/v1/about" && r.Method == http.MethodGet:
		s.handleAbout(w)
	case path == "/api/v1/accounts" && r.Method == http.MethodGet:
		s.handleListAccounts(w, r)
	case path == "/api/v1/accounts" && r.Method == http.MethodPost:
		s.handleCreateAccount(w, r)
	case path == "/api/v1/search/accounts" && r.Method == http.MethodGet:
		s.handleSearchAccounts(w, r)
	case path == "/api/v1/search/transactions" && r.Method == http.MethodGet:
		s.handleSearchTransactions(w, r)
	case path == "/api/v1/transactions" && r.Method == http.MethodPost:
		s.handleCreateTransaction(w, r)
	case strings.HasPrefix(path, "/api/v1/accounts/") && strings.HasSuffix(path, "/transactions") && r.Method == http.MethodGet:
		s.handleAccountTransactions(w, r)
	case strings.HasPrefix(path, "/api/v1/transactions/") && r.Method == http.MethodGet:
		s.handleGetTransaction(w, r)
	case strings.HasPrefix(path, "/api/v1/transactions/") && r.Method == http.MethodPut:
		s.handleUpdateTransaction(w, r)
	case strings.HasPrefix(path, "/api/v1/accounts/") && r.Method == http.MethodPut:
		s.handleUpdateAccount(w, r)
	case strings.HasPrefix(path, "/api/v1/accounts/") && r.Method == http.MethodGet:
		s.handleGetAccount(w, r)
	default:
		writeJSON(w, http.StatusNotFound, errorBody("Page not found"))
	}
}

func (s *Server) handleAbout(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{"version": s.version, "api_version": "1.0.0", "os": "Linux"},
	})
}

// consumeWriteBudget reports whether a write should be failed or dropped.
func (s *Server) consumeWriteBudget() (fail bool, drop bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeCount++
	if s.failWrites > 0 {
		s.failWrites--
		return true, false
	}
	if s.dropResults > 0 {
		s.dropResults--
		return false, true
	}
	return false, false
}

// writeMissingTransaction answers the way a real Firefly does for a transaction
// that is not there, which is not what one would guess.
//
// Verified against 6.6.6 for a deleted group, an id that never existed and an id
// that is not even a number: all three come back 401 "Unauthenticated", never
// 404. Route model binding cannot resolve the object, so the ownership check
// fails before anything reaches the point of reporting it absent.
//
// The fixture said 404 here for as long as nobody had asked a real instance, and
// firefly.Store mapped only that status to budget.ErrGone — so against the real
// thing a user who deleted a transaction saw "Unauthenticated" in their sync log
// and went looking at their token.
func writeMissingTransaction(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, map[string]any{
		"message":   "Unauthenticated.",
		"exception": "AuthenticationException",
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func page(r *http.Request, fallback int) (int, int) {
	p, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if p < 1 {
		p = 1
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 {
		limit = fallback
	}
	return p, limit
}

func paginate[T any](items []T, current, perPage int) ([]T, map[string]any) {
	total := len(items)
	totalPages := (total + perPage - 1) / perPage
	if totalPages == 0 {
		totalPages = 1
	}
	start := (current - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	slice := items[start:end]
	return slice, map[string]any{
		"total": total, "count": len(slice), "per_page": perPage,
		"current_page": current, "total_pages": totalPages,
	}
}

// MutateSplit applies fn to one split in place, reproducing a change the user
// made in the Firefly UI.
func (s *Server) MutateSplit(groupID, journalID string, fn func(*Split)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.groupLocked(groupID)
	if g == nil {
		return fmt.Errorf("unknown group %s", groupID)
	}
	for _, sp := range g.Splits {
		if sp.JournalID == journalID {
			fn(sp)
			return nil
		}
	}
	return fmt.Errorf("unknown journal %s in group %s", journalID, groupID)
}
