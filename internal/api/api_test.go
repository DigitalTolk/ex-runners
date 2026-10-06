package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

type seen struct {
	method, path, auth string
	body               map[string]any
}

// serve answers every request with status/body and records what it saw.
func serve(t *testing.T, status int, body string) (*httptest.Server, *[]seen) {
	t.Helper()
	var got []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		got = append(got, seen{r.Method, r.URL.Path, r.Header.Get("Authorization"), b})
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

var id = Identity{RunnerID: "r1", Host: "mac", OS: "darwin", Harnesses: []protocol.RunnerHarness{{Name: "claude", Version: "1", Authed: true}}}

func fixed(tok string) func() string { return func() string { return tok } }

func TestRegisterSendsIdentityWithBearer(t *testing.T) {
	srv, got := serve(t, 200, `{"runnerID":"r1","agents":[{"id":"a","displayName":"gg","slug":"gg"}],"leaseSec":60}`)
	res, err := New(srv.URL, fixed("tok")).Register(context.Background(), id)
	if err != nil || res.RunnerID != "r1" || res.Agents[0].Slug != "gg" {
		t.Fatalf("Register = %+v, %v", res, err)
	}
	s := (*got)[0]
	if s.method != "POST" || s.path != "/api/v1/agent/runner/register" || s.auth != "Bearer tok" || s.body["runnerID"] != "r1" {
		t.Fatalf("request = %+v", s)
	}
}

func TestClaimFlattensHarnessNamesAndEmptyPoll(t *testing.T) {
	srv, got := serve(t, 204, "")
	as, err := New(srv.URL, fixed("tok")).Claim(context.Background(), id, 2, 20)
	if err != nil || len(as) != 0 {
		t.Fatalf("Claim(204) = %v, %v", as, err)
	}
	b := (*got)[0].body
	if b["runnerID"] != "r1" || b["max"] != float64(2) || b["waitSec"] != float64(20) || b["harnesses"].([]any)[0] != "claude" {
		t.Fatalf("claim body = %v", b)
	}

	srv2, _ := serve(t, 200, `{"assignments":[{"runID":"run1","harness":"claude"}]}`)
	as, err = New(srv2.URL, fixed("tok")).Claim(context.Background(), id, 1, 20)
	if err != nil || len(as) != 1 || as[0].RunID != "run1" {
		t.Fatalf("Claim(work) = %v, %v", as, err)
	}
}

func TestErrorsCarryServerCodeAndMessage(t *testing.T) {
	cases := []struct {
		status          int
		body, code, msg string
		unauthorized    bool
	}{
		{401, `{"error":{"code":"runner_disconnected","message":"runner: this runner was disconnected"}}`, "runner_disconnected", "runner: this runner was disconnected", true},
		{401, `{"error":"token_revoked","message":"runner token revoked"}`, "token_revoked", "runner token revoked", true},
		// Plain-text bodies (the server's auth middleware uses http.Error).
		{401, "runner token revoked\n", "http_error", "401: runner token revoked", true},
		{502, `not json`, "http_error", "502: not json", false},
		{502, "<html><body>Bad Gateway</body></html>", "http_error", "502", false},
		{503, "", "http_error", "503", false},
		{500, `{}`, "http_error", "500", false},
	}
	for _, c := range cases {
		srv, _ := serve(t, c.status, c.body)
		_, err := New(srv.URL, fixed("tok")).Heartbeat(context.Background(), id, []string{"run1"})
		var e *Error
		if !errors.As(err, &e) || e.Status != c.status || e.Code != c.code || e.Message != c.msg || e.Error() != c.msg {
			t.Errorf("%d %s: err = %#v", c.status, c.body, err)
		}
		if IsUnauthorized(err) != c.unauthorized {
			t.Errorf("%d: IsUnauthorized = %v", c.status, !c.unauthorized)
		}
	}
	if IsUnauthorized(errors.New("plain")) {
		t.Error("plain error is not unauthorized")
	}
}

func TestTokenGetterIsReadPerCall(t *testing.T) {
	srv, got := serve(t, 200, `{}`)
	current := "tok-1"
	c := New(srv.URL, func() string { return current })
	_, _ = c.Heartbeat(context.Background(), id, nil)
	current = "tok-2"
	_, _ = c.Heartbeat(context.Background(), id, nil)
	if (*got)[0].auth != "Bearer tok-1" || (*got)[1].auth != "Bearer tok-2" {
		t.Fatalf("auth = %q %q", (*got)[0].auth, (*got)[1].auth)
	}
	if (*got)[0].body["activeRunIDs"] != nil {
		t.Fatalf("nil active runs should marshal as null, got %v", (*got)[0].body["activeRunIDs"])
	}
}

func TestRunCallsHitTheirRoutes(t *testing.T) {
	srv, got := serve(t, 200, `{"abort":true,"reason":"turn_limit","token":"fresh","expiresAt":"2026-11-04T00:00:00Z"}`)
	c := New(srv.URL, fixed("tok"))
	ctx := context.Background()
	ev, err := c.Events(ctx, "r1", "run9", []protocol.RunEventInput{{Seq: 1, Type: "turn"}})
	if err != nil || !ev.Abort || ev.Reason != "turn_limit" {
		t.Fatalf("Events = %+v, %v", ev, err)
	}
	if err := c.Complete(ctx, "r1", "run9", "done", protocol.Usage{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Fail(ctx, "r1", "run9", "boom"); err != nil {
		t.Fatal(err)
	}
	rr, err := c.Renew(ctx)
	if err != nil || rr.Token != "fresh" {
		t.Fatalf("Renew = %+v, %v", rr, err)
	}
	if err := c.Revoke(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/api/v1/agent/runner/runs/run9/events",
		"/api/v1/agent/runner/runs/run9/complete",
		"/api/v1/agent/runner/runs/run9/fail",
		"/api/v1/agent/runner/renew",
		"/api/v1/agent/runner/revoke",
	}
	for i, p := range want {
		if (*got)[i].path != p {
			t.Errorf("call %d path = %s, want %s", i, (*got)[i].path, p)
		}
	}
	if (*got)[2].body["reason"] != "boom" || (*got)[1].body["finalText"] != "done" {
		t.Errorf("bodies = %v / %v", (*got)[1].body, (*got)[2].body)
	}
}

func TestTransportAndDecodeFailures(t *testing.T) {
	c := New("http://127.0.0.1:1", fixed("tok"))
	if _, err := c.Register(context.Background(), id); err == nil {
		t.Fatal("expected a transport error")
	}
	if _, err := New("http://bad host", fixed("tok")).Register(context.Background(), id); err == nil {
		t.Fatal("expected a request-build error")
	}
	srv, _ := serve(t, 200, `{not json`)
	if _, err := New(srv.URL, fixed("tok")).Register(context.Background(), id); err == nil || !strings.Contains(err.Error(), "decode /api/v1/agent/runner/register") {
		t.Fatalf("decode error = %v", err)
	}
	if err := New(srv.URL, fixed("tok")).call(context.Background(), "POST", "/x", make(chan int), nil); err == nil {
		t.Fatal("expected a marshal error")
	}
}

func TestErrorFieldsAndFlatten(t *testing.T) {
	if c, m := ErrorFields(map[string]any{"error": map[string]any{"code": "x", "message": "y"}}); c != "x" || m != "y" {
		t.Errorf("nested = %q %q", c, m)
	}
	if c, m := ErrorFields(map[string]any{"error": "x", "message": "y"}); c != "x" || m != "y" {
		t.Errorf("flat = %q %q", c, m)
	}
	if c, m := ErrorFields(map[string]any{"error": map[string]any{"code": 7}}); c != "" || m != "" {
		t.Errorf("odd = %q %q", c, m)
	}

	d := FlattenBackendError(map[string]any{"error": map[string]any{"code": "project_unknown", "message": "unknown project"}})
	if d["error"] != "project_unknown" || d["message"] != "unknown project" {
		t.Errorf("flatten nested = %v", d)
	}
	d = FlattenBackendError(map[string]any{"error": map[string]any{"code": "c", "message": "nested"}, "message": "flat wins"})
	if d["message"] != "flat wins" {
		t.Errorf("flat message must win: %v", d)
	}
	d = FlattenBackendError(map[string]any{"error": "already flat", "message": "m"})
	if d["error"] != "already flat" {
		t.Errorf("flat passthrough = %v", d)
	}
	d = FlattenBackendError(map[string]any{"error": map[string]any{"detail": 1}})
	if _, ok := d["error"].(map[string]any); !ok {
		t.Errorf("foreign shape must pass through: %v", d)
	}
}

func TestSetRunStatus(t *testing.T) {
	srv, got := serve(t, 200, `{}`)
	if err := SetRunStatus(context.Background(), srv.URL, "run-tok", "⚙️"); err != nil {
		t.Fatal(err)
	}
	if s := (*got)[0]; s.path != "/api/v1/agent/run/state" || s.auth != "Bearer run-tok" || s.body["state"] != "⚙️" {
		t.Fatalf("request = %+v", s)
	}
	srv2, _ := serve(t, 403, `{}`)
	if err := SetRunStatus(context.Background(), srv2.URL, "run-tok", "⚙️"); err == nil || err.Error() != "set status failed: 403" {
		t.Fatalf("refusal = %v", err)
	}
	if err := SetRunStatus(context.Background(), "http://127.0.0.1:1", "t", "x"); err == nil {
		t.Fatal("expected a transport error")
	}
}
