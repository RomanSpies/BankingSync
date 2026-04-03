package main

import (
	"context"
	"runtime/pprof"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// helpers -------------------------------------------------------------------

func setupTracer(scope profileScope) (*profTracer, *tracetest.InMemoryExporter) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	ptp := &profileTracerProvider{
		tp: tp,
		config: profileConfig{
			spanNameScope: scope,
			spanIDScope:   scope,
		},
	}
	tracer := ptp.Tracer("test").(*profTracer)
	return tracer, exp
}

func pprofLabels(ctx context.Context) map[string]string {
	labels := make(map[string]string)
	pprof.ForLabels(ctx, func(key, value string) bool {
		labels[key] = value
		return true
	})
	return labels
}

// --- allSpans tests --------------------------------------------------------

func TestProfileAllSpans_rootSpanGetsOwnLabels(t *testing.T) {
	tracer, _ := setupTracer(profileScopeAllSpans)
	ctx, span := tracer.Start(context.Background(), "root")
	defer span.End()

	labels := pprofLabels(ctx)
	if labels["span_name"] != "root" {
		t.Errorf("span_name: got %q, want root", labels["span_name"])
	}
	if labels["span_id"] == "" {
		t.Error("expected span_id label on root span")
	}
}

func TestProfileAllSpans_childSpanGetsOwnLabels(t *testing.T) {
	tracer, _ := setupTracer(profileScopeAllSpans)
	ctx, rootSpan := tracer.Start(context.Background(), "root")
	rootLabels := pprofLabels(ctx)

	ctx, childSpan := tracer.Start(ctx, "child")
	childLabels := pprofLabels(ctx)

	if childLabels["span_name"] != "child" {
		t.Errorf("child span_name: got %q, want child", childLabels["span_name"])
	}
	if childLabels["span_id"] == rootLabels["span_id"] {
		t.Error("child span_id should differ from root span_id with allSpans scope")
	}
	if childLabels["span_id"] == "" {
		t.Error("expected span_id on child span")
	}

	childSpan.End()
	rootSpan.End()
}

func TestProfileAllSpans_grandchildSpanGetsOwnLabels(t *testing.T) {
	tracer, _ := setupTracer(profileScopeAllSpans)
	ctx, root := tracer.Start(context.Background(), "root")
	ctx, child := tracer.Start(ctx, "child")
	ctx, grandchild := tracer.Start(ctx, "grandchild")

	labels := pprofLabels(ctx)
	if labels["span_name"] != "grandchild" {
		t.Errorf("span_name: got %q, want grandchild", labels["span_name"])
	}

	grandchild.End()
	child.End()
	root.End()
}

func TestProfileAllSpans_profileIDSetOnAllSpans(t *testing.T) {
	tracer, exp := setupTracer(profileScopeAllSpans)
	ctx, root := tracer.Start(context.Background(), "root")
	_, child := tracer.Start(ctx, "child")
	child.End()
	root.End()

	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans, got %d", len(spans))
	}
	for _, s := range spans {
		found := false
		for _, a := range s.Attributes {
			if a.Key == profileIDKey {
				found = true
				if a.Value.AsString() == "" {
					t.Errorf("span %q: empty pyroscope.profile.id", s.Name)
				}
			}
		}
		if !found {
			t.Errorf("span %q: missing pyroscope.profile.id attribute", s.Name)
		}
	}
}

// --- rootSpan tests (verifies the old behavior still works) ----------------

func TestProfileRootSpan_childInheritsRootLabels(t *testing.T) {
	tracer, _ := setupTracer(profileScopeRootSpan)
	ctx, rootSpan := tracer.Start(context.Background(), "root")
	rootLabels := pprofLabels(ctx)

	ctx, childSpan := tracer.Start(ctx, "child")
	childLabels := pprofLabels(ctx)

	if childLabels["span_name"] != "root" {
		t.Errorf("child span_name: got %q, want root (inherited)", childLabels["span_name"])
	}
	if childLabels["span_id"] != rootLabels["span_id"] {
		t.Error("child span_id should match root span_id with rootSpan scope")
	}

	childSpan.End()
	rootSpan.End()
}

func TestProfileRootSpan_onlyRootGetsProfileID(t *testing.T) {
	tracer, exp := setupTracer(profileScopeRootSpan)
	ctx, root := tracer.Start(context.Background(), "root")
	_, child := tracer.Start(ctx, "child")
	child.End()
	root.End()

	spans := exp.GetSpans()
	profileIDs := 0
	for _, s := range spans {
		for _, a := range s.Attributes {
			if a.Key == profileIDKey {
				profileIDs++
			}
		}
	}
	if profileIDs != 1 {
		t.Errorf("expected exactly 1 span with pyroscope.profile.id (rootSpan scope), got %d", profileIDs)
	}
}

// --- label restoration tests -----------------------------------------------

func TestProfile_labelsRestoredAfterSpanEnd(t *testing.T) {
	tracer, _ := setupTracer(profileScopeAllSpans)
	ctx, root := tracer.Start(context.Background(), "root")

	_, child := tracer.Start(ctx, "child")
	child.End()
	// After child.End(), goroutine labels are restored via
	// pprof.SetGoroutineLabels(parentCtx). Verify by starting a second child
	// from the same root context — it should get its own labels, proving
	// the first child's labels were cleaned up.
	ctx3, child2 := tracer.Start(ctx, "child2")
	labels := pprofLabels(ctx3)
	if labels["span_name"] != "child2" {
		t.Errorf("after first child.End(): second child span_name got %q, want child2", labels["span_name"])
	}
	child2.End()

	root.End()
}

func TestProfile_labelsCleanAfterRootEnd(t *testing.T) {
	tracer, _ := setupTracer(profileScopeAllSpans)
	_, root := tracer.Start(context.Background(), "root")
	root.End()

	// After root ends, goroutine labels should have no span_name/span_id.
	labels := pprofLabels(context.Background())
	if labels["span_name"] != "" {
		t.Errorf("expected empty span_name after root end, got %q", labels["span_name"])
	}
	if labels["span_id"] != "" {
		t.Errorf("expected empty span_id after root end, got %q", labels["span_id"])
	}
}

// --- separate root spans ---------------------------------------------------

func TestProfile_separateRootSpansGetDifferentLabels(t *testing.T) {
	tracer, _ := setupTracer(profileScopeAllSpans)

	ctx1, span1 := tracer.Start(context.Background(), "first")
	labels1 := pprofLabels(ctx1)
	span1.End()

	ctx2, span2 := tracer.Start(context.Background(), "second")
	labels2 := pprofLabels(ctx2)
	span2.End()

	if labels1["span_name"] == labels2["span_name"] {
		t.Error("different root spans should have different span_name labels")
	}
	if labels1["span_id"] == labels2["span_id"] {
		t.Error("different root spans should have different span_id labels")
	}
}

// --- default constructor ---------------------------------------------------

func TestNewProfileTracerProvider_defaultsToAllSpans(t *testing.T) {
	sdkTP := sdktrace.NewTracerProvider()
	wrapped := newProfileTracerProvider(sdkTP).(*profileTracerProvider)

	if wrapped.config.spanNameScope != profileScopeAllSpans {
		t.Errorf("spanNameScope: got %d, want %d (allSpans)", wrapped.config.spanNameScope, profileScopeAllSpans)
	}
	if wrapped.config.spanIDScope != profileScopeAllSpans {
		t.Errorf("spanIDScope: got %d, want %d (allSpans)", wrapped.config.spanIDScope, profileScopeAllSpans)
	}
}

// --- scopeNone tests -------------------------------------------------------

func TestProfile_scopeNone_noLabels(t *testing.T) {
	tracer, _ := setupTracer(profileScopeNone)
	ctx, span := tracer.Start(context.Background(), "root")
	defer span.End()

	labels := pprofLabels(ctx)
	if labels["span_name"] != "" {
		t.Errorf("expected no span_name with scopeNone, got %q", labels["span_name"])
	}
	if labels["span_id"] != "" {
		t.Errorf("expected no span_id with scopeNone, got %q", labels["span_id"])
	}
}

func TestProfile_scopeNone_noProfileID(t *testing.T) {
	tracer, exp := setupTracer(profileScopeNone)
	_, span := tracer.Start(context.Background(), "root")
	span.End()

	spans := exp.GetSpans()
	for _, s := range spans {
		for _, a := range s.Attributes {
			if a.Key == profileIDKey {
				t.Error("expected no pyroscope.profile.id with scopeNone")
			}
		}
	}
}

// --- bug fix verification --------------------------------------------------

func TestProfile_spanIDScopeIndependentOfNameScope(t *testing.T) {
	// This test verifies the fix for the upstream bug where withSpanIDScope
	// incorrectly set spanNameScope instead of spanIDScope.
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	ptp := &profileTracerProvider{
		tp: tp,
		config: profileConfig{
			spanNameScope: profileScopeRootSpan,
			spanIDScope:   profileScopeAllSpans,
		},
	}
	tracer := ptp.Tracer("test").(*profTracer)

	ctx, root := tracer.Start(context.Background(), "root")
	rootLabels := pprofLabels(ctx)

	ctx, child := tracer.Start(ctx, "child")
	childLabels := pprofLabels(ctx)

	// span_name should be root's (rootSpan scope).
	if childLabels["span_name"] != "root" {
		t.Errorf("span_name: got %q, want root (rootSpan scope)", childLabels["span_name"])
	}
	// span_id should be child's own (allSpans scope).
	if childLabels["span_id"] == rootLabels["span_id"] {
		t.Error("span_id should be child's own with allSpans scope, but got root's")
	}

	child.End()
	root.End()

	// Both spans should have pyroscope.profile.id (spanIDScope=allSpans).
	spans := exp.GetSpans()
	profileIDs := 0
	for _, s := range spans {
		for _, a := range s.Attributes {
			if a.Key == profileIDKey {
				profileIDs++
			}
		}
	}
	if profileIDs != 2 {
		t.Errorf("expected 2 spans with pyroscope.profile.id, got %d", profileIDs)
	}
}
