package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/DigitalTolk/ex-runners/internal/describetool"
	"github.com/DigitalTolk/ex-runners/internal/spill"
	"github.com/DigitalTolk/ex-runners/internal/taskpolicy"
)

func str(args map[string]any, k string) string {
	s, _ := args[k].(string)
	return s
}

func num(args map[string]any, k string) (float64, bool) {
	n, ok := args[k].(float64)
	return n, ok
}

// textOr returns data.text when it's a string, else fallback.
func textOr(data map[string]any, fallback string) string {
	if t, ok := data["text"].(string); ok {
		return t
	}
	return fallback
}

// nonEmptyText is textOr that also treats "" as missing.
func nonEmptyText(data map[string]any, fallback string) string {
	if t := textOr(data, ""); t != "" {
		return t
	}
	return fallback
}

func remaining(data map[string]any) string {
	if n, ok := data["remainingPosts"].(float64); ok {
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	return "?"
}

func jsNum(n float64) string { return strconv.FormatFloat(n, 'f', -1, 64) }

var blankRuns = regexp.MustCompile(`\n{3,}`)

func limitArg(args map[string]any, max, def float64) string {
	if n, ok := num(args, "limit"); ok {
		if n > max {
			n = max
		}
		return jsNum(n)
	}
	return jsNum(def)
}

func threadQuery(args map[string]any) string {
	if t := str(args, "thread"); t != "" {
		return "&thread=" + url.QueryEscape(t)
	}
	return ""
}

// call dispatches one tool call.
func (s *Server) call(name string, args map[string]any) toolResult {
	switch name {
	case "post_message":
		// Models pad with runs of blank lines; in chat that renders as big
		// empty gaps. Collapse to one blank line — paragraph breaks survive.
		body := strings.TrimSpace(blankRuns.ReplaceAllString(str(args, "body"), "\n\n"))
		if body == "" {
			return fail("post_message requires a non-empty body")
		}
		s.mu.Lock()
		s.postSeq++
		key := fmt.Sprintf("mcp-%d", s.postSeq)
		s.mu.Unlock()
		// Idempotency: a network retry of the same logical post must not
		// double-post (plan-v2 §7). Sequence-scoped to this process = run.
		res := s.callBackend(http.MethodPost, "/api/v1/agent/run/messages", map[string]any{"body": body, "idempotencyKey": key})
		if !res.ok {
			return s.failure(res)
		}
		return ok(fmt.Sprintf("posted (messageID=%v, remaining posts: %s)", res.data["messageID"], remaining(res.data)))
	case "get_thread", "get_context":
		path, empty := "/api/v1/agent/run/thread", "(thread is empty)"
		if name == "get_context" {
			path, empty = "/api/v1/agent/run/context", "(context is empty)"
		}
		res := s.callBackend(http.MethodGet, path, nil)
		if !res.ok {
			return s.failure(res)
		}
		return ok(nonEmptyText(res.data, empty))
	case "write_shared_context":
		body := str(args, "body")
		if strings.TrimSpace(body) == "" {
			return fail("write_shared_context requires a non-empty body")
		}
		res := s.callBackend(http.MethodPost, "/api/v1/agent/run/context", map[string]any{"body": body, "pinned": args["pinned"] == true})
		if !res.ok {
			return s.failure(res)
		}
		return ok(fmt.Sprintf("stored (itemID=%v)", res.data["itemID"]))
	case "request_approval":
		summary := str(args, "summary")
		if strings.TrimSpace(summary) == "" {
			return fail("request_approval requires a summary")
		}
		v := s.awaitApproval(summary, str(args, "risk"), nil, "")
		switch {
		case v.err != "":
			return fail(v.err)
		case v.state == "approved":
			if v.note != "" {
				return ok("approved — proceed with the action. The invoker adds: " + v.note)
			}
			return ok("approved — proceed with the action")
		case v.state == "denied":
			if v.note != "" {
				return fail(fmt.Sprintf("denied by the invoker — do NOT take the action. They say: %s — follow that instead [retryable=false]", v.note))
			}
			return fail("denied by the invoker — do NOT take the action; explain and wind down [retryable=false]")
		}
		return fail("denied (approval_timeout) — nobody decided in time; do NOT take the action [retryable=false]")
	case "ask_user":
		question := str(args, "question")
		var options []string
		if list, isList := args["options"].([]any); isList {
			for _, o := range list {
				if s, isStr := o.(string); isStr {
					options = append(options, s)
				}
			}
		}
		if strings.TrimSpace(question) == "" || len(options) < 2 {
			return fail("ask_user requires a question and 2–5 options")
		}
		v := s.awaitApproval(question, "", options, "")
		switch {
		case v.err != "":
			return fail(v.err)
		case v.state == "approved" && v.choice != "":
			if v.note != "" {
				return ok(fmt.Sprintf("the invoker chose: %s — and adds: %s", v.choice, v.note))
			}
			return ok("the invoker chose: " + v.choice)
		case v.state == "denied":
			if v.note != "" {
				return fail("the invoker answered in their own words instead: " + v.note)
			}
			return fail("the invoker dismissed the question — decide sensibly yourself and say which assumption you made")
		}
		return fail("no answer in time — decide sensibly yourself and say which assumption you made")
	case "approval_prompt":
		return s.approvalPrompt(args)
	case "list_channels":
		res := s.callBackend(http.MethodGet, "/api/v1/agent/run/channels", nil)
		if !res.ok {
			return s.failure(res)
		}
		return ok(textOr(res.data, ""))
	case "create_channel":
		chName := str(args, "name")
		if strings.TrimSpace(chName) == "" {
			return fail("create_channel requires a name")
		}
		res := s.callBackend(http.MethodPost, "/api/v1/agent/run/channels", map[string]any{
			"name": chName, "description": str(args, "description"), "private": args["private"] == true,
		})
		if !res.ok {
			return s.failure(res)
		}
		return ok(fmt.Sprintf("created ~%s (channelID=%v)", chName, res.data["channelID"]))
	case "join_channel":
		channelID := str(args, "channelID")
		if channelID == "" {
			return fail("join_channel requires channelID")
		}
		res := s.callBackend(http.MethodPost, "/api/v1/agent/run/channels/"+url.PathEscape(channelID)+"/join", nil)
		if !res.ok {
			return s.failure(res)
		}
		return ok("joined")
	case "read_channel":
		channelID := str(args, "channelID")
		if channelID == "" {
			return fail("read_channel requires channelID")
		}
		res := s.callBackend(http.MethodGet, "/api/v1/agent/run/channels/"+url.PathEscape(channelID)+"/messages?limit="+limitArg(args, 50, 30)+threadQuery(args), nil)
		if !res.ok {
			return s.failure(res)
		}
		return ok(nonEmptyText(res.data, "(no messages)"))
	case "read_pins":
		channelID := str(args, "channelID")
		if channelID == "" {
			return fail("read_pins requires channelID")
		}
		res := s.callBackend(http.MethodGet, "/api/v1/agent/run/channels/"+url.PathEscape(channelID)+"/pins", nil)
		if !res.ok {
			return s.failure(res)
		}
		return ok(nonEmptyText(res.data, "(no pinned messages)"))
	case "read_dm":
		userID := str(args, "userID")
		if userID == "" {
			return fail("read_dm requires userID")
		}
		res := s.callBackend(http.MethodGet, "/api/v1/agent/run/dm/"+url.PathEscape(userID)+"/messages?limit="+limitArg(args, 50, 30)+threadQuery(args), nil)
		if !res.ok {
			return s.failure(res)
		}
		return ok(nonEmptyText(res.data, "(no messages with that user)"))
	case "post_to_channel":
		channelID, body := str(args, "channelID"), str(args, "body")
		if channelID == "" || strings.TrimSpace(body) == "" {
			return fail("post_to_channel requires channelID and body")
		}
		payload := map[string]any{"body": body}
		if root := str(args, "thread_root"); root != "" {
			payload["thread_root"] = root
		}
		res := s.callBackend(http.MethodPost, "/api/v1/agent/run/channels/"+url.PathEscape(channelID)+"/messages", payload)
		if !res.ok {
			return s.failure(res)
		}
		return ok(fmt.Sprintf("posted (messageID=%v, remaining posts: %s)", res.data["messageID"], remaining(res.data)))
	case "search_messages":
		query := str(args, "query")
		if strings.TrimSpace(query) == "" {
			return fail("search_messages requires a query")
		}
		res := s.callBackend(http.MethodGet, "/api/v1/agent/run/search?q="+url.QueryEscape(query)+"&limit="+limitArg(args, 20, 10), nil)
		if !res.ok {
			return s.failure(res)
		}
		return ok(textOr(res.data, "(no results)"))
	case "add_reaction":
		messageID, emoji := str(args, "messageID"), str(args, "emoji")
		if messageID == "" || emoji == "" {
			return fail("add_reaction requires messageID and emoji")
		}
		payload := map[string]any{"messageID": messageID, "emoji": emoji}
		if ch := str(args, "channelID"); ch != "" {
			payload["parentID"] = ch
			payload["parentType"] = "channel"
		}
		res := s.callBackend(http.MethodPost, "/api/v1/agent/run/reactions", payload)
		if !res.ok {
			return s.failure(res)
		}
		return ok("reaction toggled")
	case "list_users":
		res := s.callBackend(http.MethodGet, "/api/v1/agent/run/users?q="+url.QueryEscape(str(args, "query")), nil)
		if !res.ok {
			return s.failure(res)
		}
		return ok(textOr(res.data, "(no matching users)"))
	case "reconnect_connector", "disconnect_connector":
		connector := strings.TrimSpace(str(args, "connector"))
		if connector == "" {
			return fail(name + " requires connector")
		}
		action := "disconnect"
		if name == "reconnect_connector" {
			action = "reconnect"
		}
		res := s.callBackend(http.MethodPost, "/api/v1/agent/run/connectors/"+action, map[string]any{"connector": connector})
		if !res.ok {
			return s.failure(res)
		}
		return ok(textOr(res.data, action+"ed"))
	case "send_dm":
		userID, body := str(args, "userID"), str(args, "body")
		if userID == "" || strings.TrimSpace(body) == "" {
			return fail("send_dm requires userID and body")
		}
		res := s.callBackend(http.MethodPost, "/api/v1/agent/run/dm", map[string]any{"userID": userID, "body": body})
		if !res.ok {
			return s.failure(res)
		}
		return ok(fmt.Sprintf("sent (messageID=%v, remaining posts: %s)", res.data["messageID"], remaining(res.data)))
	case "propose_reply":
		text := str(args, "text")
		if strings.TrimSpace(text) == "" {
			return fail("propose_reply requires text (your drafted reply)")
		}
		payload := map[string]any{"text": text}
		if v := str(args, "thread_root"); v != "" {
			payload["thread_root"] = v
		}
		if v := str(args, "reply_to"); v != "" {
			payload["reply_to"] = v
		}
		res := s.callBackend(http.MethodPost, "/api/v1/agent/run/propose-reply", payload)
		if !res.ok {
			return s.failure(res)
		}
		return ok(textOr(res.data, "reply drafted for approval"))
	case "fetch_spill":
		locator := str(args, "locator")
		if locator == "" {
			return fail("fetch_spill requires locator")
		}
		offset, length := 0, spill.FetchMax
		if n, isNum := num(args, "offset"); isNum {
			offset = int(n)
		}
		if n, isNum := num(args, "length"); isNum {
			length = int(n)
		}
		text, isErr := s.spills.Fetch(locator, offset, length)
		return result(text, isErr)
	case "use_connector":
		return s.useConnector(args)
	case "connector_lookup":
		return s.connectorLookup(args)
	case "connector_call":
		return s.connectorCall(args)
	case "link_message":
		messageID := str(args, "message_id")
		if strings.TrimSpace(messageID) == "" {
			return fail("link_message requires message_id")
		}
		payload := map[string]any{"message_id": messageID}
		for _, k := range []string{"channel_id", "conversation_id", "thread_root"} {
			if v := str(args, k); v != "" {
				payload[k] = v
			}
		}
		res := s.callBackend(http.MethodPost, "/api/v1/agent/run/link-message", payload)
		if !res.ok {
			return s.failure(res)
		}
		if u, isStr := res.data["url"].(string); isStr {
			return ok(u)
		}
		if t, has := res.data["text"]; has && t != nil {
			return ok(fmt.Sprint(t))
		}
		return ok("link built")
	case "notify_owner":
		body := str(args, "body")
		if strings.TrimSpace(body) == "" {
			return fail("notify_owner requires body")
		}
		res := s.callBackend(http.MethodPost, "/api/v1/agent/run/notify", map[string]any{"body": body})
		if !res.ok {
			return s.failure(res)
		}
		return ok(textOr(res.data, "notified your creator"))
	case "set_reminder":
		payload := map[string]any{}
		if n, isNum := num(args, "in_minutes"); isNum {
			payload["in_minutes"] = n
		}
		if v := str(args, "remind_at"); v != "" {
			payload["remind_at"] = v
		}
		if v := str(args, "message_id"); v != "" {
			payload["message_id"] = v
		}
		_, hasIn := payload["in_minutes"]
		_, hasAt := payload["remind_at"]
		if !hasIn && !hasAt {
			return fail("set_reminder requires in_minutes or remind_at")
		}
		res := s.callBackend(http.MethodPost, "/api/v1/agent/run/reminders", payload)
		if !res.ok {
			return s.failure(res)
		}
		return ok(textOr(res.data, "reminder set"))
	case "list_reminders", "list_schedules":
		path, empty := "/api/v1/agent/run/reminders", "(no pending reminders)"
		if name == "list_schedules" {
			path, empty = "/api/v1/agent/run/schedules", "(no scheduled orders)"
		}
		res := s.callBackend(http.MethodGet, path, nil)
		if !res.ok {
			return s.failure(res)
		}
		return ok(textOr(res.data, empty))
	case "cancel_reminder", "delete_schedule":
		key, path, done := "reminder_id", "/api/v1/agent/run/reminders/", "reminder canceled"
		if name == "delete_schedule" {
			key, path, done = "schedule_id", "/api/v1/agent/run/schedules/", "schedule removed"
		}
		id := str(args, key)
		if id == "" {
			return fail(name + " requires " + key)
		}
		res := s.callBackend(http.MethodDelete, path+url.PathEscape(id), nil)
		if !res.ok {
			return s.failure(res)
		}
		return ok(textOr(res.data, done))
	case "create_schedule":
		payload := map[string]any{}
		for _, k := range []string{"instruction", "schedule", "timezone", "destination", "agent"} {
			if v := str(args, k); v != "" {
				payload[k] = v
			}
		}
		if payload["instruction"] == nil || payload["schedule"] == nil {
			return fail("create_schedule requires instruction and schedule")
		}
		for _, k := range []string{"connectors", "skills"} {
			if list, isList := args[k].([]any); isList {
				kept := []string{}
				for _, v := range list {
					if s, isStr := v.(string); isStr {
						kept = append(kept, s)
					}
				}
				payload[k] = kept
			}
		}
		res := s.callBackend(http.MethodPost, "/api/v1/agent/run/schedules", payload)
		if !res.ok {
			return s.failure(res)
		}
		return ok(textOr(res.data, "scheduled"))
	case "pin_message":
		messageID := str(args, "message_id")
		if messageID == "" {
			return fail("pin_message requires message_id")
		}
		payload := map[string]any{"message_id": messageID}
		if pinned, isBool := args["pinned"].(bool); isBool {
			payload["pinned"] = pinned
		}
		res := s.callBackend(http.MethodPost, "/api/v1/agent/run/pins", payload)
		if !res.ok {
			return s.failure(res)
		}
		return ok(textOr(res.data, "pinned"))
	case "publish_artifact":
		title, content := str(args, "title"), str(args, "content")
		if strings.TrimSpace(title) == "" || strings.TrimSpace(content) == "" {
			return fail("publish_artifact requires title and content")
		}
		kind := "text"
		if k, isStr := args["kind"].(string); isStr {
			kind = k
		}
		res := s.callBackend(http.MethodPost, "/api/v1/agent/run/artifacts", map[string]any{"kind": kind, "title": title, "content": content})
		if !res.ok {
			return s.failure(res)
		}
		return ok(fmt.Sprintf("published (artifactID=%v) — visible in this run's activity drawer; reference it by title in your reply", res.data["artifactID"]))
	case "list_skills":
		res := s.callBackend(http.MethodGet, "/api/v1/agent/run/skills", nil)
		if !res.ok {
			return s.failure(res)
		}
		skills, _ := res.data["skills"].([]any)
		if len(skills) == 0 {
			return ok("(no skills defined in this workspace)")
		}
		lines := make([]string, 0, len(skills))
		for _, raw := range skills {
			sk, _ := raw.(map[string]any)
			desc := ""
			if d, has := sk["description"]; has && d != nil {
				desc = fmt.Sprint(d)
			}
			lines = append(lines, fmt.Sprintf("[s:%v] %v — %s", sk["id"], sk["name"], desc))
		}
		return ok(strings.Join(lines, "\n"))
	case "invoke_skill":
		// Skills appear in the context as [sk:<id>] markers; models pass that
		// form as often as the bare id.
		skillID := strings.TrimPrefix(strings.Trim(strings.TrimSpace(str(args, "skillID")), "[]"), "sk:")
		if skillID == "" {
			return fail("invoke_skill requires skillID")
		}
		res := s.callBackend(http.MethodPost, "/api/v1/agent/run/skills/"+url.PathEscape(skillID), nil)
		if !res.ok {
			return s.failure(res)
		}
		return ok(fmt.Sprintf("# Skill: %v\n\n%v", res.data["name"], res.data["instructions"]))
	case "claim_task":
		label := str(args, "label")
		if strings.TrimSpace(label) == "" {
			return fail("claim_task requires a label")
		}
		res := s.callBackend(http.MethodPost, "/api/v1/agent/run/claims", map[string]any{"label": label})
		if !res.ok {
			return s.failure(res)
		}
		listing := ""
		if claims, _ := res.data["claims"].([]any); len(claims) > 0 {
			lines := make([]string, 0, len(claims))
			for _, c := range claims {
				lines = append(lines, fmt.Sprintf("- %v", c))
			}
			listing = "\ncurrent claims:\n" + strings.Join(lines, "\n")
		}
		if res.data["mine"] == true {
			return ok("claimed — this part is yours." + listing)
		}
		return ok("already taken — pick a DIFFERENT part." + listing)
	case "create_coding_task":
		return s.createCodingTask(args)
	case "publish_test_plan":
		return s.publishTestPlan(args)
	case "request_mr":
		return s.requestMR(args)
	case "task_state":
		if s.env.TaskID == "" {
			return fail("task_state only works inside a coding task run")
		}
		state, note := str(args, "state"), strings.TrimSpace(str(args, "note"))
		if state == "" && note == "" {
			return fail("task_state requires state and/or note")
		}
		res := s.callBackend(http.MethodPost, "/api/v1/agent/run/coding-task/report", map[string]any{"state": state, "note": note})
		if !res.ok {
			return s.failure(res)
		}
		return ok(stringOr(res.data["text"], "recorded"))
	case "register_project_commands":
		return s.registerProjectCommands(args)
	case "update_memory":
		res := s.callBackend(http.MethodPost, "/api/v1/agent/run/memory", map[string]any{"content": str(args, "content")})
		if !res.ok {
			return s.failure(res)
		}
		return ok(fmt.Sprintf("memory updated (%v bytes)", res.data["bytes"]))
	case "set_state":
		res := s.callBackend(http.MethodPost, "/api/v1/agent/run/state", map[string]any{"state": str(args, "state")})
		if !res.ok {
			return s.failure(res)
		}
		return ok("state updated")
	default:
		return fail("unknown tool: " + name)
	}
}

// stringOr renders v like JS String(v ?? fallback).
func stringOr(v any, fallback string) string {
	if v == nil {
		return fallback
	}
	return fmt.Sprint(v)
}

// approvalPrompt is the harness's permission gateway (claude
// --permission-prompt-tool): every native-tool permission request (Bash,
// Write, WebSearch, …) lands here and becomes an approval card for the
// invoker. The reply is the JSON contract the CLI expects, as text content.
func (s *Server) approvalPrompt(args map[string]any) toolResult {
	toolName := str(args, "tool_name")
	if toolName == "" {
		toolName = "unknown tool"
	}
	input, _ := args["input"].(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	allow := func() toolResult {
		// No updatedInput: omitting it means "run with the original input",
		// and echoing a large Write back made the reply itself big enough to
		// hit the clip layer — which corrupts the permission JSON.
		raw, _ := json.Marshal(map[string]any{"behavior": "allow"})
		return ok(string(raw))
	}
	// Credential locations (the runner's own token, ~/.ssh, cloud and CLI
	// logins…) always ask: no profile or standing permission covers them.
	protected := s.inputTouchesProtected(input)
	cls := toolClass(toolName)
	policyReason := ""
	switch {
	case protected:
		policyReason = "it touches a credential location"
	case s.env.TaskID != "" && s.env.TaskDir != "":
		// Task permission profile: inside a coding task's checkout, routine
		// coding work (edits, package managers, tests, local git) is auto-
		// approved — the approval card is for what reaches outside the
		// workspace.
		d := taskpolicy.Allows(toolName, input, taskpolicy.Context{TaskDir: s.env.TaskDir, StateDir: s.env.WorkDir})
		if d.Allow {
			return allow()
		}
		policyReason = d.Reason
	}
	// The invoker's standing "always allow <class> for this agent". For
	// reads and edits it covers the run's own folders, not the whole disk.
	if !protected && cls != "" && s.env.AutoAllow[cls] && (cls == "shell" || cls == "web" || s.inRunScope(input)) {
		return allow()
	}
	// Say WHY the card was raised — "asked because: unrecognized command …"
	// tells the invoker (and us, when debugging) which rule fired.
	summary := describetool.DescribeToolUse(toolName, input)
	if policyReason != "" {
		summary += " — asked because: " + policyReason
	}
	v := s.awaitApproval(summary, "tool", nil, cls)
	if v.err == "" && v.state == "approved" {
		return allow()
	}
	// A denial carries the invoker's direction so the model changes course
	// instead of retrying blindly.
	msg := "The invoker denied this tool use."
	if v.err == "" && v.note != "" {
		msg = "The invoker denied this and says: " + v.note
	}
	raw, _ := json.Marshal(map[string]any{"behavior": "deny", "message": msg})
	return ok(string(raw))
}
