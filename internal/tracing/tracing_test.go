package tracing_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/krateo-platformops/alert-provider/internal/tracing"
	"github.com/krateo-platformops/alert-provider/internal/tracing/tracingtest"
)

// unset clears key for the test, restoring it after.
func unset(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

// capture is a base transport that records each request and answers 200 with body, so a wrapped
// transport can be checked without a network.
type capture struct {
	reqs []*http.Request
	body string
}

func (c *capture) RoundTrip(r *http.Request) (*http.Response, error) {
	c.reqs = append(c.reqs, r)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: r,
		Body: io.NopCloser(strings.NewReader(c.body))}, nil
}

// MUTATION: tracing enabled by default (Enabled's last fallback true) fails this.
func TestOffByDefaultRegistersNothingAndWrapsNothing(t *testing.T) {
	unset(t, tracing.EnvEnabled)
	unset(t, tracing.EnvTracingEnabled)
	if tracing.Enabled() {
		t.Fatal("tracing is on with neither OTEL_ENABLED nor OTEL_TRACING_ENABLED set")
	}
	before := otel.GetTracerProvider()
	shutdown, err := tracing.Setup(context.Background(), "v")
	if err != nil || shutdown == nil {
		t.Fatalf("Setup: %v, shutdown nil %v", err, shutdown == nil)
	}
	if otel.GetTracerProvider() != before {
		t.Error("Setup registered a TracerProvider while off")
	}
	_, span := tracing.Start(context.Background(), "x")
	if span.SpanContext().IsValid() || span.IsRecording() {
		t.Error("the tracer is not a no-op while off")
	}
	rt := &capture{}
	if got := tracing.WrapTransport(rt); got != http.RoundTripper(rt) {
		t.Errorf("WrapTransport wrapped while off: %T", got)
	}
	if got := tracing.WrapTransport(nil); got != nil {
		t.Errorf("WrapTransport(nil) = %T while off, want nil (http.DefaultTransport)", got)
	}
}

func TestTheGate(t *testing.T) {
	for _, c := range []struct {
		master, signal string // "-" is unset
		want           bool
	}{
		{"-", "-", false},
		{"true", "-", true},
		{"true", "false", false},
		{"false", "true", true},
		{"-", "true", true},
	} {
		for key, v := range map[string]string{tracing.EnvEnabled: c.master, tracing.EnvTracingEnabled: c.signal} {
			if v == "-" {
				unset(t, key)
			} else {
				t.Setenv(key, v)
			}
		}
		if got := tracing.Enabled(); got != c.want {
			t.Errorf("OTEL_ENABLED=%s OTEL_TRACING_ENABLED=%s: enabled %v, want %v", c.master, c.signal, got, c.want)
		}
	}
}

// The redacting exporter, on the production pipeline: a client span's url.full keeps its path and
// loses its query, whatever the query holds.
func TestTheExporterStripsQueryStrings(t *testing.T) {
	rec := tracingtest.Install(t)
	const sentinel = "zq-api-key-not-a-jwt"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v2/alerts?api_key="+sentinel+"&page=2#frag", nil)
	resp, err := (&http.Client{Transport: tracing.WrapTransport(nil)}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	spans := rec.Spans()
	saw := false
	for _, s := range spans {
		for _, a := range s.Attributes {
			v := a.Value.Emit()
			if strings.Contains(v, sentinel) || strings.Contains(v, "page=") {
				t.Errorf("span %q attribute %s carries the query: %s", s.Name, a.Key, v)
			}
			if a.Key == "url.full" {
				saw = true
				if v != srv.URL+"/api/v2/alerts" {
					t.Errorf("url.full %q, want the URL without its query", v)
				}
			}
		}
	}
	if !saw {
		t.Fatalf("no url.full on %d spans: the check proved nothing", len(spans))
	}
}

func TestStripURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://u:p@h.example/a/b?token=x#f": "https://h.example/a/b",
		"http://h:8000/api":                   "http://h:8000/api",
		"http://h/?":                          "http://h/",
		"%zz?token=x":                         "%zz",
	} {
		if got := tracing.StripURL(in); got != want {
			t.Errorf("StripURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInCluster(t *testing.T) {
	for host, want := range map[string]bool{
		"incident-agent":                          true,
		"incident-agent.krateo-system.svc":        true,
		"authn.krateo-system.svc.cluster.local.":  true,
		"krateo-clickstack-api.krateo-system.svc": true,
		"127.0.0.1":                         true,
		"10.4.0.12":                         true,
		"::1":                               true,
		"generativelanguage.googleapis.com": false,
		"api.openai.com":                    false,
		"8.8.8.8":                           false,
		"svc.example.com":                   false,
		"":                                  false,
	} {
		if got := tracing.InCluster(host); got != want {
			t.Errorf("InCluster(%q) = %v, want %v", host, got, want)
		}
	}
}

// Both kinds of host get a client span; only the in-cluster one gets traceparent and baggage.
// MUTATION: propagating to every host (RoundTrip always through inCluster) fails this.
func TestTraceparentGoesOnlyToInClusterHosts(t *testing.T) {
	rec := tracingtest.Install(t)
	base := &capture{}
	client := &http.Client{Transport: tracing.WrapTransport(base)}
	ctx, parent := tracing.Start(context.Background(), "op")
	for _, u := range []string{"http://incident-agent.krateo-system.svc:8080/", "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	parent.End()
	if len(base.reqs) != 2 {
		t.Fatalf("%d requests", len(base.reqs))
	}
	if base.reqs[0].Header.Get("traceparent") == "" {
		t.Error("no traceparent toward the in-cluster host")
	}
	if h := base.reqs[1].Header; h.Get("traceparent") != "" || h.Get("baggage") != "" {
		t.Errorf("trace headers toward an external host: %v", h)
	}
	clients := 0
	for _, s := range rec.ChildrenOf(parent.SpanContext()) {
		if s.SpanKind == trace.SpanKindClient {
			clients++
		}
	}
	if clients != 2 {
		t.Errorf("%d client spans under the caller, want one per request", clients)
	}
}

// Detach keeps the long context's lifetime and links its first span to the operation's.
func TestDetachLinksAndKeepsTheLongLifetime(t *testing.T) {
	rec := tracingtest.Install(t)
	long, stop := context.WithCancel(context.Background())
	defer stop()
	opCtx, cancelOp := context.WithCancel(context.Background())
	opCtx, op := tracing.Start(opCtx, "op")
	detached := tracing.Detach(long, opCtx)
	op.End()
	cancelOp()
	if detached.Err() != nil {
		t.Fatal("the operation's cancellation reached the detached context")
	}
	ctx, async := tracing.Start(detached, "async")
	_, child := tracing.Start(ctx, "child")
	child.End()
	async.End()
	got := rec.One("async")
	if got.Parent.IsValid() {
		t.Error("the async span has a parent; want a new root")
	}
	if got.SpanContext.TraceID() == op.SpanContext().TraceID() {
		t.Error("the async span is in the operation's trace")
	}
	if len(got.Links) != 1 || got.Links[0].SpanContext.SpanID() != op.SpanContext().SpanID() {
		t.Errorf("links %v, want the operation's span", got.Links)
	}
	if c := rec.One("child"); len(c.Links) != 0 || c.Parent.SpanID() != got.SpanContext.SpanID() {
		t.Errorf("the child is linked again or not under the async span: %+v", c.Links)
	}
	stop()
	if detached.Err() == nil {
		t.Error("the long context's cancellation did not reach the detached one")
	}
}
