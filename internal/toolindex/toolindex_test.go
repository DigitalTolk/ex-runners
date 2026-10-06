package toolindex

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func def(name, description string, required ...string) ToolDef {
	req, _ := json.Marshal(append([]string{}, required...))
	return ToolDef{
		Name:        name,
		Description: description,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"},"y":{"type":"number"}},"required":` + string(req) + `}`),
	}
}

var visible = []ToolDef{
	def("post_message", "Post into the thread. Long details follow."),
	def("get_thread", "Read the thread."),
	def("list_channels", "List the channels your invoker is in. They can read them."),
	def("set_reminder", "Set a reminder for YOUR INVOKER — it fires later", "x"),
}

func names(tools []ToolDef) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}

func TestIsCore(t *testing.T) {
	for _, n := range []string{"post_message", "get_thread", "get_context", "set_state", "connector_lookup", "connector_call", "use_connector", "fetch_spill", "request_approval"} {
		if !IsCore(n) {
			t.Errorf("%s should be core", n)
		}
	}
	for _, n := range []string{"list_channels", "set_reminder", ToolInfo, ToolCall, ""} {
		if IsCore(n) {
			t.Errorf("%s should not be core", n)
		}
	}
}

func TestSummaryKeepsFirstSentenceAndCapsLongOnes(t *testing.T) {
	cases := [][2]string{
		{"Read  the\nthread. Then more.", "Read the thread."},
		{"No full stop here", "No full stop here"},
		{"v1.2 stays whole. Next.", "v1.2 stays whole."},
		// Golden values from the TS implementation: JS \s covers \v, NBSP and
		// BOM (Go's \s does not); NEL (U+0085) is NOT JS whitespace.
		{"a\vb\u00a0c\ufeff.  x", "a b c ."},
		{"\u0085lead. x", "\u0085lead."},
		{"Ends with ?", "Ends with ?"},
		{"Dot.Inside. ok", "Dot.Inside."},
		{"  ", ""},
		{"Wow! Then", "Wow!"},
		{"q?\u2028next", "q?"},
	}
	for _, c := range cases {
		if got := Summary(c[0]); got != c[1] {
			t.Errorf("Summary(%q) = %q, want %q", c[0], got, c[1])
		}
	}
	long := Summary(strings.Repeat("x", 200))
	if n := len([]rune(long)); n != 108 || !strings.HasSuffix(long, "…") {
		t.Errorf("long summary = %q (%d chars)", long, n)
	}
	// Character-based cap: multi-byte text is cut at 107 characters.
	if got := Summary(strings.Repeat("é", 120)); got != strings.Repeat("é", 107)+"…" {
		t.Errorf("multibyte cap = %q", got)
	}
	if got := Summary(strings.Repeat("é", 110)); got != strings.Repeat("é", 110) {
		t.Errorf("exactly 110 chars must not be capped: %q", got)
	}
}

func TestSignatureListsArgumentsMarkingOptionalOnes(t *testing.T) {
	if got := Signature(visible[3]); got != "set_reminder(x, y?)" {
		t.Errorf("got %q", got)
	}
	if got := Signature(ToolDef{Name: "bare", InputSchema: json.RawMessage(`{}`)}); got != "bare()" {
		t.Errorf("got %q", got)
	}
}

func TestSignatureKeepsSourceOrderLikeJSONParse(t *testing.T) {
	cases := []struct{ schema, want string }{
		// Source order, not sorted.
		{`{"properties":{"zeta":{},"alpha":{},"mid":{}},"required":["alpha"]}`, "t(zeta?, alpha, mid?)"},
		// Golden from TS: a repeated property keeps its first position.
		{`{"required":["b","zz"],"properties":{"b":{},"a":{},"b":{"type":"x"}},"properties2":1}`, "t(b, a?)"},
		// Golden from TS: a repeated top-level key — last one wins.
		{`{"properties":{"a":{}},"properties":{"c":{},"d":{}},"required":["c"],"required":["d"]}`, "t(c?, d)"},
		// Pretty-printed input reads the same.
		{"{\n  \"properties\": {\n    \"q\": {\"type\": \"string\"}\n  },\n  \"required\": [\"q\"]\n}", "t(q)"},
		// Non-string required entries never match.
		{`{"properties":{"a":{}},"required":[1,null,"a"]}`, "t(a)"},
		// Shapes that are not a JSON Schema read as "no arguments".
		{``, "t()"},
		{`null`, "t()"},
		{`[]`, "t()"},
		{`{"properties":null,"required":null}`, "t()"},
		{`{"properties":["a"],"required":"a"}`, "t()"},
		{`{"properties":{"a":{}},"required":"a"}`, "t(a?)"},
		// Malformed JSON: no fields.
		{`{1:2}`, "t()"},
		{`{"properties": }`, "t()"},
		{`{"properties":{"a":{}}`, "t()"},
		{`not json`, "t()"},
	}
	for _, c := range cases {
		if got := Signature(ToolDef{Name: "t", InputSchema: json.RawMessage(c.schema)}); got != c.want {
			t.Errorf("Signature(%s) = %q, want %q", c.schema, got, c.want)
		}
	}
}

func TestIndexedToolsAdvertisesCoreAndCataloguesTheRest(t *testing.T) {
	tools := IndexedTools(visible)
	if got := names(tools); !reflect.DeepEqual(got, []string{"post_message", "get_thread", ToolInfo, ToolCall}) {
		t.Fatalf("names = %v", got)
	}
	if !reflect.DeepEqual(tools[0], visible[0]) {
		t.Error("core tool must be passed through unchanged")
	}
	info := tools[2]
	for _, want := range []string{
		"before telling anyone you can't do something",
		"- list_channels(x?, y?): List the channels your invoker is in.",
		"- set_reminder(x, y?): Set a reminder for YOUR INVOKER — it fires later",
	} {
		if !strings.Contains(info.Description, want) {
			t.Errorf("info description missing %q", want)
		}
	}
	if strings.Contains(info.Description, "They can read them") {
		t.Error("catalog must carry only the first sentence")
	}
	if !strings.Contains(tools[3].Description, "AFTER reading its contract") {
		t.Error("call description")
	}
}

func TestIndexedToolsMetaToolsAreByteFaithful(t *testing.T) {
	// Golden values: JSON.stringify of the TS definitions.
	tools := IndexedTools(visible)
	wantInfo := "MORE EX TOOLS — channels, DMs, people, reminders, reactions, pins, scheduled orders, search, skills and more are listed below. Check this list before telling anyone you can't do something. Pass the names you need to get each one's full contract (when to use it, exact arguments), then run it with ex_tool_call.\n- list_channels(x?, y?): List the channels your invoker is in.\n- set_reminder(x, y?): Set a reminder for YOUR INVOKER — it fires later"
	if tools[2].Description != wantInfo {
		t.Errorf("info description = %q", tools[2].Description)
	}
	if got := string(tools[2].InputSchema); got != `{"type":"object","properties":{"names":{"type":"array","items":{"type":"string"},"description":"Tool names from the list."}},"required":["names"],"additionalProperties":false}` {
		t.Errorf("info schema = %s", got)
	}
	wantCall := "Run a tool from the ex_tool_info list, AFTER reading its contract with ex_tool_info. A tool whose contract you have not read, or a call missing required arguments, is not run — you get the contract back."
	if tools[3].Description != wantCall {
		t.Errorf("call description = %q", tools[3].Description)
	}
	if got := string(tools[3].InputSchema); got != `{"type":"object","properties":{"tool":{"type":"string","description":"Tool name from the ex_tool_info list."},"arguments":{"type":"object","description":"The tool's arguments, per its contract."}},"required":["tool"],"additionalProperties":false}` {
		t.Errorf("call schema = %s", got)
	}
	// The whole definition marshals as the MCP wire shape, schema verbatim.
	b, err := json.Marshal(tools[3])
	if err != nil || !strings.Contains(string(b), `"inputSchema":{"type":"object","properties":{"tool"`) {
		t.Errorf("marshal = %s %v", b, err)
	}
}

func TestIndexedToolsAddsNoMetaToolsWhenEveryVisibleToolIsCore(t *testing.T) {
	var coreOnly []ToolDef
	for _, d := range visible {
		if IsCore(d.Name) {
			coreOnly = append(coreOnly, d)
		}
	}
	if got := IndexedTools(coreOnly); !reflect.DeepEqual(got, coreOnly) {
		t.Errorf("got %v", names(got))
	}
	// Nothing visible: an empty list, never nil (tools/list marshals as []).
	got := IndexedTools(nil)
	if got == nil || len(got) != 0 {
		t.Errorf("empty = %#v", got)
	}
	if b, _ := json.Marshal(got); string(b) != "[]" {
		t.Errorf("marshal = %s", b)
	}
}

func TestInfoReturnsContractsRecordsThemAndNamesUnavailable(t *testing.T) {
	described := map[string]bool{}
	out := Info(visible, []any{"list_channels", "nope", float64(7)}, described)
	for _, want := range []string{
		"## list_channels\nList the channels your invoker is in. They can read them.",
		`Arguments (JSON schema): {"type":"object"`,
		"## nope\n(no such tool available to this run)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	// Golden from TS.
	want := "## list_channels\nList the channels your invoker is in. They can read them.\nArguments (JSON schema): {\"type\":\"object\",\"properties\":{\"x\":{\"type\":\"string\"},\"y\":{\"type\":\"number\"}},\"required\":[]}\n\n## nope\n(no such tool available to this run)"
	if out != want {
		t.Errorf("out = %q", out)
	}
	if !reflect.DeepEqual(described, map[string]bool{"list_channels": true}) {
		t.Errorf("described = %v", described)
	}
}

func TestInfoRequiresNames(t *testing.T) {
	for _, n := range []any{[]any{}, "list_channels", nil, []any{float64(1), true}, map[string]any{"names": "x"}, []string{}} {
		if got := Info(visible, n, nil); got != "ex_tool_info requires names" {
			t.Errorf("Info(%#v) = %q", n, got)
		}
	}
}

func TestInfoGoCallersAndNilDescribed(t *testing.T) {
	// []string is accepted; a nil described set records nothing (and must
	// not panic).
	out := Info(visible, []string{"set_reminder"}, nil)
	if !strings.HasPrefix(out, "## set_reminder\nSet a reminder") {
		t.Errorf("out = %q", out)
	}
}

func TestInfoCompactsServedSchemas(t *testing.T) {
	tools := []ToolDef{
		{Name: "pretty", Description: "P.", InputSchema: json.RawMessage("{\n  \"type\": \"object\",\n  \"properties\": {\"b\": {}, \"a\": {}}\n}")},
		{Name: "none", Description: "N."},
		{Name: "junk", Description: "J.", InputSchema: json.RawMessage(`{oops`)},
	}
	out := Info(tools, []any{"pretty", "none", "junk"}, nil)
	for _, want := range []string{
		`Arguments (JSON schema): {"type":"object","properties":{"b":{},"a":{}}}`,
		"## none\nN.\nArguments (JSON schema): {}",
		"## junk\nJ.\nArguments (JSON schema): {oops",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
}

func TestUnwrapCallHandsContractBackUntilRead(t *testing.T) {
	described := map[string]bool{}
	first := UnwrapCall(visible, map[string]any{"tool": "set_reminder", "arguments": map[string]any{}}, described)
	if first.Name != "" || first.Error != "" {
		t.Fatalf("first = %+v", first)
	}
	for _, want := range []string{"Not run: read set_reminder's contract first", "## set_reminder"} {
		if !strings.Contains(first.Contract, want) {
			t.Errorf("contract missing %q", want)
		}
	}
	// Golden from TS.
	if want := "Not run: read set_reminder's contract first, then call it again.\n\n## set_reminder\nSet a reminder for YOUR INVOKER — it fires later\nArguments (JSON schema): {\"type\":\"object\",\"properties\":{\"x\":{\"type\":\"string\"},\"y\":{\"type\":\"number\"}},\"required\":[\"x\"]}"; first.Contract != want {
		t.Errorf("contract = %q", first.Contract)
	}
	// …which counts as reading it: the next call runs.
	if got := UnwrapCall(visible, map[string]any{"tool": "set_reminder", "arguments": map[string]any{"x": "a"}}, described); !reflect.DeepEqual(got, Unwrapped{Name: "set_reminder", Args: map[string]any{"x": "a"}}) {
		t.Errorf("second = %+v", got)
	}
	// `name` works as well as `tool`.
	if got := UnwrapCall(visible, map[string]any{"name": "set_reminder", "arguments": map[string]any{"x": "b"}}, described); !reflect.DeepEqual(got, Unwrapped{Name: "set_reminder", Args: map[string]any{"x": "b"}}) {
		t.Errorf("by name = %+v", got)
	}
}

func TestUnwrapCallRefusesMissingRequiredArguments(t *testing.T) {
	described := map[string]bool{"set_reminder": true}
	out := UnwrapCall(visible, map[string]any{"tool": "set_reminder", "arguments": map[string]any{"y": float64(1)}}, described)
	if !strings.Contains(out.Contract, "Not run: missing required arguments x.") || out.Name != "" || out.Error != "" {
		t.Errorf("out = %+v", out)
	}
	// Golden from TS.
	if want := "Not run: missing required arguments x.\n\n## set_reminder\nSet a reminder for YOUR INVOKER — it fires later\nArguments (JSON schema): {\"type\":\"object\",\"properties\":{\"x\":{\"type\":\"string\"},\"y\":{\"type\":\"number\"}},\"required\":[\"x\"]}"; out.Contract != want {
		t.Errorf("contract = %q", out.Contract)
	}
	// Several missing, in `required` order; a present null counts as present.
	multi := ToolDef{Name: "multi", Description: "M.", InputSchema: json.RawMessage(`{"properties":{"a":{},"b":{},"c":{}},"required":["c","a","b"]}`)}
	got := UnwrapCall([]ToolDef{multi}, map[string]any{"tool": "multi", "arguments": map[string]any{"b": nil}}, map[string]bool{"multi": true})
	if !strings.HasPrefix(got.Contract, "Not run: missing required arguments c, a.\n\n## multi\n") {
		t.Errorf("multi = %q", got.Contract)
	}
}

func TestUnwrapCallRunsNormalisingAbsentOrMalformedArguments(t *testing.T) {
	described := map[string]bool{"list_channels": true}
	want := Unwrapped{Name: "list_channels", Args: map[string]any{}}
	for _, args := range []map[string]any{
		{"tool": "list_channels"},
		{"tool": "list_channels", "arguments": []any{"bad"}},
		{"tool": "list_channels", "arguments": nil},
		{"tool": "list_channels", "arguments": "str"},
		{"tool": "list_channels", "arguments": map[string]any(nil)},
	} {
		if got := UnwrapCall(visible, args, described); !reflect.DeepEqual(got, want) {
			t.Errorf("UnwrapCall(%v) = %+v", args, got)
		}
	}
	// A schema with no required list at all.
	bare := ToolDef{Name: "bare", Description: "Bare.", InputSchema: json.RawMessage(`{}`)}
	if got := UnwrapCall([]ToolDef{bare}, map[string]any{"tool": "bare"}, map[string]bool{"bare": true}); !reflect.DeepEqual(got, Unwrapped{Name: "bare", Args: map[string]any{}}) {
		t.Errorf("bare = %+v", got)
	}
}

func TestUnwrapCallRefusesMissingNameMetaToolsAndUnavailable(t *testing.T) {
	described := map[string]bool{}
	if got := UnwrapCall(visible, map[string]any{}, described); !reflect.DeepEqual(got, Unwrapped{Error: "ex_tool_call requires tool"}) {
		t.Errorf("empty = %+v", got)
	}
	for _, name := range []string{ToolCall, ToolInfo, "send_dm"} {
		want := Unwrapped{Error: "no such tool available to this run: " + name}
		if got := UnwrapCall(visible, map[string]any{"tool": name}, described); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %+v", name, got)
		}
	}
	// Meta-tools are refused even when they appear in the visible list.
	withMeta := append(append([]ToolDef{}, visible...), ToolDef{Name: ToolInfo})
	if got := UnwrapCall(withMeta, map[string]any{"tool": ToolInfo}, described); got.Error != "no such tool available to this run: ex_tool_info" {
		t.Errorf("meta = %+v", got)
	}
	// A string `tool` wins over `name`, even when empty; a non-string
	// `tool` falls back to `name`.
	if got := UnwrapCall(visible, map[string]any{"tool": "", "name": "get_thread"}, described); got.Error != "ex_tool_call requires tool" {
		t.Errorf("empty tool = %+v", got)
	}
	if got := UnwrapCall(visible, map[string]any{"tool": float64(3), "name": "send_dm"}, described); got.Error != "no such tool available to this run: send_dm" {
		t.Errorf("non-string tool = %+v", got)
	}
	if got := UnwrapCall(visible, map[string]any{"name": float64(3)}, described); got.Error != "ex_tool_call requires tool" {
		t.Errorf("non-string name = %+v", got)
	}
	// Nil args and a nil described set are tolerated.
	if got := UnwrapCall(visible, nil, nil); got.Error != "ex_tool_call requires tool" {
		t.Errorf("nil = %+v", got)
	}
	if got := UnwrapCall(visible, map[string]any{"tool": "get_thread"}, nil); !strings.HasPrefix(got.Contract, "Not run: read get_thread's contract first") {
		t.Errorf("nil described = %+v", got)
	}
}
