// Package logs is a thin wrapper around the OpenTelemetry logs SDK. Emissions
// pull the active span from the supplied context, so every record carries the
// trace_id and span_id of the operation it describes — enabling trace ↔ log
// correlation in observability backends like Grafana.
//
// Stderr output via the standard library log package is unaffected; this
// wrapper only feeds the OTLP exporter.
package logs

import (
	"context"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
)

// KV is an alias for otellog.KeyValue so callers do not need to import the
// OTel log package directly.
type KV = otellog.KeyValue

var (
	String  = otellog.String
	Int64   = otellog.Int64
	Int     = otellog.Int
	Float64 = otellog.Float64
	Bool    = otellog.Bool
)

// Logger wraps the OTel log API with severity-typed helper methods. The
// underlying log.Logger is resolved on every emit so that callers can construct
// a Logger at package init time (before initTelemetry has installed the global
// LoggerProvider) without binding to the no-op default.
type Logger struct {
	name string
}

// Get returns a Logger for the given instrumentation scope name. Pass the
// importing package's path (e.g. "bankingsync/web") so backends can group
// records by source.
func Get(name string) *Logger {
	return &Logger{name: name}
}

func (l *Logger) Info(ctx context.Context, msg string, attrs ...KV) {
	l.emit(ctx, otellog.SeverityInfo, "INFO", msg, attrs)
}

func (l *Logger) Warn(ctx context.Context, msg string, attrs ...KV) {
	l.emit(ctx, otellog.SeverityWarn, "WARN", msg, attrs)
}

func (l *Logger) Error(ctx context.Context, msg string, attrs ...KV) {
	l.emit(ctx, otellog.SeverityError, "ERROR", msg, attrs)
}

func (l *Logger) emit(ctx context.Context, sev otellog.Severity, sevText, msg string, attrs []KV) {
	var r otellog.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(sev)
	r.SetSeverityText(sevText)
	r.SetBody(otellog.StringValue(msg))
	if len(attrs) > 0 {
		r.AddAttributes(attrs...)
	}
	global.GetLoggerProvider().Logger(l.name).Emit(ctx, r)
}
