package hyperdx_test

import (
	"testing"

	"github.com/krateo-platformops/alert-provider/internal/hyperdx"
)

// A runbook follows the default message as it follows a description: an Alert without a
// description still sends its guidance.
func TestARunbookFollowsTheDefaultMessage(t *testing.T) {
	c, api := client(t)
	f := fields
	f.Message, f.Runbook = "", "Never drop audit partitions.\n"
	out, _ := c.EnsureAlert(ctx, "my-alert", "d1", "count", "hook", f)
	eq(t, "message", api.Get("alerts", out["id"].(string))["message"],
		"my-alert threshold crossed — incident-agent will auto-triage.\n\nNever drop audit partitions.")
}

// The drift compares the message as pushed: the live text with its runbook is no drift, the
// text without it is one.
func TestAlertDriftComparesTheMessageWithItsRunbook(t *testing.T) {
	f := hyperdx.AlertFields{Interval: "5m", Threshold: 1, ThresholdType: "above", Message: "m", Runbook: "r\n"}
	live := map[string]any{"name": "a", "interval": "5m", "threshold": 1, "thresholdType": "above", "message": "m\n\nr"}
	eq(t, "pushed", hyperdx.AlertDrift(live, "a", f), []string(nil))
	live["message"] = "m"
	eq(t, "without it", hyperdx.AlertDrift(live, "a", f), []string{"message"})
	f.Runbook = "  \n"
	eq(t, "a blank runbook", hyperdx.AlertDrift(live, "a", f), []string(nil))
}
