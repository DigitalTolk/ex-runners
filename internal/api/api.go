// Package api is the HTTP client for the ex backend's runner API. Outbound
// HTTPS only — the runner never listens for work (plan-v2 §2); it
// long-polls for it.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

// Error is a non-2xx answer from the server.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// IsUnauthorized reports whether err is the server rejecting the runner
// token (revoked, expired, or the account deactivated).
func IsUnauthorized(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusUnauthorized
}

// ErrorFields reads the ex server's error body. The server writes
// {"error":{"code","message"}}; older shapes put both at the top level.
func ErrorFields(body map[string]any) (code, message string) {
	if nested, ok := body["error"].(map[string]any); ok {
		code, _ = nested["code"].(string)
		message, _ = nested["message"].(string)
		return code, message
	}
	code, _ = body["error"].(string)
	message, _ = body["message"].(string)
	return code, message
}

// FlattenBackendError hoists a nested {error: {code, message}} body in
// place: `error` becomes the code string, `message` the human text. Flat or
// foreign shapes pass through untouched, and an existing flat `message`
// wins. Every reader in the MCP server keys off the flat fields.
func FlattenBackendError(data map[string]any) map[string]any {
	wrapped, ok := data["error"].(map[string]any)
	if !ok {
		return data
	}
	if code, ok := wrapped["code"].(string); ok {
		data["error"] = code
	}
	if msg, ok := wrapped["message"].(string); ok {
		if _, flat := data["message"].(string); !flat {
			data["message"] = msg
		}
	}
	return data
}

// Identity is what a runner announces about itself.
type Identity struct {
	RunnerID  string                   `json:"runnerID"`
	Host      string                   `json:"host"`
	OS        string                   `json:"os"`
	Harnesses []protocol.RunnerHarness `json:"harnesses"`
}

// RenewResponse is a fresh token with a new expiry.
type RenewResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expiresAt"`
}

// Client talks to one ex server with a runner token. Token is read on
// every call: the CLI renews it while the runner is live, and each request
// must carry the CURRENT one.
type Client struct {
	BaseURL string
	Token   func() string
	HTTP    *http.Client
}

// New builds a client with a 30s request timeout — above the server's 20s
// claim long-poll, below anything that would look hung.
func New(baseURL string, token func() string) *Client {
	return &Client{BaseURL: baseURL, Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// call sends one JSON request. A 204 leaves out untouched; a non-2xx becomes
// an *Error carrying the server's code and message when it sent them.
func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token())
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode == http.StatusNoContent {
		return nil
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		e := &Error{Status: res.StatusCode, Code: "http_error", Message: strconv.Itoa(res.StatusCode)}
		var parsed map[string]any
		if json.Unmarshal(raw, &parsed) == nil {
			code, msg := ErrorFields(parsed)
			if code != "" {
				e.Code = code
			}
			if msg != "" {
				e.Message = msg
			}
		} else if text := strings.TrimSpace(string(raw)); text != "" && len(text) <= 200 && !strings.ContainsAny(text, "<\n") {
			// Middleware answers in plain text (http.Error: "runner token
			// revoked"); keep it — "401" alone says nothing in the log.
			e.Message = e.Message + ": " + text
		}
		return e
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// Register announces the runner.
func (c *Client) Register(ctx context.Context, id Identity) (protocol.RegisterResponse, error) {
	var out protocol.RegisterResponse
	err := c.call(ctx, http.MethodPost, "/api/v1/agent/runner/register", id, &out)
	return out, err
}

// Claim long-polls for work; an empty poll (204) yields no assignments.
func (c *Client) Claim(ctx context.Context, id Identity, max, waitSec int) ([]protocol.Assignment, error) {
	names := make([]string, 0, len(id.Harnesses))
	for _, h := range id.Harnesses {
		names = append(names, h.Name)
	}
	var out struct {
		Assignments []protocol.Assignment `json:"assignments"`
	}
	err := c.call(ctx, http.MethodPost, "/api/v1/agent/runner/claim", map[string]any{
		"runnerID":  id.RunnerID,
		"harnesses": names,
		"max":       max,
		"waitSec":   waitSec,
	}, &out)
	return out.Assignments, err
}

// Heartbeat refreshes the runner lease and every active run's lease.
func (c *Client) Heartbeat(ctx context.Context, id Identity, activeRunIDs []string) (protocol.HeartbeatResponse, error) {
	var out protocol.HeartbeatResponse
	body := struct {
		Identity
		ActiveRunIDs []string `json:"activeRunIDs"`
	}{id, activeRunIDs}
	err := c.call(ctx, http.MethodPost, "/api/v1/agent/runner/heartbeat", body, &out)
	return out, err
}

// Events reports a batch; the response says whether to kill the harness.
func (c *Client) Events(ctx context.Context, runnerID, runID string, events []protocol.RunEventInput) (protocol.EventsResponse, error) {
	var out protocol.EventsResponse
	err := c.call(ctx, http.MethodPost, "/api/v1/agent/runner/runs/"+runID+"/events", map[string]any{
		"runnerID": runnerID,
		"events":   events,
	}, &out)
	return out, err
}

// Complete closes a run successfully.
func (c *Client) Complete(ctx context.Context, runnerID, runID, finalText string, usage protocol.Usage) error {
	return c.call(ctx, http.MethodPost, "/api/v1/agent/runner/runs/"+runID+"/complete", map[string]any{
		"runnerID":  runnerID,
		"finalText": finalText,
		"usage":     usage,
	}, nil)
}

// Fail closes a run with a reason the server turns into a chat notice.
func (c *Client) Fail(ctx context.Context, runnerID, runID, reason string) error {
	return c.call(ctx, http.MethodPost, "/api/v1/agent/runner/runs/"+runID+"/fail", map[string]any{
		"runnerID": runnerID,
		"reason":   reason,
	}, nil)
}

// Renew trades the current token for a fresh one with a new expiry. Same
// token ID server-side, so a revoke still kills both.
func (c *Client) Renew(ctx context.Context) (RenewResponse, error) {
	var out RenewResponse
	err := c.call(ctx, http.MethodPost, "/api/v1/agent/runner/renew", map[string]any{}, &out)
	return out, err
}

// Revoke retires this runner's own token (`ex-runner logout`, or a
// re-login that replaces it) so it leaves the Runners page.
func (c *Client) Revoke(ctx context.Context) error {
	return c.call(ctx, http.MethodPost, "/api/v1/agent/runner/revoke", map[string]any{}, nil)
}

// SetRunStatus sets the run's visible status emoji straight from the
// runner, over the run-token endpoint the set_state MCP tool uses. It used
// to be the agent's FIRST tool call on every run — a whole model turn spent
// on a status emoji. Best-effort: callers log and move on.
func SetRunStatus(ctx context.Context, baseURL, runToken, state string) error {
	c := New(baseURL, func() string { return runToken })
	if err := c.call(ctx, http.MethodPost, "/api/v1/agent/run/state", map[string]any{"state": state}, nil); err != nil {
		var e *Error
		if errors.As(err, &e) {
			return fmt.Errorf("set status failed: %d", e.Status)
		}
		return err
	}
	return nil
}
