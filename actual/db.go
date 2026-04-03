package actual

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// DB wraps a SQLite connection to the Actual Budget database and accumulates
// protobuf change messages for the next sync commit.
type DB struct {
	sql     *sql.DB
	changes []ProtoMessage

	payeeByName map[string]string
	payeeByID   map[string]string
	acctByName  map[string]string
}

// Account is a minimal representation of an Actual Budget account.
type Account struct {
	ID   string
	Name string
}

// Transaction is a single transaction row from the Actual Budget SQLite database.
type Transaction struct {
	ID            string
	AccountID     string
	Date          time.Time
	AmountCents   int64
	PayeeID       string
	PayeeName     string
	Notes         string
	FinancialID   string
	Cleared       bool
	ImportedPayee string
	CategoryID    string
}

// OpenDB opens the SQLite database at path, enables WAL mode, and warms the
// in-memory payee and account caches.
func OpenDB(path string) (*DB, error) {
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if err := raw.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	if _, err := raw.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("WAL: %w", err)
	}
	d := &DB{
		sql:         raw,
		payeeByName: make(map[string]string),
		payeeByID:   make(map[string]string),
		acctByName:  make(map[string]string),
	}
	if err := d.warmCaches(); err != nil {
		return nil, fmt.Errorf("warm caches: %w", err)
	}
	return d, nil
}

// Close releases the underlying SQLite connection.
func (d *DB) Close() { _ = d.sql.Close() }

// FlushChanges drains and returns all accumulated change messages, resetting
// the internal buffer.
func (d *DB) FlushChanges() []ProtoMessage {
	ch := d.changes
	d.changes = nil
	return ch
}

func (d *DB) warmCaches() error {

	d.payeeByName = make(map[string]string)
	d.payeeByID = make(map[string]string)

	rows, err := d.sql.Query(
		`SELECT id, COALESCE(name, '') FROM payees WHERE tombstone = 0`,
	)
	if err != nil {
		return fmt.Errorf("load payees: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			continue
		}
		d.payeeByID[id] = name
		if name != "" {
			d.payeeByName[name] = id
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	d.acctByName = make(map[string]string)
	arows, err := d.sql.Query(
		`SELECT id, name FROM accounts WHERE tombstone = 0`,
	)
	if err != nil {
		return fmt.Errorf("load accounts: %w", err)
	}
	defer arows.Close()
	for arows.Next() {
		var id, name string
		if err := arows.Scan(&id, &name); err != nil {
			continue
		}
		d.acctByName[name] = id
	}
	return arows.Err()
}

func (d *DB) LoadHULC() (*HULCClient, error) {
	var clockJSON sql.NullString
	err := d.sql.QueryRow("SELECT clock FROM messages_clock LIMIT 1").Scan(&clockJSON)
	if err == sql.ErrNoRows || !clockJSON.Valid || clockJSON.String == "" {
		return NewHULCClient(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read messages_clock: %w", err)
	}

	var obj struct {
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal([]byte(clockJSON.String), &obj); err != nil || obj.Timestamp == "" {
		return NewHULCClient(), nil
	}

	h, err := HULCFromTimestamp(obj.Timestamp)
	if err != nil {
		return NewHULCClient(), nil
	}
	return h, nil
}

func (d *DB) SaveHULC(h *HULCClient) error {
	data, err := json.Marshal(map[string]string{"timestamp": h.SinceTimestamp()})
	if err != nil {
		return err
	}
	clockStr := string(data)

	var count int
	_ = d.sql.QueryRow("SELECT COUNT(*) FROM messages_clock").Scan(&count)
	if count == 0 {
		_, err = d.sql.Exec("INSERT INTO messages_clock (id, clock) VALUES (1, ?)", clockStr)
	} else {
		_, err = d.sql.Exec("UPDATE messages_clock SET clock = ?", clockStr)
	}
	return err
}

func (d *DB) ApplyMessages(msgs []*ProtoMessage) error {
	if len(msgs) == 0 {
		return nil
	}

	type rowKey struct{ table, id string }
	grouped := make(map[rowKey]map[string]any)
	order := make([]rowKey, 0, len(msgs))

	for _, m := range msgs {
		if m.Dataset == "prefs" {
			continue
		}
		key := rowKey{m.Dataset, m.Row}
		if _, ok := grouped[key]; !ok {
			grouped[key] = make(map[string]any)
			order = append(order, key)
		}
		grouped[key][m.Column] = decodeProtoValue(m.Value)
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, key := range order {
		if err := applyChange(tx, key.table, key.id, grouped[key]); err != nil {
			return fmt.Errorf("applyChange %s/%s: %w", key.table, key.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	return d.warmCaches()
}

func applyChange(tx *sql.Tx, table, rowID string, cols map[string]any) error {
	names := make([]string, 0, len(cols)+1)
	vals := make([]any, 0, len(cols)+1)
	updates := make([]string, 0, len(cols))

	names = append(names, "id")
	vals = append(vals, rowID)

	for col, val := range cols {
		if !isSafeIdentifier(col) {
			continue
		}
		names = append(names, `"`+col+`"`)
		vals = append(vals, val)
		updates = append(updates, `"`+col+`" = excluded."`+col+`"`)
	}

	if len(updates) == 0 {
		return nil
	}

	ph := strings.Repeat("?,", len(vals))
	ph = ph[:len(ph)-1]

	q := fmt.Sprintf(
		`INSERT INTO "%s" (%s) VALUES (%s) ON CONFLICT(id) DO UPDATE SET %s`,
		table,
		strings.Join(names, ","),
		ph,
		strings.Join(updates, ","),
	)
	_, err := tx.Exec(q, vals...)
	return err
}

func isSafeIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}

func decodeProtoValue(s string) any {
	if len(s) < 2 {
		return nil
	}
	switch s[0] {
	case 'S':
		return s[2:]
	case 'N':
		f, err := strconv.ParseFloat(s[2:], 64)
		if err != nil {
			return nil
		}
		return f
	case '0':
		return nil
	}
	return nil
}

// GetOrCreateAccount returns the account with the given name, creating it
// (along with a transfer payee) if it does not exist.
func (d *DB) GetOrCreateAccount(name string) (*Account, error) {
	if id, ok := d.acctByName[name]; ok {
		return &Account{ID: id, Name: name}, nil
	}

	var id string
	err := d.sql.QueryRow(
		`SELECT id FROM accounts WHERE name = ? AND tombstone = 0 LIMIT 1`, name,
	).Scan(&id)
	if err == nil {
		d.acctByName[name] = id
		return &Account{ID: id, Name: name}, nil
	}
	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("query account: %w", err)
	}

	id = uuid.New().String()
	_, err = d.sql.Exec(
		`INSERT INTO accounts (id, name, offbudget, closed, tombstone) VALUES (?, ?, 0, 0, 0)`,
		id, name,
	)
	if err != nil {
		return nil, fmt.Errorf("insert account: %w", err)
	}

	payeeID := uuid.New().String()
	_, _ = d.sql.Exec(
		`INSERT INTO payees (id, name, tombstone, transfer_acct) VALUES (?, NULL, 0, ?)`,
		payeeID, id,
	)
	_, _ = d.sql.Exec(
		`INSERT INTO payee_mapping (id, targetId) VALUES (?, ?)`,
		payeeID, payeeID,
	)

	d.acctByName[name] = id
	d.payeeByID[payeeID] = ""

	d.track("accounts", id, "name", name)
	d.track("accounts", id, "offbudget", 0)
	d.track("accounts", id, "closed", 0)
	d.track("payees", payeeID, "transfer_acct", id)

	return &Account{ID: id, Name: name}, nil
}

func (d *DB) getOrCreatePayee(name string) (string, error) {
	if id, ok := d.payeeByName[name]; ok {
		return id, nil
	}

	var id string
	err := d.sql.QueryRow(
		`SELECT id FROM payees WHERE name = ? AND tombstone = 0 LIMIT 1`, name,
	).Scan(&id)
	if err == nil {
		d.payeeByName[name] = id
		d.payeeByID[id] = name
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", fmt.Errorf("query payee: %w", err)
	}

	id = uuid.New().String()
	_, err = d.sql.Exec(
		`INSERT INTO payees (id, name, tombstone) VALUES (?, ?, 0)`,
		id, name,
	)
	if err != nil {
		return "", fmt.Errorf("insert payee: %w", err)
	}
	_, _ = d.sql.Exec(
		`INSERT INTO payee_mapping (id, targetId) VALUES (?, ?)`,
		id, id,
	)

	d.payeeByName[name] = id
	d.payeeByID[id] = name
	d.track("payees", id, "name", name)
	d.track("payee_mapping", id, "targetId", id)
	return id, nil
}

func (d *DB) resolvePayeeName(payeeID string) string {
	return d.payeeByID[payeeID]
}

// GetTransactions returns all non-deleted, non-parent transactions for the given account.
func (d *DB) GetTransactions(accountID string) ([]*Transaction, error) {
	rows, err := d.sql.Query(`
		SELECT t.id, t.acct, t.date, t.amount,
		       COALESCE(t.description, ''),
		       COALESCE(t.notes, ''),
		       COALESCE(t.financial_id, ''),
		       COALESCE(t.cleared, 0),
		       COALESCE(t.imported_description, ''),
		       COALESCE(t.category, '')
		FROM transactions t
		WHERE t.acct      = ?
		  AND t.isParent  = 0
		  AND t.tombstone = 0
		ORDER BY t.date DESC
	`, accountID)
	if err != nil {
		return nil, fmt.Errorf("query transactions: %w", err)
	}
	defer rows.Close()

	var txns []*Transaction
	for rows.Next() {
		var (
			t       Transaction
			dateInt int
			cleared int
		)
		if err := rows.Scan(
			&t.ID, &t.AccountID, &dateInt, &t.AmountCents,
			&t.PayeeID, &t.Notes, &t.FinancialID, &cleared, &t.ImportedPayee, &t.CategoryID,
		); err != nil {
			return nil, err
		}
		t.Date = intToDate(dateInt)
		t.Cleared = cleared != 0
		t.PayeeName = d.resolvePayeeName(t.PayeeID)
		txns = append(txns, &t)
	}
	return txns, rows.Err()
}

// CreateTransaction inserts a new transaction into the database and queues the
// corresponding sync change messages.
func (d *DB) CreateTransaction(
	date time.Time, account *Account,
	payeeName, notes string,
	amountCents int64,
	cleared bool,
	importedID string,
	importedPayee string,
) (*Transaction, error) {
	id := uuid.New().String()
	dateInt := dateToInt(date)
	sortOrder := float64(time.Now().UnixMilli())
	clearedInt := btoi(cleared)

	if payeeName != "" {
		payeeName = titleCase(payeeName)
	}

	var payeeID string
	if payeeName != "" {
		pid, err := d.getOrCreatePayee(payeeName)
		if err != nil {
			return nil, err
		}
		payeeID = pid
	}

	_, err := d.sql.Exec(`
		INSERT INTO transactions
		  (id, acct, date, amount, description, notes, cleared, tombstone,
		   isParent, isChild, reconciled, sort_order, financial_id, imported_description)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, 0, 0, ?, ?, ?)
	`, id, account.ID, dateInt, amountCents, nullStr(payeeID), notes, clearedInt, sortOrder,
		nullStr(importedID), nullStr(importedPayee))
	if err != nil {
		return nil, fmt.Errorf("insert transaction: %w", err)
	}

	d.track("transactions", id, "acct", account.ID)
	d.track("transactions", id, "date", dateInt)
	d.track("transactions", id, "amount", int(amountCents))
	if payeeID != "" {
		d.track("transactions", id, "description", payeeID)
	}
	if notes != "" {
		d.track("transactions", id, "notes", notes)
	}
	d.track("transactions", id, "cleared", clearedInt)
	d.track("transactions", id, "sort_order", sortOrder)
	d.track("transactions", id, "reconciled", 0)
	if importedID != "" {
		d.track("transactions", id, "financial_id", importedID)
	}
	if importedPayee != "" {
		d.track("transactions", id, "imported_description", importedPayee)
	}

	return &Transaction{
		ID:            id,
		AccountID:     account.ID,
		Date:          date,
		AmountCents:   amountCents,
		PayeeID:       payeeID,
		PayeeName:     payeeName,
		Notes:         notes,
		Cleared:       cleared,
		FinancialID:   importedID,
		ImportedPayee: importedPayee,
	}, nil
}

// UpdateTransactionCleared marks the transaction as cleared in the database.
func (d *DB) UpdateTransactionCleared(t *Transaction) error {
	_, err := d.sql.Exec(
		`UPDATE transactions SET cleared = 1 WHERE id = ?`, t.ID,
	)
	if err != nil {
		return fmt.Errorf("update cleared: %w", err)
	}
	t.Cleared = true
	d.track("transactions", t.ID, "cleared", 1)
	return nil
}

// ReconcileTransaction finds an existing transaction that matches the given
// parameters and updates any stale fields, or creates a new one if no match is
// found. It returns the transaction, whether it was newly created, and any error.
func (d *DB) ReconcileTransaction(
	date time.Time, account *Account,
	payeeName, notes string,
	amountCents int64,
	cleared bool,
	importedID string,
	importedPayee string,
	alreadyMatched []*Transaction,
) (*Transaction, bool, error) {
	match := d.matchTransaction(date, account, payeeName, amountCents, importedID, alreadyMatched)
	if match != nil {
		changed := false

		if importedID != "" && match.FinancialID != importedID {
			if _, err := d.sql.Exec(
				`UPDATE transactions SET financial_id = ? WHERE id = ?`, importedID, match.ID,
			); err != nil {
				return nil, false, err
			}
			d.track("transactions", match.ID, "financial_id", importedID)
			match.FinancialID = importedID
			changed = true
		}

		if notes != "" && notes != match.Notes {
			if _, err := d.sql.Exec(
				`UPDATE transactions SET notes = ? WHERE id = ?`, notes, match.ID,
			); err != nil {
				return nil, false, err
			}
			d.track("transactions", match.ID, "notes", notes)
			match.Notes = notes
			changed = true
		}

		clearedInt := btoi(cleared)
		if btoi(match.Cleared) != clearedInt {
			if _, err := d.sql.Exec(
				`UPDATE transactions SET cleared = ? WHERE id = ?`, clearedInt, match.ID,
			); err != nil {
				return nil, false, err
			}
			d.track("transactions", match.ID, "cleared", clearedInt)
			match.Cleared = cleared
			changed = true
		}

		normalizedPayee := payeeName
		if normalizedPayee != "" {
			normalizedPayee = titleCase(normalizedPayee)
		}
		if normalizedPayee != "" && normalizedPayee != match.PayeeName {
			if pid, err := d.getOrCreatePayee(normalizedPayee); err == nil && pid != match.PayeeID {
				if _, err := d.sql.Exec(
					`UPDATE transactions SET description = ? WHERE id = ?`, pid, match.ID,
				); err != nil {
					return nil, false, err
				}
				d.track("transactions", match.ID, "description", pid)
				match.PayeeID = pid
				match.PayeeName = normalizedPayee
				changed = true
			}
		}

		if importedPayee != "" && match.ImportedPayee == "" {
			if _, err := d.sql.Exec(
				`UPDATE transactions SET imported_description = ? WHERE id = ?`, importedPayee, match.ID,
			); err != nil {
				return nil, false, err
			}
			d.track("transactions", match.ID, "imported_description", importedPayee)
			match.ImportedPayee = importedPayee
			changed = true
		}

		return match, changed, nil
	}

	t, err := d.CreateTransaction(date, account, payeeName, notes, amountCents, cleared, importedID, importedPayee)
	if err != nil {
		return nil, false, err
	}
	return t, true, nil
}

func (d *DB) matchTransaction(
	date time.Time, account *Account,
	payeeName string, amountCents int64,
	importedID string,
	alreadyMatched []*Transaction,
) *Transaction {
	if importedID != "" {
		for _, t := range alreadyMatched {
			if t.FinancialID == importedID {
				return t
			}
		}
	}

	lo := dateToInt(date.AddDate(0, 0, -7))
	hi := dateToInt(date.AddDate(0, 0, 8))

	rows, err := d.sql.Query(`
		SELECT t.id, t.acct, t.date, t.amount,
		       COALESCE(t.description, ''),
		       COALESCE(t.notes, ''),
		       COALESCE(t.financial_id, ''),
		       COALESCE(t.cleared, 0),
		       COALESCE(t.imported_description, '')
		FROM transactions t
		WHERE t.acct      = ?
		  AND t.date     >= ?
		  AND t.date      < ?
		  AND t.amount    = ?
		  AND t.isParent  = 0
		  AND t.tombstone = 0
	`, account.ID, lo, hi, amountCents)
	if err != nil {
		return nil
	}
	defer rows.Close()

	exclude := make(map[string]struct{}, len(alreadyMatched))
	for _, t := range alreadyMatched {
		exclude[t.ID] = struct{}{}
	}

	var candidates []*Transaction
	for rows.Next() {
		var (
			t       Transaction
			dateInt int
			cleared int
		)
		if err := rows.Scan(
			&t.ID, &t.AccountID, &dateInt, &t.AmountCents,
			&t.PayeeID, &t.Notes, &t.FinancialID, &cleared, &t.ImportedPayee,
		); err != nil {
			continue
		}
		if _, skip := exclude[t.ID]; skip {
			continue
		}
		t.Date = intToDate(dateInt)
		t.Cleared = cleared != 0
		t.PayeeName = d.resolvePayeeName(t.PayeeID)
		candidates = append(candidates, &t)
	}

	if len(candidates) == 0 {
		return nil
	}

	target := dateToInt(date)
	sortByDateProximity(candidates, target)

	if payeeName != "" {
		for _, c := range candidates {
			if strings.EqualFold(c.PayeeName, payeeName) {
				return c
			}
		}
	}
	return candidates[0]
}

func sortByDateProximity(txns []*Transaction, target int) {
	for i := 1; i < len(txns); i++ {
		for j := i; j > 0; j-- {
			if abs(dateToInt(txns[j].Date)-target) < abs(dateToInt(txns[j-1].Date)-target) {
				txns[j-1], txns[j] = txns[j], txns[j-1]
			} else {
				break
			}
		}
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func (d *DB) track(table, id, column string, value any) {
	d.changes = append(d.changes, ProtoMessage{
		Dataset: table,
		Row:     id,
		Column:  column,
		Value:   encodeValue(value),
	})
}

func dateToInt(t time.Time) int {
	return t.Year()*10000 + int(t.Month())*100 + t.Day()
}

func intToDate(n int) time.Time {
	y := n / 10000
	m := (n % 10000) / 100
	day := n % 100
	return time.Date(y, time.Month(m), day, 0, 0, 0, 0, time.UTC)
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// DecimalToCents converts a decimal string such as "12.50" to integer cents (1250).
func DecimalToCents(s string) (int64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return int64(math.Round(f * 100)), nil
}

func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		r := []rune(w)
		if len(r) == 0 {
			continue
		}
		r[0] = unicode.ToUpper(r[0])
		for j := 1; j < len(r); j++ {
			r[j] = unicode.ToLower(r[j])
		}
		words[i] = string(r)
	}
	return strings.Join(words, " ")
}
