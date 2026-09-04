package main

import (
	"context"
	"fmt"
	"log"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"bankingsync/budget"
	"bankingsync/logs"
	"bankingsync/store"
	"bankingsync/web"
)

// promotedTrial is the fitted parameter set in force, or a zero Trial when the
// installation is running on the shipped ones — which is the default and, for
// most installations, permanent.
//
// A stored set that cannot be read is ignored rather than fatal. The shipped
// parameters are always a valid answer, and refusing to sync because a fitted
// file is damaged would take the budget down over an optimisation.
func (s *Syncer) promotedTrial() budget.Trial {
	return s.storedTrial(store.SettingPromotedTrial, "in force")
}

// linkageInForce is the parameter set the matcher is deciding with: a promoted
// one where there is one, the shipped set otherwise.
//
// One definition rather than two. The matching path and the gauge that reports
// what each comparison level is worth have to agree, or a dashboard shows the
// weights of a model that is not running — and the gauge exists for exactly the
// moment they would diverge.
func (s *Syncer) linkageInForce() budget.Linkage {
	if t := s.promotedTrial(); !t.IsZero() {
		return t.Linkage
	}
	return budget.DefaultLinkage()
}

// shadowTrial is the candidate being evaluated alongside the one in force.
func (s *Syncer) shadowTrial() budget.Trial {
	return s.storedTrial(store.SettingShadowTrial, "under evaluation")
}

func (s *Syncer) storedTrial(key, which string) budget.Trial {
	raw, err := s.st.GetSetting(key)
	if err != nil || raw == "" {
		return budget.Trial{}
	}
	t, err := budget.UnmarshalTrial([]byte(raw))
	if err != nil {
		log.Printf("The matching parameters %s could not be read (%v) — "+
			"the shipped ones are being used instead", which, err)
		return budget.Trial{}
	}
	return t
}

// labelledDecisionsForFitting collects the settled decisions in the form a refit
// needs: the comparison levels rather than the weight they produced.
//
// The weight is deliberately not reused. It was computed under whatever
// parameters were in force at the time, so a candidate scored against stored
// weights is being compared with itself.
func (s *Syncer) labelledDecisionsForFitting() []budget.LabelledDecision {
	decisions, err := s.st.GetLabelledMatchDecisions(100000)
	if err != nil {
		return nil
	}
	out := make([]budget.LabelledDecision, 0, len(decisions))
	for _, d := range decisions {
		payee, okP := budget.ParsePayeeLevel(d.PayeeLevel)
		amount, okA := budget.ParseAmountLevel(d.AmountLevel)
		date, okD := budget.ParseDateLevel(d.DateLevel)
		if !okP || !okA || !okD {
			// A decision recorded under level names this build no longer has.
			// Skipping it is the only honest option: guessing which level a
			// retired name became would invent evidence.
			continue
		}
		out = append(out, budget.LabelledDecision{
			Comparison: budget.Comparison{Payee: payee, Amount: amount, Date: date},
			Candidates: d.Candidates,
			Match:      *d.Truth,
		})
	}
	return out
}

// PromotionState is everything the promotion page shows.
type PromotionState struct {
	InForce   string
	Candidate string
	Labelled  int
	Verdict   budget.Verdict
	Shadow    store.ShadowTally
	Watching  bool
}

// PromotionStatus works out what could be promoted and what is known about it.
//
// The candidate is recomputed from the evidence every time this is asked rather
// than stored, so the page cannot show a verdict about parameters that were
// proposed a month ago. What is stored is only the candidate being *watched*,
// because a shadow tally has to accumulate against a fixed set to mean anything.
func (s *Syncer) PromotionStatus(ctx context.Context) (PromotionState, error) {
	// Traced because this is the most expensive thing an HTTP request in this
	// program does, and nothing about the page says so. Reaching a verdict refits
	// the linkage, fits a Platt calibration, runs six anchors through the real
	// decision function and takes a significance test over the held-back third —
	// twice, once for the candidate and once for what is in force. It is
	// recomputed from the corpus on every page load on purpose, because a stored
	// verdict would describe parameters proposed a month ago, and the cost of that
	// choice belongs somewhere a person can see it.
	tracer := otel.Tracer("bankingsync")
	_, span := tracer.Start(ctx, "match.promotion_status")
	defer span.End()

	pol := s.matchPolicy("")
	labelled := s.labelledDecisionsForFitting()
	span.SetAttributes(
		attribute.Int("labelled_decisions", len(labelled)),
		attribute.String("param_version", pol.Version()),
	)

	st := PromotionState{
		InForce:  pol.Version(),
		Labelled: len(labelled),
	}
	if len(labelled) == 0 {
		return st, nil
	}

	watched := s.shadowTrial()
	st.Watching = !watched.IsZero()
	if st.Watching {
		tally, err := s.st.CountShadowOutcomes(watched.Version(pol))
		if err != nil {
			return st, fmt.Errorf("count the shadow decisions: %w", err)
		}
		st.Shadow = tally
	}

	st.Verdict = budget.EvaluateTrial(pol, labelled, s.sampledU(pol.ClassificationVersion()), 0,
		st.Shadow.Differing, st.Shadow.Total)
	st.Candidate = st.Verdict.Version
	s.recordGate(st)

	span.SetAttributes(
		attribute.String("candidate", st.Candidate),
		attribute.Bool("watching", st.Watching),
		attribute.Bool("promotable", st.Verdict.Promotable()),
		attribute.Int("holdout", st.Verdict.Holdout),
	)
	// The figures only exist when there was something to compare. Setting them
	// unconditionally would put a p-value of zero on every span taken before an
	// installation had enough labels to test anything.
	if st.Verdict.Comparison.Count > 0 {
		span.SetAttributes(
			attribute.Float64("skill_percent", st.Verdict.Skill),
			attribute.Float64("p_value", st.Verdict.Comparison.PValue),
			attribute.Float64("significance_level", st.Verdict.Level),
			attribute.Float64("base_brier", st.Verdict.Base.Score),
			attribute.Float64("trial_brier", st.Verdict.Trial.Score),
		)
	}
	for _, c := range st.Verdict.Checks {
		span.SetAttributes(attribute.String("check."+c.Name, string(c.Status)))
	}

	// How the candidate was arrived at, which is a different question from
	// whether it scores well. A set fitted from thirty labels can have half its
	// levels still carrying the shipped weight, and the Brier score it wins on
	// will not mention that.
	f := st.Verdict.Fit
	span.SetAttributes(
		attribute.Float64("fit.alpha", f.Alpha),
		attribute.Int("fit.labelled", f.Labelled),
		attribute.Int("fit.sampled_u", f.SampledU),
		attribute.Int("fit.levels_held_at_prior", f.LevelsHeld),
		attribute.String("fit.platt_outcome", f.Platt.Outcome),
		attribute.Int("fit.platt_iterations", f.Platt.Iterations),
		attribute.Int("fit.platt_positive", f.Platt.Positive),
		attribute.Int("fit.platt_negative", f.Platt.Negative),
		attribute.Float64("fit.platt_slope", f.Platt.A),
		attribute.Float64("fit.platt_intercept", f.Platt.B),
	)
	return st, nil
}

// recordGate keeps what the gate concluded so a metric can report it without
// running it. See the gateSnapshot field for why that separation exists.
func (s *Syncer) recordGate(st PromotionState) {
	snap := gateSnapshot{
		labelled:   st.Labelled,
		holdout:    st.Verdict.Holdout,
		skill:      st.Verdict.Skill,
		pValue:     st.Verdict.Comparison.PValue,
		level:      st.Verdict.Level,
		statistic:  st.Verdict.Comparison.Statistic,
		baseBrier:  st.Verdict.Base.Score,
		trialBrier: st.Verdict.Trial.Score,
		levelsHeld: st.Verdict.Fit.LevelsHeld,
		plattFit:   st.Verdict.Fit.Platt.Outcome,
		tested:     st.Verdict.Comparison.Count > 0,
		promotable: st.Verdict.Promotable(),
		checks:     make(map[string]string, len(st.Verdict.Checks)),
	}
	for _, c := range st.Verdict.Checks {
		snap.checks[c.Name] = string(c.Status)
	}

	s.gateMu.Lock()
	s.gate, s.gateSeen = snap, true
	s.gateMu.Unlock()
}

// gateState returns the last verdict and whether there has been one.
func (s *Syncer) gateState() (gateSnapshot, bool) {
	s.gateMu.RLock()
	defer s.gateMu.RUnlock()
	return s.gate, s.gateSeen
}

// WatchTrial starts evaluating the current candidate alongside the parameters in
// force, without acting on it.
func (s *Syncer) WatchTrial(ctx context.Context, version string) error {
	pol := s.matchPolicy("")
	labelled := s.labelledDecisionsForFitting()
	if len(labelled) == 0 {
		return web.Refuse("there is nothing settled to fit parameters to yet")
	}

	candidate, _ := budget.CandidateTrial(budget.FittingPrior(), labelled,
		s.sampledU(pol.ClassificationVersion()), 0, pol.Overlap)
	if got := candidate.Version(pol); got != version {
		return web.Refuse("the evidence changed while this page was open — the candidate " +
			"is no longer the one shown. Look at it again")
	}
	raw, err := budget.MarshalTrial(candidate)
	if err != nil {
		return fmt.Errorf("store the candidate: %w", err)
	}
	if err := s.st.SetSetting(store.SettingShadowTrial, string(raw)); err != nil {
		return fmt.Errorf("store the candidate: %w", err)
	}
	olog.Info(ctx, "match.trial_watched", logs.String("param_version", version))
	log.Printf("Now watching candidate matching parameters %s — nothing has changed yet", version)
	return nil
}

// StopWatching drops the candidate being evaluated.
func (s *Syncer) StopWatching(ctx context.Context) error {
	if err := s.st.SetSetting(store.SettingShadowTrial, ""); err != nil {
		return fmt.Errorf("drop the candidate: %w", err)
	}
	olog.Info(ctx, "match.trial_dropped")
	return nil
}

// PromoteTrial puts the watched candidate into force.
//
// Only the watched one, and only when the gate is open. Promoting a candidate
// nobody has been watching would install parameters with no record of what they
// would have changed, which is the one finding this whole page exists to put in
// front of somebody.
func (s *Syncer) PromoteTrial(ctx context.Context, version string) error {
	watched := s.shadowTrial()
	if watched.IsZero() {
		return web.Refuse("nothing is being watched, so there is no record of what this " +
			"would have changed. Start watching it first")
	}
	pol := s.matchPolicy("")
	if got := watched.Version(pol); got != version {
		return web.Refuse("the parameters on this page are not the ones being watched — " +
			"look at it again")
	}

	state, err := s.PromotionStatus(ctx)
	if err != nil {
		return err
	}
	if !state.Verdict.Promotable() {
		return web.Refuse("the checks on this candidate do not pass, so it cannot be put " +
			"into force")
	}
	if state.Candidate != version {
		return web.Refuse("the candidate the evidence now supports is not the one being " +
			"watched. Start watching the new one instead")
	}

	raw, err := budget.MarshalTrial(watched)
	if err != nil {
		return fmt.Errorf("store the parameters: %w", err)
	}
	if err := s.st.SetSetting(store.SettingPromotedTrial, string(raw)); err != nil {
		return fmt.Errorf("store the parameters: %w", err)
	}
	if err := s.st.SetSetting(store.SettingShadowTrial, ""); err != nil {
		log.Printf("Promoted the parameters but could not clear the watch: %v", err)
	}

	log.Printf("Matching parameters %s are now in force (were %s)", version, state.InForce)
	olog.Info(ctx, "match.trial_promoted",
		logs.String("param_version", version),
		logs.String("previous", state.InForce),
		logs.Int("settled_decisions", state.Labelled),
		logs.Int("shadow_differing", state.Shadow.Differing),
		logs.Int("shadow_total", state.Shadow.Total))
	return nil
}

// RevertParameters puts the shipped parameters back.
//
// The way out, and it has to exist. A promotion is a change to how money is
// matched, and a change that cannot be undone from the same page it was made on
// is one nobody should be encouraged to make.
func (s *Syncer) RevertParameters(ctx context.Context) error {
	if err := s.st.SetSetting(store.SettingPromotedTrial, ""); err != nil {
		return fmt.Errorf("restore the shipped parameters: %w", err)
	}
	log.Print("Matching parameters reverted to the shipped ones")
	olog.Info(ctx, "match.trial_reverted")
	return nil
}

// promotionState adapts the syncer's view for the web layer.
func (s *Syncer) PromotionPage(ctx context.Context) (web.PromotionView, error) {
	st, err := s.PromotionStatus(ctx)
	if err != nil {
		return web.PromotionView{}, err
	}
	v := web.PromotionView{
		InForce:    st.InForce,
		Candidate:  st.Candidate,
		Labelled:   st.Labelled,
		Watching:   st.Watching,
		Promotable: st.Verdict.Promotable() && st.Watching,
		Fitted:     !s.promotedTrial().IsZero(),
	}
	for _, c := range st.Verdict.Checks {
		v.Checks = append(v.Checks, web.PromotionCheck{
			Name: c.Name, Status: string(c.Status), Detail: c.Detail,
		})
	}
	return v, nil
}
