package budget

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

func cand(id string, weight float64) Candidate {
	return Candidate{
		Transaction: &Transaction{ID: id, Date: day(2026, time.July, 10)},
		Weight:      weight,
		Probability: Probability(weight),
	}
}

func chosen(a Assignment) string {
	if a.Candidate == nil {
		return ""
	}
	return a.Candidate.Transaction.ID
}

// TestHungarian_findsTheCheapestArrangement checks the solver against a matrix
// small enough to work out by hand, because everything above it trusts this.
func TestHungarian_findsTheCheapestArrangement(t *testing.T) {
	// The greedy choice is row 0 to column 0 at cost 1, which forces row 1 onto
	// column 1 at 9 for a total of 10. The optimum is 2 + 3 = 5.
	got := hungarian([][]float64{
		{1, 2},
		{3, 9},
	})
	if got[0] != 1 || got[1] != 0 {
		t.Errorf("got rows assigned to columns %v, want [1 0] — the greedy pick was taken", got)
	}
}

// TestHungarian_matchesABruteForceSearch is the property test the hand-worked
// example cannot give: on small random matrices the optimum is enumerable, and
// an implementation with a subtle error in its potentials shows up here and
// nowhere else.
func TestHungarian_matchesABruteForceSearch(t *testing.T) {
	r := rand.New(rand.NewPCG(17, 23))

	for trial := 0; trial < 200; trial++ {
		n := 1 + r.IntN(5)
		a := make([][]float64, n)
		for i := range a {
			a[i] = make([]float64, n)
			for j := range a[i] {
				a[i][j] = float64(r.IntN(40)) - 20
			}
		}

		var want float64
		perm := make([]int, n)
		for i := range perm {
			perm[i] = i
		}
		first := true
		var permute func(k int)
		permute = func(k int) {
			if k == n {
				var sum float64
				for i, j := range perm {
					sum += a[i][j]
				}
				if first || sum < want {
					want, first = sum, false
				}
				return
			}
			for i := k; i < n; i++ {
				perm[k], perm[i] = perm[i], perm[k]
				permute(k + 1)
				perm[k], perm[i] = perm[i], perm[k]
			}
		}
		permute(0)

		var got float64
		for i, j := range hungarian(a) {
			got += a[i][j]
		}
		if math.Abs(got-want) > 1e-9 {
			t.Fatalf("trial %d (n=%d): solver found %.0f, brute force %.0f", trial, n, got, want)
		}
	}
}

// TestSolve_takesBothWhenBothFit is the multiplicity case, and the single
// clearest reason for weighing a batch together.
//
// Two identical bookings against two identical authorisations: judged pair by
// pair every combination scores the same and nothing can choose, so both go to a
// person. Under the constraint there is exactly one arrangement that uses both
// authorisations, and it is the best one.
func TestSolve_takesBothWhenBothFit(t *testing.T) {
	rows := [][]Candidate{
		{cand("auth-a", 9), cand("auth-b", 9)},
		{cand("auth-a", 9), cand("auth-b", 9)},
	}
	got := Solve(rows, 0)

	if chosen(got[0]) == "" || chosen(got[1]) == "" {
		t.Fatalf("one booking was left unpaired: %q and %q", chosen(got[0]), chosen(got[1]))
	}
	if chosen(got[0]) == chosen(got[1]) {
		t.Errorf("both bookings claimed %q; the one-to-one constraint did not hold",
			chosen(got[0]))
	}
}

// TestSolve_leavesTheSpareOneUnpaired covers the other side of multiplicity:
// two bookings, one authorisation. One of them is genuinely new, and which is
// not decidable — so the arrangement pairs one and leaves the other, and the
// margin says the choice was arbitrary.
func TestSolve_leavesTheSpareOneUnpaired(t *testing.T) {
	rows := [][]Candidate{
		{cand("auth-a", 9)},
		{cand("auth-a", 9)},
	}
	got := Solve(rows, 0)

	paired := 0
	for _, a := range got {
		if a.Candidate != nil {
			paired++
		}
	}
	if paired != 1 {
		t.Fatalf("%d of two bookings were paired to one authorisation, want 1", paired)
	}
	for _, a := range got {
		if a.Candidate != nil && a.Margin > 1 {
			t.Errorf("the pairing claims a margin of %.2f bits, but the other booking "+
				"fits exactly as well", a.Margin)
		}
	}
}

// TestSolve_aContestedRunnerUpIsNotAmbiguity is what the assignment buys beyond
// correctness: a smaller queue, without giving anything up.
//
// Judged on its own, the first booking looks torn between two near-equal
// candidates. It is not: the second candidate is wanted much more by the second
// booking, so no arrangement would ever give it away. Asking a person about that
// is asking about a choice that was never open.
func TestSolve_aContestedRunnerUpIsNotAmbiguity(t *testing.T) {
	rows := [][]Candidate{
		{cand("auth-a", 9.0), cand("auth-b", 8.6)},
		{cand("auth-b", 12.0)},
	}
	got := Solve(rows, 0)

	if chosen(got[0]) != "auth-a" || chosen(got[1]) != "auth-b" {
		t.Fatalf("arrangement was %q and %q, want auth-a and auth-b",
			chosen(got[0]), chosen(got[1]))
	}
	// Pairwise the gap is 0.4 bits and would be held. Against the batch, giving
	// auth-a up costs the whole of it, because auth-b is not available.
	if got[0].Margin <= 1 {
		t.Errorf("margin %.2f bits: the runner-up is treated as available when another "+
			"transaction has a far stronger claim on it", got[0].Margin)
	}
}

// TestSolve_refusesAPairingWorseThanNone pins what the no-match weight is for.
// Without it the arrangement reaches for any pairing at all, because any weight
// above minus infinity improves the total.
func TestSolve_refusesAPairingWorseThanNone(t *testing.T) {
	rows := [][]Candidate{{cand("auth-a", -3)}}

	if got := Solve(rows, 0); got[0].Candidate != nil {
		t.Errorf("a pairing worth %.1f bits was taken over leaving it alone",
			got[0].Candidate.Weight)
	}
	if got := Solve(rows, -5); got[0].Candidate == nil {
		t.Error("a pairing better than the threshold was refused")
	}
}

// TestSolve_isIndependentOfOrder is the property the whole change exists for.
func TestSolve_isIndependentOfOrder(t *testing.T) {
	rows := [][]Candidate{
		{cand("auth-a", 9.0), cand("auth-b", 8.0)},
		{cand("auth-b", 8.5), cand("auth-c", 7.0)},
		{cand("auth-a", 6.0)},
	}
	forward := Solve(rows, 0)

	reversed := [][]Candidate{rows[2], rows[1], rows[0]}
	back := Solve(reversed, 0)

	want := map[int]string{0: chosen(forward[0]), 1: chosen(forward[1]), 2: chosen(forward[2])}
	got := map[int]string{2: chosen(back[0]), 1: chosen(back[1]), 0: chosen(back[2])}
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("transaction %d took %q one way round and %q the other",
				i, want[i], got[i])
		}
	}
}

// TestComponents_splitsWhatCannotCompete keeps the solver's cost bounded. Two
// transactions with no candidate in common cannot affect each other, and a first
// sync of three hundred rows must not become one problem of three hundred.
func TestComponents_splitsWhatCannotCompete(t *testing.T) {
	rows := [][]Candidate{
		{cand("a", 1)},
		{cand("b", 1)},
		{cand("b", 1), cand("c", 1)},
		nil,
	}
	got := components(rows)

	sizes := map[int]int{}
	for _, comp := range got {
		sizes[len(comp)]++
	}
	if sizes[1] != 2 || sizes[2] != 1 || len(got) != 3 {
		t.Errorf("components %v: want one pair and two singles", got)
	}
}

// TestSolve_handlesAnEmptyBatch guards the edge the sync loop reaches most often.
func TestSolve_handlesAnEmptyBatch(t *testing.T) {
	if got := Solve(nil, 0); len(got) != 0 {
		t.Errorf("got %d assignments for no transactions", len(got))
	}
	got := Solve([][]Candidate{nil, nil}, 0)
	if len(got) != 2 {
		t.Fatalf("got %d assignments for two transactions", len(got))
	}
	for i, a := range got {
		if a.Candidate != nil {
			t.Errorf("transaction %d was paired with something out of an empty window", i)
		}
	}
}

// TestSolve_interchangeableRowsAreNotAChoice is the refinement that makes the
// multiplicity case work, and the one that had to be got right twice.
//
// Two authorisations for the same amount, the same day and the same payee leave
// the same budget whichever a booking settles onto, so the arrangement's
// indifference between them is not doubt. Two authorisations of 120.00 and
// 121.00 also compare identically to a booking of 125.00 — both "within
// tolerance" — but settling onto one leaves the other open at a different
// figure, and that is a decision.
func TestSolve_interchangeableRowsAreNotAChoice(t *testing.T) {
	same := func(id string) Candidate {
		c := cand(id, 9)
		c.Transaction.AmountCents = -12000
		c.Transaction.PayeeName = "Spotify"
		return c
	}
	// Two bookings, two authorisations nobody could tell apart.
	got := Solve([][]Candidate{
		{same("auth-a"), same("auth-b")},
		{same("auth-a"), same("auth-b")},
	}, 0)
	for i, a := range got {
		if a.Candidate == nil {
			t.Fatalf("booking %d was left unpaired", i)
		}
		if math.IsInf(a.Margin, 1) || a.Margin < 1 {
			t.Errorf("booking %d claims a margin of %.2f: choosing between two rows that "+
				"leave the same budget is being treated as a decision", i, a.Margin)
		}
	}

	// The same shape, but the two authorisations differ by a euro.
	differing := func(id string, cents int64) Candidate {
		c := cand(id, 9)
		c.Transaction.AmountCents = cents
		c.Transaction.PayeeName = "Spotify"
		return c
	}
	got = Solve([][]Candidate{
		{differing("auth-a", -12000), differing("auth-b", -12100)},
	}, 0)
	if got[0].Margin >= 1 {
		t.Errorf("margin %.2f: two authorisations a euro apart leave different budgets "+
			"and the choice between them belongs to a person", got[0].Margin)
	}
}
