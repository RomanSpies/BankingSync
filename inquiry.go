package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"bankingsync/budget"
	"bankingsync/logs"
	"bankingsync/store"
	"bankingsync/web"
)

// inquirySampler picks the one decision a run may ask a person to confirm even
// though the model settled it alone.
//
// The point is not to double-check the matcher. It is that the automatic bands
// are the only place where nobody ever finds out whether the model was right,
// and they are where every costly mistake happens: a merge above the automatic
// threshold takes two payments and leaves one, and nothing in the program will
// ever contradict it. The review queue produces labels only from the middle
// band, so a model fitted on review answers alone is fitted on the cases it was
// least sure about — the credit-scoring literature calls the same shape reject
// inference, and the correction there is the same as here, which is to spend a
// little effort labelling outside the band on purpose.
//
// Nothing waits on the answer. The decision has already been carried out and the
// transaction is already where it belongs, which is what separates this from the
// review queue: a question here costs attention, not money in the wrong place.
type inquirySampler struct {
	// enabled is false when the operator has not asked for this, when a question
	// is already unanswered, or when the parameters cannot be read.
	enabled bool

	// base and counts are the two halves of the estimate, and they belong
	// together: base is the parameters the observations have not yet been folded
	// into, counts the observations. Handing this the result of a refit instead
	// would read the uncertainty off parameters that have already absorbed the
	// evidence, and every level would look better known than it is.
	base   budget.Linkage
	counts budget.LevelCounts

	found   []budget.Inquiry
	targets map[string]inquiryTarget
}

// inquiryTarget is everything needed to write the question down, kept from the
// moment the decision was made rather than looked up again afterwards.
type inquiryTarget struct {
	account    store.BankAccount
	bank       string
	pendingKey string
	outcome    string
	version    string
	incoming   budget.ImportedFields
	candidate  budget.Transaction
	why        string
}

// newInquirySampler prepares the sampler for one run, or returns a disabled one.
func (s *Syncer) newInquirySampler() *inquirySampler {
	if !s.st.Tunables().AskWhenUnsure {
		return &inquirySampler{}
	}
	// One at a time, and an unanswered question is a reason to stop asking
	// rather than to ask again.
	if open, err := s.st.HasOpenInquiry(); err != nil || open {
		return &inquirySampler{}
	}
	return &inquirySampler{
		enabled: true,
		base:    budget.DefaultLinkage(),
		counts:  s.levelCounts(0),
		targets: map[string]inquiryTarget{},
	}
}

// consider offers one settled decision to the sampler.
//
// Held decisions are passed over: they are already on their way to a person
// through the review queue, and asking about them here would be asking twice.
// So are decisions with no candidate at all, which had nothing to be wrong
// about.
func (p *inquirySampler) consider(acct store.BankAccount, w modelWork, out budget.Outcome, pol budget.Policy) {
	if !p.enabled || len(out.Held) > 0 || out.Best == nil ||
		out.Best.Transaction == nil || w.pendingKey == "" {
		return
	}
	key := fmt.Sprintf("%d|%s", acct.ID, w.pendingKey)
	p.found = append(p.found, budget.ConsiderInquiry(
		key, p.base, out.Best.Comparison, p.counts, 0, out.Best.Probability))
	p.targets[key] = inquiryTarget{
		account:    acct,
		bank:       bankLabel(acct),
		pendingKey: w.pendingKey,
		outcome:    out.Name(),
		version:    pol.Version(),
		incoming:   w.fields,
		candidate:  *out.Best.Transaction,
		why:        explainComparison(out.Best.Comparison, out.Best.Transaction.Date, w.date.Format("2006-01-02")),
	}
}

// ask writes down the single most informative question of the run, if any was
// worth asking.
//
// A failure is logged and swallowed. This is how the model gets better, not how
// transactions get imported, and it must never be the reason a sync reports
// trouble.
func (p *inquirySampler) ask(ctx context.Context, s *Syncer) {
	if !p.enabled {
		return
	}
	best, ok := budget.MostInformative(p.found)
	if !ok {
		return
	}
	t := p.targets[best.Key]

	// The decision this question is about, pinned now. Filing the answer against
	// whatever the newest record happens to be by the time somebody answers
	// would attach it to levels nobody was shown.
	decisionID, err := s.st.LatestMatchDecisionID(t.account.ID, t.pendingKey)
	if err != nil {
		olog.Warn(ctx, "match.inquiry_not_recorded",
			logs.String("bank", t.bank), logs.String("error", err.Error()))
		return
	}

	if err := s.st.AddMatchInquiry(store.MatchInquiry{
		BankAccountID: t.account.ID,
		DecisionID:    decisionID,
		Bank:          t.bank,
		PendingKey:    t.pendingKey,
		ParamVersion:  t.version,
		Outcome:       t.outcome,
		Probability:   best.Probability,
		Gain:          best.Gain,
		TxnDate:       t.incoming.Date.Format("2006-01-02"),
		AmountCents:   t.incoming.AmountCents,
		Currency:      t.incoming.Currency,
		Payee:         t.incoming.PayeeName,
		CandidateDate: t.candidate.Date.Format("2006-01-02"),
		// The candidate as it stood when the decision was made. On an adopted
		// decision the row has since been overwritten with the incoming values,
		// so looking it up now would show the question its own answer.
		CandidateAmount: t.candidate.AmountCents,
		CandidatePayee:  t.candidate.PayeeName,
		Why:             t.why,
	}); err != nil {
		log.Printf("[%s] could not record the confirmation request: %v", t.bank, err)
		olog.Warn(ctx, "match.inquiry_not_recorded",
			logs.String("bank", t.bank), logs.String("error", err.Error()))
		return
	}

	log.Printf("[%s] Asking about one automatic decision on the review page — "+
		"it is already in the budget either way", t.bank)
	olog.Info(ctx, "match.inquiry_raised",
		logs.String("bank", t.bank),
		logs.String("outcome", t.outcome),
		logs.Float64("probability", best.Probability),
		logs.Float64("expected_bits", best.Gain),
		logs.Int("considered", len(p.found)))
	if s.met != nil && s.met.inquiryGain != nil {
		s.met.inquiryGain.Record(ctx, best.Gain, metric.WithAttributes(
			attribute.String("bank", t.bank),
			attribute.String("outcome", t.outcome)))
	}
}

// recordInquiryAnswer files the answer against the decision the question was
// about, falling back to the transaction for questions raised before the
// decision was pinned.
func (s *Syncer) recordInquiryAnswer(q store.MatchInquiry, answer bool) error {
	if q.DecisionID != 0 {
		return s.st.SetMatchDecisionTruthByID(q.DecisionID, answer)
	}
	return s.st.SetMatchDecisionTruth(q.BankAccountID, q.PendingKey, answer)
}

// PendingInquiry returns the confirmation waiting to be answered, if there is
// one, for the review page to show underneath the queue.
func (s *Syncer) PendingInquiry(ctx context.Context) (*web.InquiryItem, error) {
	q, ok, err := s.st.OpenInquiry()
	if err != nil {
		return nil, fmt.Errorf("read the confirmation request: %w", err)
	}
	if !ok {
		return nil, nil
	}
	return &web.InquiryItem{
		ID:              q.ID,
		BankName:        q.Bank,
		Merged:          q.Outcome == "adopted",
		Percent:         percent(q.Probability),
		ParamVersion:    q.ParamVersion,
		Date:            q.TxnDate,
		Amount:          centsToDecimal(q.AmountCents),
		Currency:        q.Currency,
		Payee:           q.Payee,
		CandidateDate:   q.CandidateDate,
		CandidateAmount: centsToDecimal(q.CandidateAmount),
		CandidatePayee:  q.CandidatePayee,
		Why:             q.Why,
		AskedAt:         shortTimestamp(q.AskedAt),
	}, nil
}

// AnswerInquiry records what a person said about a decision the model made
// alone.
//
// The answer is stored against the decision, where the estimators already look
// for it, so nothing downstream has to know that this label came from a question
// rather than from the queue.
//
// A nil answer is somebody saying they do not know, and it deliberately leaves
// the decision unlabelled. Pressing for a yes or a no on a transaction nobody
// remembers buys a label at the cost of it being wrong, and a wrong label is
// worse than none here: it is about to be weighed against a stated prior that at
// least had an argument behind it.
func (s *Syncer) AnswerInquiry(ctx context.Context, id int64, answer *bool, paramVersion string) error {
	q, ok, err := s.st.OpenInquiry()
	if err != nil || !ok || q.ID != id {
		return web.Refuse("that question is no longer open")
	}
	// The same guard the queue uses. An answer given against parameters that
	// have since changed describes a decision the program would no longer make,
	// and folding it into the counts would teach the model about itself as it
	// used to be.
	if paramVersion != "" && paramVersion != q.ParamVersion {
		return web.Refuse("the matching settings changed while this page was open, so this " +
			"decision was made under different rules. It has been dropped rather than guessed at")
	}

	if answer != nil {
		switch err := s.recordInquiryAnswer(q, *answer); {
		case errors.Is(err, store.ErrNoSuchDecision):
			// The decision aged out from under the question. The answer is lost,
			// but the question still has to close: only one may be open at a
			// time, so leaving it would block every later one for good.
			log.Printf("[%s] the decision this question was about is gone, so the answer "+
				"could not be filed", q.Bank)
		case err != nil:
			return fmt.Errorf("record the answer: %w", err)
		}
	}
	if err := s.st.CloseInquiry(id); err != nil {
		return fmt.Errorf("close the confirmation request: %w", err)
	}

	verdict := "unknown"
	if answer != nil {
		verdict = "same_payment"
		if !*answer {
			verdict = "different_payments"
		}
	}
	olog.Info(ctx, "match.inquiry_answered",
		logs.String("bank", q.Bank),
		logs.String("outcome", q.Outcome),
		logs.String("verdict", verdict))
	if s.met != nil && s.met.inquiryAnswers != nil {
		s.met.inquiryAnswers.Add(ctx, 1, metric.WithAttributes(
			attribute.String("bank", q.Bank),
			attribute.String("outcome", q.Outcome),
			attribute.String("verdict", verdict)))
	}
	return nil
}
