package budget

// LevelCounts is how often each level of each field was seen among decisions
// something other than the model has settled, split by what the answer was.
//
// The m side counts pairs that turned out to be one payment, the u side pairs
// that did not. They are the two distributions the whole model is built on, and
// this is the only way they can ever stop being stated priors.
type LevelCounts struct {
	PayeeM, PayeeU   map[PayeeLevel]int
	AmountM, AmountU map[AmountLevel]int
	DateM, DateU     map[DateLevel]int
}

// defaultAlpha is the concentration of the Dirichlet prior: how much evidence the
// shipped claims are worth before any observation is counted.
//
// Two. The value and the name it used to carry both needed correcting, and they
// are separate mistakes.
//
// The name first. This was called "add-one smoothing", which it is not.
// Add-one, Laplace's rule, is concentration K over a *uniform* base measure:
// P(wᵢ) = (cᵢ + 1)/(N + V), so every one of the V levels is incremented and the
// denominator carries all V of them (Jurafsky & Martin, Speech and Language
// Processing 3rd ed., §3.6.1, Eq. 3.24). What this does is put a total
// concentration of α over a *non-uniform* base — the shipped probabilities — and
// take the posterior mean. That construction is Dirichlet prior smoothing (Zhai
// & Lafferty 2004, Eq. 12), and an asymmetric Dirichlet prior with concentration
// α and known non-uniform base measure m is its proper name. In the naming used
// for total concentration, α = 1 is Perks' prior; Laplace here would be α = 7 for
// payee and α = 5 for amount and date, and Jeffreys half of that. Calling it
// add-one confused the two definitions of "concentration parameter" that differ
// by a factor of the dimension, which is a trap the literature warns about
// explicitly.
//
// Now the value. It was fifty, then one, and one was argued for from a claim that
// does not survive checking: "one beat fifty at every corpus size tried". The
// harness in estimator_test.go prints six counterexamples, one of them today. The
// figure quoted alongside it — 6.494 bits against 6.004 at twenty labels — does
// not reproduce either; α = 50 at twenty labels gives 6.8933. Both were stale.
//
// Two is chosen for a reason the harness alone cannot supply. Scored over the
// four scenarios under four losses, α = 2 is the only tested concentration that
// improves on α = 1 under all of them, and it is never catastrophic on any single
// scenario the way α ≥ 5 is. But that margin is small — under the
// decision-relevant occurrence-weighted loss the whole improvement is about six
// thousandths of a bit, and nothing decides differently at that scale.
//
// What actually chooses it is how far an unobserved level collapses. A level the
// labels never reached shifts by −log₂((α + M)/α), and at thirty labelled matches
// that is −4.954 bits at α = 1 against −4.000 at α = 2. On the flagship truncated
// level, whose shipped weight is +4.322 bits, the difference is between −0.632
// bits and +0.322: at α = 1 an unobserved truncation becomes evidence *against* a
// match, and at α = 2 it does not. Halving that collapse is the whole of the
// argument.
//
// It is a mitigation and not a fix. Even α = 7 leaves truncation 2.4 bits below
// its prior at thirty labels. The structural answer is the ratio-holding rule in
// Refit, and α is what makes the remainder survivable.
const defaultAlpha = 2.0

// Posterior mixes a stated prior with observed counts.
//
//	p̂ₖ = (α·priorₖ + cₖ) / (α + Σcⱼ)
//
// The Dirichlet posterior mean with the shipped priors as the base measure and α
// as the concentration. It sums to one by construction, so the constraint that
// keeps these parameters from being free numbers survives the fitting.
//
// It does not return the prior for a level nothing was observed about. Such a
// level comes back at α·priorₖ/(α + total): smaller than its prior, because
// drawing `total` observations and seeing k none of them is evidence that k is
// rarer than claimed. The two sides shrink by different amounts — m is fed by
// labels numbering in the tens, u by random sampling numbering in the thousands
// — so log2(m̂/û) moves for an unobserved level by log2((α+U)/(α+M)).
//
// That inference is valid under random sampling and it is not valid here, which
// an earlier version of this comment asserted the opposite of. The m-side labels
// are structurally truncated: a pair is only ever put to a person when its
// probability falls in the review band, so the high-agreement levels are missing
// from the m tallies because they were excluded by construction and not because
// they are rare. The update then treats a structural zero as an empirical one.
// Measured, that takes the flagship truncated case from P = 0.943 to P = 0.350 —
// created as a duplicate, and never shown to anyone.
//
// Posterior is nonetheless left saying what it always said, because it is the
// right answer to the question it is actually asked: given these counts drawn
// this way, what is the posterior mean. The truncation is a property of how the
// counts were collected, so the correction belongs where the counts are assembled
// and not inside the estimator.
//
// An earlier version of this file held unobserved levels at their prior *mass*
// and rescaled the observed ones into what was left, and it was withdrawn. The
// reason recorded for withdrawing it was wrong: it said the rule failed "in
// exactly the case a refit exists for", a bank unlike the shipped assumption, and
// on that scenario the withdrawn rule in fact wins at four of five corpus sizes.
// Where it really fails is on near-impossible levels, because reserving the
// prior's mass for a level that turns out never to happen squeezes every observed
// level by an amount that can be two orders of magnitude too large. That is a
// different fault with a different fix, and holding the *ratio* rather than the
// mass does not inherit it.
func Posterior[K comparable](prior map[K]float64, counts map[K]int, alpha float64) map[K]float64 {
	if alpha <= 0 {
		alpha = defaultAlpha
	}
	var total float64
	for _, c := range counts {
		total += float64(c)
	}

	out := make(map[K]float64, len(prior))
	for k, p := range prior {
		out[k] = (alpha*p + float64(counts[k])) / (alpha + total)
	}
	return out
}

// Refit returns the parameters a set of observations supports, starting from the
// ones it is given.
//
// Starting from rather than replacing: with no observations at all the result is
// the prior exactly, so an installation that never labels anything gets today's
// behaviour and not an approximation of it.
//
// The two sides of a field are refitted together rather than one at a time, which
// is what Posterior on its own cannot do. A level the labels never reached is
// shrunk on the m side against a denominator of tens while the u side, fed by
// thousands of sampled window losers, pins it precisely — so the ratio moves by
// log2((alpha+U)/(alpha+M)) against a level nobody said anything about. See
// holdUnobservedRatio for what is done about it and what that was measured to
// cost.
//
// PayeeMissing is excluded from estimation entirely and kept at its stated mass.
// The counts are left alone — CountLevels stays a faithful tally of what was seen
// — and the decision about what may be estimated from them is taken here.
func Refit(base Linkage, counts LevelCounts, alpha float64) Linkage {
	payeeM, payeeU := refitField(base.PayeeM, base.PayeeU,
		counts.PayeeM, counts.PayeeU, alpha, PayeeMissing)
	amountM, amountU := refitField(base.AmountM, base.AmountU,
		counts.AmountM, counts.AmountU, alpha, noPinnedAmountLevel)
	dateM, dateU := refitField(base.DateM, base.DateU,
		counts.DateM, counts.DateU, alpha, noPinnedDateLevel)

	return Linkage{
		PayeeM:  payeeM,
		PayeeU:  payeeU,
		AmountM: amountM,
		AmountU: amountU,
		DateM:   dateM,
		DateU:   dateU,
	}
}

// Sentinels for fields that have no null level to hold out. They are levels that
// cannot occur — the enumerations start at zero, so a negative value is not one
// of them — which keeps refitField's signature the same for all three fields
// rather than making the pin optional and every call site say so.
const (
	noPinnedAmountLevel = AmountLevel(-1)
	noPinnedDateLevel   = DateLevel(-1)
)

// informativeSilence is how many times the prior has to have expected a level
// before its absence counts as evidence rather than as an accident.
//
// Five, which is the usual bar for when a count is large enough to reason about
// and has no deeper claim on it than that. What it separates: at thirty labels a
// level the prior puts at 0.08 is expected 2.4 times, so seeing none of it is
// unremarkable and says nothing; at five hundred labels the same level is
// expected forty times, and seeing none of it is a fact about the bank.
const informativeSilence = 5.0

// refitField refits one field's m and u distributions together.
//
// pinned names a level excluded from estimation and left at its stated mass on
// both sides. This is Splink's treatment of a null comparison level, which it
// ignores for the purpose of parameter estimation, and it exists because
// PayeeMissing is shipped at m = u = 0.15 precisely so that an absent payee is
// worth exactly zero bits. Nothing in an unconstrained refit preserves that: the
// m side has some forty times fewer observations than the u side, so the two
// estimates of the same quantity move apart, and measured over two thousand
// replications at the minimum promotion bar an absent payee came to carry 0.41
// bits on average and 5.60 bits at worst — against a match, on the strength of
// the payee field having said nothing at all.
func refitField[K comparable](priorM, priorU map[K]float64,
	countsM, countsU map[K]int, alpha float64, pinned K) (map[K]float64, map[K]float64) {

	m := posteriorExcluding(priorM, countsM, alpha, pinned)
	u := posteriorExcluding(priorU, countsU, alpha, pinned)
	holdUnobservedRatio(priorM, priorU, countsM, m, u, pinned)
	return m, u
}

// posteriorExcluding is Posterior with one level held out of the estimate.
//
// The held-out level keeps its prior mass exactly; the rest are estimated among
// themselves and scaled back into the mass that leaves, so the result still sums
// to one. With no level to hold out — the sentinel values above — it is Posterior
// unchanged.
func posteriorExcluding[K comparable](prior map[K]float64, counts map[K]int,
	alpha float64, pinned K) map[K]float64 {

	reserved, isPinned := prior[pinned]
	if !isPinned || reserved <= 0 || reserved >= 1 {
		return Posterior(prior, counts, alpha)
	}

	rest := 1 - reserved
	conditional := make(map[K]float64, len(prior))
	estimated := make(map[K]int, len(counts))
	for k, p := range prior {
		if k == pinned {
			continue
		}
		conditional[k] = p / rest
		estimated[k] = counts[k]
	}

	post := Posterior(conditional, estimated, alpha)
	out := make(map[K]float64, len(prior))
	for k, v := range post {
		out[k] = v * rest
	}
	out[pinned] = reserved
	return out
}

// holdUnobservedRatio keeps a level the labels never reached at the weight it was
// shipped with, instead of letting the two sides shrink it by different amounts.
//
// For a level with no m-side observation the posterior mean is set so that
// log2(m/u) equals the shipped ratio for that level, and the levels that were
// observed are rescaled into the mass that leaves so the distribution still sums
// to one. It is Splink's refusal — it declines to train an m value for a level it
// never saw — expressed as a value rather than as an error, because there is
// nowhere here for an error to go.
//
// It is narrower than the rule withdrawn in 5b6fb61, which held such a level at
// its prior *mass* and rescaled everything else into what was left. That one
// reserved the whole of a near-impossible level's prior mass, which can be two
// orders of magnitude too large, and squeezed every observed level by that
// amount. Holding the ratio moves only as much mass as the u side says the level
// is actually worth.
//
// Scored against known truth by TestEstimator_recoversWhatItIsMeantTo — four
// scenarios per field, four losses, two hundred replications on seeds paired
// across rules, so the differences are not sampling noise. Aggregates over the
// four payee scenarios, which is the field this exists for:
//
//	                          flat  byMatch   byPair  squared
//	no refit                 6.971   0.6354   0.9057    7.597
//	unpaired posterior a=2  21.012   1.0339   4.9774   59.995
//	withdrawn (5b6fb61)     16.970   0.9603   2.8076   38.591
//	this rule, a=2          11.674   0.7758   1.9832   15.863
//
// Against the unpaired posterior it fits — the same concentration, the same
// data, the two sides taken separately — that is 44%, 25%, 60% and 74% less
// error. It beats the withdrawn rule under every loss too. On the date field, at
// the corpus sizes those scenarios use, it is bit-identical to the unpaired
// posterior: every date level carries enough mass that a silence about it is
// always informative, so the rule never fires and there is no unobserved-level
// problem there to solve. At a small enough corpus it would fire on date too,
// which is the point — it keys on what the prior expected, not on the field.
//
// Not refitting at all wins the payee aggregate, and that is reported rather than
// buried. It is an artefact of the scenario mix — two of the four payee banks
// have priors close to the truth, and on those a refit can only add estimation
// noise. On the bank whose priors are wrong, which is the case a refit exists
// for, this rule wins under the decision-relevant loss: 0.1860 bits per true
// match against 0.2718 for leaving the priors alone. That comparison is the one
// the test asserts on; the aggregate is printed for a person to read.
//
// It applies only where the silence is uninformative, and that qualification is
// what keeps it from being the withdrawn rule in another costume. A level the
// prior expected to see five times over and did not see once is genuinely rarer
// than claimed, and shrinking it is the right answer; a level the prior expected
// to see twice in thirty labels tells you nothing by not appearing. The two look
// identical from the m side alone and are told apart by what the prior expected,
// which is the only thing that distinguishes an empirical zero from a structural
// one. Without that test the rule loses badly on the scenario where the priors
// overstate a rare level forty-fold, which is the same failure that sank the
// withdrawn rule.
func holdUnobservedRatio[K comparable](priorM, priorU map[K]float64,
	countsM map[K]int, m, u map[K]float64, pinned K) {

	var total float64
	for _, c := range countsM {
		total += float64(c)
	}

	var heldMass, observedMass, pinnedMass float64
	held := make(map[K]bool, len(m))

	for k := range m {
		if k == pinned {
			pinnedMass += m[k]
			continue
		}
		if countsM[k] > 0 {
			observedMass += m[k]
			continue
		}
		if priorU[k] <= 0 || u[k] <= 0 {
			// No shipped ratio to hold. Leave the plain posterior, which is
			// still strictly positive.
			observedMass += m[k]
			continue
		}
		if priorM[k]*total >= informativeSilence {
			// The silence is evidence. Enough labels went by that the prior
			// expected to see this level several times over, and it did not
			// appear once, so the level really is rarer than claimed and the
			// posterior should say so.
			observedMass += m[k]
			continue
		}
		held[k] = true
	}
	if len(held) == 0 {
		return
	}

	restored := make(map[K]float64, len(held))
	for k := range held {
		restored[k] = u[k] * (priorM[k] / priorU[k])
		heldMass += restored[k]
	}

	target := 1 - pinnedMass - heldMass
	if observedMass <= 0 || target <= 0 {
		// Either nothing was observed at all, in which case there is no estimate
		// to hold anything against, or the held levels would take the whole
		// distribution. Both mean the plain posterior is the better answer.
		return
	}

	scale := target / observedMass
	for k := range m {
		switch {
		case k == pinned:
		case held[k]:
			m[k] = restored[k]
		default:
			m[k] *= scale
		}
	}
}

// LevelsAtPriorRatio counts the comparison levels a refit left carrying exactly
// the weight they were shipped with.
//
// Those are the levels the labels said nothing about, held at their ratio by
// holdUnobservedRatio rather than estimated. The count is how much of a fitted
// parameter set is still a stated claim, and it is the number to look at before
// believing a promotion: a candidate fitted from thirty labels can easily have
// half its levels in this state, and the Brier score it wins on will not say so.
//
// Compared by exact equality on purpose. Holding a ratio sets the m side so that
// log2(m/u) reproduces the base's to the last bit, so anything that survives the
// comparison was held rather than coincidentally close.
func LevelsAtPriorRatio(base, fitted Linkage) int {
	held := 0
	for lv := range base.PayeeM {
		if evidence(fitted.PayeeM[lv], fitted.PayeeU[lv]) == evidence(base.PayeeM[lv], base.PayeeU[lv]) {
			held++
		}
	}
	for lv := range base.AmountM {
		if evidence(fitted.AmountM[lv], fitted.AmountU[lv]) == evidence(base.AmountM[lv], base.AmountU[lv]) {
			held++
		}
	}
	for lv := range base.DateM {
		if evidence(fitted.DateM[lv], fitted.DateU[lv]) == evidence(base.DateM[lv], base.DateU[lv]) {
			held++
		}
	}
	return held
}
