package tracing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func withRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})
	return sr
}

func attrValue(span sdktrace.ReadOnlySpan, key attribute.Key) (attribute.Value, bool) {
	for _, kv := range span.Attributes() {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestTransport_recordsSpanOnSuccess(t *testing.T) {
	sr := withRecorder(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)

	client := &http.Client{Transport: NewTransport(nil, "test-tracer")}
	resp, err := client.Get(ts.URL + "/some/path")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	span := spans[0]
	if span.Name() != "HTTP GET" {
		t.Errorf("span name: got %q, want HTTP GET", span.Name())
	}
	if span.SpanKind() != trace.SpanKindClient {
		t.Errorf("span kind: got %v, want client", span.SpanKind())
	}
	if v, ok := attrValue(span, "http.response.status_code"); !ok || v.AsInt64() != 200 {
		t.Errorf("status_code attr: got %v (present=%v), want 200", v.AsInt64(), ok)
	}
	if v, ok := attrValue(span, "url.path"); !ok || v.AsString() != "/some/path" {
		t.Errorf("url.path attr: got %q (present=%v), want /some/path", v.AsString(), ok)
	}
	if span.Status().Code == codes.Error {
		t.Error("success must not set error status")
	}
}

func TestTransport_marksHTTPErrorStatus(t *testing.T) {
	sr := withRecorder(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	t.Cleanup(ts.Close)

	client := &http.Client{Transport: NewTransport(nil, "test-tracer")}
	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].Status().Code != codes.Error {
		t.Error("4xx response must set error status on the span")
	}
	if v, ok := attrValue(spans[0], "http.response.status_code"); !ok || v.AsInt64() != 422 {
		t.Errorf("status_code attr: got %v, want 422", v.AsInt64())
	}
}

func TestTransport_recordsTransportError(t *testing.T) {
	sr := withRecorder(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ts.Close()

	client := &http.Client{Transport: NewTransport(nil, "test-tracer")}
	_, err := client.Get(ts.URL)
	if err == nil {
		t.Fatal("expected a connection error")
	}

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].Status().Code != codes.Error {
		t.Error("transport error must set error status")
	}
	if len(spans[0].Events()) == 0 {
		t.Error("expected a recorded error event")
	}
}

func TestTransport_childOfRequestContextSpan(t *testing.T) {
	sr := withRecorder(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(ts.Close)

	ctx, parent := otel.Tracer("test-tracer").Start(context.Background(), "parent-op")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	client := &http.Client{Transport: NewTransport(nil, "test-tracer")}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()
	parent.End()

	spans := sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	httpSpan := spans[0]
	if httpSpan.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Error("HTTP span must be a child of the request context span")
	}
	if httpSpan.SpanContext().TraceID() != parent.SpanContext().TraceID() {
		t.Error("HTTP span must share the parent's trace ID")
	}
}
