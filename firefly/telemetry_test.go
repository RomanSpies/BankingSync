package firefly_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"bankingsync/budget"
	"bankingsync/firefly"
	"bankingsync/firefly/fireflytest"
)

// recordTelemetry installs a span recorder and a manual metric reader for the
// duration of one test, then builds a Store on top of them.
//
// The provider has to be installed before firefly.New runs: the counters are
// created there and bind to whichever provider is current, which is the reason
// they hang off the Client instead of off a package variable.
func recordTelemetry(t *testing.T) (*fireflytest.Server, *firefly.Store, *tracetest.SpanRecorder, *sdkmetric.ManualReader) {
	t.Helper()

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	prevT, prevM := otel.GetTracerProvider(), otel.GetMeterProvider()
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prevT)
		otel.SetMeterProvider(prevM)
	})

	s := fireflytest.New(t)
	c := firefly.New(s.URL, s.Token(),
		firefly.WithHTTPClient(s.Client()),
		firefly.WithBackoffBase(time.Millisecond))
	return s, firefly.NewStore(c, firefly.Config{PendingTag: "pending"}), sr, reader
}

func spanNames(sr *tracetest.SpanRecorder) []string {
	var out []string
	for _, s := range sr.Ended() {
		out = append(out, s.Name())
	}
	return out
}

func hasSpan(sr *tracetest.SpanRecorder, name string) bool {
	for _, s := range sr.Ended() {
		if s.Name() == name {
			return true
		}
	}
	return false
}

// counterPoints returns the data points of an int64 counter, or nil if the
// instrument produced nothing at all.
func counterPoints(t *testing.T, r *sdkmetric.ManualReader, name string) []metricdata.DataPoint[int64] {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collecting metrics: %v", err)
	}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", name, m.Data)
			}
			return sum.DataPoints
		}
	}
	return nil
}

func attrValue(dp metricdata.DataPoint[int64], key string) string {
	v, ok := dp.Attributes.Value(attribute.Key(key))
	if !ok {
		return ""
	}
	return v.String()
}

func TestTelemetry_everyRequestGetsAClientSpan(t *testing.T) {
	_, st, sr, _ := recordTelemetry(t)

	if _, err := st.GetOrCreateAccount(context.Background(),
		budget.AccountSpec{Name: "Checking", Currency: "EUR"}); err != nil {
		t.Fatalf("GetOrCreateAccount: %v", err)
	}

	if !hasSpan(sr, "firefly.get_or_create_account") {
		t.Errorf("no span for the store operation, got %v", spanNames(sr))
	}
	var sawHTTP bool
	for _, s := range sr.Ended() {
		if s.SpanKind() == 3 { // trace.SpanKindClient
			sawHTTP = true
		}
	}
	if !sawHTTP {
		t.Errorf("no client span for the HTTP requests, got %v", spanNames(sr))
	}
}

// TestTelemetry_spanNamesCollapseIdentifiers is what keeps the trace backend
// usable. A span named after a transaction id is a span nobody can aggregate.
func TestTelemetry_spanNamesCollapseIdentifiers(t *testing.T) {
	_, st, sr, _ := recordTelemetry(t)
	ctx := context.Background()

	acct, err := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR"})
	if err != nil {
		t.Fatalf("GetOrCreateAccount: %v", err)
	}
	created, err := st.Create(ctx, acct.ID, fields(day(2026, time.July, 10), -1000, "Shop", "ref-1", false))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := st.Update(ctx, created, budget.Patch{Cleared: budget.Bool(true)}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	for _, name := range spanNames(sr) {
		for _, seg := range strings.Split(name, "/") {
			if seg == "" {
				continue
			}
			if _, err := strconv.Atoi(seg); err == nil {
				t.Errorf("span %q carries the identifier %q; it must be templated, or "+
					"every transaction becomes its own span name", name, seg)
			}
		}
	}
	if !hasSpan(sr, "firefly GET /api/v1/transactions/{id}") {
		t.Errorf("the templated route is missing, got %v", spanNames(sr))
	}
}

func TestTelemetry_requestsAreCountedByStatus(t *testing.T) {
	_, st, _, reader := recordTelemetry(t)

	if _, err := st.GetOrCreateAccount(context.Background(),
		budget.AccountSpec{Name: "Checking", Currency: "EUR"}); err != nil {
		t.Fatalf("GetOrCreateAccount: %v", err)
	}

	points := counterPoints(t, reader, "bankingsync_backend_requests_total")
	if len(points) == 0 {
		t.Fatal("no request was counted")
	}
	for _, dp := range points {
		if got := attrValue(dp, "backend"); got != "firefly" {
			t.Errorf("backend label = %q, want firefly", got)
		}
		if attrValue(dp, "route") == "" || attrValue(dp, "method") == "" {
			t.Errorf("a data point is missing its route or method: %v", dp.Attributes)
		}
	}
}

// TestTelemetry_rateLimitingIsCounted covers the case the operator actually
// meets. Firefly rate limits, the client waits and retries, and until now the
// only trace of that was a line in the container log.
func TestTelemetry_rateLimitingIsCounted(t *testing.T) {
	s, st, sr, reader := recordTelemetry(t)

	s.RateLimitNext(1)
	if _, err := st.GetOrCreateAccount(context.Background(),
		budget.AccountSpec{Name: "Checking", Currency: "EUR"}); err != nil {
		t.Fatalf("GetOrCreateAccount: %v", err)
	}

	points := counterPoints(t, reader, "bankingsync_backend_rate_limited_total")
	if len(points) != 1 || points[0].Value != 1 {
		t.Fatalf("rate limit counter: got %v, want a single point of 1", points)
	}

	var sawEvent bool
	for _, span := range sr.Ended() {
		for _, e := range span.Events() {
			if e.Name == "rate_limited" {
				sawEvent = true
			}
		}
	}
	if !sawEvent {
		t.Error("the retry is not visible on the span either, so a trace shows an " +
			"unexplained gap rather than a wait")
	}
}

// TestTelemetry_failedRequestsMarkTheSpan is the difference between a trace that
// shows what went wrong and one that shows a request that merely took a while.
func TestTelemetry_failedRequestsMarkTheSpan(t *testing.T) {
	s, st, sr, reader := recordTelemetry(t)

	s.FailNextWrites(1)
	_, err := st.GetOrCreateAccount(context.Background(), budget.AccountSpec{Name: "Checking", Currency: "EUR"})
	if err == nil {
		t.Fatal("expected the seeded failure to surface")
	}

	// Specifically the client span, not merely some span. The store-level span
	// marks itself failed too, so a check that accepts any failed span passes even
	// with the HTTP span left green — and then a trace shows the operation failing
	// with no indication of which request did it.
	var clientFailed, storeFailed bool
	for _, span := range sr.Ended() {
		if span.Status().Code != 1 { // codes.Error
			continue
		}
		if span.SpanKind() == 3 { // trace.SpanKindClient
			clientFailed = true
		} else {
			storeFailed = true
		}
	}
	if !clientFailed {
		t.Error("the failing HTTP request was not marked on its own span")
	}
	if !storeFailed {
		t.Error("the store operation was not marked as failed")
	}

	// 500 rather than 422: the fixture's write failure is a server error, and a
	// POST is not retried because a lost response may mean the transaction was
	// created after all.
	var sawFailure bool
	for _, dp := range counterPoints(t, reader, "bankingsync_backend_requests_total") {
		if attrValue(dp, "status") == "500" {
			sawFailure = true
		}
	}
	if !sawFailure {
		t.Error("the failed request was not counted under its status, so an error rate " +
			"cannot be built from this metric")
	}
}

// conflictReasons returns the reasons recorded on the conflict counter, so a test
// can assert which case fired rather than only that something did.
func conflictReasons(t *testing.T, r *sdkmetric.ManualReader) []string {
	t.Helper()
	var out []string
	for _, dp := range counterPoints(t, r, "bankingsync_backend_conflicts_total") {
		out = append(out, attrValue(dp, "reason"))
	}
	return out
}

func hasReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}

// TestTelemetry_deletedTransactionIsCountedAsAConflict covers the case a user
// creates by deleting a transaction in Firefly. The sync reports a write failure
// and carries on; until now nothing said how often that happens.
func TestTelemetry_deletedTransactionIsCountedAsAConflict(t *testing.T) {
	s, st, _, reader := recordTelemetry(t)
	ctx := context.Background()

	acct, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR"})
	created, err := st.Create(ctx, acct.ID, fields(day(2026, time.July, 10), -1000, "Shop", "ref-1", false))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	s.DeleteGroup(s.Groups()[0].ID)

	if err := st.Update(ctx, created, budget.Patch{Cleared: budget.Bool(true)}); err == nil {
		t.Fatal("expected the update to fail")
	}
	if got := conflictReasons(t, reader); !hasReason(got, firefly.ConflictGone) {
		t.Errorf("conflict reasons = %v, want one of them to be %q", got, firefly.ConflictGone)
	}
}

// TestTelemetry_userSplitGroupIsCountedAsAConflict is the quietest of the four.
// bankingsync stops updating a transaction the user has split, on purpose and
// without telling anyone — which is right, and is exactly why it needs counting.
func TestTelemetry_userSplitGroupIsCountedAsAConflict(t *testing.T) {
	s, st, _, reader := recordTelemetry(t)
	ctx := context.Background()

	acct, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR"})
	created, err := st.Create(ctx, acct.ID, fields(day(2026, time.July, 10), -1000, "Baumarkt", "ref-1", false))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.SplitGroup(s.Groups()[0].ID); err != nil {
		t.Fatalf("SplitGroup: %v", err)
	}

	if err := st.Update(ctx, created, budget.Patch{Cleared: budget.Bool(true)}); err == nil {
		t.Fatal("expected the update to be refused")
	}
	if got := conflictReasons(t, reader); !hasReason(got, firefly.ConflictUserSplitGroup) {
		t.Errorf("conflict reasons = %v, want one of them to be %q", got, firefly.ConflictUserSplitGroup)
	}
}

func TestTelemetry_duplicateRejectionIsCounted(t *testing.T) {
	_, st, _, reader := recordTelemetry(t)
	ctx := context.Background()

	acct, _ := st.GetOrCreateAccount(ctx, budget.AccountSpec{Name: "Checking", Currency: "EUR"})
	twin := fields(day(2026, time.July, 10), -350, "Kaffee", "ref-1", true)
	if _, err := st.Create(ctx, acct.ID, twin); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := st.Create(ctx, acct.ID, twin); err == nil {
		t.Fatal("the second identical row should have been rejected as a duplicate")
	}

	if got := conflictReasons(t, reader); !hasReason(got, firefly.ConflictDuplicate) {
		t.Errorf("conflict reasons = %v, want one of them to be %q", got, firefly.ConflictDuplicate)
	}
}

// TestTelemetry_currencyMismatchIsCountedAsAConflict covers the refusal that
// stops an import outright. Firefly would silently overrule the currency and
// denominate every amount wrongly, so bankingsync declines — and an operator
// staring at an account that imports nothing should be able to see why.
func TestTelemetry_currencyMismatchIsCountedAsAConflict(t *testing.T) {
	s, st, _, reader := recordTelemetry(t)

	s.AddAccount("Checking", "asset", "USD", "")
	_, err := st.GetOrCreateAccount(context.Background(),
		budget.AccountSpec{Name: "Checking", Currency: "EUR"})
	if err == nil {
		t.Fatal("a currency mismatch must stop the import")
	}
	if got := conflictReasons(t, reader); !hasReason(got, firefly.ConflictCurrencyMismatch) {
		t.Errorf("conflict reasons = %v, want one of them to be %q", got, firefly.ConflictCurrencyMismatch)
	}
}
