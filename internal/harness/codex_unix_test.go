//go:build unix

package harness

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

// codexHelp is a trimmed real `codex exec --help`.
const codexHelp = `Run Codex non-interactively

Usage: codex exec [OPTIONS] [PROMPT] [COMMAND]

Commands:
  resume  Resume a previous session by id or pick the most recent with --last
  help    Print this message or the help of the given subcommand(s)

Options:
  -m, --model <MODEL>           Model the agent should use
  -s, --sandbox <SANDBOX_MODE>  Select the sandbox policy to use when executing model-generated shell commands
      --skip-git-repo-check     Allow running Codex outside a Git repository
      --json                    Print events to stdout as JSONL
`

// helpPre answers the `exec --help` probe from help.txt, counting probes.
const helpPre = `if [ "$1" = exec ] && [ "$2" = --help ]; then
  echo probe >> "$D/help-calls"
  cat "$D/help.txt"
  exit 0
fi`

// A realistic `codex exec --json` run (JSONL v2 shapes).
const codexRun = `{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"reasoning","text":"**Checking the thread**"}}
{"type":"item.completed","item":{"id":"item_1","type":"mcp_tool_call","server":"ex","tool":"get_thread","arguments":{"limit":5},"result":{"content":[{"type":"text","text":"3 messages"}],"structured_content":null},"error":null,"status":"completed"}}
{"type":"item.completed","item":{"id":"item_2","type":"mcp_tool_call","server":"ex","tool":"post_message","arguments":{"text":"hi"},"result":null,"error":{"message":"user cancelled MCP tool call"},"status":"failed"}}
{"type":"item.completed","item":{"id":"item_3","type":"mcp_tool_call","tool":"list_users","arguments":"not-an-object","status":"failed"}}
{"type":"item.completed","item":{"id":"item_4","type":"command_execution","command":"bash -lc ls","aggregated_output":"a.txt\nb.txt\n","exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"id":"item_5","type":"command_execution","command":"bash -lc false","aggregated_output":"","exit_code":1,"status":"failed"}}
{"type":"item.completed","item":{"id":"item_6","type":"command_execution","command":"bash -lc 'cat x'","aggregated_output":"cat: x: No such file","exit_code":2,"status":"failed"}}
{"type":"item.completed","item":{"id":"item_7","type":"agent_message","text":"All done."}}
{"type":"turn.completed","usage":{"input_tokens":52000,"cached_input_tokens":48000,"output_tokens":300}}
{"type":"turn.failed","error":{"message":"stream disconnected"}}
{"type":"error","message":"reconnecting 1/5"}
{"type":"error"}
noise
`

// codexUser points CODEX_HOME (the user's codex dir) at a scratch dir,
// optionally holding an auth.json.
// fixedNow is the clock codex runs see in tests (the prompt carries the date).
var fixedNow = time.Date(2026, 10, 6, 13, 5, 0, 0, time.FixedZone("IST", 5*3600+1800))

const dateLine = "\n\nCurrent date and time: Tuesday 2026-10-06 13:05 IST"

func codexUser(t *testing.T, lastRefresh, token string) (home, authFile string) {
	t.Helper()
	origNow := now
	now = func() time.Time { return fixedNow }
	t.Cleanup(func() { now = origNow })
	home = t.TempDir()
	t.Setenv("CODEX_HOME", home)
	authFile = filepath.Join(home, "auth.json")
	if lastRefresh != "" {
		writeAuth(t, authFile, lastRefresh, token)
	}
	return home, authFile
}

func writeAuth(t *testing.T, file, lastRefresh, token string) {
	t.Helper()
	body := `{"last_refresh":"` + lastRefresh + `","tokens":{"refresh_token":"` + token + `"}}`
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newCodex(t *testing.T, s fakeSpec) *fakeCLI {
	t.Helper()
	s.name = "codex"
	if s.pre == "" {
		s.pre = helpPre
	}
	if s.files == nil {
		s.files = map[string]string{"help.txt": codexHelp}
	}
	return newFake(t, s)
}

func TestRunCodexNormalCompletion(t *testing.T) {
	_, userAuth := codexUser(t, "2026-09-07T12:46:44Z", "user-token")
	t.Setenv("EX_TEST_INHERITED", "yes")
	// The run refreshes its token mid-run: codex writes the rotated pair
	// into the CODEX_HOME it ran with.
	f := newCodex(t, fakeSpec{stdout: codexRun, tail: `cp "$CODEX_HOME/auth.json" "$D/seeded-auth"
printf '{"last_refresh":"2026-10-01T00:00:00Z","tokens":{"refresh_token":"rotated"}}' > "$CODEX_HOME/auth.json"
exit 0`})
	work := t.TempDir()
	sink := &recSink{}
	logs := &logRec{}
	var sessions []string
	o := waitOutcome(t, RunCodex(baseAssignment(), RunOptions{
		BinPath:     f.bin,
		SearchPath:  searchPath,
		WorkDir:     work,
		MCPServer:   exServer(),
		OnSessionID: func(id string) { sessions = append(sessions, id) },
		Log:         logs.log,
	}, sink))

	if o != (protocol.RunOutcome{OK: true, FinalText: "All done.", Usage: protocol.Usage{InputTokens: 4000, OutputTokens: 300}}) {
		t.Fatalf("outcome = %+v", o)
	}
	if !reflect.DeepEqual(sessions, []string{"0199a213-81c0-7800-8aa1-bbab2a035a53"}) {
		t.Errorf("sessions = %v", sessions)
	}
	assertEvents(t, sink.list(), []event{
		{Kind: "turn"},
		{Kind: "progress", Text: "**Checking the thread**"},
		{Kind: "tool", Name: "mcp__ex__get_thread", Detail: "use mcp__ex__get_thread `{\"limit\":5}`", Input: map[string]any{"limit": 5.0}},
		{Kind: "tool_result", Name: "mcp__ex__get_thread", Detail: "3 messages (10B)"},
		{Kind: "tool", Name: "mcp__ex__post_message", Detail: "use mcp__ex__post_message `{\"text\":\"hi\"}`", Input: map[string]any{"text": "hi"}},
		{Kind: "tool_result", Name: "mcp__ex__post_message", Detail: "ERROR: user cancelled MCP tool call"},
		{Kind: "tool", Name: "mcp__ex__list_users", Detail: "use mcp__ex__list_users `{}`", Input: map[string]any{}},
		{Kind: "tool_result", Name: "mcp__ex__list_users", Detail: "ERROR: failed"},
		{Kind: "tool", Name: "shell", Detail: "run `bash -lc ls`", Input: map[string]any{"command": "bash -lc ls"}},
		{Kind: "tool_result", Name: "shell", Detail: "a.txt b.txt (11B)"},
		{Kind: "tool", Name: "shell", Detail: "run `bash -lc false`", Input: map[string]any{"command": "bash -lc false"}},
		{Kind: "tool_result", Name: "shell", Detail: "ERROR: exit 1"},
		{Kind: "tool", Name: "shell", Detail: "run `bash -lc 'cat x'`", Input: map[string]any{"command": "bash -lc 'cat x'"}},
		{Kind: "tool_result", Name: "shell", Detail: "ERROR: exit 2 cat: x: No such file (20B)"},
		{Kind: "progress", Text: "All done."},
		{Kind: "usage", In: 4000, Out: 300},
		{Kind: "tool_result", Name: "codex", Detail: "ERROR: stream disconnected"},
		{Kind: "tool_result", Name: "codex", Detail: "ERROR: reconnecting 1/5"},
		{Kind: "tool_result", Name: "codex", Detail: "ERROR: unknown error"},
	})

	if got, want := f.argv(), []string{"exec", "--json", "--skip-git-repo-check", "-"}; !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q", got)
	}
	codexHome := filepath.Join(work, "codex-home")
	env := f.env()
	// The run token reaches the MCP server through codex's own environment
	// (env_vars) — never through config.toml, which the agent can read.
	for k, v := range map[string]string{"PATH": searchPath, "CODEX_HOME": codexHome, "CI": "true", "EX_TEST_INHERITED": "yes", "EX_RUN_TOKEN": "tok"} {
		if env[k] != v {
			t.Errorf("env %s = %q, want %q", k, env[k], v)
		}
	}
	// Codex has no --append-system-prompt: the rules head the stdin document.
	if got, want := f.read("stdin"), golden(t, "system_rules_plain.golden")+dateLine+"\n\ndo the thing\n\n# Task"; got != want {
		t.Errorf("stdin = %q", got)
	}
	if got := f.cwd(); got != realPath(t, work) {
		t.Errorf("cwd = %s", got)
	}
	wantConfig := `# Generated per-run by the Ex runner. Deleted with the run.
approval_policy = "never"
include_permissions_instructions = false
include_environment_context = false
include_apps_instructions = false
` + strings.Join(codexProfile(false, codexHome, nil), "\n") + `
[history]
persistence = "save-all"

[mcp_servers.ex]
command = "/usr/bin/node"
args = ["/x/mcp-server.js"]
tool_timeout_sec = 600
default_tools_approval_mode = "approve"
env = { "EX_BASE_URL" = "https://ex.example" }
env_vars = ["EX_RUN_TOKEN"]

[shell_environment_policy]
exclude = ["EX_*"]
` + slimTail(codexHome)
	if got := readFile(t, filepath.Join(codexHome, "config.toml")); got != wantConfig {
		t.Errorf("config.toml =\n%s\nwant\n%s", got, wantConfig)
	}
	if st, _ := os.Stat(filepath.Join(codexHome, "config.toml")); st.Mode().Perm() != 0o600 {
		t.Errorf("config.toml mode = %o", st.Mode().Perm())
	}
	if st, _ := os.Stat(codexHome); st.Mode().Perm() != 0o700 {
		t.Errorf("codex-home mode = %o", st.Mode().Perm())
	}
	// Seeded from the user before the run, handed back after it.
	if !strings.Contains(f.read("seeded-auth"), "user-token") {
		t.Errorf("seeded auth = %s", f.read("seeded-auth"))
	}
	if !strings.Contains(readFile(t, userAuth), "rotated") {
		t.Errorf("user auth not refreshed: %s", readFile(t, userAuth))
	}
	if extra, ok := logs.find("codex: refreshed credentials written back"); !ok || extra["user"] != userAuth {
		t.Errorf("logs = %v", logs.msgs())
	}
	if extra, ok := logs.find("codex error event"); !ok || extra["message"] != "stream disconnected" {
		t.Errorf("logs = %v", logs.msgs())
	}
	if got := strings.Count(f.read("help-calls"), "probe"); got != 1 {
		t.Errorf("help probed %d times", got)
	}
}

func TestRunCodexWarmTaskRunWithOverrides(t *testing.T) {
	codexUser(t, "", "")
	t.Setenv("EX_RUNNER_TOOL_INDEX", "1")
	f := newCodex(t, fakeSpec{stdout: `{"id":"0","msg":{"type":"task_started"}}
{"id":"0","msg":{"type":"agent_message","message":"legacy"}}
{"id":"0","msg":{"type":"task_complete","last_agent_message":"Legacy final."}}
`})
	work, repo := t.TempDir(), t.TempDir()
	a := baseAssignment()
	a.Model = "gpt-5-codex"
	a.Task = &protocol.TaskSpec{ID: "t1"}
	a.ContextBundle = "# Installed connectors\n- hub"
	override := "[continuing in the same thread]\n# Task\ndo the thing"
	sink := &recSink{}
	o := waitOutcome(t, RunCodex(a, RunOptions{
		BinPath:         f.bin,
		SearchPath:      searchPath,
		WorkDir:         work,
		Cwd:             repo,
		MCPServer:       MCPServerCmd{Command: "node"},
		ResumeSessionID: "th-1",
		PromptOverride:  override,
	}, sink))
	if !o.OK || o.FinalText != "Legacy final." {
		t.Fatalf("outcome = %+v", o)
	}
	assertEvents(t, sink.list(), []event{{Kind: "turn"}, {Kind: "progress", Text: "legacy"}})
	if got, want := f.argv(), []string{"exec", "resume", "th-1", "--json", "--skip-git-repo-check", "-"}; !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q", got)
	}
	if got, want := f.read("stdin"), golden(t, "system_rules_connectors.golden")+dateLine+"\n\n"+override; got != want {
		t.Errorf("stdin = %q", got)
	}
	if got := f.cwd(); got != realPath(t, repo) {
		t.Errorf("cwd = %s", got)
	}
	wantConfig := `# Generated per-run by the Ex runner. Deleted with the run.
model = "gpt-5-codex"
approval_policy = "never"
include_permissions_instructions = false
include_environment_context = false
include_apps_instructions = false
` + strings.Join(codexProfile(true, filepath.Join(work, "codex-home"), nil), "\n") + `
[history]
persistence = "save-all"

[mcp_servers.ex]
command = "node"
args = []
tool_timeout_sec = 600
default_tools_approval_mode = "approve"
env = { "EX_TOOL_INDEX" = "1" }
env_vars = []

[shell_environment_policy]
exclude = ["EX_*"]
` + slimTail(filepath.Join(work, "codex-home"))
	if got := readFile(t, filepath.Join(work, "codex-home", "config.toml")); got != wantConfig {
		t.Errorf("config.toml =\n%s\nwant\n%s", got, wantConfig)
	}
}

func TestRunCodexColdTaskSandbox(t *testing.T) {
	codexUser(t, "", "")
	f := newCodex(t, fakeSpec{stdout: `{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}` + "\n"})
	a := baseAssignment()
	a.Task = &protocol.TaskSpec{ID: "t1"}
	o := waitOutcome(t, RunCodex(a, RunOptions{BinPath: f.bin, SearchPath: searchPath, WorkDir: t.TempDir()}, &recSink{}))
	if !o.OK {
		t.Fatalf("outcome = %+v", o)
	}
	if got, want := f.argv(), []string{"exec", "--json", "--skip-git-repo-check", "-"}; !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q", got)
	}
}

func TestRunCodexDegradesOnOldVersions(t *testing.T) {
	codexUser(t, "", "")
	t.Run("no --json", func(t *testing.T) {
		f := newCodex(t, fakeSpec{files: map[string]string{"help.txt": "Usage: codex exec [OPTIONS] [PROMPT]\n"}})
		logs := &logRec{}
		o := waitOutcome(t, RunCodex(baseAssignment(), RunOptions{BinPath: f.bin, SearchPath: searchPath, WorkDir: t.TempDir(), Log: logs.log}, &recSink{}))
		if o != (protocol.RunOutcome{Reason: "harness_incompatible: this codex version has no `exec --json`; update the Codex CLI"}) {
			t.Fatalf("outcome = %+v", o)
		}
		if _, ok := logs.find("codex: exec --help lacks --json; version too old"); !ok {
			t.Errorf("logs = %v", logs.msgs())
		}
		if f.ran() {
			t.Error("codex was spawned anyway")
		}
	})
	t.Run("no exec resume", func(t *testing.T) {
		f := newCodex(t, fakeSpec{files: map[string]string{"help.txt": "      --json  Print events\n"}})
		o := waitOutcome(t, RunCodex(baseAssignment(), RunOptions{BinPath: f.bin, SearchPath: searchPath, WorkDir: t.TempDir(), ResumeSessionID: "th-1"}, &recSink{}))
		if o != (protocol.RunOutcome{Reason: "spawn_failed: this codex version has no `exec resume`"}) {
			t.Fatalf("outcome = %+v", o)
		}
		if f.ran() {
			t.Error("codex was spawned anyway")
		}
	})
	t.Run("no --skip-git-repo-check", func(t *testing.T) {
		f := newCodex(t, fakeSpec{files: map[string]string{"help.txt": "      --json  Print events\n"}})
		waitOutcome(t, RunCodex(baseAssignment(), RunOptions{BinPath: f.bin, SearchPath: searchPath, WorkDir: t.TempDir()}, &recSink{}))
		if got, want := f.argv(), []string{"exec", "--json", "-"}; !reflect.DeepEqual(got, want) {
			t.Errorf("argv = %q", got)
		}
	})
}

func TestRunCodexHelpProbeFailuresAreCached(t *testing.T) {
	codexUser(t, "", "")
	f := newCodex(t, fakeSpec{pre: `if [ "$2" = --help ]; then echo probe >> "$D/help-calls"; cat "$D/help.txt"; exit 1; fi`})
	for range 2 {
		o := waitOutcome(t, RunCodex(baseAssignment(), RunOptions{BinPath: f.bin, SearchPath: searchPath, WorkDir: t.TempDir()}, &recSink{}))
		if !strings.HasPrefix(o.Reason, "harness_incompatible: ") {
			t.Fatalf("outcome = %+v", o)
		}
	}
	// One probe per binary, failures included (the TS promise cache).
	if got := strings.Count(f.read("help-calls"), "probe"); got != 1 {
		t.Fatalf("probed %d times", got)
	}
}

func TestRunCodexHelpProbeTimesOut(t *testing.T) {
	codexUser(t, "", "")
	old := helpTimeout
	helpTimeout = 200 * time.Millisecond
	t.Cleanup(func() { helpTimeout = old })
	f := newCodex(t, fakeSpec{pre: `if [ "$2" = --help ]; then exec sleep 30; fi`})
	start := time.Now()
	o := waitOutcome(t, RunCodex(baseAssignment(), RunOptions{BinPath: f.bin, SearchPath: searchPath, WorkDir: t.TempDir()}, &recSink{}))
	if !strings.HasPrefix(o.Reason, "harness_incompatible: ") || time.Since(start) > 10*time.Second {
		t.Fatalf("outcome = %+v after %s", o, time.Since(start))
	}
}

func TestRunCodexNonZeroExit(t *testing.T) {
	codexUser(t, "", "")
	for name, c := range map[string]struct {
		tail   string
		reason string
		code   any
		stderr string
	}{
		"exit code": {"echo 'codex: fatal' >&2\nexit 7", "harness_exit_7", 7, "codex: fatal\n"},
		"signal":    {"kill -9 $$", "harness_exit_null", nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCodex(t, fakeSpec{stdout: `{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":1}}` + "\n", tail: c.tail})
			logs := &logRec{}
			o := waitOutcome(t, RunCodex(baseAssignment(), RunOptions{BinPath: f.bin, SearchPath: searchPath, WorkDir: t.TempDir(), Log: logs.log}, &recSink{}))
			if o != (protocol.RunOutcome{Reason: c.reason, Usage: protocol.Usage{InputTokens: 6, OutputTokens: 1}}) {
				t.Fatalf("outcome = %+v", o)
			}
			if extra, ok := logs.find("codex exited non-zero"); !ok || extra["code"] != c.code || extra["stderr"] != c.stderr {
				t.Fatalf("log = %#v", extra)
			}
		})
	}
}

// Codex's own error message (a usage limit) rides the failure reason, so the
// chat says why the agent stopped instead of a bare exit code.
func TestRunCodexFailureCarriesItsErrorMessage(t *testing.T) {
	codexUser(t, "", "")
	stream := `{"type":"error","message":"You've hit your usage limit.\n Try again later."}` + "\n" +
		`{"type":"turn.failed","error":{"message":"You've hit your usage limit.\n Try again later."}}` + "\n"
	f := newCodex(t, fakeSpec{stdout: stream, tail: "exit 1"})
	o := waitOutcome(t, RunCodex(baseAssignment(), RunOptions{BinPath: f.bin, SearchPath: searchPath, WorkDir: t.TempDir()}, &recSink{}))
	if o.Reason != "harness_exit_1: You've hit your usage limit. Try again later." {
		t.Fatalf("reason = %q", o.Reason)
	}
}

func TestRunCodexKillTakesDownTheTree(t *testing.T) {
	codexUser(t, "", "")
	f := newCodex(t, fakeSpec{
		stdout: `{"type":"turn.started"}` + "\n",
		tail:   `sleep 300 & echo $! > "$D/child.pid"` + "\nwait",
	})
	r := RunCodex(baseAssignment(), RunOptions{BinPath: f.bin, SearchPath: searchPath, WorkDir: t.TempDir()}, &recSink{})
	child, _ := strconv.Atoi(f.waitFile("child.pid"))
	r.Kill("abort:user")
	r.Kill("deadline")
	o := waitOutcome(t, r)
	if o.OK || o.Reason != "abort:user" {
		t.Fatalf("outcome = %+v", o)
	}
	waitDead(t, r.(*run).pid)
	waitDead(t, child)
}

func TestRunCodexKillDuringProbe(t *testing.T) {
	codexUser(t, "", "")
	f := newCodex(t, fakeSpec{
		pre:  `if [ "$2" = --help ]; then sleep 1; cat "$D/help.txt"; exit 0; fi`,
		tail: `sleep 300 & echo $! > "$D/child.pid"` + "\nwait",
	})
	start := time.Now()
	r := RunCodex(baseAssignment(), RunOptions{BinPath: f.bin, SearchPath: searchPath, WorkDir: t.TempDir()}, &recSink{})
	r.Kill("deadline") // the probe is still running: no child to signal yet
	o := waitOutcome(t, r)
	if o.OK || o.Reason != "deadline" {
		t.Fatalf("outcome = %+v", o)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("killed child lingered: %s", time.Since(start))
	}
	// The child spawned after the probe was killed the moment it existed.
	waitDead(t, r.(*run).pid)
	if b, err := os.ReadFile(filepath.Join(f.dir, "child.pid")); err == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		waitDead(t, pid)
	}
}

func TestRunCodexSetupFailures(t *testing.T) {
	codexUser(t, "", "")
	file := filepath.Join(t.TempDir(), "plain-file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	occupied := t.TempDir()
	if err := os.MkdirAll(filepath.Join(occupied, "codex-home", "config.toml"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, work := range map[string]string{"codex home": filepath.Join(file, "sub"), "config.toml": occupied} {
		t.Run(name, func(t *testing.T) {
			f := newCodex(t, fakeSpec{})
			logs := &logRec{}
			o := waitOutcome(t, RunCodex(baseAssignment(), RunOptions{BinPath: f.bin, SearchPath: searchPath, WorkDir: work, Log: logs.log}, &recSink{}))
			if o.OK || !strings.HasPrefix(o.Reason, "runner_error: ") {
				t.Fatalf("outcome = %+v", o)
			}
			if extra, ok := logs.find("run execution error"); !ok || extra["runID"] != "r1" {
				t.Fatalf("logs = %v", logs.msgs())
			}
			if f.ran() {
				t.Error("codex was spawned anyway")
			}
		})
	}
}

func TestRunCodexSpawnFailure(t *testing.T) {
	codexUser(t, "", "")
	// The binary answers the probe, then disappears before the real spawn.
	f := newCodex(t, fakeSpec{pre: `if [ "$2" = --help ]; then cat "$D/help.txt"; rm -f "$0"; exit 0; fi`})
	o := waitOutcome(t, RunCodex(baseAssignment(), RunOptions{BinPath: f.bin, SearchPath: searchPath, WorkDir: t.TempDir()}, &recSink{}))
	if o.OK || !strings.HasPrefix(o.Reason, "spawn_failed: ") || !strings.Contains(o.Reason, "no such file") {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestRunCodexAuthSync(t *testing.T) {
	t.Run("no user credentials", func(t *testing.T) {
		_, userAuth := codexUser(t, "", "")
		f := newCodex(t, fakeSpec{})
		logs := &logRec{}
		waitOutcome(t, RunCodex(baseAssignment(), RunOptions{BinPath: f.bin, SearchPath: searchPath, WorkDir: t.TempDir(), Log: logs.log}, &recSink{}))
		if extra, ok := logs.find("codex: no auth.json found — run will rely on env auth if any"); !ok || extra["authSrc"] != userAuth {
			t.Fatalf("logs = %v", logs.msgs())
		}
		if _, err := os.Stat(userAuth); err == nil {
			t.Fatal("user auth.json invented")
		}
	})
	t.Run("fresher thread copy survives the seed", func(t *testing.T) {
		_, userAuth := codexUser(t, "2026-09-07T12:46:44Z", "spent")
		work := t.TempDir()
		if err := os.MkdirAll(filepath.Join(work, "codex-home"), 0o700); err != nil {
			t.Fatal(err)
		}
		writeAuth(t, filepath.Join(work, "codex-home", "auth.json"), "2026-09-30T09:43:58Z", "live")
		f := newCodex(t, fakeSpec{tail: `cp "$CODEX_HOME/auth.json" "$D/seeded-auth"`})
		logs := &logRec{}
		waitOutcome(t, RunCodex(baseAssignment(), RunOptions{BinPath: f.bin, SearchPath: searchPath, WorkDir: work, Log: logs.log}, &recSink{}))
		if !strings.Contains(f.read("seeded-auth"), "live") {
			t.Fatalf("seeded = %s", f.read("seeded-auth"))
		}
		// The warm thread's earlier refresh flows back to the user.
		if !strings.Contains(readFile(t, userAuth), "live") {
			t.Fatalf("user auth = %s", readFile(t, userAuth))
		}
	})
	t.Run("write-back failure is logged", func(t *testing.T) {
		_, userAuth := codexUser(t, "", "")
		// The user's auth.json is an occupied directory: nothing can replace it.
		if err := os.MkdirAll(filepath.Join(userAuth, "x"), 0o755); err != nil {
			t.Fatal(err)
		}
		f := newCodex(t, fakeSpec{tail: `printf '{"last_refresh":"2026-10-01T00:00:00Z"}' > "$CODEX_HOME/auth.json"`})
		logs := &logRec{}
		o := waitOutcome(t, RunCodex(baseAssignment(), RunOptions{BinPath: f.bin, SearchPath: searchPath, WorkDir: t.TempDir(), Log: logs.log}, &recSink{}))
		if !o.OK {
			t.Fatalf("outcome = %+v", o)
		}
		extra, ok := logs.find("codex: credential write-back failed")
		if !ok || !strings.Contains(extra["error"].(string), userAuth) {
			t.Fatalf("logs = %v", logs.msgs())
		}
	})
	t.Run("default user home is ~/.codex", func(t *testing.T) {
		fakeHome := t.TempDir()
		t.Setenv("HOME", fakeHome)
		t.Setenv("CODEX_HOME", "")
		if err := os.MkdirAll(filepath.Join(fakeHome, ".codex"), 0o700); err != nil {
			t.Fatal(err)
		}
		writeAuth(t, filepath.Join(fakeHome, ".codex", "auth.json"), "2026-09-07T12:46:44Z", "home-token")
		f := newCodex(t, fakeSpec{tail: `cp "$CODEX_HOME/auth.json" "$D/seeded-auth"`})
		waitOutcome(t, RunCodex(baseAssignment(), RunOptions{BinPath: f.bin, SearchPath: searchPath, WorkDir: t.TempDir()}, &recSink{}))
		if !strings.Contains(f.read("seeded-auth"), "home-token") {
			t.Fatalf("seeded = %s", f.read("seeded-auth"))
		}
	})
}
