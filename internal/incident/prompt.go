package incident

import (
	"fmt"
	"strings"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"

	"github.com/krateo-platformops/alert-provider/internal/compare"
	"github.com/krateo-platformops/alert-provider/internal/pyfmt"
	"github.com/krateo-platformops/alert-provider/internal/report"
)

// BuildPrompt is the RCA prompt. The user message carries the incident; the agent's system prompt
// carries the method. The method therefore lives in one place, so the A2A URL must point at an
// agent whose prompt carries it (incident-agent does; a bare generalist does not).
//
// source is the Alert's spec.source: the rows where reads, log records unless it is traces.
func BuildPrompt(alertName, alertState, where, message, source string) string {
	scope := ""
	if where != "" {
		scope = "\n\nIt fired on " + compare.Records(source) + " matching `" + where + "`"
		if source == compare.Traces {
			scope += " in the traces source (otel_traces)"
		}
		if message != "" {
			scope += " — intent: " + message
		}
		scope += ". That query is your entry point, and the workload those rows name is what you diagnose."
	}
	return fmt.Sprintf("The HyperDX alert \"%s\" has fired (state %s) on this Krateo PlatformOps cluster.", alertName, alertState) +
		scope +
		"\n\nRoot-cause it: the single most likely cause, the composition or component affected, and how to fix it." +
		report.Instructions
}

// Process logs a HyperDX notification. It opens nothing: the reconcile mirrors every alert's state
// on each pass and fires the firing ones, so a notification would only fire twice. The webhook
// exists because a HyperDX alert needs a channel; the body is hyperdx.DefaultWebhookBody.
//
// Of the notification's body it logs only the group line (a grouped alert's series), never the
// rest: a body can quote the records it counted. It logs the eventId too, when there is one.
func Process(log logging.Logger, payload any) {
	p, _ := payload.(map[string]any)
	title := ""
	alert, _ := p["alert"].(map[string]any)
	for _, v := range []any{p["alertName"], p["title"], p["name"], alert["name"]} {
		if pyfmt.Truthy(v) {
			title = pyfmt.Str(v)
			break
		}
	}
	state := ""
	for _, v := range []any{p["state"], p["status"]} {
		if pyfmt.Truthy(v) {
			state = strings.ToUpper(pyfmt.Str(v))
			break
		}
	}
	if state == "" {
		state = "?"
	}
	line := fmt.Sprintf("[webhook] %s %s", pyfmt.Repr(title), state)
	if body, ok := p["body"].(string); ok {
		if g := Group(body); g != "" {
			line += fmt.Sprintf(" group=%q", pyfmt.Cut(g, 1000))
		}
	}
	if id, ok := p["eventId"].(string); ok && id != "" {
		line += " eventId=" + pyfmt.Cut(id, 100)
	}
	log.Info(line + ": the reconciler evaluates it on its next pass")
}

// Group is the series a notification body names, "" when it names none: the first line that starts
// with "Group: ". HyperDX opens a grouped alert's body with `Group: "<group>"` (followed by " - The
// alert has been resolved." on a resolve); the message's own line (hyperdx.GroupLine) is unquoted.
func Group(body string) string {
	for _, l := range strings.Split(body, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(l), "Group: ")
		if !ok {
			continue
		}
		if strings.HasPrefix(rest, `"`) {
			if end := strings.LastIndex(rest, `"`); end > 0 {
				return rest[1:end]
			}
		}
		return strings.TrimSpace(rest)
	}
	return ""
}
