package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"bankingsync/actual"
	"bankingsync/budget"
	"bankingsync/enablebanking"
	"bankingsync/firefly"
	"bankingsync/logs"
	"bankingsync/store"
	"bankingsync/web"
)

// Version is set at build time via -ldflags "-X main.Version=...".
var Version = "dev"

var olog = logs.Get("bankingsync")

const bannerArt = `
 __________                                 _________      .__
 \______   \ ____   _____ _____    ____    /   _____/_____ |__| ____   ______
  |       _//  _ \ /     \\__  \  /    \   \_____  \\____ \|  |/ __ \ /  ___/
  |    |   (  <_> )  Y Y  \/ __ \|   |  \  /        \  |_> >  \  ___/ \___ \
  |____|_  /\____/|__|_|  (____  /___|  / /_______  /   __/|__|\___  >____  >
         \/             \/     \/     \/          \/|__|           \/     \/
  Roman Spies - Licensed under the GNU AFFERO GENERAL PUBLIC LICENSE, v3.
  https://github.com/RomanSpies/BankingSync
`

func main() {
	// One subcommand, and it does not start anything: an installation reporting a
	// matching problem runs this to produce a file it can attach to the issue.
	if len(os.Args) > 1 && os.Args[1] == "export-linkage-stats" {
		st, err := store.Open(store.DBPath)
		if err != nil {
			log.Fatalf("open store: %v", err)
		}
		defer func() { _ = st.Close() }()

		t := st.Tunables()
		pol := budget.Policy{
			PayeePrefixes: t.PayeePrefixes, TolerancePercent: t.TolerancePercent,
			ToleranceCents:    t.ToleranceCents,
			AutoProbability:   float64(t.AutoProbabilityPct) / 100,
			ReviewProbability: float64(t.ReviewProbabilityPct) / 100,
		}
		backend, _ := st.GetSetting(backendSettingKey)
		if err := exportLinkageStats(st, backend, pol.Version()); err != nil {
			log.Fatalf("export: %v", err)
		}
		return
	}

	fmt.Print(bannerArt)
	log.Printf("Version %s", Version)
	web.AppVersion = Version
	syncHours := envInt("SYNC_INTERVAL_HOURS", 6)
	log.Printf("Starting scheduler (every %dh)", syncHours)

	s, err := newSyncer()
	if err != nil {
		log.Fatalf("Startup failed: %v", err)
	}
	defer s.st.Close()

	webSrv, err := web.New(s.st, s.eb, s.run, sendTestEmail, web.TemplateFS)
	if err != nil {
		log.Fatalf("Web server init: %v", err)
	}

	webSrv.SetBackendStatus(s.BackendStatus)
	webSrv.SetOpeningBalanceFunc(s.OpeningBalancePreview)
	webSrv.SetReviewQueue(s)
	webSrv.SetPromotions(s)

	shutdown := initTelemetry(s)
	webSrv.InitTelemetry()
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

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s.ctx = rootCtx

	s.run()
	ticker := time.NewTicker(time.Duration(syncHours) * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-rootCtx.Done():
			log.Println("Shutdown signal received — stopping scheduler")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = webSrv.Shutdown(shutdownCtx)
			cancel()
			return
		case <-ticker.C:
			s.run()
		}
	}
}

// Syncer orchestrates a full sync cycle: fetching transactions from Enable
// Banking and importing them into the configured budget backend.
type Syncer struct {
	state *State
	st    *store.Store
	ac    budget.Store
	eb    *enablebanking.Client
	met   *syncMetrics

	ctx context.Context

	mu      sync.Mutex
	running bool

	pendingGauge    atomic.Int64
	expiryDaysGauge atomic.Int64
	expiryDaysKnown atomic.Bool

	// The last verdict the promotion gate reached, kept so that a metric can
	// report it without provoking one. Evaluating the gate refits the linkage
	// twice and fits Platt twice, which is fine on a page load and would not be
	// fine every thirty seconds from a metric callback. So the figures are
	// recorded when the gate genuinely runs and the gauge reads them back; a
	// series that stops moving means nobody has looked at the page, which is
	// itself worth being able to see.
	gateMu   sync.RWMutex
	gateSeen bool
	gate     gateSnapshot

	backendName string

	backendMu        sync.Mutex
	backendVersion   string
	backendReachable bool
	backendCheckedAt time.Time

	checkUpdate func(context.Context, *store.Store)

	connectBackend func(context.Context) (budget.Store, error)
}

// gateSnapshot is what the promotion gate concluded the last time it was asked.
type gateSnapshot struct {
	labelled   int
	holdout    int
	skill      float64
	pValue     float64
	level      float64
	statistic  float64
	baseBrier  float64
	trialBrier float64
	levelsHeld int
	plattFit   string
	tested     bool
	promotable bool
	checks     map[string]string
}

const (
	syncTimeout        = 30 * time.Minute
	updateCheckTimeout = 2 * time.Minute
)

// reviewEmail is the notification raised once at the end of a run that held
// something back.
//
// One email per run, not one per transaction: a bank that changes its payee
// format holds a dozen at once, and a dozen emails get filtered — which would
// silence exactly the notification that matters most, since a held transaction
// is in no budget and no later run will offer it again.
func reviewEmail(held int, webAddr string) (subject, body string) {
	return fmt.Sprintf("BankingSync: %d transaction(s) need a decision", held),
		fmt.Sprintf("BankingSync could not tell whether %d transaction(s) are new or are "+
			"settlements of something already in your budget.\n\n"+
			"They have NOT been imported, and will not be until you decide.\n\n"+
			"Review them at: %s/review\n", held, webAddr)
}

// bookkeepingFailed reports a change that reached the budget while the note that
// stops it happening again did not.
//
// These failures were stderr-only, which is the wrong place for them: the budget
// has already been written, so the program carries on as if nothing happened,
// and the damage only appears on the next run. Usually nothing comes of it —
// the transaction carries a bank reference and the external-reference fast path
// recognises it. A pending row with no reference has no such path, and is then
// imported a second time with nothing anywhere having said why.
//
// Error severity rather than warning for exactly that reason: it is not a
// retryable hiccup, it is a durable inconsistency between the budget and the
// record of what has been imported.
func bookkeepingFailed(ctx context.Context, op, bank, ref string, err error) {
	log.Printf("[%s] %s failed: %v — the budget was written but the record of it was not",
		bank, op, err)
	olog.Error(ctx, "state.bookkeeping_failed",
		logs.String("op", op),
		logs.String("bank", bank),
		logs.String("external_ref", ref),
		logs.Bool("recoverable", ref != ""),
		logs.String("error", err.Error()))
}

// importKeys assigns every incoming transaction the identity it is recognised by
// across runs, in the order the slice is already sorted.
//
// A bank reference is used verbatim when there is one. When there is not — the
// ordinary case for pending rows at many institutions, and the reason this
// fallback exists at all — the key is built from the fields that tell one
// purchase from another, plus an occurrence index for the case where they
// genuinely do not tell them apart.
//
// The payee belongs in the key. Without it two different shops charging the same
// amount on the same day share an identity, and the second one is discarded as a
// repeat of the first. The occurrence index covers the remaining case: two
// identical purchases on the same day are two transactions, not one.
//
// The index is counted per status, so that a pending row and the booking that
// settles it arrive at the same key and the pending map still recognises the
// pair. It is stable across runs as long as the bank returns a day's rows in a
// stable order, which is the one assumption this scheme rests on — and a broken
// assumption produces a visible duplicate rather than a silent loss.
func importKeys(txns []enablebanking.Transaction, prefixes []string) (keys []string, collisions int) {
	seen := make(map[string]int, len(txns))
	legacy := make(map[string]int, len(txns))
	keys = make([]string, len(txns))

	for i, t := range txns {
		if t.EntryRef != "" {
			keys[i] = t.EntryRef
			continue
		}
		status := t.Status
		if status == "" {
			status = "BOOK"
		}
		// What the identity used to be, counted but never used. It says how often
		// this feed would have lost a row under the old scheme, which is the only
		// way to know whether the defect was ever reached in a given installation.
		if l := legacyImportKey(t.Date, t.AmountCents); l != "" {
			legacy[status+"|"+l]++
			if legacy[status+"|"+l] > 1 {
				collisions++
			}
		}

		base := fmt.Sprintf("%s|%s|%s", t.Date.Format("2006-01-02"),
			centsToDecimal(t.AmountCents), keyPayee(t.Payee, prefixes))
		// Counted per status but not keyed by it. Counting them together would
		// hand a booking the index after its own authorisation; keying by status
		// would stop the two ever meeting in the pending map, which is what lets
		// a same-day settlement be confirmed without troubling the matcher.
		seen[status+"|"+base]++
		keys[i] = fmt.Sprintf("%s|%d", base, seen[status+"|"+base])
	}
	return keys, collisions
}

// legacyImportKey is the identity a reference-less transaction was recorded
// under before v3: the date and the amount, and nothing else.
//
// It is read, never written. A pending row imported by an older version carries
// this key, and its booking now computes a different one — without the fallback
// the authorisation would go unrecognised and be imported a second time. The
// scheme disappears on its own once the retention window has passed over it.
func legacyImportKey(date time.Time, amountCents int64) string {
	return fmt.Sprintf("%s|%s", date.Format("2006-01-02"), centsToDecimal(amountCents))
}

// keyPayee reduces a payee to the form the key uses.
//
// Normalised with the card scheme prefixes discounted, so that a bank which
// prepends "VISA" only once the payment settles still produces the same key for
// both halves of it — that is what lets the pending map confirm a booking
// without going through the matcher at all. The separator is stripped because
// the key is read back by splitting on it.
func keyPayee(payee string, prefixes []string) string {
	return strings.ReplaceAll(strings.ToLower(budget.NormalisePayee(payee, prefixes)), "|", " ")
}

// matchOutcome names what became of an incoming transaction, for the label on
// the probability histogram.
func matchOutcome(created bool) string {
	if created {
		return "created"
	}
	return "adopted"
}

// traceDecision writes one decision to a span of its own.
//
// A record rather than a timing: it is emitted once the decision has been made,
// so its duration is nothing and the span is there to be searched by attribute.
// The question a trace is opened for here is "why was this one held", and that is
// answered by the weight term by term against the thresholds it was judged
// against — not by how long any of it took, which is what the batch span is for.
//
// Every field is a span attribute rather than an event attribute because Tempo
// can select on the first and cannot select on the second, and selecting is the
// whole use: outcome="held" with a probability above the automatic threshold is
// the query that finds every case the margin rule caught.
func (s *Syncer) traceDecision(ctx context.Context, tracer trace.Tracer, label string, d budget.DecisionTrace) {
	_, span := tracer.Start(ctx, "match.decide")
	defer span.End()

	span.SetAttributes(
		attribute.String("bank", label),
		attribute.Int("index", d.Index),
		attribute.String("outcome", d.Outcome),
		attribute.String("reason", d.Reason),
		// The three counts differ, and where they differ is often the answer: a
		// fortnight of forty rows that leaves two adoptable is a different story
		// from one that leaves forty, and the prior is taken over neither.
		attribute.Int("window_rows", d.Window),
		attribute.Int("adoptable", d.Adoptable),
		attribute.Int("plausible", d.Plausible),
		attribute.Float64("auto_probability", d.AutoProbability),
		attribute.Float64("review_probability", d.ReviewProbability),
		attribute.Float64("margin_bits", d.MarginBits),
	)
	if d.ChosenID != "" {
		span.SetAttributes(attribute.String("chosen_id", d.ChosenID))
	}
	if d.ShadowOutcome != "" {
		span.SetAttributes(attribute.String("shadow_outcome", d.ShadowOutcome))
	}
	// An infinite margin means nothing was paired, which is not a measurement of
	// how close the call was.
	if !math.IsInf(d.Margin, 1) {
		span.SetAttributes(attribute.Float64("margin", d.Margin))
	}
	if d.Interchangeable > 1 {
		span.SetAttributes(attribute.Int("interchangeable", d.Interchangeable))
	}

	if d.Best == nil {
		span.SetAttributes(attribute.Bool("empty_window", true))
		return
	}
	span.SetAttributes(
		attribute.Float64("weight", d.Best.Weight),
		attribute.Float64("probability", d.Best.Probability),
		attribute.String("payee_level", d.Best.Comparison.Payee.String()),
		attribute.String("amount_level", d.Best.Comparison.Amount.String()),
		attribute.String("date_level", d.Best.Comparison.Date.String()),
	)
	if f := d.Best.Comparison.PayeeFrequency; f > 0 {
		span.SetAttributes(attribute.Float64("payee_frequency", f))
	}
	// The weight term by term, which is the part that explains rather than
	// states. The four sum to weight, and the term that dominates is the field
	// that decided.
	for _, e := range d.Evidence {
		span.SetAttributes(
			attribute.Float64("bits."+e.Field, e.Bits),
			attribute.String("level."+e.Field, e.Level))
	}
	if d.RunnerUp != nil {
		span.SetAttributes(
			attribute.Float64("runner_up_weight", d.RunnerUp.Weight),
			attribute.Float64("runner_up_probability", d.RunnerUp.Probability),
			attribute.Float64("window_gap", d.Best.Weight-d.RunnerUp.Weight))
	}
}

// recordMatch records the figure a matching decision was made on.
//
// Every incoming transaction contributes one observation, labelled by what
// happened to it. That is what turns the two thresholds from numbers a person
// has to guess at into a cut through a distribution they can look at: if nothing
// sits between them, the review band is doing nothing; if half the traffic does,
// it is set too wide for that bank.
func (s *Syncer) recordMatch(ctx context.Context, label, outcome, paramVersion string, out budget.Outcome) {
	if s.met == nil || out.Best == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("outcome", outcome),
		attribute.String("bank", label),
		attribute.String("backend", s.backendName),
	)
	// The probability carries the parameters that produced it, which the other
	// series deliberately do not. A distribution is only comparable with itself
	// while the thresholds it is cut by have not moved, and a promotion moves
	// them: without this label a change of parameters looks like a change in the
	// banks. It does mean a promotion starts a new series — that is the point,
	// and it is in the release notes.
	probAttrs := metric.WithAttributes(
		attribute.String("outcome", outcome),
		attribute.String("bank", label),
		attribute.String("backend", s.backendName),
		attribute.String("param_version", paramVersion),
	)
	s.met.matchProb.Record(ctx, out.Best.Probability, probAttrs)

	// An infinite margin means nothing was paired, which is not a measurement of
	// how close the call was.
	if !math.IsInf(out.Margin, 1) {
		s.met.matchMargin.Record(ctx, out.Margin, attrs)
	}
	if out.Interchangeable > 1 {
		s.met.matchMultiplicity.Add(ctx, 1, metric.WithAttributes(
			attribute.String("bank", label),
			attribute.String("backend", s.backendName)))
	}
}

// webAddr is the address the user reaches this instance on, for links in email.
func (s *Syncer) webAddr() string {
	if v, _ := s.st.GetSetting("eb_base_url"); v != "" {
		return v
	}
	return "https://localhost:8443"
}

func (s *Syncer) baseContext() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

func (s *Syncer) tryAcquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return false
	}
	s.running = true
	return true
}

func (s *Syncer) release() {
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()
}

func (s *Syncer) publishStateGauges() {
	if s.state == nil {
		return
	}
	s.pendingGauge.Store(int64(s.state.TotalPending()))
	if _, _, expiry, err := s.state.GetSession(); err == nil {
		s.expiryDaysGauge.Store(int64(time.Until(expiry).Hours() / 24))
		s.expiryDaysKnown.Store(true)
	} else {
		s.expiryDaysKnown.Store(false)
	}
}

// newSyncer opens the store, loads persisted state, and initialises the Enable Banking client.
const (
	backendActual  = "actual"
	backendFirefly = "firefly"

	backendSettingKey = "budget_backend"
)

var supportedBackends = map[string]bool{
	backendActual:  true,
	backendFirefly: true,
}

func selectedBackend() (string, error) {
	name := strings.ToLower(strings.TrimSpace(envOr("BUDGET_BACKEND", backendActual)))
	if !supportedBackends[name] {
		return "", fmt.Errorf("BUDGET_BACKEND=%q is not a known backend (want %q or %q)",
			name, backendActual, backendFirefly)
	}
	return name, nil
}

func resolveBackend(st *store.Store) (string, error) {
	want, err := selectedBackend()
	if err != nil {
		return "", err
	}

	previous, err := st.GetSetting(backendSettingKey)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", backendSettingKey, err)
	}

	if previous == want {
		return want, nil
	}

	if previous != "" {
		if !strings.EqualFold(strings.TrimSpace(os.Getenv("BUDGET_BACKEND_MIGRATE")), "true") {
			return "", fmt.Errorf(
				"backend changed from %q to %q, but the deduplication state in imported_refs and pending_map "+
					"still belongs to %q. Continuing would silently skip every transaction already imported "+
					"there. Set BUDGET_BACKEND_MIGRATE=true to discard that state and re-import into %q",
				previous, want, previous, want)
		}
		refs, pending, err := st.ResetImportState()
		if err != nil {
			return "", fmt.Errorf("purge dedupe state: %w", err)
		}
		log.Printf("Backend changed %s -> %s: discarded %d imported refs and %d pending entries",
			previous, want, refs, pending)
	}

	if err := st.SetSetting(backendSettingKey, want); err != nil {
		return "", fmt.Errorf("persist %s: %w", backendSettingKey, err)
	}
	return want, nil
}

func newSyncer() (*Syncer, error) {
	st, err := store.Open(store.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	backendName, err := resolveBackend(st)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	log.Printf("Budget backend: %s (BUDGET_BACKEND=%q)", backendName, os.Getenv("BUDGET_BACKEND"))

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

	return &Syncer{
		state:          state,
		st:             st,
		backendName:    backendName,
		eb:             eb,
		checkUpdate:    checkForUpdate,
		connectBackend: dialBackend,
	}, nil
}

// BackendStatus reports what the last connection attempt learned, for /health.
func (s *Syncer) BackendStatus() web.BackendStatus {
	s.backendMu.Lock()
	defer s.backendMu.Unlock()
	return web.BackendStatus{
		Name:      s.backendName,
		Version:   s.backendVersion,
		Reachable: s.backendReachable,
		CheckedAt: s.backendCheckedAt,
	}
}

func (s *Syncer) recordBackendHealth(ctx context.Context, reachable bool) {
	version := ""
	if reachable {
		if d, ok := s.ac.(budget.Describer); ok {
			if v, err := d.BackendVersion(ctx); err == nil {
				version = v
			}
		}
	}
	s.backendMu.Lock()
	s.backendReachable = reachable
	s.backendCheckedAt = time.Now().UTC()
	if version != "" {
		s.backendVersion = version
	}
	s.backendMu.Unlock()
}

func (s *Syncer) ensureActual(ctx context.Context) error {
	if s.ac != nil {
		if err := s.ac.Ping(ctx); err != nil {
			log.Printf("Backend unreachable (%v) — reconnecting", err)
			s.recordBackendHealth(ctx, false)
			s.ac.Close()
			s.ac = nil
			return s.connect(ctx)
		}
		s.recordBackendHealth(ctx, true)
		return nil
	}
	return s.connect(ctx)
}

func (s *Syncer) connect(ctx context.Context) error {
	if s.connectBackend == nil {
		return fmt.Errorf("no budget backend connector configured")
	}
	ac, err := s.connectBackend(ctx)
	if err != nil {
		return err
	}
	s.ac = ac
	s.recordBackendHealth(ctx, true)
	return nil
}

// fallbackAccountName names an account row that carries none, which only happens
// for rows written before the picker derived a name. It stays backend-aware and
// bank-specific: falling back to one shared constant would import every bank into
// the same budget account.
func (s *Syncer) fallbackAccountName(acct store.BankAccount) string {
	env := "ACTUAL_ACCOUNT"
	if s.backendName == backendFirefly {
		env = "FIREFLY_ACCOUNT"
	}
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v
	}
	sa := enablebanking.SessionAccount{IBAN: acct.IBAN, Currency: acct.Currency}
	if name := sa.SuggestedAccountName(acct.BankName); name != "" {
		return name
	}
	return "bankingsync " + acct.AccountUID
}

func dialBackend(ctx context.Context) (budget.Store, error) {
	name, err := selectedBackend()
	if err != nil {
		return nil, err
	}
	switch name {
	case backendActual:
		return dialActual(ctx)
	case backendFirefly:
		return dialFirefly(ctx)
	default:
		return nil, fmt.Errorf("no connector for backend %q", name)
	}
}

func dialFirefly(_ context.Context) (budget.Store, error) {
	baseURL, err := requireEnv(backendFirefly, "FIREFLY_URL")
	if err != nil {
		return nil, err
	}
	token, err := requireEnv(backendFirefly, "FIREFLY_TOKEN")
	if err != nil {
		return nil, err
	}

	var opts []firefly.Option
	if strings.EqualFold(os.Getenv("FIREFLY_INSECURE_TLS"), "true") {
		opts = append(opts, firefly.WithHTTPClient(&http.Client{
			Timeout:   60 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		}))
	}

	return firefly.NewStore(firefly.New(baseURL, token, opts...), firefly.Config{
		PendingTag:   envOr("FIREFLY_PENDING_TAG", "pending"),
		ApplyRules:   !strings.EqualFold(os.Getenv("FIREFLY_APPLY_RULES"), "false"),
		FireWebhooks: strings.EqualFold(os.Getenv("FIREFLY_FIRE_WEBHOOKS"), "true"),
	}), nil
}

// requireEnv names the backend in its error. A missing ACTUAL_URL means nothing
// on its own: the reader cannot tell whether Actual was chosen deliberately or
// fallen back into, which is exactly the case where the variable is missing.
func requireEnv(backend, key string) (string, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return "", fmt.Errorf("%s is required for BUDGET_BACKEND=%s but is not set", key, backend)
	}
	return v, nil
}

func dialActual(ctx context.Context) (budget.Store, error) {
	url, err := requireEnv(backendActual, "ACTUAL_URL")
	if err != nil {
		return nil, err
	}
	password, err := requireEnv(backendActual, "ACTUAL_PASSWORD")
	if err != nil {
		return nil, err
	}
	syncID, err := requireEnv(backendActual, "ACTUAL_SYNC_ID")
	if err != nil {
		return nil, err
	}

	insecureTLS := strings.EqualFold(os.Getenv("ACTUAL_INSECURE_TLS"), "true")
	c, err := actual.NewClient(ctx, url, password, syncID, "/data/actual-cache", insecureTLS)
	if err != nil {
		return nil, err
	}
	return actual.NewAdapter(c), nil
}

func (s *Syncer) run() bool {
	if !s.tryAcquire() {
		log.Printf("Sync already running — skipping this trigger")
		return false
	}
	defer s.release()
	defer s.publishStateGauges()

	// Reload session from store in case a new account was connected via the web UI.
	if err := s.state.Reload(s.st); err != nil {
		log.Printf("State reload: %v", err)
	}

	ctx, cancelRun := context.WithTimeout(s.baseContext(), syncTimeout)
	defer cancelRun()
	tracer := otel.Tracer("bankingsync")
	ctx, span := tracer.Start(ctx, "sync.run")
	defer span.End()

	start := time.Now()
	status := "success"
	syncMessage := ""
	added, updated, skipped := 0, 0, 0
	// Two different things, deliberately not one counter. A dropped transaction is
	// one Enable Banking sent that could not be parsed, which is a defect worth an
	// email. A zero-amount row is one bankingsync declines on purpose, and at banks
	// that issue them — Revolut does, routinely — folding the two together turns
	// every ordinary sync into a degraded one with an alert claiming a parse
	// failure that never happened.
	totalDropped := 0
	zeroAmount := 0
	held := 0
	var syncErrors []string
	defer func() {
		elapsed := time.Since(start).Seconds()
		if s.met != nil {
			s.met.syncRuns.Add(ctx, 1, metric.WithAttributes(
				attribute.String("status", status),
				attribute.String("backend", s.backendName)))
			s.met.syncDuration.Record(ctx, elapsed, metric.WithAttributes(attribute.String("backend", s.backendName)))
		}
		// Only parse failures degrade the run. Zero-amount rows are counted and
		// logged below, and that is all: they are the expected answer from some
		// banks, not a fault to be alerted on.
		if totalDropped > 0 {
			if status == "success" {
				status = "degraded"
			}
			syncErrors = append(syncErrors, fmt.Sprintf(
				"%d transaction(s) returned by Enable Banking could not be parsed and were dropped — check logs for the field names",
				totalDropped))
		}
		if len(syncErrors) > 0 {
			syncMessage = strings.Join(syncErrors, "; ")
			var body strings.Builder
			body.WriteString(fmt.Sprintf("BankingSync encountered %d error(s) during sync.\n\n", len(syncErrors)))
			for _, e := range syncErrors {
				body.WriteString("- " + e + "\n")
			}
			sendEmail(ctx, "BankingSync: sync errors", body.String())
		}
		if _, err := s.st.AddSyncLog(status, added, updated, skipped, elapsed, syncMessage); err != nil {
			log.Printf("Failed to save sync log: %v", err)
		}
		log.Printf("Sync finished in %.1fs (status=%s)", elapsed, status)
		olog.Info(ctx, "sync.finished",
			logs.String("status", status),
			logs.Float64("duration_sec", elapsed),
			logs.Int("added", added),
			logs.Int("confirmed", updated),
			logs.Int("skipped", skipped),
			logs.Int("dropped", totalDropped),
			logs.Int("zero_amount", zeroAmount),
			logs.Int("held_for_review", held),
			logs.Int("errors", len(syncErrors)),
		)
		span.SetAttributes(
			attribute.Int("tx_dropped", totalDropped),
			attribute.Int("tx_zero_amount", zeroAmount),
			attribute.Int("tx_held", held),
		)
		if zeroAmount > 0 {
			log.Printf("Skipped %d zero-amount transaction(s) — expected from some banks, not an error", zeroAmount)
		}
		if held > 0 {
			log.Printf("Held %d transaction(s) for review — they are NOT in the budget until decided", held)
			// One email per run rather than one per transaction. A held
			// transaction is in no budget and in no bank feed the next run will
			// re-offer, so the only thing standing between it and being forgotten
			// is somebody being told — but a bank that changes its payee format
			// can hold a dozen at once, and a dozen emails get filtered.
			subject, body := reviewEmail(held, s.webAddr())
			sendEmail(ctx, subject, body)
		}
	}()

	// One identity per run, so a reading of the decision log can tell one pass
	// over an account from the next.
	runID := uuid.NewString()

	log.Println("Starting sync...")
	olog.Info(ctx, "sync.started", logs.String("run_id", runID))

	bankAccounts, err := s.st.GetAllBankAccounts()
	if err != nil || len(bankAccounts) == 0 {
		log.Printf("No bank accounts configured — connect via web UI")
		status = "no_session"
		span.SetAttributes(attribute.Int("account_count", 0))
		span.SetStatus(codes.Error, "no bank accounts")
		return true
	}
	span.SetAttributes(attribute.Int("account_count", len(bankAccounts)))

	webAddr := s.webAddr()

	if err := s.state.PruneImportedRefs(s.st); err != nil {
		log.Printf("Prune imported refs: %v", err)
	}
	if err := s.state.PrunePendingMap(s.st); err != nil {
		log.Printf("Prune pending map: %v", err)
	}
	// Both of these had retention policies written down and no caller. A held
	// transaction past the window describes an import the bank will not offer
	// again, and a decision past it describes one nobody can check any more.
	if err := s.st.PruneMatchReviews(); err != nil {
		log.Printf("Prune match reviews: %v", err)
	}
	if err := s.st.PruneMatchDecisions(); err != nil {
		log.Printf("Prune match decisions: %v", err)
	}
	if err := s.st.PruneMatchInquiries(); err != nil {
		log.Printf("Prune match inquiries: %v", err)
	}
	// The sample describes what the amount tolerance, the payee prefixes and the
	// date window classified. Change one of those and what was counted is no
	// longer what would be counted now, so it goes rather than being merged.
	// Everything else may move without touching it.
	if err := s.st.PruneLevelObservations(s.matchPolicy("").ClassificationVersion()); err != nil {
		log.Printf("Prune level observations: %v", err)
	}

	connCtx, connSpan := tracer.Start(ctx, "budget.ensure_connection")
	connErr := s.ensureActual(ctx)
	if connErr != nil {
		connSpan.RecordError(connErr)
		connSpan.SetStatus(codes.Error, "connection failed")
		connSpan.End()
		log.Printf("Budget backend error: %v", connErr)
		olog.Error(connCtx, "budget.ensure_connection.failed", logs.String("error", connErr.Error()))
		span.RecordError(connErr)
		span.SetStatus(codes.Error, "connection failed")
		status = "error"
		syncErrors = append(syncErrors, fmt.Sprintf("budget backend connection: %v", connErr))
		return true
	}
	connSpan.End()

	var newlyTouched []*budget.Transaction
	var backfilled []int64
	var synced []int64
	fetchFailed := 0

	// Phase 6: the one question a run is allowed to ask about a decision it made
	// on its own. Set up before the loop because the choice is made across all
	// accounts, and skipped outright when it is off or when the last one has not
	// been answered — an unanswered question is a reason to stop asking, not to
	// ask again.
	sampler := s.newInquirySampler()

	for _, acct := range bankAccounts {
		label := acct.BankName
		if label == "" {
			label = acct.AccountUID
		}

		if t, err := time.Parse(time.RFC3339, acct.SessionExpiry); err == nil {
			daysLeft := int(time.Until(t).Hours() / 24)
			switch {
			case daysLeft < 0:
				log.Printf("WARNING: session for %s expired %d day(s) ago. Renew via %s", label, -daysLeft, webAddr)
				if s.shouldWarnExpiry(acct.ID, "expired") {
					sendEmail(ctx,
						fmt.Sprintf("BankingSync: %s session has expired", label),
						fmt.Sprintf("Your Enable Banking session for %s expired %d day(s) ago and syncing has stopped.\n\nRenew it at: %s\n", label, -daysLeft, webAddr),
					)
				}
			case daysLeft < 7:
				log.Printf("WARNING: session for %s expires in %d days. Renew via %s", label, daysLeft, webAddr)
				if s.shouldWarnExpiry(acct.ID, fmt.Sprintf("d%d", daysLeft)) {
					sendEmail(ctx,
						fmt.Sprintf("BankingSync: %s session expires in %d days", label, daysLeft),
						fmt.Sprintf("Your Enable Banking session for %s expires in %d days.\n\nRenew it at: %s\n", label, daysLeft, webAddr),
					)
				}
			}
		}

		var dateFrom time.Time
		if acct.StartSyncDate != "" {
			if d, err := time.Parse("2006-01-02", acct.StartSyncDate); err == nil {
				dateFrom = d
			}
		}
		if dateFrom.IsZero() && acct.LastSyncDate != "" {
			if d, err := time.Parse("2006-01-02", acct.LastSyncDate); err == nil {
				dateFrom = d
			}
		}
		if dateFrom.IsZero() && s.state.LastSyncDate != "" {
			if d, err := time.Parse("2006-01-02", s.state.LastSyncDate); err == nil {
				dateFrom = d
			}
		}
		if dateFrom.IsZero() {
			dateFrom = time.Now().UTC().AddDate(0, 0, -30)
		}

		if earliest, ok := s.state.EarliestPendingDate(acct.ID); ok && earliest.Before(dateFrom) {
			dateFrom = earliest
		}

		// Balances are read before the transactions and again after the import.
		// Both readings have to agree before an opening balance is derived from
		// them; an account that moved during the run is deferred rather than
		// written wrong, because the value is written once and never revised.
		balancesBefore, balanceErr := s.readBalances(ctx, acct)

		fetchStart := time.Now()
		fetchCtx, fetchSpan := tracer.Start(ctx, "enable_banking.fetch_transactions",
			trace.WithAttributes(
				attribute.String("bank", label),
				attribute.String("date_from", dateFrom.Format("2006-01-02")),
				attribute.String("account_uid", acct.AccountUID),
			),
		)
		rawTxns, droppedTxns, err := s.eb.FetchTransactions(ctx, acct.AccountUID, dateFrom)
		fetchElapsed := time.Since(fetchStart).Seconds()
		totalDropped += droppedTxns
		if s.met != nil {
			s.met.fetchDuration.Record(ctx, fetchElapsed,
				metric.WithAttributes(attribute.String("bank", label)))
			if droppedTxns > 0 {
				s.met.txDropped.Add(ctx, int64(droppedTxns), metric.WithAttributes(attribute.String("bank", label)))
			}
		}
		if err != nil {
			fetchSpan.RecordError(err)
			fetchSpan.SetStatus(codes.Error, err.Error())
			fetchSpan.End()
			log.Printf("Enable Banking error (%s): %v", label, err)
			olog.Error(fetchCtx, "fetch_transactions.failed",
				logs.String("bank", label),
				logs.String("error", err.Error()),
			)
			span.RecordError(err)
			syncErrors = append(syncErrors, fmt.Sprintf("%s: %v", label, err))
			fetchFailed++
			continue
		}
		fetchSpan.SetAttributes(
			attribute.Int("txn_count", len(rawTxns)),
			attribute.Int("txn_dropped", droppedTxns),
			attribute.Float64("duration_sec", fetchElapsed),
		)
		olog.Info(fetchCtx, "fetch_transactions.completed",
			logs.String("bank", label),
			logs.Int("txn_count", len(rawTxns)),
			logs.Int("txn_dropped", droppedTxns),
			logs.Float64("duration_sec", fetchElapsed),
		)
		fetchSpan.End()

		markSynced := func() {
			synced = append(synced, acct.ID)
			if acct.StartSyncDate != "" {
				backfilled = append(backfilled, acct.ID)
			}
		}

		// The account is resolved before the transaction count is examined. An
		// account whose window happens to be empty still has a balance, and
		// skipping it here used to mean a freshly connected account never
		// appeared in the budget at all.
		accountName := acct.ActualAccount
		if accountName == "" {
			accountName = s.fallbackAccountName(acct)
		}
		account, err := s.ac.GetOrCreateAccount(ctx, budget.AccountSpec{
			Name:     accountName,
			Currency: acct.Currency,
			IBAN:     acct.IBAN,
		})
		if err != nil {
			log.Printf("Backend error (account %s): %v", accountName, err)
			syncErrors = append(syncErrors, fmt.Sprintf("%s: account %q: %v", label, accountName, err))
			continue
		}

		if len(rawTxns) == 0 {
			log.Printf("No new transactions for %s", label)
			s.settleBalances(ctx, acct, account.ID, dateFrom, balancesBefore, balanceErr, nil, 0, false, false)
			markSynced()
			continue
		}

		pol := s.matchPolicy(label)
		pol.PayeeFrequency = payeeFrequency(rawTxns, pol.PayeePrefixes)

		lo, hi := candidateWindow(dateFrom, rawTxns)
		existing, err := s.ac.ListTransactions(ctx, account.ID, lo, hi)
		if err != nil {
			log.Printf("Backend error (transactions %s): %v", accountName, err)
			syncErrors = append(syncErrors, fmt.Sprintf("%s: transactions: %v", label, err))
			continue
		}

		knownByID := make(map[string]*budget.Transaction, len(existing))
		knownByRef := make(map[string]*budget.Transaction, len(existing))
		for _, t := range existing {
			knownByID[t.ID] = t
			if t.ExternalRef != "" {
				knownByRef[t.ExternalRef] = t
			}
		}
		remember := func(t *budget.Transaction) {
			knownByID[t.ID] = t
			if t.ExternalRef != "" {
				knownByRef[t.ExternalRef] = t
			}
		}
		matchedThisRun := make([]*budget.Transaction, 0, len(rawTxns))

		// Process oldest first so an interrupted run leaves a resume point that
		// is safe: everything before it is done, everything after it is not.
		//
		// The tie-breaks matter as much as the date. Sorting by date alone left
		// same-day rows in whatever order the bank happened to page them, and the
		// order decides which of two competing rows adopts an open authorisation
		// and which is created beside it. A result that depends on the feed's
		// order is a result nobody can reproduce.
		sort.SliceStable(rawTxns, func(i, j int) bool { return lessTxn(rawTxns[i], rawTxns[j]) })

		// Identities are assigned once, over the sorted batch, so the occurrence
		// index in a reference-less key is a property of the run rather than of
		// the loop's progress through it.
		if width, truncating := measureFieldWidth(rawTxns); truncating {
			log.Printf("[%s] payee names stop at %d characters across several merchants — "+
				"this institution truncates, so a shortened name is expected here", label, width)
			olog.Info(ctx, "bank.field_width_observed",
				logs.String("bank", label),
				logs.Int("width", width))
		}

		txnKeys, keyCollisions := importKeys(rawTxns, pol.PayeePrefixes)
		if keyCollisions > 0 {
			log.Printf("[%s] %d transaction(s) would have shared an identity under the "+
				"pre-v3 import key and been dropped", label, keyCollisions)
			if s.met != nil {
				s.met.importKeyCollisions.Add(ctx, int64(keyCollisions),
					metric.WithAttributes(attribute.String("bank", label)))
			}
		}

		acctAdded, acctUpdated, acctSkipped, acctHeld := 0, 0, 0, 0
		usample := newUSampler()
		acctWriteFailed := false
		acctInterrupted := false
		var resumeFrom time.Time
		importStarted := time.Now()
		failWrite := func(what string, err error) {
			acctWriteFailed = true
			log.Printf("[%s] %s: %v", label, what, err)
			syncErrors = append(syncErrors, fmt.Sprintf("%s: %s: %v", label, what, err))
			if s.met != nil {
				s.met.writeErrors.Add(ctx, 1,
					metric.WithAttributes(attribute.String("backend", s.backendName)))
			}
		}
		// Transactions that need the model are set aside and placed together, so
		// that two bookings competing for one authorisation are decided by which
		// arrangement is better rather than by which the loop reached first.
		var work []modelWork
		flushWork := func() {
			if len(work) == 0 {
				return
			}
			fields := make([]budget.ImportedFields, len(work))
			for i, w := range work {
				fields[i] = w.fields
			}

			// The matching gets a span of its own because it is the one step in
			// an import whose cost is not proportional to the transactions in it.
			// A batch is weighed against every open row in a fortnight and then
			// arranged one-to-one, so the work grows with the account's traffic
			// rather than with the feed's, and an import that has become slow on a
			// busy account looks from the outside exactly like one that is slow
			// for any other reason.
			matchCtx, matchSpan := tracer.Start(ctx, "match.reconcile_batch",
				trace.WithAttributes(
					attribute.String("bank", label),
					attribute.Int("incoming", len(work)),
					attribute.String("param_version", pol.Version()),
					attribute.Bool("shadow", pol.Trial != nil),
				))
			// The hooks are set on a copy used for this call alone. They are
			// reporting rather than policy, and a Policy carrying a closure over
			// one batch's span has no business outliving it.
			traced := pol
			outcomes := map[string]int{}
			traced.OnBatch = func(b budget.BatchTrace) {
				matchSpan.SetAttributes(
					attribute.Int("candidates_weighed", b.Weighed),
					attribute.Int("modelled", b.Incoming),
					attribute.Float64("assess_ms", float64(b.Assessed.Microseconds())/1000),
					attribute.Float64("arrange_ms", float64(b.Arranged.Microseconds())/1000),
					attribute.Float64("shadow_ms", float64(b.Shadowed.Microseconds())/1000),
				)
			}
			traced.OnDecision = func(d budget.DecisionTrace) {
				outcomes[d.Outcome]++
				s.traceDecision(matchCtx, tracer, label, d)
			}

			outs, err := budget.ReconcileBatch(matchCtx, s.ac, account.ID, fields, matchedThisRun, traced)
			if err != nil {
				matchSpan.RecordError(err)
				matchSpan.SetStatus(codes.Error, "reconcile failed")
				matchSpan.End()
				failWrite("reconcile transactions", err)
				work = work[:0]
				return
			}

			matchSpan.SetAttributes(
				attribute.Int("adopted", outcomes["adopted"]),
				attribute.Int("held", outcomes["held"]),
				attribute.Int("created", outcomes["created"]),
			)
			matchSpan.End()

			for i, w := range work {
				out := outs[i]
				if !out.Done() {
					// The batch ran out of time before reaching this one. Stop
					// where the writing stopped, so the resume point names the
					// last transaction that actually landed.
					acctInterrupted = true
					log.Printf("[%s] Run deadline reached while placing transactions — "+
						"resuming from %s on the next sync", label, resumeFrom.Format("2006-01-02"))
					break
				}
				s.recordDecision(ctx, runID, label, acct, w, out, pol)
				s.recordShadow(ctx, label, out, pol)
				sampler.consider(acct, w, out, pol)
				usample.observe(out)

				if len(out.Held) > 0 {
					s.holdForReview(ctx, acct, w.pendingKey, w.fields, out, pol)
					s.recordMatch(ctx, label, "held", pol.Version(), out)
					held++
					acctHeld++
					continue
				}
				t, wasCreated := out.Transaction, out.Created
				s.recordMatch(ctx, label, matchOutcome(wasCreated), pol.Version(), out)
				matchedThisRun = append(matchedThisRun, t)
				remember(t)

				placed, touched := s.settle(ctx, label, acct, w, t, wasCreated)
				if touched {
					newlyTouched = append(newlyTouched, t)
				}
				switch placed {
				case dispositionAdded:
					added++
					acctAdded++
				case dispositionUpdated:
					updated++
					acctUpdated++
				default:
					skipped++
					acctSkipped++
				}
				// Only now is this transaction finished, so only now may the
				// resume point pass it.
				resumeFrom = w.date
			}
			work = work[:0]
		}

		importCtx, importSpan := tracer.Start(ctx, "import.transactions_batch",
			trace.WithAttributes(
				attribute.String("bank", label),
				attribute.String("budget_account", accountName),
				attribute.Int("txn_count", len(rawTxns)),
			),
		)
		for txnIndex, txn := range rawTxns {
			// A long backfill can outlive the run deadline. Stopping here leaves
			// the work done so far durable and records where to resume, instead
			// of being cut mid-transaction.
			if err := ctx.Err(); err != nil {
				acctInterrupted = true
				log.Printf("[%s] Run deadline reached — resuming from %s on the next sync",
					label, resumeFrom.Format("2006-01-02"))
				break
			}

			txnStatus := txn.Status
			if txnStatus == "" {
				txnStatus = "BOOK"
			}
			date := txn.Date
			amountCents := txn.AmountCents
			payee := txn.Payee
			notes := txn.Notes
			ref := txn.EntryRef
			pendingKey := txnKeys[txnIndex]
			// Only reference-less rows ever had a different identity; a bank
			// reference is what it always was.
			legacyKey := ""
			if ref == "" {
				legacyKey = legacyImportKey(date, amountCents)
			}

			// Already waiting for a decision. It must not be offered a second time,
			// and it must not slip in through the ordinary path either — the
			// decision is what releases it.
			if held := s.state.Held(acct.ID); held[pendingKey] || (legacyKey != "" && held[legacyKey]) {
				continue
			}

			if amountCents == 0 {
				log.Printf("[%s] Skipping zero-amount transaction %q: it has no direction, "+
					"and stored as-is it becomes a match magnet for every other zero row", label, ref)
				zeroAmount++
				if s.met != nil {
					s.met.txZeroAmount.Add(ctx, 1,
						metric.WithAttributes(attribute.String("bank", label)))
				}
				continue
			}

			log.Printf("[%s] Txn: %s | %s | %s | %s", label, txnStatus, date.Format("2006-01-02"), centsToDecimal(amountCents), payee)

			pending := budget.ImportedFields{
				Date: date, AmountCents: amountCents, Currency: txn.Currency,
				PayeeName: payee, Notes: notes, ExternalRef: ref, ImportedPayee: payee,
				CounterpartyIBAN: txn.CounterpartyIBAN,
				SEPA: budget.SEPARefs{
					EndToEnd:   txn.SEPA.EndToEnd,
					Mandate:    txn.SEPA.Mandate,
					CreditorID: txn.SEPA.CreditorID,
				},
			}
			booked := pending
			booked.Cleared = true

			if txnStatus == "PDNG" {
				if _, _, exists := s.pendingEntry(acct.ID, pendingKey, legacyKey); !exists {
					work = append(work, modelWork{
						kind: workPending, fields: pending,
						pendingKey: pendingKey, ref: ref, date: date,
					})
					if len(work) >= assignBatchSize {
						flushWork()
					}
					continue
				} else {
					_, val, _ := s.pendingEntry(acct.ID, pendingKey, legacyKey)
					if prev, _ := splitPendingVal(val); prev != "" {
						if t := knownByID[prev]; t != nil {
							matchedThisRun = append(matchedThisRun, t)
						}
					}
					skipped++
					acctSkipped++
				}

			} else {

				if ref != "" {
					if _, done := s.state.Imported(acct.ID)[ref]; done {
						if t := knownByRef[ref]; t != nil {
							matchedThisRun = append(matchedThisRun, t)
						}
						skipped++
						acctSkipped++
						continue
					}
				}

				if matchedKey, pendingVal, inPending := s.pendingEntry(acct.ID, pendingKey, legacyKey); inPending {
					txnID, _ := splitPendingVal(pendingVal)
					existingTxn := knownByID[txnID]

					if existingTxn != nil {
						asItWas := *existingTxn

						if existingTxn.AmountCents != amountCents {
							log.Printf("[%s] Pending booked at a different amount: %s -> %s",
								label, centsToDecimal(existingTxn.AmountCents), centsToDecimal(amountCents))
							if err := s.ac.Update(ctx, existingTxn, budget.AmountPatch(existingTxn, amountCents)); err != nil {
								failWrite("correct booked amount", err)
								continue
							}
						}
						if err := s.ac.Update(ctx, existingTxn, budget.MergePatch(existingTxn, booked, pol.PayeePrefixes)); err != nil {
							log.Printf("Failed to enrich pending, confirming as-is: %v", err)
							if err := s.ac.Update(ctx, existingTxn, budget.Patch{Cleared: budget.Bool(true)}); err != nil {
								failWrite("confirm pending transaction", err)
								continue
							}
						}
						s.recordReferenceLabel(ctx, runID, label, acct, &asItWas, booked,
							pendingKey, ref, pol)

						matchedThisRun = append(matchedThisRun, existingTxn)
						newlyTouched = append(newlyTouched, existingTxn)
						if err := s.state.DeletePending(acct.ID, matchedKey, s.st); err != nil {
							bookkeepingFailed(ctx, "DeletePending", label, ref, err)
						}
						if ref != "" {
							if err := s.state.AddImportedRef(acct.ID, ref, date.Format("2006-01-02"), s.st); err != nil {
								bookkeepingFailed(ctx, "AddImportedRef", label, ref, err)
							}
						}
						updated++
						acctUpdated++
					} else {
						work = append(work, modelWork{
							kind: workStalePending, fields: booked,
							pendingKey: pendingKey, matchedKey: matchedKey,
							ref: ref, date: date,
						})
						if len(work) >= assignBatchSize {
							flushWork()
						}
						continue
					}

				} else {
					work = append(work, modelWork{
						kind: workBooked, fields: booked,
						pendingKey: pendingKey, ref: ref, date: date,
					})
					if len(work) >= assignBatchSize {
						flushWork()
					}
					continue
				}
			}

			// Reached only when the transaction was handled in full, so it is a
			// safe point to resume from if the next iteration is cut short — and
			// only while nothing earlier is still waiting to be placed, or the
			// resume point would step over work that has not happened yet.
			if len(work) == 0 {
				resumeFrom = txn.Date
			}
		}
		// Whatever is left over is placed before anything is concluded about the
		// run: the watermark, the resume point and the run's status all depend on
		// work that has not happened until now.
		flushWork()

		if s.met != nil {
			s.met.writeDuration.Record(ctx, time.Since(importStarted).Seconds(),
				metric.WithAttributes(attribute.String("backend", s.backendName)))
		}

		switch {
		case acctWriteFailed:
			status = "error"
			log.Printf("[%s] write errors occurred — holding the watermark back for this account", label)
		case acctInterrupted:
			if status == "success" {
				status = "degraded"
			}
			syncErrors = append(syncErrors, fmt.Sprintf(
				"%s: run deadline reached, %d transaction(s) imported, resuming from %s",
				label, acctAdded+acctUpdated, resumeFrom.Format("2006-01-02")))
			if !resumeFrom.IsZero() {
				if err := s.st.UpdateBankAccountStartDate(acct.ID, resumeFrom.Format("2006-01-02")); err != nil {
					log.Printf("Failed to record resume point for account %d: %v", acct.ID, err)
				}
			}
		default:
			markSynced()
		}

		s.settleBalances(ctx, acct, account.ID, dateFrom, balancesBefore, balanceErr,
			rawTxns, droppedTxns, acctWriteFailed, acctInterrupted)

		importSpan.SetAttributes(
			attribute.Int("added", acctAdded),
			attribute.Int("confirmed", acctUpdated),
			attribute.Int("skipped", acctSkipped),
			attribute.Int("held", acctHeld),
			attribute.Bool("write_failed", acctWriteFailed),
		)
		importSpan.End()
		olog.Info(importCtx, "import.batch.completed",
			logs.String("bank", label),
			logs.String("budget_account", accountName),
			logs.Int("added", acctAdded),
			logs.Int("confirmed", acctUpdated),
			logs.Int("skipped", acctSkipped),
			logs.Int("held", acctHeld),
		)
		s.flushUSample(importCtx, acct.ID, label, pol.ClassificationVersion(), usample)
	}

	sampler.ask(ctx, s)

	if fetchFailed == len(bankAccounts) {
		status = "fetch_error"
		span.SetStatus(codes.Error, "all fetches failed")
		return true
	}

	if runner, ok := s.ac.(budget.RuleRunner); ok {
		rulesCtx, rulesSpan := tracer.Start(ctx, "rules.apply")
		applied, err := runner.ApplyRules(rulesCtx, newlyTouched)
		if err != nil {
			log.Printf("Failed to apply rules: %v", err)
			rulesSpan.RecordError(err)
			olog.Error(rulesCtx, "rules.apply.failed", logs.String("error", err.Error()))
		} else {
			rulesApplied := int64(applied)
			if s.met != nil && rulesApplied > 0 {
				s.met.rulesApplied.Add(ctx, rulesApplied, metric.WithAttributes(attribute.String("backend", s.backendName)))
			}
			rulesSpan.SetAttributes(attribute.Int64("rules_applied", rulesApplied))
			olog.Info(rulesCtx, "rules.applied",
				logs.Int64("rules_applied", rulesApplied),
				logs.Int("transactions_evaluated", len(newlyTouched)),
			)
		}
		rulesSpan.End()
	}

	var commitErr error
	if flusher, ok := s.ac.(budget.Flusher); ok {
		commitErr = flusher.Commit(ctx)
	}
	if commitErr != nil {
		log.Printf("Budget backend commit error: %v", commitErr)
		span.RecordError(commitErr)
		span.SetStatus(codes.Error, "commit failed")
		if s.met != nil {
			s.met.commitErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("backend", s.backendName)))
		}
		status = "error"
		syncErrors = append(syncErrors, fmt.Sprintf(
			"budget backend commit failed (%v) — buffered changes are retried on the next sync", commitErr))
		olog.Error(ctx, "budget.commit.failed_in_sync", logs.String("error", commitErr.Error()))
	}

	span.SetAttributes(
		attribute.Int("total_added", added),
		attribute.Int("total_confirmed", updated),
		attribute.Int("total_skipped", skipped),
		attribute.Int("accounts_synced", len(bankAccounts)-fetchFailed),
		attribute.Int("accounts_failed", fetchFailed),
	)

	if s.met != nil {
		s.met.txAdded.Add(ctx, int64(added), metric.WithAttributes(attribute.String("backend", s.backendName)))
		s.met.txConfirmed.Add(ctx, int64(updated), metric.WithAttributes(attribute.String("backend", s.backendName)))
		s.met.txSkipped.Add(ctx, int64(skipped), metric.WithAttributes(attribute.String("backend", s.backendName)))
	}
	log.Printf("Done: %d added, %d confirmed, %d skipped", added, updated, skipped)

	if commitErr != nil {
		return true
	}

	stamp := time.Now().UTC().Format("2006-01-02")
	for _, id := range synced {
		if err := s.st.SetBankAccountLastSyncDate(id, stamp); err != nil {
			log.Printf("Failed to save watermark for account %d: %v", id, err)
		}
	}
	for _, id := range backfilled {
		if err := s.st.UpdateBankAccountStartDate(id, ""); err != nil {
			log.Printf("Failed to clear start sync date for account %d: %v", id, err)
			continue
		}
		log.Printf("Backfill complete for account %d — future syncs use the rolling window", id)
	}

	if err := s.state.SetLastSyncDate(time.Now().UTC().Format("2006-01-02"), s.st); err != nil {
		log.Printf("Failed to save state: %v", err)
	}

	if s.checkUpdate != nil {
		go func() {
			updCtx, cancel := context.WithTimeout(s.baseContext(), updateCheckTimeout)
			defer cancel()
			s.checkUpdate(updCtx, s.st)
		}()
	}

	return true
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

// candidateWindow bounds the fetch of existing transactions to what the matcher
// can possibly reach: the earliest date we asked the bank for, widened by the
// match window on both sides, and never ending before the latest date the bank
// actually returned.
func candidateWindow(dateFrom time.Time, txns []enablebanking.Transaction) (time.Time, time.Time) {
	lo, _ := budget.WindowBounds(dateFrom)
	latest := time.Now().UTC()
	for _, t := range txns {
		if t.Date.After(latest) {
			latest = t.Date
		}
	}
	_, hi := budget.WindowBounds(latest)
	return lo, hi
}

func (s *Syncer) shouldWarnExpiry(accountID int64, stage string) bool {
	key := fmt.Sprintf("expiry_warned_%d", accountID)
	prev, _ := s.st.GetSetting(key)
	if prev == stage {
		return false
	}
	_ = s.st.SetSetting(key, stage)
	return true
}

// lessTxn orders two incoming transactions so that a run over the same data is a
// run over the same order, whatever sequence the bank returned it in.
//
// Date first, because the loop's resume point depends on it. Then status, so an
// authorisation is offered before the booking that settles it rather than after.
// The rest are tie-breaks with no meaning beyond being total.
func lessTxn(a, b enablebanking.Transaction) bool {
	if !a.Date.Equal(b.Date) {
		return a.Date.Before(b.Date)
	}
	if a.Status != b.Status {
		return a.Status > b.Status // "PDNG" before "BOOK"
	}
	if a.AmountCents != b.AmountCents {
		return a.AmountCents < b.AmountCents
	}
	if a.Payee != b.Payee {
		return a.Payee < b.Payee
	}
	return a.EntryRef < b.EntryRef
}

// pendingEntry finds the pending map entry an incoming transaction settles,
// under its current identity or the one it was recorded with before v3.
//
// It returns the key that actually matched, because that is the key the entry
// has to be deleted under once the booking has consumed it. Writing only ever
// uses the current scheme, so the old one drains away by itself.
func (s *Syncer) pendingEntry(acctID int64, key, legacy string) (matched, value string, ok bool) {
	pending := s.state.Pending(acctID)
	if v, found := pending[key]; found {
		return key, v, true
	}
	if legacy != "" {
		if v, found := pending[legacy]; found {
			return legacy, v, true
		}
	}
	return "", "", false
}

// minFrequencySample is how many transactions an account has to have offered
// before its payee distribution is used as evidence.
//
// A frequency drawn from five rows is not a distribution, it is a coincidence:
// every payee in it looks like twenty per cent of the account, and the
// correction would then penalise all of them equally and for no reason. Below
// this the shipped u is the better estimate, and the correction stands down.
const minFrequencySample = 50

// minPayeeFrequency is the rarest a payee is allowed to count as, and it caps the
// term-frequency correction at log2(u_exact / minPayeeFrequency) = +3.64 bits.
//
// A constant, not a function of the statement length. See payeeFrequency for what
// went wrong when it was the latter.
const minPayeeFrequency = 0.004

// payeeFrequency builds this account's payee distribution from the statement the
// bank just served.
//
// The bank feed is the right sample for it: u asks how often two unrelated rows
// on one account agree on a payee, and that is a property of the account's
// traffic rather than of the budget's contents. It is rebuilt each run and
// stored nowhere — a distribution is a description of what the account does now,
// not a parameter of the product.
//
// The returned function floors the frequency at a fixed rate, which is the part
// that has to be a constant and used to not be.
//
// It was two occurrences divided by the statement length. That reads as a floor
// and behaves as the opposite of one: it falls as the account accumulates
// history, so the cap on the correction loosens exactly as the traffic that could
// trip it grows. Measured across statement sizes it removed a fixed single bit at
// every size — fifty rows or eight thousand — and left the payee term reaching
// +11.1 bits on a long statement, against +6.544 for the entire rest of the model
// at its strongest. A correction worth more than everything else combined is a
// gate, which is the thing the floor was introduced to prevent.
//
// A constant caps it at log2(u_exact / floor), so 0.004 buys +3.64 bits: enough
// for a genuinely distinctive name to matter and not enough for it to decide
// alone. Splink's tf_minimum_u_value is a fixed constant for the same reason,
// documented as stopping exceedingly rare values from creating disproportionately
// large adjustments.
func payeeFrequency(txns []enablebanking.Transaction, prefixes []string) func(string) float64 {
	if len(txns) < minFrequencySample {
		return nil
	}
	counts := make(map[string]int, len(txns))
	for _, t := range txns {
		if n := keyPayee(t.Payee, prefixes); n != "" {
			counts[n]++
		}
	}
	total := 0
	for _, c := range counts {
		total += c
	}
	if total == 0 {
		return nil
	}
	return func(normalised string) float64 {
		c := counts[strings.ToLower(normalised)]
		if c == 0 {
			// Not in this statement at all. The account has said nothing about
			// this name, so neither does the correction.
			return 0
		}
		if f := float64(c) / float64(total); f > minPayeeFrequency {
			return f
		}
		return minPayeeFrequency
	}
}

// minWidthEvidence is how many distinct payees have to end at exactly the same
// length before that length is read as a field boundary rather than a
// coincidence. One long name proves nothing; several stopping at the same
// character is what a fixed-width field looks like from the outside.
const minWidthEvidence = 3

// measureFieldWidth reports the longest payee an institution has been seen to
// send, and whether that length looks like a limit it imposes.
//
// The longest string a bank has ever sent is a lower bound on its field width.
// An institution whose maximum is reached by several different names is cutting
// them there; one whose longest name is simply its longest name is not.
//
// This is a diagnostic and changes no weight. Turning it into a per-bank
// parameter would be an unlabelled adjustment to the model — a claim about that
// institution derived from nobody's decision — and the place for it is the
// decision log and the export, where a person can look at it.
func measureFieldWidth(txns []enablebanking.Transaction) (width int, truncating bool) {
	atMax := map[string]bool{}
	for _, t := range txns {
		n := len([]rune(t.Payee))
		switch {
		case n > width:
			width = n
			atMax = map[string]bool{t.Payee: true}
		case n == width && n > 0:
			atMax[t.Payee] = true
		}
	}
	return width, len(atMax) >= minWidthEvidence
}

// assignBatchSize bounds how many transactions are arranged together.
//
// The arrangement only has to see transactions that compete for the same rows,
// and those are near one another in date order — a candidate window is a
// fortnight, a chunk this size spans far more. Splitting the batch therefore
// costs nothing in practice and buys two things that matter: bounded memory on a
// first sync of years of history, and a resume point that keeps advancing when a
// long backfill runs out of time.
const assignBatchSize = 200

// workKind is which of the sync loop's paths set a transaction aside, and so
// which bookkeeping follows once it has been placed.
type workKind int

const (
	// workPending is an authorisation with no record of it yet.
	workPending workKind = iota

	// workStalePending is a booking whose recorded authorisation is no longer in
	// the backend — deleted by the user, or lost with a backend change.
	workStalePending

	// workBooked is a booking with no authorisation on record at all.
	workBooked
)

// modelWork is one incoming transaction set aside to be placed with the rest of
// its batch, together with what the loop will need afterwards.
type modelWork struct {
	kind       workKind
	fields     budget.ImportedFields
	pendingKey string
	matchedKey string
	ref        string
	date       time.Time
}

// disposition is what became of one placed transaction, in the vocabulary the
// run's counters use.
type disposition int

const (
	// dispositionAdded is a row the budget did not hold before this run.
	dispositionAdded disposition = iota

	// dispositionUpdated is a row the budget already held that this transaction
	// settled — the pre-authorisation case.
	dispositionUpdated

	// dispositionSkipped is a row the budget already held in its final form.
	dispositionSkipped
)

// settle carries out the deduplication bookkeeping one placed transaction needs
// and reports what became of it.
//
// The bookkeeping and the counting are separated because they answer different
// questions. Which records have to be written follows from what the bank sent;
// which counter the run credits follows from what the backend did with it. Kept
// together they produced a switch inside a switch, and three copies of the same
// counter arithmetic underneath it.
//
// touched reports whether the transaction should be offered to the rule runner,
// which is everything except a row the budget already held unchanged.
func (s *Syncer) settle(
	ctx context.Context, label string, acct store.BankAccount,
	w modelWork, t *budget.Transaction, wasCreated bool,
) (placed disposition, touched bool) {
	day := w.date.Format("2006-01-02")
	rememberRef := func() {
		if w.ref == "" {
			return
		}
		if err := s.state.AddImportedRef(acct.ID, w.ref, day, s.st); err != nil {
			bookkeepingFailed(ctx, "AddImportedRef", label, w.ref, err)
		}
	}

	switch w.kind {
	case workPending:
		if !wasCreated {
			return dispositionSkipped, false
		}
		if err := s.state.SetPending(acct.ID, w.pendingKey, t.ID, day, s.st); err != nil {
			bookkeepingFailed(ctx, "SetPending", label, w.ref, err)
		}
		return dispositionAdded, true

	case workStalePending:
		if err := s.state.DeletePending(acct.ID, w.matchedKey, s.st); err != nil {
			bookkeepingFailed(ctx, "DeletePending", label, w.ref, err)
		}
		rememberRef()
		if wasCreated {
			return dispositionAdded, true
		}
		// Touched either way: the row was rewritten even when it already
		// existed, which is the whole point of settling an authorisation.
		return dispositionSkipped, true

	default:
		rememberRef()
		if wasCreated {
			return dispositionAdded, true
		}
		if stale, ok := s.state.FindPendingKeyByTxnID(acct.ID, t.ID); ok {
			if err := s.state.DeletePending(acct.ID, stale, s.st); err != nil {
				bookkeepingFailed(ctx, "DeletePending", label, w.ref, err)
			}
			return dispositionUpdated, true
		}
		return dispositionSkipped, false
	}
}

// recordDecision writes down one matching decision and the parameters that made
// it.
//
// Every incoming transaction, not only the ones a person is asked about. The
// automatic band is where the costly mistakes happen and where nobody is
// looking, and a model later estimated on the doubtful cases alone would be
// biased by exactly the cases it never saw.
//
// A failure here is logged and swallowed. The record is a diagnostic; losing one
// must not cost an import.
func (s *Syncer) recordDecision(
	ctx context.Context, runID, label string, acct store.BankAccount,
	w modelWork, out budget.Outcome, pol budget.Policy,
) {
	d := store.MatchDecision{
		RunID: runID, BankAccountID: acct.ID, Bank: label,
		IncomingRef: w.ref, PendingKey: w.pendingKey,
		TxnDate: w.date.Format("2006-01-02"),
		Outcome: out.Name(), ParamVersion: pol.Version(),
		Margin: out.Margin,
	}
	if b := out.Best; b != nil {
		d.PayeeLevel = b.Comparison.Payee.String()
		d.AmountLevel = b.Comparison.Amount.String()
		d.DateLevel = b.Comparison.Date.String()
		d.Weight = b.Weight
		d.Probability = b.Probability
	}
	if out.Best != nil {
		// How many candidates the prior was taken over, which is what the weight
		// above was actually computed with — not how many rows the window held.
		d.Candidates = out.Best.Plausible
	}
	if out.Shadow != nil && pol.Trial != nil {
		d.ShadowVersion = pol.Trial.Version(pol)
		d.ShadowOutcome = out.Shadow.Outcome
	}
	if out.Adopted() {
		d.CandidateID = out.Transaction.ID
	}

	if err := s.st.AddMatchDecision(d); err != nil {
		log.Printf("[%s] could not record the matching decision: %v", label, err)
		olog.Warn(ctx, "match.decision_not_recorded",
			logs.String("bank", label),
			logs.String("error", err.Error()))
	}
}

// recordShadow counts one decision against the candidate parameter set being
// watched, if there is one.
//
// The promotion page already reports how many decisions a candidate would have
// changed. This is the same comparison as a series, which answers a question the
// page cannot: not how many, but when. A candidate that diverges steadily is a
// different thing from one that diverged during a single week of unusual
// traffic, and only a time series tells them apart.
//
// The candidate is labelled by its own parameter version rather than by the one
// in force. A counter does not reset when the watch moves to another candidate,
// so without it two candidates' tallies would add up into one meaningless line.
func (s *Syncer) recordShadow(ctx context.Context, label string, out budget.Outcome, pol budget.Policy) {
	if s.met == nil || s.met.shadowDecisions == nil || out.Shadow == nil || pol.Trial == nil {
		return
	}
	agreement := "same"
	if out.Differs() {
		agreement = "different"
	}
	s.met.shadowDecisions.Add(ctx, 1, metric.WithAttributes(
		attribute.String("bank", label),
		attribute.String("backend", s.backendName),
		attribute.String("candidate", pol.Trial.Version(pol)),
		attribute.String("agreement", agreement)))
}

// recordReferenceLabel writes down what the model would have said about a pair
// the bank's own key has already settled.
//
// This is the second source of labels, and the one that reaches where the first
// cannot. Review answers only ever describe the band a person was asked about;
// above and below it the model decides alone and nobody ever finds out whether
// it was right.
//
// It only counts as evidence when the bank supplied the reference. Then the key
// the pending map matched on is the bank's own identifier, the model's three
// comparison fields played no part in selecting the pair, and the record is a
// match somebody other than this program vouched for. That is the standard way
// m is estimated in record linkage: from the subset a shared identifier resolves,
// applied to the rest.
//
// Without a reference the key is date|amount|payee|n, which is every field the
// model compares. A pair selected that way agrees on those fields by
// construction, so scoring the model against it asks the model to confirm the
// rule that picked the pair. Fellegi-Sunter parameters cannot be estimated for
// the fields that did the blocking, and these are all of them. Such a record is
// still written, because the false-negative count is worth having either way,
// but its truth is left unset so that nothing ever fits to it.
//
// The comparison must be taken from the row as it stood before the merge. Both
// backends end Update with budget.Apply, which patches the caller's transaction
// in place, so a row read afterwards has already been given the incoming amount
// and payee and would score as a perfect agreement with itself.
//
// Candidates is left at zero rather than at what a one-element window reports.
// Zero means the window was not measured, which is the truth here: this pair
// never reached the matcher.
//
// It buys nothing arithmetically today and it is recorded for what it says, not
// for what it changes. An earlier version of this comment claimed zero differed
// from one because "prior(1) is a half, and a half is zero bits" — but prior()
// floors its argument at one, so prior(0) and prior(1) are the same number and
// Weight(exact/exact/same, 0) and Weight(..., 1) are bit-identical. The
// distinction the comment claimed did not exist.
//
// The distinction that does exist is downstream and is worth keeping the field
// honest for: these labels are all positives, and they carry a prior term of
// zero bits while a review label carries −1 to −2.3 bits for the two to five
// candidates its window held. That is an artificial positive correlation between
// weight and label which comes from bookkeeping rather than from data, and it
// inflates apparent discrimination and biases the Platt slope upward. Recording
// the count as unmeasured is what leaves a later correction able to find these
// rows.
func (s *Syncer) recordReferenceLabel(
	ctx context.Context, runID, label string, acct store.BankAccount,
	existing *budget.Transaction, in budget.ImportedFields,
	pendingKey, ref string, pol budget.Policy,
) {
	scored := budget.Assess([]*budget.Transaction{existing}, in, nil, pol)
	if len(scored) == 0 {
		log.Printf("[%s] a pair the key settled could not be scored, so it went unrecorded: "+
			"candidate %s is cleared under a foreign reference", label, existing.ID)
		if s.met != nil {
			s.met.matchLabels.Add(ctx, 1, metric.WithAttributes(
				attribute.String("source", "reference_unscorable"),
				attribute.String("bank", label)))
		}
		return
	}
	c := scored[0]

	byReference := ref != ""
	outcome := "confirmed_by_fallback_key"
	var truth *bool
	if byReference {
		outcome = "confirmed_by_reference"
		settled := true
		truth = &settled
	}

	d := store.MatchDecision{
		RunID: runID, BankAccountID: acct.ID, Bank: label,
		IncomingRef: ref, PendingKey: pendingKey,
		CandidateID: existing.ID,
		PayeeLevel:  c.Comparison.Payee.String(),
		AmountLevel: c.Comparison.Amount.String(),
		DateLevel:   c.Comparison.Date.String(),
		Candidates:  0,
		Weight:      c.Weight, Probability: c.Probability,
		Margin:  math.Inf(1),
		Outcome: outcome, ParamVersion: pol.Version(),
		TxnDate: in.Date.Format("2006-01-02"),
		Truth:   truth,
	}
	// Written settled, not written and then settled: the answer is known at the
	// moment the record is made, and a second statement to say so would only be
	// a second thing that can fail.
	if err := s.st.AddMatchDecision(d); err != nil {
		log.Printf("[%s] could not record a reference label: %v", label, err)
		return
	}

	if s.met != nil {
		// The distribution behind the agreed/disagreed count below. Whether the
		// model would have got there is a yes or a no; how nearly it got there is
		// what tells an operator where a threshold belongs, and on a feed the
		// reference resolves there is no other source for it.
		s.met.referenceProb.Record(ctx, c.Probability, metric.WithAttributes(
			attribute.String("bank", label),
			attribute.String("backend", s.backendName),
			attribute.String("param_version", pol.Version())))
		source := "fallback_key"
		if byReference {
			source = "reference"
		}
		s.met.matchLabels.Add(ctx, 1, metric.WithAttributes(
			attribute.String("source", source),
			attribute.String("bank", label)))
		// Whether the model would have got there on its own. This is the
		// measurement, and it needs no fitting to be useful — which is why it is
		// recorded for both sources even though only one of them is evidence.
		agreed := "no"
		if c.Probability >= pol.AutoProbability {
			agreed = "yes"
		}
		s.met.matchLabels.Add(ctx, 1, metric.WithAttributes(
			attribute.String("source", source+"_model_agreed"),
			attribute.String("agreed", agreed),
			attribute.String("bank", label)))
	}
}
