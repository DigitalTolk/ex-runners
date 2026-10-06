// Package describetool renders a harness tool call as one human-readable
// line for the chat notice, approval card and run timeline. Raw JSON of the
// tool input was unreadable in chat (escaped quotes, embedded \n, 300-char
// blobs) — the invoker needs the gist, not the wire format.
package describetool

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/DigitalTolk/ex-runners/internal/textutil"
)

// scratchPath collapses long absolute run-scratch paths so the interesting
// part of a command (the grep pattern, the file name) survives clipping:
// "/Users/…/Application Support/…/threads/<id>/connectors/x" → "…/connectors/x".
// Path segments may contain spaces ("Application Support"), so it matches
// lazily up to the anchor directory instead of stopping at whitespace.
var scratchPath = regexp.MustCompile("/[^'\"`]*?/(connectors|agent-runner)/")

func shortenPaths(s string) string { return scratchPath.ReplaceAllString(s, "…/$1/") }

// DescribeToolUse renders one tool call as a line like "run `ls` — list files".
func DescribeToolUse(toolName string, input map[string]any) string {
	str := func(k string) string { return textutil.Str(input, k) }
	inline := func(s string, n int) string {
		return textutil.Clip(shortenPaths(textutil.CollapseSpace(s)), n)
	}
	switch toolName {
	case "Bash":
		desc := inline(str("description"), 120)
		cmd := inline(str("command"), 220)
		if desc != "" {
			return fmt.Sprintf("run `%s` — %s", cmd, desc)
		}
		return fmt.Sprintf("run `%s`", cmd)
	case "Write":
		return fmt.Sprintf("write `%s` (%s)", str("file_path"), textutil.SizeLabel(textutil.Len(str("content"))))
	case "Edit", "MultiEdit":
		return fmt.Sprintf("edit `%s`", str("file_path"))
	case "NotebookEdit":
		return fmt.Sprintf("edit notebook `%s`", str("notebook_path"))
	case "Read":
		return fmt.Sprintf("read `%s`", shortenPaths(str("file_path")))
	case "Glob", "Grep":
		return fmt.Sprintf("search files for `%s`", inline(str("pattern"), 120))
	case "WebFetch":
		return "fetch " + inline(str("url"), 200)
	case "WebSearch":
		return fmt.Sprintf("search the web for “%s”", inline(str("query"), 120))
	// Connector tools — say WHICH API is being called, not just "a tool ran".
	case "mcp__ex__connector_call", "connector_call":
		method := strings.ToUpper(str("method"))
		if method == "" {
			method = "GET"
		}
		q := ""
		if query, ok := input["query"].(map[string]any); ok && len(query) > 0 {
			keys := make([]string, 0, len(query))
			for k := range query {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, k+"="+textutil.JSString(query[k]))
			}
			q = "?" + strings.Join(parts, "&")
		}
		return fmt.Sprintf("%s API: %s %s%s", str("connector"), method, str("path"), inline(q, 80))
	case "mcp__ex__use_connector", "use_connector":
		return fmt.Sprintf("attach connector %s — %s", str("connector"), inline(str("reason"), 140))
	case "mcp__ex__connector_lookup", "connector_lookup":
		what := fmt.Sprintf("“%s”", inline(str("query"), 100))
		if str("route_id") != "" {
			what = "contract " + str("route_id")
		}
		scope := ""
		if str("service") != "" {
			scope = " in " + str("service")
		}
		return fmt.Sprintf("%s docs: look up %s%s", str("connector"), what, scope)
	case "mcp__ex__fetch_spill", "fetch_spill":
		return "read more of spilled result " + str("locator")
	case "mcp__ex__invoke_skill", "invoke_skill":
		id := str("id")
		if id == "" {
			id = str("skill_id")
		}
		return "use skill " + id
	// Coding-task tools.
	case "mcp__ex__create_coding_task", "create_coding_task":
		return fmt.Sprintf("open coding task in %s: %s", str("project"), inline(str("title"), 100))
	case "mcp__ex__publish_test_plan", "publish_test_plan":
		steps := 0
		if s, ok := input["steps"].([]any); ok {
			steps = len(s)
		}
		url := str("url")
		if url == "" {
			url = "no UI URL"
		}
		plural := "s"
		if steps == 1 {
			plural = ""
		}
		return fmt.Sprintf("publish test plan — %s, %d step%s", url, steps, plural)
	case "mcp__ex__request_mr", "request_mr":
		return "request the merge request (push + MR after sign-off)"
	case "mcp__ex__task_state", "task_state":
		state := str("state")
		if state == "" {
			state = "note"
		}
		return fmt.Sprintf("task %s: %s", state, inline(str("note"), 120))
	case "mcp__ex__register_project_commands", "register_project_commands":
		return "remember project commands in the workspace registry"
	default:
		detail := "(unserializable input)"
		if b, err := json.Marshal(input); err == nil {
			detail = string(b)
		}
		return fmt.Sprintf("use %s `%s`", toolName, inline(detail, 200))
	}
}
