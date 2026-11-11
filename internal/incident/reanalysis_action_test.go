package incident

import (
	"testing"

	"github.com/krateo-platformops/alert-provider/internal/pyfmt"
	"github.com/krateo-platformops/alert-provider/internal/report"
)

// reanalysisHead is ReanalysisPrompt's text for promptIncident up to its last script's result: the
// part an applyAction section follows.
const reanalysisHead = "BASE PROMPT" +
	"\n\n## This is a reanalysis\n\nA person asked for a new analysis of this incident. The previous one is below; " +
	"it may be wrong. Diagnose again from the live cluster rather than restating it, and write scripts that run. " +
	"A script whose last run gave no verdict (an exit other than 0 or 1) is broken: its output says why." +
	"\n\nWhat the person found wrong with the previous analysis:\n\n```text\nthe patch is refused\n```" +
	"\n\nPrevious root cause: the CRD moved to v1-2-0" +
	"\n\nThe incident was Open." +
	"\n\nPrevious precondition script:\n\n```bash\n#!/usr/bin/env bash\n# holds while web is on v1-1-0\nexit 1\n```" +
	"\n\nIts last run exited 1 (since 2026-11-11T10:00:00Z)." +
	"\n\nPrevious apply script:\n\n```bash\n#!/usr/bin/env bash\n# move web to v1-2-0\ntrue\n```" +
	"\n\nPrevious verify script:\n\n```bash\n#!/usr/bin/env bash\n# fixed once web is on v1-2-0\nexit 0\n```"

const staleMessage = "applyAction targets composition.krateo.io/v1-1-0 fireworksapps, which this cluster does not serve; " +
	"served: composition.krateo.io/v1-2-0, composition.krateo.io/v1-3-0"

// promptIncident is an Open incident with a full analysis, with action as its applyAction (none
// when nil) and conds as its conditions. Its values are decoded as the Kube reads them.
func promptIncident(t *testing.T, action map[string]any, conds ...map[string]any) map[string]any {
	t.Helper()
	fix := map[string]any{
		"precondition": "#!/usr/bin/env bash\n# holds while web is on v1-1-0\nexit 1\n",
		"apply":        "#!/usr/bin/env bash\n# move web to v1-2-0\ntrue\n",
		"verify":       "#!/usr/bin/env bash\n# fixed once web is on v1-2-0\nexit 0\n",
	}
	if action != nil {
		fix["applyAction"] = action
	}
	list := []any{map[string]any{"type": "Ready", "status": "True"}}
	for _, c := range conds {
		list = append(list, c)
	}
	return clone(map[string]any{"status": map[string]any{
		"state":      "Open",
		"rootCause":  map[string]any{"statement": "the CRD moved to v1-2-0"},
		"howToFix":   fix,
		"lastChecks": map[string]any{"precondition": map[string]any{"exit": 1, "since": "2026-11-11T10:00:00Z"}},
		"conditions": list,
	}})
}

func reanalysisPrompt(inc map[string]any) string {
	return ReanalysisPrompt("BASE PROMPT"+report.Instructions, inc, "the patch is refused")
}

func wantPrompt(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("prompt\n got %q\nwant %q", got, want)
	}
}

// The payload carries non-ASCII text, a number and nested keys out of order: rendered as
// pyfmt.Dumps renders them, with the top-level fields in the contract's order.
func patchAction() map[string]any {
	return map[string]any{
		"payload":    map[string]any{"spec": map[string]any{"replicas": 2, "chart": map[string]any{"version": "1.2.0"}, "owner": "Zoë"}},
		"name":       "web",
		"namespace":  "team-a",
		"resource":   "fireworksapps",
		"apiVersion": "composition.krateo.io/v1-1-0",
		"verb":       "patch",
	}
}

const patchActionJSON = `{"verb": "patch", "apiVersion": "composition.krateo.io/v1-1-0", "resource": "fireworksapps", ` +
	`"namespace": "team-a", "name": "web", "payload": {"spec": {"chart": {"version": "1.2.0"}, "owner": "Zo\u00eb", "replicas": 2}}}`

func TestAReanalysisPromptWithoutAnApplyActionIsUnchanged(t *testing.T) {
	wantPrompt(t, reanalysisPrompt(promptIncident(t, nil)), reanalysisHead+report.Instructions)
	// A stale condition without an applyAction to be stale says nothing.
	wantPrompt(t, reanalysisPrompt(promptIncident(t, nil,
		map[string]any{"type": "ApplyActionStale", "status": "True", "reason": "VersionNotServed", "message": staleMessage})),
		reanalysisHead+report.Instructions)
}

func TestAReanalysisPromptCarriesThePreviousApplyAction(t *testing.T) {
	wantPrompt(t, reanalysisPrompt(promptIncident(t, patchAction())),
		reanalysisHead+"\n\nPrevious applyAction:\n\n```json\n"+patchActionJSON+"\n```"+report.Instructions)
}

// A delete has neither payload nor, cluster-scoped, a namespace: the block leaves them out.
func TestAReanalysisPromptRendersADeleteApplyActionWithoutPayload(t *testing.T) {
	action := map[string]any{"verb": "delete", "apiVersion": "v1", "resource": "persistentvolumes", "name": "pv-1"}
	wantPrompt(t, reanalysisPrompt(promptIncident(t, action)),
		reanalysisHead+"\n\nPrevious applyAction:\n\n```json\n"+
			`{"verb": "delete", "apiVersion": "v1", "resource": "persistentvolumes", "name": "pv-1"}`+
			"\n```"+report.Instructions)
}

func TestAReanalysisPromptSaysAStaleApplyActionCanNoLongerBeSent(t *testing.T) {
	for _, reason := range []string{"VersionNotServed", "ResourceNotFound"} {
		wantPrompt(t, reanalysisPrompt(promptIncident(t, patchAction(),
			map[string]any{"type": "ApplyActionStale", "status": "True", "reason": reason, "message": staleMessage})),
			reanalysisHead+"\n\nPrevious applyAction:\n\n```json\n"+patchActionJSON+"\n```"+
				"\n\nThe previous applyAction can no longer be sent: "+staleMessage+"."+report.Instructions)
	}
}

func TestAReanalysisPromptHasNoStaleLineWhenTheConditionIsNotTrue(t *testing.T) {
	for _, status := range []string{"False", "Unknown"} {
		wantPrompt(t, reanalysisPrompt(promptIncident(t, patchAction(),
			map[string]any{"type": "ApplyActionStale", "status": status, "reason": "Served", "message": staleMessage})),
			reanalysisHead+"\n\nPrevious applyAction:\n\n```json\n"+patchActionJSON+"\n```"+report.Instructions)
	}
}

// The JSON block parses back to the applyAction it renders.
func TestThePreviousApplyActionBlockIsTheApplyAction(t *testing.T) {
	inc := promptIncident(t, patchAction())
	v, err := pyfmt.Decode([]byte(actionJSON(st(inc)["howToFix"].(map[string]any)["applyAction"].(map[string]any))))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "applyAction", v, st(inc)["howToFix"].(map[string]any)["applyAction"])
}
