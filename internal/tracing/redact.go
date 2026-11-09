package tracing

import (
	"context"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// urlAttrKeys are the span attributes that carry a whole URL: url.full, which otelhttp's client
// spans record with the query (it strips only userinfo), and http.url, its pre-1.21 name, so an
// older instrumentation cannot route around the redactor.
var urlAttrKeys = map[attribute.Key]bool{"url.full": true, "http.url": true}

// serverDroppedKeys are dropped from every Server span: client-chosen strings no view reads.
// url.path is the raw webhook path, which HyperDX may deliver redacted or with a token in it; the
// span name and http.route already say "/webhook". http.request.method_original is any token the
// client sends as its method.
var serverDroppedKeys = map[attribute.Key]bool{"url.path": true, "http.request.method_original": true}

// redactingExporter strips the query string, the fragment and the userinfo from every URL
// attribute, and the raw path from Server spans, on the way out of the process.
//
// It works at the exporter, not in a SpanProcessor's OnStart: otelhttp records url.full after the
// span has started, so only the exporter sees every span in its final state (snowplow#489).
type redactingExporter struct {
	sdktrace.SpanExporter
}

var _ sdktrace.SpanExporter = redactingExporter{}

// ExportSpans redacts, then delegates. The slice is rewritten in place: the batch processor clears
// and truncates its buffer right after ExportSpans returns (otel/sdk v1.45.0), and the simple
// processor passes a fresh one-element slice.
func (e redactingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	for i, s := range spans {
		spans[i] = redactSpan(s)
	}
	return e.SpanExporter.ExportSpans(ctx, spans)
}

// redactSpan is s with its URL attributes stripped and, on a Server span, the dropped keys
// removed; or s itself when nothing changes.
func redactSpan(s sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	if s == nil {
		return s
	}
	server := s.SpanKind() == trace.SpanKindServer
	attrs := s.Attributes()
	var out []attribute.KeyValue // nil until the first change
	for i, a := range attrs {
		drop := server && serverDroppedKeys[a.Key]
		changed := drop
		if !drop && urlAttrKeys[a.Key] && a.Value.Type() == attribute.STRING {
			if safe := StripURL(a.Value.AsString()); safe != a.Value.AsString() {
				a, changed = a.Key.String(safe), true
			}
		}
		if changed && out == nil {
			out = append(make([]attribute.KeyValue, 0, len(attrs)), attrs[:i]...)
		}
		if out != nil && !drop {
			out = append(out, a)
		}
	}
	if out == nil {
		return s
	}
	return redactedSpan{ReadOnlySpan: s, attrs: out}
}

// StripURL is raw without its query string, fragment and userinfo. An unparsable value is cut at
// its first '?' or '#', so a malformed URL cannot carry a query past it.
func StripURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		if i := strings.IndexAny(raw, "?#"); i >= 0 {
			return raw[:i]
		}
		return raw
	}
	u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment, u.User = "", false, "", "", nil
	return u.String()
}

// redactedSpan is a span with its attributes substituted; every other method is the original's.
type redactedSpan struct {
	sdktrace.ReadOnlySpan
	attrs []attribute.KeyValue
}

func (r redactedSpan) Attributes() []attribute.KeyValue { return r.attrs }
