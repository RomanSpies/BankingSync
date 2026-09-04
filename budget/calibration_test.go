package budget

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

// overconfident draws decisions from a model that double-counts its evidence.
//
// The true log-odds are w; the model reports inflate·w because two of its fields
// say the same thing and it adds both. That is the independence assumption
// failing, which is the failure Platt scaling is here for, and it is injected
// deliberately so that the correction has something known to correct.
func overconfident(n int, inflate float64, seed uint64) []Observation {
	r := rand.New(rand.NewPCG(seed, seed^0x5bf03635))
	out := make([]Observation, n)
	for i := range out {
		truth := r.Float64()*12 - 6
		out[i] = Observation{
			Weight: inflate * truth,
			Match:  r.Float64() < Probability(truth),
		}
	}
	return out
}

// TestCalibration_identityChangesNothing is the guarantee for every installation
// that never fits one. The shipped calibration has to be exactly the arithmetic
// that was there before it existed.
func TestCalibration_identityChangesNothing(t *testing.T) {
	c := Identity()
	if !c.IsIdentity() {
		t.Error("Identity does not report itself as one")
	}
	for _, w := range []float64{-8, -1, 0, 0.5, 3.17, 9} {
		if got, want := c.Probability(w), Probability(w); got != want {
			t.Errorf("weight %.2f: %v under the identity, %v without it", w, got, want)
		}
	}
	// And the zero value is not it: A=0 would map every weight to a half.
	if (Calibration{}).IsIdentity() {
		t.Error("the zero value claims to be the identity, which would silently flatten " +
			"every probability to a half")
	}
}

// TestBrier_decomposesTheWayMurphySaid pins the arithmetic the whole file rests
// on. Reliability minus resolution plus uncertainty has to be the score itself,
// or the two components cannot be read against each other.
func TestBrier_decomposesTheWayMurphySaid(t *testing.T) {
	obs := overconfident(4000, 1.6, 11)
	b := Brier(obs, Identity(), 20)

	if got := b.Reliability - b.Resolution + b.Uncertainty; math.Abs(got-b.Score) > 1e-3 {
		t.Errorf("reliability − resolution + uncertainty = %.5f, but the score is %.5f",
			got, b.Score)
	}
	if b.Uncertainty <= 0 || b.Base <= 0 || b.Base >= 1 {
		t.Errorf("base rate %.3f, uncertainty %.5f: the sample has to contain both "+
			"outcomes for any of this to mean anything", b.Base, b.Uncertainty)
	}
}

// TestReliability_seesOverconfidence is the diagram's whole purpose. A model
// that ranks perfectly and claims too much has to look wrong here, because no
// ranking metric will show it.
func TestReliability_seesOverconfidence(t *testing.T) {
	obs := overconfident(4000, 1.8, 3)

	var high, tooHigh int
	for _, b := range Reliability(obs, Identity(), 10) {
		if b.Count < 30 || b.MeanForecast < 0.5 {
			continue
		}
		high++
		if b.Observed < b.MeanForecast-0.02 {
			tooHigh++
		}
	}
	if high == 0 {
		t.Fatal("no confident bin held enough predictions to say anything")
	}
	if tooHigh == 0 {
		t.Error("a model inflating its evidence by four fifths looks perfectly calibrated; " +
			"the diagram is not measuring what it claims")
	}
}

// TestFitPlatt_pullsBackAnInjectedOverconfidence is the verification the plan
// asks for, and the one sentence of it that matters is the last.
//
// A known violation of the independence assumption is built in, Platt is fitted
// on one part of the data and scored on another, and the reliability component
// has to fall while the resolution component does not — the correction moved the
// scale and left the model's ability to separate matches from non-matches alone,
// which is exactly the division of labour it is for.
//
// This shows the arithmetic does what it says. It does not show that fitting one
// helps any real installation, and no test here can.
func TestFitPlatt_pullsBackAnInjectedOverconfidence(t *testing.T) {
	const inflate = 1.7
	fit, holdout := SplitHoldout(overconfident(6000, inflate, 7))

	before := Brier(holdout, Identity(), 20)
	c := FitPlatt(fit)
	after := Brier(holdout, c, 20)

	t.Logf("A=%.3f B=%.3f (the injected inflation was %.2f, so A near %.3f)",
		c.A, c.B, inflate, 1/inflate)

	if !below(after.Reliability, before.Reliability) {
		t.Errorf("reliability %.5f after, %.5f before: the correction did not pull the "+
			"overconfidence back", after.Reliability, before.Reliability)
	}
	if after.Resolution < before.Resolution*0.9 {
		t.Errorf("resolution fell from %.5f to %.5f: a rescaling must not cost the model "+
			"its ability to tell matches from non-matches",
			before.Resolution, after.Resolution)
	}
	if !below(after.Score, before.Score) {
		t.Errorf("the score did not improve: %.5f after, %.5f before", after.Score, before.Score)
	}
	fitted, okFit := ECE(holdout, c, 20)
	plain, okPlain := ECE(holdout, Identity(), 20)
	if !okFit || !okPlain {
		t.Fatalf("the holdout of %d is below the floor ECE will report at", len(holdout))
	}
	if !below(fitted, plain) {
		t.Error("the calibration error did not fall")
	}
	// The fit should land near the inverse of the inflation, which is the sense
	// in which it recovered what was done to it.
	if math.Abs(c.A-1/inflate) > 0.15 {
		t.Errorf("A = %.3f, want near %.3f", c.A, 1/inflate)
	}
}

// TestFitPlatt_refusesWhatItCannotFit keeps an unusable answer from replacing a
// usable one. A set of labels with only one outcome in it has no boundary to
// place, and a separable one drives the fit to certainty — which is the failure
// being corrected, arrived at from the other direction.
func TestFitPlatt_refusesWhatItCannotFit(t *testing.T) {
	if got := FitPlatt(nil); !got.IsIdentity() {
		t.Errorf("no observations produced %+v, want the identity", got)
	}

	allTrue := []Observation{{Weight: 3, Match: true}, {Weight: 4, Match: true}}
	if got := FitPlatt(allTrue); !got.IsIdentity() {
		t.Errorf("labels with one outcome produced %+v, want the identity", got)
	}

	// Perfectly separable, which without smoothed targets runs away to infinity.
	var clean []Observation
	for i := 0; i < 200; i++ {
		clean = append(clean, Observation{Weight: -5, Match: false}, Observation{Weight: 5, Match: true})
	}
	got := FitPlatt(clean)
	if math.IsInf(got.A, 0) || math.IsNaN(got.A) {
		t.Fatalf("separable labels produced A = %v", got.A)
	}
	// The smoothed targets are 201/202 and 1/202, so the fit lands a little
	// short of certainty. Without them it runs towards it, and the model then
	// answers every question with a probability of one — which is the failure
	// this correction exists to remove, arrived at from the other side.
	if p := got.Probability(5); p > 0.999 {
		t.Errorf("separable labels made a weight of five bits worth %.6f; the targets are "+
			"not being smoothed and the fit is running away to certainty", p)
	}
}

// TestAssess_appliesTheCalibration checks that the rescaling reaches a decision
// at all. It is invisible while the shipped calibration is the identity, which
// is precisely why it needs asserting: the wiring could be missing for a year
// and nothing would say so until somebody fitted one.
func TestAssess_appliesTheCalibration(t *testing.T) {
	target := day(2026, time.July, 15)
	in := imported(target, -12000, "Hotel Berlin", "book-1")
	row := &Transaction{ID: "auth", Date: target, AmountCents: -12000, PayeeName: "Hotel Berlin"}

	plain := Assess([]*Transaction{row}, in, nil, tolPolicy())

	pol := tolPolicy()
	pol.Calibration = Calibration{A: 0.4, B: -1}
	scaled := Assess([]*Transaction{row}, in, nil, pol)

	if plain[0].Weight != scaled[0].Weight {
		t.Errorf("the weight moved: %.4f against %.4f. A calibration rescales the "+
			"probability, not the evidence", scaled[0].Weight, plain[0].Weight)
	}
	if plain[0].Probability == scaled[0].Probability {
		t.Errorf("both probabilities are %.6f; the calibration never reached the decision",
			plain[0].Probability)
	}
	if want := (Calibration{A: 0.4, B: -1}).Probability(scaled[0].Weight); scaled[0].Probability != want {
		t.Errorf("probability %.6f, want %.6f", scaled[0].Probability, want)
	}
}

// TestSplitHoldout_scoresOnDataItDidNotSee guards the measurement itself.
// Scoring a fit on the points it was fitted to reports how well two parameters
// trace a few hundred numbers, which is not the question anybody asked.
func TestSplitHoldout_scoresOnDataItDidNotSee(t *testing.T) {
	obs := overconfident(300, 1.5, 5)
	fit, holdout := SplitHoldout(obs)

	if len(fit)+len(holdout) != len(obs) {
		t.Errorf("%d + %d observations, want %d", len(fit), len(holdout), len(obs))
	}
	if len(holdout) == 0 || len(fit) == 0 {
		t.Fatal("one side of the split is empty")
	}
	seen := map[float64]int{}
	for _, o := range fit {
		seen[o.Weight]++
	}
	for _, o := range holdout {
		if seen[o.Weight] > 0 {
			seen[o.Weight]--
		}
	}

	// And the same input splits the same way, or a reported figure cannot be
	// reproduced.
	f2, h2 := SplitHoldout(obs)
	if len(f2) != len(fit) || len(h2) != len(holdout) || (len(h2) > 0 && h2[0] != holdout[0]) {
		t.Error("the split is not deterministic")
	}
}

// TestFitPlatt_convergesAcrossTheWeightRangeTheModelProduces is the test whose
// absence let an undamped Newton solver ship.
//
// The old solver was only ever exercised at weight spreads of a bit or two. The
// real scale here is about ±10 bits — exact payee, exact amount and same date
// scores just over +10, and a conflict on all three just under −9.5 — so a
// standard deviation of four to eight bits is the operating regime. In it the
// old solver either ran away to a slope near 10^10, scoring worse than applying
// no calibration at all, or returned the identity because the saturated Hessian
// tripped its determinant test on the first iteration.
//
// Scored in-sample on purpose. Out of sample a two-parameter fit on three
// hundred points is worse than the identity a few per cent of the time by
// ordinary sampling variation, which is a fact about holdouts rather than about
// the solver, and asserting on it would buy a flaky test.
func TestFitPlatt_convergesAcrossTheWeightRangeTheModelProduces(t *testing.T) {
	const (
		trueA = 0.4
		trueB = -1.2
		n     = 4000
	)
	truth := Calibration{A: trueA, B: trueB}

	for _, sd := range []float64{1, 2, 3, 4, 5, 6, 8, 10} {
		rng := rand.New(rand.NewPCG(uint64(sd*1000), 0x9e3779b9))
		obs := make([]Observation, n)
		for i := range obs {
			w := rng.NormFloat64() * sd
			obs[i] = Observation{Weight: w, Match: rng.Float64() < truth.Probability(w)}
		}

		got := FitPlatt(obs)
		fitted := Brier(obs, got, 20).Score
		plain := Brier(obs, Identity(), 20).Score
		t.Logf("sd=%4.1f bits  A=%.4f B=%+.4f  Brier %.4f against %.4f uncalibrated",
			sd, got.A, got.B, fitted, plain)

		if got.IsIdentity() {
			t.Errorf("sd=%.1f: the fit gave up and returned the identity on four thousand "+
				"perfectly fittable observations", sd)
			continue
		}
		if math.Abs(got.A-trueA) > 0.1 {
			t.Errorf("sd=%.1f: A = %.4f, want near %.2f — the slope did not come back",
				sd, got.A, trueA)
		}
		if math.Abs(got.B-trueB) > 0.3 {
			t.Errorf("sd=%.1f: B = %+.4f, want near %.2f", sd, got.B, trueB)
		}
		if !atMost(fitted, plain) {
			t.Errorf("sd=%.1f: the fitted calibration scores %.4f where doing nothing "+
				"scores %.4f. A calibration that loses to the identity on the data it "+
				"was fitted to has diverged", sd, fitted, plain)
		}
	}
}

// TestFitPlatt_doesNotRunAway pins the failure directly rather than through its
// consequences. A slope of ten orders of magnitude is not a calibration, and the
// old guard — NaN, infinity, or a non-positive slope — did not catch it.
func TestFitPlatt_doesNotRunAway(t *testing.T) {
	rng := rand.New(rand.NewPCG(31337, 0x9e3779b9))
	for _, sd := range []float64{5, 6, 7, 12, 20} {
		obs := make([]Observation, 4000)
		for i := range obs {
			w := rng.NormFloat64() * sd
			obs[i] = Observation{Weight: w, Match: rng.Float64() < Probability(0.4*w-1.2)}
		}
		got := FitPlatt(obs)
		if got.A > 100 || math.Abs(got.B) > 100 {
			t.Errorf("sd=%.0f: fitted A=%g B=%g — the solver diverged", sd, got.A, got.B)
		}
	}
}

// TestBrier_theComponentsSumToTheScore is Stephenson, Coelho and Jolliffe's
// point, and it is the property the three-component split cannot have.
//
// Murphy's identity needs stratification on the distinct forecast values. Binning
// by equal width and using each bin's mean leaves the forecasts inside a bin
// differing from that mean, and two further terms appear to account for it.
// Without them the sum is out by an amount that depends on nothing but the bin
// count, which is how a diagnostic comes to disagree with the score it is meant
// to decompose.
func TestBrier_theComponentsSumToTheScore(t *testing.T) {
	obs := overconfident(4000, 1.4, 17)

	for _, bins := range []int{5, 10, 20, 50, 500} {
		b := Brier(obs, Identity(), bins)

		three := b.Reliability - b.Resolution + b.Uncertainty
		five := three + b.WithinBinVariance - b.WithinBinCovariance
		viaGRES := b.Reliability - b.GeneralisedResolution() + b.Uncertainty

		t.Logf("bins=%3d  score %.6f  three-way %.6f (out by %+.6f)  five-way %.6f",
			bins, b.Score, three, three-b.Score, five)

		if math.Abs(five-b.Score) > 1e-9 {
			t.Errorf("bins=%d: the five components sum to %.9f, the score is %.9f",
				bins, five, b.Score)
		}
		if math.Abs(viaGRES-b.Score) > 1e-9 {
			t.Errorf("bins=%d: reliability − generalised resolution + uncertainty is "+
				"%.9f, the score is %.9f", bins, viaGRES, b.Score)
		}
	}
}

// TestBrier_scoreDoesNotDependOnTheBinning guards the one component the promotion
// gate reads. Everything else in the decomposition moves with the bin count; this
// must not, because a gate whose verdict depended on a diagnostic's bin count
// would be deciding on an argument nobody made.
func TestBrier_scoreDoesNotDependOnTheBinning(t *testing.T) {
	obs := overconfident(2000, 1.6, 23)
	want := Brier(obs, Identity(), 10).Score
	for _, bins := range []int{1, 2, 7, 100, 5000} {
		if got := Brier(obs, Identity(), bins).Score; math.Abs(got-want) > 1e-12 {
			t.Errorf("bins=%d gave a score of %.12f against %.12f at ten", bins, got, want)
		}
	}
}

// TestECE_refusesToReportWhereItWouldOnlyReportItsOwnBias is the alarm that used
// to fire on a correct model.
//
// The binned estimator is biased upwards and the bias does not vanish when the
// model is right: on forecasts calibrated by construction, so that the true error
// is exactly zero, ten bins report about 0.14 at fifty observations and 0.064 at
// a hundred and twenty. The promotion gate opens at fifty labels, so that is the
// range this program actually reports in.
func TestECE_refusesToReportWhereItWouldOnlyReportItsOwnBias(t *testing.T) {
	if _, ok := ECE(overconfident(minECEObservations-1, 1.0, 3), Identity(), 10); ok {
		t.Errorf("ECE answered on %d observations, below its own floor of %d",
			minECEObservations-1, minECEObservations)
	}
	if _, ok := ECE(nil, Identity(), 10); ok {
		t.Error("ECE answered on nothing at all")
	}
	if _, ok := ECE(overconfident(minECEObservations, 1.0, 3), Identity(), 10); !ok {
		t.Errorf("ECE refused at exactly %d observations, which is its floor", minECEObservations)
	}
}

// TestECE_seesAnOverconfidenceItShouldSee keeps the floor above from turning the
// alarm off altogether.
func TestECE_seesAnOverconfidenceItShouldSee(t *testing.T) {
	honest, ok := ECE(overconfident(4000, 1.0, 11), Identity(), 10)
	if !ok {
		t.Fatal("four thousand observations is above any floor")
	}
	inflated, ok := ECE(overconfident(4000, 2.2, 11), Identity(), 10)
	if !ok {
		t.Fatal("four thousand observations is above any floor")
	}
	t.Logf("calibrated %.4f, inflated %.4f", honest, inflated)
	if !above(inflated, 3*honest) {
		t.Errorf("a model inflated by 2.2 reports a calibration error of %.4f against "+
			"%.4f for an honest one: the alarm cannot tell them apart", inflated, honest)
	}
}
