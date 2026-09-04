package store

import (
	"context"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"path/filepath"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// instrumented opens a store whose instruments are bound to a manual reader, in
// the order the application uses: provider first, then InitTelemetry.
func instrumented(t *testing.T) (*Store, *sdkmetric.ManualReader) {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	st.InitTelemetry()
	return st, reader
}

type series struct {
	attrs attribute.Set
	count int64
	sum   float64
}

func collect(t *testing.T, reader *sdkmetric.ManualReader, name string) []series {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var out []series
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					out = append(out, series{attrs: dp.Attributes, count: dp.Value})
				}
			case metricdata.Histogram[float64]:
				for _, dp := range d.DataPoints {
					out = append(out, series{attrs: dp.Attributes, count: int64(dp.Count), sum: dp.Sum})
				}
			default:
				t.Fatalf("%s is %T", name, m.Data)
			}
		}
	}
	return out
}

func attrOf(set attribute.Set, key string) string {
	v, ok := set.Value(attribute.Key(key))
	if !ok {
		return ""
	}
	return v.AsString()
}

// TestStoreTelemetry_bindsAfterTheProviderIsInstalled is the check this project
// has earned the hard way.
//
// Open runs before initTelemetry, so an instrument built in Open binds to the
// no-op provider and records into nothing — which is how the balance drift gauge
// came to exist in the code, in the README, and in no metrics backend anywhere.
func TestStoreTelemetry_bindsAfterTheProviderIsInstalled(t *testing.T) {
	st, reader := instrumented(t)

	if err := st.SetSetting("k", "v"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if got := collect(t, reader, "bankingsync_store_operations_total"); len(got) == 0 {
		t.Fatal("no operations recorded; the instruments are bound to a provider " +
			"that goes nowhere")
	}
}

// TestStoreTelemetry_isSilentUntilBound covers the other side. A store opened
// before telemetry exists must not panic or record; it must simply say nothing.
func TestStoreTelemetry_isSilentUntilBound(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.SetSetting("k", "v"); err != nil {
		t.Errorf("an uninstrumented store failed an ordinary write: %v", err)
	}
	if _, err := st.GetSetting("k"); err != nil {
		t.Errorf("an uninstrumented store failed an ordinary read: %v", err)
	}
}

// TestStoreTelemetry_everyQueryNamesItsTable is what makes the metric answerable.
//
// "Inserts are failing" is not something anybody can act on; "inserts into
// match_reviews are failing" is. The label is read from the statement rather than
// given at each call site, so this holds the reader to the queries this package
// actually issues instead of to a list somebody has to keep up to date.
func TestStoreTelemetry_everyQueryNamesItsTable(t *testing.T) {
	st, reader := instrumented(t)

	// A broad sweep of real work, chosen to touch each table rather than to
	// assert anything about the results.
	id, err := st.AddBankAccount(NewBankAccount{
		SessionID: "s", AccountUID: "u", BankName: "TestBank", Currency: "EUR",
	})
	if err != nil {
		t.Fatalf("AddBankAccount: %v", err)
	}
	_, _ = st.GetAllBankAccounts()
	_ = st.SetSetting("k", "v")
	_, _ = st.GetSetting("k")
	_, _ = st.AddSyncLog("success", 1, 0, 0, 0.1, "")
	_, _ = st.GetSyncLogs(5)
	_ = st.SetLastSyncDate("2026-08-25")
	_, _ = st.GetLastSyncDate()
	_ = st.AddMatchReview(MatchReview{
		BankAccountID: id, Backend: "actual", PendingKey: "p", TxnDate: "2026-08-25",
	})
	_, _ = st.GetMatchReviews()
	_, _ = st.CountMatchReviews()
	_, _ = st.CountMatchReviewsByAccount()
	_, _ = st.AllHeldKeys()
	_ = st.PruneMatchReviews()
	_, _, _ = st.ResetImportState()
	_ = st.RemoveBankAccount(id)

	sets := collect(t, reader, "bankingsync_store_operations_total")
	if len(sets) == 0 {
		t.Fatal("nothing recorded")
	}

	tables := map[string]bool{}
	for _, s := range sets {
		table := attrOf(s.attrs, "table")
		tables[table] = true
		if table == "other" || table == "" {
			t.Errorf("a %s statement could not be attributed to a table; the metric "+
				"cannot say what is failing", attrOf(s.attrs, "kind"))
		}
		if k := attrOf(s.attrs, "kind"); k == "other" || k == "" {
			t.Errorf("a statement on %q has no kind", table)
		}
		if r := attrOf(s.attrs, "result"); r != "ok" && r != "error" {
			t.Errorf("result = %q, want ok or error", r)
		}
	}
	// Spot check that the queue really is distinguishable, which is the whole
	// reason for carrying the table at all.
	if !tables["match_reviews"] {
		t.Errorf("no series for match_reviews; got %v", tables)
	}
}

// TestStoreTelemetry_anEmptyResultIsNotAFailure keeps the error rate honest.
//
// Most optional lookups in this package are a QueryRow that may match nothing.
// Counting sql.ErrNoRows as an error would put every one of them in the failure
// series and make the metric useless for spotting a database that is actually
// broken.
func TestStoreTelemetry_anEmptyResultIsNotAFailure(t *testing.T) {
	st, reader := instrumented(t)

	if _, err := st.GetSetting("never-set"); err != nil {
		t.Fatalf("reading a missing setting is an ordinary outcome: %v", err)
	}

	for _, s := range collect(t, reader, "bankingsync_store_operations_total") {
		if attrOf(s.attrs, "result") == "error" {
			t.Errorf("a lookup that matched nothing was counted as a failure on table %q",
				attrOf(s.attrs, "table"))
		}
	}
}

// TestStoreTelemetry_aRealFailureIsCounted is the other half: if nothing can
// reach the error series, the metric is decoration.
func TestStoreTelemetry_aRealFailureIsCounted(t *testing.T) {
	st, reader := instrumented(t)
	_ = st.db.sql.Close()

	if err := st.SetSetting("k", "v"); err == nil {
		t.Fatal("setup: a write against a closed database succeeded")
	}

	var errors int
	for _, s := range collect(t, reader, "bankingsync_store_operations_total") {
		if attrOf(s.attrs, "result") == "error" {
			errors++
		}
	}
	if errors == 0 {
		t.Error("a failing database produced no error series; a full disk would be " +
			"invisible while every write failed")
	}
}

func TestStoreTelemetry_durationIsRecorded(t *testing.T) {
	st, reader := instrumented(t)
	_ = st.SetSetting("k", "v")

	got := collect(t, reader, "bankingsync_store_duration_seconds")
	if len(got) == 0 {
		t.Fatal("no durations recorded; a database that has become slow would " +
			"show up only as a slow sync with no cause")
	}
	// The sum, not just the presence of a series: a timer wired to record zero
	// produces exactly as many data points as a working one, and every query
	// would then sit in the fastest bucket for ever.
	var total float64
	for _, s := range got {
		total += s.sum
	}
	if total <= 0 {
		t.Error("every operation took zero seconds; the elapsed time is not being measured")
	}
}

// TestStoreTelemetry_aFailedSingleRowReadIsCounted covers the path that defers
// its error to Scan.
//
// Most optional lookups in this package are a QueryRow, so if the returned row
// were handed back unwrapped, the largest group of reads in the store would be
// timed and never counted as failing — the silence would be worst exactly where
// the reads are most numerous.
func TestStoreTelemetry_aFailedSingleRowReadIsCounted(t *testing.T) {
	st, reader := instrumented(t)
	_ = st.db.sql.Close()

	if _, err := st.GetSetting("k"); err == nil {
		t.Fatal("setup: a read against a closed database succeeded")
	}

	var errs, selects int
	for _, s := range collect(t, reader, "bankingsync_store_operations_total") {
		if attrOf(s.attrs, "kind") == "select" {
			selects++
			if attrOf(s.attrs, "result") == "error" {
				errs++
			}
		}
	}
	if selects == 0 {
		t.Fatal("single-row reads are not counted at all")
	}
	if errs == 0 {
		t.Error("a single-row read failed against a closed database and was counted as ok")
	}
}

// TestStoreTelemetry_aFailureReachesTheLogPipeline is the half a counter cannot
// do. A metric says how many writes failed; only the log says why, and "why" is
// the difference between a full disk, a locked database and a permissions
// problem.
func TestStoreTelemetry_aFailureReachesTheLogPipeline(t *testing.T) {
	st, _ := instrumented(t)
	rec := recordStoreLogs(t)
	_ = st.db.sql.Close()

	if err := st.SetSetting("k", "v"); err == nil {
		t.Fatal("setup: a write against a closed database succeeded")
	}

	got, ok := rec.find("store.operation_failed")
	if !ok {
		t.Fatalf("a failing database left no record; got %v", rec.bodies())
	}
	if v := attrOfRecord(got, "table"); v != "settings" {
		t.Errorf("table = %q, want settings", v)
	}
	if attrOfRecord(got, "error") == "" {
		t.Error("the cause is not recorded, which is the only thing the log adds " +
			"over the counter")
	}
}

// TestStoreTelemetry_anOrdinaryReadIsNotLogged keeps the error log worth reading.
func TestStoreTelemetry_anOrdinaryReadIsNotLogged(t *testing.T) {
	st, _ := instrumented(t)
	rec := recordStoreLogs(t)

	_ = st.SetSetting("k", "v")
	_, _ = st.GetSetting("k")
	_, _ = st.GetSetting("never-set")

	if _, found := rec.find("store.operation_failed"); found {
		t.Errorf("ordinary work was logged as a failure; got %v", rec.bodies())
	}
}

func TestDescribeSQL(t *testing.T) {
	for query, want := range map[string][2]string{
		"SELECT id FROM bank_accounts WHERE id = ?":     {"select", "bank_accounts"},
		"INSERT INTO match_reviews (a, b) VALUES (?,?)": {"insert", "match_reviews"},
		"UPDATE settings SET value = ? WHERE key = ?":   {"update", "settings"},
		"DELETE FROM imported_refs WHERE id = ?":        {"delete", "imported_refs"},
		"\n\t\tSELECT COUNT(*) FROM pending_map":        {"select", "pending_map"},
		"PRAGMA journal_mode=WAL":                       {"pragma", "schema"},
		"commit":                                        {"commit", "transaction"},
		"begin":                                         {"begin", "transaction"},
		"":                                              {"other", "other"},
		"EXPLAIN QUERY PLAN SELECT 1":                   {"other", "other"},
	} {
		kind, table := describeSQL(query)
		if kind != want[0] || table != want[1] {
			t.Errorf("%q -> (%s, %s), want (%s, %s)", query, kind, table, want[0], want[1])
		}
	}
}

type storeLogRecorder struct {
	records []sdklog.Record
}

func (r *storeLogRecorder) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }

func (r *storeLogRecorder) OnEmit(_ context.Context, rec *sdklog.Record) error {
	r.records = append(r.records, *rec)
	return nil
}

func (r *storeLogRecorder) Shutdown(context.Context) error   { return nil }
func (r *storeLogRecorder) ForceFlush(context.Context) error { return nil }

func (r *storeLogRecorder) find(body string) (sdklog.Record, bool) {
	for _, rec := range r.records {
		if rec.Body().AsString() == body {
			return rec, true
		}
	}
	return sdklog.Record{}, false
}

func (r *storeLogRecorder) bodies() []string {
	var out []string
	for _, rec := range r.records {
		out = append(out, rec.Body().AsString())
	}
	return out
}

func recordStoreLogs(t *testing.T) *storeLogRecorder {
	t.Helper()
	rec := &storeLogRecorder{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(rec))
	prev := global.GetLoggerProvider()
	global.SetLoggerProvider(lp)
	t.Cleanup(func() { global.SetLoggerProvider(prev) })
	return rec
}

func attrOfRecord(rec sdklog.Record, key string) string {
	var out string
	rec.WalkAttributes(func(kv otellog.KeyValue) bool {
		if string(kv.Key) == key {
			out = kv.Value.AsString()
			return false
		}
		return true
	})
	return out
}
