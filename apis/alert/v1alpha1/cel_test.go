package v1alpha1

import (
	"context"
	"os"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

// specSchema is the Alert CRD's spec schema, structural, as the apiserver admits a spec with it.
func specSchema(t *testing.T) *structuralschema.Structural {
	t.Helper()
	data, err := os.ReadFile("../../../helm/alert-provider-crds/templates/crd.alert.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	v1 := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	var internal apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&v1, &internal, nil); err != nil {
		t.Fatal(err)
	}
	s, err := structuralschema.NewStructural(&internal)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// admit runs the spec's x-kubernetes-validations over spec, defaulted first as the apiserver does,
// and returns the messages of the rules it breaks.
func admit(t *testing.T, s *structuralschema.Structural, spec string) []string {
	t.Helper()
	var obj map[string]any
	if err := yaml.Unmarshal([]byte(spec), &obj); err != nil {
		t.Fatal(err)
	}
	structuraldefaulting.Default(obj, s)
	errs, _ := cel.NewValidator(s, false, celconfig.PerCallLimit).
		Validate(context.Background(), field.NewPath("spec"), s, obj, nil, celconfig.RuntimeCELCostBudget)
	var out []string
	for _, e := range errs {
		out = append(out, e.Detail)
	}
	return out
}

// TestTheSourceRulesAdmitRowAlertsOnly holds spec.source's admission rules: it names the rows a
// row Alert reads, so a metric Alert refuses it, and a traces Alert must say which spans it counts.
func TestTheSourceRulesAdmitRowAlertsOnly(t *testing.T) {
	s := specSchema(t)
	const base = "interval: 5m\nthreshold: 1\nthresholdType: above\n"
	for _, c := range []struct {
		name, spec string
		refused    string // a substring of the one broken rule's message; empty when admitted
	}{
		{"a log alert without a source", "where: \"Body LIKE '%x%'\"\n", ""},
		{"a log alert naming its source", "source: logs\nwhere: \"Body LIKE '%x%'\"\n", ""},
		{"a log alert counting every row", "", ""},
		{"a traces alert counting failed spans", "source: traces\nwhere: \"StatusCode = 'Error'\"\n", ""},
		{"a traces alert aggregating span durations", "source: traces\nwhere: \"ServiceName = 'snowplow'\"\nvalue: Duration / 1e6\naggregation: p95\n", ""},
		{"a metric alert without a source", "metricRef: {name: m}\naggregation: avg\n", ""},
		{"a traces alert counting every span", "source: traces\n", "a traces alert needs a where"},
		{"a traces alert with an empty where", "source: traces\nwhere: \"\"\n", "a traces alert needs a where"},
		{"a metric alert naming a source", "source: logs\nmetricRef: {name: m}\naggregation: avg\n", "source names the rows where reads"},
		{"a metric alert naming traces", "source: traces\nwhere: x\nmetricRef: {name: m}\naggregation: avg\n", "source names the rows where reads"},
		{"a traces alert filtering labels", "source: traces\nwhere: x\nlabels: {tenant: acme}\n", "labels filter a metric's points"},
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
