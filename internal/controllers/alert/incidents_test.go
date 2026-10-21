package alert

import (
	"testing"

	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
)

// spec.incidents decides whether a firing reaches the incident writer (#66). The cases below are
// the ones that can regress: that `Never` stops a COUNTING alert (which would otherwise open one),
// that it does not stop the notification, and that leaving the field unset changes nothing for
// either kind of alert.

// OpensIncidents is the single place the unset default is decided, so its truth table is asserted
// directly rather than only through the controller.
func TestOpensIncidentsTruthTable(t *testing.T) {
	for _, c := range []struct {
		name        string
		incidents   string
		aggregation string
		want        bool
	}{
		{"unset on a counting alert opens one", "", "", true},
		{"unset on an aggregated alert does not", "", "sum", false},
		{"Never on a counting alert does not", v1alpha1.IncidentsNever, "", false},
		{"Never on an aggregated alert does not", v1alpha1.IncidentsNever, "sum", false},
		{"Auto on a counting alert opens one", v1alpha1.IncidentsAuto, "", true},
		// Auto + aggregated is refused by the CRD, so it cannot reach the controller. The helper
		// still has to answer, and answering true keeps it honest: the refusal belongs at
		// admission, not hidden in a helper that quietly disagrees with the field it was given.
		{"Auto on an aggregated alert is taken at its word", v1alpha1.IncidentsAuto, "sum", true},
	} {
		a := &v1alpha1.Alert{}
		a.Spec.Incidents, a.Spec.Aggregation = c.incidents, c.aggregation
		if got := a.OpensIncidents(); got != c.want {
			t.Errorf("%s: OpensIncidents() = %v, want %v", c.name, got, c.want)
		}
	}
}

// The case #66 exists for: a counting alert that is a notification, not a problem to triage. It
// fires, the state is mirrored, and no incident is opened. This test FAILS without the gate --
// before #66 a counting alert always reached the writer.
func TestIncidentsNeverStopsACountingAlertFromOpeningOne(t *testing.T) {
	a := alertCR(k, "where-k", v1alpha1.AlertStatus{})
	a.Spec.Incidents = v1alpha1.IncidentsNever
	h := newHarness(t, a)
	h.pass(1)
	h.api.Set("alerts", h.status(k).HyperdxAlertID, map[string]any{"state": "ALERT"})
	h.pass(2)
	eq(t, "fired", h.fired, []string(nil))
	// The notification half is untouched: the ALERT state still reaches the Alert, and HyperDX
	// still notifies through the webhook. `Never` suppresses the incident, not the alert.
	eq(t, "state", h.status(k).State, "ALERT")
	eq(t, "phase", h.status(k).Phase, "Synced")
}

// The default must not become "Never for everything": an explicit Auto, and an unset field on a
// counting alert, both still open incidents. Without this, a gate that always returned false
// would pass the test above.
func TestACountingAlertStillOpensIncidentsByDefaultAndOnAuto(t *testing.T) {
	for _, incidents := range []string{"", v1alpha1.IncidentsAuto} {
		a := alertCR(k, "where-k", v1alpha1.AlertStatus{})
		a.Spec.Incidents = incidents
		h := newHarness(t, a)
		h.pass(1)
		h.api.Set("alerts", h.status(k).HyperdxAlertID, map[string]any{"state": "ALERT"})
		h.pass(2)
		if len(h.fired) == 0 {
			t.Errorf("incidents=%q: fired %v, want the firing to reach the incident writer", incidents, h.fired)
		}
	}
}
