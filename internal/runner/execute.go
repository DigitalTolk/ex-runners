package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/api"
	"github.com/DigitalTolk/ex-runners/internal/connectors"
	"github.com/DigitalTolk/ex-runners/internal/detect"
	"github.com/DigitalTolk/ex-runners/internal/harness"
	"github.com/DigitalTolk/ex-runners/internal/protocol"
	"github.com/DigitalTolk/ex-runners/internal/sessions"
	"github.com/DigitalTolk/ex-runners/internal/textutil"
	"github.com/DigitalTolk/ex-runners/internal/workspace"
)

// Per-assignment execution: scratch dir, MCP config, harness spawn, event
// pump, and the local (advisory) limit enforcement. The backend re-enforces
// every bound authoritatively (plan-v2 §9).

var (
	eventFlushEvery = time.Second
	infraRetryPause = 3 * time.Second
	// mkdirTemp is os.MkdirTemp; tests make one call fail.
	mkdirTemp = os.MkdirTemp
)

const eventFlushCount = 20

// executor runs claimed assignments. Everything it touches outside the
// process arrives through these fields, so tests can swap any of them.
type executor struct {
	api           *api.Client
	runnerID      string
	baseURL       string
	workspaceRoot string
	harnesses     []detect.Harness
	searchPath    string
	mcpEntry      MCPEntry
	stateDir      string
	sessions      *sessions.Store
	log           protocol.Logger
	// onHarness hands the caller a kill switch for the spawned harness the
	// moment it exists — the heartbeat's kill list must be able to stop a
	// WEDGED harness, and a wedged harness isn't calling home to hear "abort".
	onHarness func(runID string, kill func(reason string))

	// seams
	runClaude, runCodex func(a protocol.Assignment, opts harness.RunOptions, sink harness.EventSink) harness.Running
	fetchConnectors     func(ctx context.Context, baseURL, runToken string) ([]connectors.Connector, error)
	prepareWorkspace    func(ctx context.Context, task *protocol.TaskSpec, cred *workspace.GitCred, log protocol.Logger, root string) (workspace.PrepareResult, error)
}

func (e *executor) withDefaults() {
	if e.runClaude == nil {
		e.runClaude = harness.RunClaude
	}
	if e.runCodex == nil {
		e.runCodex = harness.RunCodex
	}
	if e.fetchConnectors == nil {
		e.fetchConnectors = connectors.FetchRunConnectors
	}
	if e.prepareWorkspace == nil {
		e.prepareWorkspace = workspace.Prepare
	}
}

// clipToolInput keeps the drawer-relevant fields of a harness tool call,
// each bounded, so the timeline can render commands, file chips and diffs
// without storing whole files.
func clipToolInput(name string, input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	out := map[string]any{}
	put := func(k string, v any) {
		if s, ok := v.(string); ok && s == "" {
			return
		}
		if v != nil {
			out[k] = v
		}
	}
	s := func(k string, n int) any {
		v, ok := input[k].(string)
		if !ok {
			return nil
		}
		return textutil.Clip(v, n)
	}
	switch strings.TrimPrefix(name, "mcp__ex__") {
	case "Bash", "shell":
		put("command", s("command", 2000))
		put("description", s("description", 200))
	case "Edit", "MultiEdit":
		put("file_path", s("file_path", 400))
		put("old_string", s("old_string", 2500))
		put("new_string", s("new_string", 2500))
	case "Write":
		put("file_path", s("file_path", 400))
		put("content", s("content", 2500))
	case "Read":
		put("file_path", s("file_path", 400))
		if n, ok := input["offset"].(float64); ok {
			put("offset", n)
		}
		if n, ok := input["limit"].(float64); ok {
			put("limit", n)
		}
	case "Glob", "Grep":
		put("pattern", s("pattern", 300))
		put("path", s("path", 400))
	case "WebFetch":
		put("url", s("url", 400))
	case "WebSearch":
		put("query", s("query", 300))
	default:
		if raw, err := json.Marshal(input); err == nil {
			put("json", textutil.Clip(string(raw), 800))
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// eventPump batches runner events toward the backend. Each event carries a
// monotonic seq (idempotent server-side); an abort response kills the run.
type eventPump struct {
	e       *executor
	runID   string
	onAbort func(reason string)

	mu      sync.Mutex
	buf     []protocol.RunEventInput
	seq     int
	sending bool
	stop    chan struct{}
	stopped sync.WaitGroup
}

func newPump(e *executor, runID string, onAbort func(string)) *eventPump {
	return &eventPump{e: e, runID: runID, onAbort: onAbort, stop: make(chan struct{})}
}

func (p *eventPump) start() {
	p.stopped.Add(1)
	go func() {
		defer p.stopped.Done()
		t := time.NewTicker(eventFlushEvery)
		defer t.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-t.C:
				p.flush()
			}
		}
	}()
}

func (p *eventPump) push(typ string, payload map[string]any) {
	p.mu.Lock()
	p.seq++
	p.buf = append(p.buf, protocol.RunEventInput{Seq: p.seq, Type: typ, Payload: payload})
	full := len(p.buf) >= eventFlushCount
	p.mu.Unlock()
	if full {
		go p.flush()
	}
}

func (p *eventPump) flush() {
	p.mu.Lock()
	if p.sending || len(p.buf) == 0 {
		p.mu.Unlock()
		return
	}
	p.sending = true
	batch := p.buf
	p.buf = nil
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	res, err := p.e.api.Events(ctx, p.e.runnerID, p.runID, batch)
	cancel()

	p.mu.Lock()
	if err != nil {
		// Re-queue in order for the next tick — seqs make the retry idempotent.
		p.buf = append(batch, p.buf...)
	}
	p.sending = false
	p.mu.Unlock()
	if err != nil {
		p.e.log("event batch failed; will retry", map[string]any{"runID": p.runID, "error": err.Error()})
		return
	}
	if res.Abort {
		reason := res.Reason
		if reason == "" {
			reason = "aborted"
		}
		p.onAbort(reason)
	}
}

// finish stops the ticker and sends what's left. A flush already in flight
// (the size trigger) is waited out so its events aren't left behind.
func (p *eventPump) finish() {
	close(p.stop)
	p.stopped.Wait()
	for i := 0; i < 50; i++ {
		p.mu.Lock()
		busy := p.sending
		p.mu.Unlock()
		if !busy {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.flush()
}

// sink adapts harness events onto the pump.
type sink struct{ p *eventPump }

func (s sink) Turn() { s.p.push("turn", nil) }

func (s sink) Usage(in, out int, cache *harness.Cache) {
	payload := map[string]any{"inputTokens": in, "outputTokens": out}
	// Cache split (when the harness reports one) — spend math stays
	// fresh-input-only, but without these fields a resume's 130k-token turn
	// is indistinguishable from 130k of real work.
	if cache != nil {
		payload["cacheReadTokens"] = cache.Read
		payload["cacheCreationTokens"] = cache.Creation
	}
	s.p.push("usage", payload)
}

func (s sink) Progress(text string) {
	s.p.push("progress", map[string]any{"text": textutil.Head(text, 2000)})
}

func (s sink) Tool(name, detail string, input map[string]any) {
	payload := map[string]any{"name": name}
	if detail != "" {
		payload["detail"] = textutil.Head(detail, 300)
	}
	if clipped := clipToolInput(name, input); clipped != nil {
		payload["input"] = clipped
	}
	s.p.push("tool", payload)
}

// Results carry more than a snippet — the drawer shows command output and
// file contents inline, IDE-style.
func (s sink) ToolResult(name, detail string) {
	s.p.push("tool_result", map[string]any{"name": name, "detail": textutil.Head(detail, 1500)})
}

// gitlabCred picks the gitlab connector out of the run's connector rows —
// the clone/push credential and the MR API token, never written to disk.
func gitlabCred(rows []connectors.Connector) *workspace.GitCred {
	for _, r := range rows {
		if r.Slug == "gitlab" {
			return &workspace.GitCred{Host: workspace.GitHostFromBaseURL(r.BaseURL), Token: r.Token}
		}
	}
	return nil
}

var harnessExit = regexp.MustCompile(`^harness_exit_`)

// infraFailure: failures worth one retry — the CLI never started or died
// abnormally. Model/task failures and deliberate kills are not retried.
func infraFailure(reason string) bool {
	return strings.HasPrefix(reason, "spawn_failed") || harnessExit.MatchString(reason)
}

// taskRepo is one checkout handed to the MCP server (EX_TASK_REPOS).
type taskRepo struct {
	Path   string `json:"path"`
	Role   string `json:"role"`
	Dir    string `json:"dir"`
	Branch string `json:"branch"`
	Base   string `json:"base"`
}

// report posts a coding-task lifecycle update with the run token.
func (e *executor) report(a protocol.Assignment, body map[string]any) {
	raw, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/api/v1/agent/run/coding-task/report", bytes.NewReader(raw))
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+a.MCPToken)
		req.Header.Set("Content-Type", "application/json")
		var res *http.Response
		if res, err = http.DefaultClient.Do(req); err == nil {
			_ = res.Body.Close()
		}
	}
	if err != nil {
		e.log("task report failed", map[string]any{"runID": a.RunID, "error": err.Error()})
	}
}

func (e *executor) fail(runID, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.api.Fail(ctx, e.runnerID, runID, reason); err != nil {
		e.log("fail report failed", map[string]any{"runID": runID, "error": err.Error()})
	}
}

// execute runs one claimed task to completion and reports the outcome.
// Never panics out — every failure path lands in api.Fail.
//
// Claude and codex runs get warm sessions: round N of a conversation
// resumes round N-1's session (per agent+thread) with a compact delta prompt
// instead of cold-starting and re-sending the whole bundle. A failed resume
// falls back to a fresh session; infra failures get one retry.
func (e *executor) execute(a protocol.Assignment) {
	var h *detect.Harness
	for i := range e.harnesses {
		if e.harnesses[i].Name == a.Harness {
			h = &e.harnesses[i]
			break
		}
	}
	if h == nil {
		e.fail(a.RunID, "harness_missing:"+a.Harness)
		return
	}

	// A scheduled firing starts cold: it is self-contained, and resuming made
	// every firing re-send all earlier firings' turns — hours apart, so the
	// prompt cache had always expired and the whole history was re-written at
	// full price, growing every day.
	canResume := (a.Harness == "claude" || a.Harness == "codex") && e.stateDir != "" && a.Mode != "scheduled"
	key := sessions.ThreadKey(a)
	var session *sessions.Entry
	if canResume {
		session = e.sessions.Get(a)
	}
	// Sessions are bound to their cwd: a resumable thread keeps a stable
	// workDir; everything else gets a swept temp dir.
	var workDir, tmpDir, privateDir string
	if canResume {
		workDir = sessions.WorkDir(e.stateDir, key)
		privateDir = sessions.PrivateDir(e.stateDir, key)
	} else {
		dir, err := mkdirTemp("", "ex-run-")
		if err != nil {
			e.fail(a.RunID, "runner_error: "+err.Error())
			return
		}
		tmpDir, workDir = dir, dir
		defer func() { _ = os.RemoveAll(tmpDir) }()
		// Owner-only and outside the workdir (MkdirTemp makes it 0700).
		if privateDir, err = mkdirTemp("", "ex-run-private-"); err != nil {
			e.fail(a.RunID, "runner_error: "+err.Error())
			return
		}
		defer func() { _ = os.RemoveAll(privateDir) }()
	}
	// The run token in mcp.json dies with the run; the file goes with it.
	defer func() { _ = os.Remove(filepath.Join(privateDir, "mcp.json")) }()

	var runMu sync.Mutex
	var running harness.Running
	kill := func(reason string) {
		runMu.Lock()
		r := running
		runMu.Unlock()
		if r != nil {
			r.Kill(reason)
		}
	}
	pump := newPump(e, a.RunID, func(reason string) {
		e.log("backend ordered abort", map[string]any{"runID": a.RunID, "reason": reason})
		kill("abort:" + reason)
	})

	// Local wall-clock fast-fail (the authoritative deadline lives
	// server-side). Task runs carry a far horizon; like the TypeScript runner
	// (whose timers could not represent it), only arm for near deadlines.
	if deadline, err := time.Parse(time.RFC3339Nano, a.Deadline); err == nil {
		if d := time.Until(deadline); d < 2_147_000_000*time.Millisecond {
			timer := time.AfterFunc(max(d, time.Second), func() { kill("deadline") })
			defer timer.Stop()
		}
	}

	// Connectors: only when the invoking message explicitly picked services
	// with /slug tokens (the server adds gitlab to coding-task runs). A
	// fetch failure downgrades to "no connectors" rather than failing the run.
	setup := connectors.EmptySetup
	var gitCred *workspace.GitCred
	if len(a.ConnectorSlugs) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		rows, err := e.fetchConnectors(ctx, e.baseURL, a.MCPToken)
		cancel()
		if err != nil {
			e.log("connector sync failed; continuing without", map[string]any{"runID": a.RunID, "error": err.Error()})
		} else {
			setup = connectors.Sync(filepath.Join(workDir, "connectors"), rows, e.log)
			gitCred = gitlabCred(rows)
		}
	}

	// Coding task: prepare the project checkout BEFORE the harness starts and
	// narrate it in the task thread. The harness then runs INSIDE the
	// checkout (cwd) while our per-run files stay in workDir.
	root := e.workspaceRoot
	if root == "" {
		root = workspace.Root()
	}
	var taskCwd, taskText string
	var repos []taskRepo
	if a.Task != nil {
		prep, err := e.prepareWorkspace(context.Background(), a.Task, gitCred, e.log, root)
		if err != nil {
			why := err.Error()
			e.log("workspace preparation failed", map[string]any{"runID": a.RunID, "error": why})
			e.report(a, map[string]any{"state": "setup_failed", "note": "⚠️ Couldn't prepare the workspace: " + textutil.Head(why, 1500)})
			e.fail(a.RunID, "workspace_failed: "+textutil.Head(why, 300))
			return
		}
		taskCwd = prep.ProjectDir
		reported := []map[string]any{}
		partial := false
		for _, r := range prep.Repos {
			if r.Error != "" {
				partial = true
				continue
			}
			repos = append(repos, taskRepo{Path: r.Path, Role: r.Role, Dir: r.Dir, Branch: r.Branch, Base: r.BaseBranch})
			reported = append(reported, map[string]any{"path": r.Path, "branch": r.Branch, "base_branch": r.BaseBranch, "workspace_dir": workspace.ShortenHome(r.Dir)})
		}
		taskText = taskPreamble(a, prep, workspace.LoadRegistry(root))
		// First run announces the workspace; later runs only re-pin the facts
		// silently. A partially failed preparation is always announced.
		first := a.Task.State == "created" || a.Task.State == "setup_failed"
		state, note := "", ""
		if first {
			state = "workspace_ready"
		}
		if first || partial {
			note = prep.Note
		}
		e.report(a, map[string]any{"state": state, "note": note, "repos": reported})
	}

	// Preamble order: the coding-task workspace frame, then the run's mode
	// frame (watcher/follow-up), then the connected-services priority block —
	// all ride the top of the prompt so they outrank everything in the bundle.
	preamble := taskText + modePreamble(a)
	if setup.Instructions != "" {
		preamble += setup.Instructions + "\n\n"
	}

	env := map[string]string{
		"EX_BASE_URL":    e.baseURL,
		"EX_RUN_TOKEN":   a.MCPToken,
		"EX_WORK_DIR":    workDir,
		"EX_PRIVATE_DIR": privateDir,
		"EX_ACTION_MODE": a.ActionMode,
	}
	if e.stateDir != "" {
		// The MCP server keeps harness tools away from the runner's own
		// state (its account token among it) unless a human approves.
		env["EX_RUNNER_HOME"] = filepath.Dir(e.stateDir)
	}
	if len(a.AutoAllow) > 0 {
		env["EX_AUTO_ALLOW"] = strings.Join(a.AutoAllow, ",")
	}
	if a.Task != nil && taskCwd != "" {
		reposJSON, _ := json.Marshal(repos)
		if repos == nil {
			reposJSON = []byte("[]")
		}
		env["EX_TASK_ID"] = a.Task.ID
		env["EX_TASK_DIR"] = taskCwd
		env["EX_TASK_PROJECT"] = a.Task.ProjectKey
		env["EX_TASK_REPOS"] = string(reposJSON)
		env["EX_WORKSPACE_ROOT"] = root
	}
	if setup.MCPConnectors != "" {
		env["EX_CONNECTORS"] = setup.MCPConnectors
	}

	attempt := func(resume string) harness.Running {
		run := e.runClaude
		if a.Harness == "codex" {
			run = e.runCodex
		}
		opts := harness.RunOptions{
			BinPath:    h.Path,
			SearchPath: e.searchPath,
			WorkDir:    workDir,
			PrivateDir: privateDir,
			Cwd:        taskCwd,
			MCPServer:  harness.MCPServerCmd{Command: e.mcpEntry.Command, Args: e.mcpEntry.Args, Env: env},
			OnSessionID: func(id string) {
				if canResume {
					e.sessions.Put(a, id, workDir)
				}
			},
			Log: e.log,
		}
		if resume != "" {
			// A resumed session already holds the earlier rounds: send the new
			// task + a nudge to re-read, not the whole bundle again.
			opts.ResumeSessionID = resume
			opts.PromptOverride = preamble + "[continuing in the same thread]\n# Task\n" + a.Prompt +
				"\n\nThe thread may have moved since your last turn — call get_thread and reply to the newest messages."
		} else if preamble != "" {
			opts.PromptOverride = preamble + a.Prompt + "\n\n" + a.ContextBundle
		}
		r := run(a, opts, sink{pump})
		runMu.Lock()
		running = r
		runMu.Unlock()
		if e.onHarness != nil {
			e.onHarness(a.RunID, kill)
		}
		return r
	}

	defer func() {
		if p := recover(); p != nil {
			e.log("run execution error", map[string]any{"runID": a.RunID, "error": fmt.Sprint(p)})
			kill("runner_error")
			e.fail(a.RunID, fmt.Sprintf("runner_error: %v", p))
		}
	}()

	resumeID := ""
	if session != nil {
		resumeID = session.SessionID
	}
	r := attempt(resumeID)
	pump.start()
	pump.push("state", map[string]any{"state": "running"})
	// ⚙️ used to be the agent's first tool call — a model turn for a status
	// emoji. The runner sets it the moment the harness starts instead.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := api.SetRunStatus(ctx, e.baseURL, a.MCPToken, "⚙️"); err != nil {
			e.log("status set failed", map[string]any{"runID": a.RunID, "error": err.Error()})
		}
	}()
	// Ours-vs-harness transparency: report exactly what EX injects into the
	// prompt so turn-1 spend can be split between our content and the
	// harness's own overhead. Warm resumes send the compact delta doc.
	rulesChars := textutil.Len(harness.SystemRules(a.AgentName, a.InvokerName, a.Persona, harness.RulesOptions{Connectors: harness.RunHasConnectors(a)}))
	taskChars := textutil.Len(preamble) + textutil.Len(a.Prompt) + textutil.Len(a.ContextBundle) + 2
	if session != nil {
		taskChars = textutil.Len(preamble) + textutil.Len(a.Prompt) + 160
	}
	pump.push("prompt", map[string]any{
		"rulesChars":    rulesChars,
		"taskChars":     taskChars,
		"resumed":       session != nil,
		"oursTokensEst": (rulesChars + taskChars + 2) / 4,
	})

	outcome := r.Wait()

	// Failed resume → the cached session is dead (evicted, corrupted, wrong
	// cwd): drop it and go again cold. Doesn't consume the infra retry.
	if !outcome.OK && session != nil && infraFailure(outcome.Reason) {
		e.log("resume failed; retrying with a fresh session", map[string]any{"runID": a.RunID, "reason": outcome.Reason})
		e.sessions.Drop(key)
		session = nil
		outcome = attempt("").Wait()
	}
	// One retry for infra-flavored failures (the CLI never ran / died
	// abnormally) — never for kills, deadlines, or model-level failures.
	if !outcome.OK && infraFailure(outcome.Reason) {
		e.log("infra failure; retrying once", map[string]any{"runID": a.RunID, "reason": outcome.Reason})
		time.Sleep(infraRetryPause)
		outcome = attempt("").Wait()
	}

	pump.finish()

	// Feed the session's weight estimate — the resume-vs-cold-start decision
	// reads it (a fat stale session is pricier to replay than a cold start).
	if canResume {
		e.sessions.AddCost(key, outcome.Usage.InputTokens+outcome.Usage.OutputTokens)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if outcome.OK {
		// Usage travels via the event pump ONLY (it retries on failure);
		// repeating it here double-counted every run's tokens.
		if err := e.api.Complete(ctx, e.runnerID, a.RunID, outcome.FinalText, protocol.Usage{}); err != nil {
			e.log("complete report failed", map[string]any{"runID": a.RunID, "error": err.Error()})
		}
		return
	}
	reason := outcome.Reason
	if reason == "" {
		reason = "unknown"
	}
	e.fail(a.RunID, reason)
}
