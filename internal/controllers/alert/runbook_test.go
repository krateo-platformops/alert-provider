package alert

import (
	"context"
	"testing"

	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
	"github.com/krateo-platformops/alert-provider/internal/hyperdx/hyperdxtest"
	"github.com/krateo-platformops/alert-provider/internal/incident"
)

const runbook = "Free space by dropping expired telemetry partitions.\n\n**Never** drop audit partitions.\n"

func runbookCR() *v1alpha1.Alert {
	a := alertCR(k, "w", v1alpha1.AlertStatus{})
	a.Spec.Runbook = runbook
	return a
}

func TestADescriptionOnlyAlertPushesTheDescriptionAsWritten(t *testing.T) {
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	h.pass(1)
	eq(t, "message", h.byName()[k]["message"], "m")
}

func TestARunbookIsPushedAfterTheDescription(t *testing.T) {
	h := newHarness(t, runbookCR())
	h.pass(1)
	eq(t, "message", h.byName()[k]["message"],
		"m\n\nFree space by dropping expired telemetry partitions.\n\n**Never** drop audit partitions.")
	eq(t, "phase", h.status(k).Phase, "Synced")

	h.api.ClearWrites()
	h.pass(3)
	eq(t, "converged", nonWebhookWrites(h.api), []hyperdxtest.Write{})
}

// A runbook edit is one PUT of the HyperDX alert, and then no drift; removing it is the same.
func TestAChangedRunbookIsOnePutAndThenQuiet(t *testing.T) {
	h := newHarness(t, runbookCR())
	h.pass(1)
	id := h.status(k).HyperdxAlertID

	h.edit(k, func(a *v1alpha1.Alert) { a.Spec.Runbook = "Check the collector first." })
	h.api.ClearWrites()
	h.pass(1)
	eq(t, "writes", nonWebhookWrites(h.api), []hyperdxtest.Write{hyperdxtest.W("PUT", "alerts", id)})
	eq(t, "message", h.api.Get("alerts", id)["message"], "m\n\nCheck the collector first.")
	h.api.ClearWrites()
	h.pass(3)
	eq(t, "quiet", nonWebhookWrites(h.api), []hyperdxtest.Write{})

	h.edit(k, func(a *v1alpha1.Alert) { a.Spec.Runbook = "" })
	h.api.ClearWrites()
	h.pass(1)
	eq(t, "removed", nonWebhookWrites(h.api), []hyperdxtest.Write{hyperdxtest.W("PUT", "alerts", id)})
	eq(t, "message", h.api.Get("alerts", id)["message"], "m")
	h.api.ClearWrites()
	h.pass(3)
	eq(t, "quiet again", nonWebhookWrites(h.api), []hyperdxtest.Write{})
}

func TestAFiringCarriesTheRunbook(t *testing.T) {
	h := newHarness(t, runbookCR())
	var got incident.Alert
	h.conn.firings.fire = func(_ context.Context, a incident.Alert, _ incident.Records) { got = a }
	h.pass(1)
	h.api.Set("alerts", h.status(k).HyperdxAlertID, map[string]any{"state": "ALERT"})
	h.pass(1)
	eq(t, "runbook", got.Runbook, runbook)
}
