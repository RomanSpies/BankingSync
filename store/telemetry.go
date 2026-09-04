package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"bankingsync/logs"
)

// scopeName identifies this package as the source of its metrics, the way
// bankingsync/firefly and bankingsync/actual name themselves.
const scopeName = "bankingsync/store"

var olog = logs.Get(scopeName)

// obs holds the instruments the store records to.
//
// It is behind an atomic pointer and starts nil because of when this package is
// built: Open runs before initTelemetry, so instruments created there would bind
// to the no-op MeterProvider and record into nothing — which is exactly how the
// balance drift gauge came to exist everywhere except in a metrics backend. The
// application calls InitTelemetry once the real provider is installed; until
// then every recording path is a nil check and a return.
type obs struct {
	ops      metric.Int64Counter
	duration metric.Float64Histogram
}

// InitTelemetry binds the store's instruments to whichever MeterProvider is
// installed now. Call it after telemetry is up; calling it twice is harmless,
// and never calling it leaves the store silent rather than broken.
func (s *Store) InitTelemetry() {
	m := otel.GetMeterProvider().Meter(scopeName)

	ops, _ := m.Int64Counter("bankingsync_store_operations_total",
		metric.WithDescription("Database operations, by statement kind, table and whether they succeeded"))
	// A local SQLite file answers in microseconds when nothing is wrong. The
	// buckets are placed to make "nothing is wrong" one bucket and everything
	// above it legible, because the condition worth catching here is not a slow
	// query but a contended or failing database.
	duration, _ := m.Float64Histogram("bankingsync_store_duration_seconds",
		metric.WithDescription("Time spent in one database operation"),
		metric.WithExplicitBucketBoundaries(0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5))

	s.obs.Store(&obs{ops: ops, duration: duration})
}

// record accounts for one completed database operation.
//
// The context is Background because the Store's methods do not take one: this
// package is called from the sync loop, the web handlers and the scheduler alike,
// and threading a context through every method is a larger change than the
// instrumentation is worth. The consequence is stated rather than hidden — these
// records carry no trace correlation, so a slow query shows up in the metric and
// in the log but not as a span under the request that caused it.
func (s *Store) record(query string, started time.Time, err error) {
	// A query that matched nothing is an ordinary answer, not a failure. Counting
	// it as one would make every optional lookup look like a fault.
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}

	kind, table := describeSQL(query)
	if o := s.obs.Load(); o != nil {
		result := "ok"
		if err != nil {
			result = "error"
		}
		attrs := metric.WithAttributes(
			attribute.String("kind", kind),
			attribute.String("table", table),
			attribute.String("result", result))
		o.ops.Add(context.Background(), 1, attrs)
		o.duration.Record(context.Background(), time.Since(started).Seconds(),
			metric.WithAttributes(
				attribute.String("kind", kind),
				attribute.String("table", table)))
	}

	if err != nil {
		// Every database error, in one place. Scattered through the call sites
		// these were either returned and logged by whoever cared or dropped with
		// `_ =`, so a database that had run out of disk could be failing every
		// write while the program reported a successful sync.
		olog.Error(context.Background(), "store.operation_failed",
			logs.String("kind", kind),
			logs.String("table", table),
			logs.String("error", err.Error()))
	}
}

// describeSQL names an operation by what it does and what it touches.
//
// The table is what makes the metric answerable: "inserts are failing" is not
// something anyone can act on, "inserts into match_reviews are failing" is. It
// reads the statement rather than taking a label at each call site, so a query
// added later is described without anybody having to remember to name it — and
// anything it cannot read is labelled "other", which a test then holds to zero.
func describeSQL(query string) (kind, table string) {
	fields := strings.Fields(strings.ToLower(query))
	if len(fields) == 0 {
		return "other", "other"
	}
	kind = fields[0]

	switch kind {
	case "select", "delete":
		return kind, tableAfter(fields, "from")
	case "insert", "replace":
		return kind, tableAfter(fields, "into")
	case "update":
		return kind, cleanTable(fields[1:])
	case "begin", "commit":
		return kind, "transaction"
	case "create", "pragma", "vacuum", "analyze":
		return kind, "schema"
	default:
		return "other", "other"
	}
}

func tableAfter(fields []string, keyword string) string {
	for i, f := range fields {
		if f == keyword {
			return cleanTable(fields[i+1:])
		}
	}
	return "other"
}

// cleanTable takes the identifier off the front of what follows a keyword,
// dropping the punctuation a table name can be written with.
func cleanTable(rest []string) string {
	if len(rest) == 0 {
		return "other"
	}
	name := strings.Trim(rest[0], "`\"'(),;")
	if name == "" {
		return "other"
	}
	return name
}

// db is the instrumented handle every store method uses.
//
// It carries the same method set as *sql.DB for the calls this package makes, so
// the fifty existing call sites are unchanged and a new one is instrumented by
// construction rather than by remembering to be.
type db struct {
	sql *sql.DB
	s   *Store
}

func newDB(handle *sql.DB, s *Store) db { return db{sql: handle, s: s} }

func (d db) Exec(query string, args ...any) (sql.Result, error) {
	started := time.Now()
	res, err := d.sql.Exec(query, args...)
	d.s.record(query, started, err)
	return res, err
}

func (d db) Query(query string, args ...any) (*sql.Rows, error) {
	started := time.Now()
	rows, err := d.sql.Query(query, args...)
	d.s.record(query, started, err)
	return rows, err
}

// QueryRow defers its error to Scan, so the row is wrapped as well. Without that
// the single-value reads — which are most of the settings and every existence
// check — would be timed but never counted as failing.
func (d db) QueryRow(query string, args ...any) *row {
	started := time.Now()
	return &row{row: d.sql.QueryRow(query, args...), s: d.s, query: query, started: started}
}

func (d db) Begin() (*tx, error) {
	started := time.Now()
	t, err := d.sql.Begin()
	if err != nil {
		d.s.record("begin", started, err)
		return nil, err
	}
	return &tx{tx: t, s: d.s, started: started}, nil
}

func (d db) Close() error { return d.sql.Close() }

type row struct {
	row     *sql.Row
	s       *Store
	query   string
	started time.Time
}

func (r *row) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	r.s.record(r.query, r.started, err)
	return err
}

// tx records the transaction as one operation rather than each statement inside
// it.
//
// That is the unit that either happened or did not: a partial transaction is
// rolled back, so counting its statements separately would report writes that
// never landed. The statements are still timed individually for the duration
// histogram, which is where a slow one would show.
type tx struct {
	tx      *sql.Tx
	s       *Store
	started time.Time
	failed  bool
}

func (t *tx) Exec(query string, args ...any) (sql.Result, error) {
	started := time.Now()
	res, err := t.tx.Exec(query, args...)
	if err != nil {
		t.failed = true
	}
	t.s.record(query, started, err)
	return res, err
}

func (t *tx) QueryRow(query string, args ...any) *row {
	started := time.Now()
	return &row{row: t.tx.QueryRow(query, args...), s: t.s, query: query, started: started}
}

func (t *tx) Commit() error {
	err := t.tx.Commit()
	t.s.record("commit", t.started, err)
	return err
}

// Rollback is not recorded. Every caller defers it and it returns
// sql.ErrTxDone on the ordinary path where Commit already succeeded, so
// recording it would report a failed operation for every transaction that
// worked.
func (t *tx) Rollback() error { return t.tx.Rollback() }
