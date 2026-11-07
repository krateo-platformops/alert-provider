package incident

import (
	"strings"
	"testing"
)

// A traces Alert's RCA starts from its spans, and says where they are.
func TestATracesRCAPromptStartsFromItsSpans(t *testing.T) {
	p := BuildPrompt("snowplow failing", "ALERT", "StatusCode = 'Error'", "m", "traces")
	if !strings.Contains(p, "It fired on spans matching `StatusCode = 'Error'` in the traces source (otel_traces) — intent: m.") {
		t.Fatalf("prompt %q", p)
	}
	if p := BuildPrompt("x", "ALERT", "w", "", "logs"); !strings.Contains(p, "It fired on log records matching `w`.") {
		t.Fatalf("log prompt %q", p)
	}
}
