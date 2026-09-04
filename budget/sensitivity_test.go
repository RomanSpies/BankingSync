// Package budget_test holds the sensitivity analysis, which lives outside the
// package so it can use the real payee classifier — that package imports this
// one, and a corpus built on a stand-in comparator would measure the stand-in.
package budget_test

import (
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"testing"

	"bankingsync/budget"
	"bankingsync/internal/linkagegen"
	"bankingsync/internal/payeematch"
)

var updateSensitivity = flag.Bool("update-sensitivity", false,
	"rewrite budget/sensitivity.md from the current parameters")

const sensitivityDoc = "sensitivity.md"

// observation is one comparison the model was asked to judge, kept with the
// candidate count so the prior is reproduced exactly.
type observation struct {
	cmp        budget.Comparison
	candidates int
}

// buildCorpus draws a synthetic statement and records every comparison the
// matcher would have made on it.
//
// Synthetic because there is no other kind available: an installation whose bank
// does not truncate never produces a truncated comparison, and a sensitivity
// analysis over the cases one developer happens to see would report that the
// parameters nobody exercises do not matter.
//
// The generator's own rates do not enter the result. What is taken from it is
// the *shape* of a statement — that settlements cluster a few days out, that
// most payees repeat and a few do not, that branches collide — and the analysis
// then asks how the decisions over that shape move when a parameter does.
func buildCorpus(t *testing.T) []observation {
	t.Helper()

	// Roughly three purchases a day over four months, which is a busy private
	// account rather than a shop. The density matters more than the total: the
	// candidate count is the prior, so a statement with too many open
	// authorisations measures the prior instead of the parameters. Few stay
	// unsettled, because in a real statement almost everything settles and only
	// the last few days are still in flight.
	hist := linkagegen.Generate(linkagegen.Config{
		Seed: 1969, Days: 120, Purchases: 400,
		FieldWidth: 22, Prefix: "VISA", References: true,
		SettleMinDays: 1, SettleMaxDays: 6, UnsettledChance: 0.05,
		UpliftChance: 0.15, UpliftMaxPercent: 20,
		ReductionChance: 0.15, ReductionMaxPercent: 40,
		BranchChance: 0.15, DuplicateChance: 0.15,
	})

	prefixes := []string{"VISA", "MASTERCARD", "MC", "MAESTRO", "DEBIT", "KARTENZAHLUNG", "POS"}
	pol := budget.Policy{
		PayeePrefixes: prefixes, TolerancePercent: 25, ToleranceCents: 5000,
		Compare: payeematch.New(prefixes),
	}

	// Walked in date order with authorisations retired as they settle, because
	// the candidate count is the prior and a window that never empties makes the
	// prior swamp everything. An earlier version of this kept every
	// authorisation open for the whole statement, put two hundred rows in every
	// window, and reported that almost nothing is ever merged automatically —
	// a fact about the fixture, not about the model.
	rows := append([]linkagegen.Row(nil), hist.Rows...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Date.Before(rows[j].Date) })

	open := map[int]*budget.Transaction{}
	var out []observation
	for _, r := range rows {
		if r.Status == linkagegen.Pending {
			open[r.PurchaseID] = &budget.Transaction{
				ID: fmt.Sprintf("t-%d", r.PurchaseID), Date: r.Date,
				AmountCents: r.AmountCents, PayeeName: r.Payee,
			}
			continue
		}

		in := budget.ImportedFields{
			Date: r.Date, AmountCents: r.AmountCents, PayeeName: r.Payee, Cleared: true,
		}
		from, to := budget.WindowBounds(in.Date)
		var window []*budget.Transaction
		for _, c := range open {
			if !c.Date.Before(from) && c.Date.Before(to) {
				window = append(window, c)
			}
		}
		if scored := budget.Assess(window, in, nil, pol); len(scored) > 0 {
			// The count the prior was taken over, not the size of the window: the
			// two differ, and using the wrong one would measure a model nobody
			// ships.
			out = append(out, observation{cmp: scored[0].Comparison, candidates: scored[0].Plausible})
		}
		// Settled, so it is no longer on offer — which is what keeps the windows
		// the size a real one is.
		delete(open, r.PurchaseID)
	}

	if len(out) < 100 {
		t.Fatalf("the corpus holds %d comparisons; too few to say anything about "+
			"sensitivity", len(out))
	}
	return out
}

// verdict is what the model would do with one comparison, judged on probability
// alone.
//
// The margin is deliberately left out. It is a property of the arrangement of a
// whole batch rather than of the parameters, so folding it in would measure the
// batch the corpus happens to describe instead of the numbers under test.
func verdict(l budget.Linkage, o observation, auto, review, overlap float64) string {
	p := budget.Probability(l.Weight(o.cmp, o.candidates, overlap))
	switch {
	case p >= auto:
		return "adopt"
	case p >= review:
		return "hold"
	default:
		return "create"
	}
}

// perturb multiplies one entry of one distribution and rescales the rest so the
// distribution still sums to one.
//
// Rescaling the others is what makes this a question about the model rather than
// about arithmetic: the levels of a field are exhaustive, so claiming one
// happens three times as often is claiming the others happen correspondingly
// less. A perturbation that broke that would be measuring an incoherent model.
func perturb[K ~int](table map[K]float64, key K, factor float64) map[K]float64 {
	out := make(map[K]float64, len(table))
	v := table[key] * factor
	// A probability cannot be tripled past one, and clamping just under it would
	// crush every other level of the field to nothing and report that as
	// sensitivity to this one. The ceiling keeps the perturbed distribution a
	// distribution; the table then reports the value actually reached, so a
	// parameter that could not be moved far is not read as one that was moved
	// and did nothing.
	if v > 0.95 {
		v = 0.95
	}
	rest := 1 - table[key]
	scale := 1.0
	if rest > 0 {
		scale = (1 - v) / rest
	}
	for k, old := range table {
		if k == key {
			out[k] = v
		} else {
			out[k] = old * scale
		}
	}
	return out
}

type row struct {
	name      string
	value     float64
	up, down  float64
	flipsUp   int
	flipsDown int
	share     float64
}

// TestSensitivity_isTheReleaseGate measures how far the decisions actually
// depend on the parameters, and refuses a change to that until somebody has
// looked at it.
//
// Calibration cannot be measured before release: that would need ground truth
// from an installation running the thing. What can be measured is whether the
// answer would move if the numbers were wrong — and if it would not, the
// numbers being priors rather than estimates costs nothing. Where it would, the
// analysis names the level, and the answer to that is a wider review band at
// that point rather than a better guess.
func TestSensitivity_isTheReleaseGate(t *testing.T) {
	corpus := buildCorpus(t)
	base := budget.DefaultLinkage()
	const auto, review, overlap = 0.90, 0.50, 0.50

	want := make([]string, len(corpus))
	shares := map[string]int{}
	for i, o := range corpus {
		want[i] = verdict(base, o, auto, review, overlap)
		shares["payee:"+o.cmp.Payee.String()]++
		shares["amount:"+o.cmp.Amount.String()]++
		shares["date:"+o.cmp.Date.String()]++
	}

	count := func(l budget.Linkage) int {
		var n int
		for i, o := range corpus {
			if verdict(l, o, auto, review, overlap) != want[i] {
				n++
			}
		}
		return n
	}

	var rows []row
	var perturbations []struct {
		name    string
		linkage budget.Linkage
	}
	measure := func(field, level string, value, reachedUp, reachedDown float64, up, down budget.Linkage) {
		perturbations = append(perturbations,
			struct {
				name    string
				linkage budget.Linkage
			}{field + "." + level + " raised", up},
			struct {
				name    string
				linkage budget.Linkage
			}{field + "." + level + " lowered", down})
		rows = append(rows, row{
			name: field + "." + level, value: value,
			up: reachedUp, down: reachedDown,
			flipsUp: count(up), flipsDown: count(down),
			share: float64(shares[strings.SplitN(field, ".", 2)[0]+":"+level]) / float64(len(corpus)),
		})
	}

	const factor = 3.0
	for _, lv := range []budget.PayeeLevel{
		budget.PayeeMissing, budget.PayeeNone, budget.PayeeConflict, budget.PayeeSubset,
		budget.PayeeTruncated, budget.PayeeFuzzy, budget.PayeeExact,
	} {
		up, down := base, base
		up.PayeeM = perturb(base.PayeeM, lv, factor)
		down.PayeeM = perturb(base.PayeeM, lv, 1/factor)
		measure("payee.m", lv.String(), base.PayeeM[lv], up.PayeeM[lv], down.PayeeM[lv], up, down)

		up, down = base, base
		up.PayeeU = perturb(base.PayeeU, lv, factor)
		down.PayeeU = perturb(base.PayeeU, lv, 1/factor)
		measure("payee.u", lv.String(), base.PayeeU[lv], up.PayeeU[lv], down.PayeeU[lv], up, down)
	}
	for _, lv := range []budget.AmountLevel{
		budget.AmountOutsideLower, budget.AmountOutsideHigher,
		budget.AmountLowerWithin, budget.AmountHigherWithin, budget.AmountExact,
	} {
		up, down := base, base
		up.AmountM = perturb(base.AmountM, lv, factor)
		down.AmountM = perturb(base.AmountM, lv, 1/factor)
		measure("amount.m", lv.String(), base.AmountM[lv], up.AmountM[lv], down.AmountM[lv], up, down)

		up, down = base, base
		up.AmountU = perturb(base.AmountU, lv, factor)
		down.AmountU = perturb(base.AmountU, lv, 1/factor)
		measure("amount.u", lv.String(), base.AmountU[lv], up.AmountU[lv], down.AmountU[lv], up, down)
	}
	for _, lv := range []budget.DateLevel{
		budget.DateBeforeFar, budget.DateAfterFar, budget.DateBeforeNear,
		budget.DateAfterNear, budget.DateSame,
	} {
		up, down := base, base
		up.DateM = perturb(base.DateM, lv, factor)
		down.DateM = perturb(base.DateM, lv, 1/factor)
		measure("date.m", lv.String(), base.DateM[lv], up.DateM[lv], down.DateM[lv], up, down)

		up, down = base, base
		up.DateU = perturb(base.DateU, lv, factor)
		down.DateU = perturb(base.DateU, lv, 1/factor)
		measure("date.u", lv.String(), base.DateU[lv], up.DateU[lv], down.DateU[lv], up, down)
	}

	// How close the corpus sits to a threshold, which is the direct answer to
	// "would a wrong parameter change anything" and needs no perturbation at all.
	outcome := map[string]int{}
	var nearThreshold int
	for i, o := range corpus {
		outcome[want[i]]++
		w := base.Weight(o.cmp, o.candidates, overlap)
		for _, t := range []float64{auto, review} {
			edge := math.Log2(t / (1 - t))
			if math.Abs(w-edge) < 1 {
				nearThreshold++
				break
			}
		}
	}

	// The two cases the whole level scheme was built to separate, held against
	// every perturbation above. If one of them is fragile, the analysis says so
	// rather than the suite going quietly green.
	canonical := map[string]observation{
		"reported truncation (Da Luigi Roma / Visa Da Luigi)": {
			cmp: budget.Comparison{Payee: budget.PayeeTruncated,
				Amount: budget.AmountHigherWithin, Date: budget.DateAfterNear}, candidates: 3},
		"another branch (Da Luigi Roma / Visa Da Luigi Milano)": {
			cmp: budget.Comparison{Payee: budget.PayeeConflict,
				Amount: budget.AmountHigherWithin, Date: budget.DateAfterNear}, candidates: 3},
	}
	fragile := map[string][]string{}
	for name, o := range canonical {
		expect := verdict(base, o, auto, review, overlap)
		for _, p := range perturbations {
			if verdict(p.linkage, o, auto, review, overlap) != expect {
				fragile[name] = append(fragile[name], p.name)
			}
		}
		sort.Strings(fragile[name])
	}

	got := renderSensitivity(rows, len(corpus), outcome, nearThreshold, canonical, fragile, base, auto, review, overlap)
	path := sensitivityDoc
	if *updateSensitivity {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("rewrote %s", path)
		return
	}

	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v — run `go test ./budget/ -run TestSensitivity "+
			"-update-sensitivity` to create it", path, err)
	}
	if string(stored) != got {
		t.Errorf("%s no longer describes the shipped parameters.\n\n"+
			"This is the gate, not a nuisance: the table is the argument for the "+
			"thresholds, so a change to how far the decisions depend on the numbers "+
			"has to be read by somebody before it ships. Regenerate with\n"+
			"    go test ./budget/ -run TestSensitivity -update-sensitivity\n"+
			"and read the diff.", path)
	}
}

func renderSensitivity(
	rows []row, corpus int, outcome map[string]int, nearThreshold int,
	canonical map[string]observation, fragile map[string][]string,
	base budget.Linkage, auto, review, overlap float64,
) string {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].flipsUp+rows[i].flipsDown, rows[j].flipsUp+rows[j].flipsDown
		if a != b {
			return a > b
		}
		return rows[i].name < rows[j].name
	})

	lines := []string{
		"# Parameter sensitivity",
		"",
		"Generated by `go test ./budget/ -run TestSensitivity -update-sensitivity`.",
		"Do not edit by hand.",
		"",
		"Each parameter is multiplied and divided by three, with the rest of its",
		"distribution rescaled so it still sums to one, and the table counts how many",
		"decisions over a synthetic statement change as a result. A value that cannot be",
		"tripled without leaving the unit interval is raised as far as it goes instead,",
		"and the column says how far: a parameter that could not be moved much is not the",
		"same as one that was moved and did nothing.",
		"",
		"Decisions are judged on probability against the shipped thresholds of 90% and",
		"50%. The margin is left out because it is a property of a batch rather than of",
		"these numbers.",
		"",
		"A parameter that moves nothing is one whose being a stated prior rather than an",
		"estimate costs nothing. A parameter that moves a great deal is one where the",
		"honest answer to not knowing it is a wider review band, not a better guess.",
		"",
		"The corpus is every comparison the *model* would make, which is not every",
		"transaction: a bank reference or a pending-map entry settles most of them before",
		"the model is consulted at all. So the share held here is far above what an",
		"installation sees, and it is meant to be — the point is to stress the numbers,",
		"not to predict a queue length.",
		"",
		"One bit is a factor of two in the odds, and a parameter wrong by a factor of",
		"three moves a weight by 1.58 bits. A decision sitting within one bit of a",
		"threshold is therefore one that a single badly-guessed parameter could turn.",
		"",
		"",
	}
	var b strings.Builder
	b.WriteString(strings.Join(lines, "\n"))
	fmt.Fprintf(&b, "Corpus: %d comparisons — ", corpus)
	fmt.Fprintf(&b, "%d adopted, %d held, %d created.\n",
		outcome["adopt"], outcome["hold"], outcome["create"])
	fmt.Fprintf(&b, "%d of them (%.1f%%) sit within one bit of a threshold; the rest would "+
		"need\nthe parameters to be wrong by more than a bit to decide differently.\n\n",
		nearThreshold, 100*float64(nearThreshold)/float64(corpus))

	b.WriteString("## The two cases the level scheme exists for\n\n")
	names := make([]string, 0, len(canonical))
	for name := range canonical {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		o := canonical[name]
		p := budget.Probability(base.Weight(o.cmp, o.candidates, overlap))
		fmt.Fprintf(&b, "- **%s** — %.1f%%, %s.", name, 100*p, verdict(base, o, auto, review, overlap))
		if len(fragile[name]) == 0 {
			b.WriteString(" Keeps that decision under every perturbation below.\n")
		} else {
			fmt.Fprintf(&b, " Changes its decision under %d of them: %s.\n",
				len(fragile[name]), strings.Join(fragile[name], ", "))
		}
	}
	b.WriteString("\n## Every parameter\n\n")
	b.WriteString("A level with a small share of the corpus can still move many decisions: the\n")
	b.WriteString("levels of a field sum to one, so raising one lowers the rest, and the flips are\n")
	b.WriteString("theirs. Read a row as a question about the field, not only about the level.\n\n")
	b.WriteString("| Parameter | Value | Share | Raised to | Flips | Lowered to | Flips |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| `%s` | %.3f | %.1f%% | %.3f | %d | %.3f | %d |\n",
			r.name, r.value, 100*r.share, r.up, r.flipsUp, r.down, r.flipsDown)
	}
	return b.String()
}

// TestPerturb_keepsItADistribution is the property that makes the analysis a
// question about the model rather than about arithmetic.
//
// The levels of a field are exhaustive, so claiming one happens three times as
// often is claiming the others happen correspondingly less. A perturbation that
// broke the sum would measure an incoherent model and report the incoherence as
// sensitivity.
func TestPerturb_keepsItADistribution(t *testing.T) {
	base := budget.DefaultLinkage().PayeeM

	for _, factor := range []float64{3, 1.0 / 3, 1.5, 0.5} {
		got := perturb(base, budget.PayeeTruncated, factor)

		var sum float64
		for _, v := range got {
			sum += v
			if v <= 0 {
				t.Errorf("factor %.2f: a level fell to %v, which has no logarithm", factor, v)
			}
		}
		if math.Abs(sum-1) > 1e-9 {
			t.Errorf("factor %.2f: the distribution sums to %.6f", factor, sum)
		}
		if want := base[budget.PayeeTruncated] * factor; math.Abs(got[budget.PayeeTruncated]-want) > 1e-9 {
			t.Errorf("factor %.2f: the target moved to %.6f, want %.6f",
				factor, got[budget.PayeeTruncated], want)
		}
		// The others keep their proportions to one another; only their share of
		// what is left changes.
		if a, b := got[budget.PayeeExact]/got[budget.PayeeNone],
			base[budget.PayeeExact]/base[budget.PayeeNone]; math.Abs(a-b) > 1e-9 {
			t.Errorf("factor %.2f: two untouched levels changed relative to each other "+
				"(%.4f, was %.4f)", factor, a, b)
		}
	}

	// A value that cannot be tripled is raised as far as it goes and the
	// distribution still holds.
	got := perturb(base, budget.PayeeExact, 3)
	var sum float64
	for _, v := range got {
		sum += v
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Errorf("a capped perturbation left the distribution summing to %.6f", sum)
	}
}

// TestCorpus_looksLikeAStatementNotAnArchive guards the mistake this analysis
// was first built with.
//
// Authorisations have to be retired as they settle. Keeping them open for the
// whole statement puts two hundred rows in every candidate window, and the
// prior — which is one over the candidate count — then swamps every field the
// analysis is supposed to be about. The first run of this reported that almost
// nothing is ever merged automatically, which was a fact about the fixture.
func TestCorpus_looksLikeAStatementNotAnArchive(t *testing.T) {
	corpus := buildCorpus(t)

	var total, worst int
	for _, o := range corpus {
		total += o.candidates
		if o.candidates > worst {
			worst = o.candidates
		}
	}
	// The bar is on the window, because the count is now the window. It used to
	// be the number of candidates whose field evidence was already positive,
	// which is a much smaller number and a circular one — the same evidence
	// entered the score twice. Every row still open in the fortnight counts now,
	// so the figure to guard is whether the fixture's windows empty as purchases
	// settle, which is what it was always really about.
	//
	// Four hundred purchases over a hundred and twenty days is between three and
	// four a day, so a fifteen-day window would hold about fifty rows if none of
	// them ever settled. Anything near that means the fixture has become an
	// archive and the prior is being measured instead of the parameters.
	mean := float64(total) / float64(len(corpus))
	if mean > 25 {
		t.Errorf("a comparison weighs %.1f candidates on average and up to %d; a "+
			"fortnight of one account does not hold that many open authorisations, so "+
			"the windows are not emptying and the prior is being measured rather than "+
			"the parameters", mean, worst)
	}
}
