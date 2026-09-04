package linkagegen

import (
	"fmt"
	"strings"
	"testing"
)

// TestGenerate_isDeterministic is the property every other test here rests on.
// A generator that drew differently each run would turn a failure into a rumour.
func TestGenerate_isDeterministic(t *testing.T) {
	cfg := Config{Seed: 7, Purchases: 40, FieldWidth: 22, Prefix: "VISA",
		BranchChance: 0.2, DuplicateChance: 0.2, UpliftChance: 0.2, UpliftMaxPercent: 20}

	a, b := Generate(cfg), Generate(cfg)
	if len(a.Rows) != len(b.Rows) {
		t.Fatalf("two draws from one seed gave %d and %d rows", len(a.Rows), len(b.Rows))
	}
	for i := range a.Rows {
		if a.Rows[i] != b.Rows[i] {
			t.Fatalf("row %d differs between draws:\n  %+v\n  %+v", i, a.Rows[i], b.Rows[i])
		}
	}

	cfg.Seed = 8
	if c := Generate(cfg); len(c.Rows) == len(a.Rows) && c.Rows[0] == a.Rows[0] {
		t.Error("a different seed drew the same history; the seed is not reaching the draw")
	}
}

// TestRender_truncatesAtTheConfiguredWidth pins the mechanism the whole payee
// level scheme was built for. It is a table rather than a rule because the
// interesting part is the interaction: the prefix eats the budget the name needs.
func TestRender_truncatesAtTheConfiguredWidth(t *testing.T) {
	for name, tc := range map[string]struct {
		merchant, prefix string
		width            int
		want             string
	}{
		"no width means no truncation": {"Da Luigi Roma", "Visa", 0, "Visa Da Luigi Roma"},
		"no prefix, room to spare":     {"Da Luigi Roma", "", 22, "Da Luigi Roma"},
		"the prefix costs the tail":    {"Da Luigi Roma", "Visa", 13, "Visa Da Luigi"},
		"a cut mid-word still cuts":    {"Elektromarkt Nord", "", 12, "Elektromarkt"},
		"trailing space is not kept":   {"Da Luigi Roma", "Visa", 14, "Visa Da Luigi"},
		"exactly the width is whole":   {"Hotel Berlin", "", 12, "Hotel Berlin"},
	} {
		if got := Render(tc.merchant, tc.prefix, tc.width); got != tc.want {
			t.Errorf("%s: Render(%q, %q, %d) = %q, want %q",
				name, tc.merchant, tc.prefix, tc.width, got, tc.want)
		}
	}
}

// TestGenerate_producesTheFourMechanisms checks that a history configured to
// contain each of them actually does.
//
// This is the test that keeps every later phase honest. A generator that quietly
// produced no truncated pairs would leave the tests that depend on them passing
// against nothing at all.
func TestGenerate_producesTheFourMechanisms(t *testing.T) {
	h := Generate(Config{
		Seed: 3, Days: 30, Purchases: 300,
		FieldWidth: 16, Prefix: "VISA", References: true,
		SettleMinDays: 1, SettleMaxDays: 4, UnsettledChance: 0.15,
		UpliftChance: 0.15, UpliftMaxPercent: 20,
		ReductionChance: 0.15, ReductionMaxPercent: 40,
		BranchChance: 0.15, DuplicateChance: 0.15,
	})

	var truncated, uplifted, reduced, unsettled, branches, duplicates int
	byPurchase := map[int][]Row{}
	for _, r := range h.Rows {
		byPurchase[r.PurchaseID] = append(byPurchase[r.PurchaseID], r)
	}
	sameDayAmount := map[string]int{}

	for _, p := range h.Purchases {
		rows := byPurchase[p.ID]
		var auth, book *Row
		for i := range rows {
			if rows[i].Status == Pending {
				auth = &rows[i]
			} else {
				book = &rows[i]
			}
		}
		if auth == nil {
			t.Fatalf("purchase %d has no authorisation", p.ID)
		}
		if book == nil {
			unsettled++
			continue
		}
		// Truncation is "the field cut it", not "the result got shorter". A
		// prefix can fill the gap back up: "Shell Tankstelle" becomes
		// "VISA Shell Tanks" at width 16, cut but exactly as long as it started.
		if book.Payee != Render(p.Merchant, "VISA", 0) {
			truncated++
		}
		switch {
		case book.AmountCents < auth.AmountCents: // more negative: spent more
			uplifted++
		case book.AmountCents > auth.AmountCents:
			reduced++
		}
		for _, town := range towns {
			if strings.HasSuffix(p.Merchant, " "+town) {
				branches++
			}
		}
		// Same day, same merchant, same amount: the multiplicity case, where no
		// field comparison can tell the two apart.
		key := fmt.Sprintf("%s|%s|%d", p.Date.Format("2006-01-02"), p.Merchant, p.AmountCents)
		sameDayAmount[key]++
	}
	for _, n := range sameDayAmount {
		if n > 1 {
			duplicates += n - 1
		}
	}

	t.Logf("drawn: %d purchases, %d rows — truncated %d, uplifted %d, reduced %d, "+
		"open %d, branches %d, duplicates %d", len(h.Purchases), len(h.Rows),
		truncated, uplifted, reduced, unsettled, branches, duplicates)

	for name, n := range map[string]int{
		"truncated bookings":  truncated,
		"upliftted bookings":  uplifted,
		"reduced bookings":    reduced,
		"open authorisations": unsettled,
		"branch collisions":   branches,
		"same-day duplicates": duplicates,
	} {
		if n == 0 {
			t.Errorf("no %s were drawn; every test that depends on them is vacuous", name)
		}
	}
}

// TestGenerate_theMerchantDrawIsSkewed matters for the frequency correction: a
// uniform distribution of payees would leave it nothing to correct, and a test
// built on one would pass whatever the correction did.
func TestGenerate_theMerchantDrawIsSkewed(t *testing.T) {
	h := Generate(Config{Seed: 11, Purchases: 500})

	counts := map[string]int{}
	for _, p := range h.Purchases {
		counts[p.Merchant]++
	}
	var top int
	for _, n := range counts {
		if n > top {
			top = n
		}
	}
	if share := float64(top) / float64(len(h.Purchases)); share < 0.25 {
		t.Errorf("the commonest merchant holds %.0f%% of the history; a real statement "+
			"is dominated by a handful of payees and the frequency correction needs that",
			100*share)
	}
}

// TestGenerate_referencesFollowTheConfiguration covers the switch that decides
// which identity path the importer takes, and therefore which defects a history
// can reach at all.
func TestGenerate_theReferenceSwitchIsHonoured(t *testing.T) {
	with := Generate(Config{Seed: 5, Purchases: 20, References: true})
	for _, r := range with.Rows {
		if r.EntryRef == "" {
			t.Fatal("a bank configured to supply references produced a row without one")
		}
	}
	without := Generate(Config{Seed: 5, Purchases: 20})
	for _, r := range without.Rows {
		if r.EntryRef != "" {
			t.Fatalf("a bank configured to supply none produced %q", r.EntryRef)
		}
	}
}

// TestGenerate_everyRowTracesToAPurchase keeps the truth usable. A row whose
// origin cannot be looked up is a row no measurement can score.
func TestGenerate_everyRowTracesToAPurchase(t *testing.T) {
	h := Generate(Config{Seed: 2, Purchases: 50, BranchChance: 0.3, DuplicateChance: 0.3})

	known := map[int]bool{}
	for _, p := range h.Purchases {
		known[p.ID] = true
	}
	for i, r := range h.Rows {
		if !known[r.PurchaseID] {
			t.Fatalf("row %d belongs to purchase %d, which is not in the history", i, r.PurchaseID)
		}
	}
	for _, p := range h.Purchases {
		if len(h.RowsFor(p.ID)) == 0 {
			t.Fatalf("purchase %d produced no rows", p.ID)
		}
	}
}

// TestFeedAsOf_neverOffersAnAuthorisationBesideItsBooking is the fidelity
// property this package got wrong first.
//
// A bank replaces a pending row with the booking that settles it. Serving both
// at once models a bank that does not exist and makes every settled purchase
// look like a duplicate the importer failed to merge — which is exactly how the
// mistake was found: the importer was blamed for a defect in the fixture.
func TestFeedAsOf_neverOffersAnAuthorisationBesideItsBooking(t *testing.T) {
	h := Generate(Config{Seed: 9, Days: 20, Purchases: 120,
		SettleMinDays: 1, SettleMaxDays: 4, UnsettledChance: 0.2})

	day := h.Purchases[0].Date
	for _, p := range h.Purchases {
		if p.Date.After(day) {
			day = p.Date
		}
	}
	day = day.AddDate(0, 0, 10)

	seen := map[int]int{}
	for _, r := range h.FeedAsOf(day) {
		seen[r.PurchaseID]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("purchase %d appears %d times in one feed", id, n)
		}
	}

	// And what it offers is the settled form where one exists.
	var booked, pending int
	for _, r := range h.FeedAsOf(day) {
		if r.Status == Booked {
			booked++
		} else {
			pending++
		}
	}
	if booked == 0 || pending == 0 {
		t.Errorf("a feed well past the history holds %d booked and %d pending rows; "+
			"both kinds have to occur for the lifecycle to be exercised", booked, pending)
	}
}

// TestFeedAsOf_growsWithTime pins that the feed is a view of a moment, which is
// what lets a test drive several runs over one history.
func TestFeedAsOf_growsWithTime(t *testing.T) {
	h := Generate(Config{Seed: 4, Days: 20, Purchases: 60, SettleMinDays: 1, SettleMaxDays: 3})

	start := h.Purchases[0].Date
	for _, p := range h.Purchases {
		if p.Date.Before(start) {
			start = p.Date
		}
	}
	early := len(h.FeedAsOf(start.AddDate(0, 0, 2)))
	late := len(h.FeedAsOf(start.AddDate(0, 0, 60)))
	if early == 0 {
		t.Error("the feed is empty two days in")
	}
	if late <= early {
		t.Errorf("the feed held %d rows early and %d late; it does not grow", early, late)
	}
	if late != len(h.Purchases) {
		t.Errorf("a feed past the whole history holds %d rows for %d purchases",
			late, len(h.Purchases))
	}
}

// TestGenerate_branchCollisionsContradict is the third fidelity property this
// package got wrong, and the one that mattered most.
//
// A branch collision is meant to be the case the payee levels exist to separate:
// two names that contradict one another. An earlier version drew the bare chain
// name against the chain plus a town — "Edeka Sued" against "Edeka Sued
// Nuernberg" — which is the shorter contained in the longer, indistinguishable
// from a bank truncating a field. Measured against it, the matcher merged
// fourteen unrelated purchases and looked much worse than it is.
//
// Both sides have to name a town for the pair to contradict.
func TestGenerate_branchCollisionsContradict(t *testing.T) {
	h := Generate(Config{Seed: 1969, Days: 60, Purchases: 200,
		BranchChance: 0.3, SettleMinDays: 1, SettleMaxDays: 6})

	// Read from the rows, not the purchases: what the matcher sees is what the
	// bank sent, and an earlier version of this test read the truth instead and
	// passed while the rows still contained one another.
	byDayAmount := map[string][]Row{}
	for _, r := range h.Rows {
		if r.Status != Pending {
			continue
		}
		key := fmt.Sprintf("%s|%d", r.Date.Format("2006-01-02"), r.AmountCents)
		byDayAmount[key] = append(byDayAmount[key], r)
	}

	var pairs, contained int
	for _, group := range byDayAmount {
		for i, a := range group {
			for _, b := range group[i+1:] {
				if a.Payee == b.Payee {
					continue
				}
				pairs++
				// One name wholly inside the other is a truncation's shape, not a
				// contradiction's.
				if strings.HasPrefix(b.Payee, a.Payee+" ") ||
					strings.HasPrefix(a.Payee, b.Payee+" ") {
					contained++
					t.Logf("contained rather than contradicting: %q vs %q", a.Payee, b.Payee)
				}
			}
		}
	}
	if pairs == 0 {
		t.Fatal("no branch collisions were drawn at all")
	}
	if contained > pairs/4 {
		t.Errorf("%d of %d same-day same-amount pairs have one name inside the other; "+
			"that is a truncation's shape and not the case branch collisions are for",
			contained, pairs)
	}
}
