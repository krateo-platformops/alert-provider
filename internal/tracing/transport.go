package tracing

import (
	"net"
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
)

// WrapTransport is rt with a client span per request when tracing is enabled, and rt unchanged
// when it is not (a nil rt stays nil, which http.Client reads as http.DefaultTransport).
//
// PROPAGATION IS SCOPED. traceparent and baggage are injected only toward an in-cluster host (see
// InCluster). A request to any other host, such as Gemini's or OpenAI's API, still gets its
// client span, but carries no trace header: the trace and its baggage are this platform's, not a
// third party's.
func WrapTransport(rt http.RoundTripper) http.RoundTripper {
	if !Enabled() {
		return rt
	}
	if rt == nil {
		rt = http.DefaultTransport
	}
	return &scopedTransport{
		inCluster: otelhttp.NewTransport(rt),
		external:  otelhttp.NewTransport(rt, otelhttp.WithPropagators(propagation.NewCompositeTextMapPropagator())),
	}
}

// scopedTransport sends each request through the instrumented transport its host is allowed: the
// one that propagates for an in-cluster host, the one that does not for any other.
type scopedTransport struct {
	inCluster, external http.RoundTripper
}

func (t *scopedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if InCluster(r.URL.Hostname()) {
		return t.inCluster.RoundTrip(r)
	}
	return t.external.RoundTrip(r)
}

// clusterSuffixes are the DNS suffixes of a cluster's Services: <svc>.<ns>.svc, and the same under
// the default cluster domain.
var clusterSuffixes = []string{".svc", ".svc.cluster.local", ".cluster.local"}

// InCluster says host is a cluster-internal address the trace may be propagated to: a Service
// name (a bare name resolved by the pod's search domains, or one under .svc / .cluster.local), or
// a loopback or private IP (a ClusterIP, a pod IP). Every other name is external.
func InCluster(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == "" {
		return false
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate()
	}
	if !strings.Contains(h, ".") {
		return true
	}
	for _, s := range clusterSuffixes {
		if strings.HasSuffix(h, s) {
			return true
		}
	}
	return false
}
