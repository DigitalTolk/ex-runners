package harness

// Claude Code adapter: drives `claude -p --output-format stream-json` and
// maps its JSONL events onto runner events (plan-v2 §6).

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/DigitalTolk/ex-runners/internal/describetool"
	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

// The Ex tools are AUTO-ALLOWED (no permission prompt — they're already
// bounded by the invoker's access server-side). Everything else — Claude's
// native Bash/Write/WebSearch/… — is now AVAILABLE but routed through the
// permission gateway: --permission-prompt-tool calls mcp__ex__approval_prompt,
// which becomes an approval card the INVOKER clicks. Open by default, gated
// by a human — not hard-blocked.
var allowedTools = strings.Join([]string{
	"mcp__ex__post_message",
	"mcp__ex__get_thread",
	"mcp__ex__get_context",
	"mcp__ex__write_shared_context",
	"mcp__ex__request_approval",
	"mcp__ex__publish_artifact",
	"mcp__ex__list_skills",
	"mcp__ex__invoke_skill",
	"mcp__ex__list_channels",
	"mcp__ex__create_channel",
	"mcp__ex__join_channel",
	"mcp__ex__read_channel",
	"mcp__ex__post_to_channel",
	"mcp__ex__search_messages",
	"mcp__ex__add_reaction",
	"mcp__ex__list_users",
	"mcp__ex__send_dm",
	"mcp__ex__update_memory",
	"mcp__ex__claim_task",
	"mcp__ex__set_state",
	"mcp__ex__set_reminder",
	"mcp__ex__list_reminders",
	"mcp__ex__cancel_reminder",
	"mcp__ex__list_schedules",
	"mcp__ex__create_schedule",
	"mcp__ex__delete_schedule",
	"mcp__ex__pin_message",
	"mcp__ex__notify_owner",
	"mcp__ex__propose_reply",
	"mcp__ex__link_message",
	"mcp__ex__fetch_spill",
	"mcp__ex__connector_call",
	"mcp__ex__use_connector",
	"mcp__ex__connector_lookup",
	// Coding tasks (plan-coding-agent.md).
	"mcp__ex__create_coding_task",
	"mcp__ex__publish_test_plan",
	"mcp__ex__request_mr",
	"mcp__ex__task_state",
	"mcp__ex__register_project_commands",
}, ",")

const permissionPromptTool = "mcp__ex__approval_prompt"

// Claude Code's OWN scheduling (session cron, claude.ai routines) knows
// nothing about Ex: asked "what have I got scheduled?", an agent answered
// "zero" from these. Ex's list/create/delete_schedule are the only schedule.
var disallowedTools = strings.Join([]string{"CronCreate", "CronDelete", "CronList", "RemoteTrigger", "ScheduleWakeup"}, ",")

// request_approval legitimately blocks for minutes while a human decides —
// the harness's own MCP tool timeout must sit ABOVE the orchestrator's
// approval deadline so the deadline resolves inside the call (plan-v2 §7).
const mcpToolTimeoutMs = 10 * 60_000

// systemPrompt layers the platform rules over the agent's persona (plan.md
// §11 kept short). Connector instructions ride the TOP of the task prompt
// instead (the runner's preamble) — priority position, not system fine print.
func systemPrompt(a protocol.Assignment) string {
	return SystemRules(a.AgentName, a.InvokerName, a.Persona, RulesOptions{Connectors: RunHasConnectors(a)})
}

type claudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

// fresh is the spend metric both adapters agree on: tokens the model
// processed for the FIRST time this run. Anthropic splits input three ways —
// input_tokens (uncached) + cache_creation (first write, real work) +
// cache_read (cheap replay). Counting only input_tokens hides ~90% of a
// multi-turn run's real input; counting cache reads too would re-bill the
// whole context every turn (the codex token_budget bug, mirrored). So:
// uncached + cache writes, reads excluded. Codex equivalent:
// input_tokens - cached_input_tokens.
func (u *claudeUsage) fresh() int { return u.InputTokens + u.CacheCreationInputTokens }

type claudeBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Name      string `json:"name"`
	ID        string `json:"id"`
	Input     any    `json:"input"`
	ToolUseID string `json:"tool_use_id"`
	IsError   bool   `json:"is_error"`
	Content   any    `json:"content"`
}

type claudeEvent struct {
	Type      string  `json:"type"`
	Subtype   *string `json:"subtype"`
	SessionID string  `json:"session_id"`
	Result    string  `json:"result"`
	Message   *struct {
		ID      string        `json:"id"`
		Content []claudeBlock `json:"content"`
		Usage   *claudeUsage  `json:"usage"`
	} `json:"message"`
	Usage *claudeUsage `json:"usage"`
}

// writeMCPConfig writes the per-run mcp.json into PrivateDir — it carries the
// run token and connector credentials, so it must not sit in the folder the
// agent works in (Claude Code reads its cwd without asking). It guarantees
// both directories rather than assuming them: a retry after a failed resume
// drops the session (which sweeps the workdir) and then re-attempts in the
// same path, so by the time we get here it may be gone — and a missing cwd
// kills the run before the model ever starts.
func writeMCPConfig(opts RunOptions) (string, error) {
	dir := opts.PrivateDir
	if dir == "" {
		dir = opts.WorkDir
	}
	for _, d := range []string{opts.WorkDir, dir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", err
		}
	}
	type server struct {
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"` // run token rides env, never argv (plan-v2 §7)
	}
	srv := server{Command: opts.MCPServer.Command, Args: opts.MCPServer.Args, Env: opts.MCPServer.Env}
	if srv.Args == nil {
		srv.Args = []string{}
	}
	if srv.Env == nil {
		srv.Env = map[string]string{}
	}
	doc := map[string]map[string]server{"mcpServers": {"ex": srv}}
	path := filepath.Join(dir, "mcp.json")
	return path, os.WriteFile(path, jsonBytes(doc), 0o600)
}

func claudeArgs(a protocol.Assignment, opts RunOptions, mcpConfigPath string) []string {
	args := []string{
		"-p",
		"--output-format",
		"stream-json",
		"--verbose",
		"--mcp-config",
		mcpConfigPath,
		"--strict-mcp-config",
		"--allowedTools",
		allowedTools,
		"--disallowedTools",
		disallowedTools,
	}
	args = append(args, ClaudeToolArgs(a, os.Getenv)...)
	args = append(args,
		"--permission-prompt-tool",
		permissionPromptTool,
		"--append-system-prompt",
		systemPrompt(a),
	)
	if a.Model != "" {
		// One argv word: a server-sent model can't become a flag of its own.
		args = append(args, "--model="+a.Model)
	}
	if opts.ResumeSessionID != "" {
		// Warm session: continue the per-thread conversation instead of cold
		// starting — the earlier rounds' context is already in the session.
		args = append(args, "--resume", opts.ResumeSessionID)
	}
	// Local fast-fail; the orchestrator is authoritative either way. MODE-
	// AWARE: a direct task gets the task budget — passing the conversation
	// cap (16) here cut deep connector workflows off mid-task (the CLI
	// exited 1 with error_max_turns right after delivering, and the runner
	// then re-ran the whole completed task as an "infra" retry).
	if limit := protocol.TurnsFor(a.Limits, a.Mode); limit > 0 {
		args = append(args, "--max-turns", strconv.Itoa(limit))
	}
	return args
}

// RunClaude spawns one bounded execution. The child gets its own process
// group so cancellation kills the whole tree — CLIs spawn children, and a
// stray MCP server or helper outliving the run is a leak (plan-v2 §6).
func RunClaude(a protocol.Assignment, opts RunOptions, sink EventSink) Running {
	log := orNop(opts.Log)
	r := newRun(log)

	mcpConfigPath, err := writeMCPConfig(opts)
	if err != nil {
		r.finish(runnerError(a, err, log))
		return r
	}

	// A resumed session gets the compact override instead of the whole bundle.
	prompt := opts.PromptOverride
	if prompt == "" {
		prompt = a.Prompt + "\n\n" + a.ContextBundle
	}
	env := harnessEnv(opts.SearchPath,
		// Keep the harness out of interactive candy.
		"CI=true",
		// request_approval blocks while a human decides; the CLI's MCP tool
		// timeout must outlast the orchestrator's approval deadline.
		"MCP_TOOL_TIMEOUT="+strconv.Itoa(mcpToolTimeoutMs),
		"MCP_TIMEOUT="+strconv.Itoa(mcpToolTimeoutMs),
		// Progressive MCP: defer the ~37 Ex tool schemas (~6k tokens) behind
		// ToolSearch — the model gets a petite name+one-line catalog and pulls
		// a tool's full schema only when it decides to use it. Measured ~24%
		// off the per-request prompt (~9k tokens: the tool block collapses to
		// the catalog). Claude Code does this automatically above a tool-size
		// threshold, but ONLY on a first-party Anthropic host — the optimistic
		// path is auto-disabled when ANTHROPIC_BASE_URL points at a gateway
		// (Bedrock proxy, etc.). Setting this forces it on there too; it's a
		// no-op when the default host already defers. The cached system-prompt
		// + catalog prefix stays warm because loaded schemas are APPENDED as
		// tool-search results, not prepended into the tools block.
		"ENABLE_TOOL_SEARCH=true",
	)
	cmd, stdout, stderr, err := spawn(opts.BinPath, claudeArgs(a, opts, mcpConfigPath), cwdOf(opts), env, prompt)
	if err != nil {
		r.finish(protocol.RunOutcome{Reason: "spawn_failed: " + err.Error()})
		return r
	}
	r.started(cmd.Process.Pid) // nothing can have killed it yet: r is not handed out

	st := &claudeStream{
		sink:          sink,
		onSessionID:   opts.OnSessionID,
		toolNames:     map[string]string{},
		seenAssistant: map[string]bool{},
	}
	go func() {
		readLines(stdout, st.handle)
		_ = cmd.Wait()
		r.finish(st.outcome(r.killed(), cmd.ProcessState.ExitCode(), stderr.head(), log))
	}()
	return r
}

// claudeStream folds the stream-json events of one run.
type claudeStream struct {
	sink        EventSink
	onSessionID func(string)

	finalText string
	usage     protocol.Usage
	// The final result event's subtype ("success" | "error_max_turns" | …);
	// nil until a result event arrives.
	resultSubtype *string
	// tool_use id → tool name, so tool_result events (which only carry the
	// id) can be attributed in the timeline.
	toolNames map[string]string
	// Tokens already streamed via per-turn assistant usage — the final result
	// event carries TOTALS, so only the remainder is reported at the end
	// (otherwise the drawer would double-count).
	streamedIn, streamedOut int
	// Assistant message ids already counted for turn/usage — see the
	// "assistant" case: the same message streams once per content block.
	seenAssistant map[string]bool
}

func (c *claudeStream) handle(line []byte) {
	var evt claudeEvent
	if !decodeEvent(line, &evt) {
		return // non-JSON noise
	}
	if evt.SessionID != "" && evt.Type == "system" && c.onSessionID != nil {
		c.onSessionID(evt.SessionID)
	}
	switch evt.Type {
	case "assistant":
		c.assistant(&evt)
	case "user":
		// Tool RESULTS ride user-role messages in stream-json. Report each as
		// a timeline event: what came back, clipped + sized.
		if evt.Message == nil {
			return
		}
		for _, b := range evt.Message.Content {
			if b.Type != "tool_result" || b.ToolUseID == "" {
				continue
			}
			name, ok := c.toolNames[b.ToolUseID]
			if !ok {
				name = "tool"
			}
			flat := FlattenToolResult(b.Content)
			if flat == "" {
				continue
			}
			if b.IsError {
				flat = "ERROR: " + flat
			}
			c.sink.ToolResult(name, flat)
		}
	case "result":
		c.finalText = evt.Result
		subtype := "success"
		if evt.Subtype != nil {
			subtype = *evt.Subtype
		}
		c.resultSubtype = &subtype
		c.usage = protocol.Usage{}
		if u := evt.Usage; u != nil {
			c.usage = protocol.Usage{InputTokens: u.fresh(), OutputTokens: u.OutputTokens}
			// Report only what per-turn streaming hasn't already counted.
			inRem := max(0, c.usage.InputTokens-c.streamedIn)
			outRem := max(0, c.usage.OutputTokens-c.streamedOut)
			if inRem > 0 || outRem > 0 {
				c.sink.Usage(inRem, outRem, nil)
			}
		}
	}
	// default: system/init etc.
}

func (c *claudeStream) assistant(evt *claudeEvent) {
	msg := evt.Message
	// stream-json re-sends the SAME assistant message once per content
	// block (text, then each tool_use), each copy carrying the full
	// message.usage — counting every copy doubled turns and tokens. Blocks
	// differ across copies, so tools/progress still process every event;
	// turn + usage count once per message id.
	mid := ""
	if msg != nil {
		mid = msg.ID
	}
	firstCopy := mid == "" || !c.seenAssistant[mid]
	if mid != "" {
		c.seenAssistant[mid] = true
	}
	if firstCopy {
		c.sink.Turn()
	}
	if msg == nil {
		return
	}
	for _, b := range msg.Content {
		if b.Type == "text" && b.Text != "" {
			c.sink.Progress(b.Text)
		}
		if b.Type == "tool_use" && b.Name != "" {
			// The timeline narrates WHAT the call does, not just the tool
			// name — "cliffhub API: GET api/leave/requests?…" beats "Tool".
			input, _ := b.Input.(map[string]any)
			describeInput := input
			if describeInput == nil {
				describeInput = map[string]any{}
			}
			detail := describetool.DescribeToolUse(b.Name, describeInput)
			if b.ID != "" {
				c.toolNames[b.ID] = b.Name
			}
			c.sink.Tool(b.Name, detail, input)
		}
	}
	// Per-turn usage streams live so the drawer's spend ticks during the run
	// instead of jumping from 0 at the very end.
	if u := msg.Usage; firstCopy && u != nil && (u.fresh() > 0 || u.OutputTokens > 0) {
		c.sink.Usage(u.fresh(), u.OutputTokens, &Cache{Read: u.CacheReadInputTokens, Creation: u.CacheCreationInputTokens})
		c.streamedIn += u.fresh()
		c.streamedOut += u.OutputTokens
	}
}

// outcome is the TS 'close' handler.
func (c *claudeStream) outcome(killedReason string, code int, stderr string, log protocol.Logger) protocol.RunOutcome {
	if killedReason != "" {
		return protocol.RunOutcome{Reason: killedReason, Usage: c.usage}
	}
	if code == 0 {
		return protocol.RunOutcome{OK: true, FinalText: c.finalText, Usage: c.usage}
	}
	subtype := ""
	var subtypeLog any // null until a result event arrived
	if c.resultSubtype != nil {
		subtype = *c.resultSubtype
		subtypeLog = subtype
	}
	log("claude exited non-zero", map[string]any{"code": exitCodeValue(code), "subtype": subtypeLog, "stderr": stderr})
	// A completed SUCCESS result outranks the exit code: the work was
	// delivered; whatever poisoned the exit (MCP shutdown, cleanup) must not
	// fail the run — and above all must not trigger an infra retry that
	// re-runs an already-completed task.
	if subtype == "success" && c.finalText != "" {
		return protocol.RunOutcome{OK: true, FinalText: c.finalText, Usage: c.usage}
	}
	// A model-level ending (max turns, execution error) is NOT infra —
	// report its real name so the retry classifier leaves it alone.
	reason := exitReason(code)
	if subtype != "" && subtype != "success" {
		reason = subtype
	}
	return protocol.RunOutcome{Reason: reason, Usage: c.usage}
}
