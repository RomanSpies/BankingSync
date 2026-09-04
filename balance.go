package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"bankingsync/budget"
	"bankingsync/enablebanking"
	"bankingsync/internal/payeematch"
	"bankingsync/logs"
	"bankingsync/store"
	"bankingsync/web"
)

// openingRefPrefix keys the opening balance to the bank account rather than to
// our own row id, so removing and re-adding a bank does not produce a second
// opening balance beside the first.
const openingRefPrefix = "bankingsync-opening-"

// openingPayee is the payee both backends use for the opening balance row.
const openingPayee = "Starting Balance"

func openingRef(acct store.BankAccount) string {
	key := acct.IBAN
	if key == "" {
		key = acct.AccountUID
	}
	return openingRefPrefix + key
}

// readBalances fetches the account balances, translating a refused scope into a
// recorded state rather than a sync failure. Transactions are the product;
// balances are an addition, and losing them must not cost the user their import.
func (s *Syncer) readBalances(ctx context.Context, acct store.BankAccount) ([]enablebanking.Balance, error) {
	balances, err := s.eb.FetchBalances(ctx, acct.AccountUID)
	switch {
	case err == nil:
		if acct.BalancesAccess != "granted" {
			_ = s.st.SetBalancesAccess(acct.ID, "granted")
		}
		return balances, nil
	case errors.Is(err, enablebanking.ErrBalancesNotPermitted):
		_ = s.st.SetBalancesAccess(acct.ID, "denied")
		_ = s.st.SetOpeningBalanceState(acct.ID, store.OpeningBalanceDenied)
	case errors.Is(err, enablebanking.ErrBalancesUnsupported):
		_ = s.st.SetBalancesAccess(acct.ID, "unsupported")
		_ = s.st.SetOpeningBalanceState(acct.ID, store.OpeningBalanceUnavailable)
	}
	return nil, err
}

// settleBalances derives the opening balance on the first clean sync of an
// account and reports drift on every later one. It never corrects drift.
func (s *Syncer) settleBalances(
	ctx context.Context,
	acct store.BankAccount,
	budgetAccountID string,
	dateFrom time.Time,
	before []enablebanking.Balance,
	beforeErr error,
	fetched []enablebanking.Transaction,
	dropped int,
	writeFailed, interrupted bool,
) {
	if beforeErr != nil {
		log.Printf("[%s] balances unavailable: %v", bankLabel(acct), beforeErr)
		olog.Error(ctx, "balance.unavailable",
			logs.String("bank", bankLabel(acct)),
			logs.String("error", beforeErr.Error()))
		s.recordDrift(acct, 0, store.DriftNoBalance)
		return
	}

	writer, canWrite := s.ac.(budget.OpeningBalanceWriter)
	reader, canRead := s.ac.(budget.BalanceReader)
	if !canWrite && !canRead {
		s.recordDrift(acct, 0, store.DriftUnsupported)
		return
	}

	label := bankLabel(acct)
	picked, err := enablebanking.SelectBalance(before, acct.Currency)
	if err != nil {
		log.Printf("[%s] no usable booked balance: %v", label, err)
		olog.Warn(ctx, "balance.no_usable_type",
			logs.String("bank", label),
			logs.String("error", err.Error()))
		if acct.OpeningBalanceState == store.OpeningBalanceAuto {
			_ = s.st.SetOpeningBalanceState(acct.ID, store.OpeningBalanceUnavailable)
		}
		s.recordDrift(acct, 0, store.DriftNoBalance)
		return
	}

	// A second reading has to agree with the first. Anything else means the
	// account moved while we were importing, and both the opening balance and
	// the drift figure would be computed across a moving target.
	after, err := s.eb.FetchBalances(ctx, acct.AccountUID)
	if err != nil || !sameBalance(picked, after, acct.Currency) {
		log.Printf("[%s] balance moved during the run — deferring", label)
		olog.Warn(ctx, "balance.moved_during_run", logs.String("bank", label))
		s.recordDrift(acct, 0, store.DriftUnstable)
		return
	}

	// Counted once for both: a transaction waiting for a decision is missing from
	// the budget on purpose, which makes this run incomplete in the same sense a
	// failed write does.
	held := len(s.state.Held(acct.ID))

	if canWrite && acct.OpeningBalanceState == store.OpeningBalanceAuto {
		s.writeOpeningBalance(ctx, acct, budgetAccountID, dateFrom, picked, fetched,
			dropped, writeFailed, interrupted, held, writer)
	}

	if canRead {
		s.checkDrift(ctx, acct, budgetAccountID, picked, fetched, dropped, writeFailed, interrupted, held, reader)
	}
}

func (s *Syncer) writeOpeningBalance(
	ctx context.Context,
	acct store.BankAccount,
	budgetAccountID string,
	dateFrom time.Time,
	picked enablebanking.Balance,
	fetched []enablebanking.Transaction,
	dropped int,
	writeFailed, interrupted bool,
	held int,
	writer budget.OpeningBalanceWriter,
) {
	label := bankLabel(acct)
	if reason := deferReason(dropped, writeFailed, interrupted, held,
		picked, fetched, acct.Currency); reason != "" {
		log.Printf("[%s] opening balance deferred: %s", label, reason)
		olog.Warn(ctx, "opening_balance.deferred",
			logs.String("bank", label),
			logs.String("reason", reason))
		return
	}
	if !s.claim(acct) {
		return
	}

	opening := OpeningCents(picked, fetched)
	date := OpeningDate(dateFrom, fetched)

	written, err := writer.SetOpeningBalance(ctx, budgetAccountID, budget.OpeningBalance{
		AmountCents: opening,
		Currency:    picked.Currency,
		Date:        date,
		Ref:         openingRef(acct),
		PayeeName:   openingPayee,
	})
	if err != nil {
		log.Printf("[%s] opening balance write failed: %v", label, err)
		olog.Error(ctx, "opening_balance.write_failed",
			logs.String("bank", label),
			logs.String("error", err.Error()))
		_ = s.st.SetOpeningBalanceState(acct.ID, store.OpeningBalanceAuto)
		return
	}
	if err := s.st.SetOpeningBalance(acct.ID, opening, date.Format("2006-01-02"), openingRef(acct)); err != nil {
		log.Printf("[%s] opening balance recorded in the backend but not locally: %v", label, err)
		olog.Error(ctx, "opening_balance.not_recorded_locally",
			logs.String("bank", label),
			logs.String("error", err.Error()))
		return
	}
	log.Printf("[%s] opening balance %s set as of %s from the bank's %s balance (written=%v)",
		label, centsToDecimal(opening), date.Format("2006-01-02"), picked.Type, written)
	olog.Info(ctx, "opening_balance.set",
		logs.String("bank", label),
		logs.Int64("cents", opening),
		logs.String("date", date.Format("2006-01-02")),
		logs.String("balance_type", picked.Type),
		logs.Bool("written", written))
}

func (s *Syncer) claim(acct store.BankAccount) bool {
	ok, err := s.st.ClaimOpeningBalance(acct.ID)
	if err != nil {
		log.Printf("[%s] could not claim the opening balance: %v", bankLabel(acct), err)
		return false
	}
	return ok
}

func (s *Syncer) checkDrift(
	ctx context.Context,
	acct store.BankAccount,
	budgetAccountID string,
	picked enablebanking.Balance,
	fetched []enablebanking.Transaction,
	dropped int,
	writeFailed, interrupted bool,
	held int,
	reader budget.BalanceReader,
) {
	// Before the opening balance exists the difference is simply the money that
	// predates the window, which is not drift and would fire on every account.
	fresh, err := s.st.GetAllBankAccounts()
	if err == nil {
		for _, a := range fresh {
			if a.ID == acct.ID {
				acct = a
			}
		}
	}
	if acct.OpeningBalanceState != store.OpeningBalanceWritten {
		s.recordDrift(acct, 0, store.DriftNoOpening)
		return
	}
	// held belongs in this condition rather than in a rule of its own. The budget
	// is short by exactly the held amount, deliberately and for as long as the
	// decision is open, so a drift figure computed now measures the queue rather
	// than the account — and it would alert on every sync until somebody looked.
	if dropped > 0 || writeFailed || interrupted || held > 0 {
		s.recordDrift(acct, 0, store.DriftIncomplete)
		return
	}

	total, err := reader.AccountBalance(ctx, budgetAccountID)
	if err != nil {
		log.Printf("[%s] could not total the budget account: %v", bankLabel(acct), err)
		olog.Error(ctx, "drift.total_failed",
			logs.String("bank", bankLabel(acct)),
			logs.String("error", err.Error()))
		s.recordDrift(acct, 0, store.DriftUnsupported)
		return
	}

	drift := total - ExpectedTotal(picked, fetched)
	state := store.DriftOK
	if drift != 0 {
		state = store.DriftAlert
		log.Printf("[%s] balance drift %s: the budget shows %s, the bank %s (%s) implying %s",
			bankLabel(acct), centsToDecimal(drift), centsToDecimal(total),
			centsToDecimal(picked.AmountCents), picked.Type,
			centsToDecimal(ExpectedTotal(picked, fetched)))
		olog.Warn(ctx, "drift.detected",
			logs.String("bank", bankLabel(acct)),
			logs.Int64("drift_cents", drift),
			logs.Int64("budget_cents", total),
			logs.Int64("bank_cents", picked.AmountCents),
			logs.String("balance_type", picked.Type))
	}

	// The alert needs two consecutive runs. A pending settling between the
	// balance reading and the import shows up as drift for exactly one cycle,
	// and an alarm that cries wolf every few days stops being read.
	threshold := s.st.Tunables().DriftNotifyCents
	if ShouldNotifyDrift(acct, drift, threshold) {
		sendEmail(ctx,
			fmt.Sprintf("bankingsync: %s is off by %s", bankLabel(acct), centsToDecimal(drift)),
			fmt.Sprintf("The budget account for %s shows %s.\n"+
				"The bank reports %s (%s), which implies %s.\n\n"+
				"Difference: %s, unchanged since the previous sync.\n\n"+
				"bankingsync does not correct this by itself.",
				bankLabel(acct), centsToDecimal(total), centsToDecimal(picked.AmountCents),
				picked.Type, centsToDecimal(ExpectedTotal(picked, fetched)), centsToDecimal(drift)))
	}

	s.recordDrift(acct, drift, state)
}

// recordDrift persists the outcome of one comparison and counts it.
//
// The gauge alone cannot answer the question an operator has. It reports the last
// known state per account, so an account that came out "unstable" on three of the
// last ten runs and settled on the fourth looks exactly like one that has never
// had a problem. The counter is what makes that visible — and "unstable" or
// "incomplete" means the comparison in that run was worth nothing, which is the
// thing worth alerting on.
func (s *Syncer) recordDrift(acct store.BankAccount, cents int64, state string) {
	ctx := context.Background()
	if err := s.st.SetAccountDrift(acct.ID, cents, state); err != nil {
		log.Printf("[%s] could not record drift: %v", bankLabel(acct), err)
		olog.Error(ctx, "drift.record_failed",
			logs.String("bank", bankLabel(acct)),
			logs.String("error", err.Error()))
	}
	if s.met != nil {
		s.met.balanceChecks.Add(ctx, 1, metric.WithAttributes(
			attribute.String("backend", s.backendName),
			attribute.String("bank", bankLabel(acct)),
			attribute.String("state", state),
		))
	}
}

// deferReason names the condition that makes an opening balance unsafe to
// derive right now, or "" when it is safe. Every one of these would fold money
// into a figure that is written once and never revised.
func deferReason(
	dropped int,
	writeFailed, interrupted bool,
	held int,
	picked enablebanking.Balance,
	fetched []enablebanking.Transaction,
	accountCurrency string,
) string {
	if dropped > 0 {
		return fmt.Sprintf("%d transaction(s) could not be parsed and would vanish into the opening balance", dropped)
	}
	if writeFailed {
		return "not every transaction reached the backend"
	}
	if interrupted {
		return "the run was cut short before the window was complete"
	}
	// A held transaction is not in the budget, so the opening balance would
	// absorb it — and the opening balance is written once and never revised, so
	// the amount would stay absorbed after the decision was made. Deferring costs
	// a sync; getting it wrong costs the figure permanently.
	if held > 0 {
		return fmt.Sprintf("%d transaction(s) are waiting for a decision under /review and "+
			"would be swallowed by a balance that is never revised", held)
	}

	today := time.Now().UTC().Truncate(24 * time.Hour)
	for _, t := range fetched {
		if t.Date.After(today) {
			return fmt.Sprintf("a transaction is dated %s, after today", t.Date.Format("2006-01-02"))
		}
		if t.Currency != "" && picked.Currency != "" && !strings.EqualFold(t.Currency, picked.Currency) {
			return fmt.Sprintf("a transaction is denominated in %s but the balance in %s",
				t.Currency, picked.Currency)
		}
	}
	if accountCurrency != "" && picked.Currency != "" && !strings.EqualFold(accountCurrency, picked.Currency) {
		return fmt.Sprintf("the account is %s but the balance is %s", accountCurrency, picked.Currency)
	}
	return ""
}

// OpeningCents is the money the account held before the imported window.
//
// What gets subtracted follows what the chosen balance already contains. A
// booked balance holds only posted entries, so only booked transactions come
// off. An available balance already reflects card holds, so pending ones come
// off too — subtracting only the booked half would leave every outstanding
// authorisation counted twice.
//
// The cut at the balance's reference date matters just as much: a CLBD reported
// as of yesterday's close does not contain today's bookings, and subtracting
// them anyway makes the opening balance too low.
func OpeningCents(picked enablebanking.Balance, fetched []enablebanking.Transaction) int64 {
	cut := time.Now().UTC().Truncate(24 * time.Hour)
	if picked.HasReferenceDate() {
		cut = picked.ReferenceDate.UTC().Truncate(24 * time.Hour)
	}

	sum := int64(0)
	for _, t := range fetched {
		if t.Status == "PDNG" && !picked.IncludesPending() {
			continue
		}
		if t.Date.UTC().Truncate(24 * time.Hour).After(cut) {
			continue
		}
		sum += t.AmountCents
	}
	return picked.AmountCents - sum
}

// ExpectedTotal is what the budget account should add up to for a given bank
// balance: the balance plus the pending entries it does not already contain.
func ExpectedTotal(picked enablebanking.Balance, fetched []enablebanking.Transaction) int64 {
	if picked.IncludesPending() {
		return picked.AmountCents
	}
	return picked.AmountCents + PendingCents(fetched)
}

// PendingCents totals the transactions the bank has authorised but not booked.
// The budget holds them, a booked balance does not, so the difference is
// expected rather than drift.
func PendingCents(fetched []enablebanking.Transaction) int64 {
	sum := int64(0)
	for _, t := range fetched {
		if t.Status == "PDNG" {
			sum += t.AmountCents
		}
	}
	return sum
}

// OpeningDate is the day before the earliest money we imported, so the opening
// balance sorts ahead of every transaction it accounts for.
func OpeningDate(dateFrom time.Time, fetched []enablebanking.Transaction) time.Time {
	earliest := dateFrom.UTC().Truncate(24 * time.Hour)
	for _, t := range fetched {
		if d := t.Date.UTC().Truncate(24 * time.Hour); d.Before(earliest) {
			earliest = d
		}
	}
	return earliest.AddDate(0, 0, -1)
}

func sameBalance(picked enablebanking.Balance, after []enablebanking.Balance, currency string) bool {
	second, err := enablebanking.SelectBalance(after, currency)
	if err != nil {
		return false
	}
	if second.Type != picked.Type || second.AmountCents != picked.AmountCents ||
		second.Currency != picked.Currency {
		return false
	}
	// last_committed_transaction is a stronger equality than the amount when the
	// ASPSP provides it: two offsetting entries leave the amount unchanged.
	return second.LastCommitted == picked.LastCommitted
}

func bankLabel(acct store.BankAccount) string {
	if acct.BankName != "" {
		return acct.BankName
	}
	return acct.AccountUID
}

// matchPolicy assembles the operator-tunable matching rules for one account.
//
// Two fitted parameter sets can be in play and they do different things. A
// promoted set replaces the shipped level tables and decides; a watched set
// decides nothing and is evaluated beside the real decision so that a person can
// see what promoting it would have changed. Most installations have neither.
func (s *Syncer) matchPolicy(label string) budget.Policy {
	t := s.st.Tunables()
	promoted := s.promotedTrial()
	pol := budget.Policy{
		PayeePrefixes:    t.PayeePrefixes,
		TolerancePercent: t.TolerancePercent,
		ToleranceCents:   t.ToleranceCents,
		// The classifier is injected rather than imported: budget/ holds the
		// rules and keeps to the standard library, payeematch holds the string
		// work and the dependency that comes with it.
		Compare: payeematch.New(t.PayeePrefixes),
		// The thresholds are stored as percentages, which is the unit the
		// operator sets them in; the model works in probabilities.
		AutoProbability:   float64(t.AutoProbabilityPct) / 100,
		ReviewProbability: float64(t.ReviewProbabilityPct) / 100,
		// How often a transaction reaching the matcher has a counterpart at all,
		// which is what the candidate-count prior is taken over. Same unit
		// conversion and the same reason for it.
		Overlap: float64(t.OverlapPct) / 100,
		// The middle band only does something because there is somewhere to put
		// what lands in it.
		HoldForReview: true,
		OnNearMiss: func(reason string, c *budget.Transaction) {
			s.nearMiss(label, reason, c)
		},
	}
	pol.Linkage = s.linkageInForce()
	if !promoted.IsZero() {
		pol.Calibration = promoted.Calibration
	}
	if watched := s.shadowTrial(); !watched.IsZero() {
		pol.Trial = &watched
	}
	return pol
}

func (s *Syncer) nearMiss(label, reason string, c *budget.Transaction) {
	switch {
	case c != nil:
		log.Printf("[%s] near miss (%s): creating a new transaction although an open row "+
			"of %s dated %s was close", label, reason,
			centsToDecimal(c.AmountCents), c.Date.Format("2006-01-02"))
	default:
		log.Printf("[%s] near miss (%s): more than one open row was close, so none was adopted",
			label, reason)
	}
	olog.Warn(context.Background(), "match.near_miss",
		logs.String("bank", label),
		logs.String("reason", reason),
		logs.Bool("single_candidate", c != nil))
	if s.met != nil && s.met.nearMiss != nil {
		s.met.nearMiss.Add(context.Background(), 1,
			metric.WithAttributes(
				attribute.String("reason", reason),
				attribute.String("bank", label),
				attribute.String("backend", s.backendName),
			))
	}
}

// ShouldNotifyDrift reports whether an email is warranted.
//
// It requires the drift to exceed the threshold in this run AND in the one
// before. A pending settling between the balance reading and the import shows up
// as drift for exactly one cycle, and an alarm that fires on that is an alarm
// nobody reads by the third time.
func ShouldNotifyDrift(previous store.BankAccount, drift, threshold int64) bool {
	if threshold <= 0 || abs64(drift) <= threshold {
		return false
	}
	return previous.DriftState == store.DriftAlert && abs64(previous.DriftCents) > threshold
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// OpeningBalancePreview computes, and optionally applies, the opening balance
// for one account on demand. It is what the web UI button calls.
//
// expected guards against a stale confirmation page: the figure is recomputed
// from a fresh balance reading, and a value that moved since the page was
// rendered re-prompts instead of writing.
func (s *Syncer) OpeningBalancePreview(
	ctx context.Context, accountID int64, apply bool, expected *int64,
) (web.OpeningBalancePreview, error) {
	accounts, err := s.st.GetAllBankAccounts()
	if err != nil {
		return web.OpeningBalancePreview{}, err
	}
	var acct store.BankAccount
	found := false
	for _, a := range accounts {
		if a.ID == accountID {
			acct, found = a, true
		}
	}
	if !found {
		return web.OpeningBalancePreview{}, fmt.Errorf("no such account")
	}

	out := web.OpeningBalancePreview{
		AccountID:     acct.ID,
		BankName:      bankLabel(acct),
		BudgetAccount: acct.ActualAccount,
		Currency:      acct.Currency,
	}
	if acct.OpeningBalanceState == store.OpeningBalanceWritten {
		out.Refusal = "This account already has an opening balance."
		return out, nil
	}

	if !s.tryAcquire() {
		return out, fmt.Errorf("a sync is running — try again in a moment")
	}
	defer s.release()

	if s.ac == nil {
		store, err := s.connectBackend(ctx)
		if err != nil {
			return out, fmt.Errorf("connect to the budget backend: %w", err)
		}
		s.ac = store
	}
	writer, ok := s.ac.(budget.OpeningBalanceWriter)
	if !ok {
		out.Refusal = "This budget backend cannot record an opening balance."
		return out, nil
	}

	balances, err := s.eb.FetchBalances(ctx, acct.AccountUID)
	if err != nil {
		out.Refusal = "The bank did not return balances: " + err.Error()
		return out, nil
	}
	picked, err := enablebanking.SelectBalance(balances, acct.Currency)
	if err != nil {
		out.Refusal = err.Error()
		return out, nil
	}

	dateFrom := time.Now().UTC().AddDate(0, 0, -30)
	if d, err := time.Parse("2006-01-02", acct.LastSyncDate); err == nil {
		dateFrom = d
	}
	fetched, dropped, err := s.eb.FetchTransactions(ctx, acct.AccountUID, dateFrom)
	if err != nil {
		return out, fmt.Errorf("fetch transactions: %w", err)
	}
	openReviews, err := s.st.CountMatchReviewsByAccount()
	if err != nil {
		return out, err
	}
	if reason := deferReason(dropped, false, false,
		openReviews[acct.ID], picked, fetched, acct.Currency); reason != "" {
		out.Refusal = reason
		return out, nil
	}

	opening := OpeningCents(picked, fetched)
	date := OpeningDate(dateFrom, fetched)

	out.BalanceType = picked.Type
	out.AvailableBalance = picked.IncludesPending()
	out.BankCents = picked.AmountCents
	out.OpeningCents = opening
	out.ImportedCents = picked.AmountCents - opening
	out.OpeningDate = date.Format("2006-01-02")
	if picked.HasReferenceDate() {
		out.ReferenceDate = picked.ReferenceDate.Format("2006-01-02")
	}
	if !apply {
		return out, nil
	}

	if expected != nil && *expected != opening {
		out.Refusal = fmt.Sprintf(
			"The balance moved while this page was open: it is now %s, not %s. "+
				"Check the new figure and confirm again.",
			centsToDecimal(opening), centsToDecimal(*expected))
		return out, nil
	}
	if !s.claim(acct) {
		out.Refusal = "This account is already being written, or it has an opening balance."
		return out, nil
	}

	budgetAccount, err := s.ac.GetOrCreateAccount(ctx, budget.AccountSpec{
		Name: budgetAccountName(acct, s.backendName), Currency: acct.Currency, IBAN: acct.IBAN,
	})
	if err != nil {
		_ = s.st.SetOpeningBalanceState(acct.ID, acct.OpeningBalanceState)
		return out, fmt.Errorf("resolve the budget account: %w", err)
	}

	if _, err := writer.SetOpeningBalance(ctx, budgetAccount.ID, budget.OpeningBalance{
		AmountCents: opening, Currency: picked.Currency, Date: date,
		Ref: openingRef(acct), PayeeName: openingPayee,
	}); err != nil {
		_ = s.st.SetOpeningBalanceState(acct.ID, acct.OpeningBalanceState)
		return out, fmt.Errorf("write the opening balance: %w", err)
	}
	if err := s.st.SetOpeningBalance(acct.ID, opening, date.Format("2006-01-02"), openingRef(acct)); err != nil {
		return out, fmt.Errorf("record the opening balance: %w", err)
	}
	out.Applied = true
	return out, nil
}

func budgetAccountName(acct store.BankAccount, backendName string) string {
	if acct.ActualAccount != "" {
		return acct.ActualAccount
	}
	env := "ACTUAL_ACCOUNT"
	if backendName == backendFirefly {
		env = "FIREFLY_ACCOUNT"
	}
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v
	}
	return bankLabel(acct)
}

// holdForReview parks a transaction the matcher would not decide on.
//
// Nothing is written to the budget: the point of the middle band is that both
// available guesses are worse than asking. What is recorded is the whole import,
// because the bank feed will have moved on by the time somebody looks, plus the
// evidence that produced the doubt — which is also the observation an estimate
// of the model's own parameters would later be built from.
func (s *Syncer) holdForReview(
	ctx context.Context,
	acct store.BankAccount,
	pendingKey string,
	in budget.ImportedFields,
	out budget.Outcome,
	pol budget.Policy,
) {
	candidates := out.Held
	best := candidates[0]
	// Why it was held, not only that it was. Under the shipped configuration the
	// ambiguous case no longer reaches bankingsync_near_miss_total at all — it is
	// held instead of created — so this is the only place that distinction still
	// surfaces, and it is the one that says whether a threshold would help.
	reason := pol.HoldReason(out.Margin)
	r := store.MatchReview{
		BankAccountID:    acct.ID,
		Backend:          s.backendName,
		ExternalRef:      in.ExternalRef,
		PendingKey:       pendingKey,
		TxnDate:          in.Date.Format("2006-01-02"),
		AmountCents:      in.AmountCents,
		Currency:         in.Currency,
		Payee:            in.PayeeName,
		Notes:            in.Notes,
		ImportedPayee:    in.ImportedPayee,
		CounterpartyIBAN: in.CounterpartyIBAN,
		SEPAEndToEnd:     in.SEPA.EndToEnd,
		SEPAMandate:      in.SEPA.Mandate,
		SEPACreditorID:   in.SEPA.CreditorID,
		Cleared:          in.Cleared,
		BestProbability:  best.Probability,
		BestPayeeLevel:   best.Comparison.Payee.String(),
		BestAmountLevel:  best.Comparison.Amount.String(),
		BestDateLevel:    best.Comparison.Date.String(),
	}
	if err := s.st.AddMatchReview(r); err != nil {
		// Recording it failed, so nothing is holding the transaction back and the
		// next sync will offer it again. That is the safe direction — a duplicate
		// is visible — but it has to be said out loud.
		log.Printf("[%s] could not hold %s for review: %v", bankLabel(acct), in.ExternalRef, err)
		olog.Error(ctx, "match.hold_failed",
			logs.String("bank", bankLabel(acct)),
			logs.String("error", err.Error()))
		return
	}
	s.state.AddHeldKey(acct.ID, pendingKey)
	if s.met != nil {
		s.met.matchReviews.Add(ctx, 1, metric.WithAttributes(
			attribute.String("outcome", "queued"),
			attribute.String("reason", reason),
			attribute.String("bank", bankLabel(acct)),
			attribute.String("backend", s.backendName)))
	}

	log.Printf("[%s] holding %s for review (%s): best candidate %.0f%% (payee %s, amount %s, date %s)",
		bankLabel(acct), in.PayeeName, reason, 100*best.Probability,
		r.BestPayeeLevel, r.BestAmountLevel, r.BestDateLevel)
	olog.Warn(ctx, "match.held_for_review",
		logs.String("bank", bankLabel(acct)),
		logs.String("reason", reason),
		logs.Int("candidates", len(candidates)),
		logs.Float64("probability", best.Probability),
		logs.String("payee_level", r.BestPayeeLevel),
		logs.String("amount_level", r.BestAmountLevel),
		logs.String("date_level", r.BestDateLevel))
}
