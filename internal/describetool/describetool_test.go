package describetool

import (
	"regexp"
	"strings"
	"testing"
)

func TestBashDescriptionAndCommandNoJSON(t *testing.T) {
	out := DescribeToolUse("Bash", map[string]any{
		"command":     `lsof -nP -iTCP:8072 -sTCP:LISTEN 2>/dev/null || echo "port 8072 free"`,
		"description": "List working dir and check port 8072",
	})
	for _, want := range []string{"List working dir and check port 8072", "`lsof -nP"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, `{"command"`) || strings.Contains(out, `\"`) {
		t.Errorf("raw JSON leaked: %q", out)
	}
}

func TestWritePathAndSizeNeverContent(t *testing.T) {
	out := DescribeToolUse("Write", map[string]any{
		"file_path": "/tmp/site/index.html",
		"content":   strings.Repeat("<html lang=\"en\">\n<meta charset=\"utf-8\">\n", 40),
	})
	if !strings.Contains(out, "write `/tmp/site/index.html`") || !regexp.MustCompile(`\(\d+(\.\d+)?(B|KB)\)`).MatchString(out) {
		t.Errorf("Write = %q", out)
	}
	if strings.Contains(out, "<html") || strings.Contains(out, `\n`) {
		t.Errorf("content leaked: %q", out)
	}
}

func TestCollapsesAndClipsLongCommands(t *testing.T) {
	out := DescribeToolUse("Bash", map[string]any{"command": "echo a\n" + strings.Repeat("x", 500)})
	if !strings.Contains(out, "echo a x") || !strings.Contains(out, "…") {
		t.Errorf("Bash = %q", out)
	}
	if n := len([]rune(out)); n >= 240 {
		t.Errorf("not bounded: %d chars", n)
	}
}

func TestShortensRunScratchPaths(t *testing.T) {
	out := DescribeToolUse("Bash", map[string]any{
		"command": "grep -i 'leave' '/Users/someone/Library/Application Support/ex-dev/agent-runner/threads/abc123/connectors/cliffhub/_catalog.tsv'",
	})
	if !strings.Contains(out, "…/connectors/cliffhub/_catalog.tsv") || strings.Contains(out, "Application Support") {
		t.Errorf("Bash = %q", out)
	}
}

func TestFallbacks(t *testing.T) {
	if got := DescribeToolUse("SomeTool", map[string]any{"alpha": 1, "beta": "two"}); got != "use SomeTool `{\"alpha\":1,\"beta\":\"two\"}`" {
		t.Errorf("fallback = %q", got)
	}
	if got := DescribeToolUse("SomeTool", map[string]any{"ch": make(chan int)}); got != "use SomeTool `(unserializable input)`" {
		t.Errorf("unserializable = %q", got)
	}
}

func TestOneLineGists(t *testing.T) {
	cases := []struct {
		tool  string
		input map[string]any
		want  string
	}{
		{"Bash", map[string]any{"command": "ls"}, "run `ls`"},
		{"Edit", map[string]any{"file_path": "/a/b.ts"}, "edit `/a/b.ts`"},
		{"MultiEdit", map[string]any{"file_path": "/a/b.ts"}, "edit `/a/b.ts`"},
		{"NotebookEdit", map[string]any{"notebook_path": "/n.ipynb"}, "edit notebook `/n.ipynb`"},
		{"Read", map[string]any{"file_path": "/x/agent-runner/threads/t/f"}, "read `…/agent-runner/threads/t/f`"},
		{"Glob", map[string]any{"pattern": "**/*.ts"}, "search files for `**/*.ts`"},
		{"Grep", map[string]any{"pattern": "getYear"}, "search files for `getYear`"},
		{"WebFetch", map[string]any{"url": "https://ex.example/doc"}, "fetch https://ex.example/doc"},
		{"WebSearch", map[string]any{"query": "leap year rules"}, "search the web for “leap year rules”"},
		{"mcp__ex__use_connector", map[string]any{"connector": "hub", "reason": "r"}, "attach connector hub — r"},
		{"mcp__ex__fetch_spill", map[string]any{"locator": "sp-9"}, "read more of spilled result sp-9"},
		{"mcp__ex__create_coding_task", map[string]any{"project": "P", "title": "t"}, "open coding task in P: t"},
		{"mcp__ex__request_mr", map[string]any{}, "request the merge request (push + MR after sign-off)"},
		{"mcp__ex__register_project_commands", map[string]any{}, "remember project commands in the workspace registry"},
		{"Write", map[string]any{"file_path": "/a", "content": "hi"}, "write `/a` (2B)"},
		{"mcp__ex__connector_call", map[string]any{"connector": "hub", "method": "post", "path": "api/people", "query": map[string]any{"per_page": float64(5), "q": "x"}}, "hub API: POST api/people?per_page=5&q=x"},
		{"connector_call", map[string]any{"connector": "hub", "path": "api/people", "query": map[string]any{}}, "hub API: GET api/people"},
		{"connector_call", map[string]any{"connector": "hub", "path": "api/people", "query": "nope"}, "hub API: GET api/people"},
		{"connector_call", map[string]any{"connector": "hub", "path": "api/people"}, "hub API: GET api/people"},
		{"use_connector", map[string]any{"connector": "hub", "reason": "people data"}, "attach connector hub — people data"},
		{"mcp__ex__connector_lookup", map[string]any{"connector": "hub", "route_id": "people.list"}, "hub docs: look up contract people.list"},
		{"connector_lookup", map[string]any{"connector": "hub", "query": "leads", "service": "crm"}, "hub docs: look up “leads” in crm"},
		{"fetch_spill", map[string]any{"locator": "sp-2"}, "read more of spilled result sp-2"},
		{"invoke_skill", map[string]any{"skill_id": "sk-1"}, "use skill sk-1"},
		{"mcp__ex__invoke_skill", map[string]any{"id": "sk-2"}, "use skill sk-2"},
		{"create_coding_task", map[string]any{"project": "CliffHub", "title": "fix login"}, "open coding task in CliffHub: fix login"},
		{"publish_test_plan", map[string]any{"url": "http://localhost:5273", "steps": []any{1, 2}}, "publish test plan — http://localhost:5273, 2 steps"},
		{"mcp__ex__publish_test_plan", map[string]any{"steps": []any{1}}, "publish test plan — no UI URL, 1 step"},
		{"publish_test_plan", map[string]any{}, "publish test plan — no UI URL, 0 steps"},
		{"request_mr", map[string]any{}, "request the merge request (push + MR after sign-off)"},
		{"task_state", map[string]any{"state": "blocked", "note": "waiting on API"}, "task blocked: waiting on API"},
		{"mcp__ex__task_state", map[string]any{}, "task note: "},
		{"register_project_commands", map[string]any{}, "remember project commands in the workspace registry"},
	}
	for _, c := range cases {
		if got := DescribeToolUse(c.tool, c.input); got != c.want {
			t.Errorf("%s(%v) = %q, want %q", c.tool, c.input, got, c.want)
		}
	}
}
