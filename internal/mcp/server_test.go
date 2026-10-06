package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/spill"
	"github.com/DigitalTolk/ex-runners/internal/toolindex"
)

func TestToolsJSONIsTheFullContract(t *testing.T) {
	if len(allTools) != 45 {
		t.Fatalf("tools = %d", len(allTools))
	}
	for _, tl := range allTools {
		if tl.Name == "" || tl.Description == "" || !json.Valid(tl.InputSchema) {
			t.Errorf("bad tool %+v", tl.Name)
		}
	}
	defer func() {
		if recover() == nil {
			t.Error("invalid tools.json must panic at startup")
		}
	}()
	mustTools([]byte("{"))
}

func TestLoadEnv(t *testing.T) {
	vals := map[string]string{
		"EX_BASE_URL":       "https://ex",
		"EX_RUN_TOKEN":      "tok",
		"EX_TASK_ID":        "t1",
		"EX_TASK_DIR":       "/w/p",
		"EX_TASK_PROJECT":   "p",
		"EX_AUTO_ALLOW":     " read , ,shell",
		"EX_ACTION_MODE":    "draft",
		"EX_TOOL_INDEX":     "1",
		"EX_TASK_REPOS":     `[{"path":"a/b","role":"frontend","dir":"/w/p/b","branch":"x","base":"main"},{"path":"","dir":"/x","branch":"y"}]`,
		"EX_CONNECTORS":     `[{"slug":"hub","title":"Hub","baseURL":"https://hub","token":"t"},{"slug":"nobase","token":"t"}]`,
		"EX_WORKSPACE_ROOT": "/w",
	}
	e := LoadEnv(func(k string) string { return vals[k] }, "/cwd")
	if e.WorkDir != "/cwd" || !e.Gated || !e.ToolIndex || !e.AutoAllow["read"] || !e.AutoAllow["shell"] || len(e.AutoAllow) != 2 {
		t.Errorf("env = %+v", e)
	}
	if len(e.TaskRepos) != 1 || e.TaskRepos[0].Path != "a/b" || len(e.Connectors) != 1 || e.Connectors["hub"].Title != "Hub" {
		t.Errorf("repos=%v conns=%v", e.TaskRepos, e.Connectors)
	}
	vals["EX_WORK_DIR"] = "/work"
	vals["EX_ACTION_MODE"] = "autonomous"
	vals["EX_CONNECTORS"] = "not json"
	e = LoadEnv(func(k string) string { return vals[k] }, "/cwd")
	if e.WorkDir != "/work" || e.Gated || len(e.Connectors) != 0 {
		t.Errorf("env2 = %+v", e)
	}
	for tool, want := range map[string]string{"Read": "read", "Grep": "read", "Write": "edit", "Bash": "shell", "WebSearch": "web", "Task": ""} {
		if toolClass(tool) != want {
			t.Errorf("toolClass(%s) = %q", tool, toolClass(tool))
		}
	}
}

func names(tools []toolindex.ToolDef) map[string]bool {
	out := map[string]bool{}
	for _, t := range tools {
		out[t.Name] = true
	}
	return out
}

func TestVisibleTools(t *testing.T) {
	b := newBackend(t)
	chat := names(testServer(t, b, nil).visibleTools())
	if chat["request_mr"] || !chat["post_message"] || !chat["connector_call"] {
		t.Errorf("chat run tools wrong")
	}
	task := names(testServer(t, b, func(e *Env) { e.TaskID = "t1" }).visibleTools())
	if !task["request_mr"] || !task["publish_test_plan"] {
		t.Error("task tools hidden on a task run")
	}
	gated := names(testServer(t, b, func(e *Env) { e.Gated = true }).visibleTools())
	for n := range gatedHidden {
		if gated[n] {
			t.Errorf("%s visible on a gated run", n)
		}
	}
	if !gated["get_thread"] {
		t.Error("read tools must stay on gated runs")
	}
}

// session drives Serve over pipes, one JSON-RPC line at a time.
type session struct {
	in  *io.PipeWriter
	out *bufio.Reader
}

func serve(t *testing.T, s *Server) *session {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		s.Serve(inR, outW)
		_ = outW.Close()
	}()
	t.Cleanup(func() { _ = inW.Close() })
	return &session{in: inW, out: bufio.NewReader(outR)}
}

func (ss *session) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(ss.in, line+"\n"); err != nil {
		t.Fatal(err)
	}
}

func (ss *session) recv(t *testing.T) map[string]any {
	t.Helper()
	line, err := ss.out.ReadString('\n')
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("bad line %q", line)
	}
	return m
}

func TestJSONRPCProtocol(t *testing.T) {
	b := newBackend(t)
	ss := serve(t, testServer(t, b, nil))

	ss.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	init := ss.recv(t)
	res := init["result"].(map[string]any)
	if init["id"] != float64(1) || res["protocolVersion"] != "2024-11-05" || res["serverInfo"].(map[string]any)["name"] != "ex" {
		t.Errorf("initialize = %v", init)
	}
	// Notifications get no reply; garbage is ignored; ping answers.
	ss.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	ss.send(t, `not json at all`)
	ss.send(t, ``)
	ss.send(t, `{"jsonrpc":"2.0","id":"p","method":"ping"}`)
	if ping := ss.recv(t); ping["id"] != "p" {
		t.Errorf("ping = %v", ping)
	}
	ss.send(t, `{"jsonrpc":"2.0","method":"ping"}`) // like the TS: answered with id null
	if ping := ss.recv(t); ping["id"] != nil || ping["result"] == nil {
		t.Errorf("id-less ping = %v", ping)
	}
	ss.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	list := ss.recv(t)["result"].(map[string]any)["tools"].([]any)
	if len(list) == 0 || list[0].(map[string]any)["name"] != "post_message" {
		t.Errorf("tools/list = %v", list[:1])
	}
	ss.send(t, `{"jsonrpc":"2.0","id":3,"method":"bogus"}`)
	errMsg := ss.recv(t)["error"].(map[string]any)
	if errMsg["code"] != float64(-32601) || errMsg["message"] != "method not found: bogus" {
		t.Errorf("bogus = %v", errMsg)
	}
	ss.send(t, `{"jsonrpc":"2.0","method":"bogus-notification"}`) // no id → no reply
	b.on("GET /api/v1/agent/run/thread", answer{200, `{"text":"[m:1] alice: hi"}`})
	ss.send(t, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_thread"}}`)
	call := ss.recv(t)["result"].(map[string]any)
	if call["isError"] != false || call["content"].([]any)[0].(map[string]any)["text"] != "[m:1] alice: hi" {
		t.Errorf("tools/call = %v", call)
	}
	// A JSON null body reads as {}.
	b.on("GET /api/v1/agent/run/thread", answer{200, `null`})
	ss.send(t, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"get_thread"}}`)
	if nullBody := ss.recv(t)["result"].(map[string]any); nullBody["isError"] != false {
		t.Errorf("null body = %v", nullBody)
	}
	if b.last().auth != "Bearer run-tok" {
		t.Errorf("auth = %q", b.last().auth)
	}
}

func TestSpillAndToolIndexMode(t *testing.T) {
	b := newBackend(t)
	big := strings.Repeat("x", spill.InlineMax+10)
	b.on("GET /api/v1/agent/run/thread", answer{200, `{"text":"` + big + `"}`})
	s := testServer(t, b, nil)
	r := s.applySpill("get_thread", s.safeCall("get_thread", nil))
	if !strings.Contains(r.Content[0].Text, "sp-") {
		t.Errorf("big result not spilled: %d chars", len(r.Content[0].Text))
	}
	// fetch_spill output, approval_prompt frames, errors and empty results never re-spill.
	for _, name := range []string{"fetch_spill", "approval_prompt"} {
		if got := s.applySpill(name, ok(big)); got.Content[0].Text != big {
			t.Errorf("%s spilled", name)
		}
	}
	if got := s.applySpill("x", fail(big)); got.Content[0].Text != big {
		t.Error("error spilled")
	}
	if got := s.applySpill("x", toolResult{}); len(got.Content) != 0 {
		t.Error("empty result changed")
	}

	idx := testServer(t, b, func(e *Env) { e.ToolIndex = true })
	ss := serve(t, idx)
	ss.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	listed := ss.recv(t)["result"].(map[string]any)["tools"].([]any)
	if len(listed) >= len(allTools) {
		t.Errorf("index mode listed %d tools", len(listed))
	}
	ss.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ex_tool_info","arguments":{"names":["link_message"]}}}`)
	if info := ss.recv(t)["result"].(map[string]any); info["isError"] != false {
		t.Errorf("tool info = %v", info)
	}
	ss.send(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ex_tool_call","arguments":{"name":"link_message","arguments":{}}}}`)
	inner := ss.recv(t)["result"].(map[string]any)
	if inner["isError"] != false || !strings.HasPrefix(inner["content"].([]any)[0].(map[string]any)["text"].(string), "Not run: missing required arguments message_id.") {
		t.Errorf("unwrapped call without required args = %v", inner)
	}
	ss.send(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ex_tool_call","arguments":{"tool":"link_message","arguments":{"message_id":"m1","channel_id":"c1"}}}}`)
	if ran := ss.recv(t)["result"].(map[string]any); ran["isError"] != false {
		t.Errorf("unwrapped call = %v", ran)
	}
	ss.send(t, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"ex_tool_call","arguments":{"name":"nope"}}}`)
	if bad := ss.recv(t)["result"].(map[string]any); bad["isError"] != true {
		t.Errorf("unknown inner tool = %v", bad)
	}
	// A tool not yet described returns its contract first.
	fresh := testServer(t, b, func(e *Env) { e.ToolIndex = true })
	ss2 := serve(t, fresh)
	ss2.send(t, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"ex_tool_call","arguments":{"name":"set_reminder","arguments":{}}}}`)
	if c := ss2.recv(t)["result"].(map[string]any); c["isError"] != false {
		t.Errorf("contract reply = %v", c)
	}
	// Inner call with no arguments object.
	ss2.send(t, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"ex_tool_call","arguments":{"name":"set_reminder"}}}`)
	if c := ss2.recv(t)["result"].(map[string]any); c["isError"] != true {
		t.Errorf("no-args inner call = %v", c)
	}
}

func TestMain(t *testing.T) {
	var errOut bytes.Buffer
	if code := Main(func(string) string { return "" }, "/", strings.NewReader(""), io.Discard, &errOut); code != 1 || !strings.Contains(errOut.String(), "EX_BASE_URL and EX_RUN_TOKEN are required") {
		t.Errorf("missing env = %d %s", code, errOut.String())
	}
	env := map[string]string{"EX_BASE_URL": "http://x", "EX_RUN_TOKEN": "t"}
	if code := Main(func(k string) string { return env[k] }, "/", strings.NewReader(""), io.Discard, io.Discard); code != 0 {
		t.Errorf("clean EOF exit = %d", code)
	}
}

func TestTransportErrorsAreRetryable(t *testing.T) {
	b := newBackend(t)
	s := testServer(t, b, func(e *Env) { e.BaseURL = "http://127.0.0.1:1" })
	expect(t, s, "get_thread", nil, true, "tool transport error:")
	expect(t, s, "get_thread", nil, true, "[retryable=true]")
	s2 := testServer(t, b, func(e *Env) { e.BaseURL = "http://bad host" })
	expect(t, s2, "get_thread", nil, true, "tool transport error:")
	expect(t, s2, "post_message", map[string]any{"body": "x"}, true, "tool transport error:")
	// A body that can't be encoded is a transport error too.
	expect(t, testServer(t, b, nil), "create_coding_task", map[string]any{"project": "p", "title": "t", "goal": "g", "repos": []any{make(chan int)}}, true, "tool transport error:")
	// Results that can't be encoded become a tool error, never a dead server.
	var out bytes.Buffer
	s3 := testServer(t, b, nil)
	s3.out = &out
	s3.send(map[string]any{"jsonrpc": "2.0", "id": 1, "result": make(chan int)})
	if !strings.Contains(out.String(), "tool result could not be encoded") {
		t.Errorf("encode failure = %s", out.String())
	}
	_ = time.Now
}

func TestGatedModesRefuseHiddenToolsByName(t *testing.T) {
	b := newBackend(t)
	s := testServer(t, b, func(e *Env) { e.ActionMode, e.Gated = "notify", true })
	expect(t, s, "post_message", map[string]any{"body": "hi"}, true, "post_message is not available in this notify run — your final text is the deliverable [retryable=false]")
	if len(b.calls) != 0 {
		t.Error("a hidden tool reached the backend")
	}
	expect(t, s, "get_thread", nil, false, "")
}
