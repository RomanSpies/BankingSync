package budget

import "fmt"

// Trial is a candidate parameter set: what a refit and a calibration fit propose
// to replace the ones in force.
//
// Only these two things can be proposed. The thresholds, the tolerance and the
// payee prefixes are the operator's policy rather than estimates of anything,
// and nothing fitted from data has standing to move them.
type Trial struct {
	Linkage     Linkage
	Calibration Calibration

	// Fit is how this set was arrived at. It is not part of the parameters, is
	// not serialised with them and takes no part in Version — a trial read back
	// from the database has an empty one. It exists so that a candidate can be
	// explained at the moment it is proposed, which is the only moment the
	// information exists at all.
	Fit FitReport
}

// FitReport is what a refit and its calibration were made from.
type FitReport struct {
	// Alpha is the Dirichlet concentration used.
	Alpha float64

	// Labelled is how many settled decisions went in; SampledU how many
	// non-match observations were added to the u side alongside them. The two
	// differ by orders of magnitude and that asymmetry is the design, not a
	// defect — but it is the reason a level can be well known on one side and
	// unknown on the other.
	Labelled int
	SampledU int

	// LevelsHeld is how many of the seventeen comparison levels came back
	// carrying exactly the weight they were shipped with, because the labels
	// said nothing about them. See LevelsAtPriorRatio.
	LevelsHeld int

	Platt PlattReport
}

// IsZero reports whether there is no candidate at all.
func (t Trial) IsZero() bool { return t.Linkage.PayeeM == nil }

// apply returns the policy this trial would produce, leaving everything the
// trial has no business changing exactly as it was.
func (t Trial) apply(pol Policy) Policy {
	pol.Linkage = t.Linkage
	pol.Calibration = t.Calibration
	return pol
}

// Version is the parameter version this trial would run under, which is what a
// dashboard will label its decisions with once it is in force.
func (t Trial) Version(pol Policy) string { return t.apply(pol).Version() }

// FittingPrior is the parameter set every refit starts from, which is always the
// shipped one and never what happens to be in force.
//
// The distinction is the whole reason a Dirichlet posterior is the right shape
// here: the shipped values are the prior and the settled decisions are the
// observations, so the estimate is "the stated claims, moved by all the evidence
// there has ever been". Refitting from the last refit instead would fold the
// same evidence in again on every promotion, and the levels would walk away from
// their priors round after round without a single new observation.
func FittingPrior() Linkage { return DefaultLinkage() }

// LabelledDecision is one settled decision with everything needed to score it
// again under different parameters.
//
// The levels rather than the weight, and that is the point. A stored weight was
// computed under the parameters in force at the time, so comparing a candidate
// set against stored weights compares it against itself and always finds no
// difference. Both sides are recomputed from the levels instead.
//
// What cannot be recomputed is the term-frequency correction, which was a
// measurement of the account's traffic on the day and is not recorded. It is
// therefore absent from both sides, which leaves the comparison fair and the
// absolute figures slightly optimistic — a difference that matters to nobody
// reading this as "is the candidate better", and would matter to somebody
// reading it as "how good is the matcher".
type LabelledDecision struct {
	Comparison Comparison
	Candidates int
	Match      bool
}

// score turns a settled decision into an observation under a given parameter set.
func score(l Linkage, c Calibration, d LabelledDecision, overlap float64) Observation {
	w := l.Weight(d.Comparison, d.Candidates, overlap)
	return Observation{Weight: c.A*w + c.B, Match: d.Match}
}

// emptyCounts is a zero tally with every map ready to be written to.
func emptyCounts() LevelCounts {
	return LevelCounts{
		PayeeM: map[PayeeLevel]int{}, PayeeU: map[PayeeLevel]int{},
		AmountM: map[AmountLevel]int{}, AmountU: map[AmountLevel]int{},
		DateM: map[DateLevel]int{}, DateU: map[DateLevel]int{},
	}
}

// CountLevels tallies settled decisions by comparison level, which is what the
// Dirichlet refit consumes.
// A note on the correction that is not applied here, because it was measured and
// it makes things worse at the rate this program achieves.
//
// These labels are selected on the model's own output: a pair is put to a person
// only when its probability lands between the two thresholds, so the m tables are
// estimated from a band rather than from the population. Fellegi-Sunter is naive
// Bayes and naive Bayes is a global learner in Zadrozny's sense, so that bias does
// not vanish with more data. The selection probability is exactly known, which is
// the textbook case for inverse-probability weighting: count a labelled decision
// as 1 when it was in the band and as 1/r when it was not, r being the rate at
// which the inquiry sampler asks about decisions outside it.
//
// Simulated against a bank whose parameters are known, three hundred replications,
// with the labels collected the way this program collects them — total error in
// bits across the payee levels, and the same weighted by how often each level
// actually occurs:
//
//	r        band labels only        inverse-probability weighted
//	0.002    7.090   0.806           12.742   1.675
//	0.010    6.692   0.743            6.919   0.647
//	0.050    5.300   0.538            2.956   0.254
//	0.200    2.925   0.259            1.430   0.124
//
// It is a real improvement from about one per cent upward and a real degradation
// below it, because a weight of five hundred lets a handful of out-of-band answers
// decide the whole table. This program asks one question per sync against
// thousands of decisions, so r is nearer 0.002 than 0.01 and the top row is the
// one that applies.
//
// What would change that is asking far more questions, and that cost is measured
// in a person's attention rather than in anything this package can trade away. So
// the selection bias is left in place, named, and partly answered elsewhere: the
// ratio-holding in Refit stops the levels the band never reaches from collapsing,
// which is the consequence that moved decisions, and the u sample and the
// reference labels supply what the band cannot.
func CountLevels(in []LabelledDecision) LevelCounts {
	out := emptyCounts()
	for _, d := range in {
		if d.Match {
			out.PayeeM[d.Comparison.Payee]++
			out.AmountM[d.Comparison.Amount]++
			out.DateM[d.Comparison.Date]++
			continue
		}
		out.PayeeU[d.Comparison.Payee]++
		out.AmountU[d.Comparison.Amount]++
		out.DateU[d.Comparison.Date]++
	}
	return out
}

// CountUnchosen tallies a sample of the non-match population by comparison
// level, filling only the u side.
//
// The m side is left empty on purpose. These pairs are known not to be matches;
// they say nothing whatever about how two rows that ARE one payment compare.
func CountUnchosen(cs []Comparison) LevelCounts {
	out := emptyCounts()
	for _, c := range cs {
		out.PayeeU[c.Payee]++
		out.AmountU[c.Amount]++
		out.DateU[c.Date]++
	}
	return out
}

// withSampledU returns the counts with a sampled non-match population added to
// the u side.
//
// Added rather than substituted. A person answering "these are two payments" has
// observed a non-match too, and it is a real observation even though it is drawn
// from the narrow band a review asks about.
//
// The two are not the same population and this pools them anyway. Review-band
// negatives are selected on the model's own output; window losers are not. An
// earlier version of this comment claimed the review counts were "numerically
// small, so it informs without steering", which is a claim about a ratio nobody
// measures — the mixture proportion is whatever the bank feed happens to yield,
// and nothing renormalises it. The two populations differ most on the rare,
// high-agreement levels, which are exactly the ones where a handful of counts
// moves the estimate a long way. Which way the mixture leans is therefore not
// known, and this comment says so rather than asserting a direction.
func withSampledU(c LevelCounts, sampled LevelCounts) LevelCounts {
	for k, v := range sampled.PayeeU {
		c.PayeeU[k] += v
	}
	for k, v := range sampled.AmountU {
		c.AmountU[k] += v
	}
	for k, v := range sampled.DateU {
		c.DateU[k] += v
	}
	return c
}

// ProposeTrial builds the candidate parameter set: a Dirichlet refit of the
// levels and a Platt fit of the scale.
//
// The two sides of the model come from different places, and they have to. The m
// probabilities describe pairs that are one payment, which only something
// outside the model can establish — a person's answer or a bank's own reference.
// The u probabilities describe pairs that are not, and those need no answer from
// anybody: almost every pair drawn from a window is a non-match, so counting the
// candidates a transaction was weighed against and not paired with samples that
// population directly. It is the same argument record linkage has always used
// for estimating u without labels, and it is what makes a refit reachable for an
// installation nobody is answering questions on.
//
// The Platt fit sees the same rows the linkage was refitted from, and that is
// deliberate rather than overlooked. Platt's own note recommends against it, on
// the grounds that a fit on the training scores is biased, and the recommendation
// was tested here rather than taken: a bank unlike the shipped assumption, labels
// drawn from its real parameters, both arrangements scored on two thousand fresh
// decisions from the same bank that neither had seen, three hundred replications
// per corpus size.
//
//	labels   as written   Platt on a held-out third
//	    50      0.04615                     0.05958
//	   120      0.04047                     0.04780
//	   300      0.03561                     0.03882
//	   800      0.03340                     0.03457
//
// Worse at every size, and in 259 to 292 runs of every 300. The bias Platt warns
// about is real and it is smaller than what splitting costs: two parameters
// fitted on a third of fifty labels are worse than two parameters fitted
// optimistically on all of them, and the linkage loses a third of its evidence
// into the bargain. The recommendation is written for datasets where a holdout is
// free.
//
// Worth reading off the same table: below a few hundred labels the calibration is
// barely earning its place at all — leaving the weights alone scored 0.04478 at
// fifty labels against 0.04615 for fitting one. What protects an installation
// from that is the gate, which scores a candidate against what is in force on
// decisions it was not fitted to and refuses one that does not win.
func ProposeTrial(base Linkage, in []LabelledDecision, sampled LevelCounts, alpha, overlap float64) Trial {
	counts := withSampledU(CountLevels(in), sampled)
	fitted := Refit(base, counts, alpha)
	obs := make([]Observation, len(in))
	for i, d := range in {
		obs[i] = score(fitted, Identity(), d, overlap)
	}
	calibration, platt := fitPlatt(obs)

	used := alpha
	if used <= 0 {
		used = defaultAlpha
	}
	var sampledU int
	for _, n := range sampled.PayeeU {
		sampledU += n
	}
	return Trial{
		Linkage:     fitted,
		Calibration: calibration,
		Fit: FitReport{
			Alpha:      used,
			Labelled:   len(in),
			SampledU:   sampledU,
			LevelsHeld: LevelsAtPriorRatio(base, fitted),
			Platt:      platt,
		},
	}
}

// SplitLabelled divides settled decisions into a part to fit on and a part to be
// judged on, deterministically and by the same rule SplitHoldout uses.
//
// Every third after ordering, rather than at random. Taking every third element
// of a weight-sorted list is systematic sampling from an ordered frame, which
// balances better than a simple random split at these sizes, and the sort key is
// monotone in the probability of a positive so the selection is implicitly
// stratified on the outcome too.
//
// What the determinism buys is narrower than an earlier version of this comment
// claimed. It said the same data has to give the same verdict every time it is
// asked, "or a promotion becomes a matter of asking until the answer is yes".
// Determinism stops the split being re-randomised. It does not stop the question
// being re-asked, because the gate is recomputed from the corpus on every page
// load and the corpus grows. That is optional stopping, it was measured at a 96%
// eventual false pass with the true parameters in force, and what answers it is
// the multiplicity correction in promotionLooks, not this ordering.
func SplitLabelled(base Linkage, in []LabelledDecision, overlap float64) (fit, holdout []LabelledDecision) {
	ordered := append([]LabelledDecision(nil), in...)
	sortByWeight(base, ordered, overlap)
	for i, d := range ordered {
		if i%3 == 2 {
			holdout = append(holdout, d)
			continue
		}
		fit = append(fit, d)
	}
	return fit, holdout
}

// CandidateTrial is the parameter set a corpus supports, together with the
// decisions held back from fitting it.
//
// One definition, because there used to be two and they disagreed. The verdict
// took its version and its anchors from a set fitted on every label; the
// calibration check built a second set from two thirds and reported that one's
// Brier score; and what was installed was the first. Confirmed by hash — the
// promoted version was b01c517ebd9c and the judged one 21d905f9fd0a — and the
// figures differed too, the gate reporting 0.0938 for a model whose installed
// counterpart scored 0.0875 in-sample on the very decisions it had been fitted
// to. The detail string said "on %d decisions it was not fitted to", which was
// true of the set it described and false of the set that went into force.
//
// So the candidate is fitted on the fitting part alone, and that is the set that
// is versioned, anchored, watched, judged and installed. It costs a third of the
// labels in the parameters that ship. That is the price of the reported figure
// being a figure about the thing it names, and the alternative — fit on
// everything, then judge on some of everything — is not an alternative but the
// thing that was wrong.
func CandidateTrial(base Linkage, in []LabelledDecision, sampled LevelCounts,
	alpha, overlap float64) (Trial, []LabelledDecision) {

	fit, holdout := SplitLabelled(base, in, overlap)
	if len(fit) == 0 {
		// Too few to split. There is nothing to judge on either, and the
		// calibration check reports that separately.
		fit = in
	}
	return ProposeTrial(base, fit, sampled, alpha, overlap), holdout
}

// minPromotionLabels is how many settled decisions there must be before a
// candidate parameter set can be judged at all.
//
// The same bar the calibration figures use, and for the same reason: a Brier
// score over a dozen answers is noise with a decimal point, and promoting on
// noise is worse than not promoting. Below it a trial is not rejected — it is
// undecidable, which is a different thing and is reported as one.
const minPromotionLabels = 50

// promotionAlpha is the significance level a candidate has to clear before it
// may replace the parameters that decide real merges.
//
// The gate used to be `trialScore.Score < baseScore.Score`, an unqualified
// inequality between two noisy means over about a hundred decisions. Measured
// with the true parameters already in force, so that nothing should ever be
// promotable, that opened on 44% of single looks. Five per cent is the
// conventional level and there is nothing deeper to say for it than that; what
// matters is that there is a level at all.
const promotionAlpha = 0.05

// promotionLooks is how many effectively independent chances at promotion a
// corpus of this size has already afforded, and it is the divisor that keeps the
// gate honest as the corpus grows.
//
// The problem it answers is optional stopping. The verdict is recomputed from the
// current corpus on every page load, so an operator who keeps looking is running
// the test again and again on overlapping data. Measured with the true parameters
// in force: a single look opened the old gate 44% of the time and eight looks
// opened it 96% of the time. A significance test alone does not fix that — the
// same simulation puts eight looks at a 20% eventual false pass even at a
// correctly sized 5% test.
//
// Counting page loads would need a write on every read and would punish an
// operator for reloading. Corpus size is the better clock: two looks a minute
// apart see the same corpus and are the same look, while the information that
// makes a genuinely new test arrives as labels do. So the looks are counted at
// doubling milestones from minPromotionLabels upward — 50, 100, 200, 400 — and
// the level is divided by how many have been passed. This needs no stored state
// and gives the same answer for the same corpus.
//
// It is capped, and the cap is the part that was measured rather than reasoned.
// Uncapped, this divides without limit as the corpus grows, which inverts the
// relationship it is supposed to protect: a mature installation with three
// thousand settled decisions would face a stricter bar than a new one with sixty,
// when its evidence is the better of the two. Measured, an uncapped correction
// refused a candidate with a 10% skill improvement at p = 0.009 because the bar
// had reached 0.008.
//
// Against that cost, the measured benefit in this system is nothing. Simulating
// the review's own scenario — the true parameters in force, an operator watching
// as the corpus grows from 60 to 880 — the gate opened in 0 of 40 runs both with
// the correction and without it, because a refit of a model that is already right
// is systematically worse rather than merely different, and a one-sided test
// almost never fires on it. The correction is kept because that argument stops
// holding once the parameters in force are themselves a promoted fit, where the
// candidate and the incumbent really are equally good; it is capped because a
// guard that only ever blocks true positives is not conservatism.
//
// Bonferroni over looks this correlated is very conservative to begin with, so
// four is a bound on the correction rather than an estimate of anything. All of
// this is a pragmatic middle course for a program with one operator, not a
// derivation. Dwork et al.'s reusable holdout is the principled alternative and
// is considerably more machinery than this decision is worth.
const maxPromotionLooks = 4

func promotionLooks(labels int) int {
	if labels <= minPromotionLabels {
		return 1
	}
	looks := 1
	for bar := minPromotionLabels * 2; bar <= labels && looks < maxPromotionLooks; bar *= 2 {
		looks++
	}
	return looks
}

// CheckStatus is what came of one gate check.
type CheckStatus string

const (
	// CheckPassed and CheckFailed are the machine's answer.
	CheckPassed CheckStatus = "passed"
	CheckFailed CheckStatus = "failed"

	// CheckUnavailable means there was not enough evidence to have an opinion.
	// It blocks promotion, because "we cannot tell whether this is better" is
	// not a reason to install it.
	CheckUnavailable CheckStatus = "unavailable"

	// CheckForAPerson is a finding no rule should adjudicate. It does not block
	// on its own; it is the thing the person pressing the button is pressing it
	// about.
	CheckForAPerson CheckStatus = "for a person"
)

// Check is one question asked of a candidate parameter set.
type Check struct {
	Name   string
	Status CheckStatus
	Detail string
}

// Verdict is everything known about a candidate parameter set, and nothing about
// whether anybody wants it.
type Verdict struct {
	Checks  []Check
	Version string

	// Base and Trial are the Brier scores on the held-out part, present only
	// when there were enough settled decisions to compute them.
	Base, Trial BrierScore

	// Comparison is the significance test the calibration check turned on, and
	// the bar it was held to. Carried out of the check rather than left in the
	// sentence it prints, so that a dashboard can plot what the page states: a
	// gate that is refusing candidates because they are no better than chance
	// looks the same from outside as one refusing them because they are worse,
	// and those want different things done about them.
	Comparison DieboldMariano

	// Level is the significance level the comparison had to clear, already
	// divided by the number of looks the corpus has afforded.
	Level float64

	// Skill is the Brier skill score of the candidate against what is in force,
	// in per cent: how much of the incumbent's loss the candidate removes.
	Skill float64

	// Holdout is how many settled decisions the comparison was made on.
	Holdout int

	// Fit is how the candidate was arrived at — the concentration, the evidence
	// that went in, how many levels came back still carrying their shipped
	// weight, and how the calibration fit went. It is the difference between
	// "this candidate scores better" and "this candidate scores better and half
	// of it is still the shipped table".
	Fit FitReport
}

// Promotable reports whether the automatic checks leave the decision open to a
// person. It is never on its own a reason to promote.
func (v Verdict) Promotable() bool {
	for _, c := range v.Checks {
		if c.Status == CheckFailed || c.Status == CheckUnavailable {
			return false
		}
	}
	return len(v.Checks) > 0
}

// EvaluateTrial puts a candidate parameter set through the gate.
//
// Three questions, and they are deliberately different in kind. The anchors ask
// whether the cases this program is documented to get right are still got right,
// and they are checked against the candidate as it would actually be installed.
// The calibration asks whether the fitting procedure produces something better
// than what is in force, and it is checked honestly: refit on part of the
// evidence, judged on the part it never saw. Refitting on everything and scoring
// on the same data would flatter any procedure whatever, including a bad one.
//
// The third is not a question the program answers. How many decisions a change
// would have altered is a fact; whether that many is acceptable is a judgement,
// and nothing here is entitled to make it.
func EvaluateTrial(pol Policy, in []LabelledDecision, sampled LevelCounts,
	alpha float64, differing, total int) Verdict {

	candidate, holdout := CandidateTrial(FittingPrior(), in, sampled, alpha, pol.overlap())

	v := Verdict{Version: candidate.Version(pol), Fit: candidate.Fit}
	v.Checks = append(v.Checks, checkAnchors(pol, candidate))

	if len(in) < minPromotionLabels {
		v.Checks = append(v.Checks, Check{
			Name:   "calibration",
			Status: CheckUnavailable,
			Detail: fmt.Sprintf("%d settled decisions, %d needed before this can be told apart from noise",
				len(in), minPromotionLabels),
		})
	} else {
		appendCalibrationCheck(pol, candidate, holdout, len(in), &v)
	}

	v.Checks = append(v.Checks, Check{
		Name:   "changed decisions",
		Status: CheckForAPerson,
		Detail: describeDifferences(differing, total),
	})
	return v
}

func describeDifferences(differing, total int) string {
	if total == 0 {
		return "nothing has been decided under both sets yet, so there is nothing to compare"
	}
	return fmt.Sprintf("%d of %d decisions would have gone differently (%.1f%%)",
		differing, total, 100*float64(differing)/float64(total))
}

// checkAnchors runs the documented cases through the real decision function
// under the candidate parameters.
func checkAnchors(pol Policy, t Trial) Check {
	trial := t.apply(pol)
	var moved []string
	for _, a := range Anchors() {
		if got := a.decide(trial); got != a.Want {
			moved = append(moved, fmt.Sprintf("%s: %s instead of %s", a.Name, got, a.Want))
		}
	}
	if len(moved) > 0 {
		return Check{Name: "anchor cases", Status: CheckFailed,
			Detail: fmt.Sprintf("%d of %d moved — %v", len(moved), len(Anchors()), moved)}
	}
	return Check{Name: "anchor cases", Status: CheckPassed,
		Detail: fmt.Sprintf("all %d still decided as documented", len(Anchors()))}
}

// appendCalibrationCheck fits on part of the evidence and judges on the rest.
//
// Two different parameter sets appear here and they are not the same thing. The
// prior the refit starts from is always the shipped one; the baseline the result
// has to beat is whatever is in force. Starting a refit from the parameters in
// force would fold the same evidence in a second time on every promotion, and
// the levels would drift further from their priors each round with nothing new
// having been observed.
func appendCalibrationCheck(pol Policy, candidate Trial, holdout []LabelledDecision,
	labels int, v *Verdict) {

	if len(holdout) == 0 {
		v.Checks = append(v.Checks, Check{
			Name: "calibration", Status: CheckUnavailable,
			Detail: "nothing was held back to judge on"})
		return
	}

	baseObs := make([]Observation, len(holdout))
	trialObs := make([]Observation, len(holdout))
	for i, d := range holdout {
		baseObs[i] = score(pol.linkage(), pol.calibration(), d, pol.overlap())
		trialObs[i] = score(candidate.Linkage, candidate.Calibration, d, pol.overlap())
	}
	baseScore := Brier(baseObs, Identity(), 10)
	trialScore := Brier(trialObs, Identity(), 10)

	// Both forecasts are already calibrated into their observations by score(),
	// so the comparison is between two sets of probabilities on the same
	// decisions and the identity is the right calibration to pass here.
	dm, ok := CompareForecasts(trialObs, baseObs, Identity(), Identity())
	level := promotionAlpha / float64(promotionLooks(labels))

	// The skill score is the reportable form: how much of the incumbent's loss
	// the candidate removes. Two raw Brier numbers to four decimals invite the
	// reader to compare them by eye, which is the habit this check exists to
	// replace.
	skill := 0.0
	if baseScore.Score > 0 {
		skill = 100 * (1 - trialScore.Score/baseScore.Score)
	}

	v.Base, v.Trial = baseScore, trialScore
	v.Comparison, v.Level, v.Skill, v.Holdout = dm, level, skill, len(holdout)

	switch {
	case !ok:
		v.Checks = append(v.Checks, Check{
			Name: "calibration", Status: CheckUnavailable,
			Detail: "the two parameter sets do not decide any held-back decision differently, " +
				"so there is nothing to tell apart"})

	case dm.MeanDiff >= 0, dm.PValue >= level:
		v.Checks = append(v.Checks, Check{
			Name: "calibration", Status: CheckFailed,
			Detail: fmt.Sprintf(
				"skill %+.1f%% (Brier %.4f against %.4f in force) on %d decisions the "+
					"candidate was not fitted to, but that is not distinguishable from "+
					"chance: p = %.3f against a bar of %.3f",
				skill, trialScore.Score, baseScore.Score, len(holdout), dm.PValue, level)})

	default:
		v.Checks = append(v.Checks, Check{
			Name: "calibration", Status: CheckPassed,
			Detail: fmt.Sprintf(
				"skill %+.1f%% (Brier %.4f against %.4f in force) on %d decisions the candidate "+
					"was not fitted to, p = %.3f against a bar of %.3f",
				skill, trialScore.Score, baseScore.Score, len(holdout), dm.PValue, level)})
	}
}
