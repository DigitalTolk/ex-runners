package harness

import (
	"bytes"
	"fmt"
	"github.com/DigitalTolk/ex-runners/internal/textutil"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

// TestMain points CODEX_HOME at a scratch dir so no test can ever touch the
// real ~/.codex credentials, and clears the runner's opt-in switches.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "harness-codex-home-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("CODEX_HOME", home)
	_ = os.Unsetenv("EX_RUNNER_TOOL_INDEX")
	_ = os.Unsetenv("EX_RUNNER_FULL_TOOLS")
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

// --- recording doubles ---

type event struct {
	Kind   string
	Name   string
	Detail string
	Text   string
	In     int
	Out    int
	Cache  *Cache
	Input  map[string]any
}

type recSink struct {
	mu     sync.Mutex
	events []event
}

func (s *recSink) add(e event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *recSink) Turn() { s.add(event{Kind: "turn"}) }
func (s *recSink) Usage(in, out int, cache *Cache) {
	s.add(event{Kind: "usage", In: in, Out: out, Cache: cache})
}
func (s *recSink) Progress(text string) { s.add(event{Kind: "progress", Text: text}) }
func (s *recSink) Tool(name, detail string, input map[string]any) {
	s.add(event{Kind: "tool", Name: name, Detail: detail, Input: input})
}
func (s *recSink) ToolResult(name, detail string) {
	s.add(event{Kind: "tool_result", Name: name, Detail: detail})
}

func (s *recSink) list() []event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.events)
}

type logLine struct {
	Msg   string
	Extra map[string]any
}

type logRec struct {
	mu    sync.Mutex
	lines []logLine
}

func (l *logRec) log(msg string, extra map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, logLine{msg, extra})
}

func (l *logRec) find(msg string) (map[string]any, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ln := range l.lines {
		if ln.Msg == msg {
			return ln.Extra, true
		}
	}
	return nil, false
}

func (l *logRec) msgs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, ln := range l.lines {
		out = append(out, ln.Msg)
	}
	return out
}

func assertEvents(t *testing.T, got, want []event) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		var b strings.Builder
		for i := range max(len(got), len(want)) {
			var g, w any = "<none>", "<none>"
			if i < len(got) {
				g = fmt.Sprintf("%+v", got[i])
			}
			if i < len(want) {
				w = fmt.Sprintf("%+v", want[i])
			}
			mark := "  "
			if g != w {
				mark = "!!"
			}
			fmt.Fprintf(&b, "%s %d got  %v\n   %d want %v\n", mark, i, g, i, w)
		}
		t.Fatalf("events differ:\n%s", b.String())
	}
}

func baseAssignment() protocol.Assignment {
	return protocol.Assignment{
		RunID:         "r1",
		AgentID:       "agent-gg",
		AgentName:     "gg",
		InvokerName:   "Alice",
		ParentID:      "chan1",
		MessageID:     "m2",
		Harness:       "claude",
		Persona:       "You are helpful.",
		Prompt:        "do the thing",
		ContextBundle: "# Task",
		MCPToken:      "tok",
	}
}

// --- flattenToolResult ---

func TestFlattenRendersStringWithSize(t *testing.T) {
	if got := FlattenToolResult("hello  world"); got != "hello world (11B)" {
		t.Fatalf("got %q", got)
	}
}

func TestFlattenJoinsBlocksToleratingEntriesWithoutText(t *testing.T) {
	blocks := []any{
		map[string]any{"type": "text", "text": "a"},
		map[string]any{"type": "image"},
		map[string]any{"type": "text", "text": "b"},
	}
	if got := FlattenToolResult(blocks); got != "a b (3B)" {
		t.Fatalf("got %q", got)
	}
	typed := []map[string]any{{"type": "text", "text": "a"}, {"type": "image"}, {"type": "text", "text": "b"}}
	if got := FlattenToolResult(typed); got != "a b (3B)" {
		t.Fatalf("typed got %q", got)
	}
}

func TestFlattenReturnsEmptyForMissingOrWhitespaceContent(t *testing.T) {
	for _, c := range []any{nil, "   \n  ", 42, map[string]any{"text": "x"}, []any{}} {
		if got := FlattenToolResult(c); got != "" {
			t.Errorf("FlattenToolResult(%#v) = %q", c, got)
		}
	}
}

func TestFlattenClipsLongResultsAndReportsKB(t *testing.T) {
	if got, want := FlattenToolResult(strings.Repeat("x", 2048)), strings.Repeat("x", 220)+"… (2.0KB)"; got != want {
		t.Fatalf("got %q", got)
	}
	// Exactly 220 characters: no ellipsis.
	if got, want := FlattenToolResult(strings.Repeat("y", 220)), strings.Repeat("y", 220)+" (220B)"; got != want {
		t.Fatalf("got %q", got)
	}
}

func TestFlattenScalarsAndOddBlocksAsJavaScriptJoins(t *testing.T) {
	blocks := []any{
		map[string]any{"text": 42.0},
		map[string]any{"text": true},
		map[string]any{"text": map[string]any{"x": 1}},
		"bare string",
		nil,
		map[string]any{"text": "end"},
	}
	if got := FlattenToolResult(blocks); got != "42 true end (11B)" {
		t.Fatalf("got %q", got)
	}
}

func TestFlattenUsesJavaScriptWhitespaceAndCharacterCounts(t *testing.T) {
	// Node: flattenToolResult('a\u00a0\u00a0b\u000bc\u0085d') === 'a b c\u0085d (7B)'
	// — NBSP and \v are \s in JavaScript, U+0085 is not.
	if got, want := FlattenToolResult("a\u00a0\u00a0b\u000bc\u0085d"), "a b c\u0085d (7B)"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := FlattenToolResult("\ufeff héllo\u2028"); got != "héllo (5B)" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("é", 230)
	if got, want := FlattenToolResult(long), strings.Repeat("é", 220)+"… (230B)"; got != want {
		t.Fatalf("got %q", got)
	}
}

func TestJSTrim(t *testing.T) {
	for in, want := range map[string]string{
		"  \u00a0x y\u2028 ": "x y",
		"\u0085x":            "\u0085x",
		"ERROR: exit 1 ":     "ERROR: exit 1",
		"":                   "",
		" \t\n":              "",
	} {
		if got := textutil.JSTrim(in); got != want {
			t.Errorf("textutil.JSTrim(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- runHasConnectors ---

func TestRunHasConnectorsForPicksInstalledAndConnected(t *testing.T) {
	cases := []protocol.Assignment{
		{ConnectorSlugs: []string{"hub"}},
		{ContextBundle: "x\n# Installed connectors\n- hub"},
		{ContextBundle: "note: [connected services] here"},
	}
	for _, a := range cases {
		if !RunHasConnectors(a) {
			t.Errorf("RunHasConnectors(%+v) = false", a)
		}
	}
}

func TestRunHasConnectorsFalseWithNothingToConnectTo(t *testing.T) {
	if RunHasConnectors(protocol.Assignment{ConnectorSlugs: []string{}, ContextBundle: "plain thread"}) {
		t.Fatal("RunHasConnectors = true")
	}
}

// --- systemRules ---

func golden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSystemRulesLayersPersonaAndNames(t *testing.T) {
	rules := SystemRules("gg", "Alice", "You are helpful.", RulesOptions{Connectors: false})
	if !strings.HasPrefix(rules, "You are helpful.") {
		t.Error("persona not first")
	}
	for _, want := range []string{`You are "gg"`, "invoked by Alice"} {
		if !strings.Contains(rules, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(rules, "connector_call is the ONLY way") {
		t.Error("connector rules present")
	}
}

func TestSystemRulesAddsConnectorWorkflowOnlyWhenReachable(t *testing.T) {
	if !strings.Contains(SystemRules("gg", "Alice", "p", RulesOptions{Connectors: true}), "connector_call is the ONLY way") {
		t.Error("connector rules missing")
	}
	// Zero options → no connector rules (the TS default-parameter branch).
	if strings.Contains(SystemRules("gg", "Alice", "p", RulesOptions{}), "connector_call is the ONLY way") {
		t.Error("connector rules present by default")
	}
}

// The goldens were produced by the TypeScript systemRules itself; the
// prompt must stay byte-identical.
func TestSystemRulesByteIdenticalToTypeScript(t *testing.T) {
	if got, want := SystemRules("gg", "Alice", "You are helpful.", RulesOptions{}), golden(t, "system_rules_plain.golden"); got != want {
		t.Errorf("plain rules differ from TS:\n%s", firstDiff(got, want))
	}
	if got, want := SystemRules("gg", "Alice", "You are helpful.", RulesOptions{Connectors: true}), golden(t, "system_rules_connectors.golden"); got != want {
		t.Errorf("connector rules differ from TS:\n%s", firstDiff(got, want))
	}
}

func firstDiff(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := range max(len(g), len(w)) {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			return fmt.Sprintf("line %d:\n got  %q\n want %q", i+1, gl, wl)
		}
	}
	return "(no line differs)"
}

// --- claudeToolArgs ---

func TestClaudeToolArgsTrimsChatRunsToLeanToolset(t *testing.T) {
	args := ClaudeToolArgs(protocol.Assignment{}, func(string) string { return "" })
	want := []string{"--tools", "Bash,PowerShell,Read,Grep,Glob,ToolSearch,WebFetch,WebSearch", "--disable-slash-commands"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %q", args)
	}
	// Without ToolSearch every Ex MCP tool would load up front.
	for _, needed := range []string{"ToolSearch", "Bash", "PowerShell", "Read"} {
		if !slices.Contains(LeanClaudeTools, needed) {
			t.Errorf("LeanClaudeTools lacks %s", needed)
		}
	}
}

func TestClaudeToolArgsLeavesCodingTasksAndFullToolRunsAlone(t *testing.T) {
	if got := ClaudeToolArgs(protocol.Assignment{Task: &protocol.TaskSpec{ID: "t"}}, func(string) string { return "" }); len(got) != 0 {
		t.Errorf("task args = %q", got)
	}
	full := func(k string) string {
		if k == "EX_RUNNER_FULL_TOOLS" {
			return "1"
		}
		return ""
	}
	if got := ClaudeToolArgs(protocol.Assignment{}, full); len(got) != 0 {
		t.Errorf("full-tools args = %q", got)
	}
}

func TestClaudeToolArgsReadsProcessEnvByDefault(t *testing.T) {
	if got := ClaudeToolArgs(protocol.Assignment{}, nil); len(got) != 3 {
		t.Fatalf("args = %q", got)
	}
	t.Setenv("EX_RUNNER_FULL_TOOLS", "1")
	if got := ClaudeToolArgs(protocol.Assignment{}, nil); len(got) != 0 {
		t.Fatalf("args = %q", got)
	}
}

// --- small plumbing ---

func TestTomlStringIsJSONStringify(t *testing.T) {
	// Node: JSON.stringify('a"\\\b\f\n\r\t\u0001\u001f\u007f\u2028é😀')
	in := "a\"\\\b\f\n\r\t\x01\x1f\x7f\u2028é😀"
	want := `"a\"\\\b\f\n\r\t\u0001\u001f` + "\x7f\u2028é😀" + `"`
	if got := tomlString(in); got != want {
		t.Fatalf("tomlString = %q, want %q", got, want)
	}
	if got := tomlString(""); got != `""` {
		t.Fatalf("empty = %q", got)
	}
	if got := tomlString("a\xffb"); got != "\"a\ufffdb\"" {
		t.Fatalf("invalid UTF-8 = %q", got)
	}
}

func readAll(t *testing.T, r interface{ Read([]byte) (int, error) }) []string {
	t.Helper()
	var lines []string
	readLines(r, func(b []byte) { lines = append(lines, string(b)) })
	return lines
}

func TestReadLinesSplitsLikeNodeReadline(t *testing.T) {
	in := "a\nb\r\nc\rd\r\n\ne"
	want := []string{"a", "b", "c", "d", "", "e"}
	if got := readAll(t, strings.NewReader(in)); !reflect.DeepEqual(got, want) {
		t.Fatalf("lines = %q", got)
	}
	// One byte at a time: a \r at the end of the buffered data must wait
	// for the next byte before deciding (CRLF is one break, not two).
	if got := readAll(t, iotest.OneByteReader(strings.NewReader(in))); !reflect.DeepEqual(got, want) {
		t.Fatalf("one-byte lines = %q", got)
	}
	// A trailing \r ends the last line; nothing follows.
	if got := readAll(t, iotest.OneByteReader(strings.NewReader("x\r"))); !reflect.DeepEqual(got, []string{"x"}) {
		t.Fatalf("trailing CR = %q", got)
	}
	if got := readAll(t, strings.NewReader("")); len(got) != 0 {
		t.Fatalf("empty = %q", got)
	}
}

func TestReadLinesHasNoLineCap(t *testing.T) {
	long := strings.Repeat("x", 3<<20)
	got := readAll(t, strings.NewReader(long+"\nnext\n"))
	if len(got) != 2 || len(got[0]) != len(long) || got[1] != "next" {
		t.Fatalf("got %d lines", len(got))
	}
}

func TestDecodeEventToleratesMistypedFields(t *testing.T) {
	var evt claudeEvent
	if decodeEvent([]byte("not json"), &evt) {
		t.Error("noise decoded")
	}
	// The other fields survive; a mistyped pointer field reads as present but
	// empty (as in JS, a non-null value of the wrong type is still present).
	if !decodeEvent([]byte(`{"type":"result","subtype":5,"result":"ok"}`), &evt) || evt.Result != "ok" || evt.Subtype == nil || *evt.Subtype != "" {
		t.Errorf("mistyped field = %+v", evt)
	}
	for _, line := range []string{"null", "42", `"s"`, "[1]"} {
		var e claudeEvent
		if !decodeEvent([]byte(line), &e) || e.Type != "" {
			t.Errorf("%s = %+v", line, e)
		}
	}
}

func TestExitCodeHelpers(t *testing.T) {
	if exitReason(-1) != "harness_exit_null" || exitCodeValue(-1) != nil {
		t.Error("signal death")
	}
	if exitReason(3) != "harness_exit_3" || exitCodeValue(3) != 3 {
		t.Error("exit 3")
	}
}

func TestCapWriterKeepsHeadAndSwallowsRest(t *testing.T) {
	w := &capWriter{}
	if n, err := w.Write(bytes.Repeat([]byte("é"), 3000)); n != 6000 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if n, _ := w.Write([]byte("more")); n != 4 {
		t.Fatalf("Write after cap = %d", n)
	}
	if len(w.buf) != stderrKeep {
		t.Fatalf("kept %d bytes", len(w.buf))
	}
	if got := w.head(); got != strings.Repeat("é", 500) {
		t.Fatalf("head = %d chars", len([]rune(got)))
	}
	short := &capWriter{}
	_, _ = short.Write([]byte("boom"))
	if short.head() != "boom" {
		t.Fatalf("head = %q", short.head())
	}
}

func TestRunKillBookkeeping(t *testing.T) {
	logs := &logRec{}
	r := newRun(logs.log)
	r.Kill("deadline") // before the child exists: remembered, nothing signalled
	r.Kill("abort:x")  // idempotent: the first reason wins
	if got := r.started(0); got != "deadline" {
		t.Fatalf("started = %q", got)
	}
	if r.killed() != "deadline" || len(logs.msgs()) != 0 {
		t.Fatalf("killed = %q, logs = %v", r.killed(), logs.msgs())
	}
	r.finish(protocol.RunOutcome{Reason: "deadline"})
	if first, again := r.Wait(), r.Wait(); first.Reason != "deadline" || again.Reason != "deadline" {
		t.Fatal("Wait not repeatable")
	}
}

func TestOrNopAndCwd(t *testing.T) {
	orNop(nil)("never panics", nil)
	called := false
	orNop(func(string, map[string]any) { called = true })("x", nil)
	if !called {
		t.Error("logger not passed through")
	}
	if cwdOf(RunOptions{WorkDir: "/w"}) != "/w" || cwdOf(RunOptions{WorkDir: "/w", Cwd: "/c"}) != "/c" {
		t.Error("cwdOf")
	}
}

func TestHarnessEnvOverridesLast(t *testing.T) {
	t.Setenv("CI", "false")
	env := harnessEnv("/p", "CI=true")
	if env[len(env)-2] != "PATH=/p" || env[len(env)-1] != "CI=true" {
		t.Fatalf("tail = %q", env[len(env)-2:])
	}
	if !slices.Contains(env, "CI=false") {
		t.Fatal("process env not inherited")
	}
}

// --- per-run config files ---

func TestWriteMCPConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "swept", "thread") // gone: recreated
	path, err := writeMCPConfig(RunOptions{WorkDir: dir, MCPServer: MCPServerCmd{
		Command: "/usr/bin/node",
		Args:    []string{"/x/mcp-server.js"},
		Env:     map[string]string{"EX_RUN_TOKEN": "tok", "EX_BASE_URL": "https://ex.example/?a=1&b=<2>"},
	}})
	if err != nil || path != filepath.Join(dir, "mcp.json") {
		t.Fatalf("writeMCPConfig = %q, %v", path, err)
	}
	b, _ := os.ReadFile(path)
	want := `{"mcpServers":{"ex":{"command":"/usr/bin/node","args":["/x/mcp-server.js"],"env":{"EX_BASE_URL":"https://ex.example/?a=1&b=<2>","EX_RUN_TOKEN":"tok"}}}}`
	if string(b) != want {
		t.Fatalf("mcp.json =\n%s\nwant\n%s", b, want)
	}
	// Nil args/env still serialise as [] and {} — never null.
	path, _ = writeMCPConfig(RunOptions{WorkDir: dir, MCPServer: MCPServerCmd{Command: "node"}})
	b, _ = os.ReadFile(path)
	if string(b) != `{"mcpServers":{"ex":{"command":"node","args":[],"env":{}}}}` {
		t.Fatalf("mcp.json = %s", b)
	}
}

func TestWriteCodexHomeEscapesEverything(t *testing.T) {
	work := t.TempDir()
	logs := &logRec{}
	a := baseAssignment()
	a.Model = `gpt-"5"`
	home, err := writeCodexHome(a, RunOptions{WorkDir: work, MCPServer: MCPServerCmd{
		Command: `C:\node "x".exe`,
		Args:    []string{"a b", "line\nbreak"},
		Env:     map[string]string{"Z": "last", "A": "tab\there", "EX_RUN_TOKEN": "tok", "EX_CONNECTORS": "[]"},
	}}, logs.log)
	if err != nil || home != filepath.Join(work, "codex-home") {
		t.Fatalf("writeCodexHome = %q, %v", home, err)
	}
	b, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	want := strings.Join([]string{
		"# Generated per-run by the Ex runner. Deleted with the run.",
		`model = "gpt-\"5\""`,
		`approval_policy = "never"`,
		"include_permissions_instructions = false",
		"include_environment_context = false",
		"include_apps_instructions = false",
		strings.Join(codexProfile(false, home, nil), "\n"),
		"[history]",
		`persistence = "save-all"`,
		"",
		"[mcp_servers.ex]",
		`command = "C:\\node \"x\".exe"`,
		`args = ["a b", "line\nbreak"]`,
		"tool_timeout_sec = 600",
		`default_tools_approval_mode = "approve"`,
		`env = { "A" = "tab\there", "Z" = "last" }`,
		`env_vars = ["EX_CONNECTORS", "EX_RUN_TOKEN"]`,
		"",
		"[shell_environment_policy]",
		`exclude = ["EX_*"]`,
		"",
	}, "\n") + slimTail(home)
	if string(b) != want {
		t.Fatalf("config.toml =\n%s\nwant\n%s", b, want)
	}
	if _, ok := logs.find("codex: no auth.json found — run will rely on env auth if any"); !ok {
		t.Errorf("logs = %v", logs.msgs())
	}
}

func TestWriteCodexHomeEmptyServerAndToolIndex(t *testing.T) {
	t.Setenv("EX_RUNNER_TOOL_INDEX", "1")
	home, err := writeCodexHome(baseAssignment(), RunOptions{WorkDir: t.TempDir()}, protocol.NopLogger)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	for _, want := range []string{"\ncommand = \"\"\n", "\nargs = []\n", "\nenv = { \"EX_TOOL_INDEX\" = \"1\" }\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("config.toml lacks %q:\n%s", want, b)
		}
	}
	t.Setenv("EX_RUNNER_TOOL_INDEX", "")
	home, _ = writeCodexHome(baseAssignment(), RunOptions{WorkDir: t.TempDir()}, protocol.NopLogger)
	b, _ = os.ReadFile(filepath.Join(home, "config.toml"))
	if !strings.Contains(string(b), "\nenv = {  }\n") {
		t.Errorf("empty env line:\n%s", b)
	}
}

func TestCodexAuthFilesFallsBackToHomeDotCodex(t *testing.T) {
	t.Setenv("CODEX_HOME", "/custom/codex")
	if user, thread := codexAuthFiles("/run/home"); user != filepath.Join("/custom/codex", "auth.json") || thread != filepath.Join("/run/home", "auth.json") {
		t.Fatalf("got %s %s", user, thread)
	}
	t.Setenv("CODEX_HOME", "")
	h, _ := os.UserHomeDir()
	if user, _ := codexAuthFiles("/run/home"); user != filepath.Join(h, ".codex", "auth.json") {
		t.Fatalf("fallback = %s", user)
	}
}

// The sandbox profile is the only boundary codex has (no approval prompt):
// credentials, the runner's state and the codex home are denied, reads
// included; no network; chat reads only, a task writes in the checkout.
func TestCodexProfile(t *testing.T) {
	orig := userHomeDir
	defer func() { userHomeDir = orig }()
	userHomeDir = func() (string, error) { return "/h", nil }
	env := map[string]string{"EX_RUNNER_HOME": "/h/.ex-runner", "EX_PRIVATE_DIR": "/s/private/abc"}
	chat := strings.Join(codexProfile(false, "/s/threads/abc/codex-home", env), "\n")
	want := `default_permissions = "ex"

[permissions.ex]
extends = ":read-only"

[permissions.ex.filesystem]
"/h/.aws" = "deny"
"/h/.azure" = "deny"
"/h/.cargo/credentials.toml" = "deny"
"/h/.claude" = "deny"
"/h/.claude.json" = "deny"
"/h/.codex" = "deny"
"/h/.config/gcloud" = "deny"
"/h/.config/gh" = "deny"
"/h/.config/glab-cli" = "deny"
"/h/.docker/config.json" = "deny"
"/h/.ex-runner" = "deny"
"/h/.git-credentials" = "deny"
"/h/.gnupg" = "deny"
"/h/.kube" = "deny"
"/h/.netrc" = "deny"
"/h/.npmrc" = "deny"
"/h/.pypirc" = "deny"
"/h/.ssh" = "deny"
"/h/Library/Keychains" = "deny"
"/s/private/abc" = "deny"
"/s/threads/abc/codex-home" = "deny"

[permissions.ex.network]
enabled = false
`
	if chat != want {
		t.Fatalf("chat profile =\n%s\nwant\n%s", chat, want)
	}
	if task := strings.Join(codexProfile(true, "/c", nil), "\n"); !strings.Contains(task, `extends = ":workspace"`) || !strings.Contains(task, "enabled = false") || !strings.Contains(task, `"/c" = "deny"`) {
		t.Fatalf("task profile = %s", task)
	}
}

// slimTail is the config tail switching off codex scaffolding an Ex agent
// never uses: multi-agent, plugins and each bundled skill.
func slimTail(codexHome string) string {
	out := "\n[agents]\nenabled = false\n\n[features]\nplugins = false\n"
	for _, skill := range []string{"imagegen", "openai-docs", "plugin-creator", "review-agent", "skill-creator", "skill-installer"} {
		out += "\n[[skills.config]]\npath = " + tomlString(filepath.Join(codexHome, "skills", ".system", skill, "SKILL.md")) + "\nenabled = false\n"
	}
	return out
}
