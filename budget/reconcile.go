package budget

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"
)

// Reconcile places one incoming bank transaction: it either enriches a row the
// backend already holds or creates a new one.
//
// It is a batch of one. The batch is the real operation — a transaction cannot be
// placed sensibly without knowing what else is competing for the same rows — and
// keeping a single implementation is what stops the two paths drifting apart.
//
// claimed carries rows already spoken for outside this batch, which the caller
// settled by other means.
func Reconcile(
	ctx context.Context,
	s Store,
	accountID string,
	in ImportedFields,
	claimed []*Transaction,
	pol Policy,
) (Outcome, error) {
	out, err := ReconcileBatch(ctx, s, accountID, []ImportedFields{in}, claimed, pol)
	if err != nil {
		return Outcome{}, err
	}
	return out[0], nil
}

// ReconcileBatch places a whole batch of incoming transactions against the rows
// the backend holds, deciding all of them together.
//
// Together is the point. Weighed one at a time, two bookings can both claim the
// same authorisation and the tie is broken by whichever the loop reached first —
// so the same statement in a different order produced a different budget, and two
// identical payments went to a person because neither pairing could be told from
// the other. Under a one-to-one constraint the arrangement of the whole batch is
// what is chosen, and both of those go away.
//
// The hard rules run first and are not part of the arrangement: a bank reference
// is an identity rather than evidence, and a row it names is settled before
// anything is weighed.
// BatchTrace is what one batch cost, split by the three things a batch does.
//
// The split is the point. Assessing reads every window from the budget backend
// and compares every row in it, so it carries a network call and grows with the
// account's traffic; arranging is a Hungarian assignment over the batch, so it
// grows with the batch; shadowing does the whole of the first two again against
// a candidate parameter set, and only when one is being watched. An import that
// has become slow is one of those three and the totals say which.
type BatchTrace struct {
	// Incoming is how many transactions the model was asked about, which is
	// fewer than the batch when a bank reference settled some of them first.
	Incoming int

	// Weighed is how many candidate rows were compared in total, across every
	// window. This is the work, and it is the number that grows with an
	// account's history rather than with the feed.
	Weighed int

	Assessed time.Duration
	Arranged time.Duration
	Shadowed time.Duration
}

// DecisionTrace is what the matcher concluded about one incoming transaction and
// what it concluded it from.
//
// Everything here is already in the Outcome or recoverable from the parameters;
// it is gathered in one place because the question asked of a trace is "why was
// this held" and answering it from the pieces means knowing which pieces to ask
// for.
type DecisionTrace struct {
	// Index is the position in the ImportedFields slice this batch was given, so
	// a record can be tied back to the transaction it is about.
	Index int

	// Window is how many rows the fortnight held; Adoptable how many were still
	// eligible once the hard rules had run; Plausible how many the prior was
	// taken over. The three differ, and where they differ is often the answer.
	Window    int
	Adoptable int
	Plausible int

	// Outcome is "adopted", "held" or "created". Reason is the rule that settled
	// it, in words, because "held" alone does not distinguish a case that sat in
	// the review band from one that cleared the automatic threshold and was held
	// anyway for want of a clear alternative.
	Outcome string
	Reason  string

	// Best is the strongest candidate considered, nil when the window was empty.
	// RunnerUp is the second strongest, nil when there was only one — and the
	// difference between their weights is what Margin describes, except that
	// Margin is the arrangement's answer rather than this window's.
	Best     *Candidate
	RunnerUp *Candidate

	Margin          float64
	Interchangeable int

	// Evidence is the weight of Best, term by term: the prior, the payee with its
	// frequency correction folded in, the amount and the date. It sums to
	// Best.Weight.
	Evidence []FieldEvidence

	// AutoProbability, ReviewProbability and MarginBits are the thresholds this
	// decision was judged against, carried so that a record made a month ago can
	// still be read against the policy that produced it.
	AutoProbability   float64
	ReviewProbability float64
	MarginBits        float64

	// ChosenID is the row the transaction was merged into, empty otherwise.
	ChosenID string

	// ShadowOutcome is what a watched candidate parameter set would have done,
	// empty when none is being watched.
	ShadowOutcome string
}

func ReconcileBatch(
	ctx context.Context,
	s Store,
	accountID string,
	in []ImportedFields,
	claimed []*Transaction,
	pol Policy,
) ([]Outcome, error) {
	out := make([]Outcome, len(in))
	taken := append([]*Transaction(nil), claimed...)

	model := make([]int, 0, len(in))
	for i, f := range in {
		if f.ExternalRef != "" {
			t, err := s.FindByExternalRef(ctx, accountID, f.ExternalRef)
			if err != nil {
				return nil, err
			}
			if t != nil {
				if err := applyImported(ctx, s, t, f, pol); err != nil {
					return nil, err
				}
				out[i] = Outcome{Transaction: t}
				// Settled by its reference, so it is no longer on offer to the
				// arrangement.
				taken = append(taken, t)
				continue
			}
		}
		model = append(model, i)
	}

	// The three phases are timed apart because they scale with different things
	// and an import that has become slow is one of them. See BatchTrace.
	var trace BatchTrace
	trace.Incoming = len(model)

	windows := make([]int, len(model))
	rows := make([][]Candidate, len(model))
	assessStarted := time.Now()
	for k, i := range model {
		from, to := WindowBounds(in[i].Date)
		candidates, err := s.ListTransactions(ctx, accountID, from, to)
		if err != nil {
			return nil, err
		}
		windows[k] = len(candidates)
		rows[k] = Assess(candidates, in[i], taken, pol)
		trace.Weighed += len(rows[k])
	}
	trace.Assessed = time.Since(assessStarted)

	arrangeStarted := time.Now()
	assignments := Solve(rows, pol.noMatchWeight())
	trace.Arranged = time.Since(arrangeStarted)

	shadowStarted := time.Now()
	shadows := shadowPass(rows, pol)
	trace.Shadowed = time.Since(shadowStarted)

	if pol.OnBatch != nil {
		pol.OnBatch(trace)
	}

	for k, i := range model {
		// A batch is placed one transaction at a time, and a run that has used up
		// its time stops here rather than at the next fetch. What has been written
		// is durable; what has not leaves its outcome zero, which is how the
		// caller tells "nothing happened to this one" from "this one was decided".
		if ctx.Err() != nil {
			break
		}

		scored := rows[k]
		var strongest *Candidate
		if len(scored) > 0 {
			strongest = &scored[0]
		}
		var shadow *ShadowOutcome
		if shadows != nil {
			shadow = &shadows[k]
		}

		chosen, hold, reason := pol.decideAssigned(assignments[k])
		switch {
		case chosen != nil && !hold:
			if err := applyImported(ctx, s, chosen.Transaction, in[i], pol); err != nil {
				return nil, err
			}
			out[i] = Outcome{Transaction: chosen.Transaction, Best: strongest, Shadow: shadow,
				Unchosen:        unchosen(scored, chosen.Transaction.ID),
				Margin:          assignments[k].Margin,
				Interchangeable: assignments[k].Interchangeable}

		case chosen != nil && pol.HoldForReview:
			// Something fits well enough to be worth a second opinion but not well
			// enough to act on. Importing it would put a duplicate in the budget
			// that somebody then has to find; adopting it might overwrite an
			// unrelated authorisation, which nobody would find at all.
			//
			// Every candidate here goes into the non-match sample, the best one
			// included, and which candidates are left out is the whole of the
			// point.
			//
			// Held windows used to contribute nothing at all. That is not a
			// neutral omission: a window is held precisely because it contains a
			// strong-looking pair, so dropping them truncates the sample on the
			// window's own maximum, and the truncation runs one way. It makes the
			// levels that agree look rarer among non-matches than they are, which
			// makes agreement worth more than it is — the self-confirming
			// direction.
			//
			// Measured over two hundred thousand windows generated under this
			// model's own assumptions, on the strongest comparison there is —
			// exact payee, exact amount, same day — against the true within-window
			// non-match rates:
			//
			//	held windows dropped, best excluded everywhere   +0.9681 bits
			//	held windows' runners-up recorded                +0.9451 bits
			//	held windows recorded entire                     +0.0386 bits
			//	every window recorded entire                     −3.1309 bits
			//
			// So recording the runners-up and keeping the exclusion — which is
			// what the exclusion in the adopted branch would suggest by symmetry —
			// buys almost nothing. What does the work is dropping the exclusion,
			// and the reason is that the exclusion has no warrant here. In the
			// adopted branch the model has decided that row is the counterpart, so
			// calling it a non-match would be recording the opposite of what it
			// just concluded. Here it has decided nothing. Leaving the best
			// candidate out would presume exactly the thing that is being put to a
			// person.
			//
			// It is not free: a held window often does contain the true
			// counterpart, and including it pushes the agreeing levels up. The
			// last row above is what that looks like unchecked — recording every
			// window entire overshoots by three bits in the other direction,
			// because adopted windows contain a true match nearly always. Here the
			// contamination is far smaller than the selection effect it removes.
			out[i] = Outcome{Held: scored, Best: strongest, Shadow: shadow,
				Unchosen:        unchosen(scored, ""),
				Margin:          assignments[k].Margin,
				Interchangeable: assignments[k].Interchangeable}

		default:
			pol.reportNearMiss(scored, assignments[k].Margin)
			created, err := s.Create(ctx, accountID, in[i])
			if err != nil {
				return nil, err
			}
			out[i] = Outcome{Transaction: created, Created: true, Best: strongest, Shadow: shadow,
				Unchosen: unchosen(scored, ""),
				Margin:   assignments[k].Margin}
		}

		if pol.OnDecision != nil {
			pol.OnDecision(pol.traceDecision(i, windows[k], scored, out[i], reason))
		}
	}
	return out, nil
}

// traceDecision assembles the record of one decision.
//
// Split out because it is bookkeeping rather than deciding, and because it must
// be able to run after any of the three branches without any of them having to
// remember to feed it.
func (p Policy) traceDecision(index, window int, scored []Candidate, out Outcome, reason string) DecisionTrace {
	t := DecisionTrace{
		Index:             index,
		Window:            window,
		Adoptable:         len(scored),
		Outcome:           out.Name(),
		Reason:            reason,
		Margin:            out.Margin,
		Interchangeable:   out.Interchangeable,
		AutoProbability:   p.autoProbability(),
		ReviewProbability: p.reviewProbability(),
		MarginBits:        p.marginBits(),
	}
	if out.Shadow != nil {
		t.ShadowOutcome = out.Shadow.Outcome
	}
	if out.Adopted() && out.Transaction != nil {
		t.ChosenID = out.Transaction.ID
	}
	if len(scored) > 0 {
		t.Best = &scored[0]
		t.Plausible = scored[0].Plausible
		// The weight term by term. Recomputed rather than stored on the candidate
		// because it is wanted only when somebody is watching, and carrying four
		// more floats on every candidate of every window to serve that would be
		// the wrong trade.
		t.Evidence = p.linkage().Explain(scored[0].Comparison, scored[0].Plausible, p.overlap())
	}
	if len(scored) > 1 {
		t.RunnerUp = &scored[1]
	}
	return t
}

// Name is what became of a transaction, in the words the rest of the program
// uses for it: "adopted", "held", "created", or "unsettled" when a batch ran out
// of time before reaching it.
//
// Here rather than at the call sites because there were two copies of this
// switch and a fourth case only one of them had.
func (o Outcome) Name() string {
	switch {
	case len(o.Held) > 0:
		return "held"
	case o.Created:
		return "created"
	case o.Transaction != nil:
		return "adopted"
	default:
		return "unsettled"
	}
}

// unchosen returns the comparisons of every candidate other than the one that
// was paired with, which is the sample u is estimated from.
//
// An empty chosenID means nothing was paired at all, and then every candidate in
// the window is a non-match.
func unchosen(scored []Candidate, chosenID string) []Comparison {
	if len(scored) == 0 {
		return nil
	}
	out := make([]Comparison, 0, len(scored))
	for i := range scored {
		if scored[i].Transaction != nil && scored[i].Transaction.ID == chosenID {
			continue
		}
		out = append(out, scored[i].Comparison)
	}
	return out
}

// shadowPass works out what the trial parameters would have made of the same
// batch, or returns nil when there is no trial.
//
// The whole batch, not one transaction at a time. A parameter change moves
// weights, a moved weight moves the arrangement, and a moved arrangement changes
// which transaction gets which row — so comparing pair by pair would report
// agreement on batches the two sets arrange completely differently.
//
// Nothing here touches the store. A shadow that could write would not be one.
func shadowPass(rows [][]Candidate, pol Policy) []ShadowOutcome {
	if pol.Trial == nil {
		return nil
	}
	trial := pol.Trial.apply(pol)
	// A trial of its own trial is a loop nobody asked for.
	trial.Trial = nil

	rescored := make([][]Candidate, len(rows))
	for k, row := range rows {
		rescored[k] = rescore(row, trial)
	}
	assignments := Solve(rescored, trial.noMatchWeight())

	out := make([]ShadowOutcome, len(rows))
	for k := range rows {
		chosen, hold, _ := trial.decideAssigned(assignments[k])
		switch {
		case chosen != nil && !hold:
			out[k] = ShadowOutcome{Outcome: "adopted", CandidateID: chosen.Transaction.ID,
				Probability: chosen.Probability, Margin: assignments[k].Margin}
		case chosen != nil && trial.HoldForReview:
			out[k] = ShadowOutcome{Outcome: "held",
				Probability: chosen.Probability, Margin: assignments[k].Margin}
		default:
			out[k] = ShadowOutcome{Outcome: "created", Margin: assignments[k].Margin}
			if chosen != nil {
				out[k].Probability = chosen.Probability
			}
		}
	}
	return out
}

// rescore recomputes weights and probabilities for candidates already compared,
// under a different parameter set.
//
// The comparison levels are reused rather than derived again, and deliberately:
// the levels are what the tolerance and the payee classifier produced, neither
// of which a trial is allowed to change. Reclassifying here would let a shadow
// report on a change that could never be promoted.
func rescore(row []Candidate, pol Policy) []Candidate {
	out := make([]Candidate, len(row))
	copy(out, row)
	for i := range out {
		out[i].Weight = pol.linkage().Weight(out[i].Comparison, out[i].Plausible, pol.overlap())
		out[i].Probability = pol.calibration().Probability(out[i].Weight)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Weight > out[j].Weight })
	return out
}

// HoldReason names why a candidate was put to a person rather than acted on.
//
// The two are different failures of confidence and want different answers. A
// margin case means two rows fit equally well and no threshold can separate
// them; an uncertain one means the best row is plausible but not convincing, and
// moving a threshold would change it.
func (p Policy) HoldReason(margin float64) string {
	if margin < p.marginBits() {
		return "ambiguous"
	}
	return "uncertain"
}

// Done reports whether anything at all happened to this transaction. A zero
// outcome comes back when a batch ran out of time before reaching it.
func (o Outcome) Done() bool { return o.Transaction != nil || len(o.Held) > 0 }

// Outcome is what became of one incoming bank transaction.
//
// Three results, not two: adopted onto an existing row, created as new, or held
// because the evidence landed between the two thresholds. The third is the
// "clerical review" zone the Fellegi-Sunter model has always had, and it is
// spelled out as its own field rather than encoded as a nil transaction — a
// caller that ignores it would otherwise silently drop the transaction.
type Outcome struct {
	// Transaction is the row that was adopted or created. Nil when held.
	Transaction *Transaction

	// Created reports that Transaction is new rather than adopted.
	Created bool

	// Held carries the candidates, best first, when nothing was written and the
	// decision is a person's. Non-empty implies Transaction is nil.
	Held []Candidate

	// Interchangeable is how many candidates the chosen one could not be told
	// from. More than one means the arrangement had a free choice.
	Interchangeable int

	// Margin is how much total evidence the batch would lose if this pairing were
	// forbidden — the arrangement's answer to "was there a real alternative".
	// Infinite when nothing was paired.
	Margin float64

	// Best is the strongest candidate the window offered, whatever was decided —
	// nil only when nothing was weighed at all, which is the case for an empty
	// window and for the reference fast path that skips weighing entirely.
	//
	// It is carried out of the decision so the caller can record the figure the
	// decision was made on. Without it the two thresholds are settings a person
	// is asked to choose with no view of the distribution they cut through.
	Best *Candidate

	// Unchosen carries the comparison levels of the candidates this transaction
	// was weighed against and not paired with.
	//
	// They are a sample of the population u describes: pairs that sit in one
	// window and are not the same payment. At most one candidate in a window can
	// be the match, so where a match was adopted, removing the row it was adopted
	// into leaves rows that are non-matches except where the decision itself was
	// wrong — a contamination on the order of the error rate rather than of the
	// base rate, measured at about half a per cent.
	//
	// That arithmetic is right and it used to be the whole of this comment, which
	// made it misleading. Contamination pushes u up on the agreeing levels; the
	// selection that produces the sample pushes it down, and the second is an
	// order of magnitude larger than the first. Against the true within-window
	// non-match rates over two hundred thousand simulated windows, an exact payee
	// with an exact amount on the same day carried +0.97 bits of evidence nothing
	// had observed, all of it from what the sampling leaves out.
	//
	// So the exclusion is now made only where the model has a claim to make it:
	// where it adopted the row. A held window contributes every candidate, the
	// best one included, and a created window always did. That takes the same
	// measurement to +0.04 bits. See the held branch in ReconcileBatch.
	Unchosen []Comparison

	// Shadow is what the trial parameter set would have done with this
	// transaction, when one is being evaluated. Nil otherwise, and never acted
	// on: the whole point of a shadow is that it is a measurement of a change
	// nobody has agreed to yet.
	Shadow *ShadowOutcome
}

// ShadowOutcome is one transaction's fate under a candidate parameter set.
type ShadowOutcome struct {
	// Outcome is "adopted", "held" or "created".
	Outcome string

	// CandidateID is the row it would have been merged into, empty otherwise.
	// It is part of the comparison: two parameter sets that both adopt, but
	// adopt different rows, have not agreed.
	CandidateID string

	Probability float64
	Margin      float64
}

// Differs reports whether the trial would have done something else with this
// transaction.
func (o Outcome) Differs() bool {
	if o.Shadow == nil {
		return false
	}
	live, id := o.liveOutcome()
	return o.Shadow.Outcome != live || o.Shadow.CandidateID != id
}

// liveOutcome names what actually happened, in the shadow's vocabulary.
func (o Outcome) liveOutcome() (outcome, candidateID string) {
	switch {
	case len(o.Held) > 0:
		return "held", ""
	case o.Created:
		return "created", ""
	case o.Transaction != nil:
		return "adopted", o.Transaction.ID
	default:
		return "", ""
	}
}

// Adopted reports that an existing row was merged into rather than a new one made.
func (o Outcome) Adopted() bool { return o.Transaction != nil && !o.Created }

// Policy carries the operator-tunable parts of matching. It travels as a value
// so the two backends provably see the same rules.
type Policy struct {
	PayeePrefixes    []string
	TolerancePercent int
	ToleranceCents   int64

	// Compare classifies how two payee spellings relate. Injected rather than
	// imported so this package keeps to the standard library: the classifier
	// needs a string metric, the rules do not.
	//
	// A nil Compare falls back to equality after normalisation, which is what the
	// tests in this package use — they exercise the rules, not the classifier.
	Compare func(a, b string) PayeeLevel

	// Linkage carries the m and u probabilities. A zero value means the shipped
	// parameters.
	Linkage Linkage

	// Calibration rescales a match weight before it becomes a probability. The
	// zero value means the identity, which is what ships: until an installation
	// has labelled decisions of its own there is nothing to fit, and a fitted
	// correction is the one part of this model that cannot be argued from first
	// principles.
	Calibration Calibration

	// PayeeFrequency reports what share of this account's traffic a normalised
	// payee holds, floored by the caller. Nil means unmeasured, and the model
	// then applies no frequency correction at all — which is the right answer
	// for an account with too little history to have a distribution.
	PayeeFrequency func(normalised string) float64

	// AutoProbability is the confidence at or above which a candidate is adopted
	// without asking, ReviewProbability the confidence below which it is not a
	// candidate at all. Between them is the band a person decides — the "clerical
	// review" zone the Fellegi-Sunter model has had since 1969. Zero means the
	// shipped defaults.
	AutoProbability   float64
	ReviewProbability float64

	// HoldForReview turns the middle band on: a candidate good enough to be worth
	// asking about, but not good enough to act on, stops the import instead of
	// letting it through as new. Off by default so that a caller which has
	// nowhere to put a held transaction cannot lose one.
	HoldForReview bool

	// MarginBits is how far the best candidate must beat the runner-up before it
	// is adopted without asking. One bit is a factor of two in the odds.
	//
	// This is not part of the per-pair model and cannot be: it is a property of
	// the candidate set. Two rows that fit equally well cannot both be the
	// settlement, and picking either would be a coin toss dressed up as a
	// decision. Zero means the shipped default.
	MarginBits float64

	// OnBatch and OnDecision, when set, are handed a record of what the matcher
	// did — the first once per batch, the second once per incoming transaction.
	//
	// Callbacks rather than a tracing library, for the reason the payee
	// classifier is injected rather than imported: this package holds the rules
	// and keeps to the standard library, and an exporter is not a rule. What they
	// carry is chosen so that a decision can be explained rather than only
	// stated — the evidence term by term, the thresholds it was judged against,
	// and the runner-up it was judged clear of.
	//
	// They are called after the decision is made and must not change it. Nothing
	// here reads a return value, and a panic in one is a panic in the sync.
	OnBatch    func(BatchTrace)
	OnDecision func(DecisionTrace)

	// Overlap is the share of transactions reaching the matcher that have a
	// counterpart in their window at all — the pi of the candidate-count prior.
	// It is a claim about the institution rather than an estimate, which is why
	// it lives here with the thresholds and not in the fitted parameters. Zero
	// means unset.
	Overlap float64

	// Trial is a candidate parameter set to evaluate alongside the one in force,
	// without acting on it. Nil means no shadow evaluation, which is the default
	// and what every installation runs until somebody has parameters to try.
	//
	// It is evaluated over the same candidate rows, so a shadow costs no extra
	// reads of the budget backend — which is the reason it lives here rather
	// than being a second pass a caller could make.
	Trial *Trial

	// OnNearMiss is called just before a transaction is created although
	// something in the window nearly matched it. It exists because the banks
	// that trigger this cannot be reproduced here; the counters are how the
	// affected user tells us which mechanism actually fires.
	OnNearMiss func(reason string, candidate *Transaction)
}

// withinTolerance reports whether booked may be treated as the settled form of
// authorised. The absolute cap is the important half: 25% of a 4.000 payment
// would otherwise be 1.000 of slack.
func (p Policy) withinTolerance(authorised, booked int64) bool {
	if p.TolerancePercent <= 0 || p.ToleranceCents <= 0 {
		return false
	}
	diff := booked - authorised
	if diff < 0 {
		diff = -diff
	}
	// The percentage is taken against the larger of the two, so the rule reads
	// the same whether the booking came in above or below the authorisation.
	base := maxInt64(abs64(booked), abs64(authorised))
	limit := base * int64(p.TolerancePercent) / 100
	if limit > p.ToleranceCents {
		limit = p.ToleranceCents
	}
	return diff <= limit
}

// reportNearMiss names why a transaction was created although something in the
// window was close, so the mechanism that fired can be counted rather than
// guessed at.
func (p Policy) reportNearMiss(scored []Candidate, margin float64) {
	if p.OnNearMiss == nil || len(scored) == 0 {
		return
	}
	top := scored[0]
	if !top.Comparison.Amount.Within() && top.Comparison.Amount != AmountExact {
		// Nothing was close. The counter is for the cases where a merge was
		// plausible and did not happen, not for every unrelated row in the window.
		return
	}
	switch {
	case margin < p.marginBits():
		p.OnNearMiss("ambiguous", nil)
	case top.Comparison.Payee == PayeeConflict || top.Comparison.Payee == PayeeNone ||
		top.Comparison.Payee == PayeeMissing:
		p.OnNearMiss("payee", top.Transaction)
	case top.Comparison.Amount != AmountExact:
		p.OnNearMiss("amount", top.Transaction)
	default:
		p.OnNearMiss("date", top.Transaction)
	}
}

func adoptable(c *Transaction, ref string) bool {
	return !c.Cleared || c.ExternalRef == "" || c.ExternalRef == ref
}

// MergePatch computes the fields an incoming bank transaction may write onto an
// existing one. It never overwrites what a user changed: notes and the imported
// payee are only filled when empty, the payee is left alone once renamed, and a
// reconciled transaction is not touched at all.
func MergePatch(t *Transaction, in ImportedFields, prefixes []string) Patch {
	var p Patch
	if t.Reconciled {
		return p
	}

	if in.ExternalRef != "" && t.ExternalRef != in.ExternalRef {
		p.ExternalRef = String(in.ExternalRef)
	}
	if in.Notes != "" && t.Notes == "" {
		p.Notes = String(in.Notes)
	}
	if in.Cleared && !t.Cleared {
		p.Cleared = Bool(true)
	}

	payee := TitleCase(in.PayeeName)
	if payee != "" && payee != t.PayeeName && !PayeeWasRenamedByUser(t) {
		p.PayeeName = String(payee)
	}
	// ImportedPayee also has to follow the booked spelling, not only fill an
	// empty field. Leaving "Hotel Berlin" behind while PayeeName becomes
	// "VISA Hotel Berlin" makes PayeeWasRenamedByUser true forever, and the
	// importer then treats its own row as user-edited and stops maintaining it.
	if in.ImportedPayee != "" && (t.ImportedPayee == "" || samePayee(t.ImportedPayee, in.ImportedPayee, prefixes)) &&
		t.ImportedPayee != in.ImportedPayee {
		p.ImportedPayee = String(in.ImportedPayee)
	}
	return p
}

// AmountPatch returns the patch that corrects a transaction booked at a value
// different from its pending authorisation. It is empty when nothing changed or
// when the user has reconciled the transaction.
func AmountPatch(t *Transaction, amountCents int64) Patch {
	var p Patch
	if t.Reconciled || t.AmountCents == amountCents {
		return p
	}
	p.AmountCents = Int64(amountCents)
	return p
}

// Adopt merges an incoming bank transaction into an existing one that has
// already been chosen, skipping the weighing entirely.
//
// It exists for the review queue, where the choice was a person's rather than
// the model's. Routing that through Reconcile would re-derive the decision and
// could arrive somewhere else, which is exactly what asking was meant to avoid.
func Adopt(ctx context.Context, s Store, t *Transaction, in ImportedFields, pol Policy) error {
	return applyImported(ctx, s, t, in, pol)
}

func applyImported(ctx context.Context, s Store, t *Transaction, in ImportedFields, pol Policy) error {
	p := MergePatch(t, in, pol.PayeePrefixes)

	// An adopted row must carry the booked amount. Without this the tolerance
	// above would leave every pre-authorisation standing at its authorised
	// value forever — the same defect this project already fixed once on the
	// pending-map path.
	if ap := AmountPatch(t, in.AmountCents); ap.AmountCents != nil {
		p.AmountCents = ap.AmountCents
	}

	if p.IsEmpty() {
		return nil
	}
	return s.Update(ctx, t, p)
}

// Apply writes the patch onto the in-memory transaction. Backends call it after
// the change is durable so the caller's view stays consistent.
func Apply(t *Transaction, p Patch) {
	if p.AmountCents != nil {
		t.AmountCents = *p.AmountCents
	}
	if p.PayeeName != nil {
		t.PayeeName = *p.PayeeName
	}
	if p.Notes != nil {
		t.Notes = *p.Notes
	}
	if p.ExternalRef != nil {
		t.ExternalRef = *p.ExternalRef
	}
	if p.ImportedPayee != nil {
		t.ImportedPayee = *p.ImportedPayee
	}
	if p.Cleared != nil {
		t.Cleared = *p.Cleared
	}
}

// samePayee reports whether two spellings denote the same payee once the card
// scheme prefix is discounted.
func samePayee(a, b string, prefixes []string) bool {
	return strings.EqualFold(NormalisePayee(a, prefixes), NormalisePayee(b, prefixes))
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// Shipped defaults for the decision thresholds. They are calibrated against the
// behaviour this package's tests already pinned, not chosen for roundness: the
// weakest case that must merge scores 94%, the strongest that must not scores
// 47%, and these sit between them with room on either side.
//
// They did not move when the candidate-count prior was rewritten, and that is
// the point: at an even overlap the new form is bit-identical to the old one, so
// there was nothing for them to move with. An earlier attempt at this counted
// every row in the window instead of the plausible ones, which does shift every
// weight and did force both thresholds down to 0.85 and 0.35 to keep the anchors
// where they are documented. That attempt is described in Assess and was
// withdrawn.
const (
	defaultAutoProbability   = 0.90
	defaultReviewProbability = 0.50
	defaultMarginBits        = 1.0

	// defaultOverlap is the share of transactions reaching the matcher that have
	// a counterpart in their window at all. See Policy.Overlap and prior().
	defaultOverlap = 0.50
)

// calibration is the rescaling in force, the identity being the default rather
// than the zero value — a zero Calibration maps every weight to a half.
func (p Policy) calibration() Calibration {
	if p.Calibration.A == 0 && p.Calibration.B == 0 {
		return Identity()
	}
	return p.Calibration
}

func (p Policy) linkage() Linkage {
	if p.Linkage.PayeeM == nil {
		return DefaultLinkage()
	}
	return p.Linkage
}

func (p Policy) autoProbability() float64 {
	if p.AutoProbability <= 0 {
		return defaultAutoProbability
	}
	return p.AutoProbability
}

func (p Policy) reviewProbability() float64 {
	if p.ReviewProbability <= 0 {
		return defaultReviewProbability
	}
	return p.ReviewProbability
}

// overlap is the operator's claim about how often a transaction reaching the
// matcher has a counterpart at all, which is what the candidate-count prior is
// taken over. Zero means unset and falls back to the shipped claim.
func (p Policy) overlap() float64 {
	if p.Overlap <= 0 || p.Overlap >= 1 {
		return defaultOverlap
	}
	return p.Overlap
}

func (p Policy) marginBits() float64 {
	if p.MarginBits <= 0 {
		return defaultMarginBits
	}
	return p.MarginBits
}

// comparePayee classifies a pair of payee spellings, falling back to equality
// after normalisation when no classifier was injected.
func (p Policy) comparePayee(a, b string) PayeeLevel {
	if p.Compare != nil {
		return p.Compare(a, b)
	}
	na, nb := NormalisePayee(a, p.PayeePrefixes), NormalisePayee(b, p.PayeePrefixes)
	switch {
	case na == "" || nb == "":
		return PayeeMissing
	case strings.EqualFold(na, nb):
		return PayeeExact
	default:
		return PayeeNone
	}
}

// Candidate is one existing transaction weighed against an incoming one.
type Candidate struct {
	Transaction *Transaction
	Comparison  Comparison
	Weight      float64 // Fellegi-Sunter match weight, in bits
	Probability float64

	// Plausible is how many candidates the prior was taken over — the same for
	// every candidate of one incoming transaction, carried here so that whatever
	// reads a decision later can reproduce the arithmetic.
	Plausible int
}

// fieldEvidence is what the fields say on their own, without the prior.
//
// The prior is subtracted rather than avoided, because the identity it used to
// lean on is gone: with the prior written as log2((pi/n)/(1-pi)) a single
// candidate carries log2(pi/(1-pi)), which is zero only at an even overlap. One
// call to Weight and one subtraction is still cheaper than a second way of adding
// the same three terms up, and it cannot drift from what Weight does.
func fieldEvidence(l Linkage, c Comparison, overlap float64) float64 {
	match, none := prior(1, overlap)
	return l.Weight(c, 1, overlap) - math.Log2(match/none)
}

// A note on the term that is deliberately not here, because it was built,
// measured and taken out again, and the next reader should not have to repeat
// that.
//
// The weight of a pair is a likelihood ratio against "these two are not one
// payment". For a decision that is the wrong ratio: the alternatives to "this row
// is the counterpart" are "no row is" and "a different row is", and only the
// first is in the denominator. The bipartite posterior fixes it by normalising
// over the candidates,
//
//	P(j) = (pi/n)L_j / [(1 - pi) + (pi/n)·Σ_k L_k]
//
// which is the pair's own weight less log2(1 + (pi/(n(1-pi)))·Σ_{k≠j} L_k). At an
// even overlap that term is decisive where it applies: two authorisations
// agreeing exactly go from a reported 0.9981 each to 0.4995, three to 0.3330,
// five to 0.1998, which is what "one of these is the settlement and nothing tells
// them apart" actually means.
//
// It is not here because this program already does that arithmetic, and better.
// Solve arranges the whole batch under a one-to-one constraint, so competition
// between candidates is resolved globally rather than window by window — and the
// two cases are not the same. Two settlements arriving for two identical
// authorisations have exactly one arrangement that uses both, and there is
// nothing to be uncertain about; a per-window term cannot see the other
// settlement, penalises each pairing for the other authorisation's existence, and
// turns a batch with one obvious answer into four rows where there should be two.
// Measured end to end, adding it did exactly that.
//
// Where the batch leaves a genuine ambiguity the margin rule holds it, and the
// margin is the arrangement's rather than the pair's for the same reason. So the
// per-pair weight, the assignment and the margin are three parts of one
// treatment, and the missing normalisation is missing from the weight because it
// lives in the other two.

// Assess narrows candidates to those an incoming transaction may legitimately
// adopt, and weighs each one.
//
// It replaces a set of independent gates — same amount, or open and within
// tolerance and payee agreeing — with a single weighing, which is the point of
// the model: a strong agreement on one field can make up for a weaker one on
// another instead of every field having to pass on its own. What stays a hard
// rule stays a hard rule and runs first: a row claimed earlier in this run, and
// a settled row carrying somebody else's reference, are not candidates at all
// regardless of how well they otherwise fit.
//
// The result is ordered best first, with ties broken by date proximity and then
// by ID so the choice is reproducible across backends.
func Assess(candidates []*Transaction, in ImportedFields, alreadyMatched []*Transaction, pol Policy) []Candidate {
	claimed := make(map[string]struct{}, len(alreadyMatched))
	for _, t := range alreadyMatched {
		claimed[t.ID] = struct{}{}
	}

	survivors := make([]*Transaction, 0, len(candidates))
	for _, c := range candidates {
		if _, skip := claimed[c.ID]; skip {
			continue
		}
		if !adoptable(c, in.ExternalRef) {
			continue
		}
		survivors = append(survivors, c)
	}

	l := pol.linkage()

	// Compared first, weighed second, because the prior depends on how many of
	// these are worth calling candidates at all.
	//
	// That count conditions on the field evidence, and in the strict sense that is
	// circular: the same evidence reaches the score twice, once through the field
	// terms and once through the count those terms qualified. It is kept anyway,
	// and the reason is measured rather than argued.
	//
	// Counting every row in the window instead — which is not circular, and is
	// what the prior's own derivation calls for — costs log2(1/n) on every
	// comparison, and n is the fortnight's traffic rather than anything about the
	// pair. A settlement agreeing on the payee and to the cent, four days after
	// its authorisation, in a fortnight holding a dozen unrelated rows, falls from
	// P = 0.991 to P = 0.890 on the strength of the dozen. Nothing about those
	// twelve rows makes that pairing less likely; they simply exist.
	//
	// The principled remedy is to normalise over the candidates so that rows with
	// no evidence stop costing anything, and this program already does that in
	// Solve — see the note above competitionBits' absence. What Solve cannot do is
	// undo the log2(1/n) the prior has already charged, because that is spent
	// before any arrangement is considered. Filtering the count is the cheap
	// stand-in: it spreads the overlap over the rows that could plausibly carry
	// it, which is the same information the normalisation would use, applied to
	// the prior instead of to the posterior.
	//
	// So this is an approximation and is now labelled as one, where it used to be
	// presented as the obvious thing to do.
	cmps := make([]Comparison, len(survivors))
	plausible := 0
	for i, c := range survivors {
		cmps[i] = Comparison{
			Payee:  pol.comparePayee(c.PayeeName, in.PayeeName),
			Amount: ClassifyAmount(c.AmountCents, in.AmountCents, pol.TolerancePercent, pol.ToleranceCents),
			Date:   ClassifyDate(c.Date, in.Date),
		}
		if cmps[i].Payee == PayeeExact && pol.PayeeFrequency != nil {
			cmps[i].PayeeFrequency = pol.PayeeFrequency(NormalisePayee(in.PayeeName, pol.PayeePrefixes))
		}
		if fieldEvidence(l, cmps[i], pol.overlap()) > 0 {
			plausible++
		}
	}
	if plausible < 1 {
		plausible = 1
	}

	out := make([]Candidate, 0, len(survivors))
	for i, c := range survivors {
		w := l.Weight(cmps[i], plausible, pol.overlap())
		out = append(out, Candidate{
			Transaction: c,
			Comparison:  cmps[i],
			Weight:      w,
			Probability: pol.calibration().Probability(w),
			Plausible:   plausible,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Weight != out[j].Weight {
			return out[i].Weight > out[j].Weight
		}
		di := dayDistance(out[i].Transaction.Date, in.Date)
		dj := dayDistance(out[j].Transaction.Date, in.Date)
		if di != dj {
			return di < dj
		}
		return out[i].Transaction.ID < out[j].Transaction.ID
	})
	return out
}

// noMatchWeight is what leaving a transaction unpaired is worth to the
// arrangement.
//
// It is the weight at which a pairing would only just be worth reviewing, so an
// arrangement never reaches for one that would be refused anyway. Setting it any
// lower would let the constraint drag a transaction onto a row nobody would have
// accepted on its own.
func (p Policy) noMatchWeight() float64 {
	r := p.reviewProbability()
	return math.Log2(r / (1 - r))
}

// decideAssigned reads one transaction's place in the arrangement: adopt the row
// it was paired with, put the pair in front of a person, or treat the
// transaction as new.
//
// The margin is the arrangement's, not the pair's. A candidate that looks like a
// close second is not a real alternative when another transaction has a stronger
// claim on it, and asking about that is asking about a choice that was never
// open.
// The order of the tests is deliberate and was questioned. The review that
// prompted this work argued the margin test should come before, or alongside, the
// review threshold: a crowded window could push a genuine match below it, and the
// pair would then be created as a duplicate with the margin never consulted.
//
// That was true of the arrangement being considered at the time, in which the
// prior counted every row in the window and a dozen unrelated ones cost a clear
// settlement four bits. It is not true of what shipped: the count is of the rows
// the evidence has not already dismissed, so unrelated company costs nothing, and
// the case the argument rested on does not arise.
//
// The ordering is also right on its own terms. The two tests answer different
// questions and only one of them is about this pair. The threshold asks whether a
// counterpart exists; the margin asks which of several it is. Applying the second
// where the first has already said "probably none" would hold pairs for review on
// the strength of their being clearly the best of a bad set, which is most windows
// in a busy fortnight and none of them a question worth a person's time.
func (p Policy) decideAssigned(a Assignment) (chosen *Candidate, hold bool, reason string) {
	if a.Candidate == nil {
		return nil, false, "nothing in the window survived the hard rules"
	}
	if a.Candidate.Probability < p.reviewProbability() {
		return nil, false, "below the review threshold"
	}
	if a.Candidate.Probability < p.autoProbability() {
		return a.Candidate, true, "inside the review band"
	}
	if a.Margin < p.marginBits() {
		return a.Candidate, true, "over the automatic threshold but the arrangement had a free choice"
	}
	return a.Candidate, false, "over the automatic threshold and clear of the alternatives"
}

// Decide reads the assessment: whether to adopt the best candidate outright, put
// it in front of a person, or treat the incoming transaction as new.
func (p Policy) Decide(scored []Candidate) (best *Candidate, auto bool) {
	if len(scored) == 0 {
		return nil, false
	}
	top := &scored[0]
	if top.Probability < p.reviewProbability() {
		return nil, false
	}
	if top.Probability < p.autoProbability() {
		return top, false
	}
	// Clear of the runner-up, or it is a coin toss between two rows that fit
	// equally well — and a visible duplicate beats an invisible mis-adoption.
	if len(scored) > 1 && top.Weight-scored[1].Weight < p.marginBits() {
		return top, false
	}
	return top, true
}

// ClassificationVersion identifies the rules that decide which comparison level
// a pair reaches, which is a smaller thing than the whole parameter set.
func (p Policy) ClassificationVersion() string {
	return ClassificationVersion(p.TolerancePercent, p.ToleranceCents, p.PayeePrefixes)
}

// Version is the identity of the whole effective parameter set: the level
// tables, both thresholds, the margin, the amount tolerance and the payee
// prefixes. Two policies with the same version decide alike.
func (p Policy) Version() string {
	c := p.calibration()
	return p.linkage().Version(p.autoProbability(), p.reviewProbability(), p.marginBits(),
		p.overlap(), p.TolerancePercent, p.ToleranceCents, p.PayeePrefixes, c.A, c.B)
}
