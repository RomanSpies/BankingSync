package main

import (
	"fmt"
	"strings"
	"time"

	"bankingsync/store"
)

// State is the in-memory runtime state for a sync cycle. It is populated from
// the SQLite store at startup and kept in sync via write-through on every
// mutation. Reading is always from memory; writing goes to both memory and the
// store immediately.
type State struct {
	EBSessionID     string
	EBAccountUID    string
	EBSessionExpiry string
	LastSyncDate    string

	PendingMap   map[string]string
	ImportedRefs map[string]string
}

// LoadFromStore populates State from the SQLite store at startup.
func LoadFromStore(st *store.Store) (*State, error) {
	s := &State{
		PendingMap:   make(map[string]string),
		ImportedRefs: make(map[string]string),
	}

	accounts, err := st.GetAllBankAccounts()
	if err != nil {
		return nil, fmt.Errorf("load bank accounts: %w", err)
	}
	if len(accounts) > 0 {
		a := accounts[0]
		s.EBSessionID = a.SessionID
		s.EBAccountUID = a.AccountUID
		s.EBSessionExpiry = a.SessionExpiry
	}

	s.LastSyncDate, err = st.GetLastSyncDate()
	if err != nil {
		return nil, fmt.Errorf("load last sync date: %w", err)
	}

	s.PendingMap, err = st.AllPendingMap()
	if err != nil {
		return nil, fmt.Errorf("load pending map: %w", err)
	}

	s.ImportedRefs, err = st.AllImportedRefs()
	if err != nil {
		return nil, fmt.Errorf("load imported refs: %w", err)
	}

	return s, nil
}

// GetSession returns the stored Enable Banking session credentials and parsed
// expiry time, or an error if the session fields are absent or the expiry is malformed.
func (s *State) GetSession() (sessionID, accountUID string, expiry time.Time, err error) {
	if s.EBSessionID == "" || s.EBAccountUID == "" {
		return "", "", time.Time{}, fmt.Errorf("no session found — connect a bank account via the web UI")
	}

	expiry, err = time.Parse(time.RFC3339, s.EBSessionExpiry)
	if err != nil {
		expiry, err = time.Parse("2006-01-02T15:04:05", s.EBSessionExpiry)
		if err != nil {
			return "", "", time.Time{}, fmt.Errorf("invalid session expiry %q: %w", s.EBSessionExpiry, err)
		}
		expiry = expiry.UTC()
	}
	return s.EBSessionID, s.EBAccountUID, expiry, nil
}

// EarliestPendingDate returns the earliest date found among values of
// PendingMap (format "txnID|date") and whether any valid date was found.
func (s *State) EarliestPendingDate() (time.Time, bool) {
	var earliest time.Time
	found := false
	for _, v := range s.PendingMap {
		_, date := splitPendingVal(v)
		d, err := time.Parse("2006-01-02", date)
		if err != nil {
			continue
		}
		if !found || d.Before(earliest) {
			earliest = d
			found = true
		}
	}
	return earliest, found
}

// splitPendingVal splits a pending map value "txnID|date" into its parts.
func splitPendingVal(val string) (txnID, date string) {
	if i := strings.IndexByte(val, '|'); i >= 0 {
		return val[:i], val[i+1:]
	}
	return val, ""
}

// SetPending adds a pending map entry to both memory and the store.
// The value is stored as "txnID|date".
func (s *State) SetPending(key, txnID, date string, st *store.Store) error {
	val := txnID + "|" + date
	s.PendingMap[key] = val
	return st.SetPending(key, val)
}

// DeletePending removes a pending map entry from both memory and the store.
func (s *State) DeletePending(key string, st *store.Store) error {
	delete(s.PendingMap, key)
	return st.DeletePending(key)
}

// AddImportedRef records an imported ref in both memory and the store.
func (s *State) AddImportedRef(ref, date string, st *store.Store) error {
	s.ImportedRefs[ref] = date
	return st.AddImportedRef(ref, date)
}

// SetLastSyncDate persists the last sync date to both memory and the store.
func (s *State) SetLastSyncDate(date string, st *store.Store) error {
	s.LastSyncDate = date
	return st.SetLastSyncDate(date)
}

// PruneImportedRefs removes stale imported refs from both memory and the store.
func (s *State) PruneImportedRefs(st *store.Store) error {
	updated, err := st.PruneImportedRefs()
	if err != nil {
		return err
	}
	s.ImportedRefs = updated
	return nil
}

// pruneImportedRefs returns a new map containing only refs whose date is within
// the last 21 days. It mirrors the cutoff used by Store.PruneImportedRefs.
func pruneImportedRefs(refs map[string]string) map[string]string {
	cutoff := time.Now().UTC().AddDate(0, 0, -21).Format("2006-01-02")
	out := make(map[string]string, len(refs))
	for ref, date := range refs {
		if date >= cutoff {
			out[ref] = date
		}
	}
	return out
}

// Reload refreshes the in-memory session fields from the store, used after a
// new bank account is connected via the web UI.
func (s *State) Reload(st *store.Store) error {
	accounts, err := st.GetAllBankAccounts()
	if err != nil {
		return err
	}
	if len(accounts) > 0 {
		a := accounts[0]
		s.EBSessionID = a.SessionID
		s.EBAccountUID = a.AccountUID
		s.EBSessionExpiry = a.SessionExpiry
	}
	return nil
}
