package firefly_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"bankingsync/budget"
	"bankingsync/firefly"
	"bankingsync/firefly/fireflytest"
)

func newStore(t *testing.T) (*fireflytest.Server, *firefly.Store) {
	t.Helper()
	s := fireflytest.New(t)
	c := firefly.New(s.URL, s.Token(),
		firefly.WithHTTPClient(s.Client()),
		firefly.WithBackoffBase(time.Millisecond))
	return s, firefly.NewStore(c, firefly.Config{PendingTag: "pending"})
}

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func fields(d time.Time, cents int64, payee, ref string, cleared bool) budget.ImportedFields {
	return budget.ImportedFields{
		Date: d, AmountCents: cents, Currency: "EUR",
		PayeeName: payee, ImportedPayee: payee, ExternalRef: ref, Cleared: cleared,
	}
}

func TestStore_getOrCreateAccountPrefersIBAN(t *testing.T) {
	s, st := newStore(t)
	s.AddAccount("Falsch getippt", "asset", "EUR", "DE63111111111111111111")

	got, err := st.GetOrCreateAccount(context.Background(), budget.AccountSpec{
		Name: "Checking", Currency: "EUR", IBAN: "DE63111111111111111111",
	})
	if err != nil {
		t.Fatalf("GetOrCreateAccount: %v", err)
	}
	if got.Name != "Falsch getippt" {
		t.Errorf("the IBAN must win over the name, got %q", got.Name)
	}
	if len(s.Accounts()) != 1 {
		t.Errorf("no second account may be created, got %d", len(s.Accounts()))
	}
}

func TestStore_getOrCreateAccountCreatesWithIBAN(t *testing.T) {
	s, st := newStore(t)

	got, err := st.GetOrCreateAccount(context.Background(), budget.AccountSpec{
		Name: "Checking", Currency: "EUR", IBAN: "DE90222222222222222222",
	})
	if err != nil {
		t.Fatalf("GetOrCreateAccount: %v", err)
	}
	if got.ID == "" {
		t.Fatal("expected an account id")
	}
	created := s.Accounts()[0]
	if created.IBAN != "DE90222222222222222222" {
		t.Errorf("the IBAN must be written so the next run can match on it, got %q", created.IBAN)
	}
	if created.Role != "defaultAsset" {
		t.Errorf("account_role: got %q, want defaultAsset", created.Role)
	}
}

func TestStore_currencyMismatchFailsLoudly(t *testing.T) {
	s, st := newStore(t)
	s.AddAccount("Checking", "asset", "USD", "DE63111111111111111111")

	_, err := st.GetOrCreateAccount(context.Background(), budget.AccountSpec{
		Name: "Checking", Currency: "EUR", IBAN: "DE63111111111111111111",
	})
	if err == nil {
		t.Fatal("Firefly overrules the submitted currency silently, so a mismatch must stop the import")
	}
	if !strings.Contains(err.Error(), "USD") || !strings.Contains(err.Error(), "EUR") {
		t.Errorf("the error should name both currencies: %v", err)
	}
}

func TestStore_createWithdrawalAndDeposit(t *testing.T) {
	s, st := newStore(t)
	ctx := context.Background()
	asset, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR", IBAN: "DE1"})

	out, err := st.Create(ctx, asset.ID, fields(day(2026, time.July, 10), -1250, "Rewe", "ref-1", true))
	if err != nil {
		t.Fatalf("Create withdrawal: %v", err)
	}
	if out.AmountCents != -1250 {
		t.Errorf("outgoing amount: got %d, want -1250", out.AmountCents)
	}

	in, err := st.Create(ctx, asset.ID, fields(day(2026, time.July, 11), 5000, "Arbeitgeber", "ref-2", true))
	if err != nil {
		t.Fatalf("Create deposit: %v", err)
	}
	if in.AmountCents != 5000 {
		t.Errorf("incoming amount: got %d, want 5000", in.AmountCents)
	}

	for _, g := range s.Groups() {
		sp := g.Splits[0]
		if strings.HasPrefix(sp.Amount, "-") {
			t.Errorf("amounts must be submitted positive, got %q", sp.Amount)
		}
	}
}

func TestStore_pendingCarriesTheTagAndBookingRemovesIt(t *testing.T) {
	s, st := newStore(t)
	ctx := context.Background()
	asset, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR", IBAN: "DE1"})

	pending, err := st.Create(ctx, asset.ID, fields(day(2026, time.July, 10), -1000, "Tankstelle", "auth-1", false))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if pending.Cleared {
		t.Error("a pending transaction must not read back as cleared")
	}
	if got := s.Groups()[0].Splits[0].Tags; len(got) != 1 || got[0] != "pending" {
		t.Fatalf("expected the pending tag, got %v", got)
	}

	if err := st.Update(ctx, pending, budget.Patch{Cleared: budget.Bool(true)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := s.Groups()[0].Splits[0].Tags; len(got) != 0 {
		t.Errorf("booking must remove the pending tag, got %v", got)
	}
}

func TestStore_bookingKeepsUserTags(t *testing.T) {
	s, st := newStore(t)
	ctx := context.Background()
	asset, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR", IBAN: "DE1"})

	pending, _ := st.Create(ctx, asset.ID, fields(day(2026, time.July, 10), -1000, "Tankstelle", "auth-1", false))

	groups := s.Groups()
	s.DeleteGroup(groups[0].ID)
	s.AddGroup(fireflytest.Split{
		Type: "withdrawal", Date: s.FormatDate(day(2026, time.July, 10)), Amount: "10.00",
		Description: "Tankstelle", SourceID: asset.ID, DestinationName: "Shop",
		ExternalID: "auth-1", Tags: []string{"pending", "urlaub"},
	})
	pending.ID = mustEncode(t, s.Groups()[0].ID, s.Groups()[0].Splits[0].JournalID)

	if err := st.Update(ctx, pending, budget.Patch{Cleared: budget.Bool(true)}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got := s.Groups()[0].Splits[0].Tags
	if len(got) != 1 || got[0] != "urlaub" {
		t.Errorf("only our marker may be removed, the user's tags must survive: got %v", got)
	}
}

func TestStore_refusesToUpdateAUserSplitGroup(t *testing.T) {
	s, st := newStore(t)
	ctx := context.Background()
	asset, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR", IBAN: "DE1"})

	created, err := st.Create(ctx, asset.ID, fields(day(2026, time.July, 10), -1000, "Baumarkt", "ref-1", false))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	groupID := s.Groups()[0].ID
	if err := s.SplitGroup(groupID); err != nil {
		t.Fatalf("SplitGroup: %v", err)
	}

	err = st.Update(ctx, created, budget.Patch{Cleared: budget.Bool(true)})
	if err == nil {
		t.Fatal("updating a group the user split would delete their work and must be refused")
	}
	if got := len(s.Groups()[0].Splits); got != 2 {
		t.Errorf("the user's splits must be untouched, got %d", got)
	}
}

func TestStore_updateOnDeletedGroupReportsGone(t *testing.T) {
	s, st := newStore(t)
	ctx := context.Background()
	asset, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR", IBAN: "DE1"})
	created, _ := st.Create(ctx, asset.ID, fields(day(2026, time.July, 10), -1000, "Shop", "ref-1", false))

	s.DeleteGroup(s.Groups()[0].ID)

	err := st.Update(ctx, created, budget.Patch{Cleared: budget.Bool(true)})
	if err == nil {
		t.Fatal("expected an error for a deleted group")
	}
	if !isGone(err) {
		t.Errorf("a deleted transaction must be reported as gone so it is not recreated: %v", err)
	}
}

func TestStore_reconciledTransactionIsNotTouched(t *testing.T) {
	s, st := newStore(t)
	ctx := context.Background()
	asset, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR", IBAN: "DE1"})
	g := s.AddGroup(fireflytest.Split{
		Type: "withdrawal", Date: s.FormatDate(day(2026, time.July, 10)), Amount: "10.00",
		Description: "Abgestimmt", SourceID: asset.ID, DestinationName: "Shop",
		ExternalID: "ref-1", Reconciled: true,
	})

	tx := &budget.Transaction{ID: mustEncode(t, g.ID, g.Splits[0].JournalID), AccountID: asset.ID}
	if err := st.Update(ctx, tx, budget.Patch{AmountCents: budget.Int64(-9999)}); err != nil {
		t.Fatalf("Update must be a silent no-op, got: %v", err)
	}
	if got := s.Groups()[0].Splits[0].Amount; got != "10.00" {
		t.Errorf("a reconciled amount must not change, got %q", got)
	}
}

func TestStore_findByExternalRefIsAccountScoped(t *testing.T) {
	_, st := newStore(t)
	ctx := context.Background()
	a, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "A", Currency: "EUR", IBAN: "DE1"})
	b, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "B", Currency: "EUR", IBAN: "DE2"})

	if _, err := st.Create(ctx, b.ID, fields(day(2026, time.July, 10), -1000, "Shop", "shared", true)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := st.FindByExternalRef(ctx, a.ID, "shared")
	if err != nil {
		t.Fatalf("FindByExternalRef: %v", err)
	}
	if got != nil {
		t.Fatal("Firefly's search is global — a ref belonging to another account must be filtered out here")
	}

	got, err = st.FindByExternalRef(ctx, b.ID, "shared")
	if err != nil {
		t.Fatalf("FindByExternalRef: %v", err)
	}
	if got == nil {
		t.Fatal("the owning account must find it")
	}
}

func TestStore_listSkipsUserSplitGroups(t *testing.T) {
	s, st := newStore(t)
	ctx := context.Background()
	asset, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR", IBAN: "DE1"})
	if _, err := st.Create(ctx, asset.ID, fields(day(2026, time.July, 10), -1000, "Baumarkt", "ref-1", true)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.SplitGroup(s.Groups()[0].ID); err != nil {
		t.Fatalf("SplitGroup: %v", err)
	}

	from, to := budget.WindowBounds(day(2026, time.July, 10))
	got, err := st.ListTransactions(ctx, asset.ID, from, to)
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a user-split group must not be offered as a match candidate, got %d", len(got))
	}
}

func TestStore_listWindowIsHalfOpen(t *testing.T) {
	s, st := newStore(t)
	ctx := context.Background()
	asset, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR", IBAN: "DE1"})
	target := day(2026, time.July, 15)

	for _, off := range []int{-8, -7, 7, 8} {
		s.AddGroup(fireflytest.Split{
			Type: "withdrawal", Date: s.FormatDate(target.AddDate(0, 0, off)), Amount: "10.00",
			Description: "Txn", SourceID: asset.ID, DestinationName: "Shop",
			ExternalID: offsetRef(off),
		})
	}

	from, to := budget.WindowBounds(target)
	got, err := st.ListTransactions(ctx, asset.ID, from, to)
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	seen := map[string]bool{}
	for _, g := range got {
		seen[g.ExternalRef] = true
	}
	if !seen["off-7"] || !seen["off+7"] {
		t.Errorf("both inclusive edges must be present, got %v", got)
	}
	if seen["off-8"] || seen["off+8"] {
		t.Errorf("days outside the window must be excluded, got %v", seen)
	}
}

func TestStore_transferWhenCounterpartyIsOurOwnAccount(t *testing.T) {
	s, st := newStore(t)
	ctx := context.Background()
	checking, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR", IBAN: "DE1"})
	savings, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Savings", Currency: "EUR", IBAN: "DE2"})

	in := fields(day(2026, time.July, 10), -20000, "Eigenuebertrag", "ref-1", true)
	in.CounterpartyIBAN = "DE2"
	if _, err := st.Create(ctx, checking.ID, in); err != nil {
		t.Fatalf("Create: %v", err)
	}

	sp := s.Groups()[0].Splits[0]
	if sp.Type != "transfer" {
		t.Errorf("a movement between two of the user's own accounts is a transfer, got %q", sp.Type)
	}
	if sp.SourceID != checking.ID || sp.DestinationID != savings.ID {
		t.Errorf("both sides must be the real asset accounts, got %s -> %s", sp.SourceID, sp.DestinationID)
	}
}

func TestStore_unknownCounterpartyStaysAWithdrawal(t *testing.T) {
	s, st := newStore(t)
	ctx := context.Background()
	checking, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR", IBAN: "DE1"})

	in := fields(day(2026, time.July, 10), -2000, "Fremder Laden", "ref-1", true)
	in.CounterpartyIBAN = "DE85999999999999999999"
	if _, err := st.Create(ctx, checking.ID, in); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if got := s.Groups()[0].Splits[0].Type; got != "withdrawal" {
		t.Errorf("an IBAN we do not hold must not become a transfer, got %q", got)
	}
}

func TestStore_writesSEPAReferences(t *testing.T) {
	s, st := newStore(t)
	ctx := context.Background()
	asset, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR", IBAN: "DE1"})

	in := fields(day(2026, time.July, 10), -84500, "Vermieter", "ref-1", true)
	in.SEPA = budget.SEPARefs{EndToEnd: "E2E-1", Mandate: "M-4711", CreditorID: "DE98ZZZ09999999999"}
	if _, err := st.Create(ctx, asset.ID, in); err != nil {
		t.Fatalf("Create: %v", err)
	}

	sp := s.Groups()[0].Splits[0]
	if sp.SepaCtID != "E2E-1" {
		t.Errorf("sepa_ct_id (end-to-end, EREF): got %q", sp.SepaCtID)
	}
	if sp.SepaDB != "M-4711" {
		t.Errorf("sepa_db (mandate, MREF): got %q", sp.SepaDB)
	}
	if sp.SepaCI != "DE98ZZZ09999999999" {
		t.Errorf("sepa_ci (creditor id, CRED): got %q", sp.SepaCI)
	}
}

func mustEncode(t *testing.T, groupID, journalID string) string {
	t.Helper()
	id, err := firefly.EncodeID(groupID, journalID)
	if err != nil {
		t.Fatalf("EncodeID: %v", err)
	}
	return id
}

func isGone(err error) bool { return errors.Is(err, budget.ErrGone) }

func offsetRef(off int) string { return fmt.Sprintf("off%+d", off) }

func TestStore_duplicateHashOnlyWithExternalRef(t *testing.T) {
	s, st := newStore(t)
	ctx := context.Background()
	asset, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR", IBAN: "DE1"})

	twin := fields(day(2026, time.July, 10), -350, "Kaffee", "", true)
	if _, err := st.Create(ctx, asset.ID, twin); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := st.Create(ctx, asset.ID, twin); err != nil {
		t.Fatalf("two reference-less twins are legitimate and must both import: %v", err)
	}
	if got := len(s.Groups()); got != 2 {
		t.Errorf("got %d transactions, want 2", got)
	}
}

func TestStore_rejectsZeroAmountBeforeTheAPI(t *testing.T) {
	s, st := newStore(t)
	ctx := context.Background()
	asset, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR", IBAN: "DE1"})
	before := len(s.Requests())

	if _, err := st.Create(ctx, asset.ID, fields(day(2026, time.July, 10), 0, "Nichts", "ref-1", true)); err == nil {
		t.Fatal("a zero amount must be rejected")
	}
	if len(s.Requests()) != before {
		t.Error("the zero amount should never reach the API at all")
	}
}

func TestStore_verifiesExternalIDAgainstTheSearchResult(t *testing.T) {
	s, st := newStore(t)
	ctx := context.Background()
	asset, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR", IBAN: "DE1"})

	if _, err := st.Create(ctx, asset.ID, fields(day(2026, time.July, 10), -1000, "Shop", "xxref-1yy", true)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	s.SetSearchContains(true)

	got, err := st.FindByExternalRef(ctx, asset.ID, "ref-1")
	if err != nil {
		t.Fatalf("FindByExternalRef: %v", err)
	}
	if got != nil {
		t.Fatalf("external_id_is is a search DSL, not an API contract. If it ever degrades to a "+
			"substring match, the returned value must be verified or the wrong transaction is adopted: got %q", got.ExternalRef)
	}
}

func TestGetOrCreateAccount_survivesARenameInFirefly(t *testing.T) {
	srv, st := newStore(t)
	ctx := context.Background()

	spec := budget.AccountSpec{Name: "Sparkasse Girokonto", Currency: "EUR", IBAN: "DE31123456789005193987"}
	created, err := st.GetOrCreateAccount(ctx, spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := srv.RenameAccount(created.ID, "Mein Hauptkonto"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	again, err := st.GetOrCreateAccount(ctx, spec)
	if err != nil {
		t.Fatalf("after rename: %v", err)
	}
	if again.ID != created.ID {
		t.Fatalf("rename created a second account (%s -> %s); the IBAN lookup did not hold",
			created.ID, again.ID)
	}
	if got := len(srv.Accounts()); got != 1 {
		t.Errorf("account count: got %d, want 1", got)
	}
}

func TestGetOrCreateAccount_ibanSubstringHitIsNotAdopted(t *testing.T) {
	srv, st := newStore(t)

	srv.AddAccount("Someone else", "asset", "EUR", "DE31123456789005193987EXTRA")

	spec := budget.AccountSpec{Name: "Mine", Currency: "EUR", IBAN: "DE31123456789005193987"}
	got, err := st.GetOrCreateAccount(context.Background(), spec)
	if err != nil {
		t.Fatalf("GetOrCreateAccount: %v", err)
	}
	for _, a := range srv.Accounts() {
		if a.ID == got.ID && a.IBAN != spec.IBAN {
			t.Fatalf("adopted account %q with IBAN %q on a substring hit", a.Name, a.IBAN)
		}
	}
}

func TestGetOrCreateAccount_nameSubstringHitIsNotAdopted(t *testing.T) {
	srv, st := newStore(t)

	other := srv.AddAccount("Old Checking", "asset", "EUR", "")

	got, err := st.GetOrCreateAccount(context.Background(),
		budget.AccountSpec{Name: "Checking", Currency: "EUR"})
	if err != nil {
		t.Fatalf("GetOrCreateAccount: %v", err)
	}
	if got.ID == other.ID {
		t.Fatal(`"Checking" adopted "Old Checking" — Firefly's name search is a substring match`)
	}
	if got.Name != "Checking" {
		t.Errorf("name: got %q, want Checking", got.Name)
	}
}

func TestGetOrCreateAccount_recordsIBANOnAnAccountMatchedByName(t *testing.T) {
	srv, st := newStore(t)

	existing := srv.AddAccount("Checking", "asset", "EUR", "")

	spec := budget.AccountSpec{Name: "Checking", Currency: "EUR", IBAN: "DE31123456789005193987"}
	if _, err := st.GetOrCreateAccount(context.Background(), spec); err != nil {
		t.Fatalf("GetOrCreateAccount: %v", err)
	}

	for _, a := range srv.Accounts() {
		if a.ID == existing.ID && a.IBAN != spec.IBAN {
			t.Errorf("IBAN not recorded (got %q) — renaming this account later would "+
				"orphan it and the next sync would open a second one", a.IBAN)
		}
	}
}

func TestSetOpeningBalance_sendsBothFieldsAndRoundTrips(t *testing.T) {
	srv, st := newStore(t)
	ctx := context.Background()

	acct, err := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Giro", Currency: "EUR"})
	if err != nil {
		t.Fatalf("GetOrCreateAccount: %v", err)
	}

	ob := budget.OpeningBalance{
		AmountCents: -123456,
		Currency:    "EUR",
		Date:        day(2026, time.July, 14),
		Ref:         "bankingsync-opening-DE77",
		PayeeName:   "Starting Balance",
	}
	written, err := st.SetOpeningBalance(ctx, acct.ID, ob)
	if err != nil {
		t.Fatalf("SetOpeningBalance: %v", err)
	}
	if !written {
		t.Fatal("written = false on a fresh account")
	}

	for _, a := range srv.Accounts() {
		if a.ID != acct.ID {
			continue
		}
		if a.OpeningBalance != "-1234.56" {
			t.Errorf("opening_balance: got %q, want -1234.56 — an overdraft must keep "+
				"its sign", a.OpeningBalance)
		}
		if a.OpeningBalanceDate != "2026-07-14" {
			t.Errorf("opening_balance_date: got %q", a.OpeningBalanceDate)
		}
	}
}

func TestSetOpeningBalance_refusesWhenAlreadySet(t *testing.T) {
	srv, st := newStore(t)
	ctx := context.Background()

	acct, err := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Giro", Currency: "EUR"})
	if err != nil {
		t.Fatalf("GetOrCreateAccount: %v", err)
	}
	ob := budget.OpeningBalance{AmountCents: 100000, Currency: "EUR", Date: day(2026, time.July, 14), Ref: "r"}
	if _, err := st.SetOpeningBalance(ctx, acct.ID, ob); err != nil {
		t.Fatalf("first write: %v", err)
	}

	ob.AmountCents = 999999
	written, err := st.SetOpeningBalance(ctx, acct.ID, ob)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if written {
		t.Error("written = true on the second call — the caller never re-adjusts, so " +
			"this would silently replace a correct balance")
	}
	for _, a := range srv.Accounts() {
		if a.ID == acct.ID && a.OpeningBalance != "1000.00" {
			t.Errorf("opening balance was overwritten: got %q, want 1000.00", a.OpeningBalance)
		}
	}
}

func TestSetOpeningBalance_preservesNameCurrencyAndIBAN(t *testing.T) {
	srv, st := newStore(t)
	ctx := context.Background()

	spec := budget.AccountSpec{Name: "Sparkasse Giro", Currency: "EUR", IBAN: "DE31123456789005193987"}
	acct, err := st.GetOrCreateAccount(ctx, spec)
	if err != nil {
		t.Fatalf("GetOrCreateAccount: %v", err)
	}
	if _, err := st.SetOpeningBalance(ctx, acct.ID, budget.OpeningBalance{
		AmountCents: 5000, Currency: "EUR", Date: day(2026, time.July, 14), Ref: "r",
	}); err != nil {
		t.Fatalf("SetOpeningBalance: %v", err)
	}

	for _, a := range srv.Accounts() {
		if a.ID != acct.ID {
			continue
		}
		if a.Name != spec.Name || a.CurrencyCode != "EUR" || a.IBAN != spec.IBAN {
			t.Errorf("a partial PUT clobbered the account: %+v", a)
		}
	}
}

func TestAccountBalance_keepsTheSignOfAnOverdraft(t *testing.T) {
	_, st := newStore(t)
	ctx := context.Background()

	acct, err := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Giro", Currency: "EUR"})
	if err != nil {
		t.Fatalf("GetOrCreateAccount: %v", err)
	}
	if _, err := st.SetOpeningBalance(ctx, acct.ID, budget.OpeningBalance{
		AmountCents: -80000, Currency: "EUR", Date: day(2026, time.July, 14), Ref: "r",
	}); err != nil {
		t.Fatalf("SetOpeningBalance: %v", err)
	}

	got, err := st.AccountBalance(ctx, acct.ID)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if got != -80000 {
		t.Errorf("balance: got %d, want -80000 — reading it through the unsigned "+
			"parser turns an overdraft into credit", got)
	}
}

func TestAccountBalance_includesImportedTransactions(t *testing.T) {
	_, st := newStore(t)
	ctx := context.Background()

	acct, err := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Giro", Currency: "EUR"})
	if err != nil {
		t.Fatalf("GetOrCreateAccount: %v", err)
	}
	if _, err := st.SetOpeningBalance(ctx, acct.ID, budget.OpeningBalance{
		AmountCents: 100000, Currency: "EUR", Date: day(2026, time.July, 14), Ref: "r",
	}); err != nil {
		t.Fatalf("SetOpeningBalance: %v", err)
	}
	if _, err := st.Create(ctx, acct.ID, fields(day(2026, time.July, 20), -2500, "Rewe", "t1", true)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := st.AccountBalance(ctx, acct.ID)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if got != 97500 {
		t.Errorf("balance: got %d, want 97500 (100000 opening − 2500 spent)", got)
	}
}

// The bug this pins was found against a live instance, not here: FormatDate used
// to send "…T00:00:00+00:00", Firefly read that as an instant and rendered it in
// the server's own timezone, and on any server west of UTC every imported
// transaction landed one calendar day early — consistently enough that nothing
// ever looked broken.
//
// The fixture could not have caught it either, because it echoed the submitted
// date string back verbatim. It now models the real behaviour, which is what
// makes this test meaningful.
func TestStore_calendarDaySurvivesAServerWestOfUTC(t *testing.T) {
	for _, offset := range []string{"+02:00", "+00:00", "-05:00", "-11:00", "+13:00"} {
		t.Run(offset, func(t *testing.T) {
			srv, st := newStore(t)
			srv.SetOffset(offset)
			ctx := context.Background()

			acct, err := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Giro", Currency: "EUR"})
			if err != nil {
				t.Fatalf("GetOrCreateAccount: %v", err)
			}

			want := day(2026, time.July, 1)
			if _, err := st.Create(ctx, acct.ID, fields(want, -1000, "Shop", "ref-1", true)); err != nil {
				t.Fatalf("Create: %v", err)
			}

			lo, hi := budget.WindowBounds(want)
			got, err := st.ListTransactions(ctx, acct.ID, lo, hi)
			if err != nil {
				t.Fatalf("ListTransactions: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("transactions: got %d, want 1", len(got))
			}
			if d := got[0].Date.Format("2006-01-02"); d != "2026-07-01" {
				t.Errorf("calendar day: got %s, want 2026-07-01 — the date was sent as "+
					"an instant and the server's offset moved it", d)
			}
		})
	}
}

// TestStore_deletedGroupIsGoneEvenThoughFireflySays401 pins the mapping a live
// instance forced.
//
// Firefly answers a missing transaction with 401 "Unauthenticated" rather than
// 404 — for a deleted group, for an id that never existed, and for an id that is
// not a number. The fixture claimed 404 for as long as nobody had asked, and the
// Store mapped only that to budget.ErrGone, so against the real thing a deleted
// transaction was reported to the user as an authentication problem.
func TestStore_deletedGroupIsGoneEvenThoughFireflySays401(t *testing.T) {
	s, st := newStore(t)
	ctx := context.Background()
	asset, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR", IBAN: "DE1"})
	created, _ := st.Create(ctx, asset.ID, fields(day(2026, time.July, 10), -1000, "Shop", "ref-1", false))

	s.DeleteGroup(s.Groups()[0].ID)

	err := st.Update(ctx, created, budget.Patch{Cleared: budget.Bool(true)})
	if err == nil {
		t.Fatal("expected an error for a deleted group")
	}
	if !isGone(err) {
		t.Errorf("a deleted transaction must be reported as gone, not as an auth failure: %v", err)
	}
}

// TestStore_badTokenIsNotMistakenForAGoneTransaction is the other half of that
// mapping, and the reason it costs an extra request rather than treating every
// 401 as absence.
//
// A dead token says exactly what a missing transaction says. Reporting it as
// "transaction no longer exists" would send whoever reads the log looking for a
// deletion that never happened, and would hide the one problem that stops every
// write in the run.
func TestStore_badTokenIsNotMistakenForAGoneTransaction(t *testing.T) {
	s, st := newStore(t)
	ctx := context.Background()
	asset, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR", IBAN: "DE1"})
	created, _ := st.Create(ctx, asset.ID, fields(day(2026, time.July, 10), -1000, "Shop", "ref-1", false))

	wrong := firefly.New(s.URL, "not-the-token",
		firefly.WithHTTPClient(s.Client()), firefly.WithBackoffBase(time.Millisecond))
	blind := firefly.NewStore(wrong, firefly.Config{PendingTag: "pending"})

	err := blind.Update(ctx, created, budget.Patch{Cleared: budget.Bool(true)})
	if err == nil {
		t.Fatal("an unauthenticated update was reported as success")
	}
	if isGone(err) {
		t.Errorf("a rejected token was reported as a vanished transaction: %v", err)
	}
}

// TestStore_singleDayWindowIsNotAValidationError covers the range Firefly refuses.
//
// It wants start strictly before end, so a half-open window of exactly one day —
// whose inclusive end is a day back from the exclusive one — collapses onto that
// rule and comes back 422. The production window is fifteen days wide and never
// reaches it; a caller asking for one day is asking something reasonable.
func TestStore_singleDayWindowIsNotAValidationError(t *testing.T) {
	s, st := newStore(t)
	ctx := context.Background()
	asset, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR", IBAN: "DE1"})

	d := day(2026, time.July, 10)
	for i, when := range []time.Time{d.AddDate(0, 0, -1), d, d.AddDate(0, 0, 1)} {
		if _, err := st.Create(ctx, asset.ID,
			fields(when, int64(-100-i), "Shop", offsetRef(i), true)); err != nil {
			t.Fatalf("seeding %s: %v", when.Format("2006-01-02"), err)
		}
	}

	got, err := st.ListTransactions(ctx, asset.ID, d, d.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("a one-day window must not be a validation error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d transactions for a one-day window, want 1: the widened range "+
			"was not trimmed back", len(got))
	}
	if !got[0].Date.Equal(d) {
		t.Errorf("date: got %s, want %s", got[0].Date.Format("2006-01-02"), d.Format("2006-01-02"))
	}
	_ = s
}

// TestStore_emptyWindowAsksNothing keeps a degenerate range from reaching the API
// at all. Firefly would answer it with a validation error, and there is nothing
// to fetch either way.
func TestStore_emptyWindowAsksNothing(t *testing.T) {
	s, st := newStore(t)
	ctx := context.Background()
	asset, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR", IBAN: "DE1"})
	before := len(s.Requests())

	d := day(2026, time.July, 10)
	got, err := st.ListTransactions(ctx, asset.ID, d, d)
	if err != nil {
		t.Fatalf("an empty window is not an error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d transactions for an empty window", len(got))
	}
	if len(s.Requests()) != before {
		t.Error("an empty window should never reach the API")
	}
}
