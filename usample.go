package main

import (
	"context"
	"log"

	"bankingsync/budget"
	"bankingsync/logs"
	"bankingsync/store"
)

// levelObservations renders a sample of non-matching pairs for storage.
func levelObservations(c budget.LevelCounts) []store.LevelObservation {
	out := make([]store.LevelObservation, 0,
		len(c.PayeeU)+len(c.AmountU)+len(c.DateU))
	for k, v := range c.PayeeU {
		out = append(out, store.LevelObservation{Field: "payee", Level: k.String(), Count: v})
	}
	for k, v := range c.AmountU {
		out = append(out, store.LevelObservation{Field: "amount", Level: k.String(), Count: v})
	}
	for k, v := range c.DateU {
		out = append(out, store.LevelObservation{Field: "date", Level: k.String(), Count: v})
	}
	return out
}

// sampledU reads the stored sample back into the shape the refit consumes.
//
// Addressed by classification rather than by parameter version, because the
// tally survives everything that leaves the comparison levels alone — a
// promotion among them.
//
// A row naming a level this build no longer has is dropped rather than guessed
// at, the same way the decision log treats one.
func (s *Syncer) sampledU(classification string) budget.LevelCounts {
	out := budget.LevelCounts{
		PayeeM: map[budget.PayeeLevel]int{}, PayeeU: map[budget.PayeeLevel]int{},
		AmountM: map[budget.AmountLevel]int{}, AmountU: map[budget.AmountLevel]int{},
		DateM: map[budget.DateLevel]int{}, DateU: map[budget.DateLevel]int{},
	}
	obs, err := s.st.LevelObservations(classification)
	if err != nil {
		return out
	}
	for _, o := range obs {
		switch o.Field {
		case "payee":
			if l, ok := budget.ParsePayeeLevel(o.Level); ok {
				out.PayeeU[l] += o.Count
			}
		case "amount":
			if l, ok := budget.ParseAmountLevel(o.Level); ok {
				out.AmountU[l] += o.Count
			}
		case "date":
			if l, ok := budget.ParseDateLevel(o.Level); ok {
				out.DateU[l] += o.Count
			}
		}
	}
	return out
}

// uSampler accumulates one run's non-matching pairs for one account.
//
// In memory for the length of the run and written once at the end of it. A busy
// account weighs thousands of pairs per sync, and a database round trip for each
// would put the cost of an estimate nobody has asked for into the path of every
// import.
type uSampler struct {
	counts budget.LevelCounts
	seen   int
}

func newUSampler() *uSampler {
	return &uSampler{counts: budget.LevelCounts{
		PayeeU:  map[budget.PayeeLevel]int{},
		AmountU: map[budget.AmountLevel]int{},
		DateU:   map[budget.DateLevel]int{},
	}}
}

// observe folds one placed transaction's rejected candidates into the tally.
func (u *uSampler) observe(out budget.Outcome) {
	for _, c := range out.Unchosen {
		u.counts.PayeeU[c.Payee]++
		u.counts.AmountU[c.Amount]++
		u.counts.DateU[c.Date]++
		u.seen++
	}
}

// flush writes the run's tally against one account.
//
// A failure is logged and swallowed. This is how the parameters could one day be
// estimated, not how transactions are imported, and it must never be the reason
// a sync reports trouble.
func (s *Syncer) flushUSample(ctx context.Context, acctID int64, label, classification string, u *uSampler) {
	if u == nil || u.seen == 0 {
		return
	}
	if err := s.st.AddLevelObservations(acctID, classification, levelObservations(u.counts)); err != nil {
		log.Printf("[%s] could not record the non-match sample: %v", label, err)
		olog.Warn(ctx, "match.usample_not_recorded",
			logs.String("bank", label), logs.String("error", err.Error()))
	}
}
