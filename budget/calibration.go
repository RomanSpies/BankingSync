package budget

import (
	"math"
	"sort"
)

// Observation is one decision the model made together with what it turned out to
// be. Weight is the match weight in bits; Match is the answer, once something
// independent of the model has settled it.
type Observation struct {
	Weight float64
	Match  bool
}

// Calibration rescales a match weight before it becomes a probability:
//
//	P = 2^(A·M + B) / (1 + 2^(A·M + B))
//
// This is Platt scaling, and it is here to keep two different things apart.
// Discrimination is whether the model ranks true matches above false ones;
// calibration is whether a figure of ninety per cent happens nine times in ten.
// A model can rank perfectly and be systematically overconfident, and ranking
// metrics cannot see that at all.
//
// Two parameters need one or two orders of magnitude less data than seventeen
// level probabilities, which is why the correction is shaped this way rather
// than as a refit. It also keeps the division of labour honest: m and u stay the
// arguable claims about bank behaviour, and A and B absorb a systematic error in
// the scale on which those claims are added up.
//
// What A and B cannot absorb is the conditional independence assumption, and an
// earlier version of this comment claimed they could. A·M + B is strictly
// monotone in M, so two comparison patterns that reach the same M — say an exact
// payee with an amount within tolerance, against a truncated payee with the
// amount to the cent — come back with identical probabilities under every (A, B)
// there is. Conditional dependence means their true match probabilities differ.
// No monotone rescaling separates two patterns the weight has already collapsed
// together, which is the same point Brier's decomposition makes below: this is a
// resolution failure, and rescaling moves reliability.
//
// The zero value is not the identity; Identity() is. A zero Calibration would
// map every weight to a half.
type Calibration struct {
	A, B float64
}

// Identity is the calibration that changes nothing, and the one shipped until
// an installation has the labels to fit a better one.
func Identity() Calibration { return Calibration{A: 1} }

// IsIdentity reports whether this calibration leaves every probability alone.
func (c Calibration) IsIdentity() bool { return c.A == 1 && c.B == 0 }

// Probability turns a match weight into a probability under this calibration.
func (c Calibration) Probability(weight float64) float64 {
	return Probability(c.A*weight + c.B)
}

// Bin is one column of a reliability diagram: what the model said, and what
// happened.
type Bin struct {
	Lower, Upper float64 // probability range
	Count        int
	MeanForecast float64 // what the model said, on average
	Observed     float64 // how often it was right
}

// defaultBins is the bin count used when a caller passes something unusable.
const defaultBins = 10

// binIndex places a probability in one of bins equal-width columns.
//
// Shared by everything that bins, so that a score and the diagram it is
// decomposed against cannot drift apart over which column a point fell in.
func binIndex(p float64, bins int) int {
	k := int(p * float64(bins))
	if k >= bins {
		k = bins - 1
	}
	if k < 0 {
		k = 0
	}
	return k
}

// Reliability builds the diagram: predictions grouped by what they claimed, each
// group's claim against its outcome.
//
// A group whose observed rate falls below its mean forecast is overconfident,
// and that is the failure this whole file exists to detect — a threshold of
// ninety per cent on a model that is really running at seventy is a threshold
// nobody chose.
func Reliability(obs []Observation, c Calibration, bins int) []Bin {
	if bins < 1 {
		bins = defaultBins
	}
	out := make([]Bin, bins)
	for i := range out {
		out[i].Lower = float64(i) / float64(bins)
		out[i].Upper = float64(i+1) / float64(bins)
	}
	for _, o := range obs {
		p := c.Probability(o.Weight)
		k := binIndex(p, bins)
		out[k].Count++
		out[k].MeanForecast += p
		if o.Match {
			out[k].Observed++
		}
	}
	for i := range out {
		if out[i].Count > 0 {
			out[i].MeanForecast /= float64(out[i].Count)
			out[i].Observed /= float64(out[i].Count)
		}
	}
	return out
}

// BrierScore is the mean squared error of the probabilities, split the way
// Murphy (1973) split it and then corrected the way Stephenson, Coelho and
// Jolliffe (2008) corrected it:
//
//	Brier = Reliability − Resolution + Uncertainty + WithinBinVariance − WithinBinCovariance
//
// The split is what makes the number useful rather than merely small. Only a
// model that separates matches from non-matches better raises Resolution, so a
// change that improves the score by lowering Reliability alone has moved the
// scale rather than the model. Without the split those two look identical.
//
// The last two terms are why this is a five-way split and not the three-way one
// every textbook prints. Murphy's identity requires stratifying on the distinct
// forecast values; this bins by equal width of probability and uses each bin's
// mean, so within a populated bin the forecasts differ from the bin's mean and
// two further terms appear. Dropped, the three components do not sum to the
// score — measured on four thousand synthetic forecasts the gap ran to 0.0034 at
// five bins, closing only as the bin count approached the sample size, which is
// exactly what the theory says. With them the identity holds at any bin count.
//
// Note what follows for the claim, made in an earlier version of this comment,
// that rescaling the probabilities cannot touch Resolution. Under binning it
// can: recalibration moves points across bin boundaries, and Resolution is
// computed per bin. The statement is true of the unbinned decomposition and
// false of this one.
//
// Uncertainty depends only on how often matches occur at all and no model can
// change it; it is the score a constant forecast of the base rate would get.
//
// Score is computed straight from the residuals. It is bin-independent, it is
// correct, and it is the only field the promotion gate reads — the binning
// argument above corrupts a diagnostic, never a decision.
type BrierScore struct {
	Score               float64
	Reliability         float64
	Resolution          float64
	Uncertainty         float64
	WithinBinVariance   float64
	WithinBinCovariance float64
	Base                float64 // share of observations that were matches
	Count               int
}

// GeneralisedResolution is Stephenson's GRES = RES − WBV + WBC, the quantity
// that makes Reliability − GeneralisedResolution + Uncertainty an identity with
// the score at any bin count.
func (b BrierScore) GeneralisedResolution() float64 {
	return b.Resolution - b.WithinBinVariance + b.WithinBinCovariance
}

// Brier scores a set of observations under a calibration.
func Brier(obs []Observation, c Calibration, bins int) BrierScore {
	out := BrierScore{Count: len(obs)}
	if len(obs) == 0 {
		return out
	}
	if bins < 1 {
		bins = defaultBins
	}

	var matches int
	for _, o := range obs {
		if o.Match {
			matches++
		}
		p := c.Probability(o.Weight)
		y := 0.0
		if o.Match {
			y = 1
		}
		out.Score += (p - y) * (p - y)
	}
	out.Score /= float64(len(obs))
	out.Base = float64(matches) / float64(len(obs))
	out.Uncertainty = out.Base * (1 - out.Base)

	diagram := Reliability(obs, c, bins)
	for _, b := range diagram {
		if b.Count == 0 {
			continue
		}
		w := float64(b.Count) / float64(len(obs))
		out.Reliability += w * (b.MeanForecast - b.Observed) * (b.MeanForecast - b.Observed)
		out.Resolution += w * (b.Observed - out.Base) * (b.Observed - out.Base)
	}

	// The within-bin terms. Stephenson weights each bin's mean by n_k/n, and
	// (n_k/n)·(1/n_k)·Σ over the bin is (1/n)·Σ over everything, so one pass over
	// the observations divided by n gives both without a second grouping.
	for _, o := range obs {
		p := c.Probability(o.Weight)
		b := diagram[binIndex(p, bins)]
		y := 0.0
		if o.Match {
			y = 1
		}
		d := p - b.MeanForecast
		out.WithinBinVariance += d * d
		out.WithinBinCovariance += 2 * d * (y - b.Observed)
	}
	out.WithinBinVariance /= float64(len(obs))
	out.WithinBinCovariance /= float64(len(obs))
	return out
}

// minECEObservations is the number of labelled decisions below which the binned
// calibration error says more about the estimator than about the model.
//
// The binned estimator is biased upwards and the bias does not vanish with a
// correct model: measured on forecasts calibrated by construction, so that the
// true error is zero, ten equal-mass bins report about 0.14 at fifty
// observations, 0.064 at a hundred and twenty and 0.041 at three hundred. An
// alarm on a correct model is worse than no alarm, so below this the answer is
// that there is no answer. Roelofs, Cain, Shlens and Mozer (AISTATS 2022)
// establish the bias and its growth with the bin count.
const minECEObservations = 300

// ECE is the expected calibration error: the average gap between what was
// claimed and what happened, weighted by how many predictions made each claim.
// One number, for an alarm.
//
// The bins are equal-mass rather than equal-width, which is the choice with the
// lower bias on the forecast distribution this model produces. That distribution
// matters and the usual advice does not transfer: on uniformly spread forecasts
// equal-mass is very slightly worse, while on the U-shaped spread a matcher
// actually emits — most decisions confident, few near the middle — it is about
// twelve per cent better at every sample size tried. Equal-width leaves the
// crowded columns at the ends carrying most of the data and estimates their gap
// from wildly different forecasts.
//
// The second return is false when there are too few observations for the number
// to mean anything; see minECEObservations. A caller must not substitute zero.
func ECE(obs []Observation, c Calibration, bins int) (float64, bool) {
	if len(obs) < minECEObservations {
		return 0, false
	}
	if bins < 1 {
		bins = defaultBins
	}

	type forecast struct {
		p float64
		y float64
	}
	fs := make([]forecast, len(obs))
	for i, o := range obs {
		y := 0.0
		if o.Match {
			y = 1
		}
		fs[i] = forecast{p: c.Probability(o.Weight), y: y}
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].p < fs[j].p })

	var out float64
	n := len(fs)
	for k := 0; k < bins; k++ {
		lo, hi := k*n/bins, (k+1)*n/bins
		if hi <= lo {
			continue
		}
		var sumP, sumY float64
		for _, f := range fs[lo:hi] {
			sumP += f.p
			sumY += f.y
		}
		m := float64(hi - lo)
		out += m / float64(n) * math.Abs(sumY/m-sumP/m)
	}
	return out, true
}

// Constants of the Platt fit, named after Lin, Weng and Lin (2007), whose
// Algorithm 1 this implements.
const (
	plattMaxIter = 100
	plattMinStep = 1e-10
	plattSigma   = 1e-12 // Hessian ridge, keeping H + σI positive definite
	plattGradTol = 1e-5
	plattArmijo  = 1e-4
)

// sigmoidStable evaluates 1/(1+e^-z) without exponentiating a large positive
// number, which would overflow to +Inf and take the result to zero or NaN.
func sigmoidStable(z float64) float64 {
	if z >= 0 {
		return 1 / (1 + math.Exp(-z))
	}
	e := math.Exp(z)
	return e / (1 + e)
}

// plattObjective is the negative log-likelihood the fit minimises, evaluated
// without overflow.
//
// Branched on the sign of z because log(1+e^z) overflows for large positive z,
// and because 1−σ(z) loses every significant digit as σ(z) approaches one — the
// "catastrophic cancellation" Lin et al. name as the second of the two failures
// their note exists to fix. Both arms are the same function written differently
// and neither ever exponentiates a large positive number.
func plattObjective(obs []Observation, hi, lo, a, b float64) float64 {
	var f float64
	for _, o := range obs {
		t := lo
		if o.Match {
			t = hi
		}
		z := a*o.Weight + b
		if z >= 0 {
			f += math.Log1p(math.Exp(-z)) + (1-t)*z
		} else {
			f += math.Log1p(math.Exp(z)) - t*z
		}
	}
	return f
}

// FitPlatt estimates a calibration from labelled decisions.
//
// Logistic regression with the raw weight as its only feature. Isotonic
// regression would be more flexible and would overfit on the few hundred labels
// an installation accumulates in a year; two parameters are what this much data
// supports.
//
// The targets are smoothed as Platt (1999) prescribes rather than being taken as
// zero and one. That keeps the optimum finite on perfectly separable labels. It
// does not keep the iteration from diverging, and an earlier version of this
// file confused the two.
//
// This is Lin, Weng and Lin (2007) Algorithm 1, not plain Newton, and the
// difference is not academic. Plain undamped Newton on this problem was measured
// against known coefficients across the weight range the model actually produces
// — the strongest comparison scores about +10 bits and the weakest about −9.5,
// so a spread of four to eight bits is the operating regime — and in it the fit
// either ran away to a slope near 10^10, scoring worse than applying no
// calibration at all, or returned the identity because a saturated Hessian
// tripped its determinant test on the first iteration. Neither was reported.
// Three things prevent that: every sigmoid and logarithm is evaluated branched
// on sign, the Hessian carries a ridge, and each step is accepted only if it
// decreases the objective by the Armijo condition. The stopping rule is on the
// gradient, because a small step is not evidence of a small gradient.
//
// It returns the identity when there is nothing to fit and when the fit is
// unusable. It does not return the identity merely because the line search ran
// out of room: at the optimum there is nothing left to decrease, the step
// collapses and the Armijo condition can no longer be met, and that is
// convergence rather than failure. Discarding the point reached there costs a
// good fit — measured, a recovered slope of 0.42 against a true 0.40, Brier
// 0.2024 where the identity scores 0.2580.
// PlattReport is how a calibration fit went, for the benefit of whoever has to
// explain a model that is not rescaling anything.
//
// A fit that gives up returns the identity, and an identity is exactly what an
// installation that never fitted one is running — so without this the two are
// the same object and the difference is unrecoverable. Outcome is the reason the
// loop stopped, in the words used below.
type PlattReport struct {
	Outcome            string
	Iterations         int
	Positive, Negative int
	A, B               float64
}

// FitPlatt estimates a calibration from labelled decisions. See fitPlatt for
// what it does and why; this is the form for callers that do not need to know
// how it went.
func FitPlatt(obs []Observation) Calibration {
	c, _ := fitPlatt(obs)
	return c
}

func fitPlatt(obs []Observation) (Calibration, PlattReport) {
	var pos, neg int
	for _, o := range obs {
		if o.Match {
			pos++
		} else {
			neg++
		}
	}
	report := PlattReport{Positive: pos, Negative: neg, A: 1}

	// Both outcomes have to occur, or there is no boundary to place.
	if pos == 0 || neg == 0 {
		report.Outcome = "one outcome only"
		return Identity(), report
	}

	hi := (float64(pos) + 1) / (float64(pos) + 2)
	lo := 1 / (float64(neg) + 2)

	// Fitted in natural log-odds and converted back, so the arithmetic is the
	// textbook one and only the last step knows this model counts in bits. The
	// initial (ln2, 0) is A = 1, B = 0: the identity.
	const ln2 = math.Ln2
	alpha, beta := ln2, 0.0
	fval := plattObjective(obs, hi, lo, alpha, beta)

	for iter := 0; iter < plattMaxIter; iter++ {
		h11, h22, h21 := plattSigma, plattSigma, 0.0
		var g1, g2 float64
		for _, o := range obs {
			t := lo
			if o.Match {
				t = hi
			}
			p := sigmoidStable(alpha*o.Weight + beta)
			d2 := p * (1 - p)
			h11 += o.Weight * o.Weight * d2
			h22 += d2
			h21 += o.Weight * d2
			d1 := p - t
			g1 += o.Weight * d1
			g2 += d1
		}
		report.Iterations = iter + 1
		if math.Abs(g1) < plattGradTol && math.Abs(g2) < plattGradTol {
			report.Outcome = "converged"
			break
		}

		det := h11*h22 - h21*h21
		if det <= 0 {
			// The ridge should make this unreachable; if it is reached the
			// direction is not a descent direction and there is nothing to do
			// with it.
			report.Outcome = "indefinite hessian"
			return Identity(), report
		}
		da := -(h22*g1 - h21*g2) / det
		db := -(h11*g2 - h21*g1) / det
		directional := g1*da + g2*db

		step := 1.0
		moved := false
		for step >= plattMinStep {
			na, nb := alpha+step*da, beta+step*db
			nf := plattObjective(obs, hi, lo, na, nb)
			if nf < fval+plattArmijo*step*directional {
				alpha, beta, fval = na, nb, nf
				moved = true
				break
			}
			step /= 2
		}
		if !moved {
			// No step along the Newton direction decreases the objective any
			// further. That is the optimum to the precision available, so stop
			// here and keep it.
			report.Outcome = "no further decrease"
			break
		}
	}
	if report.Outcome == "" {
		report.Outcome = "iteration limit"
	}

	c := Calibration{A: alpha / ln2, B: beta / ln2}
	if math.IsNaN(c.A) || math.IsInf(c.A, 0) || math.IsNaN(c.B) || math.IsInf(c.B, 0) || c.A <= 0 {
		report.Outcome = "unusable coefficients"
		return Identity(), report
	}
	report.A, report.B = c.A, c.B
	return c, report
}

// SplitHoldout divides observations into a set to fit on and a set to score on.
//
// Scoring a calibration on the data it was fitted to reports how well two
// parameters can trace a few hundred points, which is not the question. The
// split is deterministic — every third observation, in weight order — so a
// figure can be reproduced rather than merely quoted.
//
// Reproduced from the observations, which is why the order is total rather than
// stable. Weight alone is not: a corpus contains many decisions of identical
// weight, and leaving those to the order they arrived in would make the figure
// depend on the sequence the database happened to return.
func SplitHoldout(obs []Observation) (fit, holdout []Observation) {
	sorted := append([]Observation(nil), obs...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Weight != sorted[j].Weight {
			return sorted[i].Weight < sorted[j].Weight
		}
		return !sorted[i].Match && sorted[j].Match
	})
	for i, o := range sorted {
		if i%3 == 2 {
			holdout = append(holdout, o)
		} else {
			fit = append(fit, o)
		}
	}
	return fit, holdout
}
