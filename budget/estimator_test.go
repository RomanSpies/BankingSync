package budget

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// scenario is one bank whose real behaviour is known, so that an estimator can
// be scored against the answer rather than against its own plausibility.
type scenario[K comparable] struct {
	name string

	// short is the column heading in the report, because the full names do not
	// fit four to a line and a truncated one says nothing.
	short string

	// order fixes the draw, so that the same seed gives the same sample under
	// every rule and the comparison is paired rather than merely repeated.
	order []K

	priorM, priorU map[K]float64
	truthM, truthU map[K]float64

	labels  int
	sampled int

	// pinned is the null level, which no rule estimates and none is scored on.
	// Scoring it would score the pin rather than the estimator.
	pinned K

	// refitMustHelp marks a scenario where beating the untouched priors is the
	// whole point. Where it is false the refit may lose, and the promotion gate
	// rather than the estimator is what keeps that from reaching an installation.
	refitMustHelp bool
}

// rule is one candidate estimator, named so a failure says which one won.
//
// It takes both sides at once, because the rule that ships couples them: a level
// the labels never reached is held at the ratio it was shipped with, and that
// cannot be expressed as a function of one side alone. Rules that do not couple
// them simply ignore the other argument.
type rule[K comparable] struct {
	name string
	fit  func(sc scenario[K], countsM, countsU map[K]int) (m, u map[K]float64)
}

// loss is one way of adding up how wrong a set of weights is.
//
// Four of them, because the choice is not neutral and the file used to make it
// silently. The unweighted sum treats a level occurring two times in a thousand
// exactly as it treats one occurring four times in ten, which flatters weak
// concentrations on rare levels and is how a rule can be chosen by the scoring
// rather than by the data. byMatchRate is the decision-relevant one: expected
// bits of weight error per true match.
type loss[K comparable] struct {
	name string
	add  func(sc scenario[K], level K, delta float64) float64
}

func losses[K comparable]() []loss[K] {
	return []loss[K]{
		{"flat", func(_ scenario[K], _ K, d float64) float64 { return math.Abs(d) }},
		{"byMatchRate", func(sc scenario[K], k K, d float64) float64 { return sc.truthM[k] * math.Abs(d) }},
		{"byPairRate", func(sc scenario[K], k K, d float64) float64 { return sc.truthU[k] * math.Abs(d) }},
		{"squared", func(_ scenario[K], _ K, d float64) float64 { return d * d }},
	}
}

const replications = 200

// TestEstimator_recoversWhatItIsMeantTo scores the refit against banks whose
// real parameters are fixed here, because that is the only way an estimator can
// be checked at all.
//
// This file has been got wrong twice, both times the same way: a rule was argued
// for from its properties — this one keeps the invariant, that one reads silence
// correctly — and the argument was checked instead of the rule. The result reads
// as careful and is not. So nothing here is asserted from a description.
//
// It was also got wrong a third way, which is why it now runs over two fields
// and four losses. Only the date field was ever exercised, and every date level
// carries substantial mass; the payee field, whose u values run to a few
// thousandths and which drives the sensitivity table, was never scored at all.
// The two regimes are not comparable, and the one that was never tested is the
// one where an unobserved level does damage.
//
// What the numbers show, and it is worth stating plainly: **no rule wins
// everywhere.** A strong concentration is better when the shipped priors are
// close to the truth and worse when they are not, which is exactly what a strong
// prior means.
//
// Two things are asserted, and only two, because only two are true everywhere:
// refitting must beat leaving the priors alone on a bank unlike them, or the
// feature has no purpose; and no rule may drive a level to zero, because a zero
// is not a small probability but an infinite weight. Everything else is printed
// for a person to read.
func TestEstimator_recoversWhatItIsMeantTo(t *testing.T) {
	scoreField(t, "date", dateScenarios(), dateRules())
	scoreField(t, "payee", payeeScenarios(), payeeRules())
}

func scoreField[K comparable](t *testing.T, field string, scs []scenario[K], rules []rule[K]) {
	t.Helper()
	t.Logf("=== %s ===", field)

	for _, l := range losses[K]() {
		t.Logf("  loss = %s", l.name)
		totals := map[string]float64{}
		best := map[string]string{}
		bestValue := map[string]float64{}
		scored := map[string]map[string]float64{}

		for _, sc := range scs {
			scored[sc.name] = map[string]float64{}
			for _, r := range rules {
				v := meanError(sc, r, l)
				scored[sc.name][r.name] = v
				totals[r.name] += v
				if b, seen := bestValue[sc.name]; !seen || v < b {
					bestValue[sc.name], best[sc.name] = v, r.name
				}
			}
		}

		heading := ""
		for _, sc := range scs {
			heading += sprintf("%12s", sc.short)
		}
		t.Logf("    %-46s%s %11s", "", heading, "TOTAL")

		for _, r := range rules {
			line := ""
			for _, sc := range scs {
				mark := " "
				if best[sc.name] == r.name {
					mark = "*"
				}
				line += sprintf("%11s%s", fmtBits(scored[sc.name][r.name]), mark)
			}
			t.Logf("    %-46s%s %11.4f", r.name, line, totals[r.name])
		}

		var winner string
		var winning float64
		for _, r := range rules {
			if winner == "" || totals[r.name] < winning {
				winner, winning = r.name, totals[r.name]
			}
		}
		t.Logf("    aggregate winner: %s (%.4f)", winner, winning)
	}

	// The one assertion: a refit has to earn its place on the banks it exists for.
	for _, sc := range scs {
		if !sc.refitMustHelp {
			continue
		}
		shipped := meanError(sc, rules[0], losses[K]()[1])
		plain := meanError(sc, noRefitRule[K](), losses[K]()[1])
		if !below(shipped, plain) {
			t.Errorf("%s/%s: refitting scores %.4f bits per true match and leaving the "+
				"priors alone scores %.4f. A refit that cannot beat the priors on a bank "+
				"unlike them has no purpose at all", field, sc.name, shipped, plain)
		}
	}
}

func fmtBits(v float64) string {
	switch {
	case v >= 100:
		return sprintf("%.1f", v)
	case v >= 10:
		return sprintf("%.2f", v)
	default:
		return sprintf("%.4f", v)
	}
}

// meanError draws repeatedly from a scenario and reports the average total error
// in log2(m/u), in bits, across the levels the estimator estimates.
//
// Seeds 0..replications-1, and the same seed drives the m draw and the u draw
// under every rule, so two rules see identical samples and the difference
// between them is not sampling noise.
func meanError[K comparable](sc scenario[K], r rule[K], l loss[K]) float64 {
	var acc float64
	for seed := 0; seed < replications; seed++ {
		rng := rand.New(rand.NewSource(int64(seed)))
		countsM := drawFrom(sc.order, sc.truthM, sc.labels, rng)
		countsU := drawFrom(sc.order, sc.truthU, sc.sampled, rng)

		m, u := r.fit(sc, countsM, countsU)
		for _, lv := range sc.order {
			if lv == sc.pinned {
				continue
			}
			acc += l.add(sc, lv, evidence(m[lv], u[lv])-evidence(sc.truthM[lv], sc.truthU[lv]))
		}
	}
	return acc / replications
}

func drawFrom[K comparable](order []K, dist map[K]float64, n int, rng *rand.Rand) map[K]int {
	out := map[K]int{}
	for i := 0; i < n; i++ {
		r, acc := rng.Float64(), 0.0
		for _, lv := range order {
			acc += dist[lv]
			if r <= acc {
				out[lv]++
				break
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- the rules

// shippedRule is the code that actually runs, scored as itself rather than as a
// description of itself.
func shippedRule[K comparable](alpha float64) rule[K] {
	return rule[K]{
		name: sprintf("shipped: paired, ratio held, α=%g", alpha),
		fit: func(sc scenario[K], cm, cu map[K]int) (map[K]float64, map[K]float64) {
			return refitField(sc.priorM, sc.priorU, cm, cu, alpha, sc.pinned)
		},
	}
}

// unpairedRule is Posterior on each side independently, which is what Refit did
// before the two sides were fitted together.
func unpairedRule[K comparable](alpha float64) rule[K] {
	return rule[K]{
		name: sprintf("unpaired posterior, α=%g", alpha),
		fit: func(sc scenario[K], cm, cu map[K]int) (map[K]float64, map[K]float64) {
			return Posterior(sc.priorM, cm, alpha), Posterior(sc.priorU, cu, alpha)
		},
	}
}

func noRefitRule[K comparable]() rule[K] {
	return rule[K]{
		name: "no refit",
		fit: func(sc scenario[K], _, _ map[K]int) (map[K]float64, map[K]float64) {
			return sc.priorM, sc.priorU
		},
	}
}

// scopedRule is the attempt withdrawn in 5b6fb61, kept only so the file can keep
// refuting it. It holds a level nothing was observed about at its prior mass and
// rescales the observed levels into the mass that leaves — which reserves a
// near-impossible level's prior mass, and that can be two orders of magnitude too
// large.
func scopedRule[K comparable](alpha float64) rule[K] {
	scope := func(prior map[K]float64, counts map[K]int) map[K]float64 {
		out := make(map[K]float64, len(prior))
		for k, p := range prior {
			out[k] = p
		}
		var scoped []K
		var mass, total float64
		for k := range prior {
			if counts[k] < 1 {
				continue
			}
			scoped = append(scoped, k)
			mass += prior[k]
			total += float64(counts[k])
		}
		if len(scoped) == 0 {
			return out
		}
		denom := alpha*float64(len(scoped)) + total
		for _, k := range scoped {
			out[k] = mass * (alpha + float64(counts[k])) / denom
		}
		return out
	}
	return rule[K]{
		name: "scoped to observed levels (withdrawn in 5b6fb61)",
		fit: func(sc scenario[K], cm, cu map[K]int) (map[K]float64, map[K]float64) {
			return scope(sc.priorM, cm), scope(sc.priorU, cu)
		},
	}
}

// The grid runs over the named concentrations rather than over round numbers.
// Per field, with s the total concentration: Perks suggests s = 1, Jeffreys
// s = k/2, Bayes and Laplace s = k, and Haldane s = 0. k is 5 for date and 7 for
// payee, which is why the two grids differ.
func dateRules() []rule[DateLevel] {
	return []rule[DateLevel]{
		shippedRule[DateLevel](defaultAlpha),
		noRefitRule[DateLevel](),
		unpairedRule[DateLevel](1),
		unpairedRule[DateLevel](defaultAlpha),
		unpairedRule[DateLevel](2.5), // Jeffreys, k = 5
		unpairedRule[DateLevel](5),   // Laplace, k = 5
		unpairedRule[DateLevel](10),
		unpairedRule[DateLevel](50), // the withdrawn concentration
		scopedRule[DateLevel](defaultAlpha),
	}
}

func payeeRules() []rule[PayeeLevel] {
	return []rule[PayeeLevel]{
		shippedRule[PayeeLevel](defaultAlpha),
		noRefitRule[PayeeLevel](),
		unpairedRule[PayeeLevel](1),
		unpairedRule[PayeeLevel](defaultAlpha),
		unpairedRule[PayeeLevel](3.5), // Jeffreys, k = 7
		unpairedRule[PayeeLevel](7),   // Laplace, k = 7
		unpairedRule[PayeeLevel](10),
		unpairedRule[PayeeLevel](50),
		scopedRule[PayeeLevel](defaultAlpha),
	}
}

// ------------------------------------------------------------ the scenarios

func dateScenarios() []scenario[DateLevel] {
	base := DefaultLinkage()
	order := []DateLevel{DateSame, DateAfterNear, DateBeforeNear, DateAfterFar, DateBeforeFar}
	mk := func(name, short string, tm, tu map[DateLevel]float64, labels, sampled int, must bool) scenario[DateLevel] {
		return scenario[DateLevel]{
			name: name, short: short, order: order,
			priorM: base.DateM, priorU: base.DateU,
			truthM: tm, truthU: tu,
			labels: labels, sampled: sampled,
			pinned: noPinnedDateLevel, refitMustHelp: must,
		}
	}
	return []scenario[DateLevel]{
		mk("a bank close to the shipped assumption", "close",
			map[DateLevel]float64{DateSame: .35, DateAfterNear: .30, DateBeforeNear: .20, DateAfterFar: .10, DateBeforeFar: .05},
			map[DateLevel]float64{DateSame: .18, DateAfterNear: .17, DateBeforeNear: .15, DateAfterFar: .26, DateBeforeFar: .24},
			120, 5000, false),
		mk("a bank unlike it, which is what a refit is for", "unlike",
			map[DateLevel]float64{DateSame: .10, DateAfterNear: .55, DateBeforeNear: .05, DateAfterFar: .28, DateBeforeFar: .02},
			map[DateLevel]float64{DateSame: .05, DateAfterNear: .20, DateBeforeNear: .10, DateAfterFar: .40, DateBeforeFar: .25},
			120, 5000, true),
		mk("the priors overstate a rare level forty-fold", "rare40x",
			map[DateLevel]float64{DateSame: .40, DateAfterNear: .40, DateBeforeNear: .19, DateAfterFar: .008, DateBeforeFar: .002},
			map[DateLevel]float64{DateSame: .15, DateAfterNear: .15, DateBeforeNear: .15, DateAfterFar: .275, DateBeforeFar: .275},
			500, 20000, true),
		mk("a bank that never books before the authorisation", "neverbefore",
			map[DateLevel]float64{DateSame: .45, DateAfterNear: .45, DateBeforeNear: .001, DateAfterFar: .098, DateBeforeFar: .001},
			map[DateLevel]float64{DateSame: .20, DateAfterNear: .30, DateBeforeNear: .001, DateAfterFar: .497, DateBeforeFar: .002},
			120, 5000, false),
	}
}

// payeeScenarios is the field the harness never had, and the regimes are not the
// date field's: u runs to four thousandths here, and the last scenario is the one
// the whole of finding 1 is about — a truncating bank whose thirty labels happen
// to contain no truncation at all, which at m = 0.08 happens 8% of the time.
func payeeScenarios() []scenario[PayeeLevel] {
	base := DefaultLinkage()
	order := []PayeeLevel{PayeeExact, PayeeTruncated, PayeeFuzzy, PayeeSubset,
		PayeeMissing, PayeeConflict, PayeeNone}
	mk := func(name, short string, tm, tu map[PayeeLevel]float64, labels, sampled int, must bool) scenario[PayeeLevel] {
		return scenario[PayeeLevel]{
			name: name, short: short, order: order,
			priorM: base.PayeeM, priorU: base.PayeeU,
			truthM: tm, truthU: tu,
			labels: labels, sampled: sampled,
			pinned: PayeeMissing, refitMustHelp: must,
		}
	}
	return []scenario[PayeeLevel]{
		mk("a bank close to the shipped assumption", "close",
			map[PayeeLevel]float64{PayeeExact: .50, PayeeTruncated: .10, PayeeFuzzy: .06, PayeeSubset: .14, PayeeMissing: .16, PayeeConflict: .02, PayeeNone: .02},
			map[PayeeLevel]float64{PayeeExact: .055, PayeeTruncated: .012, PayeeFuzzy: .007, PayeeSubset: .022, PayeeMissing: .14, PayeeConflict: .12, PayeeNone: .644},
			120, 5000, false),
		mk("a bank that truncates almost everything", "truncating",
			map[PayeeLevel]float64{PayeeExact: .12, PayeeTruncated: .55, PayeeFuzzy: .06, PayeeSubset: .10, PayeeMissing: .13, PayeeConflict: .02, PayeeNone: .02},
			map[PayeeLevel]float64{PayeeExact: .02, PayeeTruncated: .06, PayeeFuzzy: .006, PayeeSubset: .02, PayeeMissing: .15, PayeeConflict: .10, PayeeNone: .644},
			120, 5000, true),
		mk("a bank that never truncates at all", "nevertrunc",
			map[PayeeLevel]float64{PayeeExact: .62, PayeeTruncated: .001, PayeeFuzzy: .05, PayeeSubset: .14, PayeeMissing: .17, PayeeConflict: .009, PayeeNone: .01},
			map[PayeeLevel]float64{PayeeExact: .05, PayeeTruncated: .0005, PayeeFuzzy: .006, PayeeSubset: .02, PayeeMissing: .15, PayeeConflict: .12, PayeeNone: .6535},
			120, 5000, false),
		mk("a truncating bank, and only thirty labels to say so", "thin",
			map[PayeeLevel]float64{PayeeExact: .50, PayeeTruncated: .08, PayeeFuzzy: .05, PayeeSubset: .13, PayeeMissing: .20, PayeeConflict: .02, PayeeNone: .02},
			map[PayeeLevel]float64{PayeeExact: .05, PayeeTruncated: .010, PayeeFuzzy: .006, PayeeSubset: .02, PayeeMissing: .15, PayeeConflict: .12, PayeeNone: .644},
			30, 1200, false),
	}
}

// TestEstimator_neverDrivesALevelToZero is the one property that has to hold
// whatever the data, because a zero is not a small probability here — it is an
// infinite weight, and one level would decide every comparison it appears in.
//
// It now covers both fields and every rule that ships, because the paired refit
// is the one that could break it: it moves mass between levels, and a rescaling
// that overshot would be exactly this failure.
func TestEstimator_neverDrivesALevelToZero(t *testing.T) {
	checkPositive(t, "date", dateScenarios(), dateRules())
	checkPositive(t, "payee", payeeScenarios(), payeeRules())
}

func checkPositive[K comparable](t *testing.T, field string, scs []scenario[K], rules []rule[K]) {
	t.Helper()
	for _, sc := range scs {
		for _, r := range rules {
			for seed := 0; seed < 50; seed++ {
				rng := rand.New(rand.NewSource(int64(seed)))
				cm := drawFrom(sc.order, sc.truthM, sc.labels, rng)
				cu := drawFrom(sc.order, sc.truthU, sc.sampled, rng)

				m, u := r.fit(sc, cm, cu)
				for side, table := range map[string]map[K]float64{"m": m, "u": u} {
					var sum float64
					for lv, v := range table {
						if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
							t.Fatalf("%s/%s/%s seed %d: %s side has %v at %v",
								field, sc.name, r.name, seed, side, v, lv)
						}
						sum += v
					}
					if math.Abs(sum-1) > 1e-9 {
						t.Fatalf("%s/%s/%s seed %d: the %s side sums to %.12f",
							field, sc.name, r.name, seed, side, sum)
					}
				}
			}
		}
	}
}

func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }
