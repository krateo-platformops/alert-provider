package incident

import (
	"strings"
	"testing"
)

// ownerBlock is block(how) with a root cause that names its owner, and a category and an
// ownerEvidence item the Incident CRD would refuse.
func ownerBlock() map[string]any {
	b := block(how)
	b["rootCause"] = map[string]any{"statement": "chart 1.1.9 is missing", "confidence": 0.8, "category": "rbac",
		"owner": "krateo", "ownerEvidence": []any{
			map[string]any{"kind": "component", "ref": "core.krateo.io/CompositionDefinition/krateo-system/fireworksapp",
				"note": "chart shipped by Krateo"},
			map[string]any{"kind": "chart", "ref": "fireworksapp"},
		}}
	return b
}

func TestTheAnalysisWritesTheOwnerFittedToTheCRDAndLogsWhatItCoerced(t *testing.T) {
	c := newCase(t)
	c.answer = answer(ownerBlock())
	c.fire(fireOpt{})
	inc := c.kube.only(ns)
	s := st(inc)
	eq(t, "state", s["state"], "Open")
	eq(t, "rootCause", s["rootCause"], map[string]any{"statement": "chart 1.1.9 is missing", "confidence": "0.8",
		"category": "other", "owner": "krateo", "ownerEvidence": []any{
			map[string]any{"kind": "component", "ref": "core.krateo.io/CompositionDefinition/krateo-system/fireworksapp",
				"note": "chart shipped by Krateo"}}})
	want := "[a2a] " + ns + "/" + str(metadata(inc), "name") + ": rootCause fitted to the Incident CRD: " +
		"rootCause.category 'rbac' is not one of config, capacity, image, network, dependency, permission, other, written as other; " +
		"rootCause.ownerEvidence[1] dropped: kind 'chart' is not one of component, config, trace, log"
	c.log.mu.Lock()
	defer c.log.mu.Unlock()
	found := false
	for _, l := range c.log.lines {
		found = found || l == want
	}
	if !found {
		t.Errorf("no log line %q in\n%s", want, strings.Join(c.log.lines, "\n"))
	}
}

// An Incident CRD older than incident-provider 0.10.0 declares neither owner nor ownerEvidence, and
// the apiserver prunes them from the status write without an error. The writer never reads them
// back, so the analysis lands and the incident works as before: a later firing compares with it.
func TestAnOlderIncidentCRDPruningTheOwnerIsHarmless(t *testing.T) {
	c := newCase(t)
	c.answer = answer(ownerBlock())
	c.kube.prune = func(status map[string]any) {
		if rc, ok := status["rootCause"].(map[string]any); ok {
			delete(rc, "owner")
			delete(rc, "ownerEvidence")
		}
	}
	c.fire(fireOpt{})
	inc := c.kube.only(ns)
	name := str(metadata(inc), "name")
	s := st(inc)
	eq(t, "state", s["state"], "Open")
	eq(t, "howToFix", s["howToFix"], how)
	eq(t, "rootCause", s["rootCause"], map[string]any{"statement": "chart 1.1.9 is missing", "confidence": "0.8",
		"category": "other"})
	writes := c.kube.statusWrites()
	if rc, _ := writes[len(writes)-1]["rootCause"].(map[string]any); rc["owner"] != "krateo" {
		t.Errorf("the write sent no owner: %v", rc)
	}
	c.log.mu.Lock()
	for _, l := range c.log.lines {
		if strings.HasPrefix(l, "[err]") {
			t.Errorf("error logged: %q", l)
		}
	}
	c.log.mu.Unlock()

	c.match = name
	c.fire(fireOpt{})
	eq(t, "compared", c.compared, [][]string{{name}})
	eq(t, "incidents", c.kube.count(), 1)
}

// An answer without an owner (an incident-agent older than the owner contract) is "not assessed":
// the status gets neither owner nor ownerEvidence, and no coercion is logged.
func TestAnAnswerWithoutAnOwnerWritesNone(t *testing.T) {
	c := newCase(t)
	c.fire(fireOpt{})
	eq(t, "rootCause", st(c.kube.only(ns))["rootCause"], map[string]any{"statement": "chart 1.1.9 is missing",
		"confidence": "0.8", "category": "config"})
	c.log.mu.Lock()
	defer c.log.mu.Unlock()
	for _, l := range c.log.lines {
		if strings.Contains(l, "fitted to the Incident CRD") {
			t.Errorf("coercion logged: %q", l)
		}
	}
}
