package main

// Inlined and fixed version of github.com/grafana/otel-profiling-go v0.5.1.
//
// Changes from upstream:
//   - spanNameScope set to allSpans (upstream default: rootSpan) so every span
//     gets its own span_name pprof label, giving per-operation breakdown in
//     Pyroscope flame graphs. spanIDScope stays at the upstream default
//     (rootSpan) so all CPU samples aggregate under the root span's profile.
//   - Bug fix: the upstream withSpanIDScope option incorrectly sets
//     spanNameScope instead of spanIDScope (line 160 in v0.5.1).

import (
	"context"
	"runtime/pprof"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

type profileScope uint

const (
	profileScopeNone     profileScope = iota
	profileScopeRootSpan              // only the first local span on a goroutine
	profileScopeAllSpans              // every span
)

var profileIDKey = attribute.Key("pyroscope.profile.id")

type profileTracerProvider struct {
	noop.TracerProvider
	tp     trace.TracerProvider
	config profileConfig
}

type profileConfig struct {
	spanNameScope profileScope
	spanIDScope   profileScope
}

// newProfileTracerProvider wraps tp so that every span annotates the current
// goroutine with pprof labels (span_name, span_id). Pyroscope reads these
// labels to correlate flame graphs with individual trace spans.
func newProfileTracerProvider(tp trace.TracerProvider) trace.TracerProvider {
	return &profileTracerProvider{
		tp: tp,
		config: profileConfig{
			spanNameScope: profileScopeAllSpans,
			spanIDScope:   profileScopeRootSpan,
		},
	}
}

func (w *profileTracerProvider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	return &profTracer{p: w, tr: w.tp.Tracer(name, opts...)}
}

type profTracer struct {
	noop.Tracer
	p  *profileTracerProvider
	tr trace.Tracer
}

func (w *profTracer) Start(ctx context.Context, spanName string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	ctx, span := w.tr.Start(ctx, spanName, opts...)
	sc := span.SpanContext()
	addID := w.p.config.spanIDScope != profileScopeNone && sc.IsSampled()
	addName := w.p.config.spanNameScope != profileScopeNone && spanName != ""
	if !(addID || addName) {
		return ctx, span
	}

	spanID := sc.SpanID().String()
	s := profSpanWrapper{Span: span, parentCtx: ctx}

	rs, ok := profRootSpanFromContext(ctx)
	if !ok {
		rs.id = spanID
		rs.name = spanName
		ctx = withProfRootSpan(ctx, rs)
	}

	// Attach pyroscope.profile.id attribute so Grafana can link to Pyroscope.
	if w.p.config.spanIDScope == profileScopeAllSpans ||
		(w.p.config.spanIDScope == profileScopeRootSpan && spanID == rs.id) {
		span.SetAttributes(profileIDKey.String(spanID))
	}

	var labels []string
	if addName {
		name := spanName
		if w.p.config.spanNameScope == profileScopeRootSpan {
			name = rs.name
		}
		labels = append(labels, "span_name", name)
	}
	if addID {
		id := spanID
		if w.p.config.spanIDScope == profileScopeRootSpan {
			id = rs.id
		}
		labels = append(labels, "span_id", id)
	}

	ctx = pprof.WithLabels(ctx, pprof.Labels(labels...))
	pprof.SetGoroutineLabels(ctx)
	return ctx, &s
}

// profSpanWrapper restores the parent goroutine labels when the span ends.
type profSpanWrapper struct {
	trace.Span
	parentCtx context.Context
}

func (s *profSpanWrapper) End(options ...trace.SpanEndOption) {
	s.Span.End(options...)
	pprof.SetGoroutineLabels(s.parentCtx)
}

// --- root-span context plumbing ---

type profRootSpanKey struct{}

type profRootSpan struct {
	id   string
	name string
}

func withProfRootSpan(ctx context.Context, s profRootSpan) context.Context {
	return context.WithValue(ctx, profRootSpanKey{}, s)
}

func profRootSpanFromContext(ctx context.Context) (profRootSpan, bool) {
	s, ok := ctx.Value(profRootSpanKey{}).(profRootSpan)
	return s, ok
}
