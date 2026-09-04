package actual

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"bankingsync/budget"
)

// Adapter exposes an Actual Budget client through the backend-neutral
// budget.Store port. It keeps the native transactions it has seen so the rule
// engine, which works on Actual's own identifiers, can still be handed real
// rows.
type Adapter struct {
	client *Client
	db     *DB

	mu     sync.Mutex
	native map[string]*Transaction
	accts  map[string]*Account
}

var (
	_ budget.Store      = (*Adapter)(nil)
	_ budget.Flusher    = (*Adapter)(nil)
	_ budget.RuleRunner = (*Adapter)(nil)

	_ budget.OpeningBalanceWriter = (*Adapter)(nil)
	_ budget.BalanceReader        = (*Adapter)(nil)
)

func NewAdapter(c *Client) *Adapter {
	return &Adapter{
		client: c,
		db:     c.DB(),
		native: make(map[string]*Transaction),
		accts:  make(map[string]*Account),
	}
}

// NewAdapterForDB builds an adapter over a bare database, without the HTTP
// client. Commit flushes locally queued changes and Ping is a no-op.
func NewAdapterForDB(d *DB) *Adapter {
	return &Adapter{
		db:     d,
		native: make(map[string]*Transaction),
		accts:  make(map[string]*Account),
	}
}

func (a *Adapter) Ping(ctx context.Context) error {
	if a.client == nil {
		return nil
	}
	return a.client.Resync(ctx)
}

func (a *Adapter) Close() {
	if a.client != nil {
		a.client.Close()
	}
}

func (a *Adapter) Commit(ctx context.Context) error {
	if a.client == nil {
		a.db.FlushChanges()
		return nil
	}
	return a.client.Commit(ctx)
}

func (a *Adapter) GetOrCreateAccount(_ context.Context, spec budget.AccountSpec) (*budget.Account, error) {
	acct, err := a.db.GetOrCreateAccount(spec.Name)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.accts[acct.ID] = acct
	a.mu.Unlock()
	return &budget.Account{ID: acct.ID, Name: acct.Name}, nil
}

func (a *Adapter) ListTransactions(_ context.Context, accountID string, from, to time.Time) ([]*budget.Transaction, error) {
	native, err := a.db.GetTransactionsBetween(accountID, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]*budget.Transaction, 0, len(native))
	for _, t := range native {
		out = append(out, a.remember(t))
	}
	return out, nil
}

func (a *Adapter) FindByExternalRef(_ context.Context, accountID, ref string) (*budget.Transaction, error) {
	if ref == "" {
		return nil, nil
	}
	t := a.db.findByFinancialID(&Account{ID: accountID}, ref)
	if t == nil {
		return nil, nil
	}
	return a.remember(t), nil
}

func (a *Adapter) Create(_ context.Context, accountID string, in budget.ImportedFields) (*budget.Transaction, error) {
	t, err := a.db.CreateTransaction(
		in.Date, &Account{ID: accountID},
		in.PayeeName, in.Notes, in.AmountCents, in.Cleared,
		in.ExternalRef, in.ImportedPayee,
	)
	if err != nil {
		return nil, err
	}
	return a.remember(t), nil
}

func (a *Adapter) Update(_ context.Context, t *budget.Transaction, p budget.Patch) error {
	native := a.lookup(t.ID)
	if native == nil {
		return fmt.Errorf("unknown transaction %q", t.ID)
	}

	if p.AmountCents != nil {
		if _, err := a.db.sql.Exec(`UPDATE transactions SET amount = ? WHERE id = ?`, *p.AmountCents, native.ID); err != nil {
			return fmt.Errorf("update amount: %w", err)
		}
		a.db.track("transactions", native.ID, "amount", int(*p.AmountCents))
		native.AmountCents = *p.AmountCents
	}
	if p.ExternalRef != nil {
		if _, err := a.db.sql.Exec(`UPDATE transactions SET financial_id = ? WHERE id = ?`, *p.ExternalRef, native.ID); err != nil {
			return fmt.Errorf("update financial_id: %w", err)
		}
		a.db.track("transactions", native.ID, "financial_id", *p.ExternalRef)
		native.FinancialID = *p.ExternalRef
	}
	if p.Notes != nil {
		if _, err := a.db.sql.Exec(`UPDATE transactions SET notes = ? WHERE id = ?`, *p.Notes, native.ID); err != nil {
			return fmt.Errorf("update notes: %w", err)
		}
		a.db.track("transactions", native.ID, "notes", *p.Notes)
		native.Notes = *p.Notes
	}
	if p.Cleared != nil && *p.Cleared {
		if _, err := a.db.sql.Exec(`UPDATE transactions SET cleared = 1 WHERE id = ?`, native.ID); err != nil {
			return fmt.Errorf("update cleared: %w", err)
		}
		a.db.track("transactions", native.ID, "cleared", 1)
		native.Cleared = true
	}
	if p.PayeeName != nil {
		pid, err := a.db.getOrCreatePayee(*p.PayeeName)
		if err != nil {
			return fmt.Errorf("resolve payee: %w", err)
		}
		if pid != native.PayeeID {
			if _, err := a.db.sql.Exec(`UPDATE transactions SET description = ? WHERE id = ?`, pid, native.ID); err != nil {
				return fmt.Errorf("update description: %w", err)
			}
			a.db.track("transactions", native.ID, "description", pid)
			native.PayeeID = pid
			native.PayeeName = *p.PayeeName
		}
	}
	if p.ImportedPayee != nil {
		if _, err := a.db.sql.Exec(`UPDATE transactions SET imported_description = ? WHERE id = ?`, *p.ImportedPayee, native.ID); err != nil {
			return fmt.Errorf("update imported_description: %w", err)
		}
		a.db.track("transactions", native.ID, "imported_description", *p.ImportedPayee)
		native.ImportedPayee = *p.ImportedPayee
	}

	budget.Apply(t, p)
	return nil
}

func (a *Adapter) ApplyRules(_ context.Context, touched []*budget.Transaction) (int, error) {
	rules, err := a.db.LoadRules()
	if err != nil {
		return 0, err
	}
	applied := 0
	for _, bt := range touched {
		native := a.lookup(bt.ID)
		if native == nil {
			continue
		}
		for _, r := range rules.Match(native) {
			applied += r.Apply(a.db, native)
		}
		refresh(bt, native)
	}
	if applied > 0 {
		log.Printf("Applied %d rule action(s) to %d transaction(s)", applied, len(touched))
	}
	return applied, nil
}

func (a *Adapter) remember(t *Transaction) *budget.Transaction {
	a.mu.Lock()
	a.native[t.ID] = t
	a.mu.Unlock()
	return toBudget(t)
}

func (a *Adapter) lookup(id string) *Transaction {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.native[id]
}

func toBudget(t *Transaction) *budget.Transaction {
	return &budget.Transaction{
		ID:            t.ID,
		AccountID:     t.AccountID,
		Date:          t.Date,
		AmountCents:   t.AmountCents,
		PayeeName:     t.PayeeName,
		Notes:         t.Notes,
		ExternalRef:   t.FinancialID,
		ImportedPayee: t.ImportedPayee,
		Cleared:       t.Cleared,
		Reconciled:    t.Reconciled,
	}
}

func refresh(bt *budget.Transaction, native *Transaction) {
	bt.PayeeName = native.PayeeName
	bt.Notes = native.Notes
	bt.Cleared = native.Cleared
	bt.AmountCents = native.AmountCents
}

// SetOpeningBalance writes the opening balance as an ordinary cleared
// transaction, which is how Actual models one — it has no balance column and
// derives every total from transactions.
//
// The row is written cleared and carrying ob.Ref as its financial_id. Both
// matter: budget.adoptable treats a cleared row with a foreign external
// reference as untouchable, so an incoming bank transaction of the same amount
// near the window start cannot swallow the opening balance instead of creating
// its own row.
func (a *Adapter) SetOpeningBalance(_ context.Context, accountID string, ob budget.OpeningBalance) (bool, error) {
	acct := &Account{ID: accountID}
	if existing := a.db.findByFinancialID(acct, ob.Ref); existing != nil {
		return false, nil
	}

	t, err := a.db.CreateTransaction(ob.Date, acct, ob.PayeeName, "", ob.AmountCents, true, ob.Ref, "")
	if err != nil {
		return false, fmt.Errorf("create opening balance on account %s: %w", accountID, err)
	}

	a.mu.Lock()
	a.native[t.ID] = t
	a.mu.Unlock()
	return true, nil
}

// AccountBalance totals an account the way Actual displays it.
//
// Split parents are excluded: their amount repeats the sum of their children,
// so counting both double-counts every split transaction.
func (a *Adapter) AccountBalance(_ context.Context, accountID string) (int64, error) {
	var cents int64
	err := a.db.sql.QueryRow(
		`SELECT COALESCE(SUM(amount), 0) FROM transactions
		  WHERE acct = ? AND tombstone = 0 AND isParent = 0`,
		accountID,
	).Scan(&cents)
	if err != nil {
		return 0, fmt.Errorf("total account %s: %w", accountID, err)
	}
	return cents, nil
}
