package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// RetentionDays is how long deduplication state is kept.
//
// It must cover the rolling fetch window plus the match window on both sides.
// A shorter retention lets a transaction the user deleted fall out of the state
// while still inside the fetch window, so the next sync imports it again.
const RetentionDays = 38

// DBPath is the default location of the SQLite database.
const DBPath = "/data/bankingsync.db"

// Store is the SQLite-backed persistence layer for bankingsync.
type Store struct {
	db  db
	obs atomic.Pointer[obs]
}

// BankAccount is a row from the bank_accounts table.
type BankAccount struct {
	ID            int64
	SessionID     string
	AccountUID    string
	BankName      string
	BankCountry   string
	ActualAccount string
	StartSyncDate string
	LastSyncDate  string
	SessionExpiry string
	CreatedAt     string
	IBAN          string
	Currency      string

	OpeningBalanceState     string
	OpeningBalanceCents     int64
	OpeningBalanceDate      string
	OpeningBalanceRef       string
	OpeningBalanceWrittenAt string
	BalancesAccess          string
	DriftCents              int64
	DriftState              string
	DriftCheckedAt          string
}

// NewBankAccount carries the fields needed to create a bank account row.
type NewBankAccount struct {
	SessionID     string
	AccountUID    string
	BankName      string
	BankCountry   string
	ActualAccount string
	StartSyncDate string
	SessionExpiry string
	IBAN          string
	Currency      string

	// OpeningBalanceState distinguishes a row created before opening balances
	// existed (empty) from one created after (OpeningBalanceAuto). Only the
	// latter may be written unattended; an existing budget must never gain a
	// transaction the user did not ask for.
	OpeningBalanceState string
}

// Open opens (or creates) the SQLite database at path and runs schema migrations.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	// busy_timeout has to travel in the DSN, not through a PRAGMA statement.
	// It is a per-connection setting and database/sql hands out pooled
	// connections, so an Exec would configure exactly one of them and leave the
	// rest failing instantly with SQLITE_BUSY the moment two writers meet —
	// which they do as soon as the web UI writes while a sync is running.
	sqlDB, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}
	if _, err := sqlDB.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("WAL: %w", err)
	}
	s := &Store{}
	s.db = newDB(sqlDB, s)
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
			actual_account  TEXT NOT NULL DEFAULT '',
			start_sync_date TEXT NOT NULL DEFAULT '',
			session_expiry  TEXT NOT NULL DEFAULT '',
			created_at     TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE IF NOT EXISTS imported_refs (
			bank_account_id INTEGER NOT NULL DEFAULT 0,
			ref             TEXT NOT NULL,
			date            TEXT NOT NULL,
			PRIMARY KEY (bank_account_id, ref)
		);
		CREATE TABLE IF NOT EXISTS pending_map (
			bank_account_id INTEGER NOT NULL DEFAULT 0,
			key             TEXT NOT NULL,
			txn_id          TEXT NOT NULL,
			PRIMARY KEY (bank_account_id, key)
		);
		CREATE TABLE IF NOT EXISTS sync_log (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			ran_at       TEXT NOT NULL DEFAULT (datetime('now')),
			status       TEXT NOT NULL DEFAULT 'success',
			tx_added     INTEGER NOT NULL DEFAULT 0,
			tx_confirmed INTEGER NOT NULL DEFAULT 0,
			tx_skipped   INTEGER NOT NULL DEFAULT 0,
			duration_sec REAL NOT NULL DEFAULT 0,
			message      TEXT NOT NULL DEFAULT ''
		);

		-- Transactions the matcher could neither adopt nor create on its own.
		-- They are not in the budget yet: holding one back is the deliberate
		-- alternative to guessing, and the whole row is kept because the import
		-- has to be reconstructable from it once somebody decides.
		--
		-- Candidates are deliberately NOT stored. A candidate can be deleted,
		-- split or edited between the hold and the decision, so the list is
		-- recomputed when it is shown — which also warms the lookup the Actual
		-- adapter needs before it can update anything.
		CREATE TABLE IF NOT EXISTS match_decisions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id TEXT NOT NULL,
			bank_account_id INTEGER NOT NULL,
			bank TEXT,
			incoming_ref TEXT,
			pending_key TEXT,
			candidate_id TEXT,
			payee_level TEXT, amount_level TEXT, date_level TEXT,
			candidates INTEGER,
			weight REAL, probability REAL, margin REAL,
			outcome TEXT NOT NULL,
			param_version TEXT NOT NULL,
			txn_date TEXT,
			decided_at TEXT DEFAULT (datetime('now')),
			truth INTEGER,
			shadow_version TEXT NOT NULL DEFAULT '',
			shadow_outcome TEXT NOT NULL DEFAULT ''
		);

		CREATE TABLE IF NOT EXISTS level_observations (
			bank_account_id INTEGER NOT NULL,
			classification  TEXT NOT NULL,
			field           TEXT NOT NULL,
			level           TEXT NOT NULL,
			observations    INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (bank_account_id, classification, field, level)
		) WITHOUT ROWID;

		CREATE TABLE IF NOT EXISTS match_inquiries (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			bank_account_id   INTEGER NOT NULL,
			decision_id       INTEGER NOT NULL DEFAULT 0,
			bank              TEXT NOT NULL DEFAULT '',
			pending_key       TEXT NOT NULL DEFAULT '',
			param_version     TEXT NOT NULL DEFAULT '',
			outcome           TEXT NOT NULL DEFAULT '',
			probability       REAL NOT NULL DEFAULT 0,
			gain              REAL NOT NULL DEFAULT 0,
			txn_date          TEXT NOT NULL DEFAULT '',
			amount_cents      INTEGER NOT NULL DEFAULT 0,
			currency          TEXT NOT NULL DEFAULT '',
			payee             TEXT NOT NULL DEFAULT '',
			candidate_date    TEXT NOT NULL DEFAULT '',
			candidate_amount  INTEGER NOT NULL DEFAULT 0,
			candidate_payee   TEXT NOT NULL DEFAULT '',
			why               TEXT NOT NULL DEFAULT '',
			asked_at          TEXT NOT NULL DEFAULT (datetime('now')),
			answered_at       TEXT NOT NULL DEFAULT ''
		);

		CREATE TABLE IF NOT EXISTS match_reviews (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			bank_account_id   INTEGER NOT NULL DEFAULT 0,
			backend           TEXT NOT NULL DEFAULT '',
			external_ref      TEXT NOT NULL DEFAULT '',
			pending_key       TEXT NOT NULL DEFAULT '',
			txn_date          TEXT NOT NULL DEFAULT '',
			amount_cents      INTEGER NOT NULL DEFAULT 0,
			currency          TEXT NOT NULL DEFAULT '',
			payee             TEXT NOT NULL DEFAULT '',
			notes             TEXT NOT NULL DEFAULT '',
			imported_payee    TEXT NOT NULL DEFAULT '',
			counterparty_iban TEXT NOT NULL DEFAULT '',
			sepa_eref         TEXT NOT NULL DEFAULT '',
			sepa_mref         TEXT NOT NULL DEFAULT '',
			sepa_cred         TEXT NOT NULL DEFAULT '',
			cleared           INTEGER NOT NULL DEFAULT 0,
			best_probability  REAL NOT NULL DEFAULT 0,
			best_payee_level  TEXT NOT NULL DEFAULT '',
			best_amount_level TEXT NOT NULL DEFAULT '',
			best_date_level   TEXT NOT NULL DEFAULT '',
			created_at        TEXT NOT NULL DEFAULT (datetime('now')),
			UNIQUE (bank_account_id, pending_key)
		);
	`)
	if err != nil {
		return err
	}
	// Migrations: add columns for existing databases.
	for _, stmt := range []string{
		`ALTER TABLE bank_accounts ADD COLUMN actual_account TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE bank_accounts ADD COLUMN start_sync_date TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE bank_accounts ADD COLUMN last_sync_date TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE bank_accounts ADD COLUMN iban TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE bank_accounts ADD COLUMN currency TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE bank_accounts ADD COLUMN opening_balance_state TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE bank_accounts ADD COLUMN opening_balance_cents INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE bank_accounts ADD COLUMN opening_balance_date TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE bank_accounts ADD COLUMN opening_balance_ref TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE bank_accounts ADD COLUMN opening_balance_written_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE bank_accounts ADD COLUMN balances_access TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE bank_accounts ADD COLUMN drift_cents INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE bank_accounts ADD COLUMN drift_state TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE bank_accounts ADD COLUMN drift_checked_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE match_decisions ADD COLUMN shadow_version TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE match_decisions ADD COLUMN shadow_outcome TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE level_observations RENAME COLUMN param_version TO classification`,
	} {
		_, _ = s.db.Exec(stmt)
	}

	if err := s.migrateAccountScopedTables(); err != nil {
		return fmt.Errorf("scope dedupe tables by account: %w", err)
	}
	if err := s.discardParametersFittedOnTheOldCorpus(); err != nil {
		return fmt.Errorf("discard parameters fitted on the old corpus: %w", err)
	}
	return nil
}

// contaminatedCorpusSetting marks that the one-shot discard below has run.
const contaminatedCorpusSetting = "match_corpus_v3_discarded"

// discardParametersFittedOnTheOldCorpus throws away any parameter set promoted
// before the decision log was corrected, once, at open.
//
// A promoted set is the fitted numbers themselves, not a recipe: it is loaded
// into the live policy on every run and nothing ever re-derives it. Sets
// promoted before this release were fitted on a corpus contaminated three ways
// — labels scored against rows the merge had already rewritten, labels
// harvested from a key built out of the model's own comparison fields, and
// every one of them carrying a candidate count of one, whose prior contributes
// nothing at all. No column records which corpus produced a set, so a
// contaminated one cannot be told from a sound one after the fact.
//
// Discarding falls back to the shipped parameters, which are the ones the
// sensitivity table vouches for. It happens here rather than during a sync so
// that it runs exactly once, at upgrade, before anything can be promoted under
// the corrected code and thrown away with the rest.
func (s *Store) discardParametersFittedOnTheOldCorpus() error {
	done, err := s.GetSetting(contaminatedCorpusSetting)
	if err != nil || done != "" {
		return err
	}
	promoted, err := s.GetSetting(SettingPromotedTrial)
	if err != nil {
		return err
	}
	shadow, err := s.GetSetting(SettingShadowTrial)
	if err != nil {
		return err
	}
	if promoted != "" || shadow != "" {
		log.Print("Discarding matching parameters fitted before the decision log was " +
			"corrected — reverting to the shipped set. They will be refitted once enough " +
			"sound labels have accumulated.")
		for _, key := range []string{SettingPromotedTrial, SettingShadowTrial} {
			if err := s.SetSetting(key, ""); err != nil {
				return err
			}
		}
	}
	return s.SetSetting(contaminatedCorpusSetting, "1")
}

func (s *Store) hasColumn(table, column string) bool {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		if name == column {
			return true
		}
	}
	return false
}

func (s *Store) migrateAccountScopedTables() error {
	refsNeedsMigration := !s.hasColumn("imported_refs", "bank_account_id")
	pendingNeedsMigration := !s.hasColumn("pending_map", "bank_account_id")
	if !refsNeedsMigration && !pendingNeedsMigration {
		return nil
	}

	var owner int64
	if err := s.db.QueryRow(
		`SELECT id FROM bank_accounts ORDER BY created_at ASC, id ASC LIMIT 1`,
	).Scan(&owner); err != nil {
		owner = 0
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if refsNeedsMigration {
		if _, err := tx.Exec(`
			CREATE TABLE imported_refs_scoped (
				bank_account_id INTEGER NOT NULL DEFAULT 0,
				ref             TEXT NOT NULL,
				date            TEXT NOT NULL,
				PRIMARY KEY (bank_account_id, ref)
			);
			INSERT OR IGNORE INTO imported_refs_scoped (bank_account_id, ref, date)
				SELECT ?, ref, date FROM imported_refs;
			DROP TABLE imported_refs;
			ALTER TABLE imported_refs_scoped RENAME TO imported_refs;
		`, owner); err != nil {
			return fmt.Errorf("imported_refs: %w", err)
		}
	}

	if pendingNeedsMigration {
		if _, err := tx.Exec(`
			CREATE TABLE pending_map_scoped (
				bank_account_id INTEGER NOT NULL DEFAULT 0,
				key             TEXT NOT NULL,
				txn_id          TEXT NOT NULL,
				PRIMARY KEY (bank_account_id, key)
			);
			INSERT OR IGNORE INTO pending_map_scoped (bank_account_id, key, txn_id)
				SELECT ?, key, txn_id FROM pending_map;
			DROP TABLE pending_map;
			ALTER TABLE pending_map_scoped RENAME TO pending_map;
		`, owner); err != nil {
			return fmt.Errorf("pending_map: %w", err)
		}
	}

	return tx.Commit()
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

	var legacyAccountID int64
	if legacy.EBSessionID != "" && legacy.EBAccountUID != "" {
		var count int
		_ = tx.QueryRow("SELECT COUNT(*) FROM bank_accounts").Scan(&count)
		if count == 0 {
			res, err := tx.Exec(
				`INSERT INTO bank_accounts (session_id, account_uid, bank_name, bank_country, actual_account, start_sync_date, session_expiry)
				 VALUES (?, ?, '', '', '', '', ?)`,
				legacy.EBSessionID, legacy.EBAccountUID, legacy.EBSessionExpiry,
			)
			if err != nil {
				return err
			}
			legacyAccountID, _ = res.LastInsertId()
		}
	}
	if legacyAccountID == 0 {
		_ = tx.QueryRow(`SELECT id FROM bank_accounts ORDER BY created_at ASC, id ASC LIMIT 1`).Scan(&legacyAccountID)
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
			`INSERT INTO imported_refs (bank_account_id, ref, date) VALUES (?, ?, ?)
			 ON CONFLICT(bank_account_id, ref) DO NOTHING`,
			legacyAccountID, ref, date,
		)
		if err != nil {
			return err
		}
	}

	for key, txnID := range legacy.PendingMap {
		_, err = tx.Exec(
			`INSERT INTO pending_map (bank_account_id, key, txn_id) VALUES (?, ?, ?)
			 ON CONFLICT(bank_account_id, key) DO NOTHING`,
			legacyAccountID, key, txnID,
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
		`SELECT id, session_id, account_uid, bank_name, bank_country, actual_account, start_sync_date,
		        COALESCE(last_sync_date, ''), session_expiry, created_at,
		        COALESCE(iban, ''), COALESCE(currency, ''),
		        COALESCE(opening_balance_state, ''), COALESCE(opening_balance_cents, 0),
		        COALESCE(opening_balance_date, ''), COALESCE(opening_balance_ref, ''),
		        COALESCE(opening_balance_written_at, ''), COALESCE(balances_access, ''),
		        COALESCE(drift_cents, 0), COALESCE(drift_state, ''), COALESCE(drift_checked_at, '')
		 FROM bank_accounts ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var accounts []BankAccount
	for rows.Next() {
		var a BankAccount
		err := rows.Scan(
			&a.ID, &a.SessionID, &a.AccountUID, &a.BankName, &a.BankCountry,
			&a.ActualAccount, &a.StartSyncDate, &a.LastSyncDate, &a.SessionExpiry,
			&a.CreatedAt, &a.IBAN, &a.Currency,
			&a.OpeningBalanceState, &a.OpeningBalanceCents,
			&a.OpeningBalanceDate, &a.OpeningBalanceRef,
			&a.OpeningBalanceWrittenAt, &a.BalancesAccess,
			&a.DriftCents, &a.DriftState, &a.DriftCheckedAt,
		)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// AddBankAccount inserts a new bank account and returns its ID.
func normaliseIBAN(v string) string {
	return strings.ToUpper(strings.ReplaceAll(v, " ", ""))
}

func (s *Store) AddBankAccount(a NewBankAccount) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO bank_accounts
		   (session_id, account_uid, bank_name, bank_country, actual_account, start_sync_date, session_expiry,
		    iban, currency, opening_balance_state)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.SessionID, a.AccountUID, a.BankName, a.BankCountry, a.ActualAccount, a.StartSyncDate, a.SessionExpiry,
		normaliseIBAN(a.IBAN), strings.ToUpper(strings.TrimSpace(a.Currency)), a.OpeningBalanceState,
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

// UpdateBankAccountStartDate sets a new sync start date for an account.
func (s *Store) UpdateBankAccountStartDate(id int64, startDate string) error {
	_, err := s.db.Exec(`UPDATE bank_accounts SET start_sync_date = ? WHERE id = ?`, startDate, id)
	return err
}

// RemoveBankAccount deletes a bank account by ID.
func (s *Store) RemoveBankAccount(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range []string{
		"DELETE FROM imported_refs WHERE bank_account_id = ?",
		"DELETE FROM pending_map WHERE bank_account_id = ?",
		// Held transactions go with the account for the same reason the other
		// two do: they name a budget account that is no longer being synced, so
		// nothing can be merged into it and nothing can be imported to it. Left
		// behind they would sit in the review queue permanently undecidable.
		"DELETE FROM match_reviews WHERE bank_account_id = ?",
		"DELETE FROM match_decisions WHERE bank_account_id = ?",
		"DELETE FROM match_inquiries WHERE bank_account_id = ?",
		"DELETE FROM level_observations WHERE bank_account_id = ?",
		"DELETE FROM bank_accounts WHERE id = ?",
	} {
		if _, err := tx.Exec(stmt, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// HasImportedRef returns whether the given reference has already been imported
// for the given bank account.
func (s *Store) HasImportedRef(bankAccountID int64, ref string) (bool, error) {
	var count int
	err := s.db.QueryRow(
		"SELECT COUNT(*) FROM imported_refs WHERE bank_account_id = ? AND ref = ?",
		bankAccountID, ref,
	).Scan(&count)
	return count > 0, err
}

// AddImportedRef records a successfully imported transaction reference.
func (s *Store) AddImportedRef(bankAccountID int64, ref, date string) error {
	_, err := s.db.Exec(
		`INSERT INTO imported_refs (bank_account_id, ref, date) VALUES (?, ?, ?)
		 ON CONFLICT(bank_account_id, ref) DO UPDATE SET date = excluded.date`,
		bankAccountID, ref, date,
	)
	return err
}

// ResetImportState discards everything that ties already-imported transactions
// to a backend: the deduplication tables and the sync watermarks.
//
// The watermarks have to go with them. They record how far the *previous*
// backend got, so leaving them in place would make the next run fetch only what
// arrived since then — the new backend would end up nearly empty, without any
// error to show for it.
//
// It returns how many deduplication rows were dropped.
func (s *Store) ResetImportState() (int64, int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	refsRes, err := tx.Exec(`DELETE FROM imported_refs`)
	if err != nil {
		return 0, 0, fmt.Errorf("purge imported_refs: %w", err)
	}
	pendingRes, err := tx.Exec(`DELETE FROM pending_map`)
	if err != nil {
		return 0, 0, fmt.Errorf("purge pending_map: %w", err)
	}
	if _, err := tx.Exec(`UPDATE bank_accounts SET last_sync_date = ''`); err != nil {
		return 0, 0, fmt.Errorf("reset account watermarks: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM settings WHERE key = 'last_sync_date'`); err != nil {
		return 0, 0, fmt.Errorf("reset global watermark: %w", err)
	}
	// A reset declares that nothing has been imported. An entry left behind here
	// would point at candidates the reset assumes are gone, and its transaction
	// would never be fetched again because the queue is what keeps it from being
	// re-offered.
	if _, err := tx.Exec(`DELETE FROM match_reviews`); err != nil {
		return 0, 0, fmt.Errorf("purge match_reviews: %w", err)
	}
	// The decision log describes imports the reset has just declared never to
	// have happened. Left behind it would answer questions about a history that
	// no longer exists.
	if _, err := tx.Exec(`DELETE FROM match_inquiries`); err != nil {
		return 0, 0, fmt.Errorf("purge match_inquiries: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM level_observations`); err != nil {
		return 0, 0, fmt.Errorf("purge level_observations: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM match_decisions`); err != nil {
		return 0, 0, fmt.Errorf("purge match_decisions: %w", err)
	}

	refs, _ := refsRes.RowsAffected()
	pending, _ := pendingRes.RowsAffected()

	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return refs, pending, nil
}

// PruneImportedRefs removes refs outside the retention window and returns the updated map.
func (s *Store) PruneImportedRefs() (map[int64]map[string]string, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -RetentionDays).Format("2006-01-02")
	if _, err := s.db.Exec("DELETE FROM imported_refs WHERE date < ?", cutoff); err != nil {
		return nil, err
	}
	return s.AllImportedRefs()
}

// AllImportedRefs returns all imported refs grouped by bank account.
func (s *Store) AllImportedRefs() (map[int64]map[string]string, error) {
	rows, err := s.db.Query("SELECT bank_account_id, ref, date FROM imported_refs")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[int64]map[string]string)
	for rows.Next() {
		var acct int64
		var ref, date string
		if err := rows.Scan(&acct, &ref, &date); err != nil {
			return nil, err
		}
		if m[acct] == nil {
			m[acct] = make(map[string]string)
		}
		m[acct][ref] = date
	}
	return m, rows.Err()
}

// GetPendingTxnID returns the Actual transaction ID for a pending key.
func (s *Store) GetPendingTxnID(bankAccountID int64, key string) (string, bool, error) {
	var id string
	err := s.db.QueryRow(
		"SELECT txn_id FROM pending_map WHERE bank_account_id = ? AND key = ?",
		bankAccountID, key,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return id, err == nil, err
}

// SetPending upserts a pending key -> transaction ID mapping.
func (s *Store) SetPending(bankAccountID int64, key, txnID string) error {
	_, err := s.db.Exec(
		`INSERT INTO pending_map (bank_account_id, key, txn_id) VALUES (?, ?, ?)
		 ON CONFLICT(bank_account_id, key) DO UPDATE SET txn_id = excluded.txn_id`,
		bankAccountID, key, txnID,
	)
	return err
}

// DeletePending removes a pending map entry.
func (s *Store) DeletePending(bankAccountID int64, key string) error {
	_, err := s.db.Exec(
		"DELETE FROM pending_map WHERE bank_account_id = ? AND key = ?", bankAccountID, key)
	return err
}

// AllPendingMap returns the full pending map grouped by bank account.
func (s *Store) AllPendingMap() (map[int64]map[string]string, error) {
	rows, err := s.db.Query("SELECT bank_account_id, key, txn_id FROM pending_map")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[int64]map[string]string)
	for rows.Next() {
		var acct int64
		var key, id string
		if err := rows.Scan(&acct, &key, &id); err != nil {
			return nil, err
		}
		if m[acct] == nil {
			m[acct] = make(map[string]string)
		}
		m[acct][key] = id
	}
	return m, rows.Err()
}

// SetBankAccountLastSyncDate records the per-account sync watermark.
func (s *Store) SetBankAccountLastSyncDate(id int64, date string) error {
	_, err := s.db.Exec(`UPDATE bank_accounts SET last_sync_date = ? WHERE id = ?`, date, id)
	return err
}

// SyncLog is a row from the sync_log table.
type SyncLog struct {
	ID          int64
	RanAt       string
	Status      string
	TxAdded     int
	TxConfirmed int
	TxSkipped   int
	DurationSec float64
	Message     string
}

// AddSyncLog records the result of a sync cycle.
func (s *Store) AddSyncLog(status string, txAdded, txConfirmed, txSkipped int, durationSec float64, message string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO sync_log (status, tx_added, tx_confirmed, tx_skipped, duration_sec, message) VALUES (?, ?, ?, ?, ?, ?)`,
		status, txAdded, txConfirmed, txSkipped, durationSec, message,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetSyncLogs returns the most recent sync log entries.
func (s *Store) GetSyncLogs(limit int) ([]SyncLog, error) {
	rows, err := s.db.Query(
		`SELECT id, ran_at, status, tx_added, tx_confirmed, tx_skipped, duration_sec, message
		 FROM sync_log ORDER BY id DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var logs []SyncLog
	for rows.Next() {
		var l SyncLog
		if err := rows.Scan(&l.ID, &l.RanAt, &l.Status, &l.TxAdded, &l.TxConfirmed, &l.TxSkipped, &l.DurationSec, &l.Message); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

// GetLatestSyncLog returns the most recent sync log entry, or nil if none exist.
func (s *Store) GetLatestSyncLog() (*SyncLog, error) {
	var l SyncLog
	err := s.db.QueryRow(
		`SELECT id, ran_at, status, tx_added, tx_confirmed, tx_skipped, duration_sec, message
		 FROM sync_log ORDER BY id DESC LIMIT 1`,
	).Scan(&l.ID, &l.RanAt, &l.Status, &l.TxAdded, &l.TxConfirmed, &l.TxSkipped, &l.DurationSec, &l.Message)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// Opening-balance lifecycle states held in bank_accounts.opening_balance_state.
//
// The distinction between the empty string and OpeningBalanceAuto is what keeps
// this feature off existing installations: rows written before it shipped carry
// the empty string and are only ever touched through the explicit button, while
// rows created afterwards start at OpeningBalanceAuto and are filled by the
// first clean sync.
const (
	OpeningBalanceLegacy      = ""
	OpeningBalanceAuto        = "auto"
	OpeningBalanceWriting     = "writing"
	OpeningBalanceWritten     = "written"
	OpeningBalanceSkipped     = "skipped"
	OpeningBalanceUnavailable = "unavailable"
	OpeningBalanceDenied      = "denied"
)

// Drift states held in bank_accounts.drift_state. Only DriftAlert is an alarm;
// every other value says why no comparison was possible, which is what keeps a
// truncated window or a moving balance from paging anyone.
const (
	DriftUnknown     = ""
	DriftOK          = "ok"
	DriftAlert       = "drift"
	DriftUnstable    = "unstable"
	DriftIncomplete  = "incomplete"
	DriftNoOpening   = "no_opening_balance"
	DriftUnsupported = "unsupported"
	DriftNoBalance   = "no_balance"
)

// ClaimOpeningBalance atomically moves an account from a writable state into
// OpeningBalanceWriting and reports whether this caller won the claim.
//
// It is a compare-and-swap rather than a read followed by a write because the
// UI button is a plain form post: a double click issues two requests that would
// both pass a check-then-act guard and write two opening balances.
func (s *Store) ClaimOpeningBalance(id int64) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE bank_accounts SET opening_balance_state = ?
		  WHERE id = ? AND opening_balance_state IN (?, ?, ?)`,
		OpeningBalanceWriting, id,
		OpeningBalanceLegacy, OpeningBalanceAuto, OpeningBalanceSkipped,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// SetOpeningBalance records a written opening balance and closes the account for
// further automatic writes.
func (s *Store) SetOpeningBalance(id int64, cents int64, date, ref string) error {
	_, err := s.db.Exec(
		`UPDATE bank_accounts
		    SET opening_balance_state = ?, opening_balance_cents = ?, opening_balance_date = ?,
		        opening_balance_ref = ?, opening_balance_written_at = datetime('now')
		  WHERE id = ?`,
		OpeningBalanceWritten, cents, date, ref, id,
	)
	return err
}

// SetOpeningBalanceState moves an account to state without touching the amount.
func (s *Store) SetOpeningBalanceState(id int64, state string) error {
	_, err := s.db.Exec(`UPDATE bank_accounts SET opening_balance_state = ? WHERE id = ?`, state, id)
	return err
}

// SetAccountDrift records the latest comparison between bank and budget.
func (s *Store) SetAccountDrift(id int64, cents int64, state string) error {
	_, err := s.db.Exec(
		`UPDATE bank_accounts SET drift_cents = ?, drift_state = ?, drift_checked_at = datetime('now')
		  WHERE id = ?`,
		cents, state, id,
	)
	return err
}

// SetBalancesAccess records what the balances endpoint answered for this account.
func (s *Store) SetBalancesAccess(id int64, access string) error {
	_, err := s.db.Exec(`UPDATE bank_accounts SET balances_access = ? WHERE id = ?`, access, id)
	return err
}

// MatchReview is a bank transaction the matcher held back for a person to
// decide on. It carries everything needed to reconstruct the import, because by
// the time somebody looks the bank feed has moved on.
type MatchReview struct {
	ID               int64
	BankAccountID    int64
	Backend          string
	ExternalRef      string
	PendingKey       string
	TxnDate          string
	AmountCents      int64
	Currency         string
	Payee            string
	Notes            string
	ImportedPayee    string
	CounterpartyIBAN string
	SEPAEndToEnd     string
	SEPAMandate      string
	SEPACreditorID   string
	Cleared          bool
	BestProbability  float64
	BestPayeeLevel   string
	BestAmountLevel  string
	BestDateLevel    string
	CreatedAt        string
}

// AddMatchReview records a held transaction. A second sync offering the same one
// is a no-op rather than a duplicate row, which is what the unique key is for.
func (s *Store) AddMatchReview(r MatchReview) error {
	_, err := s.db.Exec(`
		INSERT INTO match_reviews (
			bank_account_id, backend, external_ref, pending_key, txn_date,
			amount_cents, currency, payee, notes, imported_payee,
			counterparty_iban, sepa_eref, sepa_mref, sepa_cred, cleared,
			best_probability, best_payee_level, best_amount_level, best_date_level
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (bank_account_id, pending_key) DO NOTHING`,
		r.BankAccountID, r.Backend, r.ExternalRef, r.PendingKey, r.TxnDate,
		r.AmountCents, r.Currency, r.Payee, r.Notes, r.ImportedPayee,
		r.CounterpartyIBAN, r.SEPAEndToEnd, r.SEPAMandate, r.SEPACreditorID, boolToInt(r.Cleared),
		r.BestProbability, r.BestPayeeLevel, r.BestAmountLevel, r.BestDateLevel)
	return err
}

// GetMatchReviews returns every held transaction, oldest first.
func (s *Store) GetMatchReviews() ([]MatchReview, error) {
	rows, err := s.db.Query(`
		SELECT id, bank_account_id, backend, external_ref, pending_key, txn_date,
		       amount_cents, currency, payee, notes, imported_payee,
		       counterparty_iban, sepa_eref, sepa_mref, sepa_cred, cleared,
		       best_probability, best_payee_level, best_amount_level, best_date_level,
		       created_at
		FROM match_reviews ORDER BY txn_date, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MatchReview
	for rows.Next() {
		var r MatchReview
		var cleared int
		if err := rows.Scan(&r.ID, &r.BankAccountID, &r.Backend, &r.ExternalRef, &r.PendingKey,
			&r.TxnDate, &r.AmountCents, &r.Currency, &r.Payee, &r.Notes, &r.ImportedPayee,
			&r.CounterpartyIBAN, &r.SEPAEndToEnd, &r.SEPAMandate, &r.SEPACreditorID, &cleared,
			&r.BestProbability, &r.BestPayeeLevel, &r.BestAmountLevel, &r.BestDateLevel,
			&r.CreatedAt); err != nil {
			return nil, err
		}
		r.Cleared = cleared != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetMatchReview returns one held transaction.
func (s *Store) GetMatchReview(id int64) (MatchReview, error) {
	all, err := s.GetMatchReviews()
	if err != nil {
		return MatchReview{}, err
	}
	for _, r := range all {
		if r.ID == id {
			return r, nil
		}
	}
	return MatchReview{}, fmt.Errorf("no held transaction with id %d", id)
}

// DeleteMatchReview removes a held transaction once it has been decided.
func (s *Store) DeleteMatchReview(id int64) error {
	_, err := s.db.Exec(`DELETE FROM match_reviews WHERE id = ?`, id)
	return err
}

// CountMatchReviews reports how many decisions are outstanding.
func (s *Store) CountMatchReviews() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM match_reviews`).Scan(&n)
	return n, err
}

// CountMatchReviewsByAccount reports how many decisions are outstanding for each
// bank account, including the accounts with none.
//
// The zeros are the point. A gauge that simply stopped reporting an account
// would be indistinguishable from an account that had gone away, and "nothing
// is waiting" is exactly the state an operator wants to see confirmed rather
// than inferred from an absence.
func (s *Store) CountMatchReviewsByAccount() (map[int64]int, error) {
	accounts, err := s.GetAllBankAccounts()
	if err != nil {
		return nil, err
	}
	out := make(map[int64]int, len(accounts))
	for _, a := range accounts {
		out[a.ID] = 0
	}

	rows, err := s.db.Query(`SELECT bank_account_id, COUNT(*) FROM match_reviews GROUP BY bank_account_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// AllHeldKeys returns the pending keys under review, per bank account, so the
// sync loop can tell a transaction it is already holding from a new one.
func (s *Store) AllHeldKeys() (map[int64]map[string]bool, error) {
	rows, err := s.db.Query(`SELECT bank_account_id, pending_key FROM match_reviews`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]map[string]bool)
	for rows.Next() {
		var id int64
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = make(map[string]bool)
		}
		out[id][key] = true
	}
	return out, rows.Err()
}

// PruneMatchReviews drops decisions nobody made in time.
//
// The same retention as the deduplication state, and for the same reason: past
// it the bank will not offer the transaction again either, so a row kept longer
// would describe an import that can no longer happen.
func (s *Store) PruneMatchReviews() error {
	cutoff := time.Now().UTC().AddDate(0, 0, -RetentionDays).Format("2006-01-02")
	_, err := s.db.Exec(`DELETE FROM match_reviews WHERE txn_date < ?`, cutoff)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// MatchDecision is one matching decision as it was made, with the parameters
// that made it.
//
// Every incoming transaction produces one, not only the ones a person is asked
// about. That is the point: the automatic band is where the expensive mistakes
// happen, and a model estimated on the doubtful cases alone and applied to all
// of them is biased by construction — the same problem credit scoring calls
// reject inference. It is also the answer to "it did not match": the levels are
// recorded, so the first question can be answered without anybody sending
// payees and amounts around.
//
// Truth is filled in later, when something independent of the model settles what
// the answer was — a person deciding, or a bank reference turning up.
type MatchDecision struct {
	ID            int64
	RunID         string
	BankAccountID int64
	Bank          string
	IncomingRef   string
	PendingKey    string
	CandidateID   string
	PayeeLevel    string
	AmountLevel   string
	DateLevel     string
	Candidates    int
	Weight        float64
	Probability   float64
	Margin        float64
	Outcome       string
	ParamVersion  string
	TxnDate       string
	DecidedAt     string
	Truth         *bool

	// ShadowVersion and ShadowOutcome are what a candidate parameter set would
	// have done with this transaction, and which candidate set that was.
	//
	// The version is stored with the outcome and not alongside it. A shadow left
	// over from parameters nobody is considering any more is not evidence about
	// the ones that are, and without the version there is no way to tell the two
	// apart short of clearing the column on every change.
	ShadowVersion string
	ShadowOutcome string
}

// AddMatchDecision records one decision.
func (s *Store) AddMatchDecision(d MatchDecision) error {
	_, err := s.db.Exec(`
		INSERT INTO match_decisions (
			run_id, bank_account_id, bank, incoming_ref, pending_key, candidate_id,
			payee_level, amount_level, date_level, candidates,
			weight, probability, margin, outcome, param_version, txn_date, truth,
			shadow_version, shadow_outcome
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.RunID, d.BankAccountID, d.Bank, d.IncomingRef, d.PendingKey, d.CandidateID,
		d.PayeeLevel, d.AmountLevel, d.DateLevel, d.Candidates,
		d.Weight, d.Probability, d.Margin, d.Outcome, d.ParamVersion, d.TxnDate,
		truthColumn(d.Truth), d.ShadowVersion, d.ShadowOutcome)
	return err
}

// ErrNoSuchDecision is returned when an answer names a decision the log no
// longer holds.
//
// This is not a rare corner. Retention deletes unanswered decisions after
// RetentionDays, and a backfill can write decisions whose transaction date is
// already past that line, so a queue entry can outlive the record it is about.
// Reporting it is the whole point: an answer that lands nowhere used to look
// exactly like an answer that was filed.
var ErrNoSuchDecision = errors.New("no decision to record the answer against")

// SetMatchDecisionTruth records what a decision turned out to be, matched by the
// transaction it was about.
//
// The most recent decision for that transaction is the one updated: a
// transaction offered over several runs produces one record per run, and only
// the last of them describes the state a person or a bank reference has now
// settled.
//
// An empty key is refused rather than matched. Retention blanks pending_key on
// decisions it has anonymised, so every aged row in an account shares the empty
// key; a lookup by it would find one of those and write the answer onto a
// stranger's label. That is worse than not recording the answer at all, and no
// rows-affected check would notice, because it updates exactly one row.
func (s *Store) SetMatchDecisionTruth(bankAccountID int64, pendingKey string, correct bool) error {
	if pendingKey == "" {
		return ErrNoSuchDecision
	}
	res, err := s.db.Exec(`
		UPDATE match_decisions SET truth = ?
		WHERE id = (
			SELECT id FROM match_decisions
			WHERE bank_account_id = ? AND pending_key = ? AND pending_key != ''
			ORDER BY id DESC LIMIT 1
		)`, boolToInt(correct), bankAccountID, pendingKey)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNoSuchDecision
	}
	return nil
}

// ResolvedComparison is the pair a person actually settled on, which is not
// always the pair the model asked about.
type ResolvedComparison struct {
	CandidateID string
	PayeeLevel  string
	AmountLevel string
	DateLevel   string
	Weight      float64
	Probability float64
}

// SetMatchDecisionResolution records a review answer against the candidate the
// person actually chose, rather than against the one the model put first.
//
// The two are not always the same and the difference poisons the m tables when it
// is ignored. A held decision is logged with the comparison levels of the model's
// best candidate, but the review page offers every candidate in the window and
// the person may merge into any of them. Recording "this was a match" against the
// stored row then attaches a positive label to a pair the person has just
// declined — and the m probabilities are estimated from exactly those labels, so
// a level that loses reviews would come to look like the level that wins them.
//
// The levels are overwritten before the truth is set, so the row describes the
// pair it is now making a claim about.
func (s *Store) SetMatchDecisionResolution(bankAccountID int64, pendingKey string,
	correct bool, c ResolvedComparison) error {

	if pendingKey == "" {
		return ErrNoSuchDecision
	}
	res, err := s.db.Exec(`
		UPDATE match_decisions
		SET truth = ?, candidate_id = ?, payee_level = ?, amount_level = ?,
		    date_level = ?, weight = ?, probability = ?
		WHERE id = (
			SELECT id FROM match_decisions
			WHERE bank_account_id = ? AND pending_key = ? AND pending_key != ''
			ORDER BY id DESC LIMIT 1
		)`,
		boolToInt(correct), c.CandidateID, c.PayeeLevel, c.AmountLevel,
		c.DateLevel, c.Weight, c.Probability, bankAccountID, pendingKey)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNoSuchDecision
	}
	return nil
}

// LatestMatchDecisionID returns the newest decision recorded about one
// transaction, which is the one a caller acting during the run that made it
// wants to hold on to.
//
// Empty keys are refused for the reason given on SetMatchDecisionTruth.
func (s *Store) LatestMatchDecisionID(bankAccountID int64, pendingKey string) (int64, error) {
	if pendingKey == "" {
		return 0, nil
	}
	var id int64
	err := s.db.QueryRow(`
		SELECT id FROM match_decisions
		WHERE bank_account_id = ? AND pending_key = ? AND pending_key != ''
		ORDER BY id DESC LIMIT 1`, bankAccountID, pendingKey).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// SetMatchDecisionTruthByID records what one particular decision turned out to
// be, for a caller that knows which one it means rather than only which
// transaction.
func (s *Store) SetMatchDecisionTruthByID(id int64, correct bool) error {
	res, err := s.db.Exec(`UPDATE match_decisions SET truth = ? WHERE id = ?`, boolToInt(correct), id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNoSuchDecision
	}
	return nil
}

// GetMatchDecisions returns the recorded decisions, most recent first,
// answered or not.
//
// Callers that want evidence want GetLabelledMatchDecisions instead. This one
// exists for the diagnosis export, which counts unanswered decisions on purpose:
// they are the denominator that says how much of the log anybody ever settled.
func (s *Store) GetMatchDecisions(limit int) ([]MatchDecision, error) {
	return s.matchDecisions("", limit)
}

// GetLabelledMatchDecisions returns the most recent decisions somebody or some
// bank reference has settled.
//
// Filtering in SQL rather than after the fetch is what makes the limit mean what
// it says. Reading a page of the log and discarding the unanswered rows from it
// yields a corpus whose size depends on the ratio of answered to unanswered
// decisions, so a run that records many unanswered ones silently starves the
// estimators of labels that are still there.
func (s *Store) GetLabelledMatchDecisions(limit int) ([]MatchDecision, error) {
	return s.matchDecisions("WHERE truth IS NOT NULL", limit)
}

func (s *Store) matchDecisions(where string, limit int) ([]MatchDecision, error) {
	rows, err := s.db.Query(`
		SELECT id, run_id, bank_account_id, bank, incoming_ref, pending_key, candidate_id,
		       payee_level, amount_level, date_level, candidates,
		       weight, probability, margin, outcome, param_version, txn_date, decided_at, truth,
		       shadow_version, shadow_outcome
		FROM match_decisions `+where+` ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []MatchDecision
	for rows.Next() {
		var d MatchDecision
		var truth *int64
		if err := rows.Scan(&d.ID, &d.RunID, &d.BankAccountID, &d.Bank, &d.IncomingRef,
			&d.PendingKey, &d.CandidateID, &d.PayeeLevel, &d.AmountLevel, &d.DateLevel,
			&d.Candidates, &d.Weight, &d.Probability, &d.Margin, &d.Outcome,
			&d.ParamVersion, &d.TxnDate, &d.DecidedAt, &truth,
			&d.ShadowVersion, &d.ShadowOutcome); err != nil {
			return nil, err
		}
		if truth != nil {
			v := *truth != 0
			d.Truth = &v
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LevelObservation is how often one comparison level was seen among pairs that
// are not the same payment.
//
// Levels travel as their names rather than as numbers, the same way the decision
// log carries them: they are an iota enumeration, and a database of numbers
// would come back describing a different model after an ordinary edit to the
// list.
type LevelObservation struct {
	Field string
	Level string
	Count int
}

// AddLevelObservations folds a run's sample into the running tally.
//
// Counted, not stored row by row. A busy account weighs tens of thousands of
// pairs a month and every one of them is a candidate that was rejected — keeping
// them individually would cost more than the estimate is worth, and the estimate
// only ever needs the totals. What is kept carries no payee, no amount and no
// date: a level name and a number.
func (s *Store) AddLevelObservations(bankAccountID int64, classification string, obs []LevelObservation) error {
	if classification == "" || len(obs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, o := range obs {
		if o.Count <= 0 {
			continue
		}
		if _, err := tx.Exec(`
			INSERT INTO level_observations (bank_account_id, classification, field, level, observations)
			VALUES (?,?,?,?,?)
			ON CONFLICT (bank_account_id, classification, field, level)
			DO UPDATE SET observations = observations + excluded.observations`,
			bankAccountID, classification, o.Field, o.Level, o.Count); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LevelObservations returns the sample gathered under one classification,
// summed over every account.
//
// Scoped to the classification and not to the whole parameter set. What decides
// which level a pair reaches is the amount tolerance, the payee prefixes and the
// date window; change one of those and the old counts describe something that is
// no longer being counted, so they are not merged with the new ones. Everything
// else may move freely. A promoted parameter set changes what a level is worth
// rather than which level a pair falls into, and a threshold changes what is
// done with the evidence rather than what the evidence is — discarding the
// sample on either would throw away a tally that is still exactly right.
func (s *Store) LevelObservations(classification string) ([]LevelObservation, error) {
	if classification == "" {
		return nil, nil
	}
	rows, err := s.db.Query(`
		SELECT field, level, SUM(observations) FROM level_observations
		WHERE classification = ? GROUP BY field, level`, classification)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []LevelObservation
	for rows.Next() {
		var o LevelObservation
		if err := rows.Scan(&o.Field, &o.Level, &o.Count); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// PruneLevelObservations drops every sample gathered under a classification
// other than the one in force.
func (s *Store) PruneLevelObservations(keep string) error {
	if keep == "" {
		return nil
	}
	_, err := s.db.Exec(`DELETE FROM level_observations WHERE classification != ?`, keep)
	return err
}

// MatchInquiry is one decision the model settled on its own that a person has
// been asked to confirm anyway.
//
// It carries its own copy of what was decided rather than pointing at the budget
// rows, for the same reason the review queue does: the question is about a
// judgement made at a particular moment, and a row edited or deleted since would
// otherwise quietly change what is being asked about.
type MatchInquiry struct {
	ID            int64
	BankAccountID int64

	// DecisionID is the decision log row this question is about, and the answer
	// goes to that row rather than to the newest one for the transaction.
	//
	// The distinction matters here and does not for the review queue. A held
	// transaction is re-offered every run, so its latest record is the one a
	// person has now settled; a question is about one particular comparison,
	// shown as it stood at the time, and if another run has since decided the
	// same transaction again the answer would otherwise be filed against levels
	// nobody was ever shown.
	DecisionID int64

	Bank         string
	PendingKey   string
	ParamVersion string

	// Outcome is what the model did — "adopted" or "created" — and decides how
	// the answer is read. The question put to the person is the same either way:
	// are these two rows one payment? Asking instead whether the program was
	// right would mean a yes meaning one thing on a merge and the opposite on an
	// import, which is how confirmation dialogs collect wrong answers.
	Outcome     string
	Probability float64
	Gain        float64

	TxnDate     string
	AmountCents int64
	Currency    string
	Payee       string

	CandidateDate   string
	CandidateAmount int64
	CandidatePayee  string
	Why             string

	AskedAt    string
	AnsweredAt string
}

// AddMatchInquiry records a question put to a person.
func (s *Store) AddMatchInquiry(q MatchInquiry) error {
	_, err := s.db.Exec(`
		INSERT INTO match_inquiries (
			bank_account_id, decision_id, bank, pending_key, param_version, outcome, probability, gain,
			txn_date, amount_cents, currency, payee,
			candidate_date, candidate_amount, candidate_payee, why
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		q.BankAccountID, q.DecisionID, q.Bank, q.PendingKey, q.ParamVersion, q.Outcome, q.Probability, q.Gain,
		q.TxnDate, q.AmountCents, q.Currency, q.Payee,
		q.CandidateDate, q.CandidateAmount, q.CandidatePayee, q.Why)
	return err
}

// OpenInquiry returns the question waiting for an answer, if there is one.
//
// There is at most one unanswered at a time, and every caller relies on that. A
// sync that runs nightly would otherwise put thirty questions in front of
// somebody by the end of a month, which is how a well-meant request for labels
// becomes something people learn to click past — and a request people click past
// produces labels that are worse than none.
func (s *Store) OpenInquiry() (MatchInquiry, bool, error) {
	var q MatchInquiry
	err := s.db.QueryRow(`
		SELECT id, bank_account_id, decision_id, bank, pending_key, param_version, outcome, probability, gain,
		       txn_date, amount_cents, currency, payee,
		       candidate_date, candidate_amount, candidate_payee, why, asked_at, answered_at
		FROM match_inquiries WHERE answered_at = '' ORDER BY id DESC LIMIT 1`).Scan(
		&q.ID, &q.BankAccountID, &q.DecisionID, &q.Bank, &q.PendingKey, &q.ParamVersion, &q.Outcome,
		&q.Probability, &q.Gain, &q.TxnDate, &q.AmountCents, &q.Currency, &q.Payee,
		&q.CandidateDate, &q.CandidateAmount, &q.CandidatePayee, &q.Why,
		&q.AskedAt, &q.AnsweredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MatchInquiry{}, false, nil
	}
	if err != nil {
		return MatchInquiry{}, false, err
	}
	return q, true, nil
}

// HasOpenInquiry reports whether somebody already has a question in front of
// them, which is the reason not to ask another.
func (s *Store) HasOpenInquiry() (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM match_inquiries WHERE answered_at = ''`).Scan(&n)
	return n > 0, err
}

// CloseInquiry marks a question answered. The answer itself belongs to the
// decision it was about, and is written there by SetMatchDecisionTruth.
func (s *Store) CloseInquiry(id int64) error {
	res, err := s.db.Exec(
		`UPDATE match_inquiries SET answered_at = datetime('now') WHERE id = ? AND answered_at = ''`, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("no open confirmation with id %d", id)
	}
	return nil
}

// PruneMatchInquiries drops questions about transactions the bank no longer
// offers, on the same window everything else in this store uses.
//
// Unanswered ones go too. A question nobody answered inside the retention window
// is one nobody is going to answer, and keeping it would block the next one
// indefinitely.
func (s *Store) PruneMatchInquiries() error {
	cutoff := time.Now().UTC().AddDate(0, 0, -RetentionDays).Format("2006-01-02")
	_, err := s.db.Exec(`DELETE FROM match_inquiries WHERE txn_date < ?`, cutoff)
	return err
}

// ShadowTally is how a candidate parameter set compared with the one in force,
// over the decisions both have seen.
type ShadowTally struct {
	Version   string
	Total     int
	Differing int
}

// CountShadowOutcomes tallies the decisions recorded under one candidate
// parameter set, and how many of them it would have decided differently.
//
// Scoped to the version, so a candidate that has only just started being
// evaluated is reported on its own handful of decisions rather than inheriting
// the record of whatever was being evaluated last week.
func (s *Store) CountShadowOutcomes(version string) (ShadowTally, error) {
	out := ShadowTally{Version: version}
	if version == "" {
		return out, nil
	}
	err := s.db.QueryRow(`
		SELECT COUNT(*), SUM(CASE WHEN shadow_outcome != outcome THEN 1 ELSE 0 END)
		FROM match_decisions WHERE shadow_version = ?`, version).Scan(&out.Total, &nullableInt{&out.Differing})
	return out, err
}

// nullableInt scans a SQL aggregate that is null over an empty set.
type nullableInt struct{ dst *int }

func (n *nullableInt) Scan(v any) error {
	if v == nil {
		*n.dst = 0
		return nil
	}
	i, ok := v.(int64)
	if !ok {
		return fmt.Errorf("expected an integer, got %T", v)
	}
	*n.dst = int(i)
	return nil
}

// CountMatchDecisions reports how many decisions are on record.
func (s *Store) CountMatchDecisions() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM match_decisions`).Scan(&n)
	return n, err
}

// EvidenceRetentionDays is how long a decision somebody has settled is kept.
//
// Longer than RetentionDays because the two answer different questions.
// RetentionDays exists so that deduplication state covers the rolling fetch
// window; a settled decision is not state but evidence, and the estimators that
// consume it need more than a month of it to say anything.
//
// Bounded rather than kept forever, because a bank's habits are not permanent
// either. Thirteen months is one full annual cycle — the Christmas trade, the
// holiday bookings, the subscriptions that renew once a year all appear exactly
// once — plus a month of slack. Beyond that an observation is likelier to
// describe a format the institution has since changed than the one it uses now,
// and the refit weights every observation equally however old it is.
const EvidenceRetentionDays = 400

// ForwardDatingDays is how far ahead of today a transaction date is still taken
// at face value.
//
// Retention keys on the transaction date, and a bank that reports a value date
// rather than a booking date can put that date in the future — a standing order
// or a scheduled payment does exactly this. Such a row is newer than every
// backward-looking cutoff, so without an upper bound it is never redacted and
// never deleted, and it keeps a payee, an amount and a second-resolution
// timestamp for as long as the database exists.
//
// Two weeks is past any settlement lag and short of any plausible schedule.
const ForwardDatingDays = 14

// PruneMatchDecisions ages the decision log out in two steps, because the log
// holds two different things.
//
// An unanswered decision is a diagnostic. Once the bank no longer offers the
// transaction nobody can check it any more, so it goes on the same window as the
// rest of the deduplication state.
//
// An answered one is evidence, and evidence does not stop being true when the
// transaction ages out. What does stop being needed is everything that says
// which transaction it was: the pending key carries a payee and an amount, the
// references identify rows in the bank's feed and in the budget, and the run id
// groups a sync's decisions together. Those are cleared and the comparison
// levels are kept, which leaves a record that can be counted and cannot be read
// back to a purchase.
//
// Both dates are coarsened to their month, and the second one is the point. A
// decision is made within hours of the transaction reaching the feed, so a
// timestamp to the second pins down the purchase more precisely than the
// transaction date ever did — coarsening one and not the other would achieve
// nothing. The month is the same resolution the diagnosis export is limited to.
//
// The account id stays, because the estimators count by it and removing an
// account has to be able to take its evidence with it. It names a bank, not a
// purchase.
//
// Redaction keys on the transaction date, bounded on both sides: a date more
// than ForwardDatingDays ahead is treated like one long past, because it is
// equally out of reach and would otherwise never age at all. Final deletion
// keys on decided_at instead, which this program writes itself and which is
// therefore always real and always in the past — a transaction date the bank
// put in the future is no clock to retire a row by.
func (s *Store) PruneMatchDecisions() error {
	now := time.Now().UTC()
	cutoff := now.AddDate(0, 0, -RetentionDays).Format("2006-01-02")
	evidenceCutoff := now.AddDate(0, 0, -EvidenceRetentionDays).Format("2006-01-02")
	horizon := now.AddDate(0, 0, ForwardDatingDays).Format("2006-01-02")

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`DELETE FROM match_decisions
		 WHERE truth IS NULL AND (txn_date < ? OR txn_date > ?)`,
		cutoff, horizon); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		UPDATE match_decisions
		SET pending_key = '', incoming_ref = '', candidate_id = '', run_id = '',
		    txn_date = substr(txn_date, 1, 7) || '-01',
		    decided_at = substr(decided_at, 1, 7) || '-01'
		WHERE truth IS NOT NULL AND (txn_date < ? OR txn_date > ?)
		  AND (pending_key != '' OR incoming_ref != '' OR candidate_id != '' OR run_id != '')`,
		cutoff, horizon); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`DELETE FROM match_decisions WHERE decided_at < ?`, evidenceCutoff); err != nil {
		return err
	}
	return tx.Commit()
}

// truthColumn renders a decision's answer for storage. Nil stays null, which is
// how "nobody has looked" is told from "the model was wrong" — the two must
// never collapse into one, or an unanswered decision becomes evidence.
func truthColumn(truth *bool) any {
	if truth == nil {
		return nil
	}
	return boolToInt(*truth)
}
