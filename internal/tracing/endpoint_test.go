package tracing

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

// setOrUnset sets key to v for the test, or unsets it when v is "-"; either way it is restored
// after.
func setOrUnset(t *testing.T, key, v string) {
	t.Helper()
	t.Setenv(key, "")
	if v == "-" {
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Setenv(key, v)
}

// The chart's endpoint is a bare base URL; the exporter must post to /v1/traces on it. A
// traces-specific endpoint is used as-is and wins over the base (OTel spec).
// MUTATION: dropping the OTEL_EXPORTER_OTLP_TRACES_ENDPOINT branch of tracesEndpointURL fails this.
func TestTracesEndpointURL(t *testing.T) {
	for _, c := range []struct {
		name            string
		generic, traces string // "-" is unset
		want            string
		wantOK          bool
	}{
		{"bare base gets the traces path", "http://10.0.0.1:4318", "-", "http://10.0.0.1:4318/v1/traces", true},
		{"root path gets the traces path", "http://10.0.0.1:4318/", "-", "http://10.0.0.1:4318/v1/traces", true},
		{"https base gets the traces path", "https://collector:4318", "-", "https://collector:4318/v1/traces", true},
		{"full traces URL is not double-suffixed", "http://collector:4318/v1/traces", "-", "http://collector:4318/v1/traces", true},
		{"non-root path is kept", "http://collector:4318/custom", "-", "http://collector:4318/custom", true},
		{"host-less base is unchanged", "collector:4318", "-", "collector:4318", true},
		{"empty base is unset", "", "-", "", false},
		{"traces-specific alone is verbatim", "-", "http://t:4318", "http://t:4318", true},
		{"traces-specific wins over the base, verbatim", "http://10.0.0.1:4318", "http://t:4318/x", "http://t:4318/x", true},
		{"empty traces-specific falls back to the base", "http://10.0.0.1:4318", "", "http://10.0.0.1:4318/v1/traces", true},
		{"nothing set means no option", "-", "-", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			setOrUnset(t, EnvOTLPEndpoint, c.generic)
			setOrUnset(t, EnvOTLPTracesEndpoint, c.traces)
			got, ok := tracesEndpointURL()
			if got != c.want || ok != c.wantOK {
				t.Errorf("tracesEndpointURL() = (%q, %v), want (%q, %v)", got, ok, c.want, c.wantOK)
			}
		})
	}
}

// The real Setup and OTLP/HTTP exporter against a collector stand-in: the span batch is POSTed to
// the path the environment names.
// MUTATION: dropping the OTEL_EXPORTER_OTLP_TRACES_ENDPOINT branch of tracesEndpointURL fails the
// precedence case (the batch goes to /v1/traces on the base instead).
func TestSetupExportsToTheConfiguredPath(t *testing.T) {
	for _, c := range []struct {
		name            string
		generic, traces string // a path appended to the server URL; "-" leaves the variable unset
		wantPath        string
	}{
		{"bare base", "", "-", "/v1/traces"},
		{"traces-specific alone", "-", "/custom/traces", "/custom/traces"},
		{"both set: traces-specific wins", "", "/custom/traces", "/custom/traces"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var (
				mu    sync.Mutex
				paths []string
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				mu.Lock()
				paths = append(paths, r.Method+" "+r.URL.Path)
				mu.Unlock()
				if r.URL.Path != c.wantPath {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)

			setOrUnset(t, EnvEnabled, "-")
			t.Setenv(EnvTracingEnabled, "true")
			for key, path := range map[string]string{EnvOTLPEndpoint: c.generic, EnvOTLPTracesEndpoint: c.traces} {
				if path == "-" {
					setOrUnset(t, key, "-")
				} else {
					t.Setenv(key, srv.URL+path)
				}
			}
			prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
			t.Cleanup(func() {
				otel.SetTracerProvider(prevTP)
				otel.SetTextMapPropagator(prevProp)
			})

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			shutdown, err := Setup(ctx, "test")
			if err != nil {
				t.Fatalf("Setup: %v", err)
			}
			_, span := Start(ctx, "probe")
			span.End()
			// Shutdown flushes the batcher: the export has happened (or failed) when it returns.
			if err := shutdown(ctx); err != nil {
				t.Fatalf("shutdown (flush): %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if want := http.MethodPost + " " + c.wantPath; len(paths) == 0 || paths[0] != want {
				t.Fatalf("the collector saw %v, want first %q", paths, want)
			}
		})
	}
}
