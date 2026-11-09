package webhook

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"

	"github.com/krateo-platformops/alert-provider/internal/tracing"
	"github.com/krateo-platformops/alert-provider/internal/tracing/tracingtest"
)

func serve(h http.Handler, method, path string) int {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(`{"state":"ALERT"}`)))
	return w.Code
}

func attr(attrs []attribute.KeyValue, key string) (string, bool) {
	for _, a := range attrs {
		if string(a.Key) == key {
			return a.Value.Emit(), true
		}
	}
	return "", false
}

// A POST, on any path, is one Server span "POST /webhook"; the probe gets none. A 5xx is Error,
// a 2xx is not. The raw path never leaves the process.
// MUTATION: dropping the /healthz filter fails this.
func TestTheWebhookPOSTIsAServerSpanAndTheProbeIsNot(t *testing.T) {
	rec := tracingtest.Install(t)
	h := Traced(Handler(logging.NewNopLogger()))
	if code := serve(h, http.MethodGet, "/healthz"); code != 200 {
		t.Fatalf("healthz %d", code)
	}
	if code := serve(h, http.MethodPost, "/s3cr3t-path"); code != 202 {
		t.Fatalf("POST %d", code)
	}
	if code := serve(h, http.MethodPut, "/webhook"); code != 501 {
		t.Fatalf("PUT %d", code)
	}
	spans := rec.Spans()
	if len(spans) != 2 {
		t.Fatalf("spans %v, want the POST's and the PUT's only", tracingtest.Names(spans))
	}
	post, put := rec.One("POST /webhook"), rec.One("PUT /webhook")
	if post.SpanKind != trace.SpanKindServer {
		t.Errorf("kind %v", post.SpanKind)
	}
	if r, _ := attr(post.Attributes, "http.route"); r != Route {
		t.Errorf("http.route %q", r)
	}
	if post.Status.Code == codes.Error {
		t.Error("a 202 is Error")
	}
	if put.Status.Code != codes.Error {
		t.Errorf("a 501 is %v, want Error", put.Status.Code)
	}
	for _, s := range spans {
		for _, a := range s.Attributes {
			if strings.Contains(a.Value.Emit(), "s3cr3t") || strings.Contains(a.Value.Emit(), "ALERT") {
				t.Errorf("span %q attribute %s carries the path or the body: %s", s.Name, a.Key, a.Value.Emit())
			}
		}
	}
}

type probe struct{}

func (*probe) ServeHTTP(http.ResponseWriter, *http.Request) {}

func TestOffTheHandlerIsUnwrapped(t *testing.T) {
	for _, k := range []string{tracing.EnvEnabled, tracing.EnvTracingEnabled} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	p := &probe{}
	if got := Traced(p); got != http.Handler(p) {
		t.Errorf("Traced wrapped while off: %T", got)
	}
}
