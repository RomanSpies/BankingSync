// Package linkagegen builds synthetic account histories whose truth is known.
//
// It exists because the parts of the matcher that learn cannot be verified any
// other way. An installation without a truncating bank never produces the
// evidence those parts consume, so the only remaining way to show that the
// arithmetic does what it claims is to feed it data whose answers are recorded
// in advance.
//
// # What this package must never be used for
//
// **Calibration against this generator is circular.** Its settlement rates, its
// truncation frequency and its branch-collision rate *are* the m parameters
// written in another notation; fitting m to data drawn from them recovers the
// input and proves nothing. The generator can show that an estimator converges,
// that a correction removes an error that was deliberately introduced, and that
// a decision does not depend on the order of its inputs. It cannot show that any
// of it helps a real user, and no test in this repository may claim otherwise.
//
// The bank behaviours modelled here are parameters, not constants. Merchant name
// fields in card messages are fixed-width with positional subfields, but the
// split differs between processor specifications — so the width is configured
// and nothing about it is hard-coded.
package linkagegen

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

// Status values as the bank reports them, matching the sync loop's vocabulary.
const (
	Pending = "PDNG"
	Booked  = "BOOK"
)

// Row is one line as the bank reports it.
//
// PurchaseID is the truth: two rows carrying the same one are the same payment,
// however differently the bank chose to describe them.
type Row struct {
	PurchaseID  int
	Status      string
	Payee       string
	AmountCents int64
	Date        time.Time
	EntryRef    string
}

// Purchase is one real payment, before the bank said anything about it.
type Purchase struct {
	ID          int
	Merchant    string
	AmountCents int64
	Date        time.Time

	// Settled reports whether the authorisation has been followed by a booking.
	// An unsettled purchase is the ordinary state of a recent card payment, not
	// an error.
	Settled bool
}

// Config describes one bank's habits and the shape of the history to draw.
//
// Every field is a claim about behaviour that can be argued with, which is the
// point: a generator with hidden constants would smuggle assumptions into every
// test that used it.
type Config struct {
	Seed uint64

	// Days is how far back the history reaches, and Start is the day it begins.
	// Start defaults to a fixed date so a history is reproducible across runs;
	// a caller driving the real importer sets it relative to today, because the
	// sync window is.
	Days  int
	Start time.Time

	// Purchases is how many real payments to draw, before duplicates.
	Purchases int

	// FieldWidth is the width of the bank's merchant name field in characters.
	// Zero means the bank does not truncate.
	FieldWidth int

	// Prefix is the card scheme label the bank prepends once a payment settles,
	// eating into the field width. Empty means it prepends nothing.
	Prefix string

	// References reports whether the bank supplies an entry reference. Many do
	// not for authorisations, which is why the importer has a fallback identity.
	References bool

	// StableReference makes a booking carry the same reference as the
	// authorisation it settles, which is what an institution does when it treats
	// the two as one entry changing state. The alternative — a fresh reference on
	// settlement — is also common, and the difference decides whether the matcher
	// is consulted at all: with a stable reference the pending map recognises the
	// pair and nothing is weighed.
	StableReference bool

	// SettleMinDays and SettleMaxDays bound the gap between authorisation and
	// booking.
	SettleMinDays, SettleMaxDays int

	// UnsettledChance is the share of authorisations still open at the end of
	// the history.
	UnsettledChance float64

	// UpliftChance covers tips and hotel incidentals — the booking exceeds the
	// authorisation. ReductionChance covers a fuel release, where it falls short.
	// They are separate mechanisms with separate rates, because they are.
	UpliftChance, UpliftMaxPercent       float64
	ReductionChance, ReductionMaxPercent float64

	// BranchChance is the share of purchases that draw a second merchant sharing
	// the same base name with a different town — the pair the comparison levels
	// exist to tell apart.
	BranchChance float64

	// DuplicateChance is the share of purchases repeated identically on the same
	// day: same merchant, same amount, two payments. Nothing in a comparison can
	// separate these, which is what makes them the assignment's problem rather
	// than the model's.
	DuplicateChance float64

	// Merchants is the name pool. When empty a default pool is used. The draw is
	// deliberately skewed — a supermarket appears constantly and a restaurant
	// abroad once — because a uniform payee distribution would make the
	// frequency correction untestable.
	Merchants []string
}

// History is a drawn account history together with its truth.
type History struct {
	Rows      []Row
	Purchases []Purchase
}

// Truth maps a row index to the purchase it came from.
func (h History) Truth() map[int]int {
	out := make(map[int]int, len(h.Rows))
	for i, r := range h.Rows {
		out[i] = r.PurchaseID
	}
	return out
}

// FeedAsOf is the statement as the bank would serve it on a given day.
//
// A bank does not offer an authorisation and its settlement side by side: the
// pending row stands until the payment clears and is then replaced by the booked
// one. Returning both — which an earlier version of this package did — models a
// bank that does not exist, and it makes every purchase look like a duplicate the
// importer failed to merge.
//
// Rows keeps the full record for truth; this is what the importer gets to see.
func (h History) FeedAsOf(day time.Time) []Row {
	byPurchase := make(map[int][]Row, len(h.Purchases))
	for _, r := range h.Rows {
		byPurchase[r.PurchaseID] = append(byPurchase[r.PurchaseID], r)
	}

	var out []Row
	for _, p := range h.Purchases {
		var latest *Row
		for i, r := range byPurchase[p.ID] {
			if r.Date.After(day) {
				continue
			}
			// Rows are recorded authorisation first, so the later one wins.
			latest = &byPurchase[p.ID][i]
		}
		if latest != nil {
			out = append(out, *latest)
		}
	}
	return out
}

// RowsFor returns every row the bank produced for one purchase.
func (h History) RowsFor(purchaseID int) []Row {
	var out []Row
	for _, r := range h.Rows {
		if r.PurchaseID == purchaseID {
			out = append(out, r)
		}
	}
	return out
}

var defaultMerchants = []string{
	"Edeka Sued", "Aldi Nord", "Shell Tankstelle", "Deutsche Bahn",
	"Rewe City", "Rossmann", "Hotel Berlin", "Da Luigi", "Kiosk Mueller",
	"Zeitschriften Weber", "Elektromarkt Nord", "App Store",
}

var towns = []string{"Roma", "Milano", "Nuernberg", "Muenchen", "Koeln", "Hamburg"}

// Generate draws a history. The same Config yields the same history.
//
// Seeded and reproducible is the requirement, not unpredictable. A corpus that
// differed between runs would make every measurement taken against it
// unrepeatable and every failure unbisectable, so a cryptographic source would
// be the wrong tool here rather than a stronger one. Nothing this package
// produces is a secret, a token or an identifier outside a test.
func Generate(cfg Config) History {
	cfg = withDefaults(cfg)
	r := rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9e3779b9)) // #nosec G404 -- reproducible test corpora, see above

	base := cfg.Start
	var h History
	nextID := 1
	ref := 0
	newRef := func() string {
		if !cfg.References {
			return ""
		}
		ref++
		return fmt.Sprintf("ref-%04d", ref)
	}

	emit := func(merchant string, day int, cents int64) {
		p := Purchase{
			ID: nextID, Merchant: merchant, AmountCents: cents,
			Date: base.AddDate(0, 0, day),
		}
		nextID++

		// The authorisation goes through the same field as the booking; what it
		// does not carry is the card scheme label, which the bank prepends only
		// once the payment settles. That is what makes the settled name the
		// shorter of the two despite being the longer string.
		h.Rows = append(h.Rows, Row{
			PurchaseID: p.ID, Status: Pending, Payee: Render(p.Merchant, "", cfg.FieldWidth),
			AmountCents: p.AmountCents, Date: p.Date, EntryRef: newRef(),
		})

		authRef := h.Rows[len(h.Rows)-1].EntryRef

		if r.Float64() >= cfg.UnsettledChance {
			p.Settled = true
			gap := cfg.SettleMinDays
			if span := cfg.SettleMaxDays - cfg.SettleMinDays; span > 0 {
				gap += r.IntN(span + 1)
			}
			h.Rows = append(h.Rows, Row{
				PurchaseID: p.ID, Status: Booked,
				Payee:       Render(p.Merchant, cfg.Prefix, cfg.FieldWidth),
				AmountCents: settledAmount(r, cfg, p.AmountCents),
				Date:        p.Date.AddDate(0, 0, gap), EntryRef: settlementRef(cfg, authRef, newRef),
			})
		}
		h.Purchases = append(h.Purchases, p)
	}

	for i := 0; i < cfg.Purchases; i++ {
		chain := cfg.Merchants[skewedIndex(r, len(cfg.Merchants))]
		day := r.IntN(cfg.Days)
		cents := -int64(r.IntN(9000) + 99)

		// A branch collision is two branches of one chain, each carrying its own
		// town. Both have to name one: a bare chain name against a chain plus a
		// town is the shorter contained in the longer, which is precisely what a
		// truncated field looks like — so drawing it that way produces the case
		// this is not for, and measures the matcher against a puzzle nothing
		// could solve. The name is therefore chosen before anything is emitted
		// rather than patched into the rows afterwards, which loses the card
		// scheme prefix on the settled one.
		name := chain
		branch := ""
		if r.Float64() < cfg.BranchChance {
			a, b := r.IntN(len(towns)), r.IntN(len(towns))
			if b == a {
				b = (b + 1) % len(towns)
			}
			name, branch = chain+" "+towns[a], chain+" "+towns[b]
		}

		emit(name, day, cents)
		if branch != "" {
			emit(branch, day, cents)
		}
		if r.Float64() < cfg.DuplicateChance {
			// The same purchase again: two coffees, two app purchases.
			emit(name, day, cents)
		}
	}
	return h
}

// Render is how a merchant name reaches the statement once the bank has
// prepended its card scheme label and cut the result to its field width.
//
// Exported because the truncation rule is the thing under test in several
// places, and a second implementation of it in a test file would be a second
// thing to keep right.
func Render(merchant, prefix string, width int) string {
	s := merchant
	if prefix != "" {
		s = prefix + " " + s
	}
	if width <= 0 {
		return s
	}
	// Counted in characters rather than bytes. A real field is byte-oriented,
	// but the difference only shows on non-ASCII names and would model the
	// encoding rather than the truncation.
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return strings.TrimRight(string(runes[:width]), " ")
}

func settledAmount(r *rand.Rand, cfg Config, authorised int64) int64 {
	switch {
	case r.Float64() < cfg.UpliftChance:
		return scale(authorised, 1+r.Float64()*cfg.UpliftMaxPercent/100)
	case r.Float64() < cfg.ReductionChance:
		return scale(authorised, 1-r.Float64()*cfg.ReductionMaxPercent/100)
	default:
		return authorised
	}
}

func scale(cents int64, factor float64) int64 {
	out := int64(float64(cents) * factor)
	if out == 0 {
		return cents
	}
	return out
}

// skewedIndex draws a merchant with a long tail: the first names in the pool
// dominate the way a supermarket dominates a real statement, and the last appear
// once. A uniform draw would leave the frequency correction with nothing to
// correct.
func skewedIndex(r *rand.Rand, n int) int {
	if n <= 1 {
		return 0
	}
	for i := 0; i < n-1; i++ {
		if r.Float64() < 0.4 {
			return i
		}
	}
	return n - 1
}

func withDefaults(cfg Config) Config {
	if cfg.Days <= 0 {
		cfg.Days = 30
	}
	if cfg.Start.IsZero() {
		cfg.Start = time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	}
	if cfg.Purchases <= 0 {
		cfg.Purchases = 100
	}
	if cfg.SettleMaxDays < cfg.SettleMinDays {
		cfg.SettleMaxDays = cfg.SettleMinDays
	}
	if len(cfg.Merchants) == 0 {
		cfg.Merchants = defaultMerchants
	}
	return cfg
}

// settlementRef is the reference a booking carries: the authorisation's when the
// institution treats the two as one entry, a fresh one when it does not.
func settlementRef(cfg Config, authRef string, next func() string) string {
	if cfg.StableReference {
		return authRef
	}
	return next()
}
