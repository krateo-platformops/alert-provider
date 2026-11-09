package tracing

import "testing"

// The chart's endpoint is a bare base URL; the exporter must post to /v1/traces on it.
func TestTheEndpointGetsTheTracesPath(t *testing.T) {
	for in, want := range map[string]string{
		"http://10.0.0.1:4318":            "http://10.0.0.1:4318/v1/traces",
		"http://10.0.0.1:4318/":           "http://10.0.0.1:4318/v1/traces",
		"http://collector:4318/v1/traces": "http://collector:4318/v1/traces",
		"collector:4318":                  "collector:4318",
	} {
		if got := endpointURL(in); got != want {
			t.Errorf("endpointURL(%q) = %q, want %q", in, got, want)
		}
	}
}
