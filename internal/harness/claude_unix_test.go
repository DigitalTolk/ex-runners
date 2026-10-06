//go:build unix

package harness

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

// The TS ALLOWED_TOOLS list, spelled out independently of claude.go.
const wantAllowedTools = "mcp__ex__post_message,mcp__ex__get_thread,mcp__ex__get_context,mcp__ex__write_shared_context," +
	"mcp__ex__request_approval,mcp__ex__publish_artifact,mcp__ex__list_skills,mcp__ex__invoke_skill," +
	"mcp__ex__list_channels,mcp__ex__create_channel,mcp__ex__join_channel,mcp__ex__read_channel," +
	"mcp__ex__post_to_channel,mcp__ex__search_messages,mcp__ex__add_reaction,mcp__ex__list_users," +
	"mcp__ex__send_dm,mcp__ex__update_memory,mcp__ex__claim_task,mcp__ex__set_state,mcp__ex__set_reminder," +
	"mcp__ex__list_reminders,mcp__ex__cancel_reminder,mcp__ex__list_schedules,mcp__ex__create_schedule," +
	"mcp__ex__delete_schedule,mcp__ex__pin_message,mcp__ex__notify_owner,mcp__ex__propose_reply," +
	"mcp__ex__link_message,mcp__ex__fetch_spill,mcp__ex__connector_call,mcp__ex__use_connector," +
	"mcp__ex__connector_lookup,mcp__ex__create_coding_task,mcp__ex__publish_test_plan,mcp__ex__request_mr," +
	"mcp__ex__task_state,mcp__ex__register_project_commands"

// A realistic `claude -p --output-format stream-json --verbose` run: init,
// one assistant message streamed once per content block (text, tool_use,
// tool_use — same id, same usage), its tool results, a closing message and
// the result totals.
const claudeRun = `{"type":"system","subtype":"init","cwd":"/w","session_id":"sess-123","tools":["Bash","mcp__ex__get_thread"],"mcp_servers":[{"name":"ex","status":"connected"}],"model":"claude-opus-5","permissionMode":"default","apiKeySource":"none"}
{"type":"assistant","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"Looking it up."}],"stop_reason":null,"usage":{"input_tokens":10,"cache_creation_input_tokens":200,"cache_read_input_tokens":5000,"output_tokens":30}},"parent_tool_use_id":null,"session_id":"sess-123"}
{"type":"assistant","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls -la","description":"list files"}}],"stop_reason":null,"usage":{"input_tokens":10,"cache_creation_input_tokens":200,"cache_read_input_tokens":5000,"output_tokens":30}},"parent_tool_use_id":null,"session_id":"sess-123"}
{"type":"assistant","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"tool_use","id":"toolu_2","name":"mcp__ex__get_thread","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"cache_creation_input_tokens":200,"cache_read_input_tokens":5000,"output_tokens":30}},"parent_tool_use_id":null,"session_id":"sess-123"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":"total 0\nfile.txt","is_error":false}]},"parent_tool_use_id":null,"session_id":"sess-123"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_2","type":"tool_result","content":[{"type":"text","text":"thread body"}],"is_error":true}]},"parent_tool_use_id":null,"session_id":"sess-123"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_unknown","type":"tool_result","content":"orphan"},{"tool_use_id":"toolu_1","type":"tool_result","content":"   "},{"type":"text","text":"not a result"},{"type":"tool_result","content":"no id"}]},"session_id":"sess-123"}
this line is not JSON
{"type":"assistant","message":{"id":"msg_02","type":"message","role":"assistant","content":[{"type":"text","text":"Done: 1 file."}],"stop_reason":"end_turn","usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":5200,"output_tokens":12}},"session_id":"sess-123"}
{"type":"result","subtype":"success","is_error":false,"duration_ms":1000,"duration_api_ms":900,"num_turns":2,"result":"Done: 1 file.","session_id":"sess-123","total_cost_usd":0.01,"usage":{"input_tokens":20,"cache_creation_input_tokens":250,"cache_read_input_tokens":10200,"output_tokens":50},"permission_denials":[]}
`

func exServer() MCPServerCmd {
	return MCPServerCmd{
		Command: "/usr/bin/node",
		Args:    []string{"/x/mcp-server.js"},
		Env:     map[string]string{"EX_BASE_URL": "https://ex.example", "EX_RUN_TOKEN": "tok"},
	}
}

func TestRunClaudeNormalCompletion(t *testing.T) {
	t.Setenv("EX_TEST_INHERITED", "yes")
	t.Setenv("CI", "false") // the harness env overrides it
	f := newFake(t, fakeSpec{name: "claude", stdout: claudeRun, tail: "echo 'some warning' >&2\nexit 0"})
	work := filepath.Join(t.TempDir(), "threads", "abc") // missing: created on demand
	sink := &recSink{}
	logs := &logRec{}
	var sessions []string
	r := RunClaude(baseAssignment(), RunOptions{
		BinPath:     f.bin,
		SearchPath:  searchPath,
		WorkDir:     work,
		MCPServer:   exServer(),
		OnSessionID: func(id string) { sessions = append(sessions, id) },
		Log:         logs.log,
	}, sink)
	o := waitOutcome(t, r)

	if o != (protocol.RunOutcome{OK: true, FinalText: "Done: 1 file.", Usage: protocol.Usage{InputTokens: 270, OutputTokens: 50}}) {
		t.Fatalf("outcome = %+v", o)
	}
	if !reflect.DeepEqual(sessions, []string{"sess-123"}) {
		t.Errorf("sessions = %v", sessions)
	}
	assertEvents(t, sink.list(), []event{
		{Kind: "turn"},
		{Kind: "progress", Text: "Looking it up."},
		{Kind: "usage", In: 210, Out: 30, Cache: &Cache{Read: 5000, Creation: 200}},
		{Kind: "tool", Name: "Bash", Detail: "run `ls -la` — list files", Input: map[string]any{"command": "ls -la", "description": "list files"}},
		{Kind: "tool", Name: "mcp__ex__get_thread", Detail: "use mcp__ex__get_thread `{}`", Input: map[string]any{}},
		{Kind: "tool_result", Name: "Bash", Detail: "total 0 file.txt (16B)"},
		{Kind: "tool_result", Name: "mcp__ex__get_thread", Detail: "ERROR: thread body (11B)"},
		{Kind: "tool_result", Name: "tool", Detail: "orphan (6B)"},
		{Kind: "turn"},
		{Kind: "progress", Text: "Done: 1 file."},
		{Kind: "usage", In: 5, Out: 12, Cache: &Cache{Read: 5200, Creation: 0}},
		// Result totals 270/50 minus the 215/42 already streamed.
		{Kind: "usage", In: 55, Out: 8},
	})

	wantArgs := []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--mcp-config", filepath.Join(work, "mcp.json"), "--strict-mcp-config",
		"--allowedTools", wantAllowedTools,
		"--disallowedTools", "CronCreate,CronDelete,CronList,RemoteTrigger,ScheduleWakeup",
		"--tools", "Bash,PowerShell,Read,Grep,Glob,ToolSearch,WebFetch,WebSearch", "--disable-slash-commands",
		"--permission-prompt-tool", "mcp__ex__approval_prompt",
		"--append-system-prompt", golden(t, "system_rules_plain.golden"),
		"--max-turns", "128",
	}
	if got := f.argv(); !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("argv =\n%q\nwant\n%q", got, wantArgs)
	}
	env := f.env()
	for k, v := range map[string]string{
		"PATH": searchPath, "CI": "true", "MCP_TOOL_TIMEOUT": "600000", "MCP_TIMEOUT": "600000",
		"ENABLE_TOOL_SEARCH": "true", "EX_TEST_INHERITED": "yes",
	} {
		if env[k] != v {
			t.Errorf("env %s = %q, want %q", k, env[k], v)
		}
	}
	if got := f.read("stdin"); got != "do the thing\n\n# Task" {
		t.Errorf("stdin = %q", got)
	}
	if got := f.cwd(); got != realPath(t, work) {
		t.Errorf("cwd = %s", got)
	}
	mcp, _ := os.ReadFile(filepath.Join(work, "mcp.json"))
	if string(mcp) != `{"mcpServers":{"ex":{"command":"/usr/bin/node","args":["/x/mcp-server.js"],"env":{"EX_BASE_URL":"https://ex.example","EX_RUN_TOKEN":"tok"}}}}` {
		t.Errorf("mcp.json = %s", mcp)
	}
	if msgs := logs.msgs(); len(msgs) != 0 {
		t.Errorf("unexpected logs %v", msgs)
	}
	// Wait is repeatable; a late Kill changes nothing about the outcome (it
	// only signals a group that is already gone, as the TS did).
	r.Kill("late")
	if again := r.Wait(); again != o {
		t.Errorf("second Wait = %+v", again)
	}
	if extra, ok := logs.find("SIGTERM failed"); !ok || extra["pid"] == nil {
		t.Errorf("late kill logs = %v", logs.msgs())
	}
}

func TestRunClaudeWarmTaskRunWithOverrides(t *testing.T) {
	f := newFake(t, fakeSpec{name: "claude", stdout: `{"type":"system","subtype":"init","session_id":"sess-9"}
{"type":"result","subtype":"success","result":"ok"}
`})
	work, repo := t.TempDir(), t.TempDir()
	a := baseAssignment()
	a.Model = "claude-opus-5"
	a.Mode = "task"
	a.Task = &protocol.TaskSpec{ID: "t1"}
	a.ConnectorSlugs = []string{"gitlab"}
	override := "[continuing in the same thread]\n# Task\ndo the thing"
	// No Log, no OnSessionID, no MCP args/env: all optional.
	o := waitOutcome(t, RunClaude(a, RunOptions{
		BinPath:         f.bin,
		SearchPath:      searchPath,
		WorkDir:         work,
		Cwd:             repo,
		MCPServer:       MCPServerCmd{Command: "node"},
		ResumeSessionID: "sess-9",
		PromptOverride:  override,
	}, &recSink{}))
	if !o.OK || o.FinalText != "ok" {
		t.Fatalf("outcome = %+v", o)
	}
	wantArgs := []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--mcp-config", filepath.Join(work, "mcp.json"), "--strict-mcp-config",
		"--allowedTools", wantAllowedTools,
		"--disallowedTools", "CronCreate,CronDelete,CronList,RemoteTrigger,ScheduleWakeup",
		// A coding task keeps the full toolset and has no turn cap.
		"--permission-prompt-tool", "mcp__ex__approval_prompt",
		"--append-system-prompt", golden(t, "system_rules_connectors.golden"),
		"--model=claude-opus-5",
		"--resume", "sess-9",
	}
	if got := f.argv(); !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("argv =\n%q\nwant\n%q", got, wantArgs)
	}
	if got := f.read("stdin"); got != override {
		t.Errorf("stdin = %q", got)
	}
	// The harness runs in the checkout; per-run config stays in WorkDir.
	if got := f.cwd(); got != realPath(t, repo) {
		t.Errorf("cwd = %s", got)
	}
	if _, err := os.Stat(filepath.Join(repo, "mcp.json")); err == nil {
		t.Error("mcp.json leaked into the checkout")
	}
}

func TestRunClaudeMaxTurnsFollowsMode(t *testing.T) {
	f := newFake(t, fakeSpec{name: "claude", stdout: `{"type":"result","result":"ok"}` + "\n"})
	a := baseAssignment()
	a.Mode = "watch"
	a.Limits = protocol.AgentLimits{MaxTurns: 7, MaxTaskTurns: 99}
	waitOutcome(t, RunClaude(a, RunOptions{BinPath: f.bin, SearchPath: searchPath, WorkDir: t.TempDir()}, &recSink{}))
	args := f.argv()
	if tail := args[len(args)-2:]; tail[0] != "--max-turns" || tail[1] != "7" {
		t.Fatalf("argv tail = %q", tail)
	}
}

func TestRunClaudeNonZeroExits(t *testing.T) {
	longErr := strings.Repeat("e", 700)
	cases := []struct {
		name    string
		stdout  string
		tail    string
		want    protocol.RunOutcome
		code    any
		subtype any
		stderr  string
	}{
		{
			// A completed SUCCESS result outranks the exit code.
			name:    "success outranks exit",
			stdout:  `{"type":"result","subtype":"success","result":"delivered","usage":{"input_tokens":4,"output_tokens":2}}` + "\n",
			tail:    "echo 'mcp shutdown failed' >&2\nexit 1",
			want:    protocol.RunOutcome{OK: true, FinalText: "delivered", Usage: protocol.Usage{InputTokens: 4, OutputTokens: 2}},
			code:    1,
			subtype: "success",
			stderr:  "mcp shutdown failed\n",
		},
		{
			name:    "model-level ending",
			stdout:  `{"type":"result","subtype":"error_max_turns","usage":{"input_tokens":9,"output_tokens":1}}` + "\n",
			tail:    "exit 1",
			want:    protocol.RunOutcome{Reason: "error_max_turns", Usage: protocol.Usage{InputTokens: 9, OutputTokens: 1}},
			code:    1,
			subtype: "error_max_turns",
		},
		{
			name:   "no result",
			tail:   "printf '%s' '" + longErr + "' >&2\nexit 3",
			want:   protocol.RunOutcome{Reason: "harness_exit_3"},
			code:   3,
			stderr: strings.Repeat("e", 500),
		},
		{
			name: "killed by a signal it did not get from us",
			tail: "kill -9 $$",
			want: protocol.RunOutcome{Reason: "harness_exit_null"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(t, fakeSpec{name: "claude", stdout: c.stdout, tail: c.tail})
			logs := &logRec{}
			o := waitOutcome(t, RunClaude(baseAssignment(), RunOptions{BinPath: f.bin, SearchPath: searchPath, WorkDir: t.TempDir(), Log: logs.log}, &recSink{}))
			if o != c.want {
				t.Fatalf("outcome = %+v", o)
			}
			extra, ok := logs.find("claude exited non-zero")
			if !ok || extra["code"] != c.code || extra["subtype"] != c.subtype || extra["stderr"] != c.stderr {
				t.Fatalf("log = %#v", extra)
			}
		})
	}
}

func TestRunClaudeKillTakesDownTheTree(t *testing.T) {
	f := newFake(t, fakeSpec{
		name:   "claude",
		stdout: `{"type":"system","subtype":"init","session_id":"s"}` + "\n",
		tail:   `sleep 300 & echo $! > "$D/child.pid"` + "\nwait",
	})
	logs := &logRec{}
	r := RunClaude(baseAssignment(), RunOptions{BinPath: f.bin, SearchPath: searchPath, WorkDir: t.TempDir(), Log: logs.log}, &recSink{})
	child, _ := strconv.Atoi(f.waitFile("child.pid"))
	leader := r.(*run).pid
	r.Kill("deadline")
	r.Kill("abort:late") // idempotent: the first reason stands
	o := waitOutcome(t, r)
	if o.OK || o.Reason != "deadline" {
		t.Fatalf("outcome = %+v", o)
	}
	waitDead(t, leader)
	waitDead(t, child) // the grandchild died with its group
	if msgs := logs.msgs(); len(msgs) != 0 {
		t.Errorf("unexpected logs %v", msgs)
	}
}

func TestRunClaudeSpawnFailure(t *testing.T) {
	work := t.TempDir()
	logs := &logRec{}
	r := RunClaude(baseAssignment(), RunOptions{BinPath: filepath.Join(t.TempDir(), "no-claude"), SearchPath: searchPath, WorkDir: work, Log: logs.log}, &recSink{})
	o := waitOutcome(t, r)
	if o.OK || !strings.HasPrefix(o.Reason, "spawn_failed: ") || !strings.Contains(o.Reason, "no such file") {
		t.Fatalf("outcome = %+v", o)
	}
	// mcp.json was written before the spawn, as in TS.
	if _, err := os.Stat(filepath.Join(work, "mcp.json")); err != nil {
		t.Error(err)
	}
	r.Kill("deadline") // nothing to signal
	if msgs := logs.msgs(); len(msgs) != 0 {
		t.Errorf("unexpected logs %v", msgs)
	}
}

func TestRunClaudeConfigFailuresAreRunnerErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "plain-file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	occupied := t.TempDir()
	if err := os.Mkdir(filepath.Join(occupied, "mcp.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, work := range map[string]string{"work dir": filepath.Join(file, "sub"), "mcp.json": occupied} {
		t.Run(name, func(t *testing.T) {
			logs := &logRec{}
			o := waitOutcome(t, RunClaude(baseAssignment(), RunOptions{BinPath: "/bin/false", WorkDir: work, Log: logs.log}, &recSink{}))
			if o.OK || !strings.HasPrefix(o.Reason, "runner_error: ") {
				t.Fatalf("outcome = %+v", o)
			}
			if extra, ok := logs.find("run execution error"); !ok || extra["runID"] != "r1" || extra["error"] == "" {
				t.Fatalf("logs = %v", logs.msgs())
			}
		})
	}
}

func TestRunClaudeHugeLinesAndCRLF(t *testing.T) {
	huge := strings.Repeat("x", 1<<20)
	stdout := `{"type":"assistant","message":{"id":"m","content":[{"type":"tool_use","id":"t","name":"Read","input":{"file_path":"/a"}}]}}` + "\r\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"` + huge + `"}]}}` + "\r\n" +
		`{"type":"result","subtype":"success","result":"read it"}`
	f := newFake(t, fakeSpec{name: "claude", stdout: stdout})
	sink := &recSink{}
	o := waitOutcome(t, RunClaude(baseAssignment(), RunOptions{BinPath: f.bin, SearchPath: searchPath, WorkDir: t.TempDir()}, sink))
	if !o.OK || o.FinalText != "read it" {
		t.Fatalf("outcome = %+v", o)
	}
	events := sink.list()
	if len(events) != 3 || events[2].Detail != strings.Repeat("x", 220)+"… (1024.0KB)" {
		t.Fatalf("events = %d, last = %.60q", len(events), events[len(events)-1].Detail)
	}
}

// The run token and connector credentials ride mcp.json: it lives in the
// private dir — never the folder the agent works in — and claude itself
// never carries them in its environment (its Bash tool would inherit them).
func TestRunClaudeKeepsMCPSecretsOutOfTheWorkdir(t *testing.T) {
	f := newFake(t, fakeSpec{name: "claude", stdout: claudeRun, tail: "exit 0"})
	work := t.TempDir()
	private := filepath.Join(t.TempDir(), "private", "run-1") // missing: created owner-only
	srv := exServer()
	srv.Env["EX_CONNECTORS"] = `[{"slug":"hub","token":"conn-tok"}]`
	r := RunClaude(baseAssignment(), RunOptions{BinPath: f.bin, SearchPath: searchPath, WorkDir: work, PrivateDir: private, MCPServer: srv}, &recSink{})
	waitOutcome(t, r)
	argv := f.argv()
	if i := indexOf(argv, "--mcp-config"); i < 0 || argv[i+1] != filepath.Join(private, "mcp.json") {
		t.Fatalf("--mcp-config = %q", argv)
	}
	if _, err := os.Stat(filepath.Join(work, "mcp.json")); err == nil {
		t.Error("mcp.json written into the workdir")
	}
	if st, _ := os.Stat(private); st.Mode().Perm() != 0o700 {
		t.Errorf("private dir mode = %o", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Join(private, "mcp.json")); st.Mode().Perm() != 0o600 {
		t.Errorf("mcp.json mode = %o", st.Mode().Perm())
	}
	for k := range SecretEnv {
		if _, has := f.env()[k]; has {
			t.Errorf("claude's own environment carries %s", k)
		}
	}
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}
