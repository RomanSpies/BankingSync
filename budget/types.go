package budget

import "time"

type Account struct {
	ID       string
	Name     string
	Currency string
}

type Transaction struct {
	ID            string
	AccountID     string
	Date          time.Time
	AmountCents   int64
	Currency      string
	PayeeName     string
	Notes         string
	ExternalRef   string
	ImportedPayee string
	Cleared       bool
	Reconciled    bool
	Tags          []string
}

// AccountSpec identifies the budget account a bank account maps onto. Backends
// that can match on IBAN should prefer it over the name, which is free text a
// user can mistype into a second account.
type AccountSpec struct {
	Name     string
	Currency string
	IBAN     string
}

// SEPARefs are the structured references a SEPA remittance carries. Backends
// with dedicated fields for them should not fold them into free text.
type SEPARefs struct {
	EndToEnd   string
	Mandate    string
	CreditorID string
}

type ImportedFields struct {
	Date          time.Time
	AmountCents   int64
	Currency      string
	PayeeName     string
	Notes         string
	ExternalRef   string
	ImportedPayee string
	Cleared       bool

	// CounterpartyIBAN is the other side's account. When it resolves to an
	// asset account the user already holds, the transaction is a transfer
	// rather than a payment to a made-up expense account.
	CounterpartyIBAN string

	SEPA SEPARefs
}

type Patch struct {
	AmountCents   *int64
	PayeeName     *string
	Notes         *string
	ExternalRef   *string
	ImportedPayee *string
	Cleared       *bool
}

func (p Patch) IsEmpty() bool {
	return p.AmountCents == nil &&
		p.PayeeName == nil &&
		p.Notes == nil &&
		p.ExternalRef == nil &&
		p.ImportedPayee == nil &&
		p.Cleared == nil
}

func Int64(v int64) *int64    { return &v }
func String(v string) *string { return &v }
func Bool(v bool) *bool       { return &v }

func (t *Transaction) HasTag(name string) bool {
	for _, tag := range t.Tags {
		if tag == name {
			return true
		}
	}
	return false
}
