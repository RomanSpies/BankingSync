package budget

import (
	"math"
	"testing"
	"time"
)

// TestLinkage_probabilitiesSumToOne is the constraint that keeps the parameters
// from being free numbers.
//
// The levels of a field are exhaustive and mutually exclusive, so m and u are
// each a probability distribution over them. Raising one level's share has to
// take it from another, which forces the question "how often does this happen?"
// to be answered for every case rather than only the convenient ones. Without
// this, tuning one weight upward would be free and the model would be an
// elaborate way of writing down a hunch.
func TestLinkage_probabilitiesSumToOne(t *testing.T) {
	l := DefaultLinkage()

	sum := func(m map[PayeeLevel]float64) float64 {
		var s float64
		for _, v := range m {
			s += v
		}
		return s
	}
	sumA := func(m map[AmountLevel]float64) float64 {
		var s float64
		for _, v := range m {
			s += v
		}
		return s
	}
	sumD := func(m map[DateLevel]float64) float64 {
		var s float64
		for _, v := range m {
			s += v
		}
		return s
	}

	for name, got := range map[string]float64{
		"payee m": sum(l.PayeeM), "payee u": sum(l.PayeeU),
		"amount m": sumA(l.AmountM), "amount u": sumA(l.AmountU),
		"date m": sumD(l.DateM), "date u": sumD(l.DateU),
	} {
		if math.Abs(got-1) > 1e-9 {
			t.Errorf("%s sums to %.4f, want 1", name, got)
		}
	}
}

// TestLinkage_everyLevelIsParameterised catches the failure mode that would
// otherwise be silent: a level added to the classifier but never given an m and
// a u contributes nothing, so a whole class of evidence would quietly stop
// counting.
func TestLinkage_everyLevelIsParameterised(t *testing.T) {
	l := DefaultLinkage()

	for _, lv := range []PayeeLevel{
		PayeeMissing, PayeeNone, PayeeConflict, PayeeSubset,
		PayeeTruncated, PayeeFuzzy, PayeeExact,
	} {
		if l.PayeeM[lv] <= 0 || l.PayeeU[lv] <= 0 {
			t.Errorf("payee level %s has no m/u and would carry no evidence", lv)
		}
	}
	for _, lv := range []AmountLevel{AmountOutsideLower, AmountOutsideHigher,
		AmountLowerWithin, AmountHigherWithin, AmountExact} {
		if l.AmountM[lv] <= 0 || l.AmountU[lv] <= 0 {
			t.Errorf("amount level %s has no m/u", lv)
		}
	}
	for _, lv := range []DateLevel{DateBeforeFar, DateAfterFar, DateBeforeNear,
		DateAfterNear, DateSame} {
		if l.DateM[lv] <= 0 || l.DateU[lv] <= 0 {
			t.Errorf("date level %s has no m/u", lv)
		}
	}
}

// TestLinkage_weightIsTheSumOfItsParts checks the arithmetic against a figure
// worked out by hand, so a change to the formula cannot hide behind a change to
// the parameters.
//
// The reported case: payee truncated, amount inside tolerance, settled two days
// later, three candidates in the window.
//
//	prior   log2((0.5/3)/0.5)    = log2(1/3)          = −1.5850
//	payee   log2(0.08/0.010)     = log2(8)            = +3.0000
//	amount  log2(0.25/0.15)      = log2(1.6667)       = +0.7370
//	date    log2(0.45/0.30)      = log2(1.5)          = +0.5850
//	                                                    ────────
//	                                                     +2.7370
//
// The prior is the overlap spread over the candidates, against the chance of no
// counterpart at all: half of the transactions reaching the matcher have one, so
// with three candidates a specific one carries a sixth against the half that have
// none. It used to be written as 1/(n+1) and comes to the same log2(1/3) — the
// form changed so that the half can be said out loud, not so that the number
// would move.
//
// The payee term was +4.3219 while PayeeU[PayeeTruncated] stood at 0.004, and the
// total was +4.0589 at P = 0.9434. Both moved when that number did; see the
// comment on the level for why it moved and what is and is not known about the
// replacement.
func TestLinkage_weightIsTheSumOfItsParts(t *testing.T) {
	l := DefaultLinkage()
	c := Comparison{Payee: PayeeTruncated, Amount: AmountHigherWithin, Date: DateAfterNear}

	const want = -1.5850 + 3.0000 + 0.7370 + 0.5850
	if got := l.Weight(c, 3, defaultOverlap); math.Abs(got-want) > 0.001 {
		t.Errorf("weight = %.4f, want %.4f", got, want)
	}

	// 2^2.7370 / (1 + 2^2.7370)
	if got := Probability(l.Weight(c, 3, defaultOverlap)); math.Abs(got-0.8696) > 0.001 {
		t.Errorf("probability = %.4f, want 0.8696", got)
	}
}

// TestLinkage_theReportedCaseOutweighsTheBranchCase is the pair the whole design
// turns on. They differ in one field and one level, and the model has to put
// them on opposite sides of any sane threshold.
//
// The separation is the promise and it is unconditional. Where the truncated case
// falls relative to the automatic threshold is a second question and the answer
// depends on how crowded the window is: alone in its fortnight it merges unasked,
// against two other plausible rows it goes to a person.
//
// It used to merge unasked in both, and that rested on PayeeU[PayeeTruncated]
// being 0.004 — the least evidenced number in the shipped tables, and the one
// that made a truncated payee better evidence than a verbatim one. Raising it to
// ten in a thousand costs this case its automatic merge in a crowded window. That
// is the honest consequence of the correction and not a regression to be tuned
// away: the auto-merge was bought with a number nobody could support.
func TestLinkage_theReportedCaseOutweighsTheBranchCase(t *testing.T) {
	l := DefaultLinkage()
	reported := Comparison{Payee: PayeeTruncated, Amount: AmountHigherWithin, Date: DateAfterNear}
	branch := Comparison{Payee: PayeeConflict, Amount: AmountHigherWithin, Date: DateAfterNear}

	// "Da Luigi Roma" pending, "Visa Da Luigi" booked, against
	// "Da Luigi Roma" pending, "Visa Da Luigi Milano" booked — another branch.
	pT, pC := Probability(l.Weight(reported, 3, defaultOverlap)), Probability(l.Weight(branch, 3, defaultOverlap))
	if pT <= pC {
		t.Fatalf("truncation %.3f must outweigh conflict %.3f", pT, pC)
	}
	if pC > 0.50 {
		t.Errorf("a different branch reaches %.3f; it is meant to fall short of review", pC)
	}
	if pT < 0.50 {
		t.Errorf("the reported case reaches only %.3f; it is meant to be at least asked "+
			"about, whatever else is in the window", pT)
	}

	// Alone in the window it still merges without asking, which is what the
	// truncating-bank anchor holds the parameters to.
	if alone := Probability(l.Weight(reported, 1, defaultOverlap)); alone < 0.90 {
		t.Errorf("with no other candidate the reported case reaches %.3f; a truncating "+
			"bank's own settlement has to be recognised without asking", alone)
	}
	// And truncation must still beat a verbatim match on nothing. The ordering
	// among payee levels is not a published requirement, but a partial agreement
	// outweighing a total one is not what anybody means by a truncated match.
	exact := evidence(l.PayeeM[PayeeExact], l.PayeeU[PayeeExact])
	truncated := evidence(l.PayeeM[PayeeTruncated], l.PayeeU[PayeeTruncated])
	if !below(truncated, exact) {
		t.Errorf("a truncated payee is worth %+.4f bits and a verbatim one %+.4f: a pair "+
			"agreeing in full would be held while the cut-off spelling of the same pair "+
			"merged unasked", truncated, exact)
	}
}

// TestLinkage_oneStrongFieldCanCarryAWeakerOne is the property that a set of
// independent gates cannot have, and the reason for using this model at all.
//
// Today an amount outside tolerance ends the matter regardless of anything else.
// Under the model it is heavy evidence against, but evidence — an exact payee on
// the same day pushes back, and the result lands in the band a person looks at
// rather than being decided silently either way.
func TestLinkage_oneStrongFieldCanCarryAWeakerOne(t *testing.T) {
	l := DefaultLinkage()

	// Only the payee differs. The earlier version of this varied the date as
	// well, so the date carried the whole difference and the test passed with
	// the payee term deleted outright — it asserted nothing it claimed to.
	weak := Probability(l.Weight(Comparison{Payee: PayeeNone, Amount: AmountOutsideHigher, Date: DateSame}, 3, defaultOverlap))
	pushed := Probability(l.Weight(Comparison{Payee: PayeeExact, Amount: AmountOutsideHigher, Date: DateSame}, 3, defaultOverlap))

	if pushed <= weak {
		t.Fatalf("an exact payee contributed nothing: %.4f vs %.4f", pushed, weak)
	}
	// Against the shipped threshold rather than a round number, because that is
	// the boundary the claim is actually about: an amount outside tolerance must
	// not be merged unasked however well the payee agrees. A loose bound of 0.95
	// held whether the amount contributed anything or not.
	if pushed >= defaultAutoProbability {
		t.Errorf("an amount outside tolerance reaches %.3f, at or above the %.2f that "+
			"merges without asking; it must not merge on the payee alone",
			pushed, defaultAutoProbability)
	}
}

// TestLinkage_aBusyWindowIsAWeakerStartingPoint pins the prior. The same
// comparison means less when it was picked out of forty rows than out of two,
// and taking the prior from the candidate count is what says so.
func TestLinkage_aBusyWindowIsAWeakerStartingPoint(t *testing.T) {
	l := DefaultLinkage()
	c := Comparison{Payee: PayeeTruncated, Amount: AmountHigherWithin, Date: DateAfterNear}

	quiet, busy := l.Weight(c, 2, defaultOverlap), l.Weight(c, 40, defaultOverlap)
	if quiet <= busy {
		t.Errorf("a two-row window (%.3f) must beat a forty-row one (%.3f)", quiet, busy)
	}
}

func TestLinkage_missingPayeeIsNeutral(t *testing.T) {
	l := DefaultLinkage()
	if got := evidence(l.PayeeM[PayeeMissing], l.PayeeU[PayeeMissing]); math.Abs(got) > 0.001 {
		t.Errorf("an absent payee contributes %.4f bits; missing information must "+
			"count neither for nor against", got)
	}
}

// TestLinkage_missingPayeeStaysNeutralAfterARefit is the half of that promise
// that used to be missing, and it is the half an installation actually meets.
//
// The shipped table is neutral by construction and only the shipped table was
// ever checked. A refit put PayeeMissing into both multinomials with nothing
// holding them together, and the m side has some forty times fewer observations
// than the u side, so the two estimates of the same quantity drifted apart.
// Measured over two thousand replications at the minimum promotion bar, an absent
// payee came to carry 0.41 bits on average and 5.60 at worst — against a match,
// on the strength of the payee field having said nothing at all.
//
// Refit now holds the null level out of the estimate on both sides, which is what
// Splink does with a null comparison level, so this is exact rather than close.
func TestLinkage_missingPayeeStaysNeutralAfterARefit(t *testing.T) {
	base := DefaultLinkage()

	for _, tc := range []struct {
		name    string
		labels  int
		sampled int
		seed    uint64
	}{
		{"at the minimum promotion bar", 50, 2000, 11},
		{"at a corpus a year might reach", 120, 5000, 12},
		{"at a corpus nobody reaches", 500, 20000, 13},
		{"with nothing on the m side at all", 0, 5000, 14},
	} {
		counts := drawCounts(base, tc.labels, tc.seed)
		for lv, v := range drawCounts(base, tc.sampled, tc.seed^0xabcdef).PayeeM {
			counts.PayeeU[lv] += v
		}

		got := Refit(base, counts, defaultAlpha)
		bits := evidence(got.PayeeM[PayeeMissing], got.PayeeU[PayeeMissing])
		if math.Abs(bits) > 1e-9 {
			t.Errorf("%s: after a refit an absent payee carries %+.4f bits", tc.name, bits)
		}

		// And it is still a distribution on both sides, which the holding out
		// must not have cost.
		for side, table := range map[string]map[PayeeLevel]float64{
			"m": got.PayeeM, "u": got.PayeeU,
		} {
			var sum float64
			for _, v := range table {
				if v <= 0 {
					t.Errorf("%s: the %s side has a non-positive level", tc.name, side)
				}
				sum += v
			}
			if math.Abs(sum-1) > 1e-9 {
				t.Errorf("%s: the %s side sums to %.12f", tc.name, side, sum)
			}
		}
	}
}

// TestLinkage_explainAccountsForTheWholeWeight keeps the reason shown to a
// person and the number used to decide from drifting apart.
func TestLinkage_explainAccountsForTheWholeWeight(t *testing.T) {
	l := DefaultLinkage()

	// Both shapes, because the frequency correction is the part most easily left
	// out of the explanation: it lives on the payee term rather than on a field
	// of its own, and a reason that quietly omits it would show a person a figure
	// the decision was not made on.
	for name, c := range map[string]Comparison{
		"without a measured frequency": {Payee: PayeeTruncated, Amount: AmountHigherWithin, Date: DateAfterNear},
		"with one":                     {Payee: PayeeExact, Amount: AmountExact, Date: DateSame, PayeeFrequency: 0.35},
	} {
		var sum float64
		for _, e := range l.Explain(c, 3, defaultOverlap) {
			sum += e.Bits
		}
		if got := l.Weight(c, 3, defaultOverlap); math.Abs(sum-got) > 1e-9 {
			t.Errorf("%s: the explanation adds up to %.4f but the weight is %.4f",
				name, sum, got)
		}
	}
}

func TestClassifyAmount(t *testing.T) {
	for name, tc := range map[string]struct {
		a, b int64
		want AmountLevel
	}{
		"identical":            {-12000, -12000, AmountExact},
		"hotel incidentals":    {-12000, -13850, AmountHigherWithin},
		"fuel release":         {-12000, -10500, AmountLowerWithin},
		"a much larger charge": {-4000, -12000, AmountOutsideHigher},
		"a much smaller one":   {-12000, -4000, AmountOutsideLower},
		"a refund grows":       {4000, 5000, AmountHigherWithin},
	} {
		if got := ClassifyAmount(tc.a, tc.b, 25, 5000); got != tc.want {
			t.Errorf("%s: got %s, want %s", name, got, tc.want)
		}
	}

	// With the tolerance switched off, only equality remains — the existing
	// meaning of a zero policy, which must not change.
	if got := ClassifyAmount(-12000, -13850, 0, 0); got != AmountOutsideHigher {
		t.Errorf("tolerance off: got %s, want outside_higher", got)
	}
}

func TestClassifyDate(t *testing.T) {
	base := day(2026, time.July, 10)
	for name, tc := range map[string]struct {
		other time.Time
		want  DateLevel
	}{
		"same day":            {base, DateSame},
		"settles next day":    {base.AddDate(0, 0, 1), DateAfterNear},
		"three days":          {base.AddDate(0, 0, 3), DateAfterNear},
		"four days":           {base.AddDate(0, 0, 4), DateAfterFar},
		"two days earlier":    {base.AddDate(0, 0, -2), DateBeforeNear},
		"a fortnight earlier": {base.AddDate(0, 0, -14), DateBeforeFar},
	} {
		if got := ClassifyDate(base, tc.other); got != tc.want {
			t.Errorf("%s: got %s, want %s", name, got, tc.want)
		}
	}
}

// TestLinkage_directionalLevelsCarryTheSameEvidence is Phase 1's guarantee,
// written as an assertion rather than as a hope.
//
// Splitting a level by direction is meant to record a distinction, not to make
// one. Until the decision log has counted how often a true match settles above
// its authorisation rather than below, asserting a difference would be inventing
// evidence — the failure this whole model was adopted to avoid. So the halves
// carry identical weight, and this is what says so.
func TestLinkage_directionalLevelsCarryTheSameEvidence(t *testing.T) {
	l := DefaultLinkage()

	amounts := map[string][2]AmountLevel{
		"within tolerance": {AmountHigherWithin, AmountLowerWithin},
		"outside it":       {AmountOutsideHigher, AmountOutsideLower},
	}
	for name, pair := range amounts {
		a := evidence(l.AmountM[pair[0]], l.AmountU[pair[0]])
		b := evidence(l.AmountM[pair[1]], l.AmountU[pair[1]])
		if math.Abs(a-b) > 1e-9 {
			t.Errorf("amount %s: %s carries %.4f bits and %s carries %.4f — a direction "+
				"nobody has measured is being treated as evidence",
				name, pair[0], a, pair[1], b)
		}
	}

	dates := map[string][2]DateLevel{
		"near": {DateAfterNear, DateBeforeNear},
		"far":  {DateAfterFar, DateBeforeFar},
	}
	for name, pair := range dates {
		a := evidence(l.DateM[pair[0]], l.DateU[pair[0]])
		b := evidence(l.DateM[pair[1]], l.DateU[pair[1]])
		if math.Abs(a-b) > 1e-9 {
			t.Errorf("date %s: %s carries %.4f bits and %s carries %.4f",
				name, pair[0], a, pair[1], b)
		}
	}
}

// TestLinkage_theSplitPreservesTheOldWeights checks the other half of the same
// claim: not only do the halves agree with each other, they agree with the
// single level they replaced. Anything else would be a silent recalibration
// dressed up as a refactor.
func TestLinkage_theSplitPreservesTheOldWeights(t *testing.T) {
	l := DefaultLinkage()

	// The figures the levels carried before they were split, worked out from the
	// parameters this file shipped in bb14c81.
	for name, tc := range map[string]struct {
		got  float64
		want float64
	}{
		"amount within":  {evidence(l.AmountM[AmountHigherWithin], l.AmountU[AmountHigherWithin]), math.Log2(0.25 / 0.15)},
		"amount outside": {evidence(l.AmountM[AmountOutsideHigher], l.AmountU[AmountOutsideHigher]), math.Log2(0.05 / 0.83)},
		"date near":      {evidence(l.DateM[DateAfterNear], l.DateU[DateAfterNear]), math.Log2(0.45 / 0.30)},
		"date far":       {evidence(l.DateM[DateAfterFar], l.DateU[DateAfterFar]), math.Log2(0.15 / 0.55)},
	} {
		if math.Abs(tc.got-tc.want) > 1e-9 {
			t.Errorf("%s now carries %.4f bits, was %.4f: the split changed the model",
				name, tc.got, tc.want)
		}
	}
}

// TestLinkage_aCommonPayeeSaysLess is what the frequency correction is for.
//
// Agreeing on the supermarket the account visits weekly is not the evidence that
// agreeing on a restaurant abroad is, and the level parameters cannot say so
// because they describe the level rather than the value.
func TestLinkage_aCommonPayeeSaysLess(t *testing.T) {
	l := DefaultLinkage()
	plain := Comparison{Payee: PayeeExact, Amount: AmountExact, Date: DateSame}

	common := plain
	common.PayeeFrequency = 0.40 // two in five transactions on this account
	rare := plain
	rare.PayeeFrequency = 0.01

	base := l.Weight(plain, 3, defaultOverlap)
	if l.Weight(common, 3, defaultOverlap) >= base {
		t.Errorf("a payee holding 40%% of the account carries %.3f bits, no less than the "+
			"unmeasured %.3f", l.Weight(common, 3, defaultOverlap), base)
	}
	if l.Weight(rare, 3, defaultOverlap) <= base {
		t.Errorf("a payee seen once carries %.3f bits, no more than the unmeasured %.3f",
			l.Weight(rare, 3, defaultOverlap), base)
	}

	// The plan's figure: at a 40% share the exact level is worth under two bits,
	// down from the 3.46 it carries unmeasured.
	got := evidence(l.PayeeM[PayeeExact], l.PayeeU[PayeeExact]) + l.tfCorrection(common)
	if got >= 2 {
		t.Errorf("an exact agreement on a payee holding 40%% of the account is worth "+
			"%.3f bits, want under 2", got)
	}
}

// TestLinkage_theCorrectionOnlyTouchesAnExactAgreement keeps it where it belongs.
// A truncated or contradicted name is not an agreement on a value, so there is
// no value whose frequency could qualify it.
func TestLinkage_theCorrectionOnlyTouchesAnExactAgreement(t *testing.T) {
	l := DefaultLinkage()
	for _, lv := range []PayeeLevel{PayeeTruncated, PayeeFuzzy, PayeeSubset,
		PayeeConflict, PayeeNone, PayeeMissing} {
		c := Comparison{Payee: lv, Amount: AmountExact, Date: DateSame, PayeeFrequency: 0.4}
		if got := l.tfCorrection(c); got != 0 {
			t.Errorf("%s attracted a %.3f bit frequency correction", lv, got)
		}
	}
}

// TestLinkage_anUnmeasuredFrequencyChangesNothing is the guarantee for an
// installation with too little history to have a distribution: no measurement
// means no correction, not maximum evidence.
func TestLinkage_anUnmeasuredFrequencyChangesNothing(t *testing.T) {
	l := DefaultLinkage()
	c := Comparison{Payee: PayeeExact, Amount: AmountExact, Date: DateSame}
	if got := l.tfCorrection(c); got != 0 {
		t.Errorf("an unmeasured payee attracted a %.3f bit correction", got)
	}
}

// TestLinkage_versionCoversEveryFigureThatDecides is what makes the version
// worth carrying. A hash that misses a parameter is worse than no hash: it
// asserts that two decisions are comparable when they are not.
func TestLinkage_versionCoversEveryFigureThatDecides(t *testing.T) {
	l := DefaultLinkage()
	base := l.Version(0.9, 0.5, 1.0, 0.4, 25, 5000, []string{"VISA", "MC"}, 1, 0)

	if got := l.Version(0.9, 0.5, 1.0, 0.4, 25, 5000, []string{"MC", "VISA"}, 1, 0); got != base {
		t.Error("reordering the prefix list changed the version; the same policy must " +
			"hash the same however it was written down")
	}

	for name, got := range map[string]string{
		"auto threshold":   l.Version(0.95, 0.5, 1.0, 0.4, 25, 5000, []string{"VISA", "MC"}, 1, 0),
		"review threshold": l.Version(0.9, 0.6, 1.0, 0.4, 25, 5000, []string{"VISA", "MC"}, 1, 0),
		"margin":           l.Version(0.9, 0.5, 2.0, 0.4, 25, 5000, []string{"VISA", "MC"}, 1, 0),
		"tolerance pct":    l.Version(0.9, 0.5, 1.0, 0.4, 30, 5000, []string{"VISA", "MC"}, 1, 0),
		"tolerance cap":    l.Version(0.9, 0.5, 1.0, 0.4, 25, 7500, []string{"VISA", "MC"}, 1, 0),
		"prefix list":      l.Version(0.9, 0.5, 1.0, 0.4, 25, 5000, []string{"VISA"}, 1, 0),
		// The overlap is a figure that decides: it moves every weight in the
		// model, so two decisions taken under different overlaps are not
		// comparable and must not claim the same identity.
		"overlap": l.Version(0.9, 0.5, 1.0, 0.3, 25, 5000, []string{"VISA", "MC"}, 1, 0),
	} {
		if got == base {
			t.Errorf("changing the %s left the version alone", name)
		}
	}

	moved := DefaultLinkage()
	moved.PayeeM[PayeeTruncated] += 0.01
	moved.PayeeM[PayeeFuzzy] -= 0.01
	if got := moved.Version(0.9, 0.5, 1.0, 0.4, 25, 5000, []string{"VISA", "MC"}, 1, 0); got == base {
		t.Error("moving weight between two levels left the version alone")
	}

	// A rescaling changes every probability without touching a level, so it has
	// to change the identity too.
	if got := l.Version(0.9, 0.5, 1.0, 0.4, 25, 5000, []string{"VISA", "MC"}, 0.8, 0); got == base {
		t.Error("rescaling the probabilities left the version alone")
	}
}
