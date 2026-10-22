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
