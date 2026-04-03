package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"runtime"
	"time"

	otelpyroscope "github.com/grafana/otel-profiling-go"
	pyroscope "github.com/grafana/pyroscope-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
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
	syncDuration  metric.Float64Histogram
	fetchDuration metric.Float64Histogram
	rulesApplied  metric.Int64Counter
	commitErrors  metric.Int64Counter
}

// initTelemetry configures OpenTelemetry metrics and traces and Pyroscope
// continuous profiling. It registers the /metrics instrumentation on mux and
// returns a shutdown function that must be deferred by the caller.
func initTelemetry(s *Syncer, mux *http.ServeMux) func() {
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
		otel.SetTracerProvider(otelpyroscope.NewTracerProvider(tp))

		log.Printf("OTLP → %s (metrics + traces)", endpoint)
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

	_, _ = meter.Int64ObservableGauge("bankingsync_pending_transactions",
		metric.WithDescription("Pending transactions awaiting BOOK confirmation"),
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			if s.state != nil {
				obs.Observe(int64(len(s.state.PendingMap)))
			}
			return nil
		}),
	)

	_, _ = meter.Int64ObservableGauge("bankingsync_session_expiry_days",
		metric.WithDescription("Days until the Enable Banking session expires"),
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			if s.state != nil {
				if _, _, expiry, err := s.state.GetSession(); err == nil {
					obs.Observe(int64(time.Until(expiry).Hours() / 24))
				}
			}
			return nil
		}),
	)

	return func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if mp != nil {
			_ = mp.Shutdown(shutCtx)
		}
		if tp != nil {
			_ = tp.Shutdown(shutCtx)
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
		metric.WithDescription("New transactions imported into Actual Budget"))
	txConfirmed, _ := meter.Int64Counter("bankingsync_transactions_confirmed_total",
		metric.WithDescription("Pending transactions promoted to BOOK"))
	txSkipped, _ := meter.Int64Counter("bankingsync_transactions_skipped_total",
		metric.WithDescription("Transactions skipped because already imported"))
	syncDuration, _ := meter.Float64Histogram("bankingsync_sync_duration_seconds",
		metric.WithDescription("Wall-clock duration of a full sync cycle"),
		metric.WithExplicitBucketBoundaries(1, 2, 5, 10, 30, 60, 120))
	fetchDuration, _ := meter.Float64Histogram("bankingsync_fetch_duration_seconds",
		metric.WithDescription("Duration of the Enable Banking transaction fetch"),
		metric.WithExplicitBucketBoundaries(0.1, 0.5, 1, 2, 5, 10, 30))
	rulesApplied, _ := meter.Int64Counter("bankingsync_rules_applied_total",
		metric.WithDescription("Rule actions applied to newly imported transactions"))
	commitErrors, _ := meter.Int64Counter("bankingsync_commit_errors_total",
		metric.WithDescription("Errors committing changes to Actual Budget"))
	return &syncMetrics{
		syncRuns:      syncRuns,
		txAdded:       txAdded,
		txConfirmed:   txConfirmed,
		txSkipped:     txSkipped,
		syncDuration:  syncDuration,
		fetchDuration: fetchDuration,
		rulesApplied:  rulesApplied,
		commitErrors:  commitErrors,
	}
}
