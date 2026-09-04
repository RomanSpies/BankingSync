//go:build fireflylive

package firefly_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"bankingsync/budget"
	"bankingsync/firefly"
	"bankingsync/internal/fireflylive"
	"bankingsync/internal/iban"
)

// These tests cover what the cross-backend sync harness cannot.
//
// That harness already runs its eighteen scenarios against a real instance, so
// creating, updating, pending tags, date windows, transfers and external
// references are proven live. What it never exercises is the handful of things
// it has no reason to: it sends no IBANs, never builds a split group the way a
// person would, never deletes anything, never asks for a reference that is a
// substring of another, and never produces enough rows for a second page.
//
// Those five gaps are exactly where firefly/fireflytest still speaks only for
// itself, so they are what is here.

const livePending = "pending"

// liveSession bootstraps once for the package. Passport is happy to mint a token
// per test, but a shared one keeps a failing run's logs about the failure rather
// than about eight identical registrations.
type liveInstance struct {
	env   fireflylive.Env
	token string
}

var liveSession = sync.OnceValues(func() (liveInstance, error) {
	e, err := fireflylive.EnvFromOS()
	if err != nil {
		return liveInstance{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	token, err := fireflylive.Bootstrap(ctx, e.BaseURL)
	if err != nil {
		return liveInstance{}, fmt.Errorf("bootstrapping %s: %w", e.BaseURL, err)
	}
	if err := fireflylive.AssertDisposable(context.Background(), e.BaseURL, token); err != nil {
		return liveInstance{}, err
	}
	return liveInstance{env: e, token: token}, nil
})

type live struct {
	t     *testing.T
	base  string
	token string
	ns    string
	c     *firefly.Client
	st    *firefly.Store
}

// newLiveStore fails rather than skips when there is no instance, for the same
// reason the rest of this arrangement does: the build tag has already decided
// these tests run, and a skip would turn a broken bootstrap into a green result.
func newLiveStore(t *testing.T) *live {
	t.Helper()

	inst, err := liveSession()
	if err != nil {
		t.Fatal(err)
	}
	c := firefly.New(inst.env.BaseURL, inst.token, firefly.WithBackoffBase(time.Millisecond))
	return &live{
		t: t, base: inst.env.BaseURL, token: inst.token,
		ns: fireflylive.Namespace(inst.env.RunID, t.Name()),
		c:  c,
		st: firefly.NewStore(c, firefly.Config{PendingTag: livePending}),
	}
}

// account creates the asset account this test works on. The IBAN is generated
// from the namespace rather than written by hand: Firefly validates it, and
// every literal in this repository that was written by hand failed that check.
func (l *live) account(n int) *budget.Account {
	l.t.Helper()
	spec := budget.AccountSpec{
		Name:     fmt.Sprintf("%sAsset-%d", l.ns, n),
		Currency: "EUR",
		IBAN:     iban.GenerateDE(l.ns, n),
	}
	a, err := l.st.GetOrCreateAccount(context.Background(), spec)
	if err != nil {
		l.t.Fatalf("creating asset account %q: %v", spec.Name, err)
	}
	return a
}

func (l *live) fields(d time.Time, cents int64, payee, ref string) budget.ImportedFields {
	return budget.ImportedFields{
		Date: d, AmountCents: cents, Currency: "EUR",
		PayeeName: payee, ImportedPayee: payee, ExternalRef: ref, Cleared: true,
	}
}

func liveDay(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// TestStoreLive_acceptsAGeneratedIBANAndFindsTheAccountByIt is the one that
// justifies internal/iban existing. It also pins the lookup that keeps a renamed
// account from being opened a second time — the case a fixture cannot settle,
// because a fixture accepts whatever IBAN it is handed.
func TestStoreLive_acceptsAGeneratedIBANAndFindsTheAccountByIt(t *testing.T) {
	l := newLiveStore(t)
	ctx := context.Background()

	spec := budget.AccountSpec{
		Name: l.ns + "Girokonto", Currency: "EUR", IBAN: iban.GenerateDE(l.ns, 1),
	}
	created, err := l.st.GetOrCreateAccount(ctx, spec)
	if err != nil {
		t.Fatalf("creating an account with a generated IBAN: %v", err)
	}

	l.rename(created.ID, l.ns+"Hauptkonto")

	again, err := l.st.GetOrCreateAccount(ctx, spec)
	if err != nil {
		t.Fatalf("resolving the account after a rename: %v", err)
	}
	if again.ID != created.ID {
		t.Fatalf("the rename produced a second account (%s then %s); the IBAN lookup did not hold, "+
			"and a real sync would now split one account's history across two", created.ID, again.ID)
	}
}

// TestStoreLive_refusesAnInventedIBAN records what Firefly does with an IBAN that
// is merely IBAN-shaped.
//
// The literal below is one this repository actually used until a real instance
// entered the picture. If this test ever fails because Firefly accepted it, that
// is a finding rather than a defect: it would mean internal/iban and the repo-wide
// literal check are solving a problem Firefly does not have, and both should go.
func TestStoreLive_refusesAnInventedIBAN(t *testing.T) {
	l := newLiveStore(t)

	_, err := l.st.GetOrCreateAccount(context.Background(), budget.AccountSpec{
		Name: l.ns + "Erfunden", Currency: "EUR", IBAN: "DE11111111111111111111",
	})
	if err == nil {
		t.Fatal("Firefly accepted an IBAN with wrong check digits; internal/iban and " +
			"TestRepoIBANLiteralsAreValid exist only because it was assumed not to, " +
			"and should be removed rather than worked around")
	}
}

// TestStoreLive_refusesToUpdateAGroupTheUserSplit builds the split group over the
// API, the way a person would in the UI.
//
// The fixture fabricates this state by hand, which makes it the weakest of its
// assumptions: it asserts that our guard fires without ever proving the shape it
// fires on is the shape Firefly produces.
func TestStoreLive_refusesToUpdateAGroupTheUserSplit(t *testing.T) {
	l := newLiveStore(t)
	ctx := context.Background()
	asset := l.account(1)

	groupID, journalID := l.postSplitGroup(asset.ID, l.ns+"Baumarkt")
	txnID, err := firefly.EncodeID(groupID, journalID)
	if err != nil {
		t.Fatalf("EncodeID: %v", err)
	}

	tx := &budget.Transaction{ID: txnID, AccountID: asset.ID}
	if err := l.st.Update(ctx, tx, budget.Patch{Cleared: budget.Bool(false)}); err == nil {
		t.Fatal("updating a group the user split must be refused; going ahead would send back " +
			"one split and Firefly deletes every journal missing from the array")
	}

	if got := len(l.splits(groupID)); got != 2 {
		t.Errorf("the user's splits: got %d, want both still there", got)
	}
}

// TestStoreLive_updateOnADeletedGroupReportsGone covers the status Firefly really
// answers with, which is the part the fixture had to guess.
func TestStoreLive_updateOnADeletedGroupReportsGone(t *testing.T) {
	l := newLiveStore(t)
	ctx := context.Background()
	asset := l.account(1)

	created, err := l.st.Create(ctx, asset.ID,
		l.fields(liveDay(2026, time.July, 10), -1000, l.ns+"Shop", l.ns+"gone"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	groupID, _, err := firefly.SplitID(created.ID)
	if err != nil {
		t.Fatalf("SplitID: %v", err)
	}
	if err := fireflylive.DeleteTransactionGroup(ctx, l.base, l.token, groupID); err != nil {
		t.Fatalf("deleting the group: %v", err)
	}

	err = l.st.Update(ctx, created, budget.Patch{Cleared: budget.Bool(false)})
	if err == nil {
		t.Fatal("updating a deleted group was reported as success")
	}
	if !errors.Is(err, budget.ErrGone) {
		t.Fatalf("a deleted transaction must surface as budget.ErrGone: got %v.\n"+
			"Firefly answers 401 \"Unauthenticated\" here rather than 404, so the Store has "+
			"to disambiguate it from a dead token. Nothing branches on ErrGone, but without "+
			"it whoever deleted the transaction reads an authentication failure in the sync "+
			"log and goes looking at their credentials", err)
	}
}

// TestStoreLive_externalRefLookupIsExact settles what external_id_is means.
//
// It is a search DSL rather than an API contract, and the fixture can only echo
// back whichever reading it was written with. Both directions are checked: a
// substring must not match, and the exact reference must — a lookup that found
// nothing at all would pass the first half while being useless.
func TestStoreLive_externalRefLookupIsExact(t *testing.T) {
	l := newLiveStore(t)
	ctx := context.Background()
	asset := l.account(1)

	exact := l.ns + "ref"
	longer := "xx" + exact + "yy"

	if _, err := l.st.Create(ctx, asset.ID,
		l.fields(liveDay(2026, time.July, 10), -1000, l.ns+"Shop", longer)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := l.st.FindByExternalRef(ctx, asset.ID, exact)
	if err != nil {
		t.Fatalf("FindByExternalRef(%q): %v", exact, err)
	}
	if got != nil {
		t.Fatalf("looking up %q found the transaction carrying %q. If external_id_is degrades "+
			"to a substring match, the returned reference has to be verified or an unrelated "+
			"transaction is adopted and then overwritten", exact, got.ExternalRef)
	}

	found, err := l.st.FindByExternalRef(ctx, asset.ID, longer)
	if err != nil {
		t.Fatalf("FindByExternalRef(%q): %v", longer, err)
	}
	if found == nil {
		t.Fatalf("the exact reference %q was not found either, so the negative result above "+
			"proves nothing about matching", longer)
	}
	if found.ExternalRef != longer {
		t.Errorf("external ref: got %q, want %q", found.ExternalRef, longer)
	}
}

// TestStoreLive_listTransactionsWalksEveryPage is the only test anywhere that
// makes GetPaged loop against a real server.
//
// The fixture's DefaultPerPage of two is an invention, so until now the loop was
// exercised only against a number this repository made up.
//
// The count is one more than defaultPageSize in firefly/client.go, which is what
// GetPaged asks for when a caller names no limit — so this is exactly two pages,
// and the second one exists only if the loop runs. Raising defaultPageSize
// without raising this count would leave the test passing on a single page,
// proving nothing.
func TestStoreLive_listTransactionsWalksEveryPage(t *testing.T) {
	l := newLiveStore(t)
	ctx := context.Background()
	asset := l.account(1)

	const want = 101 // defaultPageSize (100) + 1; see the comment above
	day := liveDay(2026, time.July, 10)
	for i := range want {
		_, err := l.st.Create(ctx, asset.ID,
			l.fields(day, -int64(100+i), fmt.Sprintf("%sShop-%d", l.ns, i),
				fmt.Sprintf("%spage-%03d", l.ns, i)))
		if err != nil {
			t.Fatalf("creating transaction %d of %d: %v", i+1, want, err)
		}
	}

	got, err := l.st.ListTransactions(ctx, asset.ID, day, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	if len(got) != want {
		t.Fatalf("got %d transactions, want %d — the paging loop stopped early, which in a "+
			"real sync means every row past the first page is reimported as new", len(got), want)
	}

	seen := map[string]bool{}
	for _, tx := range got {
		if seen[tx.ID] {
			t.Fatalf("transaction %s came back twice; pages are overlapping", tx.ID)
		}
		seen[tx.ID] = true
	}
}

// TestStoreLive_calendarDaySurvivesTheRoundTrip is the assertion the CI container
// runs in America/New_York for.
//
// A date sent as an instant renders in the server's own zone. East of UTC that
// lands on the same calendar day and nothing looks wrong; west of it the day
// moves backwards. That defect was live in this repository, and the fixture
// echoed the submitted string back, so nothing caught it.
func TestStoreLive_calendarDaySurvivesTheRoundTrip(t *testing.T) {
	l := newLiveStore(t)
	ctx := context.Background()
	asset := l.account(1)

	day := liveDay(2026, time.July, 10)
	if _, err := l.st.Create(ctx, asset.ID,
		l.fields(day, -1234, l.ns+"Tagesprobe", l.ns+"day")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := l.st.ListTransactions(ctx, asset.ID, day, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d transactions in a one-day window, want 1: the stored day is not the "+
			"day that was sent", len(got))
	}
	if y, m, d := got[0].Date.Date(); y != 2026 || m != time.July || d != 10 {
		t.Errorf("date came back as %04d-%02d-%02d, want 2026-07-10", y, int(m), d)
	}
}

// rename changes the account's name behind the Store's back, the way a person
// would in the UI.
//
// Only the name is sent, and that is the opposite of what the transaction seeder
// has to do. Firefly's two update endpoints differ: a transaction PUT deletes
// every journal missing from the splits array, so it demands a read-modify-write,
// while an account PUT leaves untouched fields alone and rejects a full echo —
// the attributes a GET returns include liability_type and interest as null, and
// the validator refuses those on an asset account.
//
// Whether the IBAN survives is not assumed here: if a partial update erased it,
// the lookup this test performs next would open a second account and the test
// would say so.
func (l *live) rename(accountID, name string) {
	l.t.Helper()

	if _, err := l.c.Put(context.Background(), "/api/v1/accounts/"+accountID,
		map[string]any{"name": name}); err != nil {
		l.t.Fatalf("renaming account %s: %v", accountID, err)
	}
}

// postSplitGroup creates a two-split withdrawal group, which is what Firefly
// stores when a user splits a transaction in the UI.
func (l *live) postSplitGroup(assetID, payee string) (groupID, firstJournalID string) {
	l.t.Helper()

	split := func(amount, description string) map[string]any {
		return map[string]any{
			"type": "withdrawal", "source_id": assetID, "destination_name": payee,
			"date":   firefly.FormatDate(liveDay(2026, time.July, 10)),
			"amount": amount, "description": description, "currency_code": "EUR",
		}
	}
	body, err := l.c.Post(context.Background(), "/api/v1/transactions", map[string]any{
		"apply_rules": false, "fire_webhooks": false,
		"group_title": payee,
		"transactions": []map[string]any{
			split("6.00", payee+" Teil 1"),
			split("4.00", payee+" Teil 2"),
		},
	})
	if err != nil {
		l.t.Fatalf("creating a user split group: %v", err)
	}

	raw, err := firefly.Unwrap(body)
	if err != nil {
		l.t.Fatalf("unwrapping the created group: %v", err)
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
		l.t.Fatalf("decoding the created group: %v", err)
	}
	if len(group.Attributes.Transactions) != 2 {
		l.t.Fatalf("the group came back with %d splits, want 2; Firefly did not store what was sent",
			len(group.Attributes.Transactions))
	}
	return group.ID, group.Attributes.Transactions[0].JournalID
}

func (l *live) splits(groupID string) []map[string]any {
	l.t.Helper()

	body, err := l.c.Get(context.Background(), "/api/v1/transactions/"+groupID, nil)
	if err != nil {
		l.t.Fatalf("reading group %s: %v", groupID, err)
	}
	raw, err := firefly.Unwrap(body)
	if err != nil {
		l.t.Fatalf("unwrapping group %s: %v", groupID, err)
	}
	var group struct {
		Attributes struct {
			Transactions []map[string]any `json:"transactions"`
		} `json:"attributes"`
	}
	if err := json.Unmarshal(raw, &group); err != nil {
		l.t.Fatalf("decoding group %s: %v", groupID, err)
	}
	return group.Attributes.Transactions
}
