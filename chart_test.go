package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

const chartDir = "helm/alert-provider"

// render is `helm template` of a copy of the chart with CHART_VERSION set to 0.0.0, as make
// lint-charts does, and its Deployment and bootstrap Job. helm is required in CI (CI=true, which
// GitHub Actions sets); elsewhere its absence skips.
func render(t *testing.T, args ...string) (*appsv1.Deployment, *batchv1.Job) {
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
	chart := filepath.Join(dst, "Chart.yaml")
	raw, err := os.ReadFile(chart)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chart, bytes.ReplaceAll(raw, []byte("CHART_VERSION"), []byte("0.0.0")), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(helm, append([]string{"template", "t", dst, "--namespace", "krateo-system"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	var dep *appsv1.Deployment
	var job *batchv1.Job
	for _, doc := range strings.Split(string(out), "\n---") {
		var head struct{ Kind string }
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			t.Fatalf("rendered document does not parse: %v\n%s", err, doc)
		}
		switch head.Kind {
		case "Deployment":
			dep = &appsv1.Deployment{}
			if err := yaml.Unmarshal([]byte(doc), dep); err != nil {
				t.Fatal(err)
			}
		case "Job":
			job = &batchv1.Job{}
			if err := yaml.Unmarshal([]byte(doc), job); err != nil {
				t.Fatal(err)
			}
		}
	}
	if dep == nil || job == nil {
		t.Fatalf("no Deployment or Job rendered:\n%s", out)
	}
	return dep, job
}

func otelVars(env []corev1.EnvVar) []string {
	var out []string
	for _, e := range env {
		if strings.HasPrefix(e.Name, "OTEL_") || e.Name == "HOST_IP" || e.Name == "SERVICE_VERSION" || e.Name == "DEPLOYMENT_NAME" {
			out = append(out, e.Name)
		}
	}
	return out
}

// The default renders no OTel variable anywhere, and values.yaml ships tracing off.
// MUTATION: otel.enabled true in values.yaml fails this.
func TestTheChartShipsTracingOff(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Otel struct {
			Enabled       *bool  `json:"enabled"`
			TracesSampler string `json:"tracesSampler"`
			ServiceName   string `json:"serviceName"`
		} `json:"otel"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	if values.Otel.Enabled == nil || *values.Otel.Enabled {
		t.Fatalf("values.yaml otel.enabled = %v, want an explicit false", values.Otel.Enabled)
	}
	if values.Otel.TracesSampler != "parentbased_always_on" || values.Otel.ServiceName != "krateo-alert-provider" {
		t.Errorf("otel defaults %+v", values.Otel)
	}
	dep, job := render(t)
	if v := otelVars(dep.Spec.Template.Spec.Containers[0].Env); len(v) != 0 {
		t.Errorf("the default Deployment renders %v", v)
	}
	if v := otelVars(job.Spec.Template.Spec.Containers[0].Env); len(v) != 0 {
		t.Errorf("the default Job renders %v", v)
	}
}

// Enabled, the Deployment gets the contract, HOST_IP before the endpoint that expands it, and its
// own name as DEPLOYMENT_NAME; the immutable Job's template does not change.
// MUTATION: a DEPLOYMENT_NAME other than the Deployment's metadata.name fails this.
func TestEnabledTheDeploymentGetsTheOTelEnv(t *testing.T) {
	dep, job := render(t, "--set", "otel.enabled=true", "--set", "otel.tracesSampler=parentbased_traceidratio",
		"--set", "otel.serviceName=ap", "--set", "image.tag=1.2.3")
	env := dep.Spec.Template.Spec.Containers[0].Env
	at := map[string]int{}
	byName := map[string]corev1.EnvVar{}
	for i, e := range env {
		at[e.Name], byName[e.Name] = i, e
	}
	want := map[string]string{"OTEL_ENABLED": "true", "OTEL_TRACING_ENABLED": "true",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://$(HOST_IP):4318", "OTEL_TRACES_SAMPLER": "parentbased_traceidratio",
		"OTEL_SERVICE_NAME": "ap", "SERVICE_VERSION": "1.2.3",
		// The resource's k8s.deployment.name: the Deployment's own name, whatever it is.
		"DEPLOYMENT_NAME": dep.Name}
	for k, v := range want {
		if e, ok := byName[k]; !ok || e.Value != v {
			t.Errorf("%s = %q (present %v), want %q", k, e.Value, ok, v)
		}
	}
	host, ok := byName["HOST_IP"]
	if !ok || host.ValueFrom == nil || host.ValueFrom.FieldRef == nil || host.ValueFrom.FieldRef.FieldPath != "status.hostIP" {
		b, _ := json.Marshal(host)
		t.Fatalf("HOST_IP %s, want fieldRef status.hostIP", b)
	}
	if at["HOST_IP"] > at["OTEL_EXPORTER_OTLP_ENDPOINT"] {
		t.Error("HOST_IP is declared after the endpoint: $(HOST_IP) would not expand")
	}
	if v := otelVars(job.Spec.Template.Spec.Containers[0].Env); len(v) != 0 {
		t.Errorf("the Job renders %v", v)
	}
}

// The schema is closed and types the knobs: an unknown key or a non-boolean switch is refused.
func TestTheSchemaRefusesUnknownOrMistypedOTelValues(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not on PATH")
	}
	for _, set := range []string{"otel.enable=true", "otel.enabled=yes", "otel.tracesSampler=sometimes"} {
		dir := filepath.Join(t.TempDir(), "alert-provider")
		if out, err := exec.Command("cp", "-R", chartDir, dir).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		raw, _ := os.ReadFile(filepath.Join(dir, "Chart.yaml"))
		_ = os.WriteFile(filepath.Join(dir, "Chart.yaml"), bytes.ReplaceAll(raw, []byte("CHART_VERSION"), []byte("0.0.0")), 0o600)
		if out, err := exec.Command("helm", "template", "t", dir, "--set-string", set).CombinedOutput(); err == nil {
			t.Errorf("--set-string %s rendered:\n%.200s", set, out)
		}
	}
}

// config.incidentCoverWindow reaches INCIDENT_COVER_WINDOW, 24h by default.
// MUTATION: dropping the env entry from the Deployment fails this.
func TestTheCoverWindowReachesTheDeployment(t *testing.T) {
	for set, want := range map[string]string{"": "24h", "config.incidentCoverWindow=1h": "1h"} {
		var args []string
		if set != "" {
			args = []string{"--set-string", set}
		}
		dep, _ := render(t, args...)
		got, found := "", false
		for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
			if e.Name == "INCIDENT_COVER_WINDOW" {
				got, found = e.Value, true
			}
		}
		if !found || got != want {
			t.Errorf("--set-string %q: INCIDENT_COVER_WINDOW = %q (present %v), want %q", set, got, found, want)
		}
	}
}

// The schema takes a Go duration or "0", and refuses anything time.ParseDuration would.
func TestTheSchemaRefusesAMalformedCoverWindow(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not on PATH")
	}
	for set, ok := range map[string]bool{"24h": true, "90m": true, "1h30m": true, "0": true,
		"24": false, "1d": false, "-1h": false, "24 h": false, "": false} {
		dir := filepath.Join(t.TempDir(), "alert-provider")
		if out, err := exec.Command("cp", "-R", chartDir, dir).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		raw, _ := os.ReadFile(filepath.Join(dir, "Chart.yaml"))
		_ = os.WriteFile(filepath.Join(dir, "Chart.yaml"), bytes.ReplaceAll(raw, []byte("CHART_VERSION"), []byte("0.0.0")), 0o600)
		out, err := exec.Command("helm", "template", "t", dir, "--set-string", "config.incidentCoverWindow="+set).CombinedOutput()
		if (err == nil) != ok {
			t.Errorf("config.incidentCoverWindow=%q: rendered %v, want %v:\n%.300s", set, err == nil, ok, out)
		}
	}
}

// The RCA concurrency and retry knobs reach the Deployment, at today's defaults (2 at once, 2
// retries, 120 s) and as set.
// MUTATION: dropping MAX_CONCURRENT_ANALYSES from the Deployment, or changing a default, fails this.
func TestTheRCAConcurrencyAndRetriesReachTheDeployment(t *testing.T) {
	for _, c := range []struct {
		set  []string
		want map[string]string
	}{
		{nil, map[string]string{"MAX_CONCURRENT_ANALYSES": "2", "ANALYSIS_RETRIES": "2", "ANALYSIS_RETRY_DELAY": "120"}},
		{[]string{"--set", "config.maxConcurrentAnalyses=4,config.analysisRetries=5,config.analysisRetryDelay=30"},
			map[string]string{"MAX_CONCURRENT_ANALYSES": "4", "ANALYSIS_RETRIES": "5", "ANALYSIS_RETRY_DELAY": "30"}},
	} {
		dep, _ := render(t, c.set...)
		got := map[string]string{}
		for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
			if _, ok := c.want[e.Name]; ok {
				got[e.Name] = e.Value
			}
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%v: %s = %q, want %q", c.set, k, got[k], v)
			}
		}
	}
}

// The schema refuses a concurrency under 1, which main would raise to 1 silently.
func TestTheSchemaRefusesNoConcurrentRCAs(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not on PATH")
	}
	for set, ok := range map[string]bool{"1": true, "8": true, "0": false, "-1": false} {
		dir := filepath.Join(t.TempDir(), "alert-provider")
		if out, err := exec.Command("cp", "-R", chartDir, dir).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		raw, _ := os.ReadFile(filepath.Join(dir, "Chart.yaml"))
		_ = os.WriteFile(filepath.Join(dir, "Chart.yaml"), bytes.ReplaceAll(raw, []byte("CHART_VERSION"), []byte("0.0.0")), 0o600)
		out, err := exec.Command("helm", "template", "t", dir, "--set", "config.maxConcurrentAnalyses="+set).CombinedOutput()
		if (err == nil) != ok {
			t.Errorf("config.maxConcurrentAnalyses=%s: rendered %v, want %v:\n%.300s", set, err == nil, ok, out)
		}
	}
}
