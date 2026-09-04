package budget

import "sort"

// Anchor is one case whose outcome is a documented promise rather than an
// accident of the parameters.
//
// The set is small on purpose. An anchor is not a test of the model; it is a
// statement that a particular kind of transaction is handled a particular way,
// and every one of them appears in the README as a sentence somebody could hold
// this program to. A candidate parameter set that moves one has changed what the
// program is for, whatever its Brier score says.
//
// Each sits clear of a threshold in probability — the closest is 0.089 away — but
// that is the weaker of the two readings and the file used to give only it. In
// the model's own units two of these sit 0.52 and 0.74 bits from the review
// threshold, so a single comparison level moving by half a bit flips them. The
// margin is in probability; the sensitivity is in bits.
//
// An anchor must also be the case it names. The truncating-bank anchor used to
// pair truncation with an exact amount, worth +5.129 bits, which nearly carried
// the case on its own while the failure it exists to catch involves an amount
// that only agrees within tolerance, worth +0.737. That is 4.4 bits of slack, and
// it meant the anchor stayed green — P = 0.944, still adopted — at the exact
// point where the case it describes had already fallen to P = 0.446 and was being
// silently created as a duplicate. An anchor with slack in it is not a promise,
// it is a decoration.
type Anchor struct {
	Name string

	// Why is what this case is a promise about, in the words the README uses.
	Why string

	Comparison Comparison

	// Candidates is how many plausible rows the prior was taken over, which is
	// part of the case: the same comparison in a crowded window is a different
	// claim.
	Candidates int

	// Want is the outcome the shipped parameters produce and any candidate set
	// must go on producing: "adopted", "held" or "created".
	Want string
}

// Anchors returns the cases a candidate parameter set must still decide as
// documented.
func Anchors() []Anchor {
	return []Anchor{
		{
			Name: "the ordinary settlement",
			Why: "an authorisation and its booking agreeing on name, amount and day " +
				"is the commonest merge there is, and if this one ever needs asking " +
				"about the matcher has stopped being useful",
			Comparison: Comparison{Payee: PayeeExact, Amount: AmountExact, Date: DateSame},
			Candidates: 1,
			Want:       "adopted",
		},
		{
			Name: "the truncating bank",
			Why: "a bank that cuts the payee to fit its field still settles its own " +
				"authorisations, and recognising that is the reason the truncated " +
				"level exists at all",
			// The amount agrees within tolerance rather than to the cent, because
			// a truncating bank is usually a card scheme and a card scheme is
			// where the settled amount drifts from the authorised one. Pairing
			// truncation with an exact amount instead made this anchor a test of
			// the amount field, which was never in doubt.
			//
			// One candidate rather than two, and that narrowing is the price of
			// the same correction. With the truncated level no longer outweighing
			// a verbatim payee match, this case in a two-candidate window reaches
			// 0.909 — over the automatic threshold but only just, which is the one
			// thing an anchor may not be. What is promised here is that a bank
			// cutting its payee field still has its own settlement recognised, and
			// that promise is about the pair, not about how crowded the fortnight
			// was. The crowded version belongs in the sensitivity table, which
			// counts near-threshold cases on purpose.
			Comparison: Comparison{Payee: PayeeTruncated, Amount: AmountHigherWithin, Date: DateAfterNear},
			Candidates: 1,
			Want:       "adopted",
		},
		{
			Name: "the authorisation with no payee",
			Why: "many institutions send a pending row with no name on it, and an " +
				"exact amount on the same day is what identifies it",
			Comparison: Comparison{Payee: PayeeMissing, Amount: AmountExact, Date: DateSame},
			Candidates: 1,
			Want:       "adopted",
		},
		{
			Name: "the payee replaced outright",
			Why: "when the second row carries an unrelated name, the amount and the " +
				"day must not be enough to merge on unasked",
			Comparison: Comparison{Payee: PayeeNone, Amount: AmountExact, Date: DateSame},
			Candidates: 2,
			Want:       "held",
		},
		{
			Name: "the drifted amount a week out",
			Why: "the same name is not enough when the amount has moved and the " +
				"settlement is far away; this is a question, not a merge",
			Comparison: Comparison{Payee: PayeeExact, Amount: AmountLowerWithin, Date: DateAfterFar},
			Candidates: 3,
			Want:       "held",
		},
		{
			Name: "the unrelated payment to a familiar payee",
			Why: "a shop somebody uses every week produces rows that agree on the " +
				"name and nothing else, and those are two payments",
			Comparison: Comparison{Payee: PayeeExact, Amount: AmountOutsideHigher, Date: DateAfterFar},
			Candidates: 2,
			Want:       "created",
		},
	}
}

// decide runs one anchor through the real decision function, so that a change to
// the thresholds or to Decide itself moves an anchor exactly as a change to the
// level tables would.
func (a Anchor) decide(pol Policy) string {
	scored := []Candidate{{
		Comparison: a.Comparison,
		Weight:     pol.linkage().Weight(a.Comparison, a.Candidates, pol.overlap()),
		Plausible:  a.Candidates,
	}}
	scored[0].Probability = pol.calibration().Probability(scored[0].Weight)

	best, auto := pol.Decide(scored)
	switch {
	case best == nil:
		return "created"
	case auto:
		return "adopted"
	default:
		return "held"
	}
}

// sortByWeight puts settled decisions into a total order, so that the two halves
// of an evaluation depend on the evidence and not on the order it arrived in.
//
// The tie-break is the point. Weight alone is not a total order — a corpus is
// full of decisions that compared the same levels against the same number of
// candidates — and a stable sort would then leave the split to the input order,
// which is how a gate turns into something that can be asked repeatedly until it
// says yes.
func sortByWeight(l Linkage, in []LabelledDecision, overlap float64) {
	weight := func(d LabelledDecision) float64 { return l.Weight(d.Comparison, d.Candidates, overlap) }
	sort.Slice(in, func(i, j int) bool {
		a, b := in[i], in[j]
		if wa, wb := weight(a), weight(b); wa != wb {
			return wa < wb
		}
		if a.Comparison.Payee != b.Comparison.Payee {
			return a.Comparison.Payee < b.Comparison.Payee
		}
		if a.Comparison.Amount != b.Comparison.Amount {
			return a.Comparison.Amount < b.Comparison.Amount
		}
		if a.Comparison.Date != b.Comparison.Date {
			return a.Comparison.Date < b.Comparison.Date
		}
		if a.Candidates != b.Candidates {
			return a.Candidates < b.Candidates
		}
		return !a.Match && b.Match
	})
}
