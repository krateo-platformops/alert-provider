package scriptcheck

import (
	"os"
	"strings"
	"testing"
)

// cluster is the discovery of a cluster like the one the fixtures came from: core, apps, batch,
// the real apiextensions group, and two Krateo groups.
func cluster(failed ...string) *Discovery {
	return NewDiscovery([]APIResource{
		{Group: "", Version: "v1", Plural: "pods", Singular: "pod", Kind: "Pod", ShortNames: []string{"po"}},
		{Group: "", Version: "v1", Plural: "nodes", Singular: "node", Kind: "Node", ShortNames: []string{"no"}},
		{Group: "apps", Version: "v1", Plural: "deployments", Singular: "deployment", Kind: "Deployment", ShortNames: []string{"deploy"}},
		{Group: "batch", Version: "v1", Plural: "cronjobs", Singular: "cronjob", Kind: "CronJob", ShortNames: []string{"cj"}},
		{Group: "apiextensions.k8s.io", Version: "v1", Plural: "customresourcedefinitions", Singular: "customresourcedefinition",
			Kind: "CustomResourceDefinition", ShortNames: []string{"crd", "crds"}},
		{Group: "composition.krateo.io", Version: "v0-2-2", Plural: "nightlyreviews", Singular: "nightlyreview", Kind: "NightlyReview"},
		{Group: "composition.krateo.io", Version: "v0-3-683", Plural: "installers", Singular: "installer", Kind: "Installer"},
	}, failed)
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The precondition seen on a live cluster reads CRDs through an invented group: refused, once per
// line that names it, with the served name in the correction.
func TestUnservedRefusesAnInventedGroup(t *testing.T) {
	src := fixture(t, "unserved-group-precondition.sh")
	got := Unserved(map[string]any{"precondition": src, "verify": src}, cluster())
	if len(got) != 2 {
		t.Fatalf("problems = %d, want 2 (precondition and verify): %v", len(got), got)
	}
	for i, script := range []string{"precondition", "verify"} {
		p := got[i]
		if p.Script != script || p.Rule != UnservedGroup || p.Line != 3 {
			t.Errorf("problem %d = %+v, want %s line 3 %s", i, p, script, UnservedGroup)
		}
		for _, want := range []string{"customresourcedefinitions.apiextensions.krateo.io", "`apiextensions.krateo.io`",
			"customresourcedefinitions.apiextensions.k8s.io"} {
			if !strings.Contains(p.Detail, want) {
				t.Errorf("problem %d detail lacks %q: %s", i, want, p.Detail)
			}
		}
	}
}

// The negative control from the same cluster: every resource it names is served.
func TestUnservedAcceptsServedResources(t *testing.T) {
	src := fixture(t, "served-verify.sh")
	if got := Unserved(map[string]any{"precondition": src, "verify": src}, cluster()); len(got) != 0 {
		t.Errorf("a script reading only served resources was refused: %v", got)
	}
	if got := Unserved(valid, cluster()); len(got) != 0 {
		t.Errorf("the instructions' valid example was refused: %v", got)
	}
}

func TestUnservedCases(t *testing.T) {
	cases := []struct {
		name, src string
		refused   bool
	}{
		{"version.group form, served", "kubectl get deployments.v1.apps x", false},
		{"version.group form, unserved version", "kubectl get deployments.v9.apps x", true},
		{"served group, resource it does not serve", "kubectl get widgets.apps x", true},
		{"short name and kind are names too", "kubectl get deploy.apps x; kubectl get Deployment.apps y", false},
		{"bare names are never judged", "kubectl get frobs x; kubectl get po", false},
		{"type/name and comma lists", "kubectl get deployments.apps/a,widgets.nope/b", true},
		{"the name argument is not a kind", "kubectl get crd installers.composition.krateo.io -o json", false},
		{".core is CoreSuffix's, not this rule's", "kubectl get nodes.core", false},
		{"explain takes a field path", "kubectl explain pods.spec.containers", false},
		{"a non-literal kind is not guessed at", "k=x.y; kubectl get \"$k\"", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Unserved(map[string]any{"verify": c.src}, cluster())
			if (len(got) > 0) != c.refused {
				t.Errorf("refused = %v, want %v: %v", len(got) > 0, c.refused, got)
			}
		})
	}
}

// apply and rollback may create an API, and are a human's: not judged.
func TestUnservedJudgesTheChecksOnly(t *testing.T) {
	bad := "kubectl get widgets.nope x"
	if got := Unserved(map[string]any{"apply": bad, "rollback": bad}, cluster()); len(got) != 0 {
		t.Errorf("apply/rollback were judged: %v", got)
	}
}

// A group whose discovery failed is unknown, never unserved; without discovery nothing is judged.
func TestUnservedNeverGuesses(t *testing.T) {
	src := fixture(t, "unserved-group-precondition.sh")
	if got := Unserved(map[string]any{"precondition": src}, cluster("apiextensions.krateo.io")); len(got) != 0 {
		t.Errorf("a group whose discovery failed was called unserved: %v", got)
	}
	if got := Unserved(map[string]any{"precondition": src}, nil); len(got) != 0 {
		t.Errorf("nil discovery judged something: %v", got)
	}
	if got := Unserved(map[string]any{"precondition": "if then fi ("}, cluster()); len(got) != 0 {
		t.Errorf("an unparsable script got an UnservedGroup problem (BashSyntax owns it): %v", got)
	}
}
