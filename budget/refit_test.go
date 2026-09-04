package budget

import (
	"math"
	"math/rand/v2"
	"testing"
)

// drawCounts samples level observations from a linkage, so that an estimator can
// be asked to find its way back to parameters that are known.
func drawCounts(l Linkage, n int, seed uint64) LevelCounts {
	r := rand.New(rand.NewPCG(seed, seed^0x2545f491))
	out := LevelCounts{
		PayeeM: map[PayeeLevel]int{}, PayeeU: map[PayeeLevel]int{},
		AmountM: map[AmountLevel]int{}, AmountU: map[AmountLevel]int{},
		DateM: map[DateLevel]int{}, DateU: map[DateLevel]int{},
	}
	drawPayee := func(dist map[PayeeLevel]float64, into map[PayeeLevel]int) {
		u, acc := r.Float64(), 0.0
		for _, k := range []PayeeLevel{PayeeMissing, PayeeNone, PayeeConflict, PayeeSubset,
			PayeeTruncated, PayeeFuzzy, PayeeExact} {
			acc += dist[k]
			if u <= acc {
				into[k]++
				return
			}
		}
		into[PayeeExact]++
	}
	for i := 0; i < n; i++ {
		drawPayee(l.PayeeM, out.PayeeM)
		drawPayee(l.PayeeU, out.PayeeU)
	}
	return out
}

// TestPosterior_isThePriorWhenNothingWasObserved is the guarantee for every
// installation that never labels anything, which is most of them. Not
// approximately the prior — exactly it.
func TestPosterior_isThePriorWhenNothingWasObserved(t *testing.T) {
	base := DefaultLinkage()
	got := Refit(base, LevelCounts{}, defaultAlpha)

	for _, lv := range []PayeeLevel{PayeeMissing, PayeeNone, PayeeConflict, PayeeSubset,
		PayeeTruncated, PayeeFuzzy, PayeeExact} {
		if got.PayeeM[lv] != base.PayeeM[lv] || got.PayeeU[lv] != base.PayeeU[lv] {
			t.Errorf("%s moved with nothing observed: m %.6f→%.6f, u %.6f→%.6f",
				lv, base.PayeeM[lv], got.PayeeM[lv], base.PayeeU[lv], got.PayeeU[lv])
		}
	}
}

// TestPosterior_staysADistribution keeps the constraint that stops these
// parameters being free numbers. An estimator that broke it would produce a
// model whose output still looks like a probability and is not one.
func TestPosterior_staysADistribution(t *testing.T) {
	base := DefaultLinkage()
	for _, n := range []int{1, 10, 500, 20000} {
		got := Refit(base, drawCounts(base, n, 4), defaultAlpha)
		var sum float64
		for _, v := range got.PayeeM {
			sum += v
		}
		if math.Abs(sum-1) > 1e-9 {
			t.Errorf("%d observations left the payee m distribution summing to %.9f", n, sum)
		}
		for lv, v := range got.PayeeM {
			if v <= 0 {
				t.Errorf("%d observations drove %s to %v, which has no logarithm", n, lv, v)
			}
		}
	}
}

// TestPosterior_convergesToWhatGeneratedTheData is the verification the plan
// asks for, and it is a check on the arithmetic rather than on the idea.
//
// Parameters are put in, observations are drawn from them, and the estimator has
// to find its way back — from a starting point deliberately far from the answer,
// so that what is being measured is the data pulling it there rather than the
// prior having been right all along.
//
// It shows the estimator converges. It does not show that refitting helps any
// real installation, and no test in this repository can.
func TestPosterior_convergesToWhatGeneratedTheData(t *testing.T) {
	// A bank quite unlike the shipped assumption: it truncates constantly and
	// rarely repeats a payee verbatim.
	truth := DefaultLinkage()
	truth.PayeeM = map[PayeeLevel]float64{
		PayeeExact: 0.15, PayeeTruncated: 0.55, PayeeFuzzy: 0.10,
		PayeeSubset: 0.10, PayeeConflict: 0.02, PayeeNone: 0.03, PayeeMissing: 0.05,
	}

	// Started from the shipped prior, which says the opposite.
	base := DefaultLinkage()
	if base.PayeeM[PayeeTruncated] > 0.2 {
		t.Fatalf("setup: the starting point is not far from the answer")
	}

	prev := math.Inf(1)
	for _, n := range []int{100, 1000, 10000, 100000} {
		got := Refit(base, drawCounts(truth, n, 9), defaultAlpha)

		// Scored conditionally on the payee being present, because that is what
		// this estimator now estimates.
		//
		// Refit holds the null level at its stated mass on both sides so that an
		// absent payee stays worth exactly zero bits, which is the whole point of
		// shipping it at m = u. What is left to estimate is therefore the
		// distribution of the level given that there is a payee at all, and the
		// mass of the null level is a stated claim rather than a fitted one.
		//
		// Getting that claim wrong is not free and it is not scored here either.
		// It scales every other level of the field by the same factor, which
		// shifts every payee weight by the same number of bits — a pure intercept
		// move, and an intercept is exactly what the B of the Platt fit absorbs.
		var err float64
		for lv, want := range conditionalOn(truth.PayeeM, PayeeMissing) {
			err += math.Abs(conditionalOn(got.PayeeM, PayeeMissing)[lv] - want)
		}
		t.Logf("%6d observations: total error %.4f (truncated %.3f, want %.3f)",
			n, err, got.PayeeM[PayeeTruncated], truth.PayeeM[PayeeTruncated])

		if err > prev {
			t.Errorf("%d observations were worse than fewer: %.4f against %.4f", n, err, prev)
		}
		prev = err
	}
	if prev > 0.05 {
		t.Errorf("a hundred thousand observations still leave a total error of %.4f", prev)
	}
}

// TestPosterior_alphaIsHowMuchEvidenceOverrulesAClaim pins the one knob, because
// a knob nobody can be told the meaning of is a knob nobody should be given.
//
// The default is stated as a consequence rather than as the constant, so that
// putting an informative value back fails here. At α = 1 a claim of one half
// against fifty observations that all say otherwise survives as 1/102; at α = 50
// it survives as 1/4, which is a prior refusing to be told anything.
func TestPosterior_alphaIsHowMuchEvidenceOverrulesAClaim(t *testing.T) {
	prior := map[PayeeLevel]float64{PayeeExact: 0.5, PayeeNone: 0.5}
	// Every observation says "none", never "exact".
	counts := map[PayeeLevel]int{PayeeNone: 50}

	got := Posterior(prior, counts, 50)
	// Fifty observations against a claim worth fifty: the prior is overruled by
	// exactly half.
	if want := 0.25; math.Abs(got[PayeeExact]-want) > 1e-9 {
		t.Errorf("with alpha equal to the observation count, the claim moved to %.6f, "+
			"want %.6f — halfway", got[PayeeExact], want)
	}

	stubborn := Posterior(prior, counts, 500)
	if !above(stubborn[PayeeExact], got[PayeeExact]) {
		t.Error("a larger alpha did not hold the claim more firmly")
	}

	shipped := Posterior(prior, counts, 0)
	if want := defaultAlpha * 0.5 / (defaultAlpha + 50); math.Abs(shipped[PayeeExact]-want) > 1e-9 {
		t.Errorf("under the shipped alpha the claim came out at %.6f, want %.6f = "+
			"(%.1f·0.5)/(%.1f+50)", shipped[PayeeExact], want, defaultAlpha, defaultAlpha)
	}
	// The shipped concentration has to stay in the weak half of the range, or the
	// priors stop being claims a bank's own behaviour can overrule. Fifty labels
	// must move a claim of one half most of the way down.
	if !below(shipped[PayeeExact], 0.05) {
		t.Errorf("fifty observations that all say otherwise left the claim at %.4f: the "+
			"shipped alpha of %.1f is no longer a weak prior", shipped[PayeeExact], defaultAlpha)
	}
}

// TestPosterior_readsSilenceAsRarityNotAsCertainty records which way round this
// goes, because it was got wrong once and the wrong way looks more careful.
//
// A level nobody observed is estimated as rarer than claimed, not held where it
// was. Drawing a hundred observations and seeing none of a level is evidence
// that the level is rare; refusing to draw that inference is not caution, it is
// discarding data. The estimate stays strictly positive — silence is never read
// as impossibility, which is what would send the level's weight to infinity —
// and it falls further the more was observed without it.
//
// The alternative, holding such a level at its prior mass and rescaling the
// observed levels into what is left, was implemented and then withdrawn. Checked
// against known truth by simulation it was far worse in the case a refit exists
// for — a bank unlike the shipped assumption — because it reserves the prior's
// mass for levels that turn out to be near-impossible and squeezes every
// observed level by that much.
func TestPosterior_readsSilenceAsRarityNotAsCertainty(t *testing.T) {
	prior := map[PayeeLevel]float64{PayeeExact: 0.5, PayeeNone: 0.5}

	var prev float64 = 1
	for _, n := range []int{10, 100, 1000, 10000} {
		got := Posterior(prior, map[PayeeLevel]int{PayeeNone: n}, defaultAlpha)
		if !below(got[PayeeExact], prior[PayeeExact]) {
			t.Errorf("%d observations, none of them exact, left exact at %.6f — silence "+
				"about a level is evidence that it is rare", n, got[PayeeExact])
		}
		if got[PayeeExact] <= 0 {
			t.Errorf("%d observations drove exact to %v, which has no logarithm; silence "+
				"is not impossibility", n, got[PayeeExact])
		}
		if !below(got[PayeeExact], prev) {
			t.Errorf("%d observations did not shrink the unobserved level further than "+
				"fewer did: %.9f against %.9f", n, got[PayeeExact], prev)
		}
		prev = got[PayeeExact]
	}
}

// conditionalOn renormalises a distribution over every level but one, which is
// how a field reads once its null level has been held out of the estimate.
func conditionalOn[K comparable](dist map[K]float64, excluded K) map[K]float64 {
	var rest float64
	for k, v := range dist {
		if k != excluded {
			rest += v
		}
	}
	out := make(map[K]float64, len(dist))
	if rest <= 0 {
		return out
	}
	for k, v := range dist {
		if k != excluded {
			out[k] = v / rest
		}
	}
	return out
}

// TestRefit_holdsTheRatioOnlyWhereTheSilenceSaysNothing pins the rule's one
// discriminator, because everything it is worth turns on that test.
//
// A level nobody observed can mean two different things and they need opposite
// treatments. If the prior expected it a couple of times in thirty labels, the
// absence is an accident and the level should keep the weight it was shipped
// with. If the prior expected it forty times in five hundred and it never came,
// the bank has told you something and the posterior should say so. The only
// thing separating them is what the prior expected, which is what informativeSilence
// measures.
func TestRefit_holdsTheRatioOnlyWhereTheSilenceSaysNothing(t *testing.T) {
	base := DefaultLinkage()
	shipped := evidence(base.PayeeM[PayeeTruncated], base.PayeeU[PayeeTruncated])

	// A u side that has seen plenty of truncation, which is what makes the
	// unpaired posterior collapse the level: one side pinned, the other silent.
	sampledU := map[PayeeLevel]int{}
	for lv, p := range base.PayeeU {
		sampledU[lv] = int(p * 4000)
	}

	for _, tc := range []struct {
		name   string
		labels int
		hold   bool
	}{
		{"thirty labels: the prior expected 2.4 truncations", 30, true},
		{"sixty labels: the prior expected 4.8, still under the bar", 60, true},
		{"a hundred labels: the prior expected eight", 100, false},
		{"five hundred labels: the prior expected forty", 500, false},
	} {
		// Every label an exact payee, so truncation is never observed.
		countsM := map[PayeeLevel]int{PayeeExact: tc.labels}
		m, u := refitField(base.PayeeM, base.PayeeU, countsM, sampledU, defaultAlpha, PayeeMissing)
		got := evidence(m[PayeeTruncated], u[PayeeTruncated])

		expected := base.PayeeM[PayeeTruncated] * float64(tc.labels)
		t.Logf("%-52s expected %.1f, level worth %+.4f bits (shipped %+.4f)",
			tc.name, expected, got, shipped)

		switch {
		case tc.hold && math.Abs(got-shipped) > 1e-9:
			t.Errorf("%s: the level moved to %+.4f bits from %+.4f. The prior expected it "+
				"%.1f times, so seeing none of it says nothing and the weight must hold",
				tc.name, got, shipped, expected)
		case !tc.hold && !below(got, shipped-0.5):
			t.Errorf("%s: the level is still worth %+.4f bits, near the shipped %+.4f. The "+
				"prior expected it %.1f times and it never appeared, which is evidence "+
				"and must be used", tc.name, got, shipped, expected)
		}

		var sum float64
		for _, v := range m {
			sum += v
		}
		if math.Abs(sum-1) > 1e-9 {
			t.Errorf("%s: the m side sums to %.12f", tc.name, sum)
		}
	}
}

// TestRefit_leavesTheUnpairedAnswerAloneWhereNothingIsHeld is the other half:
// where no level qualifies, the paired refit has to be the plain posterior to the
// last bit, or it is changing answers it was never meant to touch.
func TestRefit_leavesTheUnpairedAnswerAloneWhereNothingIsHeld(t *testing.T) {
	base := DefaultLinkage()

	// A date corpus big enough that every level's silence would be informative,
	// and in fact none of them is silent.
	countsM := map[DateLevel]int{
		DateSame: 200, DateAfterNear: 120, DateBeforeNear: 100,
		DateAfterFar: 40, DateBeforeFar: 30,
	}
	countsU := map[DateLevel]int{
		DateSame: 700, DateAfterNear: 700, DateBeforeNear: 700,
		DateAfterFar: 1400, DateBeforeFar: 1400,
	}

	m, u := refitField(base.DateM, base.DateU, countsM, countsU, defaultAlpha, noPinnedDateLevel)
	wantM := Posterior(base.DateM, countsM, defaultAlpha)
	wantU := Posterior(base.DateU, countsU, defaultAlpha)

	for lv := range wantM {
		if m[lv] != wantM[lv] {
			t.Errorf("%v: the paired refit gave %.17g on the m side where the plain "+
				"posterior gives %.17g", lv, m[lv], wantM[lv])
		}
		if u[lv] != wantU[lv] {
			t.Errorf("%v: the paired refit gave %.17g on the u side where the plain "+
				"posterior gives %.17g", lv, u[lv], wantU[lv])
		}
	}
}
