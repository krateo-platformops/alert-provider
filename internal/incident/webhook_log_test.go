package incident

import (
	"testing"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"
)

// lines is a Logger that keeps the Info messages.
type lines struct{ info *[]string }

func (l lines) Info(msg string, _ ...any)        { *l.info = append(*l.info, msg) }
func (l lines) Debug(string, ...any)             {}
func (l lines) Warn(string, ...any)              {}
func (l lines) Error(error, string, ...any)      {}
func (l lines) WithValues(...any) logging.Logger { return l }
func (l lines) WithName(string) logging.Logger   { return l }

func logged(payload map[string]any) []string {
	var out []string
	Process(lines{&out}, payload)
	return out
}

// What HyperDX 2.35 renders into {{body}} for a grouped tile alert: its own quoted group line, the
// value, the time range, then the Alert's message.
const (
	readyGroup = "if(notEmpty(arrayElement(Attributes, 'k8s.pod.name')), arrayElement(Attributes, 'k8s.pod.name'), " +
		"arrayElement(ResourceAttributes, 'k8s.pod.name')):snowplow-5c4954d6fc-s5rvj"
	firingBody = `Group: "` + readyGroup + `"` + "\n0 falls below 1\nTime Range (UTC): [Nov 8 3:35:00 PM - Nov 8 3:40:00 PM)\n" +
		"A container is not ready."
	resolvedBody = `Group: "` + readyGroup + `" - The alert has been resolved.` + "\nTime Range (UTC): [Nov 8 3:40:00 PM - Nov 8 3:41:00 PM)\n" +
		"A container is not ready."
)

func TestAGroupedNotificationLogsItsGroupLineAndEventID(t *testing.T) {
	eq(t, "firing", logged(map[string]any{"alertName": "🚨 sre-pod-not-ready", "state": "ALERT", "body": firingBody,
		"eventId": "3f2a9c", "source": "hyperdx-alert"}),
		[]string{`[webhook] '🚨 sre-pod-not-ready' ALERT group="` + readyGroup + `" eventId=3f2a9c: the reconciler evaluates it on its next pass`})
	eq(t, "resolved", logged(map[string]any{"alertName": "✅ sre-pod-not-ready", "state": "OK", "body": resolvedBody, "eventId": "3f2a9c"}),
		[]string{`[webhook] '✅ sre-pod-not-ready' OK group="` + readyGroup + `" eventId=3f2a9c: the reconciler evaluates it on its next pass`})
}

// The body's other lines can quote records: they are never logged.
func TestAnUngroupedNotificationLogsNoBody(t *testing.T) {
	body := "3 lines found, which exceeds the threshold of 2 lines\n```\nERROR secret=hunter2\n```"
	eq(t, "ungrouped", logged(map[string]any{"alertName": "🚨 krateo-platform-crashloop", "state": "ALERT", "body": body, "eventId": "e1"}),
		[]string{"[webhook] '🚨 krateo-platform-crashloop' ALERT eventId=e1: the reconciler evaluates it on its next pass"})
	// A notification of the old body, without body or eventId, logs today's line.
	eq(t, "old body", logged(map[string]any{"alertName": "🚨 krateo-platform-crashloop", "state": "ALERT"}),
		[]string{"[webhook] '🚨 krateo-platform-crashloop' ALERT: the reconciler evaluates it on its next pass"})
}

func TestGroupReadsTheFirstGroupLine(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{firingBody, readyGroup},
		{resolvedBody, readyGroup},
		// A message's own unquoted line does not shadow HyperDX's quoted one, even before it.
		{"Group: mine\n" + firingBody, readyGroup},
		{"Group: mine\n" + resolvedBody, readyGroup},
		{"A container is not ready.\nGroup: ns:a, pod:b", "ns:a, pod:b"}, // only an unquoted line: tolerated
		{"no group here\nGrouping: x", ""},
		{"", ""},
	} {
		eq(t, c.body, Group(c.body), c.want)
	}
}
