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
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/listtype"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

// specValidators are the Alert CRD's spec schema, structural, as the apiserver admits a spec with
// it, and the OpenAPI validator of its keywords (minimum, pattern, maxItems...).
type specValidators struct {
	structural *structuralschema.Structural
	openapi    apiservervalidation.SchemaValidator
}

// specSchema reads the spec's validators off the CRD.
func specSchema(t *testing.T) specValidators {
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
	v, _, err := apiservervalidation.NewSchemaValidator(&internal)
	if err != nil {
		t.Fatal(err)
	}
	return specValidators{structural: s, openapi: v}
}

// admit validates spec as the apiserver admits it, defaulted first: the schema's keywords, its
// list types (a set's duplicates), then its x-kubernetes-validations. It returns the messages of
// what it breaks.
func admit(t *testing.T, sc specValidators, spec string) []string {
	t.Helper()
	s := sc.structural
	var obj map[string]any
	if err := yaml.Unmarshal([]byte(spec), &obj); err != nil {
		t.Fatal(err)
	}
	structuraldefaulting.Default(obj, s)
	path := field.NewPath("spec")
	errs := apiservervalidation.ValidateCustomResource(path, obj, sc.openapi)
	errs = append(errs, listtype.ValidateListSetsAndMaps(path, s, obj)...)
	celErrs, _ := cel.NewValidator(s, false, celconfig.PerCallLimit).
		Validate(context.Background(), path, s, obj, nil, celconfig.RuntimeCELCostBudget)
	errs = append(errs, celErrs...)
	var out []string
	for _, e := range errs {
		out = append(out, e.Error())
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

// TestTheGroupRulesAdmitGroupedMetricAlertsOnly holds spec.groupBy's and spec.consecutiveWindows'
// admission rules: groupBy splits a metric's series, never a ratio's, over a few distinct
// attribute keys; consecutiveWindows is 1 to 60 intervals, on any Alert.
func TestTheGroupRulesAdmitGroupedMetricAlertsOnly(t *testing.T) {
	s := specSchema(t)
	const metric = "interval: 5m\nthreshold: 1\nthresholdType: below\nmetricRef: {name: m}\naggregation: max\n"
	for _, c := range []struct {
		name, spec string
		refused    string // a substring of a refusal; empty when admitted
	}{
		// The observability fixture: one series per container, ALERT after 5 not-ready minutes.
		{"the per-container readiness alert", "interval: 1m\nthreshold: 1\nthresholdType: below\n" +
			"metricRef: {name: k8s-container-ready}\naggregation: max\nconsecutiveWindows: 5\n" +
			"groupBy: [k8s.namespace.name, k8s.pod.name, k8s.container.name]\n", ""},
		{"a grouped metric alert", metric + "groupBy: [k8s.pod.name]\n", ""},
		{"five keys", metric + "groupBy: [a, b, c, d, e]\n", ""},
		{"consecutiveWindows on a log alert", "interval: 5m\nthreshold: 1\nthresholdType: above\nwhere: x\nconsecutiveWindows: 3\n", ""},
		{"consecutiveWindows 60", metric + "consecutiveWindows: 60\n", ""},
		{"groupBy on a log alert", "interval: 5m\nthreshold: 1\nthresholdType: above\nwhere: x\ngroupBy: [k8s.pod.name]\n",
			"groupBy splits a metric's series; it requires metricRef"},
		{"groupBy on a traces alert", "interval: 5m\nthreshold: 1\nthresholdType: above\nsource: traces\nwhere: x\ngroupBy: [ServiceName]\n",
			"groupBy splits a metric's series; it requires metricRef"},
		{"groupBy with ratio", "interval: 5m\nthreshold: 0.01\nthresholdType: above\nmetricRef: {name: m}\naggregation: sum\n" +
			"labels: {failed: \"true\"}\nratio: {labels: {}}\ngroupBy: [service.name]\n", "groupBy and ratio are mutually exclusive"},
		{"a key that is not an attribute name", metric + "groupBy: [\"k8s.pod.name) OR (1\"]\n", "should match"},
		{"a key with a quote", metric + "groupBy: [\"a'b\"]\n", "should match"},
		{"an empty key", metric + "groupBy: [\"\"]\n", "at least 1 chars long"},
		{"no key", metric + "groupBy: []\n", "should have at least 1 items"},
		{"six keys", metric + "groupBy: [a, b, c, d, e, f]\n", "must have at most 5 items"},
		{"a duplicate key", metric + "groupBy: [k8s.pod.name, k8s.pod.name]\n", "Duplicate value"},
		{"consecutiveWindows 0", metric + "consecutiveWindows: 0\n", "should be greater than or equal to 1"},
		{"consecutiveWindows 61", metric + "consecutiveWindows: 61\n", "should be less than or equal to 60"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := admit(t, s, c.spec)
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
