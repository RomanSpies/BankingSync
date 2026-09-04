package main

import (
	"context"
	"log"
	"os"
	"runtime"
	"time"

	"bankingsync/budget"
	"bankingsync/store"

	pyroscope "github.com/grafana/pyroscope-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// syncMetrics holds all OpenTelemetry counters and histograms used during a sync cycle.
type syncMetrics struct {
	syncRuns      metric.Int64Counter
	txAdded       metric.Int64Counter
	txConfirmed   metric.Int64Counter
	txSkipped     metric.Int64Counter
	txDropped     metric.Int64Counter
	syncDuration  metric.Float64Histogram
	fetchDuration metric.Float64Histogram
	rulesApplied  metric.Int64Counter
	commitErrors  metric.Int64Counter
	writeErrors   metric.Int64Counter
	writeDuration metric.Float64Histogram
	txZeroAmount  metric.Int64Counter
	balanceChecks metric.Int64Counter
	nearMiss      metric.Int64Counter
	matchReviews  metric.Int64Counter
	reviewChoice  metric.Int64Counter
	matchProb     metric.Float64Histogram

	importKeyCollisions metric.Int64Counter
	matchMargin         metric.Float64Histogram
	matchMultiplicity   metric.Int64Counter
	matchLabels         metric.Int64Counter
	inquiryGain         metric.Float64Histogram
	inquiryAnswers      metric.Int64Counter
	referenceProb       metric.Float64Histogram
	shadowDecisions     metric.Int64Counter
}

// initTelemetry configures OpenTelemetry metrics, traces, and logs plus
// Pyroscope continuous profiling. It returns a shutdown function that must be
// deferred by the caller.
func initTelemetry(s *Syncer) func() {
	res, _ := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes("",
			attribute.String("service.name", "bankingsync"),
			attribute.String("service.version", Version),
		),
	)

	ctx := context.Background()
	endpoint := os.Getenv("OTLP_ENDPOINT")

	var mp *sdkmetric.MeterProvider
	var tp *sdktrace.TracerProvider
	var lp *sdklog.LoggerProvider

	if endpoint != "" {
		metricExp, err := otlpmetricgrpc.New(ctx,
			otlpmetricgrpc.WithEndpoint(endpoint),
			otlpmetricgrpc.WithInsecure(),
		)
		if err != nil {
			log.Fatalf("OTLP metric exporter: %v", err)
		}
		mp = sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp,
				sdkmetric.WithInterval(30*time.Second),
			)),
			sdkmetric.WithResource(res),
		)
		otel.SetMeterProvider(mp)

		traceExp, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(endpoint),
			otlptracegrpc.WithInsecure(),
		)
		if err != nil {
			log.Fatalf("OTLP trace exporter: %v", err)
		}
		tp = sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(traceExp),
			sdktrace.WithResource(res),
		)
		otel.SetTracerProvider(newProfileTracerProvider(tp))

		logExp, err := otlploggrpc.New(ctx,
			otlploggrpc.WithEndpoint(endpoint),
			otlploggrpc.WithInsecure(),
		)
		if err != nil {
			log.Fatalf("OTLP log exporter: %v", err)
		}
		lp = sdklog.NewLoggerProvider(
			sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
			sdklog.WithResource(res),
		)
		global.SetLoggerProvider(lp)

		log.Printf("OTLP → %s (metrics + traces + logs)", endpoint)
	} else {
		log.Printf("OTLP_ENDPOINT not set — telemetry disabled")
	}

	var profiler *pyroscope.Profiler
	if pyroscopeURL := os.Getenv("PYROSCOPE_SERVER_ADDRESS"); pyroscopeURL != "" {
		runtime.SetMutexProfileFraction(5)
		runtime.SetBlockProfileRate(1)

		cfg := pyroscope.Config{
			ApplicationName: "bankingsync",
			ServerAddress:   pyroscopeURL,
			Logger:          nil,
			ProfileTypes: []pyroscope.ProfileType{
				pyroscope.ProfileCPU,
				pyroscope.ProfileAllocObjects,
				pyroscope.ProfileAllocSpace,
				pyroscope.ProfileInuseObjects,
				pyroscope.ProfileInuseSpace,
				pyroscope.ProfileGoroutines,
				pyroscope.ProfileMutexCount,
				pyroscope.ProfileMutexDuration,
				pyroscope.ProfileBlockCount,
				pyroscope.ProfileBlockDuration,
			},
		}
		if u := os.Getenv("PYROSCOPE_BASIC_AUTH_USER"); u != "" {
			cfg.BasicAuthUser = u
			cfg.BasicAuthPassword = os.Getenv("PYROSCOPE_BASIC_AUTH_PASSWORD")
		}
		var err error
		profiler, err = pyroscope.Start(cfg)
		if err != nil {
			log.Printf("Pyroscope init failed: %v", err)
		} else {
			log.Printf("Pyroscope → %s", pyroscopeURL)
		}
	} else {
		log.Printf("PYROSCOPE_SERVER_ADDRESS not set — profiling disabled")
	}

	meter := otel.GetMeterProvider().Meter("bankingsync")
	s.met = newSyncMetrics(meter)

	// The store was opened before this function ran, so its instruments could not
	// have existed yet. Binding them here — after the provider is installed, not
	// before — is what keeps them off the no-op provider that records nothing.
	s.st.InitTelemetry()

	// Registered separately so that a test can bind them to a manual reader.
	// An observable gauge is only ever exercised through its callback, and a
	// callback nothing collects from is indistinguishable from one that was
	// never registered — which is the failure the drift gauge shipped with once
	// already.
	s.registerObservers(meter)

	return func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if mp != nil {
			_ = mp.Shutdown(shutCtx)
		}
		if tp != nil {
			_ = tp.Shutdown(shutCtx)
		}
		if lp != nil {
			_ = lp.Shutdown(shutCtx)
		}
		if profiler != nil {
			_ = profiler.Stop()
		}
	}
}

// newSyncMetrics registers all OpenTelemetry instruments with the given meter.
func newSyncMetrics(meter metric.Meter) *syncMetrics {
	syncRuns, _ := meter.Int64Counter("bankingsync_sync_runs_total",
		metric.WithDescription("Total sync cycles completed, labelled by status"))
	txAdded, _ := meter.Int64Counter("bankingsync_transactions_added_total",
		metric.WithDescription("New transactions imported into the budget backend"))
	txConfirmed, _ := meter.Int64Counter("bankingsync_transactions_confirmed_total",
		metric.WithDescription("Pending transactions promoted to BOOK"))
	txSkipped, _ := meter.Int64Counter("bankingsync_transactions_skipped_total",
		metric.WithDescription("Transactions skipped because already imported"))
	txDropped, _ := meter.Int64Counter("bankingsync_transactions_dropped_total",
		metric.WithDescription("Transactions returned by Enable Banking that failed to parse and were dropped"))
	syncDuration, _ := meter.Float64Histogram("bankingsync_sync_duration_seconds",
		metric.WithDescription("Wall-clock duration of a full sync cycle"),
		metric.WithExplicitBucketBoundaries(1, 2, 5, 10, 30, 60, 120))
	fetchDuration, _ := meter.Float64Histogram("bankingsync_fetch_duration_seconds",
		metric.WithDescription("Duration of the Enable Banking transaction fetch"),
		metric.WithExplicitBucketBoundaries(0.1, 0.5, 1, 2, 5, 10, 30))
	rulesApplied, _ := meter.Int64Counter("bankingsync_rules_applied_total",
		metric.WithDescription("Rule actions applied to newly imported transactions (Actual only; Firefly applies rules server-side)"))
	commitErrors, _ := meter.Int64Counter("bankingsync_commit_errors_total",
		metric.WithDescription("Errors committing buffered changes to the budget backend"))
	writeErrors, _ := meter.Int64Counter("bankingsync_write_errors_total",
		metric.WithDescription("Per-transaction write errors against the budget backend"))
	writeDuration, _ := meter.Float64Histogram("bankingsync_budget_write_duration_seconds",
		metric.WithDescription("Time spent importing one bank account into the budget backend"),
		metric.WithExplicitBucketBoundaries(0.01, 0.05, 0.1, 0.5, 1, 5, 15, 60, 300))
	txZeroAmount, _ := meter.Int64Counter("bankingsync_transactions_zero_amount_total",
		metric.WithDescription("Zero-amount transactions skipped on purpose, which is not an error"))
	balanceChecks, _ := meter.Int64Counter("bankingsync_balance_checks_total",
		metric.WithDescription("Balance comparisons concluded, by outcome"))
	nearMiss, _ := meter.Int64Counter("bankingsync_near_miss_total",
		metric.WithDescription("Transactions created although an open row in the window nearly matched"))
	// The bucket edges sit around the two thresholds rather than being evenly
	// spaced. The point of this histogram is to let an operator see where their
	// bank's transactions actually fall before moving a threshold, so resolution
	// is worth most exactly where the decision changes.
	matchProb, _ := meter.Float64Histogram("bankingsync_match_probability",
		metric.WithDescription("Probability of the strongest candidate considered for an incoming "+
			"transaction, by what was decided — the distribution the two thresholds cut through"),
		metric.WithExplicitBucketBoundaries(0.1, 0.25, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 0.95, 0.99))
	// Counts a defect that no longer happens, on purpose. Before v3 a
	// reference-less transaction was identified by its date and amount alone, so
	// two purchases of the same size on the same day — at different shops, even —
	// collapsed into one and the second was silently dropped. This says how often
	// a given feed actually reached that.
	importKeyCollisions, _ := meter.Int64Counter("bankingsync_import_key_collisions_total",
		metric.WithDescription("Incoming transactions that would have shared an identity "+
			"under the pre-v3 import key, and been dropped as repeats of one another"))
	// The margin is bits of total evidence, so the buckets are placed where the
	// decision changes: below one bit the arrangement had a free choice and the
	// case is put to a person, above it the pairing stood on its own.
	matchMargin, _ := meter.Float64Histogram("bankingsync_match_margin",
		metric.WithDescription("How much total evidence a batch would lose if a chosen "+
			"pairing were forbidden — the arrangement's answer to whether there was a "+
			"real alternative"),
		metric.WithExplicitBucketBoundaries(0.25, 0.5, 1, 2, 4, 8, 16))
	matchMultiplicity, _ := meter.Int64Counter("bankingsync_match_multiplicity_total",
		metric.WithDescription("Transactions settled onto one of several rows nothing "+
			"could tell apart — the shape the reported multiplicity defect had"))
	matchLabels, _ := meter.Int64Counter("bankingsync_match_labels_total",
		metric.WithDescription("Decisions something other than the model has settled, by "+
			"source: a person answering a review, or a bank key confirming a pair"))
	// What the one question a run may ask was expected to be worth, in bits of
	// information about the parameters. The buckets are decades because the
	// figure spans them, and they now reach one because the answer is one binary
	// label and cannot be worth more than that.
	//
	// The range moved when the criterion did. It used to be a second-order
	// expansion, which at the variances a fresh installation carries returned
	// values in the thousandths — so buckets ending at 0.5 held everything. Exact
	// BALD saturates instead: measured on shipped parameters with an empty
	// decision log, every comparison is worth between 0.84 and 0.96 bits, and all
	// of it would have landed in one overflow bucket. Against three hundred
	// settled decisions the same comparisons are worth 0.00002 to 0.0015, which
	// is the other end this has to resolve.
	//
	// So a series pinned near one is a fresh installation that knows nothing, and
	// one settling towards the bottom is saying there is nothing left worth
	// asking about and the setting can be turned off again. Those are the two
	// readings the buckets exist to separate.
	inquiryGain, _ := meter.Float64Histogram("bankingsync_match_inquiry_bits",
		metric.WithDescription("Expected information about the matching parameters from the "+
			"one automatic decision a run asks a person to confirm, in bits; an answer is "+
			"one binary label and cannot be worth more than one"),
		metric.WithExplicitBucketBoundaries(0.00001, 0.0001, 0.001, 0.01, 0.05, 0.1, 0.25, 0.5, 0.75, 0.9, 0.99))
	// What the model would have said about a pair the bank's own key already
	// settled. Deliberately not folded into match_probability: that histogram is
	// the distribution the two thresholds cut through, and these pairs never
	// reached a threshold — the reference resolved them before the model was
	// consulted. Mixing them would make "how many decisions sit near a threshold"
	// answer a question nobody asked.
	//
	// It is the only view of the model's accuracy an installation gets when its
	// bank keeps a stable reference, because then the matcher is asked almost
	// nothing and match_probability stays empty. The buckets match
	// match_probability so the two can be read side by side.
	referenceProb, _ := meter.Float64Histogram("bankingsync_match_reference_probability",
		metric.WithDescription("Probability the model would have given a pair the bank's own "+
			"reference settled — its accuracy measured against ground truth it did not provide"),
		metric.WithExplicitBucketBoundaries(0.1, 0.25, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 0.95, 0.99))
	// How a watched candidate compares with the parameters in force, decision by
	// decision. The same tally the promotion page shows, as a series: the page
	// says how many would have differed, this says when.
	shadowDecisions, _ := meter.Int64Counter("bankingsync_match_shadow_decisions_total",
		metric.WithDescription("Decisions made while a candidate parameter set was being "+
			"watched, by whether it would have decided the same"))
	inquiryAnswers, _ := meter.Int64Counter("bankingsync_match_inquiry_answers_total",
		metric.WithDescription("Answers to those confirmations, by verdict: the two rows were "+
			"one payment, they were not, or the person could not say"))
	matchReviews, _ := meter.Int64Counter("bankingsync_match_reviews_total",
		metric.WithDescription("Transactions entering and leaving the review queue, by outcome: "+
			"queued when the matcher would not decide, assigned or imported when a person did"))
	// Whether the row a person picked was the row the model put first. This is
	// the only direct measurement of the model's ranking that exists anywhere in
	// the program: everything else scores the probability it assigned, and this
	// scores the order it put the candidates in.
	//
	// It became recordable at all because the answer is now filed against the
	// candidate actually chosen. Before that a merge into the second row was
	// written down as a merge into the first, so the two cases were
	// indistinguishable afterwards — which is the same defect that was poisoning
	// the m tables, seen from the reporting side.
	reviewChoice, _ := meter.Int64Counter("bankingsync_match_review_choice_total",
		metric.WithDescription("Review answers by whether the person merged into the "+
			"candidate the model ranked first, a different one, or called it new"))
	return &syncMetrics{
		syncRuns:      syncRuns,
		txAdded:       txAdded,
		txConfirmed:   txConfirmed,
		txSkipped:     txSkipped,
		txDropped:     txDropped,
		syncDuration:  syncDuration,
		fetchDuration: fetchDuration,
		rulesApplied:  rulesApplied,
		commitErrors:  commitErrors,
		writeErrors:   writeErrors,
		writeDuration: writeDuration,
		txZeroAmount:  txZeroAmount,
		balanceChecks: balanceChecks,
		nearMiss:      nearMiss,
		matchReviews:  matchReviews,
		reviewChoice:  reviewChoice,
		matchProb:     matchProb,

		importKeyCollisions: importKeyCollisions,
		matchMargin:         matchMargin,
		matchMultiplicity:   matchMultiplicity,
		matchLabels:         matchLabels,
		inquiryGain:         inquiryGain,
		inquiryAnswers:      inquiryAnswers,
		referenceProb:       referenceProb,
		shadowDecisions:     shadowDecisions,
	}
}

// observeDrift reports the last known drift per account.
//
// Split out of the callback so it can be tested without standing up an exporter:
// the callback itself is unreachable unless OTLP_ENDPOINT is set, which is
// exactly the arrangement that let the gauge go unobserved unnoticed.
func (s *Syncer) observeDrift(obs metric.Int64Observer) error {
	accounts, err := s.st.GetAllBankAccounts()
	if err != nil {
		// Returning the error would make the collector log it on every scrape for
		// as long as the database is unhappy, which is noise on top of a problem
		// that is already reported elsewhere.
		return nil
	}
	for _, a := range accounts {
		// An account with no state has never been compared. Reporting zero for it
		// would be indistinguishable from "agrees to the cent".
		if a.DriftState == store.DriftUnknown {
			continue
		}
		obs.Observe(a.DriftCents, metric.WithAttributes(
			attribute.String("bank", bankLabel(a)),
			attribute.String("backend", s.backendName),
			attribute.String("state", a.DriftState),
		))
	}
	return nil
}

// observeOpenReviews reports how many decisions are outstanding per account.
//
// Split out of the callback for the same reason observeDrift is: the callback
// only runs when OTLP_ENDPOINT is set, and a gauge nobody can reach in a test is
// a gauge that goes unobserved without anyone noticing — which has happened here
// once already.
func (s *Syncer) observeOpenReviews(obs metric.Int64Observer) error {
	counts, err := s.st.CountMatchReviewsByAccount()
	if err != nil {
		return nil
	}
	accounts, err := s.st.GetAllBankAccounts()
	if err != nil {
		return nil
	}
	for _, a := range accounts {
		obs.Observe(int64(counts[a.ID]), metric.WithAttributes(
			attribute.String("bank", bankLabel(a)),
			attribute.String("backend", s.backendName),
		))
	}
	return nil
}

// registerObservers binds every observable gauge to a meter.
//
// These are the model's health, and they are gauges rather than counters
// because each answers "what is true now" rather than "how often has this
// happened". They are collected on the reader's own interval, so anything
// expensive has to be read from a cache rather than computed here — see the
// gate gauges, which report the last verdict the promotion page reached rather
// than reaching one of their own.
func (s *Syncer) registerObservers(meter metric.Meter) {
	_, _ = meter.Int64ObservableGauge("bankingsync_pending_transactions",
		metric.WithDescription("Pending transactions awaiting BOOK confirmation"),
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			obs.Observe(s.pendingGauge.Load())
			return nil
		}),
	)

	// The drift gauge is observed here rather than in newSyncMetrics because an
	// observable gauge without a registered callback is never collected. It was
	// declared and stored for exactly that long: the metric appeared in the
	// documentation and in the code, and emitted nothing.
	//
	// The value is read back from the database rather than kept in memory, so a
	// restart does not silently reset every account to no-drift, and so an account
	// that was not part of the last run still reports what it last knew.
	_, _ = meter.Int64ObservableGauge("bankingsync_balance_drift_cents",
		metric.WithDescription("Budget account total minus the bank's booked balance plus outstanding pendings"),
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			return s.observeDrift(obs)
		}),
	)

	// Held transactions are the one state in this program that is invisible by
	// construction: not in the budget, not in the bank feed's future, and waiting
	// on a person who may not be looking. It gets a gauge of its own.
	_, _ = meter.Int64ObservableGauge("bankingsync_match_reviews_open",
		metric.WithDescription("Transactions held back for a person to decide, which are in no budget until they are"),
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			return s.observeOpenReviews(obs)
		}),
	)

	// What each comparison level is currently worth. Constant while the shipped
	// parameters are in force, and that is why it is here: the day a promotion
	// moves one, the change shows on a dashboard instead of only in a database.
	// Read from the set actually deciding, not from the shipped one — reporting
	// the latter would make the gauge silent about the only event it exists for.
	_, _ = meter.Float64ObservableGauge("bankingsync_match_level_weight",
		metric.WithDescription("Evidence a comparison level carries, in bits: log2(m/u)"),
		metric.WithFloat64Callback(func(_ context.Context, obs metric.Float64Observer) error {
			for _, e := range s.linkageInForce().LevelWeights() {
				obs.Observe(e.Bits, metric.WithAttributes(
					attribute.String("field", e.Field),
					attribute.String("level", e.Level)))
			}
			return nil
		}),
	)

	// Calibration, reported only once there are enough settled decisions to say
	// anything. Discrimination and calibration are different properties: a model
	// can rank perfectly and still claim ninety per cent where seventy is the
	// truth, and no ranking metric will show that.
	_, _ = meter.Float64ObservableGauge("bankingsync_match_brier_score",
		metric.WithDescription("Brier score of the matching probabilities against settled "+
			"decisions, split the way Murphy did: rescaling lowers reliability, only a "+
			"better model raises resolution"),
		metric.WithFloat64Callback(func(_ context.Context, obs metric.Float64Observer) error {
			labelled := s.labelledDecisions()
			if len(labelled) == 0 {
				return nil
			}
			b := budget.Brier(labelled, budget.Identity(), 10)
			// All six, because three of them do not add up. Murphy's identity
			// needs stratification on the distinct forecast values; this bins by
			// width and uses each bin's mean, so two further terms appear and
			// reliability − resolution + uncertainty is out by an amount that
			// depends on nothing but the bin count. With the within-bin terms, or
			// equivalently with the generalised resolution, it is an identity
			// again — and a panel that plots the parts against the whole should be
			// able to close.
			for part, v := range map[string]float64{
				"score": b.Score, "reliability": b.Reliability,
				"resolution": b.Resolution, "uncertainty": b.Uncertainty,
				"within_bin_variance":    b.WithinBinVariance,
				"within_bin_covariance":  b.WithinBinCovariance,
				"generalised_resolution": b.GeneralisedResolution(),
			} {
				obs.Observe(v, metric.WithAttributes(
					attribute.String("component", part),
					attribute.String("backend", s.backendName)))
			}
			return nil
		}),
	)

	_, _ = meter.Float64ObservableGauge("bankingsync_match_calibration_error",
		metric.WithDescription("Expected calibration error: the average gap between what "+
			"the matcher claimed and what happened. One number, for an alarm"),
		metric.WithFloat64Callback(func(_ context.Context, obs metric.Float64Observer) error {
			labelled := s.labelledDecisions()
			if len(labelled) == 0 {
				return nil
			}
			// Reported only when there are enough labels for the binned
			// estimator to mean anything. It is biased upwards and the bias
			// does not vanish on a correct model, so below that floor there is
			// no number rather than a small one.
			e, ok := budget.ECE(labelled, budget.Identity(), 10)
			if !ok {
				return nil
			}
			obs.Observe(e, metric.WithAttributes(attribute.String("backend", s.backendName)))
			return nil
		}),
	)

	// How much evidence each level actually rests on. This is the counterpart to
	// match_level_weight: that one says what a level is worth, this says whether
	// anybody has ever seen one. A level whose m side is empty is a level the
	// refit is holding at the ratio it shipped with, which is a stated claim
	// rather than a measurement, and knowing which of the seventeen are in that
	// state is knowing how much of the model is still a guess.
	_, _ = meter.Int64ObservableGauge("bankingsync_match_level_observations",
		metric.WithDescription("Settled decisions that reached each comparison level, by "+
			"field and side: the evidence the fitted parameters would rest on"),
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			counts := s.levelCounts(0)
			observe := func(field, side, level string, n int) {
				obs.Observe(int64(n), metric.WithAttributes(
					attribute.String("field", field),
					attribute.String("side", side),
					attribute.String("level", level)))
			}
			// Driven from the shipped tables rather than from the counts, so that
			// every level reports on every collection. Iterating the counts would
			// leave a level nobody has seen out of the series altogether, and a
			// missing series and a zero would then look the same — which is the
			// distinction this gauge exists to draw.
			base := budget.DefaultLinkage()
			for lv := range base.PayeeM {
				observe("payee", "m", lv.String(), counts.PayeeM[lv])
				observe("payee", "u", lv.String(), counts.PayeeU[lv])
			}
			for lv := range base.AmountM {
				observe("amount", "m", lv.String(), counts.AmountM[lv])
				observe("amount", "u", lv.String(), counts.AmountU[lv])
			}
			for lv := range base.DateM {
				observe("date", "m", lv.String(), counts.DateM[lv])
				observe("date", "u", lv.String(), counts.DateU[lv])
			}
			return nil
		}),
	)

	// The policy actually deciding, as a series rather than only as a settings
	// page and a change record. Without it a shift in any of the other matching
	// series has to be correlated against a log line to be explained, and the two
	// thresholds are the most common reason for one.
	_, _ = meter.Float64ObservableGauge("bankingsync_match_policy",
		metric.WithDescription("The matching policy in force: the two decision thresholds, "+
			"the margin, and the overlap the candidate-count prior is taken over"),
		metric.WithFloat64Callback(func(_ context.Context, obs metric.Float64Observer) error {
			t := s.st.Tunables()
			for name, v := range map[string]float64{
				"auto_probability":   float64(t.AutoProbabilityPct) / 100,
				"review_probability": float64(t.ReviewProbabilityPct) / 100,
				"overlap":            float64(t.OverlapPct) / 100,
				"tolerance_percent":  float64(t.TolerancePercent),
			} {
				obs.Observe(v, metric.WithAttributes(attribute.String("setting", name)))
			}
			return nil
		}),
	)

	// The rescaling in force, which is silent in every other series. A
	// calibration that has fallen back to the identity looks exactly like one
	// that was never fitted, and a slope far from one is the shape of a fit that
	// ran away — the failure this solver was rewritten to stop. Both are visible
	// here and nowhere else.
	_, _ = meter.Float64ObservableGauge("bankingsync_match_calibration_coefficient",
		metric.WithDescription("The Platt rescaling in force: A is the slope applied to the "+
			"match weight and B the intercept, in bits. A = 1 with B = 0 is no rescaling"),
		metric.WithFloat64Callback(func(_ context.Context, obs metric.Float64Observer) error {
			c := s.matchPolicy("").Calibration
			if (c == budget.Calibration{}) {
				c = budget.Identity()
			}
			obs.Observe(c.A, metric.WithAttributes(attribute.String("coefficient", "slope")))
			obs.Observe(c.B, metric.WithAttributes(attribute.String("coefficient", "intercept")))
			return nil
		}),
	)

	// What the promotion gate concluded, which is the closest thing this program
	// has to "is the model getting better". The page states it in a sentence; a
	// series is what says whether the sentence has been the same for a month.
	//
	// Read from the last real evaluation rather than provoking one, so these move
	// when somebody opens the page. A flat line therefore means either that
	// nothing has changed or that nobody has looked, and gate_evaluated_seconds
	// is what tells the two apart.
	_, _ = meter.Float64ObservableGauge("bankingsync_match_gate",
		metric.WithDescription("The promotion gate's last verdict: the candidate's Brier "+
			"skill against the parameters in force, the significance of that difference, "+
			"and the bar it had to clear"),
		metric.WithFloat64Callback(func(_ context.Context, obs metric.Float64Observer) error {
			g, seen := s.gateState()
			if !seen {
				return nil
			}
			figures := map[string]float64{
				"labelled":    float64(g.labelled),
				"holdout":     float64(g.holdout),
				"base_brier":  g.baseBrier,
				"trial_brier": g.trialBrier,
				// How many of the seventeen comparison levels the candidate
				// still carries at the weight they shipped with, because the
				// labels said nothing about them. A candidate that scores well
				// with half its levels in that state is scoring well on the
				// shipped table, and no other figure here says so.
				"levels_held_at_prior": float64(g.levelsHeld),
			}
			// The test's own figures exist only when there was something to test.
			// Reporting a p-value of zero for "no comparison was possible" would
			// read on a dashboard as overwhelming significance.
			if g.tested {
				figures["skill_percent"] = g.skill
				figures["p_value"] = g.pValue
				figures["significance_level"] = g.level
				figures["statistic"] = g.statistic
			}
			for name, v := range figures {
				obs.Observe(v, metric.WithAttributes(attribute.String("figure", name)))
			}
			return nil
		}),
	)

	// Each check separately, because they fail for unrelated reasons and want
	// unrelated responses. A calibration check that will not open wants more
	// labels; an anchor check that will not open wants somebody to look at what
	// the candidate would do to a documented case.
	_, _ = meter.Int64ObservableGauge("bankingsync_match_gate_check",
		metric.WithDescription("Each promotion check's standing, one where it holds that "+
			"status and zero where it does not, by check name and status"),
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			g, seen := s.gateState()
			if !seen {
				return nil
			}
			for name, status := range g.checks {
				for _, possible := range []string{"passed", "failed", "unavailable", "for a person"} {
					v := int64(0)
					if status == possible {
						v = 1
					}
					obs.Observe(v, metric.WithAttributes(
						attribute.String("check", name),
						attribute.String("status", possible)))
				}
			}
			promotable := int64(0)
			if g.promotable {
				promotable = 1
			}
			obs.Observe(promotable, metric.WithAttributes(
				attribute.String("check", "overall"),
				attribute.String("status", "promotable")))
			return nil
		}),
	)

	_, _ = meter.Int64ObservableGauge("bankingsync_session_expiry_days",
		metric.WithDescription("Days until the Enable Banking session expires"),
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			if s.expiryDaysKnown.Load() {
				obs.Observe(s.expiryDaysGauge.Load())
			}
			return nil
		}),
	)
}
