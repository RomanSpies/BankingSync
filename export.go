package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"bankingsync/store"
)

// LinkageStats is what an installation can hand over when it reports a matching
// problem.
//
// It is the only route by which a bank that truncates its payee field can ever
// improve the shipped parameters. The developer's own institution does not
// truncate, so measuring against it and shipping the result to everyone would
// dress one bank's habits up as a finding. Whoever has the problem has the data;
// this is how they can attach it to an issue.
//
// What it deliberately does not contain: payees, amounts, dates below the month,
// account identifiers, bank names, or any stable identifier for the
// installation. Level counts and field widths say how a bank behaves without
// saying what anybody bought. There is no network traffic and no server — the
// file is written to standard output and the person decides what happens to it.
type LinkageStats struct {
	Version      string         `json:"version"`
	GeneratedFor string         `json:"generated_for"` // month, no finer
	ParamVersion string         `json:"param_version"`
	Backend      string         `json:"backend"`
	Accounts     []AccountStats `json:"accounts"`
}

// AccountStats describes one account's traffic by shape alone. The account is
// numbered within this file and nowhere else, so two exports cannot be joined.
type AccountStats struct {
	Account int `json:"account"`

	// FieldWidth is the longest payee this institution has been seen to send and
	// whether several different names end there — the signature of a fixed-width
	// field, which is what decides whether a shortened name is expected.
	FieldWidth int  `json:"observed_field_width"`
	Truncating bool `json:"looks_like_it_truncates"`
	Decisions  int  `json:"decisions"`
	Labelled   int  `json:"decisions_settled"`

	// Levels counts how often each comparison level occurred, split by whether
	// the pair turned out to be one payment. These are the m and u counts, and
	// they are the whole point of the file.
	Levels map[string]LevelCount `json:"levels"`

	Outcomes map[string]int `json:"outcomes"`
}

// LevelCount is one comparison level's tally: how often it was seen at all, and
// how often among pairs that turned out to be the same payment.
type LevelCount struct {
	Seen    int `json:"seen"`
	Matched int `json:"matched"`
	Refuted int `json:"refuted"`
}

// exportLinkageStats writes the diagnostic file to standard output.
func exportLinkageStats(st *store.Store, backend, paramVersion string) error {
	decisions, err := st.GetMatchDecisions(100000)
	if err != nil {
		return fmt.Errorf("read the decision log: %w", err)
	}
	accounts, err := st.GetAllBankAccounts()
	if err != nil {
		return fmt.Errorf("read the accounts: %w", err)
	}

	// Numbered by position, so the file carries no account identity of its own.
	number := map[int64]int{}
	byAccount := map[int64]*AccountStats{}
	for i, a := range accounts {
		number[a.ID] = i + 1
		byAccount[a.ID] = &AccountStats{Account: i + 1, Levels: map[string]LevelCount{},
			Outcomes: map[string]int{}}
	}

	for _, d := range decisions {
		acct := byAccount[d.BankAccountID]
		if acct == nil {
			continue
		}
		acct.Decisions++
		acct.Outcomes[d.Outcome]++
		if d.Truth != nil {
			acct.Labelled++
		}
		for field, level := range map[string]string{
			"payee": d.PayeeLevel, "amount": d.AmountLevel, "date": d.DateLevel,
		} {
			if level == "" {
				continue
			}
			key := field + "." + level
			c := acct.Levels[key]
			c.Seen++
			if d.Truth != nil {
				if *d.Truth {
					c.Matched++
				} else {
					c.Refuted++
				}
			}
			acct.Levels[key] = c
		}
	}

	out := LinkageStats{
		Version:      Version,
		GeneratedFor: time.Now().UTC().Format("2006-01"),
		ParamVersion: paramVersion,
		Backend:      backend,
	}
	ids := make([]int64, 0, len(byAccount))
	for id := range byAccount {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return number[ids[i]] < number[ids[j]] })
	for _, id := range ids {
		out.Accounts = append(out.Accounts, *byAccount[id])
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
