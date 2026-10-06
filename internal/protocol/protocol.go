// Package protocol holds the wire types shared between ex-runner and the ex
// backend's runner API (ex/internal/handler/agentrunner.go and
// ex/internal/service/orchestrator.go). JSON tags match the server's.
package protocol

// Logger is the runner's structured log line: a message plus optional
// key/value detail. The CLI renders it with a timestamp.
type Logger func(msg string, extra map[string]any)

// NopLogger discards everything.
func NopLogger(string, map[string]any) {}

// RunnerHarness is one agent CLI the runner can drive ("claude" | "codex").
type RunnerHarness struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Authed  bool   `json:"authed"`
}

// AgentLimits mirrors the server's model.AgentLimits (the fields the runner
// reads). Task budgets (direct @mentions) get more turns/wall-clock than
// ambient conversation.
type AgentLimits struct {
	MaxTurns            int   `json:"maxTurns,omitempty"`
	MaxWallClockSec     int   `json:"maxWallClockSec,omitempty"`
	MaxTokens           int64 `json:"maxTokens,omitempty"`
	MaxPosts            int   `json:"maxPosts,omitempty"`
	MaxConsultDepth     int   `json:"maxConsultDepth,omitempty"`
	MaxTaskTurns        int   `json:"maxTaskTurns,omitempty"`
	MaxTaskWallClockSec int   `json:"maxTaskWallClockSec,omitempty"`
}

// TurnsFor mirrors the backend's mode-aware turn budget: direct tasks get
// depth, ambient modes (watch/heartbeat/followup) stay conversational.
// Coding-task runs are uncapped by decision: 0 means "pass no --max-turns";
// the server's rolling idle deadline is the reaper. A scheduled standing
// order is explicit work, so it gets the direct depth.
func TurnsFor(l AgentLimits, mode string) int {
	switch mode {
	case "task":
		return 0
	case "direct", "scheduled", "":
		if l.MaxTaskTurns > 0 {
			return l.MaxTaskTurns
		}
		return 128
	default:
		if l.MaxTurns > 0 {
			return l.MaxTurns
		}
		return 16
	}
}

// TaskSpecRepo is one repo of a coding task (mirrors model.TaskSpecRepo).
type TaskSpecRepo struct {
	Path       string `json:"path"` // GitLab "group/sub/repo"
	Role       string `json:"role"` // backend | frontend | mobile | infra | other
	Branch     string `json:"branch"`
	BaseBranch string `json:"baseBranch,omitempty"` // "" → the repo's default branch
	MRURL      string `json:"mrURL,omitempty"`
}

// TaskSpec is the coding-task snapshot on a task-mode assignment (mirrors
// model.TaskSpec). The workspace manager prepares the checkout from it.
type TaskSpec struct {
	ID           string         `json:"id"`
	ProjectKey   string         `json:"projectKey"`
	ProjectName  string         `json:"projectName"`
	Title        string         `json:"title"`
	Goal         string         `json:"goal"`
	Kind         string         `json:"kind"`
	State        string         `json:"state"`
	Repos        []TaskSpecRepo `json:"repos"`
	ChannelID    string         `json:"channelID"`
	ThreadRootID string         `json:"threadRootID"`
	TestURL      string         `json:"testURL,omitempty"`
	SignedOff    bool           `json:"signedOff,omitempty"`
	RunnerID     string         `json:"runnerID,omitempty"`
}

// Assignment is one claimed run (mirrors service.Assignment).
type Assignment struct {
	RunID        string `json:"runID"`
	AgentID      string `json:"agentID"`
	AgentName    string `json:"agentName"`
	InvokerID    string `json:"invokerID"`
	InvokerName  string `json:"invokerName"`
	ParentID     string `json:"parentID"`
	ParentType   string `json:"parentType"`
	ThreadRootID string `json:"threadRootID,omitempty"`
	MessageID    string `json:"messageID"`
	Harness      string `json:"harness"`
	Model        string `json:"model,omitempty"`
	Persona      string `json:"persona"`
	// Mode: "direct" | "watch" | "heartbeat" | "followup" | "task" | "scheduled".
	Mode string `json:"mode,omitempty"`
	// AskFirst (follow-up runs): the invoker wants an approval gate before
	// the agent actually posts its reply.
	AskFirst bool `json:"askFirst,omitempty"`
	// Watcher runs: the creator's standing order + how much the agent may do
	// (notify | draft | reply | autonomous).
	WatchInstruction string `json:"watchInstruction,omitempty"`
	ActionMode       string `json:"actionMode,omitempty"`
	Prompt           string `json:"prompt"`
	ContextBundle    string `json:"contextBundle"`
	// Connectors the invoking message explicitly picked with /slug tokens.
	ConnectorSlugs []string `json:"connectorSlugs,omitempty"`
	// Coding task (mode "task"): the harness runs INSIDE the checkout.
	Task *TaskSpec `json:"task,omitempty"`
	// Harness tool classes the invoker pre-approved (read|edit|shell|web).
	AutoAllow      []string    `json:"autoAllow,omitempty"`
	Limits         AgentLimits `json:"limits"`
	MCPToken       string      `json:"mcpToken"`
	LeaseExpiresAt string      `json:"leaseExpiresAt"`
	Deadline       string      `json:"deadline"`
}

// RunEventInput is one runner→backend event. Seq is a per-run monotonic
// counter starting at 1 so retried batches are idempotent server-side.
// Type: turn | usage | progress | tool | tool_result | state | prompt.
type RunEventInput struct {
	Seq     int            `json:"seq"`
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload,omitempty"`
}

// EventsResponse tells the runner whether to abort the run.
type EventsResponse struct {
	Abort  bool   `json:"abort"`
	Reason string `json:"reason,omitempty"`
}

// RegisteredAgent is one shared agent the server lists at registration.
type RegisteredAgent struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Slug        string `json:"slug"`
}

// RegisterResponse answers POST /api/v1/agent/runner/register.
type RegisterResponse struct {
	RunnerID string            `json:"runnerID"`
	Agents   []RegisteredAgent `json:"agents"`
	LeaseSec int               `json:"leaseSec"`
}

// HeartbeatResponse lists runs the server wants killed.
type HeartbeatResponse struct {
	Kill []string `json:"kill"`
}

// Usage is a token count pair.
type Usage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
}

// RunOutcome is the result of one harness execution.
type RunOutcome struct {
	OK        bool
	FinalText string
	Reason    string
	Usage     Usage
}
