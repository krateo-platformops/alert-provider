package hyperdx

import (
	"os"
	"testing"

	"github.com/krateo-platformops/alert-provider/internal/tracing"
)

// Off, the client's transport is the default, as before tracing; on, it is wrapped.
func TestTheClientIsWrappedOnlyWhenTracingIsOn(t *testing.T) {
	for _, k := range []string{tracing.EnvEnabled, tracing.EnvTracingEnabled} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	if rt := New("http://h", "k").http.Transport; rt != nil {
		t.Errorf("off: transport %T, want nil (http.DefaultTransport)", rt)
	}
	t.Setenv(tracing.EnvTracingEnabled, "true")
	if rt := New("http://h", "k").http.Transport; rt == nil {
		t.Error("on: the transport is not wrapped")
	}
}
