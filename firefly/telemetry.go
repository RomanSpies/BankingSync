package firefly

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// scopeName identifies this package as the source of its spans and metrics, the
// same way actual/client.go names itself.
const scopeName = "bankingsync/firefly"

func tracer() trace.Tracer { return otel.Tracer(scopeName) }

// obs holds the instruments a Client records to.
//
// They hang off the Client rather than off a package variable because an
// instrument binds to whichever MeterProvider was installed when it was created.
// A package-level instrument would bind once, at package initialisation, and a
// test that installs its own provider afterwards would be recording into the
// previous one. Building them in New keeps that from being a question anybody has
// to think about: telemetry is initialised long before a backend is dialled.
type obs struct {
	requests    metric.Int64Counter
	rateLimited metric.Int64Counter
	conflicts   metric.Int64Counter
}

func newObs() obs {
	m := otel.GetMeterProvider().Meter(scopeName)

	requests, _ := m.Int64Counter("bankingsync_backend_requests_total",
		metric.WithDescription("Requests to the budget backend's API, by route and response status"))
	rateLimited, _ := m.Int64Counter("bankingsync_backend_rate_limited_total",
		metric.WithDescription("Requests the budget backend rate limited and that were retried"))
	conflicts, _ := m.Int64Counter("bankingsync_backend_conflicts_total",
		metric.WithDescription("Writes that did not happen as intended, by reason"))

	return obs{requests: requests, rateLimited: rateLimited, conflicts: conflicts}
}

// backendAttr labels every instrument in this package. It is a constant rather
// than a parameter because the package is the Firefly backend — but the metric
// name is deliberately generic, so an Actual implementation can join the same
// series instead of inventing a parallel one.
var backendAttr = attribute.String("backend", "firefly")

func (o obs) recordRequest(ctx context.Context, method, route string, status int) {
	if o.requests == nil {
		return
	}
	o.requests.Add(ctx, 1, metric.WithAttributes(
		backendAttr,
		attribute.String("method", method),
		attribute.String("route", route),
		attribute.Int("status", status),
	))
}

func (o obs) recordRateLimited(ctx context.Context, route string) {
	if o.rateLimited == nil {
		return
	}
	o.rateLimited.Add(ctx, 1, metric.WithAttributes(backendAttr, attribute.String("route", route)))
}

// Reasons a write did not go through as intended. Each is a case where
// bankingsync deliberately does nothing, and deliberate inaction is the hardest
// thing to notice in production: the sync reports success, the transaction is
// simply not there.
const (
	ConflictGone             = "transaction_gone"
	ConflictDuplicate        = "duplicate_rejected"
	ConflictUserSplitGroup   = "user_split_group"
	ConflictCurrencyMismatch = "currency_mismatch"
)

// recordConflict counts one of those cases.
//
// The read paths that skip a user's split group are deliberately not counted
// here: they fire once per such row on every sync, so the counter would measure
// how many splits the user keeps rather than anything that happened. Only the
// refusal to write is counted, because that is the moment something the user
// expected did not occur.
func (o obs) recordConflict(ctx context.Context, reason string) {
	if o.conflicts == nil {
		return
	}
	o.conflicts.Add(ctx, 1, metric.WithAttributes(backendAttr, attribute.String("reason", reason)))
}

// routeTemplate collapses identifiers out of a path, so that it can name a span
// and label a metric without either growing without bound.
//
// /api/v1/transactions/161 becomes /api/v1/transactions/{id}. Without this, every
// transaction the sync touches would be its own span name and its own metric
// series — which for a backend that is written to per transaction is precisely
// the shape that makes a metrics backend fall over.
func routeTemplate(path string) string {
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		if seg != "" && isAllDigits(seg) {
			segments[i] = "{id}"
		}
	}
	return strings.Join(segments, "/")
}

func isAllDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// endSpan closes a span and marks it failed when the operation was.
//
// It exists so the store methods can record an error once, in a deferred call,
// rather than at every return site. A span that ends without a status looks
// successful, which for an operation that returned an error is the one thing a
// trace must not say.
func endSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}
