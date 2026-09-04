package budget

import (
	"context"
	"errors"
	"time"
)

// ErrGone reports that a transaction the caller still holds a reference to no
// longer exists in the backend. Callers must not recreate it: the user deleted
// it deliberately, and re-importing would fight them every sync.
var ErrGone = errors.New("transaction no longer exists in the backend")

// Store is the minimum a budgeting backend must provide. All matching and merge
// policy lives in this package, not behind the interface, so that both backends
// are known to behave identically.
//
// Durability contract: a write method returning nil has either made its change
// durable, or queued it for automatic retry by a later Commit. A non-nil error
// from any write method means the change may not exist, and the caller must not
// advance any watermark for the affected account. Atomicity is per call, never
// per batch.
//
// Update precondition: backends that model a transaction as a group of splits
// must refuse to update a group holding more than one split, because a partial
// write would destroy the splits the user created.
type Store interface {
	Ping(ctx context.Context) error
	Close()

	GetOrCreateAccount(ctx context.Context, spec AccountSpec) (*Account, error)

	// ListTransactions returns transactions in the half-open window [from, to).
	ListTransactions(ctx context.Context, accountID string, from, to time.Time) ([]*Transaction, error)

	// FindByExternalRef looks up a single transaction by the bank's reference,
	// scoped to accountID. It returns nil without error when nothing matches.
	FindByExternalRef(ctx context.Context, accountID, ref string) (*Transaction, error)

	Create(ctx context.Context, accountID string, in ImportedFields) (*Transaction, error)
	Update(ctx context.Context, t *Transaction, p Patch) error
}

// Flusher is implemented by backends that buffer writes locally and publish them
// in one step. Write-through backends do not implement it.
type Flusher interface {
	Commit(ctx context.Context) error
}

// Describer is implemented by backends that can report the remote software
// version, which operators need when a compatibility problem is suspected.
type Describer interface {
	BackendVersion(ctx context.Context) (string, error)
}

// RuleRunner is implemented by backends that apply user rules as a distinct step
// after import. Backends whose rules run server-side do not implement it.
type RuleRunner interface {
	ApplyRules(ctx context.Context, touched []*Transaction) (int, error)
}

// OpeningBalance is the money an account held before the first imported
// transaction, expressed so a backend can record it in whatever way it models
// such a thing.
type OpeningBalance struct {
	AmountCents int64
	Currency    string
	Date        time.Time

	// Ref is the idempotency key. It also keeps the resulting row out of the
	// reconciler: a cleared transaction carrying a foreign external reference is
	// not adoptable, so an incoming bank transaction of the same amount cannot
	// swallow the opening balance instead of creating its own row.
	Ref string

	PayeeName string
}

// OpeningBalanceWriter is implemented by backends that can carry an opening
// balance.
//
// SetOpeningBalance must be idempotent against Ref: a second call with the same
// Ref leaves exactly one opening balance behind and reports written=false. The
// caller never re-adjusts an opening balance once written, so a backend that
// silently overwrites turns a stale call into a wrong balance.
type OpeningBalanceWriter interface {
	SetOpeningBalance(ctx context.Context, accountID string, ob OpeningBalance) (bool, error)
}

// BalanceReader is implemented by backends that can total an account. It exists
// for drift detection only; a backend without it simply reports no drift.
type BalanceReader interface {
	AccountBalance(ctx context.Context, accountID string) (int64, error)
}
