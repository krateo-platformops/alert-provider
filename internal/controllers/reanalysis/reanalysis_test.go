package reanalysis

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestRequested(t *testing.T) {
	cases := []struct {
		name string
		spec map[string]any
		want bool
	}{
		{name: "no request", spec: map[string]any{"closed": true}, want: false},
		{name: "a request without feedback", spec: map[string]any{"reanalysis": map[string]any{}}, want: true},
		{name: "a request with feedback", spec: map[string]any{"reanalysis": map[string]any{"feedback": "x"}}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &unstructured.Unstructured{Object: map[string]any{"spec": tc.spec}}
			if got := requested(u); got != tc.want {
				t.Errorf("requested = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUnanswered(t *testing.T) {
	// Numbers in a watched object are int64, not json.Number.
	status := func(exit any) map[string]any {
		return map[string]any{"state": "Open", "lastChecks": map[string]any{
			"precondition": map[string]any{"exit": exit, "since": "2026-10-22T10:00:00Z"}}}
	}
	cases := []struct {
		name   string
		status map[string]any
		want   bool
	}{
		{name: "unknown", status: status(int64(2)), want: true},
		{name: "holds", status: status(int64(1)), want: false},
		{name: "gone", status: status(int64(0)), want: false},
		{name: "no checks", status: map[string]any{"state": "Open"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &unstructured.Unstructured{Object: map[string]any{"status": tc.status}}
			if got := unanswered(u); got != tc.want {
				t.Errorf("unanswered = %v, want %v", got, tc.want)
			}
		})
	}
}
