package tracingtest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"

	"github.com/krateo-platformops/alert-provider/internal/tracing"
)

// Logs captures what the production log pipeline writes: provider-runtime's slog Logger over
// NewOTelJSONHandler, built as main builds it, into memory instead of stderr.
type Logs struct {
	t   testing.TB
	mu  sync.Mutex
	buf bytes.Buffer
	// Logger is the Logger to hand the code under test.
	Logger logging.Logger
}

// Line is one record the pipeline wrote: its message, and the span it names ("" when none).
type Line struct {
	Msg     string `json:"msg"`
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`
}

// CaptureLogs returns a Logs whose Logger writes every level.
func CaptureLogs(t testing.TB) *Logs {
	l := &Logs{t: t}
	h := logging.NewOTelJSONHandler(slog.LevelDebug, l, logging.ServiceNameAttr(tracing.DefaultServiceName)...)
	l.Logger = logging.NewSlogLogger(*slog.New(h))
	return l
}

// Write is the handler's sink; records may come from several goroutines.
func (l *Logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// Lines are the records written so far.
func (l *Logs) Lines() []Line {
	l.t.Helper()
	l.mu.Lock()
	raw := l.buf.String()
	l.mu.Unlock()
	var out []Line
	sc := bufio.NewScanner(strings.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var line Line
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			l.t.Fatalf("a log record is not JSON: %v: %s", err, sc.Text())
		}
		out = append(out, line)
	}
	return out
}

// One is the one record whose message contains sub; the test fails unless there is exactly one.
func (l *Logs) One(sub string) Line {
	l.t.Helper()
	var got []Line
	for _, line := range l.Lines() {
		if strings.Contains(line.Msg, sub) {
			got = append(got, line)
		}
	}
	if len(got) != 1 {
		l.t.Fatalf("%d log records contain %q, want 1; written: %v", len(got), sub, l.Lines())
	}
	return got[0]
}

// In fails the test unless the record names span s: its trace and its span.
func (line Line) In(t testing.TB, s tracetest.SpanStub) {
	t.Helper()
	if line.TraceID != s.SpanContext.TraceID().String() || line.SpanID != s.SpanContext.SpanID().String() {
		t.Errorf("log %q names trace %q span %q; want %s's trace %s span %s",
			line.Msg, line.TraceID, line.SpanID, s.Name, s.SpanContext.TraceID(), s.SpanContext.SpanID())
	}
}

// Unbound fails the test when the record names any span.
func (line Line) Unbound(t testing.TB) {
	t.Helper()
	if line.TraceID != "" || line.SpanID != "" {
		t.Errorf("log %q names trace %q span %q; want none", line.Msg, line.TraceID, line.SpanID)
	}
}
