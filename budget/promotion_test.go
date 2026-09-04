package budget

import (
	"math"
	"math/rand"
	"testing"
)

// TestAnchors_theShippedParametersPassTheirOwnGate is the first thing that has to
// be true. An anchor set the shipped parameters fail is a set that documents
// something the program does not do.
func TestAnchors_theShippedParametersPassTheirOwnGate(t *testing.T) {
	pol := Policy{}
	for _, a := range Anchors() {
		if got := a.decide(pol); got != a.Want {
			t.Errorf("%s: %s, want %s — %s", a.Name, got, a.Want, a.Why)
		}
	}
}

// TestAnchors_noneOfThemBalancesOnAThreshold is what keeps the gate meaningful.
//
// An anchor a hair from its threshold fails on rounding and says nothing about
// whether the parameters are still sound. Cases that sit that close are the
// subject of the sensitivity table, which counts them on purpose; here they
// would only produce a gate nobody could ever pass.
func TestAnchors_noneOfThemBalancesOnAThreshold(t *testing.T) {
	const clearance = 0.04
	pol := Policy{}
	for _, a := range Anchors() {
		p := Probability(pol.linkage().Weight(a.Comparison, a.Candidates, defaultOverlap))
		for name, threshold := range map[string]float64{
			"the automatic threshold": pol.autoProbability(),
			"the review threshold":    pol.reviewProbability(),
		} {
			if gap := math.Abs(p - threshold); gap < clearance {
				t.Errorf("%s sits %.3f from %s (at %.3f) — too close to be an anchor",
					a.Name, gap, name, p)
			}
		}
	}
}

// TestAnchors_everyOutcomeIsRepresented keeps the gate from checking one half of
// the behaviour. A set of anchors that only ever expects a merge would pass any
// parameters that merge everything.
func TestAnchors_everyOutcomeIsRepresented(t *testing.T) {
	seen := map[string]int{}
	for _, a := range Anchors() {
		seen[a.Want]++
	}
	for _, want := range []string{"adopted", "held", "created"} {
		if seen[want] == 0 {
			t.Errorf("no anchor expects %q, so parameters that never produce it would pass", want)
		}
	}
}

// TestPromotion_parametersThatBreakAPromiseAreRefused is the gate doing its job.
// A candidate set that stops recognising a truncating bank's own settlements has
// changed what the program is for, and no score it achieves elsewhere buys that
// back.
func TestPromotion_parametersThatBreakAPromiseAreRefused(t *testing.T) {
	pol := Policy{}
	broken := DefaultLinkage()
	broken.PayeeM = copyPayee(broken.PayeeM)
	broken.PayeeM[PayeeTruncated] = 0.0001

	v := Verdict{}
	v.Checks = append(v.Checks, checkAnchors(pol, Trial{Linkage: broken, Calibration: Identity()}))
	if v.Promotable() {
		t.Fatal("a parameter set that stops recognising truncated payees was accepted")
	}
	if v.Checks[0].Status != CheckFailed {
		t.Fatalf("the anchor check reported %q", v.Checks[0].Status)
	}
	if !contains(v.Checks[0].Detail, "the truncating bank") {
		t.Errorf("the failure does not name the promise it broke: %q", v.Checks[0].Detail)
	}
}

// TestPromotion_isUndecidableBeforeThereIsEvidence keeps an installation without
// labels from installing a refit of nothing.
//
// Undecidable rather than refused, and reported as such: "we cannot tell whether
// this is better" is a different statement from "this is worse", and it is the
// state most installations are permanently in.
func TestPromotion_isUndecidableBeforeThereIsEvidence(t *testing.T) {
	v := EvaluateTrial(Policy{}, syntheticLabels(t, 20, 1), LevelCounts{}, defaultAlpha, 0, 0)
	if v.Promotable() {
		t.Fatal("a trial judged on twenty decisions was promotable")
	}
	var found bool
	for _, c := range v.Checks {
		if c.Name == "calibration" && c.Status == CheckUnavailable {
			found = true
		}
	}
	if !found {
		t.Errorf("the shortage was not reported as undecidable: %+v", v.Checks)
	}
}

// TestPromotion_theCalibrationCheckCanActuallyFail covers the gate in both
// directions, which is what a gate needs and what this one lacked.
//
// Every existing test either asserted CheckPassed or CheckUnavailable, so the
// comparison could be deleted outright — replaced with an unconditional pass —
// and the whole suite stayed green. A gate nothing can prove closed is not a
// gate.
//
// The failing case is the ordinary one, not a contrived one: when the parameters
// in force already describe the data, a refit on two thirds of it can only add
// estimation noise, and changing working parameters for a worse score on
// evidence they were not fitted to is exactly what must be refused.
func TestPromotion_theCalibrationCheckCanActuallyFail(t *testing.T) {
	calibration := func(v Verdict) Check {
		t.Helper()
		for _, c := range v.Checks {
			if c.Name == "calibration" {
				return c
			}
		}
		t.Fatalf("no calibration check among %+v", v.Checks)
		return Check{}
	}

	// Labels drawn against the model in force. The refit has nothing to find.
	settled := EvaluateTrial(Policy{}, syntheticLabels(t, 300, 1), LevelCounts{}, defaultAlpha, 0, 0)
	if c := calibration(settled); c.Status != CheckFailed {
		t.Errorf("a candidate that scores worse on held-back evidence reported %q (%s); "+
			"the check must be able to close", c.Status, c.Detail)
	}
	if settled.Promotable() {
		t.Error("a candidate that lost the Brier comparison was promotable anyway")
	}

	// And the other direction, so that a check wired shut fails here instead.
	// Labels that contradict the parameters in force: the refit has a great deal
	// to find and the candidate genuinely is better.
	wrong := syntheticLabels(t, 300, 1)
	for i := range wrong {
		wrong[i].Match = !wrong[i].Match
	}
	improved := EvaluateTrial(Policy{}, wrong, LevelCounts{}, defaultAlpha, 0, 0)
	if c := calibration(improved); c.Status != CheckPassed {
		t.Errorf("a candidate fitted to labels that contradict the parameters in force "+
			"reported %q (%s); the check must be able to open", c.Status, c.Detail)
	}
}

// TestPromotion_judgesOnEvidenceItDidNotFitTo is the check that makes the Brier
// comparison mean anything.
//
// A refit scored on the decisions it was fitted to flatters any procedure
// whatever. The gate therefore fits on part and judges on the rest, and this
// pins that it really does: told to fit on everything, the same data would
// produce a better-looking figure than the gate reports.
func TestPromotion_judgesOnEvidenceItDidNotFitTo(t *testing.T) {
	in := syntheticLabels(t, 600, 7)
	base := DefaultLinkage()

	fit, holdout := SplitLabelled(base, in, defaultOverlap)
	if len(holdout) == 0 || len(fit) == 0 {
		t.Fatalf("the split produced %d and %d", len(fit), len(holdout))
	}
	if len(fit)+len(holdout) != len(in) {
		t.Fatalf("the split lost %d decisions", len(in)-len(fit)-len(holdout))
	}

	honest := brierOf(base, ProposeTrial(base, fit, LevelCounts{}, defaultAlpha, defaultOverlap), holdout)
	flattering := brierOf(base, ProposeTrial(base, in, LevelCounts{}, defaultAlpha, defaultOverlap), holdout)
	if !atMost(flattering, honest) {
		t.Fatalf("fitting on the judged data did not flatter the score (%.5f against %.5f), "+
			"so this test is not measuring what it claims", flattering, honest)
	}

	// And the gate reports the honest figure, not the flattering one. Without
	// this the split above could be perfectly correct and never used.
	got := EvaluateTrial(Policy{}, in, LevelCounts{}, defaultAlpha, 0, 0)
	if math.Abs(got.Trial.Score-honest) > 1e-9 {
		t.Errorf("the gate reported a Brier of %.6f; fitting on the held-out part gives "+
			"%.6f and fitting on everything gives %.6f", got.Trial.Score, honest, flattering)
	}
}

// TestPromotion_theSameEvidenceGivesTheSameVerdict keeps promotion from becoming
// a matter of asking until the answer is yes.
func TestPromotion_theSameEvidenceGivesTheSameVerdict(t *testing.T) {
	in := syntheticLabels(t, 400, 3)
	shuffled := append([]LabelledDecision(nil), in...)
	rand.New(rand.NewSource(99)).Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})

	a := EvaluateTrial(Policy{}, in, LevelCounts{}, defaultAlpha, 3, 100)
	b := EvaluateTrial(Policy{}, shuffled, LevelCounts{}, defaultAlpha, 3, 100)

	if a.Version != b.Version {
		t.Errorf("the order of the evidence changed the parameters: %s against %s", a.Version, b.Version)
	}
	if a.Promotable() != b.Promotable() {
		t.Errorf("the order of the evidence changed the verdict: %v against %v",
			a.Promotable(), b.Promotable())
	}
	for i := range a.Checks {
		if a.Checks[i].Detail != b.Checks[i].Detail {
			t.Errorf("check %q read differently: %q against %q",
				a.Checks[i].Name, a.Checks[i].Detail, b.Checks[i].Detail)
		}
	}
}

// TestPromotion_aChangeNobodyLookedAtIsNotAPass records the one finding the
// program refuses to adjudicate. How many decisions a change would have altered
// is a fact; whether that many is acceptable is a judgement, and it is put in
// front of a person rather than compared against a number somebody invented.
func TestPromotion_aChangeNobodyLookedAtIsNotAPass(t *testing.T) {
	v := EvaluateTrial(Policy{}, syntheticLabels(t, 400, 5), LevelCounts{}, defaultAlpha, 91, 100)
	for _, c := range v.Checks {
		if c.Name != "changed decisions" {
			continue
		}
		if c.Status != CheckForAPerson {
			t.Errorf("the program judged a 91%% change on its own: %q", c.Status)
		}
		if !contains(c.Detail, "91") {
			t.Errorf("the size of the change is not stated: %q", c.Detail)
		}
		return
	}
	t.Fatal("no finding about changed decisions was reported at all")
}

// TestPromotion_aTrialChangesTheParameterVersion is what makes a promotion
// visible on a dashboard rather than only in a database.
func TestPromotion_aTrialChangesTheParameterVersion(t *testing.T) {
	pol := Policy{}
	v := EvaluateTrial(pol, syntheticLabels(t, 400, 11), LevelCounts{}, defaultAlpha, 0, 0)
	if v.Version == pol.Version() {
		t.Error("a refit of four hundred decisions produced the version already in force")
	}
}

// TestPromotion_countsMandUSeparately pins the direction of the tally. Folding a
// non-match into the m side would teach the model that disagreement is evidence
// of a match.
func TestPromotion_countsMandUSeparately(t *testing.T) {
	got := CountLevels([]LabelledDecision{
		{Comparison: Comparison{Payee: PayeeExact, Amount: AmountExact, Date: DateSame}, Match: true},
		{Comparison: Comparison{Payee: PayeeConflict, Amount: AmountExact, Date: DateSame}, Match: false},
	})
	if got.PayeeM[PayeeExact] != 1 || got.PayeeU[PayeeExact] != 0 {
		t.Errorf("a match landed as m=%d u=%d", got.PayeeM[PayeeExact], got.PayeeU[PayeeExact])
	}
	if got.PayeeU[PayeeConflict] != 1 || got.PayeeM[PayeeConflict] != 0 {
		t.Errorf("a non-match landed as m=%d u=%d",
			got.PayeeM[PayeeConflict], got.PayeeU[PayeeConflict])
	}
}

func brierOf(base Linkage, t Trial, holdout []LabelledDecision) float64 {
	obs := make([]Observation, len(holdout))
	for i, d := range holdout {
		obs[i] = score(t.Linkage, t.Calibration, d, defaultOverlap)
	}
	return Brier(obs, Identity(), 10).Score
}

func copyPayee(in map[PayeeLevel]float64) map[PayeeLevel]float64 {
	out := make(map[PayeeLevel]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// syntheticLabels builds settled decisions whose levels agree with the shipped
// priors, so that a refit of them is a mild adjustment rather than a different
// model. It is a source of well-formed input, not a source of truth: nothing
// here measures whether the matcher is any good.
func syntheticLabels(t *testing.T, n int, seed int64) []LabelledDecision {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	payees := []PayeeLevel{PayeeExact, PayeeTruncated, PayeeSubset, PayeeConflict, PayeeNone, PayeeMissing}
	amounts := []AmountLevel{AmountExact, AmountHigherWithin, AmountLowerWithin, AmountOutsideHigher}
	dates := []DateLevel{DateSame, DateAfterNear, DateBeforeNear, DateAfterFar}

	l := DefaultLinkage()
	out := make([]LabelledDecision, n)
	for i := range out {
		c := Comparison{
			Payee:  payees[rng.Intn(len(payees))],
			Amount: amounts[rng.Intn(len(amounts))],
			Date:   dates[rng.Intn(len(dates))],
		}
		cands := 1 + rng.Intn(4)
		// The label follows the model's own probability, which is what makes the
		// refit a mild adjustment: a corpus drawn against the model would refit
		// into something else entirely and prove nothing about the machinery.
		out[i] = LabelledDecision{
			Comparison: c, Candidates: cands,
			Match: rng.Float64() < Probability(l.Weight(c, cands, defaultOverlap)),
		}
	}
	return out
}

// TestPromotion_refittingDoesNotCompound is the property that keeps repeated
// promotions from walking the parameters away from their priors.
//
// A Dirichlet posterior mixes a prior with observations. If the prior it starts
// from is the previous posterior, the same observations are folded in again on
// every round, and the levels drift further each time with nothing new having
// been seen. So the prior is always the shipped set, whatever is in force.
func TestPromotion_refittingDoesNotCompound(t *testing.T) {
	in := syntheticLabels(t, 500, 31)

	first, _ := CandidateTrial(FittingPrior(), in, LevelCounts{}, defaultAlpha, defaultOverlap)
	inForce := Policy{Linkage: first.Linkage, Calibration: first.Calibration}

	// The same evidence, evaluated again against a policy already running the
	// result of it, must propose exactly what it proposed the first time.
	second := EvaluateTrial(inForce, in, LevelCounts{}, defaultAlpha, 0, 0)
	if got, want := second.Version, first.Version(inForce); got != want {
		t.Fatalf("refitting the same evidence a second time moved the parameters: "+
			"%s became %s", want, got)
	}
}

// TestPromotion_theBaselineIsWhatIsInForce keeps the other half of that
// distinction. The prior a refit starts from is the shipped set; the score it
// has to beat belongs to whatever is actually running.
func TestPromotion_theBaselineIsWhatIsInForce(t *testing.T) {
	in := syntheticLabels(t, 500, 37)

	shipped := EvaluateTrial(Policy{}, in, LevelCounts{}, defaultAlpha, 0, 0)

	fitted := ProposeTrial(FittingPrior(), in, LevelCounts{}, defaultAlpha, defaultOverlap)
	already := EvaluateTrial(Policy{Linkage: fitted.Linkage, Calibration: fitted.Calibration},
		in, LevelCounts{}, defaultAlpha, 0, 0)

	if shipped.Base.Score == already.Base.Score {
		t.Fatal("the baseline did not move when the parameters in force did, so it is " +
			"not being taken from them")
	}
	// And an installation already running the candidate has nothing to gain.
	if already.Promotable() {
		t.Error("a candidate identical to what is already in force was promotable")
	}
}

// TestPromotionLooks_countsDoublingsOfTheCorpus pins the multiplicity clock.
//
// One look until the corpus is worth judging at all, then one more per doubling.
// The exact schedule is a judgement rather than a theorem, so it is pinned here
// where a change to it has to be deliberate.
func TestPromotionLooks_countsDoublingsOfTheCorpus(t *testing.T) {
	for _, tc := range []struct {
		labels, want int
	}{
		{0, 1}, {49, 1}, {50, 1}, {51, 1},
		{99, 1}, {100, 2}, {150, 2},
		{200, 3}, {399, 3}, {400, 4},
		// Capped: a mature corpus must not face a stricter bar than a new one.
		{800, 4}, {1600, 4}, {100000, 4},
	} {
		if got := promotionLooks(tc.labels); got != tc.want {
			t.Errorf("at %d labels the gate has had %d looks, want %d", tc.labels, got, tc.want)
		}
	}
	// Monotone, or an operator could lose a correction by gaining a label.
	prev := 0
	for n := 0; n < 5000; n += 7 {
		got := promotionLooks(n)
		if got < prev {
			t.Fatalf("the look count fell from %d to %d at %d labels", prev, got, n)
		}
		prev = got
	}
}

// TestPromotion_doesNotOpenOnAGrowingCorpusOfNothing is finding 3 of the
// statistical review, turned into the test that would have caught it.
//
// The parameters in force are the ones the labels were drawn from, so no
// candidate is ever genuinely better and the calibration check should never
// open. An operator watches the page as the corpus grows. Measured against the
// gate as it stood — a bare inequality between two Brier means — this opened on
// 44% of single looks and 96% of eight. The bare inequality is still reported
// below for contrast, because a test that only shows the fix passing does not
// show that it was needed.
func TestPromotion_doesNotOpenOnAGrowingCorpusOfNothing(t *testing.T) {
	calibration := func(v Verdict) Check {
		t.Helper()
		for _, c := range v.Checks {
			if c.Name == "calibration" {
				return c
			}
		}
		t.Fatalf("no calibration check among %+v", v.Checks)
		return Check{}
	}

	const runs = 40
	sizes := []int{60, 90, 130, 190, 280, 410, 600, 880}

	var everOpened, everWouldHave, singleOpened, singleWouldHave int
	for seed := int64(1); seed <= runs; seed++ {
		opened, wouldHave := false, false
		for i, n := range sizes {
			v := EvaluateTrial(Policy{}, syntheticLabels(t, n, seed), LevelCounts{},
				defaultAlpha, 0, 0)
			passed := calibration(v).Status == CheckPassed
			// What the withdrawn rule would have said: any improvement at all.
			bare := v.Trial.Count > 0 && v.Trial.Score < v.Base.Score

			if i == 0 {
				if passed {
					singleOpened++
				}
				if bare {
					singleWouldHave++
				}
			}
			opened = opened || passed
			wouldHave = wouldHave || bare
		}
		if opened {
			everOpened++
		}
		if wouldHave {
			everWouldHave++
		}
	}

	t.Logf("one look:    tested %d/%d (%.0f%%), bare inequality %d/%d (%.0f%%)",
		singleOpened, runs, 100*float64(singleOpened)/runs,
		singleWouldHave, runs, 100*float64(singleWouldHave)/runs)
	t.Logf("eight looks: tested %d/%d (%.0f%%), bare inequality %d/%d (%.0f%%)",
		everOpened, runs, 100*float64(everOpened)/runs,
		everWouldHave, runs, 100*float64(everWouldHave)/runs)

	if everOpened > runs/5 {
		t.Errorf("the gate opened at some point in %d of %d runs where the parameters in "+
			"force were already the truth. That is optional stopping and it is what the "+
			"significance level and its multiplicity correction are for", everOpened, runs)
	}
	if everWouldHave <= everOpened {
		t.Errorf("the bare inequality opened %d times and the tested gate %d: if the test "+
			"is not stricter than what it replaced then this test is measuring nothing",
			everWouldHave, everOpened)
	}
}

// TestAnchors_catchTheTruncationTheyAreNamedFor covers both halves of the worst
// failure this model has, because each half is guarded by a different change and
// either one alone would leave it reachable.
//
// The scenario: an installation labels a few score of decisions and none of them
// happens to involve a truncated payee — not unlikely, since the review band a
// person is asked about excludes the high-agreement pairs where truncation lives,
// and at thirty labels a truncating bank produces none at all 8% of the time. The
// u side meanwhile sees truncation constantly, because two unrelated rows from the
// same truncating merchant reach that level as readily as a real pair does. So one
// side of the ratio is pinned by thousands of samples and the other is silent.
//
// Left alone that collapses the level, and the bank that cuts its payee field
// stops being recognised: its settlements are created as duplicates, with nobody
// asked and nothing logged as wrong.
//
// The first half is that Refit no longer does it. The second is that the anchors
// would still see it if something else did — a guard is worth nothing if the only
// evidence it works is that the bug it was written for is gone.
func TestAnchors_catchTheTruncationTheyAreNamedFor(t *testing.T) {
	var truncating Anchor
	for _, a := range Anchors() {
		if a.Name == "the truncating bank" {
			truncating = a
		}
	}
	if truncating.Name == "" {
		t.Fatal("the truncating-bank anchor is gone")
	}
	if truncating.Comparison.Amount == AmountExact {
		t.Fatal("the truncating-bank anchor is back to pairing truncation with an exact " +
			"amount, which is worth +5.129 bits and carries the case without the payee")
	}

	// Thirty labels drawn from the shipped model, with every truncation resampled
	// away. Only that one level is missing; everything else is present in the
	// proportions the model expects, so what follows is about the silence on
	// truncation and not about a corpus that is narrow in every direction.
	labels := labelsWithout(t, 30, PayeeTruncated, 4)

	// Three thousand sampled window losers, spread as the shipped u says they
	// fall — which means they do contain truncations.
	base := DefaultLinkage()
	sampled := emptyCounts()
	const losers = 3000
	for lv, p := range base.PayeeU {
		sampled.PayeeU[lv] += int(p * losers)
	}
	for lv, p := range base.AmountU {
		sampled.AmountU[lv] += int(p * losers)
	}
	for lv, p := range base.DateU {
		sampled.DateU[lv] += int(p * losers)
	}

	shipped := Policy{}
	if got := truncating.decide(shipped); got != truncating.Want {
		t.Fatalf("under the shipped parameters the anchor already decides %q, not %q",
			got, truncating.Want)
	}

	// Half one: the refit holds the ratio, so the level survives the silence.
	blind := ProposeTrial(FittingPrior(), labels, sampled, defaultAlpha, defaultOverlap)
	weight := evidence(blind.Linkage.PayeeM[PayeeTruncated], blind.Linkage.PayeeU[PayeeTruncated])
	want := evidence(base.PayeeM[PayeeTruncated], base.PayeeU[PayeeTruncated])
	t.Logf("after a refit blind to truncation the level is worth %+.4f bits, shipped %+.4f",
		weight, want)
	if math.Abs(weight-want) > 1e-9 {
		t.Errorf("thirty labels that never mentioned truncation moved it from %+.4f bits "+
			"to %+.4f. A level the labels are silent about must keep the weight it was "+
			"shipped with, not the one the u side's sample size implies", want, weight)
	}
	if got := truncating.decide(blind.apply(shipped)); got != truncating.Want {
		t.Errorf("a refit blind to truncation decides the anchor as %q, want %q", got, truncating.Want)
	}

	// Half two: if the level does collapse, the anchors say so. This is the
	// unpaired refit — each side shrunk against its own denominator, which is what
	// Refit did before it held the ratio.
	collapsed := Trial{Linkage: Linkage{
		PayeeM:  Posterior(base.PayeeM, CountLevels(labels).PayeeM, defaultAlpha),
		PayeeU:  Posterior(base.PayeeU, sampled.PayeeU, defaultAlpha),
		AmountM: Posterior(base.AmountM, CountLevels(labels).AmountM, defaultAlpha),
		AmountU: Posterior(base.AmountU, sampled.AmountU, defaultAlpha),
		DateM:   Posterior(base.DateM, CountLevels(labels).DateM, defaultAlpha),
		DateU:   Posterior(base.DateU, sampled.DateU, defaultAlpha),
	}, Calibration: Identity()}

	unpaired := evidence(collapsed.Linkage.PayeeM[PayeeTruncated], collapsed.Linkage.PayeeU[PayeeTruncated])
	got := truncating.decide(collapsed.apply(shipped))
	t.Logf("unpaired, the level is worth %+.4f bits and the anchor decides %q", unpaired, got)

	if got == truncating.Want {
		t.Errorf("a parameter set that takes the truncated level to %+.4f bits still "+
			"decides the truncating-bank anchor as %q. The anchor cannot see the failure "+
			"it is named for", unpaired, got)
	}
	if c := checkAnchors(shipped, collapsed); c.Status != CheckFailed {
		t.Errorf("the anchor gate reported %q (%s) for a candidate that stops recognising "+
			"a truncating bank", c.Status, c.Detail)
	}
}

// labelsWithout draws labelled matches from the shipped model with one payee
// level held out, which is what a truncated review band does to a corpus: it
// removes a level rather than thinning the whole of it.
func labelsWithout(t *testing.T, n int, excluded PayeeLevel, seed int64) []LabelledDecision {
	t.Helper()
	l := DefaultLinkage()
	rng := rand.New(rand.NewSource(seed))

	pick := func(dist map[PayeeLevel]float64) PayeeLevel {
		for {
			r, acc := rng.Float64(), 0.0
			for _, lv := range []PayeeLevel{PayeeExact, PayeeTruncated, PayeeFuzzy,
				PayeeSubset, PayeeMissing, PayeeConflict, PayeeNone} {
				acc += dist[lv]
				if r <= acc {
					if lv == excluded {
						break // draw again
					}
					return lv
				}
			}
		}
	}
	pickAmount := func() AmountLevel {
		r, acc := rng.Float64(), 0.0
		for _, lv := range []AmountLevel{AmountExact, AmountHigherWithin, AmountLowerWithin,
			AmountOutsideHigher, AmountOutsideLower} {
			acc += l.AmountM[lv]
			if r <= acc {
				return lv
			}
		}
		return AmountExact
	}
	pickDate := func() DateLevel {
		r, acc := rng.Float64(), 0.0
		for _, lv := range []DateLevel{DateSame, DateAfterNear, DateBeforeNear,
			DateAfterFar, DateBeforeFar} {
			acc += l.DateM[lv]
			if r <= acc {
				return lv
			}
		}
		return DateSame
	}

	out := make([]LabelledDecision, n)
	for i := range out {
		out[i] = LabelledDecision{
			Comparison: Comparison{Payee: pick(l.PayeeM), Amount: pickAmount(), Date: pickDate()},
			Candidates: 1, Match: true,
		}
	}
	return out
}

// TestPromotion_judgesTheSetItWouldInstall is finding 7, which was found by
// hashing the two and noticing they differed.
//
// The verdict used to version a set fitted on every label while the calibration
// check scored a different set fitted on two thirds, and what went into force was
// the first. The gate therefore reported a held-out figure for a model, and
// installed a model that had seen the holdout. Nothing about that is visible from
// the page: both are called "the candidate" and both have a version.
func TestPromotion_judgesTheSetItWouldInstall(t *testing.T) {
	in := syntheticLabels(t, 300, 5)
	pol := Policy{}

	candidate, holdout := CandidateTrial(FittingPrior(), in, LevelCounts{}, defaultAlpha, defaultOverlap)
	v := EvaluateTrial(pol, in, LevelCounts{}, defaultAlpha, 0, 0)

	if v.Version != candidate.Version(pol) {
		t.Errorf("the verdict is about %s and the candidate is %s", v.Version, candidate.Version(pol))
	}

	// And the set that is judged really has not seen the decisions it is judged
	// on: every holdout decision must be absent from what it was fitted to.
	fit, _ := SplitLabelled(FittingPrior(), in, defaultOverlap)
	if len(fit)+len(holdout) != len(in) {
		t.Fatalf("the split lost decisions: %d + %d against %d", len(fit), len(holdout), len(in))
	}
	if len(holdout) == 0 {
		t.Fatal("three hundred decisions produced no holdout")
	}

	// The all-labels set is what used to be installed. It has to be a different
	// object, or this test would pass whatever the code did.
	everything := ProposeTrial(FittingPrior(), in, LevelCounts{}, defaultAlpha, defaultOverlap)
	if everything.Version(pol) == candidate.Version(pol) {
		t.Fatal("fitting on everything and fitting on two thirds gave the same parameters, " +
			"so this proves nothing")
	}
}

// TestFitReport_saysHowMuchOfTheCandidateIsStillTheShippedTable is the figure
// that separates "this candidate scores better" from "this candidate scores
// better and half of it is the table it started from".
//
// A refit holds a level the labels never mentioned at the weight it shipped
// with, which is the right thing to do and is invisible in every score. On a thin
// corpus most of the model can be in that state, and the Brier comparison the
// gate runs will not mention it once.
func TestFitReport_saysHowMuchOfTheCandidateIsStillTheShippedTable(t *testing.T) {
	base := DefaultLinkage()

	// Labels that only ever mention one payee level, one amount level and one
	// date level. Everything else is held.
	var narrow []LabelledDecision
	for i := 0; i < 40; i++ {
		narrow = append(narrow, LabelledDecision{
			Comparison: Comparison{Payee: PayeeExact, Amount: AmountExact, Date: DateSame},
			Candidates: 1, Match: true,
		})
	}
	thin := ProposeTrial(base, narrow, LevelCounts{}, defaultAlpha, defaultOverlap)
	t.Logf("thin corpus: %d labels, %d of seventeen levels still at their shipped weight, "+
		"platt %q", thin.Fit.Labelled, thin.Fit.LevelsHeld, thin.Fit.Platt.Outcome)

	if thin.Fit.Labelled != len(narrow) {
		t.Errorf("the report says %d labels went in, want %d", thin.Fit.Labelled, len(narrow))
	}
	if thin.Fit.Alpha != defaultAlpha {
		t.Errorf("the report says alpha %v, want %v", thin.Fit.Alpha, defaultAlpha)
	}
	if thin.Fit.LevelsHeld < 6 {
		t.Errorf("only %d levels came back at their shipped weight from a corpus that "+
			"mentions three; the count is not measuring what it claims", thin.Fit.LevelsHeld)
	}

	// And a corpus that covers the field properly holds far fewer.
	broad := ProposeTrial(base, syntheticLabels(t, 600, 3), LevelCounts{}, defaultAlpha, defaultOverlap)
	t.Logf("broad corpus: %d labels, %d levels held, platt %q",
		broad.Fit.Labelled, broad.Fit.LevelsHeld, broad.Fit.Platt.Outcome)
	if !below(float64(broad.Fit.LevelsHeld), float64(thin.Fit.LevelsHeld)) {
		t.Errorf("a corpus of six hundred varied labels holds %d levels and one of forty "+
			"identical ones holds %d; the figure has to fall as evidence arrives",
			broad.Fit.LevelsHeld, thin.Fit.LevelsHeld)
	}

	// The report must describe the trial it came with, not a fresh one.
	if thin.Fit.Platt.Outcome == "" {
		t.Error("no calibration outcome recorded")
	}
	if thin.Fit.Platt.Positive+thin.Fit.Platt.Negative != len(narrow) {
		t.Errorf("the calibration saw %d observations, want %d",
			thin.Fit.Platt.Positive+thin.Fit.Platt.Negative, len(narrow))
	}
}

// TestFitReport_namesTheReasonAFitGaveUp covers the failure that is silent by
// construction: a calibration that could not be fitted returns the identity, and
// an identity is exactly what an installation that never fitted one runs.
func TestFitReport_namesTheReasonAFitGaveUp(t *testing.T) {
	// One outcome only. There is no boundary to place and the fit cannot start.
	var oneSided []Observation
	for i := 0; i < 50; i++ {
		oneSided = append(oneSided, Observation{Weight: float64(i)*0.1 - 2, Match: true})
	}
	c, report := fitPlatt(oneSided)
	if !c.IsIdentity() {
		t.Errorf("a one-sided corpus produced a calibration of %+v", c)
	}
	if report.Outcome != "one outcome only" {
		t.Errorf("outcome %q, want it to name the reason", report.Outcome)
	}

	// And a fit that works says so, with the coefficients it reached.
	c, report = fitPlatt(overconfident(2000, 1.8, 5))
	if c.IsIdentity() {
		t.Fatal("two thousand fittable observations produced the identity")
	}
	if report.Outcome != "converged" && report.Outcome != "no further decrease" {
		t.Errorf("outcome %q, want a converged one", report.Outcome)
	}
	if report.Iterations == 0 {
		t.Error("no iterations recorded")
	}
	if report.A != c.A || report.B != c.B {
		t.Errorf("the report says A=%v B=%v and the calibration is %+v", report.A, report.B, c)
	}
}
