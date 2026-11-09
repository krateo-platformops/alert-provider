// Package tracing wires the alert provider's OpenTelemetry trace pipeline. It follows the platform
// contract snowplow and access-review use:
//
//	OTEL_ENABLED                        master switch                 (default false)
//	OTEL_TRACING_ENABLED                gates tracing (default: the value of OTEL_ENABLED)
//	OTEL_EXPORTER_OTLP_ENDPOINT         the collector's OTLP/HTTP base URL, e.g. http://<node>:4318
//	OTEL_EXPORTER_OTLP_TRACES_ENDPOINT  the traces URL, used as-is; wins over the base URL
//	OTEL_SERVICE_NAME                   service.name                  (default krateo-alert-provider)
//	OTEL_TRACES_SAMPLER(_ARG)           the sampler, read by the SDK itself
//
// GATING. Setup is a no-op unless tracing resolves to enabled. When it is off nothing is
// registered: the global TracerProvider stays the no-op default, Start returns spans with no
// SpanContext, and WrapTransport returns its argument unchanged, so the off path is the pre-OTel
// binary.
//
// WHAT IS SPANNED. The outbound HTTP calls (HyperDX, the RCA agent over A2A, authn, the comparison
// model, the bootstrap Job's HyperDX session), the webhook server, and the controllers' actions
// (alert.*, incident.*). The Kubernetes client is not instrumented: its watches would flood the
// pipeline with spans nobody reads.
//
// WHAT IS NEVER RECORDED. No header, body, prompt, completion or token is set as an attribute.
// The exporter strips the query string of every URL attribute (redact.go), and the transport
// injects traceparent only toward in-cluster hosts (transport.go).
package tracing

import (
	"context"
	"net/url"
	"time"

	"github.com/krateo-platformops/plumbing/env"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	// EnvEnabled is the master OTel switch. Default false.
	EnvEnabled = "OTEL_ENABLED"
	// EnvTracingEnabled gates the trace pipeline. Unset, it takes the value of EnvEnabled.
	EnvTracingEnabled = "OTEL_TRACING_ENABLED"
	// EnvOTLPEndpoint is the collector's OTLP/HTTP base URL. The traces path is appended
	// (tracesEndpointURL).
	EnvOTLPEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
	// EnvOTLPTracesEndpoint is the traces-specific OTLP/HTTP URL. When set it wins over
	// EnvOTLPEndpoint and is used as-is (OTel spec: a per-signal endpoint URL is not modified).
	EnvOTLPTracesEndpoint = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	// EnvServiceName overrides service.name.
	EnvServiceName = "OTEL_SERVICE_NAME"

	// DefaultServiceName is service.name when OTEL_SERVICE_NAME is unset.
	DefaultServiceName = "krateo-alert-provider"

	// tracesPath is the OTLP/HTTP traces path (OTLP spec, "OTLP/HTTP Request").
	tracesPath = "/v1/traces"
	// scope is the instrumentation scope of every span this module starts.
	scope = "github.com/krateo-platformops/alert-provider"
)

// ShutdownFunc flushes and stops the trace pipeline. It is never nil.
type ShutdownFunc func(context.Context) error

// Enabled reports whether tracing resolves to enabled: OTEL_TRACING_ENABLED, defaulting to
// OTEL_ENABLED, defaulting to false.
func Enabled() bool {
	return env.Bool(EnvTracingEnabled, env.Bool(EnvEnabled, false))
}

// Setup registers the OTLP/HTTP trace pipeline as the global TracerProvider, with the W3C
// TraceContext and Baggage propagators, when tracing is enabled. When it is not, it registers
// nothing and returns a no-op ShutdownFunc.
//
// build is recorded as service.version. No sampler is installed: the SDK reads
// OTEL_TRACES_SAMPLER and OTEL_TRACES_SAMPLER_ARG, which the chart sets.
func Setup(ctx context.Context, build string) (ShutdownFunc, error) {
	noop := func(context.Context) error { return nil }
	if !Enabled() {
		return noop, nil
	}
	var opts []otlptracehttp.Option
	if ep, ok := tracesEndpointURL(); ok {
		opts = append(opts, otlptracehttp.WithEndpointURL(ep))
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return noop, err
	}
	res, err := resource.New(ctx, resource.WithAttributes(
		semconv.ServiceName(env.String(EnvServiceName, DefaultServiceName)),
		semconv.ServiceVersion(build),
	))
	if err != nil && res == nil { // a partial resource with a merge error is kept
		res = resource.Default()
	}
	tp := NewTracerProvider(exp, sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(Propagator())
	return tp.Shutdown, nil
}

// Propagator is the composite W3C TraceContext + Baggage propagator Setup installs.
func Propagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
}

// NewTracerProvider is the one constructor of a TracerProvider in this module: a batcher (5s)
// over exp wrapped in the redacting exporter, so no span leaves the process with a URL query in
// it. Setup passes the OTLP exporter; tests pass an in-memory one and exercise the same pipeline.
func NewTracerProvider(exp sdktrace.SpanExporter, opts ...sdktrace.TracerProviderOption) *sdktrace.TracerProvider {
	return sdktrace.NewTracerProvider(append([]sdktrace.TracerProviderOption{
		sdktrace.WithBatcher(redactingExporter{exp}, sdktrace.WithBatchTimeout(5*time.Second)),
	}, opts...)...)
}

// tracesEndpointURL is the URL the exporter posts to, and whether one is configured at all (if
// not, Setup passes no option and the exporter keeps its own default).
//
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT, when set, is returned verbatim. Without this branch the
// WithEndpointURL option built from the generic variable would override it, since exporter
// options take precedence over the environment, and the generic variable would win against the
// OTel spec. Otherwise OTEL_EXPORTER_OTLP_ENDPOINT goes through endpointURL.
func tracesEndpointURL() (string, bool) {
	if ep := env.String(EnvOTLPTracesEndpoint, ""); ep != "" {
		return ep, true
	}
	if ep := env.String(EnvOTLPEndpoint, ""); ep != "" {
		return endpointURL(ep), true
	}
	return "", false
}

// endpointURL is the traces URL for the generic OTLP base endpoint. WithEndpointURL takes its
// URL literally (since otlptracehttp v1.45.0 an empty path posts to "/", which the collector
// answers 404: snowplow#596), so an empty or "/" path gets /v1/traces, as the OTLP spec appends
// it to the generic endpoint. A non-root path is kept: it already names the signal.
func endpointURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = tracesPath
	}
	return u.String()
}

// Tracer is the module's tracer, resolved from the global provider on every call, so it follows
// a provider registered after the caller was built.
func Tracer() trace.Tracer {
	return otel.Tracer(scope)
}

// linkKey carries, on a detached context, the span context its first span links to.
type linkKey struct{}

// Detach is the context of work that outlives the operation in from: it has long's deadline,
// cancellation and values, plus from's span context as a pending link. The first span Start
// opens on it is a new root linked to from's span, so the long work is its own trace and the
// operation's trace stays as long as the operation.
//
// Neither from's cancellation nor its deadline reaches the result: the work stops when long
// does, as it did before tracing.
func Detach(long, from context.Context) context.Context {
	sc := trace.SpanContextFromContext(from)
	if !sc.IsValid() {
		return long
	}
	return context.WithValue(long, linkKey{}, sc)
}

// Start opens a span named name on ctx. On a context from Detach that carries no span yet, the
// span is a new root linked to the span Detach was given.
func Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if sc, ok := ctx.Value(linkKey{}).(trace.SpanContext); ok && !trace.SpanContextFromContext(ctx).IsValid() {
		opts = append(opts, trace.WithNewRoot(), trace.WithLinks(trace.Link{SpanContext: sc}))
	}
	return Tracer().Start(ctx, name, opts...)
}
