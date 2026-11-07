package compare

import (
	"strings"
	"testing"
)

// A traces Alert's comparison names what it counts: spans, not log records.
func TestATracesPromptCountsSpans(t *testing.T) {
	p := Prompt(Alert{Name: "a", Where: "StatusCode = 'Error'", Source: Traces}, nil, []Row{{Record: "snowplow krateo-system: GET /call Error timeout", Count: 3}})
	if !strings.Contains(p, "counts the spans matching `StatusCode = 'Error'`") || strings.Contains(p, "log records") {
		t.Fatalf("prompt %q", p)
	}
	if p := Prompt(Alert{Name: "a", Where: "w"}, nil, nil); !strings.Contains(p, "counts the log records matching `w`") {
		t.Fatalf("log prompt %q", p)
	}
}

func TestTheGroupFollowsTheSource(t *testing.T) {
	if GroupOf(Traces) != TraceRowGroup || GroupOf("") != RowGroup || GroupOf("logs") != RowGroup {
		t.Fatal("group")
	}
	// HyperDX nulls a group whose expression holds `[` or `=`.
	if strings.ContainsAny(TraceRowGroup, "[=") {
		t.Fatalf("TraceRowGroup %q", TraceRowGroup)
	}
}
