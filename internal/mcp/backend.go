package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/api"
)

// backendResult is one run-token call to the ex backend.
type backendResult struct {
	ok     bool
	status int
	data   map[string]any
}

// callBackend sends one request with the run token. A nil body sends none.
// The backend wraps rejections as {error: {code, message}} — they are
// hoisted so every reader (describeFailure, the per-tool 409 branches) sees
// flat `error` (the code) and `message` fields. A transport failure panics
// into safeCall's "tool transport error … [retryable=true]".
func (s *Server) callBackend(method, path string, body any) backendResult {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, s.env.BaseURL+path, reader)
	if err != nil {
		panic(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.env.RunToken)
	req.Header.Set("Content-Type", "application/json")
	res, err := s.http.Do(req)
	if err != nil {
		panic(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	data := map[string]any{}
	_ = json.Unmarshal(raw, &data) // empty/non-JSON body → {}
	if data == nil {
		data = map[string]any{}
	}
	return backendResult{ok: res.StatusCode >= 200 && res.StatusCode <= 299, status: res.StatusCode, data: api.FlattenBackendError(data)}
}

// describeFailure turns a backend rejection into an actionable, structured
// message for the model: a permission denial should not be retried; a
// closed run means stop entirely.
func describeFailure(status int, data map[string]any) string {
	msg, isStr := data["message"].(string)
	if !isStr {
		msg = fmt.Sprintf("HTTP %d", status)
	}
	switch status {
	case 409:
		// 409s carry a code (handler contract): only run_closed means down tools.
		if data["error"] == "run_closed" {
			return fmt.Sprintf("run is closed; stop all further work (%s) [retryable=false]", msg)
		}
		return fmt.Sprintf("blocked: %s — this step is out of order; do a different step, don't just retry [retryable=false]", msg)
	case 403:
		return fmt.Sprintf("not permitted: %s [retryable=false]", msg)
	case 429:
		return fmt.Sprintf("rate limited or post cap reached: %s [retryable=false]", msg)
	}
	return fmt.Sprintf("failed: %s [retryable=%v]", msg, status >= 500)
}

func (s *Server) failure(r backendResult) toolResult { return fail(describeFailure(r.status, r.data)) }

// verdict is an approval's outcome: approved | denied | expired, or a
// backend error.
type verdict struct {
	state, choice, note, approvalID string
	err                             string
}

// awaitApproval opens an approval (plain, or multiple-choice when options
// are given) and blocks-by-polling until the invoker decides or the backend
// expires it (plan-v2 §7) — shared by request_approval, ask_user, and the
// harness's approval_prompt permission gateway.
func (s *Server) awaitApproval(summary, risk string, options []string, toolKind string) verdict {
	body := map[string]any{"summary": summary, "risk": risk}
	if options != nil {
		body["options"] = options
	}
	if toolKind != "" {
		body["toolKind"] = toolKind
	}
	created := s.callBackend(http.MethodPost, "/api/v1/agent/run/approvals", body)
	if !created.ok {
		return verdict{err: describeFailure(created.status, created.data)}
	}
	deadline, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(created.data["deadline"]))
	return s.pollApproval(fmt.Sprint(created.data["approvalID"]), deadline)
}

// pollApproval waits on an EXISTING approval — the gates the backend raises
// itself (use_connector, request_mr), where the card's text and its purpose
// are the server's, not the model's. A zero deadline is learned from the
// first poll.
func (s *Server) pollApproval(approvalID string, deadline time.Time) verdict {
	for {
		time.Sleep(s.pollEvery)
		res := s.callBackend(http.MethodGet, "/api/v1/agent/run/approvals/"+approvalID, nil)
		if !res.ok {
			return verdict{err: describeFailure(res.status, res.data)}
		}
		state := fmt.Sprint(res.data["state"])
		if state == "approved" || state == "denied" || state == "expired" {
			v := verdict{state: state, approvalID: approvalID}
			v.choice, _ = res.data["choice"].(string)
			// The invoker's typed direction ("no — use the seed DB instead").
			v.note, _ = res.data["note"].(string)
			return v
		}
		if deadline.IsZero() {
			deadline, _ = time.Parse(time.RFC3339Nano, fmt.Sprint(res.data["deadline"]))
		}
		if !deadline.IsZero() && s.now().After(deadline.Add(15*time.Second)) {
			return verdict{state: "expired", approvalID: approvalID}
		}
	}
}
