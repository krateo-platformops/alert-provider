package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/krateo-platformops/alert-provider/internal/report"
)

// streamLine is one A2A status event carrying parts.
func streamLine(parts ...any) string {
	b, _ := json.Marshal(map[string]any{"result": map[string]any{"status": map[string]any{
		"message": map[string]any{"role": "agent", "parts": parts}}}})
	return "data: " + string(b)
}

func responsePart(name string, resp any) map[string]any {
	return map[string]any{"kind": "data", "metadata": map[string]any{"adk_type": "function_response"},
		"data": map[string]any{"id": "1", "name": name, "response": resp}}
}

// session is a kagent GET /api/sessions/<id> answer whose events hold these function responses.
func session(responses ...map[string]any) []byte {
	var events []map[string]any
	for _, fr := range responses {
		ev, _ := json.Marshal(map[string]any{"Author": "agent", "Content": map[string]any{
			"role": "user", "parts": []any{map[string]any{"functionResponse": fr}}}})
		events = append(events, map[string]any{"id": "e", "data": string(ev)})
	}
	b, _ := json.Marshal(map[string]any{"error": false, "data": map[string]any{"session": map[string]any{}, "events": events}})
	return b
}

// delegatingServer is a gateway: an A2A stream at /api/a2a/krateo-system/autopilot whose one tool
// call delegates to incident-agent, and the session API.
func delegatingServer(t *testing.T, sessions map[string][]byte, auth *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*auth = append(*auth, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		if id, ok := strings.CutPrefix(r.URL.Path, "/api/sessions/"); ok {
			body, found := sessions[id]
			if !found {
				http.Error(w, `{"error":true,"message":"Session not found"}`, http.StatusNotFound)
				return
			}
			_, _ = w.Write(body)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, streamLine(responsePart("krateo_system__NS__incident_agent",
			map[string]any{"result": "## Root cause\nforbidden: nothing", "subagent_session_id": "sub-1"})))
		fmt.Fprintln(w, streamLine(map[string]any{"kind": "text", "text": "## Root cause\nanswer"}))
	}))
}

func analyzeThrough(t *testing.T, srv *httptest.Server, path string) []report.ToolResult {
	t.Helper()
	a := &A2A{URL: srv.URL + path, Timeout: 5 * time.Second, JWT: func(context.Context) string { return "jwt" }}
	text, ledger, err := a.Analyze(context.Background(), "prompt", "ctx")
	if err != nil {
		t.Fatal(err)
	}
	if text != "## Root cause\nanswer" {
		t.Errorf("text = %q", text)
	}
	return ledger
}

func ledgerLines(ledger []report.ToolResult) []string {
	var out []string
	for _, r := range ledger {
		out = append(out, fmt.Sprintf("%s failed=%v %s", r.Name, r.Failed, r.Payload))
	}
	return out
}

func TestADelegatedRCAsLedgerIsTheSubAgentsReads(t *testing.T) {
	var calls []string
	srv := delegatingServer(t, map[string][]byte{
		"sub-1": session(
			map[string]any{"id": "a", "name": "k8s_get_resources", "response": map[string]any{"output": "deployment web: 1/1"}},
			map[string]any{"id": "b", "name": "run_query", "response": map[string]any{"error": "Forbidden: cannot query"}},
			map[string]any{"id": "c", "name": "krateo_system__NS__clickstack_agent",
				"response": map[string]any{"result": "logs say X", "subagent_session_id": "sub-2"}},
		),
		"sub-2": session(map[string]any{"id": "d", "name": "search_logs", "response": map[string]any{"output": "3 rows"}}),
	}, &calls)
	defer srv.Close()
	got := ledgerLines(analyzeThrough(t, srv, "/api/a2a/krateo-system/autopilot"))
	want := []string{
		"k8s_get_resources failed=false deployment web: 1/1",
		"run_query failed=true Forbidden: cannot query",
		"search_logs failed=false 3 rows",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("ledger\n got %q\nwant %q", got, want)
	}
	for _, c := range calls[1:] {
		if !strings.HasPrefix(c, "GET /api/sessions/sub-") || !strings.HasSuffix(c, " Bearer jwt") {
			t.Errorf("session call %q", c)
		}
	}
}

func TestAnUnreadableSessionLeavesTheDelegationInTheLedger(t *testing.T) {
	var calls []string
	srv := delegatingServer(t, map[string][]byte{}, &calls)
	defer srv.Close()
	got := ledgerLines(analyzeThrough(t, srv, "/api/a2a/krateo-system/autopilot"))
	if len(got) != 1 || !strings.HasPrefix(got[0], "krateo_system__NS__incident_agent failed=false") {
		t.Errorf("ledger %q", got)
	}
}

func TestADirectCallHasNoSessionAPI(t *testing.T) {
	var calls []string
	srv := delegatingServer(t, map[string][]byte{"sub-1": session()}, &calls)
	defer srv.Close()
	got := ledgerLines(analyzeThrough(t, srv, "/"))
	if len(got) != 1 || len(calls) != 1 {
		t.Errorf("ledger %q, calls %q", got, calls)
	}
}
