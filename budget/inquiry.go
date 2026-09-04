package budget

import "math"

// ln2 is the natural logarithm of two, which is what carries a variance
// between nats and bits in this file.
const ln2 = math.Ln2

// trigamma is ψ′(x), the derivative of the digamma function, for x > 0.
//
// Go's standard library has Gamma and Lgamma but no polygamma, so this is the
// standard construction: push the argument up with the recurrence
// ψ′(x) = ψ′(x+1) + 1/x², where the series is slow, then take the Bernoulli
// asymptotic expansion, where it converges fast. Checked against ψ′(1) = π²/6,
// ψ′(½) = π²/2 and ψ′(2) = π²/6 − 1, and against a 300000-draw Monte Carlo of
// the variance it is used for, which agrees to three digits.
//
// The series is truncated after the 1/x⁹ term, so its error falls as x⁻¹¹ and the
// recurrence threshold is what buys the accuracy. Six is the usual figure and
// leaves about 1e-9 of relative error; twelve costs six more iterations of a
// two-flop loop and leaves about 1e-12, which is enough that the function agrees
// with its own recurrence to within rounding and cannot be the reason two
// questions are ordered one way rather than the other.
func trigamma(x float64) float64 {
	if x <= 0 {
		return math.Inf(1)
	}
	var acc float64
	for x < 12 {
		acc += 1 / (x * x)
		x++
	}
	inv := 1 / x
	inv2 := inv * inv
	// 1/x + 1/(2x²) + 1/(6x³) − 1/(30x⁵) + 1/(42x⁷) − 1/(30x⁹)
	series := inv * (1 + inv*(0.5+inv*(1.0/6.0+inv2*(-1.0/30.0+inv2*(1.0/42.0+inv2*(-1.0/30.0))))))
	return acc + series
}

// levelVariance is how badly one level's probability is known, expressed as the
// variance of its logarithm in bits.
//
// Under the Dirichlet posterior of refit.go a level carries concentration
// aₖ = α·priorₖ + cₖ against a total a₀ = α + Σcⱼ, and the variance of the log
// of a Dirichlet component has a closed form in the trigamma function:
//
//	Var(log₂ pₖ) = (ψ′(aₖ) − ψ′(a₀)) / ln²2
//
// Exact, not an approximation. This used to be the delta method,
// (a₀ − aₖ)/(aₖ(a₀ + 1)ln²2), carried through the logarithm from Var(pₖ). That
// algebra is right — the a₀² really does cancel — and it agrees with the exact
// form to three digits once aₖ is past a few dozen. It fails in the one regime
// this function exists to describe. At aₖ = 0.08, a level nobody has observed
// holding its share of a concentration of two, it understates by 27×; at
// aₖ = 0.004 it understates by 502×. Both forms fall as aₖ grows, so the single
// most informative question is often the same either way, but the relative
// weighting among questions was wrong by orders of magnitude and that weighting
// is what the sampler maximises over.
//
// Two properties matter for what this is used for. It falls as observations
// arrive, so a level already settled by evidence stops attracting questions. And
// with no observations at all it is a function of the prior alone, so a rare
// level starts out badly known and a common one does not — which is why the
// sampler has something to say on an installation that has never labelled
// anything.
//
// A level the parameters give no mass at all returns zero rather than infinity.
// Such a level contributes an infinitely negative weight, so no answer about it
// could move anything, and treating "declared impossible" as "maximally
// uncertain" would make it the only question ever asked.
func levelVariance[K comparable](prior map[K]float64, counts map[K]int, alpha float64, level K) float64 {
	if alpha <= 0 {
		alpha = defaultAlpha
	}
	var total float64
	for _, c := range counts {
		total += float64(c)
	}
	a0 := alpha + total
	ak := alpha*prior[level] + float64(counts[level])
	if ak <= 0 || a0 <= ak {
		return 0
	}
	return (trigamma(ak) - trigamma(a0)) / (ln2 * ln2)
}

// WeightVariance is how badly the match weight of one comparison is known,
// in bits², given what has been observed so far.
//
// The receiver is the base parameters and the counts are the observations, the
// same two arguments Refit takes: this is the uncertainty of the refit that
// those inputs produce, not of the fitted result, which no longer knows how much
// evidence it rests on.
//
// The six terms are the m and u probabilities of the three fields at the levels
// this comparison reached. They are summed rather than combined, which is the
// model's own conditional-independence assumption applied to its parameters as
// well as to its fields; where that assumption fails the figure is an
// underestimate, and correcting for that failure is what the calibration in
// calibration.go is for.
//
// The prior term contributes nothing: it is a count of candidates, not an
// estimated probability. Nor does the term-frequency correction, which is a
// measurement of this account's own traffic rather than a parameter — its own
// uncertainty is real but is not what this figure is about.
func (l Linkage) WeightVariance(c Comparison, counts LevelCounts, alpha float64) float64 {
	if l.PayeeM == nil {
		l = DefaultLinkage()
	}
	return levelVariance(l.PayeeM, counts.PayeeM, alpha, c.Payee) +
		levelVariance(l.PayeeU, counts.PayeeU, alpha, c.Payee) +
		levelVariance(l.AmountM, counts.AmountM, alpha, c.Amount) +
		levelVariance(l.AmountU, counts.AmountU, alpha, c.Amount) +
		levelVariance(l.DateM, counts.DateM, alpha, c.Date) +
		levelVariance(l.DateU, counts.DateU, alpha, c.Date)
}

// baldNodes is how many points the quadrature below uses. The integrand is
// smooth and one-dimensional, so this is cheap; it is generous because the
// spread can reach eighteen bits and a coarse grid would then step straight over
// the region where the entropy actually changes.
const baldNodes = 2001

// binaryEntropy is H(p) in bits, zero at the endpoints where p log p vanishes.
func binaryEntropy(p float64) float64 {
	if p <= 0 || p >= 1 {
		return 0
	}
	return -(p*math.Log2(p) + (1-p)*math.Log2(1-p))
}

// InformationGain is how much an answer about one decision would be expected to
// say about the parameters, in bits.
//
// This is BALD — Bayesian Active Learning by Disagreement, Houlsby, Huszár,
// Ghahramani and Lengyel (2011), equation 2:
//
//	I(y; θ) = H[y | x, D] − E_θ[H[y | x, θ]]
//
// the decision the model is marginally most unsure of while individual parameter
// settings are confident, which is to say the decision the posterior disagrees
// with itself about most. It is not uncertainty sampling: that would be P(1 − P)
// alone, and the variance factor is what makes this epistemic rather than
// aleatoric. Given the comparison levels the label depends on the parameters only
// through the weight, so y ⊥ θ | M and I(y; θ) reduces exactly to I(y; M) — the
// reduction is exact, not an approximation, and it is what makes a
// one-dimensional integral enough.
//
// It is computed here by quadrature over M ~ Normal(M̄, variance), where M̄ is
// the log-odds the reported probability implies. It used to be the second-order
// expansion (ln2/2)·Var(M)·P(1 − P), whose derivation is correct and whose
// constant is right: expanding the mutual information to second order gives
// I ≈ ½·Var(M)·(−H″(P))·(dP/dM)², and with H in bits and P = 2^M/(1+2^M) those
// middle terms are 1/(P(1−P)ln2) and P²(1−P)²ln²2, leaving exactly that product.
// It agrees with the integral to 0.2% at Var = 0.01.
//
// The regime is the problem. With an empty decision log this model's weights
// carry variances of 80 to 330 bits², a spread of nine to eighteen bits, and a
// second-order expansion in the variance has no business there. Measured against
// the integral, the expansion over-valued decisions near the review band by 11×
// and under-valued confident ones by 100×, so the comment that used to stand here
// — claiming the criterion selects the confident frontier just outside the band,
// which is the one stratum a review answer can never reach — described the
// opposite of what the arithmetic did. It selected the band edge, where answers
// were being collected anyway.
//
// At these variances exact BALD saturates near one bit for every comparison,
// which is itself worth reading: the label is close to a coin flip whatever the
// levels, because the parameters are held under a prior so weak that the model
// claims near-total ignorance of them.
func InformationGain(variance, probability float64) float64 {
	if variance <= 0 || probability <= 0 || probability >= 1 {
		return 0
	}
	mean := math.Log2(probability / (1 - probability))
	if math.IsInf(mean, 0) || math.IsNaN(mean) {
		return 0
	}

	sd := math.Sqrt(variance)
	lo, hi := mean-9*sd, mean+9*sd
	h := (hi - lo) / float64(baldNodes-1)

	var meanP, meanH, weightSum float64
	for i := 0; i < baldNodes; i++ {
		m := lo + float64(i)*h
		z := (m - mean) / sd
		w := math.Exp(-0.5 * z * z)
		if i == 0 || i == baldNodes-1 {
			w *= 0.5 // trapezoid ends
		}
		p := Probability(m)
		meanP += w * p
		meanH += w * binaryEntropy(p)
		weightSum += w
	}
	if weightSum <= 0 {
		return 0
	}
	gain := binaryEntropy(meanP/weightSum) - meanH/weightSum
	if gain <= 0 || math.IsNaN(gain) {
		return 0
	}
	return gain
}

// Inquiry is one decision considered as a question worth asking, and what it
// would be expected to be worth.
type Inquiry struct {
	// Key identifies the decision to whoever will store the answer. This package
	// does not interpret it beyond breaking ties by it.
	Key string

	Variance    float64
	Probability float64
	Gain        float64
}

// ConsiderInquiry scores one decision as a candidate question.
func ConsiderInquiry(key string, l Linkage, c Comparison, counts LevelCounts,
	alpha, probability float64) Inquiry {
	v := l.WeightVariance(c, counts, alpha)
	return Inquiry{
		Key:         key,
		Variance:    v,
		Probability: probability,
		Gain:        InformationGain(v, probability),
	}
}

// MostInformative picks the question worth asking out of a run's worth of them,
// or reports that none is.
//
// One per run, by the plan and on purpose. The value of a label falls off
// sharply — the second question of a run lands on parameters the first has
// already moved, and this estimate cannot see that because it is computed
// against the counts as they were before either was answered. Asking one and
// waiting is both the cheaper thing to do to a person and the more defensible
// thing to do to the estimate.
//
// Ties break on Key so that the same run over the same data asks the same
// question, which the permutation test in the import path depends on.
func MostInformative(in []Inquiry) (Inquiry, bool) {
	var best Inquiry
	found := false
	for _, q := range in {
		if q.Gain <= 0 {
			continue
		}
		switch {
		case !found, q.Gain > best.Gain, q.Gain == best.Gain && q.Key < best.Key:
			best, found = q, true
		}
	}
	return best, found
}
