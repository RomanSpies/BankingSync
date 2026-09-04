//go:build fireflylive

package main

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"bankingsync/firefly"
	"bankingsync/internal/fireflylive"
)

// liveBackends adds a third backend that runs the shared sync semantics against
// a real Firefly III instance.
//
// The point is not more coverage of our own branching — the fixture already
// gives that. It is that firefly/fireflytest encodes a dozen assumptions about
// how Firefly behaves, and a fixture can only ever confirm the model it was
// written from. One of those assumptions was already wrong: dates were sent as
// instants, which moved the calendar day on any server west of UTC, and the
// fixture echoed the submitted string back so nothing noticed.
func liveBackends() []backendCase {
	return []backendCase{{"live", newFireflyLiveHarness}}
}

const livePendingTag = "pending"

// requireLiveEnv resolves the instance to test against, and refuses rather than
// skips when it cannot.
//
// A skip here would be the worst outcome available: a bootstrap failure would
// produce a green pipeline that tested nothing, which is precisely the silent
// wrongness this project treats as the thing to avoid. The build tag already
// decides whether these tests run at all; once compiled in, they must either
// run or fail.
func requireLiveEnv(t *testing.T) (base, runID string) {
	t.Helper()
	e, err := fireflylive.EnvFromOS()
	if err != nil {
		t.Fatal(err)
	}
	return e.BaseURL, e.RunID
}

// assertDisposable refuses to write to an instance that holds anything this test
// suite did not create.
//
// One request, and it is the difference between a red pipeline and forty test
// transactions in somebody's real budget.
func assertDisposable(t *testing.T, base, token string) {
	t.Helper()
	if err := fireflylive.AssertDisposable(context.Background(), base, token); err != nil {
		t.Fatal(err)
	}
}

const liveNamePrefix = fireflylive.NamePrefix

func liveNamespace(runID, testName string) string { return fireflylive.Namespace(runID, testName) }

func newFireflyLiveHarness(t *testing.T) *harness {
	t.Helper()
	base, runID := requireLiveEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	token, err := fireflylive.Bootstrap(ctx, base)
	if err != nil {
		t.Fatalf("bootstrapping %s: %v", base, err)
	}

	assertDisposable(t, base, token)
	c := firefly.New(base, token, firefly.WithBackoffBase(time.Millisecond))

	h := newHarness(t)
	h.fake = nil
	h.ns = liveNamespace(runID, t.Name())
	h.syncer.ac = firefly.NewStore(c, firefly.Config{PendingTag: livePendingTag})
	h.seed = liveSeeder{c: c, base: base, token: token, pendingTag: livePendingTag}
	return h
}

// liveSeeder plays the user against a real instance: it creates rows nobody
// imported and edits rows the importer wrote, over the same API a person would
// drive through the UI.
type liveSeeder struct {
	c          *firefly.Client
	base       string
	token      string
	pendingTag string
}

// assetID resolves the asset account without going through Store.GetOrCreateAccount,
// which is the code under test.
func (s liveSeeder) assetID(t *testing.T, name string) string {
	t.Helper()
	ctx := context.Background()

	// Every page, and asset accounts only. A plain Get returns the first fifty and
	// nothing says so, which is enough for a fresh instance and wrong the moment
	// one run has filled it: the account is there, the seeder does not see it, and
	// creating it again fails with "This account name is already in use". The
	// eighteen harness scenarios pass that mark on their own, so whether this bit
	// was reached depended on the order tests happened to run in.
	raws, err := s.c.GetPaged(ctx, "/api/v1/accounts", url.Values{"type": {"asset"}})
	if err != nil {
		t.Fatalf("listing accounts: %v", err)
	}
	for _, raw := range raws {
		var a struct {
			ID         string `json:"id"`
			Attributes struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"attributes"`
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatalf("decoding an account: %v", err)
		}
		if a.Attributes.Type == "asset" && strings.EqualFold(a.Attributes.Name, name) {
			return a.ID
		}
	}

	created, err := s.c.Post(ctx, "/api/v1/accounts", map[string]any{
		"name": name, "type": "asset", "account_role": "defaultAsset",
		"currency_code": "EUR",
	})
	if err != nil {
		t.Fatalf("creating asset account %q: %v", name, err)
	}
	raw, err := firefly.Unwrap(created)
	if err != nil {
		t.Fatalf("unwrapping the created account: %v", err)
	}
	var acct struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &acct); err != nil {
		t.Fatalf("decoding the created account: %v", err)
	}
	return acct.ID
}

func (s liveSeeder) foreignTransaction(t *testing.T, accountName string, date time.Time, payee, notes string, cents int64) string {
	t.Helper()
	assetID := s.assetID(t, accountName)

	split := map[string]any{
		"date": firefly.FormatDate(date), "amount": firefly.FormatAmount(cents),
		"description": payee, "notes": notes, "currency_code": "EUR",
		"tags": []string{s.pendingTag},
	}
	if cents < 0 {
		split["type"], split["source_id"], split["destination_name"] = "withdrawal", assetID, payee
	} else {
		split["type"], split["source_name"], split["destination_id"] = "deposit", payee, assetID
	}

	// No external_id: that absence is what makes the row look user-entered
	// rather than imported, which is the whole point of a "foreign" transaction.
	body, err := s.c.Post(context.Background(), "/api/v1/transactions", map[string]any{
		"apply_rules": false, "fire_webhooks": false,
		"transactions": []map[string]any{split},
	})
	if err != nil {
		t.Fatalf("seeding a foreign transaction: %v", err)
	}

	groupID, journalID := s.firstSplit(t, body)
	id, err := firefly.EncodeID(groupID, journalID)
	if err != nil {
		t.Fatalf("EncodeID: %v", err)
	}
	return id
}

func (s liveSeeder) editPayeeAndNotes(t *testing.T, txnID, payee, notes string) {
	t.Helper()
	s.mutate(t, txnID, func(split map[string]any) {
		split["description"] = payee
		split["notes"] = notes
	})
}

func (s liveSeeder) setCleared(t *testing.T, txnID string, cleared bool) {
	t.Helper()
	s.mutate(t, txnID, func(split map[string]any) {
		tags, _ := split["tags"].([]any)
		out := make([]string, 0, len(tags)+1)
		for _, tag := range tags {
			if v, _ := tag.(string); v != "" && !strings.EqualFold(v, s.pendingTag) {
				out = append(out, v)
			}
		}
		if !cleared {
			out = append(out, s.pendingTag)
		}
		split["tags"] = out
	})
}

// mutate is a read-modify-write, and it has to be. Firefly deletes every journal
// missing from the submitted array, so a partial PUT would destroy the very row
// the seeder is trying to edit — the same hazard firefly/store.go guards against
// on the production path.
func (s liveSeeder) mutate(t *testing.T, txnID string, fn func(map[string]any)) {
	t.Helper()
	ctx := context.Background()

	groupID, journalID, err := firefly.SplitID(txnID)
	if err != nil {
		t.Fatalf("SplitID(%q): %v", txnID, err)
	}

	body, err := s.c.Get(ctx, "/api/v1/transactions/"+groupID, nil)
	if err != nil {
		t.Fatalf("reading transaction %s: %v", groupID, err)
	}
	raw, err := firefly.Unwrap(body)
	if err != nil {
		t.Fatalf("unwrapping transaction %s: %v", groupID, err)
	}
	var group struct {
		Attributes struct {
			Transactions []map[string]any `json:"transactions"`
		} `json:"attributes"`
	}
	if err := json.Unmarshal(raw, &group); err != nil {
		t.Fatalf("decoding transaction %s: %v", groupID, err)
	}

	found := false
	for _, split := range group.Attributes.Transactions {
		id, _ := split["transaction_journal_id"].(string)
		if id != journalID {
			continue
		}
		fn(split)
		found = true
	}
	if !found {
		t.Fatalf("journal %s is not in group %s", journalID, groupID)
	}

	if _, err := s.c.Put(ctx, "/api/v1/transactions/"+groupID, map[string]any{
		"apply_rules": false, "fire_webhooks": false,
		"transactions": group.Attributes.Transactions,
	}); err != nil {
		t.Fatalf("updating transaction %s: %v", groupID, err)
	}
}

func (s liveSeeder) firstSplit(t *testing.T, body []byte) (groupID, journalID string) {
	t.Helper()
	raw, err := firefly.Unwrap(body)
	if err != nil {
		t.Fatalf("unwrapping the created group: %v", err)
	}
	var group struct {
		ID         string `json:"id"`
		Attributes struct {
			Transactions []struct {
				JournalID string `json:"transaction_journal_id"`
			} `json:"transactions"`
		} `json:"attributes"`
	}
	if err := json.Unmarshal(raw, &group); err != nil {
		t.Fatalf("decoding the created group: %v", err)
	}
	if len(group.Attributes.Transactions) == 0 {
		t.Fatal("the created group came back without a split")
	}
	return group.ID, group.Attributes.Transactions[0].JournalID
}

// deleteGroup is the one verb the production client deliberately does not have:
// bankingsync never deletes, it maps a missing group to budget.ErrGone. A test
// that needs a deletion does it itself rather than growing the client a
// destructive method it would never use.
func (s liveSeeder) deleteGroup(t *testing.T, groupID string) {
	t.Helper()
	if err := fireflylive.DeleteTransactionGroup(context.Background(), s.base, s.token, groupID); err != nil {
		t.Fatal(err)
	}
}

// TestFireflyLive_backendIsRegistered fails loudly if the live backend is
// compiled in but never reaches the shared harness.
//
// Without it a typo in the build tag, or a semanticBackends() that forgot to
// append liveBackends(), would produce a job that passes having run nothing —
// the one failure mode this whole arrangement exists to prevent.
func TestFireflyLive_backendIsRegistered(t *testing.T) {
	var names []string
	for _, b := range semanticBackends() {
		names = append(names, b.name)
	}
	for _, name := range names {
		if name == "live" {
			return
		}
	}
	t.Fatalf("the live backend is not in semanticBackends(): %v", names)
}

var _ harnessSeeder = liveSeeder{}
