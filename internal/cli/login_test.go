package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// get issues a plain request at the local callback listener.
func get(t *testing.T, port int, pathQuery string) (int, string) {
	t.Helper()
	res, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + pathQuery)
	if err != nil {
		t.Fatalf("callback request: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestNormalizeServer(t *testing.T) {
	ok := map[string]string{
		"https://ex.example.com/some/path?x=1": "https://ex.example.com",
		"  ex.example.com ":                    "https://ex.example.com",
		"http://localhost:5173/":               "http://localhost:5173",
	}
	for in, want := range ok {
		if got, good := normalizeServer(in); !good || got != want {
			t.Errorf("normalizeServer(%q) = %q %v", in, got, good)
		}
	}
	for _, bad := range []string{"", "ftp://ex.example.com", "https://", "http://bad host"} {
		if _, good := normalizeServer(bad); good {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestMachineLabelAndConnectURL(t *testing.T) {
	if got := machineLabel("Alices-MacBook-Pro.local"); got != "Alices-MacBook-Pro" {
		t.Errorf("label = %q", got)
	}
	if got := machineLabel(strings.Repeat("x", 80)); len(got) != 64 {
		t.Errorf("long label = %d", len(got))
	}
	if got := machineLabel("   "); got != "this computer" {
		t.Errorf("blank label = %q", got)
	}
	u, _ := url.Parse(connectURL("https://ex.example", 4321, "s", "c", "My Mac"))
	q := u.Query()
	if u.Scheme+"://"+u.Host+u.Path != "https://ex.example/runner/connect" || q.Get("port") != "4321" || q.Get("state") != "s" || q.Get("challenge") != "c" || q.Get("name") != "My Mac" {
		t.Errorf("connect URL = %s", u)
	}
}

func TestCallbackServer(t *testing.T) {
	cb, err := startCallbackServer("st8", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, stray := range []string{"/other?state=st8&code=c", "/callback?state=wrong&code=c", "/callback?state=st8"} {
		if code, _ := get(t, cb.port, stray); code != 400 {
			t.Errorf("%s → %d", stray, code)
		}
	}
	code, body := get(t, cb.port, "/callback?state=st8&code=the-code")
	if code != 200 || !strings.Contains(body, "ex-runner is connected") {
		t.Fatalf("callback = %d %s", code, body)
	}
	if r := <-cb.result; r.code != "the-code" {
		t.Errorf("result = %+v", r)
	}
	cb.finish(callbackResult{err: "late"}) // after a result: a no-op

	cancel, _ := startCallbackServer("st8", time.Minute)
	_, body = get(t, cancel.port, "/callback?state=st8&error=access_denied")
	if !strings.Contains(body, "Sign-in cancelled") {
		t.Errorf("cancel page = %s", body)
	}
	if r := <-cancel.result; r.err != "access_denied" {
		t.Errorf("cancel result = %+v", r)
	}

	timed, _ := startCallbackServer("st8", 10*time.Millisecond)
	if r := <-timed.result; r.err != "timeout" {
		t.Errorf("timeout result = %+v", r)
	}
}

func exchangeServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runner-tokens/exchange" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestExchangeCode(t *testing.T) {
	ctx := context.Background()
	ok := exchangeServer(t, 200, `{"token":"t","id":"rt1","label":"mac","expiresAt":"2026-11-04T00:00:00Z","userName":"Alice"}`)
	res, err := exchangeCode(ctx, http.DefaultClient, ok.URL, "c", "v")
	if err != nil || res.ID != "rt1" || res.UserName != "Alice" {
		t.Fatalf("exchange = %+v %v", res, err)
	}
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{400, `{"error":{"code":"invalid_grant","message":"runner: this sign-in link is invalid or has expired"}}`, "Sign-in failed: runner: this sign-in link is invalid or has expired."},
		{500, `{}`, "Sign-in failed: the server answered 500."},
		{502, `not json`, "Sign-in failed: the server answered 502."},
	}
	for _, c := range cases {
		_, err := exchangeCode(ctx, http.DefaultClient, exchangeServer(t, c.status, c.body).URL, "c", "v")
		var le *loginError
		if !errors.As(err, &le) || le.Error() != c.want {
			t.Errorf("%d: err = %v", c.status, err)
		}
	}
	if _, err := exchangeCode(ctx, http.DefaultClient, exchangeServer(t, 200, `{bad`).URL, "c", "v"); err == nil {
		t.Error("expected decode error")
	}
	if _, err := exchangeCode(ctx, http.DefaultClient, "http://127.0.0.1:1", "c", "v"); err == nil {
		t.Error("expected transport error")
	}
	if _, err := exchangeCode(ctx, http.DefaultClient, "http://bad host", "c", "v"); err == nil {
		t.Error("expected request error")
	}
}

// fakeBrowser "approves" by hitting the callback with what reply() builds.
func fakeBrowser(t *testing.T, reply func(q url.Values) string, seen *url.Values) func(string) {
	return func(raw string) {
		u, _ := url.Parse(raw)
		q := u.Query()
		if seen != nil {
			*seen = q
		}
		go func() {
			res, err := http.Get("http://127.0.0.1:" + q.Get("port") + reply(q))
			if err == nil {
				_ = res.Body.Close()
			}
		}()
	}
}

func TestLoginFlow(t *testing.T) {
	var gotVerifier string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotVerifier = body["verifier"]
		if body["code"] != "one-time" {
			t.Errorf("code = %q", body["code"])
		}
		_, _ = io.WriteString(w, `{"token":"t","id":"rt1","label":"mac","expiresAt":"2026-11-04T00:00:00Z","userName":"Alice"}`)
	}))
	defer srv.Close()
	var seen url.Values
	var lines []string
	creds, err := login(context.Background(), srv.URL, loginDeps{
		label:  "mac",
		out:    func(l string) { lines = append(lines, l) },
		client: http.DefaultClient, timeout: time.Minute,
		openBrowser: fakeBrowser(t, func(q url.Values) string {
			return "/callback?state=" + q.Get("state") + "&code=one-time"
		}, &seen),
	})
	if err != nil {
		t.Fatal(err)
	}
	if *creds != (Credentials{Server: srv.URL, Token: "t", TokenID: "rt1", Label: "mac", User: "Alice", ExpiresAt: "2026-11-04T00:00:00Z"}) {
		t.Errorf("creds = %+v", creds)
	}
	// The verifier the server got is the one the challenge was made from.
	if challengeFor(gotVerifier) != seen.Get("challenge") {
		t.Error("verifier does not match the challenge")
	}
	if !strings.Contains(strings.Join(lines, "\n"), "/runner/connect?") || !strings.Contains(lines[0], `"mac"`) {
		t.Errorf("lines = %v", lines)
	}
}

func TestLoginRefusals(t *testing.T) {
	noop := func(string) {}
	run := func(d loginDeps) error {
		d.client, d.out, d.label = http.DefaultClient, noop, "mac"
		if d.timeout == 0 {
			d.timeout = time.Minute
		}
		_, err := login(context.Background(), "http://127.0.0.1:1", d)
		return err
	}
	if err := run(loginDeps{openBrowser: fakeBrowser(t, func(q url.Values) string {
		return "/callback?state=" + q.Get("state") + "&error=access_denied"
	}, nil)}); err == nil || !strings.Contains(err.Error(), "Sign-in was cancelled in the browser") {
		t.Errorf("cancel = %v", err)
	}
	if err := run(loginDeps{openBrowser: noop, timeout: 10 * time.Millisecond}); err == nil || !strings.Contains(err.Error(), "Timed out waiting for you to approve") {
		t.Errorf("timeout = %v", err)
	}
	// A successful callback whose exchange then fails (server down).
	if err := run(loginDeps{openBrowser: fakeBrowser(t, func(q url.Values) string {
		return "/callback?state=" + q.Get("state") + "&code=c"
	}, nil)}); err == nil {
		t.Error("expected exchange failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := login(ctx, "http://x", loginDeps{openBrowser: noop, out: noop, timeout: time.Minute, client: http.DefaultClient}); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("interrupted = %v", err)
	}

	orig := randRead
	defer func() { randRead = orig }()
	calls := 0
	randRead = func(b []byte) (int, error) {
		calls++
		if calls == 2 { // the state, after the verifier succeeded
			return 0, errors.New("no entropy")
		}
		return orig(b)
	}
	if err := run(loginDeps{openBrowser: noop}); err == nil {
		t.Error("expected state entropy failure")
	}
	randRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	if err := run(loginDeps{openBrowser: noop}); err == nil {
		t.Error("expected verifier entropy failure")
	}
	randRead = orig
	origListen := listen
	listen = func(string, string) (net.Listener, error) { return nil, errors.New("no sockets") }
	defer func() { listen = origListen }()
	if err := run(loginDeps{openBrowser: noop}); err == nil || err.Error() != "no sockets" {
		t.Errorf("listen failure = %v", err)
	}
}
