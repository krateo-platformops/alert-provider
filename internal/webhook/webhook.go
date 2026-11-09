// Package webhook serves the HyperDX webhook and the health probe on one port.
package webhook

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"

	"github.com/krateo-platformops/alert-provider/internal/incident"
	"github.com/krateo-platformops/alert-provider/internal/pyfmt"
	"github.com/krateo-platformops/alert-provider/internal/tracing"
)

// Handler answers GET /healthz with 200 and any POST, on any path, as a HyperDX notification: 202
// after logging it. HyperDX redacts the webhook URL path to /**** in its API and may deliver to a
// redacted path, so the path is not checked.
func Handler(log logging.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if r.URL.Path == "/healthz" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPost:
			raw, _ := io.ReadAll(r.Body)
			if len(raw) == 0 {
				raw = []byte("{}")
			}
			payload, err := pyfmt.Decode(raw)
			if err != nil {
				payload = map[string]any{"raw": string(raw)}
			}
			// Bound to the request's server span, when tracing is on.
			incident.Process(logging.WithContext(log, r.Context()), payload)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte("accepted"))
		default:
			http.Error(w, "Unsupported method ("+r.Method+")", http.StatusNotImplemented)
		}
	})
}

// Route is the webhook's route on its server spans: HyperDX may deliver to any path (see Handler),
// so the span is named after the endpoint, never after the path it was sent to.
const Route = "/webhook"

// Traced is h with a Server span per request when tracing is on, and h itself when it is off.
// The span is "<METHOD> /webhook", its http.route is Route, and its status is Error on a 5xx only:
// a 4xx is the client's. The readiness probe's GET /healthz gets no span. An inbound traceparent
// is honoured (the global propagator). Neither the body nor a header is recorded, and the raw path
// is dropped on export (tracing's redactor).
func Traced(h http.Handler) http.Handler {
	if !tracing.Enabled() {
		return h
	}
	traced := otelhttp.NewHandler(h, Route,
		otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/healthz" }),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return r.Method + " " + Route }),
	)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Pattern = Route // otelhttp reads http.route from it when the span starts
		traced.ServeHTTP(w, r)
	})
}

// Serve serves Handler, Traced, on addr until ctx is done.
func Serve(ctx context.Context, addr string, log logging.Logger) error {
	srv := &http.Server{Addr: addr, Handler: Traced(Handler(log)), ReadHeaderTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
