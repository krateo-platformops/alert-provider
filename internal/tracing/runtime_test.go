package tracing

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"

	prtracing "github.com/krateo-platformops/provider-runtime/pkg/tracing"
)

// collector is an OTLP/HTTP stand-in that decodes every batch it is sent.
type collector struct {
	mu   sync.Mutex
	reqs []*coltracepb.ExportTraceServiceRequest
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	req := &coltracepb.ExportTraceServiceRequest{}
	if err := proto.Unmarshal(b, req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.reqs = append(c.reqs, req)
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

// exported is every span the collector received, by name, with its resource's attributes.
type exported struct {
	attrs, resource map[string]string
}

func (c *collector) spans() map[string]exported {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]exported{}
	str := func(kvs []*commonpb.KeyValue) map[string]string {
		m := map[string]string{}
		for _, kv := range kvs {
			m[kv.GetKey()] = kv.GetValue().GetStringValue()
		}
		return m
	}
	for _, req := range c.reqs {
		for _, rs := range req.GetResourceSpans() {
			res := str(rs.GetResource().GetAttributes())
			for _, ss := range rs.GetScopeSpans() {
				for _, s := range ss.GetSpans() {
					out[s.GetName()] = exported{attrs: str(s.GetAttributes()), resource: res}
				}
			}
		}
	}
	return out
}

// restoreGlobals puts the OTel globals and provider-runtime's provider back after the test.
func restoreGlobals(t *testing.T) {
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
		prtracing.SetTracerProvider(nil)
	})
}

// The real Setup hands its pipeline to provider-runtime: the spans provider-runtime's reconciler
// starts (StartReconcile and StartStep with prtracing.TracerProvider(), as the reconciler calls
// them) leave through the redacting exporter, under the resource that names the Deployment.
// MUTATION: dropping prtracing.SetTracerProvider from Register fails this (provider-runtime has no
// provider and starts nothing); handing provider-runtime a TracerProvider built without
// NewTracerProvider's redacting exporter fails it (url.full keeps its query); so does a resource
// without DEPLOYMENT_NAME's attributes.
func TestSetupHandsTheRedactingPipelineToProviderRuntime(t *testing.T) {
	col := &collector{}
	srv := httptest.NewServer(col)
	t.Cleanup(srv.Close)
	setOrUnset(t, EnvEnabled, "-")
	setOrUnset(t, EnvOTLPEndpoint, "-")
	setOrUnset(t, EnvServiceName, "-")
	setOrUnset(t, "OTEL_RESOURCE_ATTRIBUTES", "-")
	t.Setenv(EnvTracingEnabled, "true")
	t.Setenv(EnvOTLPTracesEndpoint, srv.URL+"/v1/traces")
	t.Setenv(EnvDeploymentName, "krateo-alert-provider")
	restoreGlobals(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdown, err := Setup(ctx, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	tp := prtracing.TracerProvider()
	if tp == nil {
		t.Fatal("Setup left provider-runtime without a TracerProvider: its reconciler would start no span")
	}
	rctx, root := prtracing.StartReconcile(ctx, tp, prtracing.Object{Kind: "Alert", Name: "a", Namespace: "ns"}, prtracing.Decision{}, time.Now())
	_, obs := prtracing.StartStep(rctx, tp, prtracing.SpanObserve)
	obs.SetAttributes(attribute.String("url.full", "https://hyperdx.example/api/v2/alerts?api_key=zq-secret"))
	obs.End()
	root.End()
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown (flush): %v", err)
	}

	spans := col.spans()
	for _, name := range []string{prtracing.SpanReconcile, prtracing.SpanObserve} {
		s, ok := spans[name]
		if !ok {
			t.Fatalf("the collector got no %s span; got %v", name, spans)
		}
		for key, want := range map[string]string{
			"service.name": DefaultServiceName, "service.version": "1.2.3",
			"k8s.deployment.name": "krateo-alert-provider", "service.instance.id": "krateo-alert-provider",
		} {
			if got := s.resource[key]; got != want {
				t.Errorf("%s resource %s = %q, want %q", name, key, got, want)
			}
		}
	}
	if got := spans[prtracing.SpanObserve].attrs["url.full"]; got != "https://hyperdx.example/api/v2/alerts" {
		t.Errorf("provider-runtime's observe url.full %q: the redacting exporter did not apply", got)
	}
}

// k8s.deployment.name and service.instance.id are DEPLOYMENT_NAME's, or absent without it;
// OTEL_RESOURCE_ATTRIBUTES wins, as the OTel spec and provider-runtime design-tracing §5 have it.
func TestTheResourceNamesTheDeployment(t *testing.T) {
	for _, c := range []struct {
		name, deployment, fromEnv string // "-" is unset
		want                      string // "" is absent
	}{
		{"DEPLOYMENT_NAME", "krateo-alert-provider", "-", "krateo-alert-provider"},
		{"unset", "-", "-", ""},
		{"OTEL_RESOURCE_ATTRIBUTES wins", "krateo-alert-provider", "k8s.deployment.name=x,service.instance.id=x", "x"},
	} {
		t.Run(c.name, func(t *testing.T) {
			setOrUnset(t, EnvDeploymentName, c.deployment)
			setOrUnset(t, "OTEL_RESOURCE_ATTRIBUTES", c.fromEnv)
			setOrUnset(t, EnvServiceName, "-")
			got := map[string]string{}
			for _, kv := range Resource(context.Background(), "v").Attributes() {
				got[string(kv.Key)] = kv.Value.Emit()
			}
			for _, key := range []string{"k8s.deployment.name", "service.instance.id"} {
				if got[key] != c.want {
					t.Errorf("%s = %q, want %q", key, got[key], c.want)
				}
			}
			if got["service.name"] != DefaultServiceName {
				t.Errorf("service.name %q", got["service.name"])
			}
		})
	}
}
