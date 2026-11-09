// Package tracingtest runs the production trace pipeline over an in-memory exporter, for tests.
package tracingtest

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/krateo-platformops/alert-provider/internal/tracing"
)

// Recorder holds the spans the pipeline exported.
type Recorder struct {
	t   testing.TB
	tp  *sdktrace.TracerProvider
	exp *tracetest.InMemoryExporter
}

// Install enables tracing for the test (OTEL_TRACING_ENABLED=true) and registers, as the global
// provider and propagator, exactly what Setup registers, with the OTLP exporter swapped for an
// in-memory one: tracing.NewTracerProvider (the redacting batcher) and tracing.Propagator. Both
// are reset when the test ends. The test cannot run in parallel: the provider is global.
func Install(t testing.TB) *Recorder {
	t.Helper()
	t.Setenv(tracing.EnvTracingEnabled, "true")
	exp := tracetest.NewInMemoryExporter()
	tp := tracing.NewTracerProvider(exp)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(tracing.Propagator())
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(noop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	})
	return &Recorder{t: t, tp: tp, exp: exp}
}

// Spans flushes the pipeline and returns every span it exported, in end order.
func (r *Recorder) Spans() tracetest.SpanStubs {
	r.t.Helper()
	if err := r.tp.ForceFlush(context.Background()); err != nil {
		r.t.Fatalf("flush: %v", err)
	}
	return r.exp.GetSpans()
}

// Named is every exported span named name.
func (r *Recorder) Named(name string) []tracetest.SpanStub {
	var out []tracetest.SpanStub
	for _, s := range r.Spans() {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

// One is the one exported span named name; the test fails unless there is exactly one.
func (r *Recorder) One(name string) tracetest.SpanStub {
	r.t.Helper()
	got := r.Named(name)
	if len(got) != 1 {
		r.t.Fatalf("%d spans named %q, want 1; exported: %v", len(got), name, Names(r.Spans()))
	}
	return got[0]
}

// ChildrenOf is every exported span whose parent is parent.
func (r *Recorder) ChildrenOf(parent trace.SpanContext) []tracetest.SpanStub {
	var out []tracetest.SpanStub
	for _, s := range r.Spans() {
		if s.Parent.SpanID() == parent.SpanID() && s.Parent.TraceID() == parent.TraceID() {
			out = append(out, s)
		}
	}
	return out
}

// Names are the spans' names.
func Names(spans []tracetest.SpanStub) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name
	}
	return out
}
