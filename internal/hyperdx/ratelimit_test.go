package hyperdx_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/krateo-platformops/provider-runtime/pkg/reconciler/reconcilertest"

	"github.com/krateo-platformops/alert-provider/internal/httpx"
	"github.com/krateo-platformops/alert-provider/internal/hyperdx"
)

// The client reports rate-limit answers to provider-runtime's governor, as its conformance helpers
// check with recorded responses. AssertReportsRateLimits's healthy case is left out: its recorded
// body is plain text, which a JSON API's client rightly fails to decode, and the helper needs the
// call to succeed. The throttles and the advisory are what the governor acts on.
func TestReportsRateLimits(t *testing.T) {
	listAlerts := func(ctx context.Context, baseURL string) error {
		_, err := hyperdx.New(baseURL, "k").ListAlerts(ctx)
		return err
	}
	for _, name := range []string{reconcilertest.GitHub429RetryAfter, reconcilertest.AzureDevOps429Blocked,
		reconcilertest.Service503RetryAfterDate} {
		t.Run(name, func(t *testing.T) { reconcilertest.AssertReportsThrottle(t, name, listAlerts) })
	}
	t.Run(reconcilertest.AzureDevOps200Advisory, func(t *testing.T) {
		reconcilertest.AssertReportsAdvisory(t, reconcilertest.AzureDevOps200Advisory, listAlerts)
	})
}

// A 429's error is a throttled StatusError carrying the wait HyperDX asked for: Retry-After in
// seconds, as an HTTP-date, or the client's default when it names none. Its message is the one
// every other HTTP error has.
func TestA429IsAThrottledErrorWithItsRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header http.Header
		want   time.Duration
	}{
		{"seconds", http.Header{"Retry-After": {"7"}}, 7 * time.Second},
		{"date", http.Header{"Retry-After": {time.Now().Add(20 * time.Second).UTC().Format(http.TimeFormat)}}, 20 * time.Second},
		{"none", nil, 42 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, api := client(t)
			c.ThrottleDefault = 42 * time.Second
			api.FailHeader = tc.header
			api.Fail = func(string, string, string) int { return http.StatusTooManyRequests }
			_, err := c.ListAlerts(ctx)
			wait, ok := httpx.Throttled(err)
			if !ok {
				t.Fatalf("not throttled: %v", err)
			}
			if wait > tc.want || wait < tc.want-2*time.Second {
				t.Errorf("wait %v, want about %v", wait, tc.want)
			}
			eq(t, "code", httpx.Code(err), http.StatusTooManyRequests)
			eq(t, "message", err.Error(), "429 Client Error: Too Many Requests for url: "+api.URL+"/api/v2/alerts")
		})
	}
}

func TestA500IsNotThrottled(t *testing.T) {
	c, api := client(t)
	api.Fail = func(string, string, string) int { return http.StatusInternalServerError }
	_, err := c.ListAlerts(ctx)
	if _, ok := httpx.Throttled(err); ok || httpx.Code(err) != 500 {
		t.Fatalf("a 500 reads as %v", err)
	}
}

// OnWrite runs after every write to the alerts or dashboards, failed ones included, and after
// nothing else: not a read, not a webhook, not a charts/series query.
func TestOnWriteRunsAfterEveryAlertAndDashboardWrite(t *testing.T) {
	c, api := client(t)
	writes := 0
	c.OnWrite = func() { writes++ }
	src := map[string]any{"id": "src-1"}
	step := func(what string, want int, f func()) {
		t.Helper()
		before := writes
		f()
		eq(t, what, writes-before, want)
	}
	var wid, dash, tile, id string
	step("webhook", 0, func() { wid, _, _ = c.EnsureWebhook(ctx, "w", "http://x", "") })
	step("dashboard create", 1, func() { dash, tile, _ = c.EnsureTile(ctx, "d", src, hyperdx.CountSelect("x"), nil) })
	step("alert create", 1, func() {
		a, _ := c.EnsureAlert(ctx, "a", dash, tile, wid, hyperdx.AlertFields{Interval: "5m", Threshold: 1, ThresholdType: "above"})
		id, _ = a["id"].(string)
	})
	step("reads", 0, func() {
		_, _ = c.ListAlerts(ctx)
		_, _ = hyperdx.New(api.URL, "k").ListDashboards(ctx)
		_, _ = c.RecordCounts(ctx, "src-1", "x", 60, "Body")
	})
	step("alert update", 1, func() {
		_, _ = c.UpdateAlert(ctx, id, "a", dash, tile, wid, hyperdx.AlertFields{Interval: "5m", Threshold: 2, ThresholdType: "above"})
	})
	step("tile update", 1, func() { _ = c.UpdateTile(ctx, dash, "d", src, hyperdx.CountSelect("y"), tile) })
	step("alert delete", 1, func() { _ = c.DeleteAlert(ctx, id) })
	step("dashboard delete", 1, func() { _ = c.DeleteDashboard(ctx, dash) })
	api.Fail = func(string, string, string) int { return http.StatusBadGateway }
	step("a failed write", 1, func() { _ = c.DeleteAlert(ctx, "gone") })
}
