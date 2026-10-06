// Package mcp is the local MCP stdio server — the agent's hands (plan-v2
// §7). The harness CLI (the MCP client) spawns it per run as
// `ex-runner mcp-server`, with the run's settings in the environment.
//
// Every tool call becomes an authenticated REST call to the ex backend using
// the run-scoped token from the environment (EX_RUN_TOKEN — env, never argv:
// argv is world-readable in `ps`). The token carries the invoker's
// permissions and dies with the run, so this process can never do anything
// the invoking human couldn't.
//
// Protocol: newline-delimited JSON-RPC 2.0 over stdio, MCP 2024-11-05.
// Hand-rolled on purpose — a few methods and a static tool set don't justify
// an SDK dependency.
package mcp

import (
	"encoding/json"
	"strings"
)

// taskRepoEnv is one coding-task checkout (EX_TASK_REPOS): request_mr pushes
// and opens an MR per CHANGED repo.
type taskRepoEnv struct {
	Path   string `json:"path"`
	Role   string `json:"role"`
	Dir    string `json:"dir"`
	Branch string `json:"branch"`
	Base   string `json:"base"`
}

// connectorCred is one connector attached to this run (EX_CONNECTORS),
// passed via env so the credential never touches the harness shell or disk.
// connector_call is the ONLY path to these APIs: the URL is pinned to the
// connector's base URL, so the agent cannot aim the token anywhere else.
type connectorCred struct {
	Slug       string `json:"slug"`
	Title      string `json:"title"`
	BaseURL    string `json:"baseURL"`
	Token      string `json:"token"`
	AuthHeader string `json:"authHeader,omitempty"` // header template; empty = Authorization: Bearer
}

// Env is one run's configuration, read once at startup.
type Env struct {
	BaseURL  string
	RunToken string
	// WorkDir is our per-run scratch dir (saved API responses, synced
	// connector docs, the dev-server pid/log). Distinct from the cwd: for
	// coding tasks the harness — and therefore this server — runs INSIDE the
	// user's repo, and nothing of ours may land there.
	WorkDir string
	// PrivateDir holds what the agent must never touch: the dev-server pid
	// files (stopping one signals whatever group its pid file names). It is
	// outside every auto-approved root; "" falls back to WorkDir.
	PrivateDir string
	// RunnerHome is the runner's own state root (~/.ex-runner: its account
	// token, per-run secrets). Tool calls that touch it always ask.
	RunnerHome string
	// Coding task: set only on task-mode runs. TaskDir is the project
	// checkout the task permission profile is scoped to.
	TaskID, TaskDir, TaskProject string
	TaskRepos                    []taskRepoEnv
	// WorkspaceRoot is where the project registry lives.
	WorkspaceRoot string
	// AutoAllow: harness tool classes the invoker pre-approved for this agent
	// ("always allow reads") — the permission gateway skips the card for them.
	AutoAllow map[string]bool
	// ActionMode: gated watcher modes (notify/draft/reply) are
	// DETERMINISTIC — the agent never routes its own output, so every tool
	// that communicates is hidden and the server delivers its final text.
	ActionMode string
	Gated      bool
	// ToolIndex (EX_TOOL_INDEX=1, set for codex): advertise the core tools
	// and a catalog behind two meta-tools instead of every definition.
	ToolIndex  bool
	Connectors map[string]connectorCred
}

// LoadEnv reads the run settings the runner put in the environment.
func LoadEnv(getenv func(string) string, cwd string) Env {
	e := Env{
		BaseURL:       getenv("EX_BASE_URL"),
		RunToken:      getenv("EX_RUN_TOKEN"),
		WorkDir:       getenv("EX_WORK_DIR"),
		PrivateDir:    getenv("EX_PRIVATE_DIR"),
		RunnerHome:    getenv("EX_RUNNER_HOME"),
		TaskID:        getenv("EX_TASK_ID"),
		TaskDir:       getenv("EX_TASK_DIR"),
		TaskProject:   getenv("EX_TASK_PROJECT"),
		WorkspaceRoot: getenv("EX_WORKSPACE_ROOT"),
		ActionMode:    getenv("EX_ACTION_MODE"),
		ToolIndex:     getenv("EX_TOOL_INDEX") == "1",
		AutoAllow:     map[string]bool{},
		Connectors:    map[string]connectorCred{},
	}
	if e.WorkDir == "" {
		e.WorkDir = cwd
	}
	e.Gated = e.ActionMode == "notify" || e.ActionMode == "draft" || e.ActionMode == "reply"
	for _, s := range strings.Split(getenv("EX_AUTO_ALLOW"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			e.AutoAllow[s] = true
		}
	}
	var repos []taskRepoEnv
	if json.Unmarshal([]byte(getenv("EX_TASK_REPOS")), &repos) == nil {
		for _, r := range repos {
			if r.Path != "" && r.Dir != "" && r.Branch != "" {
				e.TaskRepos = append(e.TaskRepos, r)
			}
		}
	}
	var conns []connectorCred
	if json.Unmarshal([]byte(getenv("EX_CONNECTORS")), &conns) == nil { // malformed → no connectors
		for _, c := range conns {
			if c.Slug != "" && c.BaseURL != "" && c.Token != "" {
				e.Connectors[c.Slug] = c
			}
		}
	}
	return e
}

// toolClass maps a harness tool onto the user-facing permission classes.
func toolClass(toolName string) string {
	switch toolName {
	case "Read", "Glob", "Grep", "LS", "NotebookRead":
		return "read"
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		return "edit"
	case "Bash":
		return "shell"
	case "WebFetch", "WebSearch":
		return "web"
	default:
		return ""
	}
}

// taskOnly tools make sense only inside a task run — hidden elsewhere so a
// plain chat run never sees "request_mr".
var taskOnly = map[string]bool{"publish_test_plan": true, "request_mr": true, "task_state": true, "register_project_commands": true}

// gatedHidden: every tool that emits a message/action outward. In gated
// modes the mode owns delivery, so none of these are offered — the agent's
// final text is the deliverable, full stop.
var gatedHidden = map[string]bool{
	"post_message": true, "post_to_channel": true, "send_dm": true,
	"add_reaction": true, "notify_owner": true, "propose_reply": true,
}
