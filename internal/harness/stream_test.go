package harness

import (
	"testing"

	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

func newClaudeStream(sink EventSink, onSession func(string)) *claudeStream {
	return &claudeStream{sink: sink, onSessionID: onSession, toolNames: map[string]string{}, seenAssistant: map[string]bool{}}
}

func feed(handle func([]byte), lines ...string) {
	for _, l := range lines {
		handle([]byte(l))
	}
}

func TestClaudeStreamEdgeShapes(t *testing.T) {
	sink := &recSink{}
	var sessions []string
	st := newClaudeStream(sink, func(id string) { sessions = append(sessions, id) })
	feed(st.handle,
		`{"type":"system","subtype":"init","session_id":"s1"}`,
		`{"type":"system","subtype":"compact_boundary","session_id":"s1"}`,
		`{"type":"assistant","session_id":"not-a-system-event"}`, // no message: a turn, nothing else
		// No message id: every copy counts (nothing to dedupe on).
		`{"type":"assistant","message":{"content":[{"type":"text","text":""}],"usage":{"input_tokens":0,"output_tokens":0}}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":"not an object"},{"type":"tool_use","name":"Read"},{"type":"tool_use","id":"t9","input":{}}]}}`,
		`{"type":"user"}`,
		`{"type":"user","message":{"content":"a plain string"}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"ok"}],"is_error":"yes"}]}}`,
		`{"type":"result","subtype":"error_during_execution","result":null}`,
	)
	assertEvents(t, sink.list(), []event{
		{Kind: "turn"},
		{Kind: "turn"},
		{Kind: "turn"},
		{Kind: "tool", Name: "Bash", Detail: "run ``"},
		{Kind: "tool", Name: "Read", Detail: "read ``"},
		// is_error of a non-boolean type is left unset (TS truthiness of
		// "yes" aside, Claude only ever sends a boolean here).
		{Kind: "tool_result", Name: "Bash", Detail: "ok (2B)"},
	})
	if len(sessions) != 2 {
		t.Errorf("sessions = %v", sessions)
	}
	if st.finalText != "" || *st.resultSubtype != "error_during_execution" || st.usage != (protocol.Usage{}) {
		t.Errorf("state = %q %q %+v", st.finalText, *st.resultSubtype, st.usage)
	}
}

func TestClaudeStreamResultUsageRemainder(t *testing.T) {
	sink := &recSink{}
	st := newClaudeStream(sink, nil)
	feed(st.handle,
		`{"type":"system","session_id":"s1"}`, // nil OnSessionID is fine
		`{"type":"assistant","message":{"id":"m1","content":[],"usage":{"input_tokens":3,"cache_creation_input_tokens":7,"cache_read_input_tokens":90,"output_tokens":4}}}`,
		`{"type":"assistant","message":{"id":"m1","content":[],"usage":{"input_tokens":3,"cache_creation_input_tokens":7,"cache_read_input_tokens":90,"output_tokens":4}}}`,
		// Totals equal to what streamed: no remainder report.
		`{"type":"result","result":"done","usage":{"input_tokens":3,"cache_creation_input_tokens":7,"output_tokens":4}}`,
	)
	assertEvents(t, sink.list(), []event{
		{Kind: "turn"},
		{Kind: "usage", In: 10, Out: 4, Cache: &Cache{Read: 90, Creation: 7}},
	})
	if *st.resultSubtype != "success" || st.usage != (protocol.Usage{InputTokens: 10, OutputTokens: 4}) {
		t.Fatalf("state = %q %+v", *st.resultSubtype, st.usage)
	}
	// A later result without usage resets the totals, as the TS did.
	feed(st.handle, `{"type":"result","subtype":"success","result":"again"}`)
	if st.usage != (protocol.Usage{}) || st.finalText != "again" {
		t.Fatalf("state = %q %+v", st.finalText, st.usage)
	}
	// Totals below what streamed never report a negative remainder;
	// outputs above it do.
	feed(st.handle, `{"type":"result","usage":{"input_tokens":1,"output_tokens":9}}`)
	assertEvents(t, sink.list()[2:], []event{{Kind: "usage", In: 0, Out: 5}})
}

func TestClaudeOutcomeBranches(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := []struct {
		name      string
		killed    string
		code      int
		subtype   *string
		finalText string
		want      protocol.RunOutcome
		logged    bool
	}{
		{"killed", "deadline", 0, str("success"), "x", protocol.RunOutcome{Reason: "deadline"}, false},
		{"clean", "", 0, nil, "x", protocol.RunOutcome{OK: true, FinalText: "x"}, false},
		{"success outranks exit", "", 1, str("success"), "x", protocol.RunOutcome{OK: true, FinalText: "x"}, true},
		{"success without text", "", 1, str("success"), "", protocol.RunOutcome{Reason: "harness_exit_1"}, true},
		{"model-level ending", "", 1, str("error_max_turns"), "", protocol.RunOutcome{Reason: "error_max_turns"}, true},
		{"empty subtype", "", 2, str(""), "", protocol.RunOutcome{Reason: "harness_exit_2"}, true},
		{"no result", "", -1, nil, "", protocol.RunOutcome{Reason: "harness_exit_null"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logs := &logRec{}
			st := &claudeStream{finalText: c.finalText, resultSubtype: c.subtype}
			if got := st.outcome(c.killed, c.code, "err", logs.log); got != c.want {
				t.Fatalf("outcome = %+v", got)
			}
			extra, ok := logs.find("claude exited non-zero")
			if ok != c.logged {
				t.Fatalf("logged = %v", ok)
			}
			if ok && extra["code"] != exitCodeValue(c.code) {
				t.Fatalf("log = %v", extra)
			}
		})
	}
}

func TestCodexStreamLegacyAndEdgeShapes(t *testing.T) {
	sink := &recSink{}
	logs := &logRec{}
	st := &codexStream{sink: sink, log: logs.log} // nil OnSessionID is fine
	feed(st.handle,
		`{"type":"thread.started","thread_id":"th-1"}`,
		`{"type":"thread.started"}`,
		`{"type":"item.started","item":{"type":"agent_message","text":"ignored"}}`,
		`{"type":"item.completed"}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":""}}`,
		`{"type":"item.completed","item":{"type":"mcp_tool_call","tool":"t","server":"","status":"failed","error":{"message":""}}}`,
		`{"type":"item.completed","item":{"type":"mcp_tool_call","tool":"t","status":"failed","result":{"content":[{"type":"text","text":"partial"}]}}}`,
		`{"type":"item.completed","item":{"type":"mcp_tool_call","tool":"t","status":"completed","result":{"content":[]}}}`,
		`{"type":"item.completed","item":{"type":"command_execution","command":"true","aggregated_output":"","exit_code":0}}`,
		`{"type":"item.completed","item":{"type":"command_execution","command":"echo hi","aggregated_output":"hi\n"}}`,
		`{"type":"item.completed","item":{"type":"command_execution","command":"x","exit_code":"1","aggregated_output":"out"}}`,
		`{"type":"turn.completed"}`,
		`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":40,"output_tokens":2}}`,
		`{"type":"error","error":"just a string","message":"top-level"}`,
		`{"id":"0","msg":{}}`,
		`{"id":"0","msg":{"type":"task_started"}}`,
		`{"id":"0","msg":{"type":"agent_message","message":""}}`,
		`{"id":"0","msg":{"type":"agent_message","message":"legacy hello"}}`,
		`{"id":"0","msg":{"type":"task_complete"}}`,
		`{"id":"0","msg":{"type":"task_complete","last_agent_message":"Legacy final."}}`,
		`not json`,
	)
	assertEvents(t, sink.list(), []event{
		{Kind: "tool", Name: "mcp____t", Detail: "use mcp____t `{}`", Input: map[string]any{}},
		{Kind: "tool_result", Name: "mcp____t", Detail: "ERROR: "},
		{Kind: "tool", Name: "mcp__ex__t", Detail: "use mcp__ex__t `{}`", Input: map[string]any{}},
		{Kind: "tool_result", Name: "mcp__ex__t", Detail: "ERROR: partial (7B)"},
		{Kind: "tool", Name: "mcp__ex__t", Detail: "use mcp__ex__t `{}`", Input: map[string]any{}},
		{Kind: "tool", Name: "shell", Detail: "run `true`", Input: map[string]any{"command": "true"}},
		{Kind: "tool", Name: "shell", Detail: "run `echo hi`", Input: map[string]any{"command": "echo hi"}},
		{Kind: "tool_result", Name: "shell", Detail: "hi (2B)"},
		{Kind: "tool", Name: "shell", Detail: "run `x`", Input: map[string]any{"command": "x"}},
		{Kind: "tool_result", Name: "shell", Detail: "out (3B)"}, // a non-numeric exit_code is no exit code
		{Kind: "usage", In: 0, Out: 2},
		{Kind: "tool_result", Name: "codex", Detail: "ERROR: top-level"},
		{Kind: "turn"},
		{Kind: "progress", Text: "legacy hello"},
	})
	if st.finalText != "Legacy final." || st.usage != (protocol.Usage{InputTokens: 0, OutputTokens: 2}) {
		t.Fatalf("state = %q %+v", st.finalText, st.usage)
	}
	if extra, ok := logs.find("codex error event"); !ok || extra["message"] != "top-level" {
		t.Fatalf("log = %v", logs.msgs())
	}
}
