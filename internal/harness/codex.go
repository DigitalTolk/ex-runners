package harness

// Codex adapter: drives `codex exec --json` and maps its JSONL events onto
// runner events (plan-v2 §6).
//
// Sandbox stance: codex runs with approval_policy "never" — there is no
// human to route a permission prompt to — so its sandbox profile is the only
// boundary. Chat runs read but never write; coding tasks write inside the
// checkout (and temp dirs); neither gets network (no card could approve it);
// and the machine's credential stores, the runner's own state and this run's
// codex home are denied outright, reads included (see codexProfile).

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/codexauth"
	"github.com/DigitalTolk/ex-runners/internal/describetool"
	"github.com/DigitalTolk/ex-runners/internal/protocol"
	"github.com/DigitalTolk/ex-runners/internal/secretpaths"
	"github.com/DigitalTolk/ex-runners/internal/textutil"
)

// tomlString renders a string as a TOML basic string, byte-for-byte what the
// TS runner's JSON.stringify produced. JSON string escaping is a strict
// subset of TOML basic-string escaping, so this is exact, not approximate.
// (Invalid UTF-8 becomes U+FFFD, as it did when Node decoded it.)
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

var userHomeDir = os.UserHomeDir

// codexBundledSkills are the skills codex installs into every CODEX_HOME
// (0.147). Their list alone was ~1.6k tokens of every prompt; none is for
// an Ex chat agent.
var codexBundledSkills = []string{"imagegen", "openai-docs", "plugin-creator", "review-agent", "skill-creator", "skill-installer"}

// now is the clock behind the date line codex gets (its own environment
// block, which carried it, is off).
var now = time.Now

// codexProfile is the per-run sandbox: a permission profile (codex ≥ 0.125)
// extending read-only (chat) or workspace (coding task: writes inside the
// checkout and temp dirs), with network off and the credential locations —
// plus the runner's home, this run's private dir and the codex home itself,
// which holds the user's codex login — denied, reads included.
func codexProfile(task bool, codexHome string, mcpEnv map[string]string) []string {
	base := ":read-only"
	if task {
		base = ":workspace"
	}
	home, _ := userHomeDir()
	lines := []string{`default_permissions = "ex"`, "", "[permissions.ex]", "extends = " + tomlString(base), "", "[permissions.ex.filesystem]"}
	for _, p := range secretpaths.Under(home, mcpEnv["EX_RUNNER_HOME"], mcpEnv["EX_PRIVATE_DIR"], codexHome) {
		lines = append(lines, tomlString(p)+` = "deny"`)
	}
	return append(lines, "", "[permissions.ex.network]", "enabled = false", "")
}

// codexAuthFiles: the user's auth.json (CODEX_HOME, else ~/.codex) and the
// thread's copy inside its per-run home.
func codexAuthFiles(home string) (user, thread string) {
	userHome := os.Getenv("CODEX_HOME")
	if userHome == "" {
		h, _ := os.UserHomeDir()
		userHome = filepath.Join(h, ".codex")
	}
	return filepath.Join(userHome, "auth.json"), filepath.Join(home, "auth.json")
}

// writeCodexHome builds the per-run CODEX_HOME (plan-v2 §7): a fresh
// config.toml carrying ONLY our MCP server + sandbox policy — never the
// user's real ~/.codex, whose config could re-enable tools or other MCP
// servers. The user's auth.json is COPIED in (CODEX_HOME is also where codex
// looks for credentials; an empty home would log the user out of the run) —
// unless this thread's copy is fresher; codexAuthFiles + SyncAuthOut hand a
// refresh back afterwards (see package codexauth).
// The run token rides the config file inside the 0700 dir — never argv.
func writeCodexHome(a protocol.Assignment, opts RunOptions, log protocol.Logger) (string, error) {
	home := filepath.Join(opts.WorkDir, "codex-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", err
	}

	authSrc, authDst := codexAuthFiles(home)
	if !codexauth.SyncAuthIn(authSrc, authDst) { // unreadable/unwritable — same outcome as none
		log("codex: no auth.json found — run will rely on env auth if any", map[string]any{"authSrc": authSrc})
	}

	// The light tool index (tool-index) is OPT-IN for codex. Measured
	// 2026-09-30 on 9 real tasks: same accuracy (7/9) but only −11% input —
	// codex already caches ~87% of its prompt — and it added failure modes
	// (a catalogued tool not noticed; `name` passed for `tool`). Full
	// definitions up front also mean codex always has a tool's whole contract
	// before calling it.
	// Credentials stay out of config.toml (the agent's shell can read any file
	// in its read-only sandbox): codex forwards them from its own environment
	// (env_vars), and the shell policy below keeps them out of commands.
	env := map[string]string{}
	var secretNames []string
	for k, v := range opts.MCPServer.Env {
		if SecretEnv[k] {
			secretNames = append(secretNames, tomlString(k))
			continue
		}
		env[k] = v
	}
	sort.Strings(secretNames)
	if os.Getenv("EX_RUNNER_TOOL_INDEX") == "1" {
		env["EX_TOOL_INDEX"] = "1"
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys) // a Go map has no insertion order; sorted keeps the file stable
	pairs := make([]string, len(keys))
	for i, k := range keys {
		pairs[i] = tomlString(k) + " = " + tomlString(env[k])
	}
	args := make([]string, len(opts.MCPServer.Args))
	for i, arg := range opts.MCPServer.Args {
		args[i] = tomlString(arg)
	}

	lines := []string{"# Generated per-run by the Ex runner. Deleted with the run."}
	if a.Model != "" {
		lines = append(lines, "model = "+tomlString(a.Model))
	}
	lines = append(lines, `approval_policy = "never"`)
	// Codex's own prompt scaffolding an Ex chat agent never uses: the
	// permission and environment blocks (the sandbox enforces the profile
	// either way; the date travels in our prompt instead), plus — below —
	// multi-agent, plugins and the bundled skills. ~4k tokens a turn.
	lines = append(lines, "include_permissions_instructions = false", "include_environment_context = false", "include_apps_instructions = false")
	lines = append(lines, codexProfile(a.Task != nil, home, opts.MCPServer.Env)...)
	lines = append(lines,
		"[history]",
		// save-all records the session rollout inside this per-thread
		// CODEX_HOME so follow-ups in the same thread can `exec resume` warm
		// instead of re-sending the whole bundle. Local to the thread workdir;
		// swept with the session. (Was "none" before codex threads were
		// resumable.)
		`persistence = "save-all"`,
		"",
		"[mcp_servers.ex]",
		"command = "+tomlString(opts.MCPServer.Command),
		"args = ["+strings.Join(args, ", ")+"]",
		// request_approval blocks while a human decides; codex's per-tool
		// timeout must outlast the orchestrator's approval deadline.
		"tool_timeout_sec = 600",
		// Codex gates every MCP tool call behind user approval, and with
		// approval_policy "never" it auto-CANCELS them ("user cancelled MCP
		// tool call") — no ex tool would ever run. Auto-approve our own
		// server: this is the same stance as claude's --allowedTools
		// mcp__ex__*, and the tools with real consequences (use_connector,
		// request_approval) carry their own human-consent flow server-side.
		// The shell stays sandboxed read-only.
		`default_tools_approval_mode = "approve"`,
		"env = { "+strings.Join(pairs, ", ")+" }",
		"env_vars = ["+strings.Join(secretNames, ", ")+"]",
		"",
		// The shell must not inherit the forwarded credentials (codex already
		// drops *TOKEN*/*KEY*/*SECRET*; EX_CONNECTORS matches none of those).
		"[shell_environment_policy]",
		`exclude = ["EX_*"]`,
		"",
		"[agents]",
		"enabled = false",
		"",
		"[features]",
		"plugins = false",
		"",
	)
	for _, skill := range codexBundledSkills {
		lines = append(lines, "[[skills.config]]", "path = "+tomlString(filepath.Join(home, "skills", ".system", skill, "SKILL.md")), "enabled = false", "")
	}
	return home, os.WriteFile(filepath.Join(home, "config.toml"), []byte(strings.Join(lines, "\n")), 0o600)
}

// Flags drift across codex versions (plan-v2 §6) — probe `exec --help` once
// per binary and degrade with a legible reason instead of an opaque exit.
// A failed probe caches "" for the binary, as the TS promise cache did.
var (
	helpCache   sync.Map // binPath → *helpProbe
	helpTimeout = 10 * time.Second
)

type helpProbe struct {
	once sync.Once
	out  string
}

func execHelp(binPath, searchPath string) string {
	v, _ := helpCache.LoadOrStore(binPath, &helpProbe{})
	p := v.(*helpProbe)
	p.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), helpTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, binPath, "exec", "--help")
		cmd.Env = harnessEnv(searchPath)
		// Node's execFile dropped the pipes when it killed a timed-out
		// child; without this a grandchild holding stdout would hang us.
		cmd.WaitDelay = time.Second
		if out, err := cmd.Output(); err == nil {
			p.out = string(out)
		}
	})
	return p.out
}

// codexError is `{ message?: string }`; Message is nil when absent so `??`
// fallbacks behave as in TS (an empty message is still a message).
type codexError struct {
	Message *string `json:"message"`
}

// codexEvent: `codex exec --json` emits one JSON event per line. Two
// generations of shapes exist; both are mapped, unknown lines are ignored.
type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Item     *struct {
		Type      string      `json:"type"`
		Text      string      `json:"text"`
		Tool      string      `json:"tool"`
		Server    *string     `json:"server"`
		Command   string      `json:"command"`
		Arguments any         `json:"arguments"`
		Status    string      `json:"status"`
		Error     *codexError `json:"error"`
		Result    *struct {
			Content any `json:"content"`
		} `json:"result"`
		ExitCode         *float64 `json:"exit_code"`
		AggregatedOutput any      `json:"aggregated_output"`
	} `json:"item"`
	Usage *struct {
		InputTokens       int `json:"input_tokens"`
		CachedInputTokens int `json:"cached_input_tokens"`
		OutputTokens      int `json:"output_tokens"`
	} `json:"usage"`
	Error   *codexError `json:"error"`
	Message *string     `json:"message"`
	// Legacy (pre-JSONL-v2) shape: {"id":"0","msg":{"type":"agent_message",...}}
	Msg *struct {
		Type             string `json:"type"`
		Message          string `json:"message"`
		LastAgentMessage string `json:"last_agent_message"`
	} `json:"msg"`
}

// RunCodex spawns one bounded execution, mirroring RunClaude's contract. It
// returns at once: the version probe and the spawn happen in the background,
// and a Kill during the probe kills the child the moment it exists.
func RunCodex(a protocol.Assignment, opts RunOptions, sink EventSink) Running {
	log := orNop(opts.Log)
	r := newRun(log)
	go func() { r.finish(runCodex(a, opts, sink, r, log)) }()
	return r
}

func runCodex(a protocol.Assignment, opts RunOptions, sink EventSink, r *run, log protocol.Logger) protocol.RunOutcome {
	help := execHelp(opts.BinPath, opts.SearchPath)
	if !strings.Contains(help, "--json") {
		log("codex: exec --help lacks --json; version too old", map[string]any{})
		return protocol.RunOutcome{Reason: "harness_incompatible: this codex version has no `exec --json`; update the Codex CLI"}
	}

	codexHome, err := writeCodexHome(a, opts, log)
	if err != nil {
		return runnerError(a, err, log)
	}
	// Warm thread: resume the recorded session instead of cold-starting.
	// Neither form takes --sandbox: the per-run config.toml's permission
	// profile covers both (and codex refuses the flag next to a profile). An id on a codex too old for resume fails as
	// spawn_failed, which the runner treats as a dead session: drop + retry
	// cold.
	if opts.ResumeSessionID != "" && !strings.Contains(help, "resume") {
		return protocol.RunOutcome{Reason: "spawn_failed: this codex version has no `exec resume`"}
	}
	var args []string
	if opts.ResumeSessionID != "" {
		args = []string{"exec", "resume", opts.ResumeSessionID, "--json"}
	} else {
		args = []string{"exec", "--json"}
	}
	if strings.Contains(help, "--skip-git-repo-check") {
		args = append(args, "--skip-git-repo-check")
	}
	// Prompt from stdin ("-" is codex's read-from-stdin sentinel): argv has
	// platform size limits and shows in `ps`; the bundle can be tens of KB.
	args = append(args, "-")

	// Codex has no --append-system-prompt: the rules travel at the top of
	// the prompt document instead. PromptOverride (mode/connector preambles,
	// warm-session deltas) replaces the default task document, mirroring the
	// claude adapter.
	taskDoc := opts.PromptOverride
	if taskDoc == "" {
		taskDoc = a.Prompt + "\n\n" + a.ContextBundle
	}
	stdin := SystemRules(a.AgentName, a.InvokerName, a.Persona, RulesOptions{Connectors: RunHasConnectors(a)}) +
		"\n\nCurrent date and time: " + now().Format("Monday 2006-01-02 15:04 MST") + "\n\n" + taskDoc

	extra := []string{"CODEX_HOME=" + codexHome, "CI=true"}
	for k, v := range opts.MCPServer.Env {
		if SecretEnv[k] {
			extra = append(extra, k+"="+v) // forwarded to the MCP server via env_vars
		}
	}
	cmd, stdout, stderr, err := spawn(opts.BinPath, args, cwdOf(opts), harnessEnv(opts.SearchPath, extra...), stdin)
	if err != nil {
		return protocol.RunOutcome{Reason: "spawn_failed: " + err.Error()}
	}
	if r.started(cmd.Process.Pid) != "" {
		KillTree(cmd.Process.Pid, log) // killed during the probe
	}

	st := &codexStream{sink: sink, onSessionID: opts.OnSessionID, log: log}
	readLines(stdout, st.handle)
	_ = cmd.Wait()

	// A refresh during the run rotated the token inside this thread's home;
	// the user's file must get it or the next thread starts spent.
	user, thread := codexAuthFiles(codexHome)
	if codexauth.SyncAuthOut(thread, user) {
		log("codex: refreshed credentials written back", map[string]any{"user": user})
	} else if codexauth.LastRefresh(thread) > codexauth.LastRefresh(user) {
		// Still fresher after a refused copy: the write-back itself failed.
		log("codex: credential write-back failed", map[string]any{"error": "could not replace " + user})
	}
	if reason := r.killed(); reason != "" {
		return protocol.RunOutcome{Reason: reason, Usage: st.usage}
	}
	code := cmd.ProcessState.ExitCode()
	if code == 0 {
		return protocol.RunOutcome{OK: true, FinalText: st.finalText, Usage: st.usage}
	}
	log("codex exited non-zero", map[string]any{"code": exitCodeValue(code), "stderr": stderr.head()})
	reason := exitReason(code)
	if st.lastError != "" {
		// The chat shows "stopped before finishing: <reason>" — codex's own
		// message (a usage limit, a bad model name) says what to do.
		reason += ": " + textutil.Clip(textutil.CollapseSpace(st.lastError), 200)
	}
	return protocol.RunOutcome{Reason: reason, Usage: st.usage}
}

// codexStream folds the JSONL events of one run.
type codexStream struct {
	sink        EventSink
	onSessionID func(string)
	log         protocol.Logger

	finalText string
	usage     protocol.Usage
	lastError string // codex's own words for the last failure ("usage limit…")
}

func (c *codexStream) handle(line []byte) {
	var evt codexEvent
	if !decodeEvent(line, &evt) {
		return // non-JSON noise
	}
	switch evt.Type {
	case "thread.started":
		// The session handle for warm follow-ups in this thread — the runner
		// pins it per (agent, thread), mirroring claude's flow.
		if evt.ThreadID != "" && c.onSessionID != nil {
			c.onSessionID(evt.ThreadID)
		}
	case "turn.started":
		c.sink.Turn()
	case "item.completed":
		c.item(&evt)
	case "turn.completed":
		if u := evt.Usage; u != nil {
			// Codex's input_tokens counts the FULL context every turn, cached
			// reads included — a multi-turn run re-bills the same ~50k context
			// each turn and trips the orchestrator's token_budget while barely
			// using the model. Count fresh input only (cache reads are
			// ~free), matching claude's accounting.
			in := max(0, u.InputTokens-u.CachedInputTokens)
			c.usage.InputTokens += in
			c.usage.OutputTokens += u.OutputTokens
			c.sink.Usage(in, u.OutputTokens, nil)
		}
	case "turn.failed", "error":
		// Surface harness-level errors in the run timeline, not just the
		// local log — "user cancelled MCP tool call" was invisible here.
		msg := "unknown error"
		if evt.Error != nil && evt.Error.Message != nil {
			msg = *evt.Error.Message
		} else if evt.Message != nil {
			msg = *evt.Message
		}
		c.log("codex error event", map[string]any{"message": msg})
		c.sink.ToolResult("codex", "ERROR: "+msg)
		c.lastError = msg
	default:
		// Legacy event stream (msg envelope).
		m := evt.Msg
		if m == nil || m.Type == "" {
			return
		}
		switch {
		case m.Type == "agent_message" && m.Message != "":
			c.finalText = m.Message
			c.sink.Progress(m.Message)
		case m.Type == "task_started":
			c.sink.Turn()
		case m.Type == "task_complete" && m.LastAgentMessage != "":
			c.finalText = m.LastAgentMessage
		}
	}
}

func (c *codexStream) item(evt *codexEvent) {
	item := evt.Item
	if item == nil {
		return
	}
	switch {
	case item.Type == "agent_message" && item.Text != "":
		c.finalText = item.Text // the last agent message is the outcome
		c.sink.Progress(item.Text)
	case item.Type == "reasoning" && item.Text != "":
		c.sink.Progress(item.Text)
	case item.Type == "mcp_tool_call" && item.Tool != "":
		// Codex reports the call and its outcome on ONE completed item — emit
		// both timeline rows (call description + clipped result) so codex
		// runs read like claude runs in the drawer.
		server := "ex"
		if item.Server != nil {
			server = *item.Server
		}
		name := "mcp__" + server + "__" + item.Tool
		args, _ := item.Arguments.(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		c.sink.Tool(name, describetool.DescribeToolUse(name, args), args)
		var content any
		if item.Result != nil {
			content = item.Result.Content
		}
		flat := FlattenToolResult(content)
		if item.Status == "failed" {
			msg := flat
			if msg == "" {
				msg = "failed"
			}
			if item.Error != nil && item.Error.Message != nil {
				msg = *item.Error.Message
			}
			c.sink.ToolResult(name, "ERROR: "+msg)
		} else if flat != "" {
			c.sink.ToolResult(name, flat)
		}
	case item.Type == "command_execution" && item.Command != "":
		c.sink.Tool("shell", describetool.DescribeToolUse("Bash", map[string]any{"command": item.Command}), map[string]any{"command": item.Command})
		out := FlattenToolResult(item.AggregatedOutput)
		if item.ExitCode != nil && *item.ExitCode != 0 {
			c.sink.ToolResult("shell", textutil.JSTrim("ERROR: exit "+textutil.JSString(*item.ExitCode)+" "+out))
		} else if out != "" {
			c.sink.ToolResult("shell", out)
		}
	}
}
