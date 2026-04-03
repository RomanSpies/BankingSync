package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"bankingsync/actual"
	"bankingsync/enablebanking"
	"bankingsync/store"
	"bankingsync/web"
)

const banner = `
 __________                                 _________      .__
 \______   \ ____   _____ _____    ____    /   _____/_____ |__| ____   ______
  |       _//  _ \ /     \\__  \  /    \   \_____  \\____ \|  |/ __ \ /  ___/
  |    |   (  <_> )  Y Y  \/ __ \|   |  \  /        \  |_> >  \  ___/ \___ \
  |____|_  /\____/|__|_|  (____  /___|  / /_______  /   __/|__|\___  >____  >
         \/             \/     \/     \/          \/|__|           \/     \/
  Roman Spies - Licensed under the GNU AFFERO GENERAL PUBLIC LICENSE, v3.
`

func main() {
	fmt.Print(banner)
	syncHours := envInt("SYNC_INTERVAL_HOURS", 6)
	log.Printf("Starting scheduler (every %dh)", syncHours)

	s, err := newSyncer()
	if err != nil {
		log.Fatalf("Startup failed: %v", err)
	}
	defer s.st.Close()

	webSrv, err := web.New(s.st, s.eb, s.run, web.TemplateFS)
	if err != nil {
		log.Fatalf("Web server init: %v", err)
	}

	shutdown := initTelemetry(s, webSrv.Mux())
	defer shutdown()

	certFile, keyFile, err := ensureTLSCert()
	if err != nil {
		log.Fatalf("TLS cert: %v", err)
	}

	go func() {
		if err := webSrv.StartTLS(envOr("WEB_ADDR", ":8443"), certFile, keyFile); err != nil {
			log.Printf("Web server: %v", err)
		}
	}()

	s.run()
	ticker := time.NewTicker(time.Duration(syncHours) * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		s.run()
	}
}

// Syncer orchestrates a full sync cycle: fetching transactions from Enable
// Banking and importing them into Actual Budget.
type Syncer struct {
	state *State
	st    *store.Store
	ac    *actual.Client
	eb    *enablebanking.Client
	met   *syncMetrics
}

// newSyncer opens the store, loads persisted state, and initialises the Enable Banking client.
func newSyncer() (*Syncer, error) {
	st, err := store.Open(store.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	state, err := LoadFromStore(st)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}

	getter := func(key string) (string, error) { return st.GetSetting(key) }
	eb := enablebanking.NewClient(
		enablebanking.DefaultAppIDResolver(getter),
		enablebanking.DefaultPEMSource(getter),
		ownNames(),
	)

	return &Syncer{state: state, st: st, eb: eb}, nil
}

func (s *Syncer) ensureActual() error {
	if s.ac != nil {
		if err := s.ac.Resync(); err != nil {
			log.Printf("Resync failed (%v) — reconnecting", err)
			s.ac.Close()
			s.ac = nil
			return s.connect()
		}
		return nil
	}
	return s.connect()
}

func (s *Syncer) connect() error {
	ac, err := actual.NewClient(
		mustEnv("ACTUAL_URL"),
		mustEnv("ACTUAL_PASSWORD"),
		mustEnv("ACTUAL_SYNC_ID"),
		"/data/actual-cache",
	)
	if err != nil {
		return err
	}
	s.ac = ac
	return nil
}

func (s *Syncer) run() {
	// Reload session from store in case a new account was connected via the web UI.
	if err := s.state.Reload(s.st); err != nil {
		log.Printf("State reload: %v", err)
	}

	ctx := context.Background()
	tracer := otel.Tracer("bankingsync")
	ctx, span := tracer.Start(ctx, "sync.run")
	defer span.End()

	start := time.Now()
	status := "success"
	defer func() {
		elapsed := time.Since(start).Seconds()
		if s.met != nil {
			s.met.syncRuns.Add(ctx, 1, metric.WithAttributes(attribute.String("status", status)))
			s.met.syncDuration.Record(ctx, elapsed)
		}
		log.Printf("Sync finished in %.1fs (status=%s)", elapsed, status)
	}()

	log.Println("Starting sync...")

	bankAccounts, err := s.st.GetAllBankAccounts()
	if err != nil || len(bankAccounts) == 0 {
		log.Printf("No bank accounts configured — connect via web UI")
		status = "no_session"
		span.SetStatus(codes.Error, "no bank accounts")
		return
	}

	webAddr, _ := s.st.GetSetting("eb_base_url")
	if webAddr == "" {
		webAddr = "https://localhost:8443"
	}

	var dateFrom time.Time
	if s.state.LastSyncDate != "" {
		if d, err := time.Parse("2006-01-02", s.state.LastSyncDate); err == nil {
			dateFrom = d
		}
	}
	if dateFrom.IsZero() {
		dateFrom = time.Now().UTC().AddDate(0, 0, -30)
		log.Println("First run: fetching last 30 days")
	}

	if earliest, ok := s.state.EarliestPendingDate(); ok && earliest.Before(dateFrom) {
		log.Printf("Extending fetch window to %s to cover outstanding pendings",
			earliest.Format("2006-01-02"))
		dateFrom = earliest
	}

	fetchStart := time.Now()
	_, fetchSpan := tracer.Start(ctx, "enable_banking.fetch_transactions",
		trace.WithAttributes(attribute.Int("account_count", len(bankAccounts))),
	)
	var rawTxns []enablebanking.Transaction
	for _, acct := range bankAccounts {
		if t, err := time.Parse(time.RFC3339, acct.SessionExpiry); err == nil {
			daysLeft := int(time.Until(t).Hours() / 24)
			if daysLeft < 7 {
				label := acct.BankName
				if label == "" {
					label = acct.AccountUID
				}
				log.Printf("WARNING: session for %s expires in %d days. Renew via %s", label, daysLeft, webAddr)
				sendEmail(
					fmt.Sprintf("BankingSync: %s session expires in %d days", label, daysLeft),
					fmt.Sprintf("Your Enable Banking session for %s expires in %d days.\n\nRenew it at: %s\n", label, daysLeft, webAddr),
				)
			}
		}
		txns, err := s.eb.FetchTransactions(acct.AccountUID, dateFrom)
		if err != nil {
			log.Printf("Enable Banking error (%s): %v", acct.AccountUID, err)
			status = "fetch_error"
			span.RecordError(err)
			continue
		}
		rawTxns = append(rawTxns, txns...)
	}
	fetchSpan.End()
	if s.met != nil {
		s.met.fetchDuration.Record(ctx, time.Since(fetchStart).Seconds())
	}
	if len(rawTxns) == 0 && status == "fetch_error" {
		span.SetStatus(codes.Error, "all fetches failed")
		return
	}

	if len(rawTxns) == 0 {
		log.Println("No new transactions")
		if err := s.state.SetLastSyncDate(time.Now().UTC().Format("2006-01-02"), s.st); err != nil {
			log.Printf("Failed to save state: %v", err)
		}
		return
	}

	if err := s.state.PruneImportedRefs(s.st); err != nil {
		log.Printf("Prune imported refs: %v", err)
	}

	_, connSpan := tracer.Start(ctx, "actual.ensure_connection")
	connErr := s.ensureActual()
	connSpan.End()
	if connErr != nil {
		log.Printf("Actual error: %v", connErr)
		span.RecordError(connErr)
		span.SetStatus(codes.Error, "connection failed")
		return
	}

	accountName := envOr("ACTUAL_ACCOUNT", "Revolut")
	account, err := s.ac.GetOrCreateAccount(accountName)
	if err != nil {
		log.Printf("Actual error (account): %v", err)
		return
	}

	existing, err := s.ac.GetTransactions(account.ID)
	if err != nil {
		log.Printf("Actual error (transactions): %v", err)
		return
	}

	alreadyMatched := make([]*actual.Transaction, len(existing))
	copy(alreadyMatched, existing)

	var newlyTouched []*actual.Transaction
	added, updated, skipped := 0, 0, 0

	_, importSpan := tracer.Start(ctx, "import.transactions_batch",
		trace.WithAttributes(attribute.Int("txn_count", len(rawTxns))),
	)
	for _, txn := range rawTxns {
		status := txn.Status
		if status == "" {
			status = "BOOK"
		}
		date := txn.Date
		amountCents := txn.AmountCents
		payee := txn.Payee
		notes := txn.Notes
		ref := txn.EntryRef
		var pendingKey string
		if ref != "" {
			pendingKey = ref
		} else {
			pendingKey = fmt.Sprintf("%s|%s", date.Format("2006-01-02"), centsToDecimal(amountCents))
		}

		log.Printf("Txn: %s | %s | %s | %s", status, date.Format("2006-01-02"), centsToDecimal(amountCents), payee)

		if status == "PDNG" {
			if _, exists := s.state.PendingMap[pendingKey]; !exists {
				t, wasCreated, err := s.ac.ReconcileTransaction(
					date, account, payee, notes, amountCents, false, ref, payee, alreadyMatched,
				)
				if err != nil {
					log.Printf("reconcile_transaction failed (%v), falling back to create_transaction", err)
					t, err = s.ac.CreateTransaction(date, account, payee, notes, amountCents, false, ref, payee)
					if err != nil {
						log.Printf("Skipping transaction: %v | %+v", err, txn)
						continue
					}
					wasCreated = true
				}
				alreadyMatched = append(alreadyMatched, t)
				if wasCreated {
					if err := s.state.SetPending(pendingKey, t.ID, date.Format("2006-01-02"), s.st); err != nil {
						log.Printf("SetPending: %v", err)
					}
					added++
					log.Printf("Imported pending: %s | %s | %s",
						date.Format("2006-01-02"), centsToDecimal(amountCents), payee)
				} else {
					skipped++
				}
			} else {
				skipped++
			}

		} else {

			if ref != "" {
				if _, done := s.state.ImportedRefs[ref]; done {
					skipped++
					log.Printf("Skipped already-imported: %s | %s | %s",
						date.Format("2006-01-02"), centsToDecimal(amountCents), payee)
					continue
				}
			}

			if pendingVal, inPending := s.state.PendingMap[pendingKey]; inPending {
				txnID, _ := splitPendingVal(pendingVal)
				var existingTxn *actual.Transaction
				for _, t := range alreadyMatched {
					if t.ID == txnID {
						existingTxn = t
						break
					}
				}

				if existingTxn != nil {
					if err := s.ac.UpdateTransactionCleared(existingTxn); err != nil {
						log.Printf("Failed to confirm pending: %v", err)
						continue
					}
					newlyTouched = append(newlyTouched, existingTxn)
					if err := s.state.DeletePending(pendingKey, s.st); err != nil {
						log.Printf("DeletePending: %v", err)
					}
					if ref != "" {
						if err := s.state.AddImportedRef(ref, date.Format("2006-01-02"), s.st); err != nil {
							log.Printf("AddImportedRef: %v", err)
						}
					}
					updated++
					log.Printf("Confirmed pending: %s | %s | %s",
						date.Format("2006-01-02"), centsToDecimal(amountCents), payee)
				} else {
					if err := s.state.DeletePending(pendingKey, s.st); err != nil {
						log.Printf("DeletePending: %v", err)
					}
					t, wasCreated, err := s.ac.ReconcileTransaction(
						date, account, payee, notes, amountCents, true, ref, payee, alreadyMatched,
					)
					if err != nil {
						log.Printf("Skipping transaction: %v", err)
						continue
					}
					alreadyMatched = append(alreadyMatched, t)
					newlyTouched = append(newlyTouched, t)
					if ref != "" {
						if err := s.state.AddImportedRef(ref, date.Format("2006-01-02"), s.st); err != nil {
							log.Printf("AddImportedRef: %v", err)
						}
					}
					if wasCreated {
						added++
					}
				}

			} else {
				t, wasCreated, err := s.ac.ReconcileTransaction(
					date, account, payee, notes, amountCents, true, ref, payee, alreadyMatched,
				)
				if err != nil {
					log.Printf("Skipping transaction: %v", err)
					continue
				}
				alreadyMatched = append(alreadyMatched, t)
				if wasCreated {
					newlyTouched = append(newlyTouched, t)
					if ref != "" {
						if err := s.state.AddImportedRef(ref, date.Format("2006-01-02"), s.st); err != nil {
							log.Printf("AddImportedRef: %v", err)
						}
					}
					added++
				} else {
					skipped++
				}
			}
		}
	}

	importSpan.SetAttributes(
		attribute.Int("added", added),
		attribute.Int("updated", updated),
		attribute.Int("skipped", skipped),
	)
	importSpan.End()

	_, rulesSpan := tracer.Start(ctx, "rules.apply")
	rules, err := s.ac.LoadRules()
	if err != nil {
		log.Printf("Failed to load rules: %v", err)
		rulesSpan.RecordError(err)
	} else {
		var rulesApplied int64
		for _, t := range newlyTouched {
			matched := rules.Match(t)
			if len(matched) > 0 {
				log.Printf("Transaction for recipient %s matched %d rule(s)", t.PayeeName, len(matched))
				for _, r := range matched {
					r.Apply(s.ac.DB(), t)
					rulesApplied++
				}
			}
		}
		if s.met != nil && rulesApplied > 0 {
			s.met.rulesApplied.Add(ctx, rulesApplied)
		}
		rulesSpan.SetAttributes(attribute.Int64("rules_applied", rulesApplied))
	}
	rulesSpan.End()

	_, commitSpan := tracer.Start(ctx, "actual.commit")
	if err := s.ac.Commit(); err != nil {
		log.Printf("Actual commit error: %v", err)
		commitSpan.RecordError(err)
		commitSpan.SetStatus(codes.Error, "commit failed")
		if s.met != nil {
			s.met.commitErrors.Add(ctx, 1)
		}
	}
	commitSpan.End()

	if s.met != nil {
		s.met.txAdded.Add(ctx, int64(added))
		s.met.txConfirmed.Add(ctx, int64(updated))
		s.met.txSkipped.Add(ctx, int64(skipped))
	}
	log.Printf("Done: %d added, %d confirmed, %d skipped", added, updated, skipped)

	if err := s.state.SetLastSyncDate(time.Now().UTC().Format("2006-01-02"), s.st); err != nil {
		log.Printf("Failed to save state: %v", err)
	}
}

// mustEnv returns the value of the environment variable key, or terminates the
// process if it is unset.
func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("Required environment variable %s is not set", key)
	}
	return v
}

// envOr returns the value of key, or def if the variable is unset or empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt returns the integer value of key, or def if the variable is unset or
// cannot be parsed.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// centsToDecimal formats an integer cent amount as a signed decimal string,
// e.g. -1234 → "-12.34".
func centsToDecimal(cents int64) string {
	if cents < 0 {
		return fmt.Sprintf("-%d.%02d", (-cents)/100, (-cents)%100)
	}
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

// ownNames parses the ACCOUNT_HOLDER_NAME environment variable (comma-separated)
// into a lowercase set used to detect self-transfer payees.
func ownNames() map[string]struct{} {
	names := make(map[string]struct{})
	for _, part := range strings.Split(os.Getenv("ACCOUNT_HOLDER_NAME"), ",") {
		if n := strings.TrimSpace(part); n != "" {
			names[strings.ToLower(n)] = struct{}{}
		}
	}
	return names
}
