package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/api"
	"github.com/DigitalTolk/ex-runners/internal/connectors"
	"github.com/DigitalTolk/ex-runners/internal/detect"
	"github.com/DigitalTolk/ex-runners/internal/harness"
	"github.com/DigitalTolk/ex-runners/internal/protocol"
	"github.com/DigitalTolk/ex-runners/internal/sessions"
	"github.com/DigitalTolk/ex-runners/internal/workspace"
)

func init() {
	eventFlushEvery = 10 * time.Millisecond
	infraRetryPause = time.Millisecond
}

func newExec(t *testing.T, f *fakeServer, h *fakeHarness) *executor {
	t.Helper()
	state := t.TempDir()
	e := &executor{
		api:           api.New(f.srv.URL, func() string { return "runner-tok" }),
		runnerID:      "r1",
		baseURL:       f.srv.URL,
		workspaceRoot: t.TempDir(),
		harnesses: []detect.Harness{
			{RunnerHarness: protocol.RunnerHarness{Name: "claude", Authed: true}, Path: "/bin/claude"},
			{RunnerHarness: protocol.RunnerHarness{Name: "codex", Authed: true}, Path: "/bin/codex"},
		},
		searchPath: "/usr/bin",
		mcpEntry:   MCPEntry{Command: "/opt/ex-runner", Args: []string{"mcp-server"}},
		stateDir:   state,
		sessions:   sessions.Open(state),
		log:        protocol.NopLogger,
		runClaude:  h.run,
		runCodex:   h.run,
	}
	e.withDefaults()
	return e
}

func assignment(over func(a *protocol.Assignment)) protocol.Assignment {
	a := protocol.Assignment{
		RunID: "run1", AgentID: "a-gg", AgentName: "gg", InvokerID: "u-alice", InvokerName: "Alice",
		ParentID: "chan1", ParentType: "channel", MessageID: "m1", Harness: "claude", Persona: "be gg",
		Mode: "direct", Prompt: "what is 2+2?", ContextBundle: "# Thread\n...", MCPToken: "run-tok",
		Deadline: time.Now().Add(time.Hour).Format(time.RFC3339Nano),
	}
	if over != nil {
		over(&a)
	}
	return a
}

func TestExecuteMissingHarness(t *testing.T) {
	f := newFakeServer(t)
	e := newExec(t, f, &fakeHarness{})
	e.execute(assignment(func(a *protocol.Assignment) { a.Harness = "bedrock" }))
	fails := f.callsTo("/api/v1/agent/runner/runs/run1/fail")
	if len(fails) != 1 || fails[0].body["reason"] != "harness_missing:bedrock" {
		t.Fatalf("fail calls = %+v", fails)
	}
}

func TestExecuteSuccessReportsEventsAndCompletes(t *testing.T) {
	f := newFakeServer(t)
	h := &fakeHarness{script: []func(harness.EventSink, *fakeRun, harness.RunOptions){succeed("4")}}
	e := newExec(t, f, h)
	e.execute(assignment(nil))

	done := f.callsTo("/api/v1/agent/runner/runs/run1/complete")
	if len(done) != 1 || done[0].body["finalText"] != "4" || done[0].auth != "Bearer runner-tok" {
		t.Fatalf("complete = %+v", done)
	}
	eventually(t, "the ⚙️ status", func() bool { return len(f.callsTo("/api/v1/agent/run/state")) == 1 })
	if st := f.callsTo("/api/v1/agent/run/state")[0]; st.body["state"] != "⚙️" || st.auth != "Bearer run-tok" {
		t.Errorf("status = %+v", st)
	}
	types := []string{}
	var usage, progress, tool, toolNoInput map[string]any
	for _, ev := range f.events("run1") {
		types = append(types, ev["type"].(string))
		p, _ := ev["payload"].(map[string]any)
		switch ev["type"] {
		case "usage":
			if usage == nil {
				usage = p
			}
		case "progress":
			progress = p
		case "tool":
			if tool == nil {
				tool = p
			} else if p["name"] == "Unknown" {
				toolNoInput = p
			}
		}
	}
	joined := strings.Join(types, ",")
	for _, want := range []string{"state", "prompt", "turn", "usage", "progress", "tool", "tool_result"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s event in %s", want, joined)
		}
	}
	if usage["cacheReadTokens"] != float64(100) || usage["cacheCreationTokens"] != float64(7) {
		t.Errorf("usage = %v", usage)
	}
	if n := len([]rune(progress["text"].(string))); n != 2000 {
		t.Errorf("progress clipped to %d", n)
	}
	if in, _ := tool["input"].(map[string]any); in["command"] != "ls" || tool["detail"] != "run `ls`" {
		t.Errorf("tool = %v", tool)
	}
	if _, has := toolNoInput["input"]; has {
		t.Errorf("nil input must not be sent: %v", toolNoInput)
	}

	// The harness got the MCP server command with the run's env.
	opts := h.got()[0].opts
	env := opts.MCPServer.Env
	if opts.MCPServer.Command != "/opt/ex-runner" || env["EX_RUN_TOKEN"] != "run-tok" || env["EX_BASE_URL"] != f.srv.URL || env["EX_WORK_DIR"] != opts.WorkDir {
		t.Errorf("mcp = %+v", opts.MCPServer)
	}
	if opts.BinPath != "/bin/claude" || opts.SearchPath != "/usr/bin" || opts.PromptOverride != "" {
		t.Errorf("opts = %+v", opts)
	}
	if _, has := env["EX_TASK_ID"]; has {
		t.Error("task env on a chat run")
	}
}

func TestWarmSessionResumesAndFallsBack(t *testing.T) {
	f := newFakeServer(t)
	h := &fakeHarness{script: []func(harness.EventSink, *fakeRun, harness.RunOptions){
		succeed("first"),
		failWith("harness_exit_1"), // the resume dies
		succeed("cold again"),
	}}
	e := newExec(t, f, h)
	e.execute(assignment(nil))
	e.execute(assignment(func(a *protocol.Assignment) { a.RunID = "run2" }))

	starts := h.got()
	if len(starts) != 3 {
		t.Fatalf("starts = %d", len(starts))
	}
	if starts[1].opts.ResumeSessionID != "sess-1" || !strings.Contains(starts[1].opts.PromptOverride, "[continuing in the same thread]") {
		t.Errorf("resume opts = %+v", starts[1].opts)
	}
	if starts[2].opts.ResumeSessionID != "" || starts[2].opts.WorkDir != starts[0].opts.WorkDir {
		t.Errorf("cold retry opts = %+v", starts[2].opts)
	}
	if c := f.callsTo("/api/v1/agent/runner/runs/run2/complete"); len(c) != 1 || c[0].body["finalText"] != "cold again" {
		t.Errorf("run2 complete = %+v", c)
	}
}

func TestInfraFailureRetriesOnceThenFails(t *testing.T) {
	f := newFakeServer(t)
	h := &fakeHarness{script: []func(harness.EventSink, *fakeRun, harness.RunOptions){
		failWith("spawn_failed: ENOENT"),
		failWith("spawn_failed: ENOENT"),
	}}
	e := newExec(t, f, h)
	e.execute(assignment(func(a *protocol.Assignment) { a.Mode = "scheduled" })) // cold, temp workdir
	if len(h.got()) != 2 {
		t.Fatalf("starts = %d", len(h.got()))
	}
	if c := f.callsTo("/api/v1/agent/runner/runs/run1/fail"); len(c) != 1 || c[0].body["reason"] != "spawn_failed: ENOENT" {
		t.Errorf("fail = %+v", c)
	}
	// A scheduled run used a throwaway workdir, removed afterwards.
	if _, err := os.Stat(h.got()[0].opts.WorkDir); !errors.Is(err, os.ErrNotExist) {
		t.Error("temp workdir not removed")
	}
	// A model-level failure is not retried; an empty reason reports "unknown".
	f2 := newFakeServer(t)
	h2 := &fakeHarness{script: []func(harness.EventSink, *fakeRun, harness.RunOptions){failWith("")}}
	newExec(t, f2, h2).execute(assignment(nil))
	if len(h2.got()) != 1 || f2.callsTo("/api/v1/agent/runner/runs/run1/fail")[0].body["reason"] != "unknown" {
		t.Errorf("model failure handling: starts=%d", len(h2.got()))
	}
}

func TestBackendAbortKillsTheRun(t *testing.T) {
	f := newFakeServer(t)
	f.on("/api/v1/agent/runner/runs/run1/events", func(map[string]any) (int, string) {
		return 200, `{"abort":true,"reason":"turn_limit"}`
	})
	h := &fakeHarness{script: []func(harness.EventSink, *fakeRun, harness.RunOptions){
		func(sink harness.EventSink, _ *fakeRun, _ harness.RunOptions) { sink.Turn() }, // then hangs
	}}
	e := newExec(t, f, h)
	e.execute(assignment(nil))
	if k := h.got()[0].run.kills(); len(k) == 0 || k[0] != "abort:turn_limit" {
		t.Errorf("kills = %v", k)
	}
	if c := f.callsTo("/api/v1/agent/runner/runs/run1/fail"); len(c) != 1 || c[0].body["reason"] != "abort:turn_limit" {
		t.Errorf("fail = %+v", c)
	}
}

func TestEventBatchRetriesAfterFailure(t *testing.T) {
	f := newFakeServer(t)
	failures := 1
	f.on("/api/v1/agent/runner/runs/run1/events", func(map[string]any) (int, string) {
		if failures > 0 {
			failures--
			return 500, `{}`
		}
		return 200, `{"abort":true}`
	})
	h := &fakeHarness{script: []func(harness.EventSink, *fakeRun, harness.RunOptions){
		func(sink harness.EventSink, r *fakeRun, _ harness.RunOptions) {
			for i := 0; i < eventFlushCount+5; i++ { // trips the size trigger
				sink.Turn()
			}
		},
	}}
	e := newExec(t, f, h)
	e.execute(assignment(nil))
	// The failed batch was re-sent: every seq arrives exactly once in order.
	seen := map[float64]int{}
	for _, ev := range f.events("run1") {
		seen[ev["seq"].(float64)]++
	}
	if len(seen) < eventFlushCount {
		t.Errorf("seqs delivered = %d", len(seen))
	}
	if k := h.got()[0].run.kills(); len(k) == 0 || k[0] != "abort:aborted" {
		t.Errorf("an abort without a reason reads 'aborted': %v", k)
	}
}

func TestDeadlineKillsTheRun(t *testing.T) {
	f := newFakeServer(t)
	h := &fakeHarness{script: []func(harness.EventSink, *fakeRun, harness.RunOptions){
		func(harness.EventSink, *fakeRun, harness.RunOptions) {}, // hangs
	}}
	e := newExec(t, f, h)
	e.execute(assignment(func(a *protocol.Assignment) { a.Deadline = time.Now().Add(-time.Minute).Format(time.RFC3339Nano) }))
	if c := f.callsTo("/api/v1/agent/runner/runs/run1/fail"); len(c) != 1 || c[0].body["reason"] != "deadline" {
		t.Errorf("fail = %+v", c)
	}
	// A far-horizon (task) deadline arms no local timer.
	f2 := newFakeServer(t)
	h2 := &fakeHarness{script: []func(harness.EventSink, *fakeRun, harness.RunOptions){succeed("ok")}}
	newExec(t, f2, h2).execute(assignment(func(a *protocol.Assignment) {
		a.Deadline = time.Now().Add(90 * 24 * time.Hour).Format(time.RFC3339Nano)
	}))
	if len(f2.callsTo("/api/v1/agent/runner/runs/run1/complete")) != 1 {
		t.Error("far deadline run did not complete")
	}
}

func TestHarnessPanicIsAReportedRunnerError(t *testing.T) {
	f := newFakeServer(t)
	e := newExec(t, f, &fakeHarness{})
	e.runClaude = func(protocol.Assignment, harness.RunOptions, harness.EventSink) harness.Running { panic("boom") }
	e.execute(assignment(nil))
	if c := f.callsTo("/api/v1/agent/runner/runs/run1/fail"); len(c) != 1 || c[0].body["reason"] != "runner_error: boom" {
		t.Errorf("fail = %+v", c)
	}
}

func TestConnectorsReachTheHarness(t *testing.T) {
	f := newFakeServer(t)
	h := &fakeHarness{script: []func(harness.EventSink, *fakeRun, harness.RunOptions){succeed("ok"), succeed("ok")}}
	e := newExec(t, f, h)
	e.fetchConnectors = func(context.Context, string, string) ([]connectors.Connector, error) {
		return []connectors.Connector{{Slug: "gitlab", Title: "GitLab", BaseURL: "https://gitlab.example/api/v4", Token: "glpat"}}, nil
	}
	e.execute(assignment(func(a *protocol.Assignment) { a.ConnectorSlugs = []string{"gitlab"}; a.Mode = "scheduled" }))
	opts := h.got()[0].opts
	if !strings.Contains(opts.MCPServer.Env["EX_CONNECTORS"], `"slug":"gitlab"`) {
		t.Errorf("EX_CONNECTORS = %q", opts.MCPServer.Env["EX_CONNECTORS"])
	}
	if cred := gitlabCred([]connectors.Connector{{Slug: "gitlab", BaseURL: "https://gitlab.example/api/v4", Token: "t"}}); cred == nil || cred.Token != "t" {
		t.Errorf("gitlabCred = %+v", cred)
	}
	if gitlabCred(nil) != nil {
		t.Error("no gitlab row → no cred")
	}
	// A failed fetch downgrades to "no connectors".
	e.fetchConnectors = func(context.Context, string, string) ([]connectors.Connector, error) { return nil, errors.New("down") }
	e.execute(assignment(func(a *protocol.Assignment) { a.RunID = "run2"; a.ConnectorSlugs = []string{"x"} }))
	if _, has := h.got()[1].opts.MCPServer.Env["EX_CONNECTORS"]; has {
		t.Error("connectors env set after a failed fetch")
	}
}

func TestCodingTaskRun(t *testing.T) {
	f := newFakeServer(t)
	h := &fakeHarness{script: []func(harness.EventSink, *fakeRun, harness.RunOptions){succeed("done")}}
	e := newExec(t, f, h)
	project := filepath.Join(e.workspaceRoot, "cliffhub")
	e.prepareWorkspace = func(_ context.Context, task *protocol.TaskSpec, _ *workspace.GitCred, _ protocol.Logger, root string) (workspace.PrepareResult, error) {
		return workspace.PrepareResult{
			ProjectDir: project,
			Note:       "📦 Workspace ready",
			Repos: []workspace.PreparedRepo{
				{Path: "acme/web", Role: "frontend", Dir: filepath.Join(project, "web"), Branch: "ex/fix", BaseBranch: "main", Cloned: true},
				{Path: "acme/api", Role: "backend", Error: "clone failed"},
			},
		}, nil
	}
	task := &protocol.TaskSpec{ID: "t1", ProjectKey: "cliffhub", ProjectName: "CliffHub", Title: "Fix login", Kind: "bug", State: "created"}
	e.execute(assignment(func(a *protocol.Assignment) { a.Mode = "task"; a.Task = task }))

	opts := h.got()[0].opts
	env := opts.MCPServer.Env
	if opts.Cwd != project || env["EX_TASK_ID"] != "t1" || env["EX_TASK_DIR"] != project || env["EX_TASK_PROJECT"] != "cliffhub" || env["EX_WORKSPACE_ROOT"] != e.workspaceRoot {
		t.Errorf("task opts = %+v env=%v", opts, env)
	}
	if !strings.Contains(env["EX_TASK_REPOS"], `"path":"acme/web"`) || strings.Contains(env["EX_TASK_REPOS"], "acme/api") {
		t.Errorf("EX_TASK_REPOS = %s", env["EX_TASK_REPOS"])
	}
	if !strings.Contains(opts.PromptOverride, "[coding task] You are working on bug \"Fix login\" for CliffHub on behalf of Alice.") {
		t.Errorf("prompt = %s", opts.PromptOverride)
	}
	rep := f.callsTo("/api/v1/agent/run/coding-task/report")
	if len(rep) != 1 || rep[0].body["state"] != "workspace_ready" || rep[0].body["note"] != "📦 Workspace ready" || rep[0].auth != "Bearer run-tok" {
		t.Errorf("report = %+v", rep)
	}

	// Preparation failure: announced in the thread and failed.
	f2 := newFakeServer(t)
	e2 := newExec(t, f2, &fakeHarness{})
	e2.prepareWorkspace = func(context.Context, *protocol.TaskSpec, *workspace.GitCred, protocol.Logger, string) (workspace.PrepareResult, error) {
		return workspace.PrepareResult{}, errors.New("git not found")
	}
	e2.execute(assignment(func(a *protocol.Assignment) { a.Mode = "task"; a.Task = task }))
	if r := f2.callsTo("/api/v1/agent/run/coding-task/report"); len(r) != 1 || r[0].body["state"] != "setup_failed" {
		t.Errorf("setup_failed report = %+v", r)
	}
	if c := f2.callsTo("/api/v1/agent/runner/runs/run1/fail"); len(c) != 1 || c[0].body["reason"] != "workspace_failed: git not found" {
		t.Errorf("fail = %+v", c)
	}

	// An unreachable server only logs the report failure.
	e3 := newExec(t, f2, &fakeHarness{})
	e3.baseURL = "http://127.0.0.1:1"
	e3.report(assignment(nil), map[string]any{"state": "x"})
	e3.baseURL = "http://bad host"
	e3.report(assignment(nil), map[string]any{"state": "x"})
}

func TestClipToolInput(t *testing.T) {
	long := strings.Repeat("x", 3000)
	cases := []struct {
		name  string
		input map[string]any
		check func(map[string]any) bool
	}{
		{"Bash", map[string]any{"command": long, "description": "d"}, func(o map[string]any) bool {
			return len([]rune(o["command"].(string))) == 2001 && o["description"] == "d"
		}},
		{"mcp__ex__shell", map[string]any{"command": "ls"}, func(o map[string]any) bool { return o["command"] == "ls" }},
		{"Edit", map[string]any{"file_path": "/a", "old_string": "o", "new_string": "n"}, func(o map[string]any) bool { return o["old_string"] == "o" && o["new_string"] == "n" }},
		{"Write", map[string]any{"file_path": "/a", "content": "c"}, func(o map[string]any) bool { return o["content"] == "c" }},
		{"Read", map[string]any{"file_path": "/a", "offset": float64(3), "limit": float64(9)}, func(o map[string]any) bool { return o["offset"] == float64(3) && o["limit"] == float64(9) }},
		{"Grep", map[string]any{"pattern": "p", "path": "/x"}, func(o map[string]any) bool { return o["pattern"] == "p" && o["path"] == "/x" }},
		{"WebFetch", map[string]any{"url": "https://x"}, func(o map[string]any) bool { return o["url"] == "https://x" }},
		{"WebSearch", map[string]any{"query": "q"}, func(o map[string]any) bool { return o["query"] == "q" }},
		{"Other", map[string]any{"k": "v"}, func(o map[string]any) bool { return o["json"] == `{"k":"v"}` }},
		{"Other", map[string]any{"c": make(chan int)}, func(o map[string]any) bool { return o == nil }},
		{"Bash", map[string]any{"command": ""}, func(o map[string]any) bool { return o == nil }},
		{"Read", map[string]any{"file_path": 7}, func(o map[string]any) bool { return o == nil }},
	}
	for _, c := range cases {
		if got := clipToolInput(c.name, c.input); !c.check(got) {
			t.Errorf("clipToolInput(%s, %v) = %v", c.name, c.input, got)
		}
	}
	if clipToolInput("Bash", nil) != nil {
		t.Error("nil input")
	}
	if !infraFailure("spawn_failed: x") || !infraFailure("harness_exit_2") || infraFailure("deadline") || infraFailure("") {
		t.Error("infraFailure")
	}
}

func TestExecuteFailsWhenNoTempDir(t *testing.T) {
	f := newFakeServer(t)
	e := newExec(t, f, &fakeHarness{})
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	e.execute(assignment(func(a *protocol.Assignment) { a.Mode = "scheduled" }))
	if c := f.callsTo("/api/v1/agent/runner/runs/run1/fail"); len(c) != 1 || !strings.HasPrefix(c[0].body["reason"].(string), "runner_error: ") {
		t.Errorf("fail = %+v", c)
	}
	// Reporting failures are logged, never fatal.
	e.api = api.New("http://127.0.0.1:1", func() string { return "t" })
	e.fail("run1", "x")
}

func TestExecuteFailsWhenThePrivateDirCannotBeMade(t *testing.T) {
	f := newFakeServer(t)
	e := newExec(t, f, &fakeHarness{})
	orig := mkdirTemp
	defer func() { mkdirTemp = orig }()
	mkdirTemp = func(dir, pattern string) (string, error) {
		if strings.Contains(pattern, "private") {
			return "", errors.New("disk full")
		}
		return orig(dir, pattern)
	}
	e.execute(assignment(func(a *protocol.Assignment) { a.Mode = "scheduled" }))
	if c := f.callsTo("/api/v1/agent/runner/runs/run1/fail"); len(c) != 1 || c[0].body["reason"] != "runner_error: disk full" {
		t.Errorf("fail = %+v", c)
	}
}

// Secrets live in an owner-only dir outside the workdir; the MCP server
// learns where it and the runner's home are; mcp.json goes with the run.
func TestRunsGetAPrivateDirOutsideTheWorkdir(t *testing.T) {
	f := newFakeServer(t)
	var seen harness.RunOptions
	h := &fakeHarness{script: []func(harness.EventSink, *fakeRun, harness.RunOptions){func(sink harness.EventSink, r *fakeRun, opts harness.RunOptions) {
		seen = opts
		_ = os.WriteFile(filepath.Join(opts.PrivateDir, "mcp.json"), []byte("{}"), 0o600)
		succeed("ok")(sink, r, opts)
	}}}
	e := newExec(t, f, h)
	e.execute(assignment(nil))
	env := seen.MCPServer.Env
	if seen.PrivateDir == "" || strings.HasPrefix(seen.PrivateDir, seen.WorkDir) || env["EX_PRIVATE_DIR"] != seen.PrivateDir {
		t.Fatalf("private dir = %q (workdir %q, env %q)", seen.PrivateDir, seen.WorkDir, env["EX_PRIVATE_DIR"])
	}
	if env["EX_RUNNER_HOME"] != filepath.Dir(e.stateDir) {
		t.Errorf("EX_RUNNER_HOME = %q", env["EX_RUNNER_HOME"])
	}
	if st, err := os.Stat(seen.PrivateDir); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("private dir: %v %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(seen.PrivateDir, "mcp.json")); err == nil {
		t.Error("mcp.json outlived the run")
	}
}

func TestCompleteReportFailureIsLogged(t *testing.T) {
	f := newFakeServer(t)
	f.on("/api/v1/agent/runner/runs/run1/complete", func(map[string]any) (int, string) { return 500, `{}` })
	f.on("/api/v1/agent/run/state", func(map[string]any) (int, string) { return 403, `{}` })
	var mu sync.Mutex
	var logged []string
	seen := func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(logged, "|")
	}
	h := &fakeHarness{script: []func(harness.EventSink, *fakeRun, harness.RunOptions){succeed("ok")}}
	e := newExec(t, f, h)
	e.log = func(msg string, _ map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, msg)
	}
	e.execute(assignment(nil))
	eventually(t, "status failure log", func() bool { return strings.Contains(seen(), "status set failed") })
	if !strings.Contains(seen(), "complete report failed") {
		t.Errorf("logged = %v", seen())
	}
}

func TestExecutorDefaultsToTheRealHarnesses(t *testing.T) {
	e := &executor{}
	e.withDefaults()
	if e.runClaude == nil || e.runCodex == nil || e.fetchConnectors == nil || e.prepareWorkspace == nil {
		t.Error("withDefaults left a seam empty")
	}
	customizeExecutor(e) // the production hook is a no-op
}

func TestCodexRunCarriesAutoAllowAndEmptyRepos(t *testing.T) {
	f := newFakeServer(t)
	h := &fakeHarness{script: []func(harness.EventSink, *fakeRun, harness.RunOptions){succeed("ok")}}
	e := newExec(t, f, h)
	e.prepareWorkspace = func(context.Context, *protocol.TaskSpec, *workspace.GitCred, protocol.Logger, string) (workspace.PrepareResult, error) {
		return workspace.PrepareResult{ProjectDir: e.workspaceRoot, Repos: []workspace.PreparedRepo{{Path: "acme/api", Error: "clone failed"}}}, nil
	}
	task := &protocol.TaskSpec{ID: "t1", ProjectKey: "cliffhub", ProjectName: "CliffHub", Title: "Fix", Kind: "bug", State: "created"}
	e.execute(assignment(func(a *protocol.Assignment) {
		a.Harness, a.AutoAllow, a.Mode, a.Task = "codex", []string{"read", "web"}, "task", task
	}))
	got := h.got()
	if len(got) != 1 || got[0].opts.BinPath != "/bin/codex" {
		t.Fatalf("starts = %+v", got)
	}
	env := got[0].opts.MCPServer.Env
	if env["EX_AUTO_ALLOW"] != "read,web" || env["EX_TASK_REPOS"] != "[]" {
		t.Errorf("env = %v", env)
	}
}

func TestPumpFinishWaitsOutAnInFlightFlush(t *testing.T) {
	f := newFakeServer(t)
	e := newExec(t, f, &fakeHarness{})
	p := newPump(e, "run1", func(string) {})
	p.start()
	p.push("text", map[string]any{"text": "hi"})
	p.mu.Lock()
	p.sending = true // a size-triggered flush is mid-request
	p.mu.Unlock()
	go func() {
		time.Sleep(50 * time.Millisecond)
		p.mu.Lock()
		p.sending = false
		p.mu.Unlock()
	}()
	p.finish()
	if ev := f.events("run1"); len(ev) != 1 {
		t.Errorf("events = %v", ev)
	}
}
