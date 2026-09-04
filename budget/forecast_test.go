package budget

import (
	"math"
	"math/rand"
	"testing"
)

// TestStudentT_matchesTheDistributionItClaimsToBe checks the CDF against the two
// degrees of freedom where it has a closed form and against published table
// values elsewhere.
//
// The p-value the promotion gate reads comes out of here, so an error in the
// continued fraction would show up as a gate that is too easy or too strict
// without anything else looking wrong.
func TestStudentT_matchesTheDistributionItClaimsToBe(t *testing.T) {
	// df = 1 is the Cauchy distribution, CDF = ½ + atan(t)/π.
	for _, x := range []float64{-3, -1, -0.25, 0, 0.25, 1, 3} {
		want := 0.5 + math.Atan(x)/math.Pi
		if got := studentTCDF(x, 1); math.Abs(got-want) > 1e-10 {
			t.Errorf("df=1 at t=%.2f: got %.12f, Cauchy says %.12f", x, got, want)
		}
	}
	// df = 2 has CDF = ½(1 + t/√(2+t²)).
	for _, x := range []float64{-3, -1, 0, 1, 3} {
		want := 0.5 * (1 + x/math.Sqrt(2+x*x))
		if got := studentTCDF(x, 2); math.Abs(got-want) > 1e-10 {
			t.Errorf("df=2 at t=%.2f: got %.12f, closed form says %.12f", x, got, want)
		}
	}
	// Published critical values, to three decimals as printed in the tables.
	for _, tc := range []struct {
		t, df, want float64
	}{
		{2.228, 10, 0.975},
		{1.812, 10, 0.950},
		{1.697, 30, 0.950},
		{2.042, 30, 0.975},
		{1.960, 100000, 0.975}, // the normal limit
	} {
		got := studentTCDF(tc.t, tc.df)
		if math.Abs(got-tc.want) > 5e-4 {
			t.Errorf("df=%.0f at t=%.3f: got %.5f, the table says %.3f", tc.df, tc.t, got, tc.want)
		}
	}
	if got := studentTCDF(0, 7); math.Abs(got-0.5) > 1e-12 {
		t.Errorf("the median of a symmetric distribution is not %.12f", got)
	}
}

// TestCompareForecasts_holdsItsSizeUnderTheNull is the test the promotion gate
// exists to pass, and the one whose absence let the gate promote on noise.
//
// Both forecasters are the same model up to refit noise, so nothing should ever
// be promotable. A bare comparison of the two mean scores calls the trial better
// about half the time by construction; a test at the five per cent level must
// call it significantly better about five per cent of the time.
func TestCompareForecasts_holdsItsSizeUnderTheNull(t *testing.T) {
	const (
		runs  = 400
		n     = 120
		alpha = 0.05
	)
	rng := rand.New(rand.NewSource(90210))

	var bare, tested int
	for r := 0; r < runs; r++ {
		trial := make([]Observation, n)
		base := make([]Observation, n)
		for i := 0; i < n; i++ {
			w := rng.NormFloat64() * 4
			// The trial is a different posterior, not a better one: same weight
			// disturbed by noise with no systematic direction.
			trial[i] = Observation{Weight: w + rng.NormFloat64()*0.3, Match: rng.Float64() < Probability(w)}
			base[i] = Observation{Weight: w, Match: trial[i].Match}
		}
		bt := Brier(trial, Identity(), 10)
		bb := Brier(base, Identity(), 10)
		if bt.Score < bb.Score {
			bare++
		}
		if dm, ok := CompareForecasts(trial, base, Identity(), Identity()); ok &&
			dm.PValue < alpha && dm.MeanDiff < 0 {
			tested++
		}
	}

	bareRate := float64(bare) / runs
	testedRate := float64(tested) / runs
	t.Logf("bare inequality passed %.1f%% of looks, the test passed %.1f%%",
		100*bareRate, 100*testedRate)

	if testedRate > 3*alpha {
		t.Errorf("a test at the %.0f%% level passed on %.1f%% of null comparisons: it is "+
			"not holding its size, and the gate is back to promoting on noise",
			100*alpha, 100*testedRate)
	}
	if !above(bareRate, 4*testedRate) {
		t.Errorf("the bare inequality passed %.1f%% and the test %.1f%%: if the test is "+
			"not far stricter than the inequality it replaced, it is not doing anything",
			100*bareRate, 100*testedRate)
	}
}

// TestCompareForecasts_findsARealImprovement is the other direction: a test that
// never rejects is not a test, it is a refusal.
func TestCompareForecasts_findsARealImprovement(t *testing.T) {
	const n = 400
	rng := rand.New(rand.NewSource(5150))

	trial := make([]Observation, n)
	base := make([]Observation, n)
	for i := 0; i < n; i++ {
		w := rng.NormFloat64() * 3
		match := rng.Float64() < Probability(w)
		// The incumbent is badly overconfident; the trial is the truth.
		trial[i] = Observation{Weight: w, Match: match}
		base[i] = Observation{Weight: w * 3, Match: match}
	}

	dm, ok := CompareForecasts(trial, base, Identity(), Identity())
	if !ok {
		t.Fatal("a comparison of four hundred differing forecasts reported nothing to test")
	}
	if !below(dm.MeanDiff, 0) {
		t.Fatalf("the better forecaster did not lose less: mean differential %+.6f", dm.MeanDiff)
	}
	if !below(dm.PValue, 0.01) {
		t.Errorf("a threefold overconfidence over four hundred decisions gives p = %.4f "+
			"(statistic %.3f); the test cannot see an improvement this large",
			dm.PValue, dm.Statistic)
	}
}

// TestCompareForecasts_refusesWhatItCannotTest covers the inputs where there is
// no comparison to make, because a false positive here reaches the gate as a
// promotion.
func TestCompareForecasts_refusesWhatItCannotTest(t *testing.T) {
	one := []Observation{{Weight: 1, Match: true}}
	if _, ok := CompareForecasts(one, one, Identity(), Identity()); ok {
		t.Error("a single paired observation has no variance to standardise by")
	}
	if _, ok := CompareForecasts(nil, nil, Identity(), Identity()); ok {
		t.Error("nothing at all is not a comparison")
	}

	two := []Observation{{Weight: 1, Match: true}, {Weight: 2}}
	three := append(append([]Observation(nil), two...), Observation{Weight: 3})
	if _, ok := CompareForecasts(two, three, Identity(), Identity()); ok {
		t.Error("forecasts of different lengths are not paired and must be refused")
	}

	// Identical forecasters: every differential is exactly zero, so there is no
	// variance and nothing to conclude. This must be refused rather than
	// reported as a dead heat with a p-value.
	same := []Observation{{Weight: 1, Match: true}, {Weight: -2}, {Weight: 0.5, Match: true}}
	if _, ok := CompareForecasts(same, same, Identity(), Identity()); ok {
		t.Error("two forecasters that agree everywhere give a degenerate test")
	}
}
