package budget

import (
	"math"
	"testing"
)

// TestInquiry_evidenceAboutALevelSettlesIt checks the property the whole sampler
// rests on: a level the installation has observed is a level it stops asking
// about.
func TestInquiry_evidenceAboutALevelSettlesIt(t *testing.T) {
	l := DefaultLinkage()
	c := Comparison{Payee: PayeeTruncated, Amount: AmountExact, Date: DateSame}

	cold := l.WeightVariance(c, LevelCounts{}, defaultAlpha)
	warm := l.WeightVariance(c, LevelCounts{
		PayeeM:  map[PayeeLevel]int{PayeeTruncated: 400},
		PayeeU:  map[PayeeLevel]int{PayeeTruncated: 400},
		AmountM: map[AmountLevel]int{AmountExact: 400},
		AmountU: map[AmountLevel]int{AmountExact: 400},
		DateM:   map[DateLevel]int{DateSame: 400},
		DateU:   map[DateLevel]int{DateSame: 400},
	}, defaultAlpha)

	if !below(warm, cold) {
		t.Fatalf("observing a level must reduce the uncertainty of its weight, "+
			"but it went from %.4f to %.4f bits²", cold, warm)
	}
}

// TestInquiry_aRareLevelIsTheLessKnownOne is what gives the sampler something to
// say on an installation that has never labelled anything: with no observations
// at all the variance is a function of the prior, and a prior spread thin over a
// level knows less about it.
func TestInquiry_aRareLevelIsTheLessKnownOne(t *testing.T) {
	l := DefaultLinkage()
	common := Comparison{Payee: PayeeExact, Amount: AmountExact, Date: DateSame}
	rare := Comparison{Payee: PayeeConflict, Amount: AmountOutsideLower, Date: DateBeforeFar}

	vc := l.WeightVariance(common, LevelCounts{}, defaultAlpha)
	vr := l.WeightVariance(rare, LevelCounts{}, defaultAlpha)
	if !above(vr, vc) {
		t.Fatalf("the rare level combination must be the less well known one, "+
			"got %.4f bits² against %.4f for the common one", vr, vc)
	}
}

// TestInquiry_aLevelGivenNoMassIsNotTreatedAsUnknown pins the guard rather than
// the arithmetic. A level the parameters declare impossible carries an
// infinitely negative weight, so no answer about it could move anything; a naive
// reading of the formula would hand it infinite variance and it would become the
// only question ever asked.
func TestInquiry_aLevelGivenNoMassIsNotTreatedAsUnknown(t *testing.T) {
	l := DefaultLinkage()
	l.PayeeM = map[PayeeLevel]float64{PayeeExact: 1}
	l.PayeeU = map[PayeeLevel]float64{PayeeExact: 1}

	v := l.WeightVariance(Comparison{
		Payee: PayeeConflict, Amount: AmountExact, Date: DateSame,
	}, LevelCounts{}, defaultAlpha)

	if math.IsInf(v, 0) || math.IsNaN(v) {
		t.Fatalf("a level with no mass must not produce %v bits²", v)
	}
	if v <= 0 {
		t.Fatalf("the other two fields still contribute, got %.4f bits²", v)
	}
}

// TestInquiry_uncertaintyAboutAnUnobservedLevelIsBounded records behaviour that
// reads wrong until it is thought through. Observing other levels raises a
// level's own variance, because its estimated probability shrinks while its
// pseudo-count does not. It saturates rather than growing without limit, which is
// what stops a long-running installation from being dragged towards levels it
// never sees.
//
// The bound is ψ′(aₖ)/ln²2, because Var = (ψ′(aₖ) − ψ′(a₀))/ln²2 and ψ′ is
// positive and falls to zero as a₀ grows, so the second term only ever subtracts.
// It used to be 1/(aₖ ln²2), which is the same statement for the delta-method
// form this no longer uses — at aₖ = 0.02 those two differ by a factor of fifty,
// and the old bound is the smaller one.
func TestInquiry_uncertaintyAboutAnUnobservedLevelIsBounded(t *testing.T) {
	l := DefaultLinkage()
	unobserved := PayeeConflict
	ak := defaultAlpha * l.PayeeM[unobserved]
	bound := trigamma(ak) / (math.Ln2 * math.Ln2)

	for _, elsewhere := range []int{100, 10_000, 1_000_000} {
		v := levelVariance(l.PayeeM, map[PayeeLevel]int{PayeeExact: elsewhere},
			defaultAlpha, unobserved)
		if v >= bound {
			t.Fatalf("with %d observations elsewhere the variance of an unobserved "+
				"level reached %.4f, past its bound of %.4f bits²", elsewhere, v, bound)
		}
	}
}

// TestInquiry_gainMatchesTheMutualInformationItApproximates is the test that
// checks the derivation rather than restating it.
//
// InformationGain claims to be the mutual information between the label and the
// parameters, expanded to second order. So it is measured against that quantity
// computed directly: put a Gaussian of the stated variance on the weight,
// integrate H(E[P]) − E[H(P)] over it, and compare. Nothing in the check shares
// a line with the implementation.
func TestInquiry_gainMatchesTheMutualInformationItApproximates(t *testing.T) {
	for _, tc := range []struct{ weight, variance float64 }{
		{weight: 0, variance: 0.05},
		{weight: 2, variance: 0.05},
		{weight: -3, variance: 0.02},
		{weight: 4, variance: 0.01},
	} {
		numeric := mutualInformation(tc.weight, tc.variance)
		got := InformationGain(tc.variance, Probability(tc.weight))

		if rel := math.Abs(got-numeric) / numeric; rel > 0.05 {
			t.Errorf("at %+.1f bits with variance %.3f the closed form gives %.6g bits "+
				"but integrating the mutual information gives %.6g (%.1f%% out)",
				tc.weight, tc.variance, got, numeric, rel*100)
		}
	}
}

// mutualInformation integrates I(y; θ) = H(E[P]) − E[H(P)] numerically over a
// Gaussian on the match weight, by Gauss-Hermite over a fixed grid.
func mutualInformation(mean, variance float64) float64 {
	const half, steps = 6.0, 4001
	sd := math.Sqrt(variance)
	step := 2 * half * sd / (steps - 1)

	var mass, meanP, meanH float64
	for i := 0; i < steps; i++ {
		m := mean - half*sd + float64(i)*step
		z := (m - mean) / sd
		w := math.Exp(-z * z / 2)
		p := Probability(m)
		mass += w
		meanP += w * p
		meanH += w * binaryEntropy(p)
	}
	return binaryEntropy(meanP/mass) - meanH/mass
}

// TestInquiry_aForegoneAnswerIsWorthNothing is the half of the criterion that
// separates it from picking the least well known parameters outright. A decision
// resting on badly known parameters still teaches nothing if its answer was
// never in doubt.
func TestInquiry_aForegoneAnswerIsWorthNothing(t *testing.T) {
	const variance = 0.4

	doubtful := InformationGain(variance, 0.93)
	settled := InformationGain(variance, 0.99999)

	if !below(settled, doubtful/100) {
		t.Fatalf("a decision at 99.999%% must be worth far less than one at 93%% on the "+
			"same parameters, got %.6g against %.6g bits", settled, doubtful)
	}
	if InformationGain(variance, 1) != 0 {
		t.Fatal("a certainty has no answer left to learn")
	}
}

// TestInquiry_asksTheFrontierRatherThanTheSurestCase is the behavioural claim, and
// the reason this is not simply "ask about the least certain decision". Of two
// decisions the model acted on alone, it prefers the one whose parameters are
// thin over the one whose probability is lower.
func TestInquiry_asksTheFrontierRatherThanTheSurestCase(t *testing.T) {
	l := DefaultLinkage()
	counts := LevelCounts{
		PayeeM:  map[PayeeLevel]int{PayeeExact: 5000},
		PayeeU:  map[PayeeLevel]int{PayeeExact: 5000},
		AmountM: map[AmountLevel]int{AmountExact: 5000},
		AmountU: map[AmountLevel]int{AmountExact: 5000},
		DateM:   map[DateLevel]int{DateSame: 5000},
		DateU:   map[DateLevel]int{DateSame: 5000},
	}

	wellTrodden := Comparison{Payee: PayeeExact, Amount: AmountExact, Date: DateSame}
	frontier := Comparison{Payee: PayeeTruncated, Amount: AmountHigherWithin, Date: DateAfterNear}

	// The well-trodden case is given the lower probability of the two, so that a
	// sampler picking on probability alone would choose it.
	quiet := ConsiderInquiry("trodden", l, wellTrodden, counts, defaultAlpha, 0.94)
	interesting := ConsiderInquiry("frontier", l, frontier, counts, defaultAlpha, 0.97)

	best, ok := MostInformative([]Inquiry{quiet, interesting})
	if !ok {
		t.Fatal("one of the two must be worth asking")
	}
	if best.Key != "frontier" {
		t.Fatalf("the question must go to the levels the installation has no evidence for, "+
			"but it picked %q (gain %.6g against %.6g)", best.Key, quiet.Gain, interesting.Gain)
	}
}

// TestInquiry_picksNothingWhenThereIsNothingToLearn keeps the sampler silent
// rather than asking a question for the sake of having asked one.
func TestInquiry_picksNothingWhenThereIsNothingToLearn(t *testing.T) {
	if _, ok := MostInformative(nil); ok {
		t.Fatal("an empty run must produce no question")
	}
	if _, ok := MostInformative([]Inquiry{
		{Key: "a", Gain: 0}, {Key: "b", Gain: 0},
	}); ok {
		t.Fatal("decisions worth nothing must produce no question")
	}
}

// TestInquiry_theSameRunAsksTheSameQuestion pins the tie-break. Two decisions of
// equal value must not be separated by the order the feed happened to arrive in,
// or the permutation property the import path holds to would not survive
// switching the sampler on.
func TestInquiry_theSameRunAsksTheSameQuestion(t *testing.T) {
	forward := []Inquiry{
		{Key: "b", Gain: 0.5}, {Key: "a", Gain: 0.5}, {Key: "c", Gain: 0.2},
	}
	reversed := []Inquiry{
		{Key: "c", Gain: 0.2}, {Key: "a", Gain: 0.5}, {Key: "b", Gain: 0.5},
	}

	f, _ := MostInformative(forward)
	r, _ := MostInformative(reversed)
	if f.Key != r.Key {
		t.Fatalf("the order of the batch decided the question: %q against %q", f.Key, r.Key)
	}
	if f.Key != "a" {
		t.Fatalf("ties break on the key, so %q was expected, not %q", "a", f.Key)
	}
}

// TestInquiry_theShippedParametersStandInForAZeroValue keeps the method usable
// from a Policy that never set a Linkage, which is every installation that has
// not refitted.
func TestInquiry_theShippedParametersStandInForAZeroValue(t *testing.T) {
	c := Comparison{Payee: PayeeExact, Amount: AmountExact, Date: DateSame}
	zero := Linkage{}.WeightVariance(c, LevelCounts{}, defaultAlpha)
	shipped := DefaultLinkage().WeightVariance(c, LevelCounts{}, defaultAlpha)
	if zero != shipped {
		t.Fatalf("a zero Linkage must mean the shipped parameters, got %.6f against %.6f", zero, shipped)
	}
}

// TestTrigamma_isTheFunctionItClaimsToBe checks ψ′ against its two closed-form
// values and against its own recurrence.
//
// Go has no polygamma, so this is hand-rolled, and every variance in this file
// now runs through it. ψ′(1) = π²/6 and ψ′(½) = π²/2 are the standard anchors;
// the recurrence ψ′(x) = ψ′(x+1) + 1/x² is what the implementation uses to reach
// the regime its asymptotic series is good in, so checking it across the seam is
// checking the part most likely to be wrong.
func TestTrigamma_isTheFunctionItClaimsToBe(t *testing.T) {
	if got, want := trigamma(1), math.Pi*math.Pi/6; math.Abs(got-want) > 1e-9 {
		t.Errorf("ψ′(1) = %.12f, want π²/6 = %.12f", got, want)
	}
	if got, want := trigamma(0.5), math.Pi*math.Pi/2; math.Abs(got-want) > 1e-9 {
		t.Errorf("ψ′(½) = %.12f, want π²/2 = %.12f", got, want)
	}
	// ψ′(2) = π²/6 − 1.
	if got, want := trigamma(2), math.Pi*math.Pi/6-1; math.Abs(got-want) > 1e-9 {
		t.Errorf("ψ′(2) = %.12f, want π²/6 − 1 = %.12f", got, want)
	}

	for _, x := range []float64{0.004, 0.08, 0.5, 1, 2.5, 5.5, 6, 6.5, 12, 100, 5000} {
		got := trigamma(x)
		want := trigamma(x+1) + 1/(x*x)
		if rel := math.Abs(got-want) / want; rel > 1e-12 {
			t.Errorf("the recurrence fails at x=%g: ψ′(x)=%.12g against ψ′(x+1)+1/x²=%.12g",
				x, got, want)
		}
	}

	// Strictly decreasing and positive, which is what makes it usable as a
	// "how little is known" ordering at all.
	prev := math.Inf(1)
	for _, x := range []float64{0.001, 0.01, 0.1, 1, 10, 100, 1000} {
		v := trigamma(x)
		if v <= 0 || v >= prev {
			t.Fatalf("ψ′ is not positive and decreasing: ψ′(%g) = %g against %g", x, v, prev)
		}
		prev = v
	}
}

// TestLevelVariance_isTheExactFormAndNotTheDeltaMethod is a regression guard on
// the defect rather than on the fix.
//
// The delta method understates badly wherever the concentration is below one,
// which is exactly the state the sampler is built to reason about: a level nobody
// has observed, holding only its share of the prior. If someone ever puts the
// approximation back, the ratios below collapse to one and this fails.
func TestLevelVariance_isTheExactFormAndNotTheDeltaMethod(t *testing.T) {
	deltaMethod := func(ak, a0 float64) float64 {
		return (a0 - ak) / (ak * (a0 + 1) * math.Ln2 * math.Ln2)
	}

	for _, tc := range []struct {
		name      string
		ak, a0    float64
		wantRatio float64 // exact / delta-method
		tol       float64
	}{
		{"a level nobody observed, prior mass 0.075", 0.075, 1, 28.8, 0.5},
		{"a level nobody observed, prior mass 0.004", 0.004, 1, 502.0, 5},
		{"a level observed 48 times in 120", 48.4, 121, 1.02, 0.03},
		{"a level observed 250 times in 5000", 250.05, 5001, 1.00, 0.03},
	} {
		exact := (trigamma(tc.ak) - trigamma(tc.a0)) / (math.Ln2 * math.Ln2)
		ratio := exact / deltaMethod(tc.ak, tc.a0)
		t.Logf("%-44s exact %12.4f  delta %10.4f  ratio %8.3f×", tc.name, exact,
			deltaMethod(tc.ak, tc.a0), ratio)
		if math.Abs(ratio-tc.wantRatio) > tc.tol {
			t.Errorf("%s: the exact form is %.1f× the delta method, want about %.1f×",
				tc.name, ratio, tc.wantRatio)
		}
	}

	// And the function itself agrees with the closed form it documents.
	l := DefaultLinkage()
	counts := map[PayeeLevel]int{PayeeExact: 30}
	got := levelVariance(l.PayeeM, counts, 1, PayeeTruncated)
	ak := 1 * l.PayeeM[PayeeTruncated]
	want := (trigamma(ak) - trigamma(1+30)) / (math.Ln2 * math.Ln2)
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("levelVariance gave %.9f, the documented closed form gives %.9f", got, want)
	}
}

// TestInformationGain_matchesTheExpansionWhereTheExpansionIsValid is the other
// half of the argument for replacing it.
//
// The withdrawn second-order form is not wrong, it is out of its regime, and the
// way to show that is to check that the two agree where it holds. At a variance
// of a hundredth of a bit squared they must agree closely; the constant ln2/2 and
// both factors are then confirmed by the integral rather than merely restated.
func TestInformationGain_matchesTheExpansionWhereTheExpansionIsValid(t *testing.T) {
	expansion := func(variance, p float64) float64 {
		return math.Ln2 / 2 * variance * p * (1 - p)
	}
	for _, tc := range []struct{ weight, variance float64 }{
		{0, 0.01}, {0, 0.05}, {2, 0.02}, {-3, 0.01}, {4, 0.005},
	} {
		p := Probability(tc.weight)
		got := InformationGain(tc.variance, p)
		want := expansion(tc.variance, p)
		if rel := math.Abs(got-want) / want; rel > 0.02 {
			t.Errorf("at %+.1f bits with variance %.3f the integral gives %.6g and the "+
				"expansion %.6g (%.1f%% apart) — they must agree in the small-variance "+
				"limit or one of them is wrong", tc.weight, tc.variance, got, want, rel*100)
		}
	}
}

// TestInformationGain_isBoundedByOneBit is the property the expansion did not
// have, and the one that makes the criterion an ordering rather than a hazard.
//
// An answer is one binary label. It cannot carry more than one bit however badly
// the parameters are known, so any expression that returns 8.4 bits — which the
// second-order form did at a variance of a hundred, a variance this model reaches
// with an empty decision log — is not a mutual information.
func TestInformationGain_isBoundedByOneBit(t *testing.T) {
	for _, variance := range []float64{0.01, 1, 50, 100, 330, 5000} {
		for _, weight := range []float64{-14, -9.5, -3, 0, 0.5, 3, 10, 14} {
			got := InformationGain(variance, Probability(weight))
			if got < 0 || got > 1 {
				t.Errorf("variance %.0f at %+.1f bits gave %.6g bits, which is not a "+
					"mutual information between a label and anything", variance, weight, got)
			}
		}
	}

	// And the specific case that used to be reported as worth eight bits.
	if got := InformationGain(100, Probability(0.5)); got > 1 {
		t.Errorf("the case the withdrawn expansion valued at 8.409 bits now gives %.4f", got)
	}
}

// TestInformationGain_prefersTheThinlyEvidencedOverTheMerelyUncertain states the
// behaviour the whole criterion exists for, at this system's actual variances
// rather than at a variance chosen to make it work.
//
// Two decisions the model acted on alone. One sits close to the review band, so
// its answer is nearly a coin toss, but its levels are well evidenced. The other
// is confident, but rests on levels the installation has almost nothing on. BALD
// prefers the second; uncertainty sampling would prefer the first.
func TestInformationGain_prefersTheThinlyEvidencedOverTheMerelyUncertain(t *testing.T) {
	nearTheBand := InformationGain(0.05, 0.91)
	confidentButThin := InformationGain(80, 0.995)

	t.Logf("near the band, well evidenced: %.6f bits; confident but thin: %.6f bits",
		nearTheBand, confidentButThin)

	if !above(confidentButThin, nearTheBand) {
		t.Errorf("a confident decision resting on parameters nothing is known about "+
			"scores %.6f bits, and a well-evidenced one at the band edge %.6f. The "+
			"sampler is asking about answers it was going to be told anyway",
			confidentButThin, nearTheBand)
	}
}
