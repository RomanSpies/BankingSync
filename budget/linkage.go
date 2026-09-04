package budget

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PayeeLevel is how two payee spellings relate, as one of a fixed set of
// situations rather than a position on a scale.
//
// This is a comparison level in the sense of the Fellegi-Sunter model of record
// linkage: the comparison of a field yields a category, and the category carries
// a weight derived from how often it occurs among true matches against how often
// it occurs by chance. Splitting the comparison this way is what lets "the
// booked name is the pending one with a word cut off" and "the booked name
// contradicts the pending one" count as different evidence, which no single
// similarity number can express — measured, "Shell Tankstelle"/"Shell" and
// "Da Luigi Roma"/"Da Luigi Milano" score identically under Sørensen-Dice, and
// no weighting of Dice against Jaro-Winkler separates them either.
//
// The order of these constants carries **no meaning**. It is not a ranking, and
// nothing may compare levels with < or >. How much each one is worth is the
// weight table's business, and that table is calibrated rather than assumed.
type PayeeLevel int

const (
	// PayeeMissing is one or both sides having no name at all. It is a level of
	// its own because absent information is not disagreement — a distinction the
	// previous rule got wrong by returning "does not agree" for an empty name.
	PayeeMissing PayeeLevel = iota

	// PayeeNone is two names with no word in common.
	PayeeNone

	// PayeeConflict is names that share at least one word while at least one word
	// of the shorter name has no counterpart. The same chain in another city
	// lands here: "Da Luigi Roma" against "Da Luigi Milano".
	PayeeConflict

	// PayeeSubset is the shorter name being fully accounted for in the longer
	// one, with several words left unexplained. A brand inside a branch name
	// ("Aldi" in "Aldi Süd Nürnberg") and a meaningless fragment ("Da" in "Da
	// Luigi Roma") are the same shape and therefore the same level; no metric
	// separates them, and pretending otherwise would be inventing evidence.
	PayeeSubset

	// PayeeTruncated is the shorter name being fully accounted for in the longer
	// one with exactly one word left over — the shape a bank produces when it
	// prepends a card scheme and then cuts the tail to fit its field. This is the
	// reported case: "Da Luigi Roma" booked as "Visa Da Luigi".
	PayeeTruncated

	// PayeeFuzzy is every word paired but at least one only approximately, so the
	// names are the same up to spelling.
	PayeeFuzzy

	// PayeeExact is equality once card scheme prefixes, case, spacing and umlaut
	// transliteration are discounted.
	PayeeExact
)

func (l PayeeLevel) String() string {
	switch l {
	case PayeeMissing:
		return "missing"
	case PayeeNone:
		return "none"
	case PayeeConflict:
		return "conflict"
	case PayeeSubset:
		return "subset"
	case PayeeTruncated:
		return "truncated"
	case PayeeFuzzy:
		return "fuzzy"
	case PayeeExact:
		return "exact"
	default:
		return "unknown"
	}
}

// AmountLevel is how two amounts relate, and in which direction.
//
// The middle levels exist because a card pre-authorisation routinely settles at
// a different figure than it reserved. The direction is split out because the
// two are different mechanisms: a restaurant adds a tip and a hotel adds
// incidentals, so the booking exceeds the authorisation; a fuel pump reserves a
// round sum and releases the remainder, so it falls short. How often each
// happens is not known here, and the shipped parameters say so — see
// DefaultLinkage.
//
// The order of these constants carries no meaning, as with PayeeLevel.
type AmountLevel int

const (
	// AmountOutsideLower and AmountOutsideHigher are beyond the configured
	// tolerance, below and above the authorised figure respectively.
	AmountOutsideLower AmountLevel = iota
	AmountOutsideHigher

	// AmountLowerWithin is a booking inside tolerance but below what was
	// authorised — the shape of a fuel release. AmountHigherWithin is above it,
	// the shape of a tip.
	AmountLowerWithin
	AmountHigherWithin

	AmountExact
)

func (l AmountLevel) String() string {
	switch l {
	case AmountExact:
		return "exact"
	case AmountHigherWithin:
		return "higher_within"
	case AmountLowerWithin:
		return "lower_within"
	case AmountOutsideHigher:
		return "outside_higher"
	default:
		return "outside_lower"
	}
}

// Within reports whether the level is one of the two inside tolerance, for the
// places that care that a booking is close rather than which side it is on.
func (l AmountLevel) Within() bool {
	return l == AmountHigherWithin || l == AmountLowerWithin
}

// DateLevel is how far apart two transactions are dated, and on which side.
//
// A settlement usually follows its authorisation within a few days; beyond that
// the proximity says progressively less, and the window itself is the outer
// bound. The direction is split out because a booking dated *before* the
// authorisation it would settle is a different thing from one dated after — but
// how different is not known here. Some institutions book with the original
// transaction date while the authorisation carries the date the hold appeared,
// which makes "before" ordinary rather than contradictory; others do not. The
// shipped parameters make no claim either way.
//
// "After" means the incoming transaction is dated after the candidate.
type DateLevel int

const (
	DateBeforeFar DateLevel = iota
	DateAfterFar
	DateBeforeNear
	DateAfterNear
	DateSame
)

func (l DateLevel) String() string {
	switch l {
	case DateSame:
		return "same"
	case DateAfterNear:
		return "after_near"
	case DateBeforeNear:
		return "before_near"
	case DateAfterFar:
		return "after_far"
	default:
		return "before_far"
	}
}

// Near reports whether the two dates are within nearDays of one another, either
// way round.
func (l DateLevel) Near() bool {
	return l == DateAfterNear || l == DateBeforeNear
}

// nearDays is how many days apart two rows may be and still count as close. Card
// settlements land within a few days of the authorisation; the fifteen-day
// window is deliberately wider than that so a late one is still a candidate, but
// it should count for less.
const nearDays = 3

// ClassifyAmount places two amounts on the scale, using the same tolerance the
// operator configures.
func ClassifyAmount(authorised, booked int64, tolerancePercent int, toleranceCents int64) AmountLevel {
	if authorised == booked {
		return AmountExact
	}
	// Direction is measured on what was spent, not on the sign: a debit is
	// negative, so a larger payment is a smaller number, and a refund mirrors
	// that. Comparing magnitudes says "more money" in both cases.
	higher := abs64(booked) > abs64(authorised)

	tol := Policy{TolerancePercent: tolerancePercent, ToleranceCents: toleranceCents}
	if tol.withinTolerance(authorised, booked) {
		if higher {
			return AmountHigherWithin
		}
		return AmountLowerWithin
	}
	if higher {
		return AmountOutsideHigher
	}
	return AmountOutsideLower
}

// ClassifyDate places two dates on the scale. candidate is the row already in
// the budget, incoming the transaction being placed; "after" means the incoming
// one is the later of the two, which is the ordinary direction of a settlement.
func ClassifyDate(candidate, incoming time.Time) DateLevel {
	d := dayDistance(candidate, incoming)
	if d == 0 {
		return DateSame
	}
	after := incoming.After(candidate)
	if d <= nearDays {
		if after {
			return DateAfterNear
		}
		return DateBeforeNear
	}
	if after {
		return DateAfterFar
	}
	return DateBeforeFar
}

// Comparison is one candidate weighed against one incoming transaction, field by
// field.
type Comparison struct {
	Payee  PayeeLevel
	Amount AmountLevel
	Date   DateLevel

	// PayeeFrequency is how large a share of this account's traffic the payee
	// holds, already floored by whoever measured it. Zero means unmeasured, and
	// unmeasured means no correction rather than maximum evidence.
	//
	// It only bears on an exact agreement: agreeing on a name the account sees
	// every week says far less than agreeing on one it has seen once, and the
	// base parameters cannot express that because they are a property of the
	// level, not of the value.
	PayeeFrequency float64
}

// Linkage is the Fellegi-Sunter parameter set: for each level of each field, how
// often it occurs among true matches (m) against how often it occurs by chance
// (u). The evidence a level carries is log2(m/u), and the evidence of a whole
// comparison is the sum, which is what lets a strong payee agreement make up for
// a weaker amount agreement instead of each field having to pass its own gate.
//
// Within a field the levels are exhaustive and mutually exclusive, so each of m
// and u is a probability distribution and has to sum to one. That constraint is
// what keeps these numbers from being free parameters: raising one level's share
// takes it from another, and the question "how often does this happen?" has to
// be answered for every case rather than only the convenient ones.
//
// The values below are stated priors, not estimates. Each is a claim that can be
// argued with, and every one is written out at its definition. They are due to be
// replaced by estimates from the review queue's recorded decisions, which is the
// standard EM route and the reason those decisions are recorded at all.
type Linkage struct {
	PayeeM, PayeeU   map[PayeeLevel]float64
	AmountM, AmountU map[AmountLevel]float64
	DateM, DateU     map[DateLevel]float64
}

// DefaultLinkage returns the shipped parameters.
func DefaultLinkage() Linkage {
	return Linkage{
		// m: of two rows that ARE the same payment, how does the bank spell the
		// payee the second time?
		PayeeM: map[PayeeLevel]float64{
			PayeeExact:     0.55, // most banks repeat the name verbatim
			PayeeTruncated: 0.08, // some prepend a scheme and cut the tail
			PayeeFuzzy:     0.05, // spelling drift and typos
			PayeeSubset:    0.13, // the name collapses to the brand
			// For a true match to contradict a word the bank must have replaced
			// one rather than dropped it — dropping is PayeeTruncated. That is
			// rare, and it is what makes a contradiction worth listening to:
			// calibrated so that a different branch of the same chain falls short
			// of review rather than balancing on the threshold.
			PayeeConflict: 0.02,
			PayeeNone:     0.02, // replaced outright
			PayeeMissing:  0.15, // one side carries no payee at all
		},
		// u: of two rows that are NOT the same payment but sit in the same
		// fifteen-day window on one account, how often does that happen anyway?
		// Note PayeeExact is not rare — a weekly supermarket run produces it.
		PayeeU: map[PayeeLevel]float64{
			PayeeExact: 0.050,
			// Ten in a thousand, not four. At four this level was worth +4.322
			// bits against exact agreement's +3.459, so a pair whose payee matched
			// verbatim was held for review while the truncated spelling of the
			// same pair merged unasked — the model preferring a partial agreement
			// to a total one. No published rule forbids that; Fellegi and Sunter
			// sort by m/u as a construction step rather than constraining it, and
			// neither Winkler nor Splink checks for it. It is simply not what
			// anybody means by a truncated match.
			//
			// The replacement is not measured either, and saying so matters more
			// than the number. Estimated against the synthetic generator, this
			// rate came out near 0.09 — but the same measurement gives 0.094 with
			// the generator's truncation switched off entirely, which shows it is
			// counting collisions between a twelve-name merchant pool rather than
			// truncations. That result refutes 0.004 without supplying a
			// replacement. Ten in a thousand is the value that removes the
			// inversion, log2(0.08/0.010) = +3.000 against exact's +3.459, and it
			// stays a stated claim until an installation's own u sample settles it
			// from level_observations, which is where this number should come from
			// in the end.
			PayeeTruncated: 0.010,
			PayeeFuzzy:     0.006,
			PayeeSubset:    0.020,
			PayeeConflict:  0.120,
			// The six thousandths that PayeeTruncated gained come from here, which
			// is the level with the most mass and the least precision behind it:
			// "the payee was replaced outright" is the residual of this
			// distribution rather than a measured quantity, and it is the only
			// entry that can absorb a change without pretending to more accuracy
			// than it has.
			PayeeNone:    0.644,
			PayeeMissing: 0.150,
		},

		// The directional levels are split evenly, and the even split is itself
		// the claim: nothing here knows whether a true match settles above its
		// authorisation more often than below. Splitting m and u by the same
		// fraction leaves every ratio — and so every scored decision — exactly as
		// it was, which means the distinction is recorded now and separated later,
		// once the decision log has counted it. Inventing the difference instead is
		// the one thing this model exists to avoid.
		//
		// "Every decision" is too broad and this comment used to say it. The
		// scoring path is bit-identical, verified in double precision. The inquiry
		// sampler is not: halving a level's prior mass halves its pseudo-count and
		// so raises its variance, by between two and seven times across these
		// levels, and the sampler ranks questions by that variance. So a fresh
		// installation with no observations at all asks about different decisions
		// after the split than before it. That is a consequence worth knowing about
		// and not a reason against the split.
		AmountM: map[AmountLevel]float64{
			AmountExact:         0.70,  // most authorisations settle at the figure reserved
			AmountHigherWithin:  0.125, // tips, hotel incidentals
			AmountLowerWithin:   0.125, // fuel releases
			AmountOutsideHigher: 0.025,
			AmountOutsideLower:  0.025,
		},
		// The amount has to discriminate more sharply than the date does, and that
		// is a claim about bank data rather than a convenience: a bank does not
		// alter an amount at random, while a settlement date shifts by days as a
		// matter of course. Calibrated against the existing suite, where a row
		// matching to the cent six days away has to beat one a euro out on the
		// same day.
		AmountU: map[AmountLevel]float64{
			AmountExact:         0.02,
			AmountHigherWithin:  0.075,
			AmountLowerWithin:   0.075,
			AmountOutsideHigher: 0.415,
			AmountOutsideLower:  0.415,
		},

		// Split the same way and for the same reason. A booking dated before the
		// authorisation it settles is either impossible or routine depending on
		// which date the institution reports, and this file cannot know which.
		DateM: map[DateLevel]float64{
			DateSame:       0.40,
			DateAfterNear:  0.225, // settlement usually lands within three days
			DateBeforeNear: 0.225,
			DateAfterFar:   0.075, // fuel and hotel holds can take a week to clear
			DateBeforeFar:  0.075,
		},
		DateU: map[DateLevel]float64{
			DateSame:       0.15,
			DateAfterNear:  0.15,
			DateBeforeNear: 0.15,
			DateAfterFar:   0.275,
			DateBeforeFar:  0.275,
		},
	}
}

// Weight returns the Fellegi-Sunter match weight in bits:
//
//	M = log2(λ/(1−λ)) + Σ log2(mᵢ/uᵢ)
//
// candidates is how many rows the window offered. It sets the prior: at most one
// of them is the settlement being looked for, so a window holding forty rows is
// a weaker starting point than one holding two. Taking the prior from the data
// rather than fixing it means a busy account does not quietly become easier to
// match in.
func (l Linkage) Weight(c Comparison, candidates int, overlap float64) float64 {
	lambda, none := prior(candidates, overlap)

	w := math.Log2(lambda / none)
	w += evidence(l.PayeeM[c.Payee], l.PayeeU[c.Payee])
	w += l.tfCorrection(c)
	w += evidence(l.AmountM[c.Amount], l.AmountU[c.Amount])
	w += evidence(l.DateM[c.Date], l.DateU[c.Date])
	return w
}

// prior is the chance a given candidate is the match before any field is looked
// at: one of n candidates, or none of them.
//
// The overlap is the operator's claim about the institution rather than an
// estimate of anything: of the transactions that reach the matcher at all — most
// are settled by the bank's own reference long before it is consulted — what
// share have a counterpart somewhere in the window. Given n candidates and that
// share pi, a *specific* candidate carries a prior of pi/n, and "none of them"
// carries 1 - pi.
//
//	prior(n) = pi/n,  and the term is log2((pi/n)/(1 - pi))
//
// The denominator is 1 - pi and not 1 - pi/n, because the alternative this weight
// is a likelihood ratio against is "no counterpart at all". That another candidate
// is the counterpart is a third possibility and it is answered by the competition
// term in Assess, not by shrinking this one.
//
// This was 1/(n+1) fed through log2(lambda/(1-lambda)), and the arithmetic of that
// was already right. It comes to log2(1/n), which is exactly the expression above
// at pi = 1/2 — checked at every candidate count from one to forty, agreeing to
// within 4e-16. What was wrong was the sentence describing it. Read as a flat
// prior over "one of the n, or none", 1/(n+1) asserts an 89% prior probability of
// a counterpart at eight candidates and drives P(new) to zero as the window fills,
// which is the opposite of the claim that most incoming transactions are simply
// new; read as the odds of one candidate against no counterpart at all, it is the
// bipartite prior at an even overlap. The formula was doing the second thing while
// the comment described the first.
//
// So what pi adds is not a correction but a statement an operator can make: at a
// half this is bit-identical to what shipped before, and moving it says how often
// a transaction reaching the matcher has a counterpart at all. Note that pi alone
// would be a pure intercept — it shifts every weight by log2(pi/(1-pi)) whatever n
// is, which is to say it would only duplicate the thresholds. It stops being an
// intercept once the competition term is there, because pi then sets the balance
// between "no counterpart" and "a different one".
func prior(candidates int, overlap float64) (match, none float64) {
	if candidates < 1 {
		candidates = 1
	}
	if overlap <= 0 || overlap >= 1 {
		overlap = defaultOverlap
	}
	return overlap / float64(candidates), 1 - overlap
}

// evidence is log2(m/u), the weight one observation contributes.
//
// A level neither seen among matches nor by chance contributes nothing rather
// than an infinity: an unparameterised level is missing information, and missing
// information must not decide anything.
func evidence(m, u float64) float64 {
	if m <= 0 || u <= 0 {
		return 0
	}
	return math.Log2(m / u)
}

// tfCorrection is the frequency adjustment for an exact payee agreement, in
// bits.
//
// It is added to the payee's weight rather than substituted for its u, which is
// the difference between a correction and a hole in the model: replacing u for
// one level would leave the level distribution no longer summing to one, and
// that constraint is the only thing keeping these parameters from being free
// numbers. Splink applies it the same way and for the same reason.
//
// A payee more common than the level's base u earns less than the base weight; a
// rarer one earns more. The caller supplies a floored frequency, and the floor
// has to be a constant. It used to be two divided by the number of rows on the
// statement, which sounds like a floor and is not one: it falls as the account
// accumulates history, so the cap loosens exactly as the data that could trip it
// grows. Measured across statement sizes it removed a fixed one bit — a factor of
// two — and left the payee term reaching +11.1 bits at eight thousand rows,
// against 6.544 for the whole of the rest of the model at its strongest. The
// stated purpose, keeping the correction from becoming a gate, was not achieved
// at any size. Splink's tf_minimum_u_value is a fixed constant for this reason.
func (l Linkage) tfCorrection(c Comparison) float64 {
	if c.Payee != PayeeExact || c.PayeeFrequency <= 0 {
		return 0
	}
	base := l.PayeeU[PayeeExact]
	if base <= 0 {
		return 0
	}
	return math.Log2(base / c.PayeeFrequency)
}

// Probability converts a match weight in bits to P(match | what was observed).
//
// Branched on the sign so that neither side ever exponentiates a large positive
// number. Computing 2^w/(1+2^w) directly overflows to infinity at w = 1024 and
// returns Inf/Inf, which is NaN — and a NaN probability is not merely a wrong
// answer here: Reliability multiplies it by the bin count and indexes an array
// with the result, so it takes the process down. Weights that large need a
// parameter set with a level driven to an extreme, which is exactly the sort a
// refit on narrow evidence produces and the promotion gate exists to catch, so
// the gate has to survive scoring one.
func Probability(weight float64) float64 {
	if weight >= 0 {
		return 1 / (1 + math.Exp2(-weight))
	}
	p := math.Exp2(weight)
	return p / (1 + p)
}

// Explain lists what each field contributed, so a decision put in front of a
// person can say why rather than only how much.
func (l Linkage) Explain(c Comparison, candidates int, overlap float64) []FieldEvidence {
	lambda, none := prior(candidates, overlap)
	return []FieldEvidence{
		{Field: "prior", Level: "1 of " + itoa(candidates), Bits: math.Log2(lambda / none)},
		{Field: "payee", Level: c.Payee.String(),
			Bits: evidence(l.PayeeM[c.Payee], l.PayeeU[c.Payee]) + l.tfCorrection(c)},
		{Field: "amount", Level: c.Amount.String(), Bits: evidence(l.AmountM[c.Amount], l.AmountU[c.Amount])},
		{Field: "date", Level: c.Date.String(), Bits: evidence(l.DateM[c.Date], l.DateU[c.Date])},
	}
}

// FieldEvidence is one field's contribution to a match weight.
type FieldEvidence struct {
	Field string
	Level string
	Bits  float64
}

func itoa(n int) string { return strconv.Itoa(n) }

// Version identifies the parameter set that decided something.
//
// Without it every reading of the decision log mixes models: a run of figures
// spanning a threshold change describes two different programs, and no
// comparison across it means anything. It is also what lets a decision made on a
// page drawn under one set of parameters be refused when the parameters have
// moved since — the probability alone cannot show that, because a threshold
// change does not move any probability at all.
//
// A hash rather than a number because nobody increments a number reliably. Every
// figure that bears on a decision goes into it, so forgetting to bump a version
// is not a thing that can happen.
// ClassificationVersion identifies what decides which comparison level a pair
// reaches: the amount tolerance, the payee prefixes and the width of the near
// date window.
//
// Deliberately narrower than Version. A tally of comparison levels stays valid
// across anything that leaves the classification alone — and that includes the
// two things most likely to move. Promoting a fitted parameter set changes what
// a level is worth, not which level a pair falls into; moving a threshold
// changes what is done with the evidence, not what the evidence is. Scoping such
// a tally to the whole parameter version would throw it away on exactly the
// events that do not invalidate it, and a promotion would discard the sample
// that made it possible.
func ClassificationVersion(tolerancePct int, toleranceCents int64, prefixes []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "tol=%d/%d near=%d\n", tolerancePct, toleranceCents, nearDays)

	sorted := append([]string(nil), prefixes...)
	sort.Strings(sorted)
	fmt.Fprintf(h, "prefixes=%s\n", strings.Join(sorted, ","))

	return hex.EncodeToString(h.Sum(nil))[:12]
}

func (l Linkage) Version(auto, review, margin, overlap float64, tolerancePct int,
	toleranceCents int64, prefixes []string, calA, calB float64) string {
	h := sha256.New()

	writeLevels(h, "payee.m", l.PayeeM)
	writeLevels(h, "payee.u", l.PayeeU)
	writeLevels(h, "amount.m", l.AmountM)
	writeLevels(h, "amount.u", l.AmountU)
	writeLevels(h, "date.m", l.DateM)
	writeLevels(h, "date.u", l.DateU)

	fmt.Fprintf(h, "auto=%.6f review=%.6f margin=%.6f overlap=%.6f\n", auto, review, margin, overlap)
	fmt.Fprintf(h, "tol=%d/%d near=%d\n", tolerancePct, toleranceCents, nearDays)
	// A rescaling changes every probability without changing a single level, so
	// leaving it out would let two decisions made under different models claim
	// the same identity.
	fmt.Fprintf(h, "cal=%.9f/%.9f\n", calA, calB)

	sorted := append([]string(nil), prefixes...)
	sort.Strings(sorted)
	fmt.Fprintf(h, "prefixes=%s\n", strings.Join(sorted, ","))

	return hex.EncodeToString(h.Sum(nil))[:12]
}

// writeLevels feeds one level table to the hash in an order that does not depend
// on how Go happened to lay the map out.
func writeLevels[K ~int](h io.Writer, name string, table map[K]float64) {
	keys := make([]int, 0, len(table))
	for k := range table {
		keys = append(keys, int(k))
	}
	sort.Ints(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "%s[%d]=%.9f\n", name, k, table[K(k)])
	}
}

// LevelWeights lists what every comparison level is currently worth, in bits.
//
// The same numbers the model decides on, in a form something else can read.
// Today they are constant; the point is that they need not stay so, and a refit
// that moved one would otherwise be visible only to whoever read the commit.
func (l Linkage) LevelWeights() []FieldEvidence {
	var out []FieldEvidence
	add := func(field, level string, m, u float64) {
		out = append(out, FieldEvidence{Field: field, Level: level, Bits: evidence(m, u)})
	}
	for _, lv := range []PayeeLevel{PayeeMissing, PayeeNone, PayeeConflict, PayeeSubset,
		PayeeTruncated, PayeeFuzzy, PayeeExact} {
		add("payee", lv.String(), l.PayeeM[lv], l.PayeeU[lv])
	}
	for _, lv := range []AmountLevel{AmountOutsideLower, AmountOutsideHigher,
		AmountLowerWithin, AmountHigherWithin, AmountExact} {
		add("amount", lv.String(), l.AmountM[lv], l.AmountU[lv])
	}
	for _, lv := range []DateLevel{DateBeforeFar, DateAfterFar, DateBeforeNear,
		DateAfterNear, DateSame} {
		add("date", lv.String(), l.DateM[lv], l.DateU[lv])
	}
	return out
}
