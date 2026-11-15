package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// renderRBAC renders the chart (CHART_VERSION -> 0.0.0, as render does) and returns its
// ClusterRoles and ClusterRoleBindings by name. err is the helm error, for schema refusals.
func renderRBAC(t *testing.T, args ...string) (map[string]rbacv1.ClusterRole, map[string]rbacv1.ClusterRoleBinding, error) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatal("helm is not on PATH in CI")
		}
		t.Skip("helm is not on PATH: the chart render test did not run")
	}
	dst := filepath.Join(t.TempDir(), "alert-provider")
	if out, err := exec.Command("cp", "-R", chartDir, dst).CombinedOutput(); err != nil {
		t.Fatalf("copy chart: %v: %s", err, out)
	}
	raw, _ := os.ReadFile(filepath.Join(dst, "Chart.yaml"))
	_ = os.WriteFile(filepath.Join(dst, "Chart.yaml"), bytes.ReplaceAll(raw, []byte("CHART_VERSION"), []byte("0.0.0")), 0o600)
	out, err := exec.Command(helm, append([]string{"template", "t", dst, "--namespace", "krateo-system"}, args...)...).CombinedOutput()
	if err != nil {
		return nil, nil, err
	}
	roles, bindings := map[string]rbacv1.ClusterRole{}, map[string]rbacv1.ClusterRoleBinding{}
	for _, doc := range strings.Split(string(out), "\n---") {
		var head struct{ Kind string }
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			t.Fatalf("rendered document does not parse: %v\n%s", err, doc)
		}
		switch head.Kind {
		case "ClusterRole":
			var r rbacv1.ClusterRole
			if err := yaml.Unmarshal([]byte(doc), &r); err != nil {
				t.Fatal(err)
			}
			roles[r.Name] = r
		case "ClusterRoleBinding":
			var b rbacv1.ClusterRoleBinding
			if err := yaml.Unmarshal([]byte(doc), &b); err != nil {
				t.Fatal(err)
			}
			bindings[b.Name] = b
		}
	}
	return roles, bindings, nil
}

const crdReader = "krateo-alert-crd-reader"

// By default no group gets the CRD read: nothing named krateo-alert-crd-reader renders.
// MUTATION: a non-empty crdReaderGroups default in values.yaml fails this.
func TestNoCRDReadByDefault(t *testing.T) {
	roles, bindings, err := renderRBAC(t)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := roles[crdReader]; ok {
		t.Error("the CRD reader ClusterRole renders by default")
	}
	if _, ok := bindings[crdReader]; ok {
		t.Error("the CRD reader ClusterRoleBinding renders by default")
	}
	if _, ok := roles["krateo-alert-viewer"]; !ok {
		t.Error("control: krateo-alert-viewer did not render, so this render saw nothing")
	}
}

// A listed group gets get on the Alert CRD by name, and nothing else.
// MUTATION: dropping resourceNames, or adding list/watch, fails this.
func TestCRDReaderGroupsGetOnlyTheAlertCRD(t *testing.T) {
	roles, bindings, err := renderRBAC(t, "--set", "crdReaderGroups={sre,oncall}")
	if err != nil {
		t.Fatal(err)
	}
	want := []rbacv1.PolicyRule{{
		APIGroups:     []string{"apiextensions.k8s.io"},
		Resources:     []string{"customresourcedefinitions"},
		ResourceNames: []string{"alerts.observability.krateo.io"},
		Verbs:         []string{"get"},
	}}
	if got := roles[crdReader].Rules; !reflect.DeepEqual(got, want) {
		t.Errorf("rules = %+v, want %+v", got, want)
	}
	b := bindings[crdReader]
	if b.RoleRef.Kind != "ClusterRole" || b.RoleRef.Name != crdReader {
		t.Errorf("roleRef = %+v", b.RoleRef)
	}
	var groups []string
	for _, s := range b.Subjects {
		if s.Kind != "Group" || s.APIGroup != "rbac.authorization.k8s.io" {
			t.Errorf("subject %+v is not a Group", s)
		}
		groups = append(groups, s.Name)
	}
	if !reflect.DeepEqual(groups, []string{"sre", "oncall"}) {
		t.Errorf("subjects = %v, want [sre oncall]", groups)
	}
}

// The schema refuses an empty group name, a repeated group and a non-list.
func TestTheSchemaRefusesMalformedCRDReaderGroups(t *testing.T) {
	for _, args := range [][]string{
		{"--set-json", `crdReaderGroups=[""]`},
		{"--set-json", `crdReaderGroups=["sre","sre"]`},
		{"--set-json", `crdReaderGroups="sre"`},
		{"--set-json", `crdReaderGroups=[1]`},
	} {
		if _, _, err := renderRBAC(t, args...); err == nil {
			t.Errorf("%v rendered", args)
		}
	}
}
