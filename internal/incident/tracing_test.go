package incident

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/krateo-platformops/alert-provider/internal/tracing"
	"github.com/krateo-platformops/alert-provider/internal/tracing/tracingtest"
)

// completions is a chat-completions endpoint that never touches the network: it records each
// request and answers one completion naming no incident.
type completions struct{ reqs []*http.Request }

func (c *completions) RoundTrip(r *http.Request) (*http.Response, error) {
	c.reqs = append(c.reqs, r)
	body := `{"choices":[{"message":{"content":"{\"match\": null, \"reason\": \"r\"}"}}]}`
	return &http.Response{StatusCode: 200, Header: http.Header{}, Request: r, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func comparerFor(t *testing.T, spec map[string]any, rt http.RoundTripper) *Comparer {
	t.Helper()
	mc := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kagent.dev/v1alpha2", "kind": "ModelConfig",
		"metadata": map[string]any{"name": "m", "namespace": ns}, "spec": spec}}
	return &Comparer{Reader: fake.NewClientBuilder().WithRuntimeObjects(mc).Build(), Namespace: ns, ModelConfig: "m",
		Timeout: time.Second, JWT: func(context.Context) string { return "service-jwt" }, Transport: rt}
}

func attrOf(s tracetest.SpanStub, key string) string {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value.Emit()
		}
	}
	return ""
}

// The comparison call is a client span either way; it carries traceparent toward an in-cluster
// baseUrl (the agentgateway) and not toward Gemini's host. No span carries the prompt or the key.
// MUTATION: propagating to every host fails this.
func TestTheCompareCallPropagatesOnlyInCluster(t *testing.T) {
	for _, c := range []struct {
		name      string
		spec      map[string]any
		host      string
		propagate bool
	}{
		{"in-cluster gateway", map[string]any{"provider": "OpenAI", "model": "m", "apiKeyPassthrough": true,
			"openAI": map[string]any{"baseUrl": "http://agentgateway.krateo-system.svc:8080/llm/v1"}},
			"agentgateway.krateo-system.svc", true},
		{"Gemini direct", map[string]any{"provider": "Gemini", "model": "m"}, "generativelanguage.googleapis.com", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := tracingtest.Install(t)
			rt := &completions{}
			ctx, parent := tracing.Start(context.Background(), "incident.compare")
			if _, _, err := comparerFor(t, c.spec, rt).Compare(ctx, "the prompt", names); err != nil {
				t.Fatal(err)
			}
			parent.End()
			if len(rt.reqs) != 1 || rt.reqs[0].URL.Hostname() != c.host {
				t.Fatalf("requests %v", rt.reqs)
			}
			if got := rt.reqs[0].Header.Get("traceparent") != ""; got != c.propagate {
				t.Errorf("traceparent sent %v, want %v", got, c.propagate)
			}
			kids := rec.ChildrenOf(parent.SpanContext())
			if len(kids) != 1 || kids[0].SpanKind != trace.SpanKindClient {
				t.Fatalf("children %v, want one client span", tracingtest.Names(kids))
			}
			for _, s := range rec.Spans() {
				for _, a := range s.Attributes {
					if v := a.Value.Emit(); strings.Contains(v, "the prompt") || strings.Contains(v, "service-jwt") {
						t.Errorf("span %q attribute %s: %s", s.Name, a.Key, v)
					}
				}
			}
		})
	}
}

// A firing compared with an open incident is incident.fire with incident.compare under it.
func TestTheComparisonIsUnderTheFiring(t *testing.T) {
	rec := tracingtest.Install(t)
	c := newCase(t)
	c.analyzed("inc-1", "", "")
	c.match = "inc-1"
	c.fire(fireOpt{})
	fire, cmp := rec.One("incident.fire"), rec.One("incident.compare")
	if cmp.Parent.SpanID() != fire.SpanContext.SpanID() {
		t.Error("incident.compare is not under incident.fire")
	}
	if attrOf(cmp, "incident.compare.candidates") != "1" || attrOf(cmp, "incident.compare.match") != "inc-1" ||
		attrOf(cmp, "incident.compare.cached") != "false" {
		t.Errorf("incident.compare attributes %v", cmp.Attributes)
	}
	if attrOf(fire, "incident.opened") != "false" {
		t.Errorf("incident.fire attributes %v", fire.Attributes)
	}
}

// The reanalysis' RCA runs off the reconcile: incident.analyze is a new trace linked to
// incident.reanalyze, its A2A call a client span under it.
// MUTATION: spawning RunAnalysis on run instead of tracing.Detach(run, ctx) fails this.
func TestTheReanalysisRCAIsLinkedToTheReanalysis(t *testing.T) {
	rec := tracingtest.Install(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("traceparent") == "" {
			t.Error("the A2A call carries no traceparent")
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := newCase(t)
	c.w.analyze = (&A2A{URL: srv.URL, Timeout: time.Second, JWT: func(context.Context) string { return "" }}).Analyze
	var wg sync.WaitGroup
	c.w.spawn = func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	c.analyzedIncident("Open")
	c.kube.writeSpec(ns, incName, map[string]any{"reanalysis": map[string]any{"feedback": "f"}})
	ctx, cancel := context.WithCancel(context.Background())
	if err := c.w.Reanalyze(ctx, context.Background(), ns, incName); err != nil {
		t.Fatal(err)
	}
	cancel() // the reconcile is over
	wg.Wait()

	re, an := rec.One("incident.reanalyze"), rec.One("incident.analyze")
	if an.Parent.IsValid() || an.SpanContext.TraceID() == re.SpanContext.TraceID() {
		t.Error("incident.analyze is not a new trace")
	}
	if len(an.Links) != 1 || an.Links[0].SpanContext.SpanID() != re.SpanContext.SpanID() {
		t.Fatalf("incident.analyze links %+v, want incident.reanalyze", an.Links)
	}
	kids := rec.ChildrenOf(an.SpanContext)
	if len(kids) != 1 || kids[0].SpanKind != trace.SpanKindClient {
		t.Errorf("incident.analyze children %v, want the A2A client span", tracingtest.Names(kids))
	}
	if an.Status.Code != codes.Error {
		t.Errorf("incident.analyze status %v after a failed RCA, want Error", an.Status)
	}
	if attrOf(an, "incident.name") != incName || attrOf(re, "incident.reanalysis.count") != "1" {
		t.Errorf("attributes: analyze %v, reanalyze %v", an.Attributes, re.Attributes)
	}
}
