package mcp

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/spill"
	"github.com/DigitalTolk/ex-runners/internal/toolindex"
)

// toolsJSON is the tool contract table (descriptions say WHEN, not just
// what — the harness routes on them). It was extracted verbatim from the
// TypeScript runner so agents see byte-identical tool definitions.
//
//go:embed tools.json
var toolsJSON []byte

var allTools = mustTools(toolsJSON)

func mustTools(raw []byte) []toolindex.ToolDef {
	var tools []toolindex.ToolDef
	if err := json.Unmarshal(raw, &tools); err != nil {
		panic("mcp: tools.json is invalid: " + err.Error())
	}
	return tools
}

// Server answers one run's MCP session.
type Server struct {
	env  Env
	http *http.Client
	// pollEvery paces approval polling (2s like the TS server).
	pollEvery time.Duration
	now       func() time.Time

	// Spill: oversized tool results are stored and replaced with a head/tail
	// preview + a fetch_spill locator — the model pages through the full data
	// instead of losing it to truncation. One store per run.
	spills *spill.Store

	mu          sync.Mutex
	described   map[string]bool // tools whose full contract this run has read (index mode)
	postSeq     int
	responseSeq int

	outMu sync.Mutex
	out   io.Writer
}

// NewServer builds a server for one run.
func NewServer(env Env) *Server {
	return &Server{
		env:       env,
		http:      &http.Client{Timeout: 120 * time.Second},
		pollEvery: 2 * time.Second,
		now:       time.Now,
		spills:    spill.NewStore(),
		described: map[string]bool{},
	}
}

// visibleTools is the advertised tool set for this run. connector_call and
// use_connector stay advertised even with nothing attached: use_connector
// can attach mid-run (harnesses cache the tool list, so hiding-then-revealing
// wouldn't propagate).
func (s *Server) visibleTools() []toolindex.ToolDef {
	out := make([]toolindex.ToolDef, 0, len(allTools))
	for _, t := range allTools {
		if s.env.TaskID == "" && taskOnly[t.Name] {
			continue
		}
		if s.env.Gated && gatedHidden[t.Name] {
			continue
		}
		out = append(out, t)
	}
	return out
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toolResult is an MCP tool result; IsError tells the model the call failed
// in a way it can reason about (retry vs give up).
type toolResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError"`
}

func result(text string, isError bool) toolResult {
	return toolResult{Content: []textContent{{Type: "text", Text: text}}, IsError: isError}
}

func ok(text string) toolResult { return result(text, false) }

func fail(text string) toolResult { return result(text, true) }

// applySpill routes every successful tool result through the spill store.
// Errors stay verbatim (short, must not be rewritten); fetch_spill output is
// already a bounded slice; approval_prompt replies are the CLI's
// permission-JSON frames — spilling one corrupts the contract.
func (s *Server) applySpill(name string, r toolResult) toolResult {
	if name == "fetch_spill" || name == "approval_prompt" || r.IsError || len(r.Content) == 0 {
		return r
	}
	text := r.Content[0].Text
	out := s.spills.MaybeSpill(name, text)
	if out == text {
		return r
	}
	return ok(out)
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  map[string]any  `json:"params,omitempty"`
}

func (s *Server) send(msg map[string]any) {
	raw, err := json.Marshal(msg)
	if err != nil {
		raw, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": fail("tool result could not be encoded: " + err.Error())})
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	_, _ = s.out.Write(append(raw, '\n'))
}

func idOf(req rpcRequest) any {
	if len(req.ID) == 0 {
		return nil
	}
	return req.ID
}

func (s *Server) reply(req rpcRequest, res any) {
	s.send(map[string]any{"jsonrpc": "2.0", "id": idOf(req), "result": res})
}

func (s *Server) handle(req rpcRequest) {
	switch req.Method {
	case "initialize":
		s.reply(req, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "ex", "version": "1.0.0"},
		})
	case "notifications/initialized", "notifications/cancelled":
		// notifications carry no reply
	case "ping":
		s.reply(req, map[string]any{})
	case "tools/list":
		tools := s.visibleTools()
		if s.env.ToolIndex {
			tools = toolindex.IndexedTools(tools)
		}
		s.reply(req, map[string]any{"tools": tools})
	case "tools/call":
		name, _ := req.Params["name"].(string)
		args, _ := req.Params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		if s.env.ToolIndex && name == toolindex.ToolInfo {
			s.mu.Lock()
			text := toolindex.Info(s.visibleTools(), args["names"], s.described)
			s.mu.Unlock()
			s.reply(req, ok(text))
			return
		}
		if s.env.ToolIndex && name == toolindex.ToolCall {
			s.mu.Lock()
			inner := toolindex.UnwrapCall(s.visibleTools(), args, s.described)
			s.mu.Unlock()
			switch {
			case inner.Error != "":
				s.reply(req, fail(inner.Error))
				return
			case inner.Contract != "":
				s.reply(req, ok(inner.Contract))
				return
			}
			name, args = inner.Name, inner.Args
		}
		s.reply(req, s.applySpill(name, s.safeCall(name, args)))
	default:
		if len(req.ID) > 0 {
			s.send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "method not found: " + req.Method}})
		}
	}
}

// safeCall runs one tool; a panic becomes a retryable transport error
// instead of killing the server mid-run.
func (s *Server) safeCall(name string, args map[string]any) (r toolResult) {
	defer func() {
		if p := recover(); p != nil {
			r = fail(fmt.Sprintf("tool transport error: %v [retryable=true]", p))
		}
	}()
	// Gated watcher modes hide every outward tool from the list; a call by
	// name (or through ex_tool_call) is refused the same way, so the mode
	// alone decides delivery.
	if s.env.Gated && gatedHidden[name] {
		return fail(fmt.Sprintf("%s is not available in this %s run — your final text is the deliverable [retryable=false]", name, s.env.ActionMode))
	}
	return s.call(name, args)
}

// Serve reads JSON-RPC lines until in closes. Requests run concurrently —
// request_approval and friends block for minutes and must not stall pings or
// other calls. The CLI closing stdin is the shutdown signal: Serve returns
// at once without waiting for in-flight calls, and the run token dies with
// the process.
func (s *Server) Serve(in io.Reader, out io.Writer) {
	s.out = out
	r := bufio.NewReader(in)
	for {
		line, err := r.ReadString('\n')
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			var req rpcRequest
			if json.Unmarshal([]byte(trimmed), &req) == nil { // not JSON-RPC → ignore
				go s.handle(req)
			}
		}
		if err != nil {
			return
		}
	}
}

// Main is the `ex-runner mcp-server` entrypoint.
func Main(getenv func(string) string, cwd string, in io.Reader, out, errOut io.Writer) int {
	env := LoadEnv(getenv, cwd)
	if env.BaseURL == "" || env.RunToken == "" {
		_, _ = fmt.Fprintln(errOut, "ex-mcp: EX_BASE_URL and EX_RUN_TOKEN are required")
		return 1
	}
	NewServer(env).Serve(in, out)
	return 0
}
