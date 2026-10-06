package mcp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/DigitalTolk/ex-runners/internal/connectors"
)

// service is a fake external API behind a connector.
type service struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
	body []string
}

func newService(t *testing.T, status int, body string) *service {
	sv := &service{}
	sv.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		sv.mu.Lock()
		sv.reqs = append(sv.reqs, r)
		sv.body = append(sv.body, string(raw))
		sv.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(sv.srv.Close)
	return sv
}

func withConnector(slug, baseURL, token, header string) func(e *Env) {
	return func(e *Env) {
		e.Connectors[slug] = connectorCred{Slug: slug, Title: "Hub", BaseURL: baseURL, Token: token, AuthHeader: header}
	}
}

func TestConnectorCall(t *testing.T) {
	b := newBackend(t)
	sv := newService(t, 200, `{"data":[1,2]}`)
	s := testServer(t, b, withConnector("hub", sv.srv.URL+"/api/", "secret", ""))

	expect(t, s, "connector_call", map[string]any{"connector": "nope", "path": "x"}, true, `unknown connector "nope" — attached to this task: hub`)
	expect(t, s, "connector_call", map[string]any{"connector": "hub", "method": "trace", "path": "x"}, true, "unsupported method TRACE")
	expect(t, s, "connector_call", map[string]any{"connector": "hub", "path": " "}, true, "path must be a relative endpoint path")
	expect(t, s, "connector_call", map[string]any{"connector": "hub", "path": "https://evil/x"}, true, "path must be a relative endpoint path")

	expect(t, s, "connector_call", map[string]any{
		"connector": "hub", "method": "post", "path": "/people/list",
		"query": map[string]any{"per_page": float64(5), "q": "a b"}, "body": map[string]any{"x": 1},
	}, false, "HTTP 200 — POST /api/people/list?per_page=5&q=a+b (14 chars)\n{\"data\":[1,2]}")
	r := sv.reqs[0]
	if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Content-Type") != "application/json" || sv.body[0] != `{"x":1}` {
		t.Errorf("request = %v %q", r.Header, sv.body[0])
	}
	// Every raw response is published as an artifact (asynchronously).
	waitUntil(t, func() bool { return len(b.to("POST /api/v1/agent/run/artifacts")) == 1 })
	if art := b.to("POST /api/v1/agent/run/artifacts")[0].body; art["kind"] != "api_response" || !strings.HasPrefix(art["title"].(string), "POST /api/people/list?per_page=5") {
		t.Errorf("artifact = %v", art)
	}

	// Gated watcher runs are read-only.
	gated := testServer(t, b, func(e *Env) { withConnector("hub", sv.srv.URL, "s", "")(e); e.Gated = true; e.ActionMode = "notify" })
	expect(t, gated, "connector_call", map[string]any{"connector": "hub", "method": "DELETE", "path": "x"}, true, "this notify run is read-only: only GET calls are allowed")

	// Custom auth header; anonymous connectors send none.
	keyed := testServer(t, b, withConnector("mb", sv.srv.URL, "k1", "X-Api-Key: {token}"))
	do(keyed, "connector_call", map[string]any{"connector": "mb", "path": "card"})
	if got := sv.reqs[len(sv.reqs)-1].Header.Get("X-Api-Key"); got != "k1" {
		t.Errorf("X-Api-Key = %q", got)
	}
	anon := testServer(t, b, func(e *Env) {
		e.Connectors["open"] = connectorCred{Slug: "open", Title: "Open", BaseURL: sv.srv.URL}
	})
	do(anon, "connector_call", map[string]any{"connector": "open", "path": "x"})
	if got := sv.reqs[len(sv.reqs)-1].Header.Get("Authorization"); got != "" {
		t.Errorf("anonymous call sent %q", got)
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		<-time_after()
	}
	t.Fatal("condition never met")
}

func TestConnectorCallStatusesAndLargeBodies(t *testing.T) {
	b := newBackend(t)
	rejected := newService(t, 401, `{"error":"bad token"}`)
	s := testServer(t, b, withConnector("hub", rejected.srv.URL, "s", ""))
	expect(t, s, "connector_call", map[string]any{"connector": "hub", "path": "x"}, true, "The stored Hub credential was rejected — tell the invoker to reconnect the hub connector. Do not retry.")

	missing := newService(t, 404, strings.Repeat("e", 5000))
	s = testServer(t, b, withConnector("hub", missing.srv.URL, "s", ""))
	text, isErr := do(s, "connector_call", map[string]any{"connector": "hub", "path": "x"})
	if !isErr || !strings.HasPrefix(text, "HTTP 404") || len(text) > 4100 {
		t.Errorf("404 = err=%v len=%d", isErr, len(text))
	}

	// Large JSON is saved to a file and summarized by shape.
	items := []string{}
	for i := 0; i < 60; i++ {
		items = append(items, `{"id":`+itoaT(i)+`,"name":"person `+itoaT(i)+`","dept":"ops"}`)
	}
	big := `{"meta":{"total":60,"page":1},"data":[` + strings.Join(items, ",") + `],"ok":true}`
	large := newService(t, 200, big)
	s = testServer(t, b, withConnector("hub", large.srv.URL, "s", ""))
	text, isErr = do(s, "connector_call", map[string]any{"connector": "hub", "path": "people"})
	if isErr || !strings.Contains(text, "→ saved: ") || !strings.Contains(text, `meta: {"total":60,"page":1}`) || !strings.Contains(text, "data: array[60] — item keys: id,name,dept") || !strings.Contains(text, "ok: true") {
		t.Errorf("saved summary = %s", text)
	}
	saved := strings.TrimSpace(strings.SplitN(strings.SplitN(text, "→ saved: ", 2)[1], "\n", 2)[0])
	if raw, _ := os.ReadFile(saved); string(raw) != big {
		t.Errorf("saved file content mismatch (%s)", saved)
	}
	if filepath.Base(saved) != "hub-1.json" {
		t.Errorf("saved name = %s", filepath.Base(saved))
	}

	// Large non-JSON passes through whole (the spill layer pages it).
	plain := newService(t, 200, strings.Repeat("p", 2000))
	s = testServer(t, b, withConnector("hub", plain.srv.URL, "s", ""))
	if text, _ := do(s, "connector_call", map[string]any{"connector": "hub", "path": "x"}); !strings.HasSuffix(text, strings.Repeat("p", 2000)) {
		t.Error("non-JSON body not passed through")
	}
	// An unwritable workdir falls back to passing the JSON through.
	s = testServer(t, b, withConnector("hub", large.srv.URL, "s", ""))
	s.env.WorkDir = filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(s.env.WorkDir, []byte("x"), 0o600)
	if text, _ := do(s, "connector_call", map[string]any{"connector": "hub", "path": "x"}); strings.Contains(text, "saved:") {
		t.Error("saved into an unwritable workdir")
	}
	ro := t.TempDir()
	_ = os.MkdirAll(filepath.Join(ro, "responses"), 0o500)
	s.env.WorkDir = ro
	if text, _ := do(s, "connector_call", map[string]any{"connector": "hub", "path": "x"}); strings.Contains(text, "saved:") {
		t.Error("saved into a read-only responses dir")
	}
	_ = os.Chmod(filepath.Join(ro, "responses"), 0o700)

	// An empty body still leaves an audit artifact.
	empty := newService(t, 204, "")
	be := newBackend(t)
	s = testServer(t, be, withConnector("hub", empty.srv.URL, "s", ""))
	expect(t, s, "connector_call", map[string]any{"connector": "hub", "path": "x"}, false, "HTTP 204")
	waitUntil(t, func() bool { return len(be.to("POST /api/v1/agent/run/artifacts")) == 1 })
	if a := be.to("POST /api/v1/agent/run/artifacts")[0].body; a["content"] != "(empty body)" {
		t.Errorf("artifact = %v", a)
	}

	// Unreachable service and an unparseable base URL.
	s = testServer(t, b, withConnector("hub", "http://127.0.0.1:1", "s", ""))
	expect(t, s, "connector_call", map[string]any{"connector": "hub", "path": "x"}, true, "connector_call failed before reaching hub")
	s = testServer(t, b, withConnector("hub", "http://bad host", "s", ""))
	expect(t, s, "connector_call", map[string]any{"connector": "hub", "path": "x"}, true, "connector_call failed before reaching hub")
	s = testServer(t, b, withConnector("hub", "http://h:port/", "s", ""))
	expect(t, s, "connector_call", map[string]any{"connector": "hub", "path": "x"}, true, "connector_call failed before reaching hub")
}

func TestShapeSummary(t *testing.T) {
	cases := map[string]string{
		`[{"a":1,"b":2},{"a":3}]`: "array[2] — item keys: a,b",
		`[1,2,3]`:                 "array[3]",
		`[]`:                      "array[0]",
		`{"list":[],"o":{"k":"v"},"n":null,"s":"x"}`: "list: array[0]\no: {\"k\":\"v\"}\nn: null\ns: \"x\"",
		`{"big":{` + bigObj(60) + `}}`:               "big: object(60 keys)",
	}
	for in, want := range cases {
		if got, isJSON := shapeSummary([]byte(in)); !isJSON || got != want {
			t.Errorf("shapeSummary(%s) = %q, want %q", in, got, want)
		}
	}
	for _, notShape := range []string{`not json`, `"str"`, `42`} {
		if _, isJSON := shapeSummary([]byte(notShape)); isJSON {
			t.Errorf("%s summarized", notShape)
		}
	}
	keys := []string{}
	for i := 0; i < 30; i++ {
		keys = append(keys, `"k`+itoaT(i)+`":1`)
	}
	got, _ := shapeSummary([]byte(`[{` + strings.Join(keys, ",") + `}]`))
	if strings.Count(got, ",") != 24 {
		t.Errorf("item keys not capped at 25: %s", got)
	}
}

func bigObj(n int) string {
	parts := []string{}
	for i := 0; i < n; i++ {
		parts = append(parts, `"key`+itoaT(i)+`":"value value"`)
	}
	return strings.Join(parts, ",")
}

func TestUseConnector(t *testing.T) {
	orig := fetchRunConnectors
	defer func() { fetchRunConnectors = orig }()
	rows := []connectors.Connector{{
		Slug: "hub", Title: "Hub", BaseURL: "https://hub/api", Token: "t",
		Files: []connectors.File{{Name: "_USAGE.md", Content: "## Services\n- `people` — staff\n- `leave` — absences\n"}},
	}}
	fetchRunConnectors = func(context.Context, string, string) ([]connectors.Connector, error) { return rows, nil }

	b := newBackend(t)
	s := testServer(t, b, nil)
	expect(t, s, "use_connector", map[string]any{"connector": "hub"}, true, "use_connector requires connector and reason")

	// Attached straight away.
	b.on("POST /api/v1/agent/run/use-connector", answer{200, `{"status":"attached","title":"Hub"}`})
	text, isErr := do(s, "use_connector", map[string]any{"connector": "hub", "reason": "staff data"})
	if isErr || !strings.HasPrefix(text, "attached: hub (Hub).") || !strings.Contains(text, "Next: connector_lookup(connector: 'hub'") {
		t.Errorf("attached = %s", text)
	}
	expect(t, s, "use_connector", map[string]any{"connector": "hub", "reason": "again"}, false, "hub is already attached — call connector_call directly")
	// Attached: lookup reaches the docs (this fixture ships no catalog).
	expect(t, s, "connector_lookup", map[string]any{"connector": "hub", "query": "people"}, true, "no catalog found for hub")

	// Consent gate raised by the server, approved → attached on the second call.
	b2 := newBackend(t)
	b2.on("POST /api/v1/agent/run/use-connector", answer{200, `{"status":"ask","approvalID":"g1","title":"Hub"}`}, answer{200, `{"status":"attached"}`})
	b2.on("GET /api/v1/agent/run/approvals/g1", answer{200, `{"state":"approved"}`})
	s2 := testServer(t, b2, nil)
	expect(t, s2, "use_connector", map[string]any{"connector": "hub", "reason": "r"}, false, "attached: hub (Hub).")
	if second := b2.to("POST /api/v1/agent/run/use-connector")[1].body; second["approvalID"] != "g1" {
		t.Errorf("second attach = %v", second)
	}
	// An old server (no id): we raise the card ourselves; denied.
	b3 := newBackend(t)
	b3.on("POST /api/v1/agent/run/use-connector", answer{200, `{"status":"ask"}`})
	approvalFlow(b3, answer{200, `{"state":"denied"}`})
	expect(t, testServer(t, b3, nil), "use_connector", map[string]any{"connector": "hub", "reason": "r"}, true, "the invoker did not approve using hub")
	if card := b3.to("POST /api/v1/agent/run/approvals")[0].body; card["summary"] != "Use the hub connector (hub) for this task: r" {
		t.Errorf("own card = %v", card)
	}
	// Other outcomes.
	for _, c := range []struct {
		answers []answer
		polls   []answer
		want    string
	}{
		{[]answer{{403, `{}`}}, nil, "not permitted"},
		{[]answer{{200, `{"status":"unavailable","message":"not installed"}`}}, nil, "not installed — do NOT try to reach this service another way"},
		{[]answer{{200, `{"status":"nope"}`}}, nil, "connector not available — do NOT try"},
		{[]answer{{200, `{"status":"ask","approvalID":"g1","summary":"card"}`}}, []answer{{500, `{}`}}, "failed"},
		{[]answer{{200, `{"status":"ask","approvalID":"g1"}`}, {500, `{}`}}, []answer{{200, `{"state":"approved"}`}}, "failed"},
	} {
		bx := newBackend(t)
		bx.on("POST /api/v1/agent/run/use-connector", c.answers...)
		if c.polls != nil {
			bx.on("GET /api/v1/agent/run/approvals/g1", c.polls...)
		}
		expect(t, testServer(t, bx, nil), "use_connector", map[string]any{"connector": "hub", "reason": "r"}, true, c.want)
	}
	// Attached, but the doc fetch fails.
	fetchRunConnectors = func(context.Context, string, string) ([]connectors.Connector, error) {
		return nil, errors.New("timeout")
	}
	b4 := newBackend(t)
	b4.on("POST /api/v1/agent/run/use-connector", answer{200, `{"status":"attached"}`})
	expect(t, testServer(t, b4, nil), "use_connector", map[string]any{"connector": "hub", "reason": "r"}, true, "attached, but doc sync failed: timeout — retry use_connector once")
	// Attached, but the docs can't be written beside the workdir.
	fetchRunConnectors = func(context.Context, string, string) ([]connectors.Connector, error) { return rows, nil }
	blocked := testServer(t, b4, func(e *Env) {
		e.WorkDir = filepath.Join(t.TempDir(), "file")
		_ = os.WriteFile(e.WorkDir, []byte("x"), 0o600)
	})
	expect(t, blocked, "use_connector", map[string]any{"connector": "hub", "reason": "r"}, true, "attached, but doc sync failed: ")
	if _, attached := blocked.connector("hub"); attached {
		t.Error("armed connector_call despite a failed sync")
	}
}

func TestConnectorLookupRequiresAttachment(t *testing.T) {
	b := newBackend(t)
	expect(t, testServer(t, b, nil), "connector_lookup", map[string]any{"connector": "hub"}, true, `"hub" is not attached to this task (attached: none) — call use_connector first`)
	if got := attachedResult("hub", "Hub", t.TempDir()); strings.Contains(got, "Services") {
		t.Errorf("no usage doc → no services block: %s", got)
	}
	dir := t.TempDir()
	usage := "## Services\n"
	for i := 0; i < 25; i++ {
		usage += "- `svc" + itoaT(i) + "` — x\n"
	}
	_ = os.WriteFile(filepath.Join(dir, "_USAGE.md"), []byte(usage), 0o600)
	got := attachedResult("hub", "Hub", dir)
	if !strings.Contains(got, "Services (route prefixes") {
		t.Errorf("services block missing: %s", got)
	}
}

// A custom credential header (Metabase's X-Api-Key) must not follow a
// redirect to another host; Go only strips Authorization by itself.
func TestConnectorCallKeepsTheCredentialOnItsHost(t *testing.T) {
	var mu sync.Mutex
	got := map[string]string{}
	record := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			got[name] = r.Header.Get("X-Api-Key")
			mu.Unlock()
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}
	other := httptest.NewServer(record("other"))
	defer other.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/away", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/landed", http.StatusFound)
	})
	mux.HandleFunc("/home", record("home"))
	mux.HandleFunc("/near", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/home", http.StatusFound) })
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", http.StatusFound) })
	own := httptest.NewServer(mux)
	defer own.Close()

	seen := func(name string) string {
		mu.Lock()
		defer mu.Unlock()
		return got[name]
	}

	s := testServer(t, newBackend(t), withConnector("mb", own.URL, "k1", "X-Api-Key: {token}"))
	expect(t, s, "connector_call", map[string]any{"connector": "mb", "path": "away"}, false, "HTTP 200")
	expect(t, s, "connector_call", map[string]any{"connector": "mb", "path": "near"}, false, "HTTP 200")
	expect(t, s, "connector_call", map[string]any{"connector": "mb", "path": "loop"}, true, "stopped after 10 redirects")
	if k := seen("other"); k != "" {
		t.Errorf("the key followed a redirect to another host: %q", k)
	}
	if k := seen("home"); k != "k1" {
		t.Errorf("a same-host redirect lost the key: %q", k)
	}
}

func TestConnectorCallRefusesAHugeBody(t *testing.T) {
	orig := maxConnectorBody
	maxConnectorBody = 16
	defer func() { maxConnectorBody = orig }()
	big := newService(t, 200, strings.Repeat("x", 17))
	s := testServer(t, newBackend(t), withConnector("hub", big.srv.URL, "s", ""))
	expect(t, s, "connector_call", map[string]any{"connector": "hub", "path": "x"}, true, "the hub response is larger than 0 MB — narrow the query")
	exact := newService(t, 200, strings.Repeat("x", 16))
	s = testServer(t, newBackend(t), withConnector("hub", exact.srv.URL, "s", ""))
	expect(t, s, "connector_call", map[string]any{"connector": "hub", "path": "x"}, false, "(16 chars)")
}

// The server drops a connector whose credential failed its check; the agent
// is told why instead of a false "attached".
func TestUseConnectorWithheldByTheServer(t *testing.T) {
	orig := fetchRunConnectors
	defer func() { fetchRunConnectors = orig }()
	fetchRunConnectors = func(context.Context, string, string) ([]connectors.Connector, error) {
		return []connectors.Connector{{Slug: "hub", Title: "Hub", BaseURL: "https://hub/api", Token: "t"}}, nil
	}
	b := newBackend(t)
	b.on("POST /api/v1/agent/run/use-connector", answer{200, `{"status":"attached","title":"Metabase"}`})
	s := testServer(t, b, nil)
	expect(t, s, "use_connector", map[string]any{"connector": "metabase", "reason": "dashboards"}, true,
		"metabase was approved, but its stored credential is expired or unavailable, so it can't be used in this run — tell the invoker to reconnect Metabase on the Connectors page")
	if _, armed := s.connector("hub"); !armed {
		t.Error("the connectors the server did send must still be armed")
	}
}
