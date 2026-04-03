package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// DBPath is the default location of the SQLite database.
const DBPath = "/data/bankingsync.db"

// Store is the SQLite-backed persistence layer for bankingsync.
type Store struct {
	db *sql.DB
}

// BankAccount is a row from the bank_accounts table.
type BankAccount struct {
	ID            int64
	SessionID     string
	AccountUID    string
	BankName      string
	BankCountry   string
	SessionExpiry string
	CreatedAt     string
}

// Open opens (or creates) the SQLite database at path and runs schema migrations.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("WAL: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := s.importLegacyStateJSON(); err != nil {
		return nil, fmt.Errorf("legacy migration: %w", err)
	}
	return s, nil
}

// Close releases the database connection.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS bank_accounts (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id     TEXT NOT NULL,
			account_uid    TEXT NOT NULL,
			bank_name      TEXT NOT NULL,
			bank_country   TEXT NOT NULL,
			session_expiry TEXT NOT NULL DEFAULT '',
			created_at     TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE IF NOT EXISTS imported_refs (
			ref  TEXT PRIMARY KEY,
			date TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS pending_map (
			key    TEXT PRIMARY KEY,
			txn_id TEXT NOT NULL
		);
	`)
	return err
}

// importLegacyStateJSON migrates /data/state.json into the database on first run.
func (s *Store) importLegacyStateJSON() error {
	const legacyPath = "/data/state.json"
	data, err := os.ReadFile(legacyPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	var legacy struct {
		EBSessionID     string            `json:"eb_session_id"`
		EBAccountUID    string            `json:"eb_account_uid"`
		EBSessionExpiry string            `json:"eb_session_expiry"`
		LastSyncDate    string            `json:"last_sync_date"`
		PendingMap      map[string]string `json:"pending_map"`
		ImportedRefs    map[string]string `json:"imported_refs"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return fmt.Errorf("parse state.json: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if legacy.EBSessionID != "" && legacy.EBAccountUID != "" {
		var count int
		_ = tx.QueryRow("SELECT COUNT(*) FROM bank_accounts").Scan(&count)
		if count == 0 {
			_, err = tx.Exec(
				`INSERT INTO bank_accounts (session_id, account_uid, bank_name, bank_country, session_expiry)
				 VALUES (?, ?, '', '', ?)`,
				legacy.EBSessionID, legacy.EBAccountUID, legacy.EBSessionExpiry,
			)
			if err != nil {
				return err
			}
		}
	}

	if legacy.LastSyncDate != "" {
		_, err = tx.Exec(
			`INSERT INTO settings (key, value) VALUES ('last_sync_date', ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			legacy.LastSyncDate,
		)
		if err != nil {
			return err
		}
	}

	for ref, date := range legacy.ImportedRefs {
		_, err = tx.Exec(
			`INSERT INTO imported_refs (ref, date) VALUES (?, ?)
			 ON CONFLICT(ref) DO NOTHING`,
			ref, date,
		)
		if err != nil {
			return err
		}
	}

	for key, txnID := range legacy.PendingMap {
		_, err = tx.Exec(
			`INSERT INTO pending_map (key, txn_id) VALUES (?, ?)
			 ON CONFLICT(key) DO NOTHING`,
			key, txnID,
		)
		if err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	return os.Rename(legacyPath, legacyPath+".migrated")
}

// GetSetting returns the value for key, or "" if not set.
func (s *Store) GetSetting(key string) (string, error) {
	var v string
	err := s.db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SetSetting upserts a key-value pair into the settings table.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	return err
}

// GetLastSyncDate returns the last successful sync date string.
func (s *Store) GetLastSyncDate() (string, error) {
	return s.GetSetting("last_sync_date")
}

// SetLastSyncDate persists the last sync date.
func (s *Store) SetLastSyncDate(date string) error {
	return s.SetSetting("last_sync_date", date)
}

// GetAllBankAccounts returns all bank accounts ordered by creation time.
func (s *Store) GetAllBankAccounts() ([]BankAccount, error) {
	rows, err := s.db.Query(
		`SELECT id, session_id, account_uid, bank_name, bank_country, session_expiry, created_at
		 FROM bank_accounts ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var accounts []BankAccount
	for rows.Next() {
		var a BankAccount
		if err := rows.Scan(&a.ID, &a.SessionID, &a.AccountUID, &a.BankName, &a.BankCountry, &a.SessionExpiry, &a.CreatedAt); err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// AddBankAccount inserts a new bank account and returns its ID.
func (s *Store) AddBankAccount(sessionID, accountUID, bankName, bankCountry, expiry string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO bank_accounts (session_id, account_uid, bank_name, bank_country, session_expiry)
		 VALUES (?, ?, ?, ?, ?)`,
		sessionID, accountUID, bankName, bankCountry, expiry,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateBankAccountSession updates the session credentials for an existing account.
func (s *Store) UpdateBankAccountSession(id int64, sessionID, expiry string) error {
	_, err := s.db.Exec(
		`UPDATE bank_accounts SET session_id = ?, session_expiry = ? WHERE id = ?`,
		sessionID, expiry, id,
	)
	return err
}

// RemoveBankAccount deletes a bank account by ID.
func (s *Store) RemoveBankAccount(id int64) error {
	_, err := s.db.Exec("DELETE FROM bank_accounts WHERE id = ?", id)
	return err
}

// HasImportedRef returns whether the given reference has already been imported.
func (s *Store) HasImportedRef(ref string) (bool, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM imported_refs WHERE ref = ?", ref).Scan(&count)
	return count > 0, err
}

// AddImportedRef records a successfully imported transaction reference.
func (s *Store) AddImportedRef(ref, date string) error {
	_, err := s.db.Exec(
		`INSERT INTO imported_refs (ref, date) VALUES (?, ?)
		 ON CONFLICT(ref) DO UPDATE SET date = excluded.date`,
		ref, date,
	)
	return err
}

// PruneImportedRefs removes refs older than 21 days and returns the updated map.
func (s *Store) PruneImportedRefs() (map[string]string, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -21).Format("2006-01-02")
	if _, err := s.db.Exec("DELETE FROM imported_refs WHERE date < ?", cutoff); err != nil {
		return nil, err
	}
	return s.AllImportedRefs()
}

// AllImportedRefs returns all imported refs as a map[ref]date.
func (s *Store) AllImportedRefs() (map[string]string, error) {
	rows, err := s.db.Query("SELECT ref, date FROM imported_refs")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]string)
	for rows.Next() {
		var ref, date string
		if err := rows.Scan(&ref, &date); err != nil {
			return nil, err
		}
		m[ref] = date
	}
	return m, rows.Err()
}

// GetPendingTxnID returns the Actual transaction ID for a pending key.
func (s *Store) GetPendingTxnID(key string) (string, bool, error) {
	var id string
	err := s.db.QueryRow("SELECT txn_id FROM pending_map WHERE key = ?", key).Scan(&id)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return id, err == nil, err
}

// SetPending upserts a pending key → transaction ID mapping.
func (s *Store) SetPending(key, txnID string) error {
	_, err := s.db.Exec(
		`INSERT INTO pending_map (key, txn_id) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET txn_id = excluded.txn_id`,
		key, txnID,
	)
	return err
}

// DeletePending removes a pending map entry.
func (s *Store) DeletePending(key string) error {
	_, err := s.db.Exec("DELETE FROM pending_map WHERE key = ?", key)
	return err
}

// AllPendingMap returns the full pending map as map[key]txnID.
func (s *Store) AllPendingMap() (map[string]string, error) {
	rows, err := s.db.Query("SELECT key, txn_id FROM pending_map")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]string)
	for rows.Next() {
		var key, id string
		if err := rows.Scan(&key, &id); err != nil {
			return nil, err
		}
		m[key] = id
	}
	return m, rows.Err()
}
