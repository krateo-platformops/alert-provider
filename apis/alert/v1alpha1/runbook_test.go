package v1alpha1

import (
	"strings"
	"testing"
)

// TestTheRunbookIsOptionalAndBounded holds spec.runbook's admission: optional, at most 3072
// characters, and with the description at most HyperDX's 4096 for a notification text (the
// description, a blank line, the runbook).
func TestTheRunbookIsOptionalAndBounded(t *testing.T) {
	s := specSchema(t)
	const base = "interval: 5m\nthreshold: 1\nthresholdType: above\nwhere: x\n"
	quoted := func(field string, n int) string { return field + ": " + strings.Repeat("a", n) + "\n" }
	for _, c := range []struct {
		name, spec string
		refused    string // a substring of a refusal; empty when admitted
	}{
		{"no runbook", quoted("description", 40), ""},
		{"no runbook nor description", "", ""},
		{"a markdown runbook", "description: The audit disk is filling up.\nrunbook: |\n  Free space by dropping expired telemetry partitions.\n\n  **Never** drop audit partitions.\n", ""},
		{"an empty runbook", "runbook: \"\"\n", ""},
		{"a runbook of 3072 without a description", quoted("runbook", 3072), ""},
		{"a runbook of 3073", quoted("runbook", 3073), "may not be more than 3072"},
		{"description and runbook of exactly 4096", quoted("description", 1022) + quoted("runbook", 3072), ""},
		{"description and runbook over 4096", quoted("description", 1023) + quoted("runbook", 3072),
			"description and runbook together exceed HyperDX's 4096 characters"},
		{"a long description with an empty runbook", quoted("description", 4000) + "runbook: \"\"\n", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := admit(t, s, base+c.spec)
			if c.refused == "" {
				if len(got) != 0 {
					t.Fatalf("refused: %v", got)
				}
				return
			}
			found := false
			for _, m := range got {
				found = found || strings.Contains(m, c.refused)
			}
			if !found {
				t.Fatalf("want a refusal containing %q, got %v", c.refused, got)
			}
		})
	}
}
