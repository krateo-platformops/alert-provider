package incident

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/krateo-platformops/alert-provider/internal/httpx"
	"github.com/krateo-platformops/alert-provider/internal/pyfmt"
	"github.com/krateo-platformops/alert-provider/internal/report"
)

// maxSessionDepth bounds how deep sessionReads follows delegations: autopilot to incident-agent
// to the specialists incident-agent asks in turn.
const maxSessionDepth = 3

// sessionTimeout bounds one session read.
const sessionTimeout = 30 * time.Second

// sessionsURL is kagent's session API next to the A2A URL: <gateway>/api/a2a/<ns>/<agent> serves
// <gateway>/api/sessions/<id>. Called directly, an agent serves no session API, so there is none.
func (a *A2A) sessionsURL() string {
	i := strings.Index(a.URL, "/api/a2a/")
	if i < 0 {
		return ""
	}
	return a.URL[:i] + "/api/sessions/"
}

// sessionReads are the tool results of the sub-agent sessions the RCA delegated to, and of the
// sessions those delegated to in turn. A sub-agent's reads never reach the parent's stream: its
// session holds them, and kagent scopes it to the caller, who owns the RCA's sessions. It returns
// what it read, and an error when a session could not be read.
func (a *A2A) sessionReads(ctx context.Context, sessions []string) ([]report.ToolResult, error) {
	base := a.sessionsURL()
	if base == "" {
		return nil, errors.New("no session API: the RCA agent is not called through the gateway")
	}
	var out []report.ToolResult
	var errs []error
	visited := map[string]bool{}
	level := sessions
	for depth := 0; depth < maxSessionDepth && len(level) > 0; depth++ {
		var next []string
		for _, id := range level {
			if visited[id] {
				continue
			}
			visited[id] = true
			results, err := a.sessionResults(ctx, base+url.PathEscape(id))
			if err != nil {
				errs = append(errs, fmt.Errorf("session %s: %w", id, err))
				continue
			}
			for _, tr := range results {
				if tr.session != "" {
					next = append(next, tr.session)
					continue
				}
				out = append(out, tr.ToolResult)
			}
		}
		level = next
	}
	return out, errors.Join(errs...)
}

// sessionResults are the tool results in one kagent session: GET /api/sessions/<id> answers
// {data: {events: [{data}]}}, each event's data an ADK event as JSON whose Content.parts carry
// functionResponse {name, response}.
func (a *A2A) sessionResults(ctx context.Context, u string) ([]toolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, sessionTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if jwt := a.JWT(ctx); jwt != "" {
		req.Header.Set("Authorization", "Bearer "+jwt)
	}
	resp, err := a2aHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := httpx.Check(resp); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var page struct {
		Data struct {
			Events []struct {
				Data string `json:"data"`
			} `json:"events"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("session response: %w", err)
	}
	var out []toolResult
	for _, e := range page.Data.Events {
		v, err := pyfmt.Decode([]byte(e.Data))
		if err != nil {
			continue
		}
		ev, _ := v.(map[string]any)
		content, _ := ev["Content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			if fr, ok := pm["functionResponse"].(map[string]any); ok {
				out = append(out, newToolResult(fr["name"], fr["response"]))
			}
		}
	}
	return out, nil
}
