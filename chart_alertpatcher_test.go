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

// renderNamespacedRBAC renders the chart into namespace ns and returns its Roles, RoleBindings
// and the names of every cluster-scoped RBAC object. err is the helm error, for schema refusals.
func renderNamespacedRBAC(t *testing.T, ns string, args ...string) (map[string]rbacv1.Role, map[string]rbacv1.RoleBinding, []string, error) {
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
	out, err := exec.Command(helm, append([]string{"template", "t", dst, "--namespace", ns}, args...)...).CombinedOutput()
	if err != nil {
		return nil, nil, nil, err
	}
	roles, bindings := map[string]rbacv1.Role{}, map[string]rbacv1.RoleBinding{}
	var cluster []string
	for _, doc := range strings.Split(string(out), "\n---") {
		var head struct {
			Kind     string
			Metadata struct{ Name string }
		}
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			t.Fatalf("rendered document does not parse: %v\n%s", err, doc)
		}
		switch head.Kind {
		case "Role":
			var r rbacv1.Role
			if err := yaml.Unmarshal([]byte(doc), &r); err != nil {
				t.Fatal(err)
			}
			roles[r.Name] = r
		case "RoleBinding":
			var b rbacv1.RoleBinding
			if err := yaml.Unmarshal([]byte(doc), &b); err != nil {
				t.Fatal(err)
			}
			bindings[b.Name] = b
		case "ClusterRole", "ClusterRoleBinding":
			cluster = append(cluster, head.Kind+"/"+head.Metadata.Name)
		}
	}
	return roles, bindings, cluster, nil
}

const alertPatcher = "krateo-alert-patcher"

// By default no group may patch Alerts: nothing named krateo-alert-patcher renders.
// MUTATION: a non-empty alertPatcherGroups default in values.yaml fails this.
func TestNoAlertPatchByDefault(t *testing.T) {
	roles, bindings, cluster, err := renderNamespacedRBAC(t, "krateo-system")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := roles[alertPatcher]; ok {
		t.Error("the alert patcher Role renders by default")
	}
	if _, ok := bindings[alertPatcher]; ok {
		t.Error("the alert patcher RoleBinding renders by default")
	}
	if len(cluster) == 0 {
		t.Error("control: no cluster-scoped RBAC rendered at all, so this render saw nothing")
	}
}

// A listed group may patch Alerts in the release namespace, and do nothing else, through a Role
// (never a ClusterRole).
// MUTATION: adding update/delete, widening resources, or making it a ClusterRole fails this.
func TestAlertPatcherGroupsPatchOnlyInTheReleaseNamespace(t *testing.T) {
	roles, bindings, cluster, err := renderNamespacedRBAC(t, "obs-ns", "--set", "alertPatcherGroups={sre,oncall}")
	if err != nil {
		t.Fatal(err)
	}
	r, ok := roles[alertPatcher]
	if !ok {
		t.Fatal("no krateo-alert-patcher Role")
	}
	if r.Namespace != "obs-ns" {
		t.Errorf("Role namespace = %q, want the release namespace obs-ns", r.Namespace)
	}
	want := []rbacv1.PolicyRule{{
		APIGroups: []string{"observability.krateo.io"},
		Resources: []string{"alerts"},
		Verbs:     []string{"patch"},
	}}
	if !reflect.DeepEqual(r.Rules, want) {
		t.Errorf("rules = %+v, want %+v", r.Rules, want)
	}
	b := bindings[alertPatcher]
	if b.Namespace != "obs-ns" || b.RoleRef.Kind != "Role" || b.RoleRef.Name != alertPatcher {
		t.Errorf("binding ns=%q roleRef=%+v", b.Namespace, b.RoleRef)
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
	for _, c := range cluster {
		if strings.Contains(c, "patcher") {
			t.Errorf("a cluster-scoped patcher object rendered: %s", c)
		}
	}
}

// The schema refuses an empty group name, a repeated group and a non-list.
func TestTheSchemaRefusesMalformedAlertPatcherGroups(t *testing.T) {
	for _, args := range [][]string{
		{"--set-json", `alertPatcherGroups=[""]`},
		{"--set-json", `alertPatcherGroups=["sre","sre"]`},
		{"--set-json", `alertPatcherGroups="sre"`},
		{"--set-json", `alertPatcherGroups=[1]`},
	} {
		if _, _, _, err := renderNamespacedRBAC(t, "krateo-system", args...); err == nil {
			t.Errorf("%v rendered", args)
		}
	}
}
