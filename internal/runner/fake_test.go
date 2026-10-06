package runner

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/harness"
	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

// fakeServer is an ex backend: it records every call and answers per path.
type fakeServer struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	calls   []call
	answers map[string]func(body map[string]any) (int, string)
}

type call struct {
	method, path, auth string
	body               map[string]any
}

func newFakeServer(t *testing.T) *fakeServer {
	f := &fakeServer{t: t, answers: map[string]func(map[string]any) (int, string){}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.calls = append(f.calls, call{r.Method, r.URL.Path, r.Header.Get("Authorization"), body})
		answer := f.answers[r.URL.Path]
		f.mu.Unlock()
		status, resp := 200, "{}"
		if answer != nil {
			status, resp = answer(body)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) on(path string, fn func(body map[string]any) (int, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[path] = fn
}

func (f *fakeServer) callsTo(path string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.path == path {
			out = append(out, c)
		}
	}
	return out
}

// events flattens every event batch the server received for a run.
func (f *fakeServer) events(runID string) []map[string]any {
	var out []map[string]any
	for _, c := range f.callsTo("/api/v1/agent/runner/runs/" + runID + "/events") {
		evs, _ := c.body["events"].([]any)
		for _, e := range evs {
			out = append(out, e.(map[string]any))
		}
	}
	return out
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fakeRun is a harness.Running driven by the test.
type fakeRun struct {
	mu      sync.Mutex
	done    chan struct{}
	outcome protocol.RunOutcome
	killed  []string
}

func newFakeRun() *fakeRun { return &fakeRun{done: make(chan struct{})} }

func (r *fakeRun) Wait() protocol.RunOutcome {
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outcome
}

func (r *fakeRun) finish(o protocol.RunOutcome) {
	r.mu.Lock()
	r.outcome = o
	r.mu.Unlock()
	select {
	case <-r.done:
	default:
		close(r.done)
	}
}

func (r *fakeRun) Kill(reason string) {
	r.mu.Lock()
	r.killed = append(r.killed, reason)
	r.mu.Unlock()
	r.finish(protocol.RunOutcome{Reason: reason})
}

func (r *fakeRun) kills() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.killed...)
}

// fakeHarness records each start and plays a script of outcomes.
type fakeHarness struct {
	mu     sync.Mutex
	starts []start
	script []func(sink harness.EventSink, r *fakeRun, opts harness.RunOptions)
}

type start struct {
	a    protocol.Assignment
	opts harness.RunOptions
	run  *fakeRun
}

func (h *fakeHarness) run(a protocol.Assignment, opts harness.RunOptions, sink harness.EventSink) harness.Running {
	r := newFakeRun()
	h.mu.Lock()
	i := len(h.starts)
	h.starts = append(h.starts, start{a, opts, r})
	var step func(harness.EventSink, *fakeRun, harness.RunOptions)
	if i < len(h.script) {
		step = h.script[i]
	}
	h.mu.Unlock()
	if step != nil {
		go step(sink, r, opts)
	}
	return r
}

func (h *fakeHarness) got() []start {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]start(nil), h.starts...)
}

// succeed emits a turn, usage, a tool call + result, then completes.
func succeed(text string) func(harness.EventSink, *fakeRun, harness.RunOptions) {
	return func(sink harness.EventSink, r *fakeRun, opts harness.RunOptions) {
		sink.Turn()
		sink.Usage(10, 5, &harness.Cache{Read: 100, Creation: 7})
		sink.Usage(1, 1, nil)
		sink.Progress("thinking " + strings.Repeat("x", 2100))
		sink.Tool("Bash", "run `ls`", map[string]any{"command": "ls"})
		sink.Tool("mcp__ex__post_message", "", map[string]any{"body": "hi"})
		sink.Tool("Unknown", "", nil)
		sink.ToolResult("Bash", "a.txt")
		if opts.OnSessionID != nil {
			opts.OnSessionID("sess-1")
		}
		r.finish(protocol.RunOutcome{OK: true, FinalText: text, Usage: protocol.Usage{InputTokens: 11, OutputTokens: 6}})
	}
}

func failWith(reason string) func(harness.EventSink, *fakeRun, harness.RunOptions) {
	return func(_ harness.EventSink, r *fakeRun, _ harness.RunOptions) {
		r.finish(protocol.RunOutcome{Reason: reason})
	}
}
