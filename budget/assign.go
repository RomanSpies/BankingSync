package budget

import "math"

// Assignment is what became of one incoming transaction once the whole batch was
// weighed together.
type Assignment struct {
	// Incoming is the index into the slice handed to Solve.
	Incoming int

	// Candidate is the row it was paired with, or nil for "no counterpart".
	Candidate *Candidate

	// Margin is how many bits of total evidence the batch loses if this pairing
	// is forbidden and everything is arranged again as well as it can be.
	//
	// This is the honest replacement for the pairwise margin. A transaction whose
	// second-best candidate is claimed by somebody else who wants it more is not
	// ambiguous at all, however close the two look side by side — forbidding its
	// pairing forces a reshuffle that costs real evidence. Judging that pair on
	// its own put it in front of a person for no reason.
	Margin float64

	// Interchangeable is how many candidates the chosen one could not be told
	// from, itself included. More than one means the arrangement had a free
	// choice that leaves the same budget either way — which is a thing worth
	// counting, because it is the shape the reported multiplicity defect had.
	Interchangeable int
}

// Solve pairs incoming transactions with candidates under a one-to-one
// constraint, maximising the total evidence of the arrangement.
//
// rows[i] holds the candidates assessed for incoming transaction i; the same
// backend row may appear in several of them, and the constraint is that it ends
// up in at most one pairing. noMatch is the weight of leaving a transaction
// unpaired — set it to the weight of the review threshold and the arrangement
// will not reach for a pairing that would be refused anyway.
//
// The one-to-one constraint is the point. Weighing pairs independently allows two
// bookings to claim the same authorisation, which the sync loop then had to break
// up by processing order — first come, first served, and a different order gave a
// different budget.
func Solve(rows [][]Candidate, noMatch float64) []Assignment {
	out := make([]Assignment, len(rows))
	for i := range out {
		out[i] = Assignment{Incoming: i, Margin: math.Inf(1)}
	}

	for _, comp := range components(rows) {
		solveComponent(rows, comp, noMatch, out)
	}
	return out
}

// components groups incoming transactions that compete for the same candidates.
//
// Two transactions with no candidate in common cannot affect one another, so
// solving them together would be arithmetic for its own sake. Splitting first is
// what keeps the cost of this bounded: a first sync offering three hundred
// transactions is three hundred components of one, not one problem of three
// hundred, and the cubic solver never sees a matrix worth worrying about.
func components(rows [][]Candidate) [][]int {
	parent := make([]int, len(rows))
	for i := range parent {
		parent[i] = i
	}
	find := func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		if ra, rb := find(a), find(b); ra != rb {
			parent[ra] = rb
		}
	}

	owner := map[string]int{}
	for i, cands := range rows {
		for _, c := range cands {
			if prev, seen := owner[c.Transaction.ID]; seen {
				union(prev, i)
			} else {
				owner[c.Transaction.ID] = i
			}
		}
	}

	grouped := map[int][]int{}
	for i := range rows {
		r := find(i)
		grouped[r] = append(grouped[r], i)
	}
	out := make([][]int, 0, len(grouped))
	for i := range rows {
		if find(i) == i {
			out = append(out, grouped[i])
		}
	}
	return out
}

func solveComponent(rows [][]Candidate, members []int, noMatch float64, out []Assignment) {
	cols, colOf := columnsFor(rows, members)
	if len(cols) == 0 {
		for _, i := range members {
			out[i] = Assignment{Incoming: i, Margin: math.Inf(1)}
		}
		return
	}

	best, total := arrange(rows, members, cols, colOf, noMatch, -1, nil)
	for k, i := range members {
		out[i] = Assignment{Incoming: i, Candidate: best[k]}
	}

	// The margin, one transaction at a time: forbid the pairing that was chosen
	// and see what the rest of the batch can still manage without it.
	//
	// What is forbidden is not one edge but every edge to a row the chosen one
	// cannot be told from. Two authorisations for the same amount, on the same
	// day, to the same payee leave the same budget whichever the booking settles
	// onto, so preferring one of them is not a decision and putting it to a
	// person is asking about nothing.
	//
	// Comparison levels are not enough for this, and that is worth stating: an
	// authorisation of 120.00 and one of 121.00 both compare as "within
	// tolerance" to a booking of 125.00, yet settling onto one leaves a 121.00
	// authorisation open and onto the other a 120.00 one. Different budgets, so a
	// real choice, so a person decides.
	for k, i := range members {
		if best[k] == nil {
			// Leaving a transaction unpaired is never ambiguous in this sense:
			// there was nothing to take instead.
			out[i].Margin = math.Inf(1)
			continue
		}
		banned := map[int]bool{}
		for j := range rows[i] {
			if interchangeable(rows[i][j].Transaction, best[k].Transaction) {
				banned[colOf[rows[i][j].Transaction.ID]] = true
			}
		}
		out[i].Interchangeable = len(banned)
		_, without := arrange(rows, members, cols, colOf, noMatch, k, banned)
		out[i].Margin = total - without
	}
}

func columnsFor(rows [][]Candidate, members []int) ([]*Candidate, map[string]int) {
	colOf := map[string]int{}
	var cols []*Candidate
	for _, i := range members {
		for j := range rows[i] {
			c := &rows[i][j]
			if _, seen := colOf[c.Transaction.ID]; !seen {
				colOf[c.Transaction.ID] = len(cols)
				cols = append(cols, c)
			}
		}
	}
	return cols, colOf
}

// arrange solves one component, optionally with a single pairing forbidden.
//
// The matrix is square by construction: every transaction gets a column of its
// own standing for "no counterpart", so an arrangement always exists and the
// solver never has to be told what to do when it does not.
func arrange(
	rows [][]Candidate, members []int, cols []*Candidate, colOf map[string]int,
	noMatch float64, banRow int, banCols map[int]bool,
) ([]*Candidate, float64) {
	n := len(members)
	m := len(cols)
	size := m + n

	const unavailable = 1e9
	cost := make([][]float64, size)
	for r := range cost {
		cost[r] = make([]float64, size)
		for c := range cost[r] {
			// Padding rows are indifferent: they exist only to make the matrix
			// square and contribute nothing whichever column they take.
			cost[r][c] = 0
		}
	}
	for k, i := range members {
		for c := 0; c < m; c++ {
			cost[k][c] = unavailable
		}
		for j := range rows[i] {
			c := colOf[rows[i][j].Transaction.ID]
			if k == banRow && banCols[c] {
				continue
			}
			cost[k][c] = -rows[i][j].Weight
		}
		for c := m; c < size; c++ {
			cost[k][c] = unavailable
		}
		cost[k][m+k] = -noMatch
	}

	assigned := hungarian(cost)

	out := make([]*Candidate, n)
	var total float64
	for k, i := range members {
		c := assigned[k]
		if c >= m || cost[k][c] >= unavailable {
			total += noMatch
			continue
		}
		// The column is only an index. Each incoming transaction has its own
		// assessment of the same backend row — different level, different weight,
		// different probability — so what comes back must be this transaction's
		// view of it and not whichever one happened to define the column.
		id := cols[c].Transaction.ID
		for j := range rows[i] {
			if rows[i][j].Transaction.ID == id {
				out[k] = &rows[i][j]
				break
			}
		}
		total += -cost[k][c]
	}
	return out, total
}

// hungarian finds the minimum-cost perfect matching of a square matrix and
// returns, for each row, the column it was given.
//
// The Kuhn-Munkres method with potentials, O(n^3). Written out rather than taken
// from a library: the matrices here are components of a handful of rows, and a
// dependency for eighty lines of arithmetic would cost more than it saves in a
// program whose single static binary is a stated feature.
func hungarian(a [][]float64) []int {
	n := len(a)
	const inf = math.MaxFloat64 / 4

	u := make([]float64, n+1)
	v := make([]float64, n+1)
	p := make([]int, n+1)
	way := make([]int, n+1)

	for i := 1; i <= n; i++ {
		p[0] = i
		j0 := 0
		minv := make([]float64, n+1)
		used := make([]bool, n+1)
		for j := range minv {
			minv[j] = inf
		}

		for {
			used[j0] = true
			i0, j1 := p[j0], 0
			delta := inf
			for j := 1; j <= n; j++ {
				if used[j] {
					continue
				}
				cur := a[i0-1][j-1] - u[i0] - v[j]
				if cur < minv[j] {
					minv[j], way[j] = cur, j0
				}
				if minv[j] < delta {
					delta, j1 = minv[j], j
				}
			}
			for j := 0; j <= n; j++ {
				if used[j] {
					u[p[j]] += delta
					v[j] -= delta
				} else {
					minv[j] -= delta
				}
			}
			j0 = j1
			if p[j0] == 0 {
				break
			}
		}
		for j0 != 0 {
			j1 := way[j0]
			p[j0] = p[j1]
			j0 = j1
		}
	}

	out := make([]int, n)
	for j := 1; j <= n; j++ {
		if p[j] > 0 {
			out[p[j]-1] = j - 1
		}
	}
	return out
}

// interchangeable reports whether settling onto either row would leave the same
// budget.
//
// What counts is what a person would still see: the figure, the day, the name,
// and whether it is confirmed. The bank reference deliberately does not — two
// authorisations for the same purchase carry different references by
// construction, and settling onto one overwrites its reference with the
// booking's anyway, so a difference there survives nothing.
func interchangeable(a, b *Transaction) bool {
	return a.AmountCents == b.AmountCents &&
		a.Date.Equal(b.Date) &&
		a.PayeeName == b.PayeeName &&
		a.Cleared == b.Cleared
}
