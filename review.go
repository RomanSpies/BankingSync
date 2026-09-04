package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"bankingsync/budget"
	"bankingsync/logs"
	"bankingsync/store"
	"bankingsync/web"
)

// HeldTransactions lists everything waiting for a decision, each with the
// budget rows it might belong to.
//
// A held transaction that cannot be given candidates is still listed, with the
// reason in Unavailable. The queue's whole purpose is that nothing goes missing
// quietly, and a row that vanished from the page because its backend was down
// would be exactly that.
func (s *Syncer) HeldTransactions(ctx context.Context) ([]web.ReviewItem, error) {
	reviews, err := s.st.GetMatchReviews()
	if err != nil {
		return nil, fmt.Errorf("read the review queue: %w", err)
	}
	if len(reviews) == 0 {
		return nil, nil
	}

	if !s.tryAcquire() {
		return nil, web.Refuse("a sync is running — try again in a moment")
	}
	defer s.release()

	accounts, err := s.st.GetAllBankAccounts()
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]store.BankAccount, len(accounts))
	for _, a := range accounts {
		byID[a.ID] = a
	}

	version := s.matchPolicy("").Version()
	out := make([]web.ReviewItem, 0, len(reviews))
	for _, r := range reviews {
		item := web.ReviewItem{
			ID:           r.ID,
			Date:         r.TxnDate,
			Amount:       centsToDecimal(r.AmountCents),
			Currency:     r.Currency,
			Payee:        r.Payee,
			HeldAt:       shortTimestamp(r.CreatedAt),
			ParamVersion: version,
		}
		acct, ok := byID[r.BankAccountID]
		if !ok {
			item.BankName = "(removed account)"
			item.Unavailable = "The bank account this came from is no longer connected, so " +
				"this cannot be decided either way. Removing an account clears its held " +
				"transactions, so a row left here means the database was edited directly."
			out = append(out, item)
			continue
		}
		item.BankName = bankLabel(acct)
		item.BudgetAccount = acct.ActualAccount

		cands, _, _, err := s.heldCandidates(ctx, r, acct)
		if err != nil {
			item.Unavailable = err.Error()
			out = append(out, item)
			continue
		}
		for _, c := range cands {
			item.Candidates = append(item.Candidates, web.ReviewCandidate{
				ID:        c.Transaction.ID,
				Date:      c.Transaction.Date.Format("2006-01-02"),
				Amount:    centsToDecimal(c.Transaction.AmountCents),
				PayeeName: c.Transaction.PayeeName,
				Percent:   percent(c.Probability),
				Why:       explainComparison(c.Comparison, c.Transaction.Date, r.TxnDate),
			})
		}
		out = append(out, item)
	}
	return out, nil
}

// ResolveHeld carries out one decision from the review page.
//
// The candidates are recomputed rather than trusted from the form, and the
// probability the user saw is checked against the fresh one. A budget the user
// edited between drawing the page and pressing the button would otherwise let a
// decision be applied to something other than what was on screen — the same
// guard the opening balance uses for its expected figure.
func (s *Syncer) ResolveHeld(
	ctx context.Context, id int64, candidateID string, shownPercent int, paramVersion string,
) error {
	r, err := s.st.GetMatchReview(id)
	if err != nil {
		return web.Refuse("that decision is no longer in the queue")
	}

	// Before anything else, and on both answers. A probability check cannot see
	// a threshold move — thresholds do not enter the probability — and the "this
	// is new" answer never had a probability to check in the first place, so
	// until now that path was checked for nothing at all.
	if current := s.matchPolicy("").Version(); paramVersion != "" && paramVersion != current {
		return web.Refuse("the matching settings changed while this page was open, so the " +
			"figures on it were worked out under different rules. Look at it again")
	}

	if !s.tryAcquire() {
		return web.Refuse("a sync is running — try again in a moment")
	}
	defer s.release()

	accounts, err := s.st.GetAllBankAccounts()
	if err != nil {
		return err
	}
	var acct store.BankAccount
	for _, a := range accounts {
		if a.ID == r.BankAccountID {
			acct = a
		}
	}

	in := importedFieldsOf(r)

	if candidateID == "" {
		if acct.ID == 0 {
			return web.Refuse("the bank account this came from is no longer connected")
		}
		accountID, err := s.budgetAccountID(ctx, acct)
		if err != nil {
			return err
		}
		t, err := s.ac.Create(ctx, accountID, in)
		if err != nil {
			return fmt.Errorf("create the transaction: %w", err)
		}
		if err := s.publish(ctx); err != nil {
			return err
		}
		return s.releaseHeld(ctx, r, acct, t, true, nil, false)
	}

	if acct.ID == 0 {
		return web.Refuse("the bank account this came from is no longer connected, " +
			"so there is nothing to merge into")
	}
	cands, _, pol, err := s.heldCandidates(ctx, r, acct)
	if err != nil {
		return err
	}
	var chosen *budget.Candidate
	for i := range cands {
		if cands[i].Transaction.ID == candidateID {
			chosen = &cands[i]
		}
	}
	// Whether this was the model's own first pick. The list is ordered best
	// first, so the comparison is with the head — and it is the only measurement
	// anywhere of whether the model ranks candidates the way a person does.
	chosenWasBest := len(cands) > 0 && cands[0].Transaction.ID == candidateID
	if chosen == nil {
		return web.Refuse("that transaction is no longer available to merge into — " +
			"it may have been edited or deleted. Look at the list again")
	}
	if got := percent(chosen.Probability); got != shownPercent {
		return web.Refuse("the budget changed since this page was drawn: that match is now %d%%, "+
			"not %d%%. Look at it again before deciding", got, shownPercent)
	}

	if err := budget.Adopt(ctx, s.ac, chosen.Transaction, in, pol); err != nil {
		return fmt.Errorf("merge the transaction: %w", err)
	}
	if err := s.publish(ctx); err != nil {
		return err
	}
	return s.releaseHeld(ctx, r, acct, chosen.Transaction, false, chosen, chosenWasBest)
}

// publish flushes a buffering backend before the decision is struck from the
// queue.
//
// Actual buffers writes locally and publishes them in one step at the end of a
// sync. A decision cleared from the queue but never published would be gone
// from the page and absent from the budget at the same time, which is the one
// outcome this whole queue exists to prevent. Firefly writes through and has
// nothing to do here.
func (s *Syncer) publish(ctx context.Context) error {
	f, ok := s.ac.(budget.Flusher)
	if !ok {
		return nil
	}
	if err := f.Commit(ctx); err != nil {
		// The same failure the sync loop counts, reached from a different caller.
		// Counting it only there would make commit trouble look like it only ever
		// happens during a sync.
		if s.met != nil {
			s.met.commitErrors.Add(ctx, 1,
				metric.WithAttributes(attribute.String("backend", s.backendName)))
		}
		olog.Error(ctx, "budget.commit.failed_in_review",
			logs.String("backend", s.backendName),
			logs.String("error", err.Error()))
		return fmt.Errorf("publish the change to the budget: %w", err)
	}
	return nil
}

// heldCandidates recomputes what a held transaction could belong to.
//
// Nothing about the candidate set is stored with the held row. A saved row may
// since have been split, edited or deleted, so a stored candidate could name
// something that is not there; and on the Actual backend this listing call is
// also what warms the in-process map that a later update depends on.
func (s *Syncer) heldCandidates(
	ctx context.Context, r store.MatchReview, acct store.BankAccount,
) ([]budget.Candidate, string, budget.Policy, error) {
	pol := s.matchPolicy(bankLabel(acct))
	pol.OnNearMiss = nil

	if r.Backend != "" && r.Backend != s.backendName {
		return nil, "", pol, web.Refuse("this was held while %s was the budget backend, and "+
			"transaction references do not carry across. Choose \"new transaction\" or discard it "+
			"by resetting the import state", web.BackendDisplayName(r.Backend))
	}

	accountID, err := s.budgetAccountID(ctx, acct)
	if err != nil {
		return nil, "", pol, err
	}

	in := importedFieldsOf(r)
	from, to := budget.WindowBounds(in.Date)
	existing, err := s.ac.ListTransactions(ctx, accountID, from, to)
	if err != nil {
		return nil, "", pol, fmt.Errorf("read the budget: %w", err)
	}
	return budget.Assess(existing, in, nil, pol), accountID, pol, nil
}

func (s *Syncer) budgetAccountID(ctx context.Context, acct store.BankAccount) (string, error) {
	if s.ac == nil {
		st, err := s.connectBackend(ctx)
		if err != nil {
			return "", fmt.Errorf("connect to the budget backend: %w", err)
		}
		s.ac = st
	}
	a, err := s.ac.GetOrCreateAccount(ctx, budget.AccountSpec{
		Name: budgetAccountName(acct, s.backendName), Currency: acct.Currency, IBAN: acct.IBAN,
	})
	if err != nil {
		return "", fmt.Errorf("resolve the budget account: %w", err)
	}
	return a.ID, nil
}

// releaseHeld records a decision that has already been written to the budget.
//
// The order matters and is the same one the sync loop uses: the backend first,
// then the bookkeeping that stops the transaction coming round again. A failure
// after the write leaves a row that will be offered a second time, which is a
// visible duplicate; the reverse would lose it silently.
func (s *Syncer) releaseHeld(
	ctx context.Context, r store.MatchReview, acct store.BankAccount,
	t *budget.Transaction, created bool, chosen *budget.Candidate, wasBest bool,
) error {
	if created && !r.Cleared {
		if err := s.state.SetPending(r.BankAccountID, r.PendingKey, t.ID, r.TxnDate, s.st); err != nil {
			bookkeepingFailed(ctx, "SetPending", bankLabel(acct), r.ExternalRef, err)
		}
	}
	if r.ExternalRef != "" && r.Cleared {
		if err := s.state.AddImportedRef(r.BankAccountID, r.ExternalRef, r.TxnDate, s.st); err != nil {
			bookkeepingFailed(ctx, "AddImportedRef", bankLabel(acct), r.ExternalRef, err)
		}
	}
	// What the model said, and what turned out to be so. These are the only
	// observations available that do not come from the model itself, which is
	// what makes them worth keeping.
	//
	// Which pair the answer is about matters, and it used to be assumed. The
	// decision was logged with the levels of the model's best candidate, but the
	// page offers every candidate in the window: merging into the second one
	// filed "this was a match" against the first one's levels — against a pair the
	// person had just declined. The m probabilities are estimated from these
	// labels, so a level that loses reviews would come to look like one that wins
	// them. The answer is now recorded against the candidate actually chosen.
	//
	// "This is new" needs no such care. It refutes the best candidate, which is
	// the pair already on the row, and it refutes every other candidate too.
	if err := s.recordReviewAnswer(r, created, chosen); err != nil {
		log.Printf("[%s] could not record the outcome of a decision: %v", bankLabel(acct), err)
	}

	if err := s.st.DeleteMatchReview(r.ID); err != nil {
		return fmt.Errorf("the change was made, but clearing it from the queue failed: %w", err)
	}
	s.state.DeleteHeldKey(r.BankAccountID, r.PendingKey)

	outcome := "assigned"
	if created {
		outcome = "imported"
	}
	// Three outcomes and not two, because "merged into a different row" is a
	// statement about the model and "called it new" is a statement about the
	// window. Collapsing them would lose the only ranking measurement there is.
	choice := "created_new"
	switch {
	case !created && wasBest:
		choice = "model_best"
	case !created:
		choice = "other_candidate"
	}
	if s.met != nil {
		s.met.matchLabels.Add(ctx, 1, metric.WithAttributes(
			attribute.String("source", "review"),
			attribute.String("bank", bankLabel(acct))))
		s.met.reviewChoice.Add(ctx, 1, metric.WithAttributes(
			attribute.String("choice", choice),
			attribute.String("bank", bankLabel(acct)),
			attribute.String("backend", s.backendName)))
		s.met.matchReviews.Add(ctx, 1, metric.WithAttributes(
			attribute.String("outcome", outcome),
			// A decision has no doubt left to describe, but the label set has to
			// match the queued case or the series cannot be summed across outcomes.
			attribute.String("reason", "decided"),
			attribute.String("bank", bankLabel(acct)),
			attribute.String("backend", s.backendName)))
	}
	log.Printf("[%s] review resolved: %s %s as %s", bankLabel(acct),
		centsToDecimal(r.AmountCents), r.Payee, outcome)
	olog.Info(ctx, "match.review_resolved",
		logs.String("bank", bankLabel(acct)),
		logs.String("outcome", outcome),
		logs.String("choice", choice),
		logs.String("payee", r.Payee),
		logs.Float64("best_probability", r.BestProbability))
	return nil
}

func importedFieldsOf(r store.MatchReview) budget.ImportedFields {
	date, _ := time.Parse("2006-01-02", r.TxnDate)
	return budget.ImportedFields{
		Date:             date,
		AmountCents:      r.AmountCents,
		Currency:         r.Currency,
		PayeeName:        r.Payee,
		Notes:            r.Notes,
		ExternalRef:      r.ExternalRef,
		ImportedPayee:    r.ImportedPayee,
		Cleared:          r.Cleared,
		CounterpartyIBAN: r.CounterpartyIBAN,
		SEPA: budget.SEPARefs{
			EndToEnd:   r.SEPAEndToEnd,
			Mandate:    r.SEPAMandate,
			CreditorID: r.SEPACreditorID,
		},
	}
}

// percent rounds a probability to whole percent, once, so that the figure shown
// on the page and the figure checked on the way back are produced by the same
// expression rather than by two roundings that agree most of the time.
func percent(p float64) int { return int(math.Round(100 * p)) }

// explainComparison puts the model's own reasoning into words. Every field
// contributes a separate term, so the reason costs nothing to produce — and a
// number a person cannot argue with is not a decision aid.
func explainComparison(c budget.Comparison, candidate time.Time, heldDate string) string {
	parts := make([]string, 0, 3)

	switch c.Amount {
	case budget.AmountExact:
		parts = append(parts, "amount to the cent")
	case budget.AmountHigherWithin:
		parts = append(parts, "amount within tolerance, above")
	case budget.AmountLowerWithin:
		parts = append(parts, "amount within tolerance, below")
	default:
		parts = append(parts, "amount differs")
	}

	switch c.Payee {
	case budget.PayeeExact:
		parts = append(parts, "same payee")
	case budget.PayeeTruncated:
		parts = append(parts, "payee cut short")
	case budget.PayeeSubset:
		parts = append(parts, "payee contained in the other")
	case budget.PayeeFuzzy:
		parts = append(parts, "payee spelled differently")
	case budget.PayeeConflict:
		parts = append(parts, "payee contradicts")
	case budget.PayeeMissing:
		parts = append(parts, "no payee to compare")
	default:
		parts = append(parts, "different payee")
	}

	if held, err := time.Parse("2006-01-02", heldDate); err == nil {
		switch d := daysBetween(candidate, held); {
		case d == 0:
			parts = append(parts, "same day")
		case d == 1:
			parts = append(parts, "1 day apart")
		default:
			parts = append(parts, fmt.Sprintf("%d days apart", d))
		}
	}
	return strings.Join(parts, " · ")
}

func daysBetween(a, b time.Time) int {
	da := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	db := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	d := int(da.Sub(db).Hours() / 24)
	if d < 0 {
		return -d
	}
	return d
}

// shortTimestamp trims the seconds off a stored timestamp for display, leaving
// the value alone if it is not in the shape SQLite writes.
func shortTimestamp(v string) string {
	if t, err := time.Parse("2006-01-02 15:04:05", v); err == nil {
		return t.Format("2006-01-02 15:04")
	}
	return v
}

// minLabelledDecisions is how many settled decisions there have to be before a
// calibration figure is worth reporting.
//
// A Brier score over a dozen answers is noise with a decimal point. The
// threshold is stated rather than derived, like the others in this program, and
// it errs towards silence: a missing series is honest, a wrong one is not.
const minLabelledDecisions = 50

// labelledDecisions collects the decisions something independent of the model
// has since settled — a person's answer, for now.
//
// These are the only observations available that the model did not produce
// itself, which is what makes them worth anything: a model scored against its
// own opinion always agrees with itself.
func (s *Syncer) labelledDecisions() []budget.Observation {
	decisions, err := s.st.GetLabelledMatchDecisions(2000)
	if err != nil {
		return nil
	}
	var out []budget.Observation
	for _, d := range decisions {
		out = append(out, budget.Observation{Weight: d.Weight, Match: *d.Truth})
	}
	if len(out) < minLabelledDecisions {
		return nil
	}
	return out
}

// levelCounts tallies the settled decisions by comparison level.
//
// The bridge from what was recorded to what could be estimated. Passing zero for
// the account counts the whole installation, which is the middle step of the
// hierarchical estimate — a bank that has never seen a level of its own starts
// from how the others behave rather than from the claim that it cannot happen.
//
// Only settled decisions count. An unanswered one is not evidence that the model
// was right; it is evidence that nobody has looked.
func (s *Syncer) levelCounts(bankAccountID int64) budget.LevelCounts {
	out := budget.LevelCounts{
		PayeeM: map[budget.PayeeLevel]int{}, PayeeU: map[budget.PayeeLevel]int{},
		AmountM: map[budget.AmountLevel]int{}, AmountU: map[budget.AmountLevel]int{},
		DateM: map[budget.DateLevel]int{}, DateU: map[budget.DateLevel]int{},
	}
	decisions, err := s.st.GetLabelledMatchDecisions(100000)
	if err != nil {
		return out
	}

	for _, d := range decisions {
		if bankAccountID != 0 && d.BankAccountID != bankAccountID {
			continue
		}
		if p, ok := payeeLevelByName[d.PayeeLevel]; ok {
			if *d.Truth {
				out.PayeeM[p]++
			} else {
				out.PayeeU[p]++
			}
		}
		if a, ok := amountLevelByName[d.AmountLevel]; ok {
			if *d.Truth {
				out.AmountM[a]++
			} else {
				out.AmountU[a]++
			}
		}
		if x, ok := dateLevelByName[d.DateLevel]; ok {
			if *d.Truth {
				out.DateM[x]++
			} else {
				out.DateU[x]++
			}
		}
	}
	return out
}

// The level tables are written down as names, because a stored number would tie
// the log to the order the constants happen to have and a level inserted in the
// middle would silently rewrite history.
var (
	payeeLevelByName  = map[string]budget.PayeeLevel{}
	amountLevelByName = map[string]budget.AmountLevel{}
	dateLevelByName   = map[string]budget.DateLevel{}
)

func init() {
	for _, l := range []budget.PayeeLevel{budget.PayeeMissing, budget.PayeeNone,
		budget.PayeeConflict, budget.PayeeSubset, budget.PayeeTruncated,
		budget.PayeeFuzzy, budget.PayeeExact} {
		payeeLevelByName[l.String()] = l
	}
	for _, l := range []budget.AmountLevel{budget.AmountOutsideLower, budget.AmountOutsideHigher,
		budget.AmountLowerWithin, budget.AmountHigherWithin, budget.AmountExact} {
		amountLevelByName[l.String()] = l
	}
	for _, l := range []budget.DateLevel{budget.DateBeforeFar, budget.DateAfterFar,
		budget.DateBeforeNear, budget.DateAfterNear, budget.DateSame} {
		dateLevelByName[l.String()] = l
	}
}

// recordReviewAnswer files a review answer against the pair it is actually about.
func (s *Syncer) recordReviewAnswer(r store.MatchReview, created bool, chosen *budget.Candidate) error {
	if created || chosen == nil {
		return s.st.SetMatchDecisionTruth(r.BankAccountID, r.PendingKey, !created)
	}
	return s.st.SetMatchDecisionResolution(r.BankAccountID, r.PendingKey, true,
		store.ResolvedComparison{
			CandidateID: chosen.Transaction.ID,
			PayeeLevel:  chosen.Comparison.Payee.String(),
			AmountLevel: chosen.Comparison.Amount.String(),
			DateLevel:   chosen.Comparison.Date.String(),
			Weight:      chosen.Weight,
			Probability: chosen.Probability,
		})
}
