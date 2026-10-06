package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func noop(string, map[string]any) {}

func mkConnector(mod func(*Connector)) Connector {
	c := Connector{
		Slug:        "cliffhub",
		Title:       "CliffHub",
		Description: "Team ops platform.",
		BaseURL:     "https://api.example.com",
		EnvPrefix:   "CLIFFHUB",
		Token:       "tok-secret-1",
		Files: []File{
			{Name: "index.yml", Content: "schema: 1"},
			{Name: "_catalog.tsv", Content: "people.index\tGET api/people\tread-only\tuser\tList people\t"},
			{Name: "people.yaml", Content: "service: people\nendpoints: []\n"},
			// The server injects this (or the bundle ships it); the runner
			// never generates one.
			{Name: "_USAGE.md", Content: "server-authored usage doc"},
		},
	}
	if mod != nil {
		mod(&c)
	}
	return c
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

type logLine struct {
	msg   string
	extra map[string]any
}

func recorder() (*[]logLine, func(string, map[string]any)) {
	var lines []logLine
	return &lines, func(msg string, extra map[string]any) { lines = append(lines, logLine{msg, extra}) }
}

// --- syncConnectors (ported TS cases) ---

func TestSyncWritesDocsVerbatimAndReturnsCredsAndInstructions(t *testing.T) {
	root := t.TempDir()
	setup := Sync(root, []Connector{mkConnector(nil)}, noop)

	dir := filepath.Join(root, "cliffhub")
	if got := readFile(t, filepath.Join(dir, "index.yml")); got != "schema: 1" {
		t.Errorf("index.yml = %q", got)
	}
	if !exists(filepath.Join(dir, "_catalog.tsv")) {
		t.Error("_catalog.tsv missing")
	}
	// _USAGE.md is written by the SERVER and synced verbatim — the runner has
	// no generator of its own, so the document cannot drift between the two.
	if got := readFile(t, filepath.Join(dir, "_USAGE.md")); got != "server-authored usage doc" {
		t.Errorf("_USAGE.md = %q", got)
	}

	// Credentials go to the MCP server env payload — never to disk, never to
	// the harness shell.
	var creds []map[string]string
	if err := json.Unmarshal([]byte(setup.MCPConnectors), &creds); err != nil {
		t.Fatal(err)
	}
	want := []map[string]string{{"slug": "cliffhub", "title": "CliffHub", "baseURL": "https://api.example.com", "token": "tok-secret-1"}}
	if !reflect.DeepEqual(creds, want) {
		t.Errorf("creds = %v", creds)
	}
	if setup.MCPConnectors != `[{"slug":"cliffhub","title":"CliffHub","baseURL":"https://api.example.com","token":"tok-secret-1"}]` {
		t.Errorf("payload = %s", setup.MCPConnectors)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(readFile(t, filepath.Join(dir, e.Name())), "tok-secret-1") {
			t.Errorf("token on disk in %s", e.Name())
		}
	}

	// The preamble is the PRIORITY frame: names the service, the
	// catalog-grep workflow, the tool-only rule, and forbids wandering +
	// oversharing.
	for _, w := range []string{"PRIORITY path", "/cliffhub — CliffHub", "connector_lookup", "connector_call is the only way in", "first call", "_identity.json"} {
		if !strings.Contains(setup.Instructions, w) {
			t.Errorf("instructions missing %q", w)
		}
	}
	for _, w := range []string{"read _USAGE.md", "tok-secret-1"} {
		if strings.Contains(setup.Instructions, w) {
			t.Errorf("instructions contain %q", w)
		}
	}
}

func TestSyncSkipsPathTraversalFileNames(t *testing.T) {
	root := t.TempDir()
	setup := Sync(root, []Connector{mkConnector(func(c *Connector) {
		c.Files = []File{{Name: "../evil.yml", Content: "x"}, {Name: "ok.yml", Content: "y"}, {Name: "a/b.yml", Content: "z"}, {Name: "x..y", Content: "w"}}
	})}, noop)
	if exists(filepath.Join(root, "evil.yml")) {
		t.Error("traversal written")
	}
	if !exists(filepath.Join(root, "cliffhub", "ok.yml")) {
		t.Error("ok.yml missing")
	}
	if exists(filepath.Join(root, "cliffhub", "a")) || exists(filepath.Join(root, "cliffhub", "x..y")) {
		t.Error("slash / dot-dot names must be skipped")
	}
	if !strings.Contains(setup.Instructions, "PRIORITY") {
		t.Error("instructions")
	}
}

func TestSyncPrunesRemovedFilesAndShowsMultiServiceHeader(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cliffhub")
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o755); err != nil { // directories are left alone
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stale.yaml"), []byte("removed upstream"), 0o644); err != nil {
		t.Fatal(err)
	}
	var pruned []string
	log := func(msg string, fields map[string]any) {
		if msg == "connector file pruned" {
			pruned = append(pruned, fmt.Sprint(fields["file"]))
		}
	}
	hub := mkConnector(func(c *Connector) {
		c.Slug, c.Title = "hub", "Hub"
		c.Files = []File{{Name: "_catalog.tsv", Content: "x\tGET /x"}} // no _USAGE.md → logged, not invented
	})
	withServices := mkConnector(func(c *Connector) {
		c.Files = []File{
			{Name: "index.yml", Content: "schema: 1"},
			{Name: "_USAGE.md", Content: "# Using it\n\n## Services\n- crm — customers and leads\n- people — employees\n\n## Next\nprose"},
		}
	})
	setup := Sync(root, []Connector{withServices, hub}, log)
	if !reflect.DeepEqual(pruned, []string{"stale.yaml"}) {
		t.Errorf("pruned = %v", pruned)
	}
	if exists(filepath.Join(dir, "stale.yaml")) {
		t.Error("stale.yaml not pruned")
	}
	if !exists(filepath.Join(dir, "subdir")) {
		t.Error("subdir removed")
	}
	// Two services attached → the pick-ONE-domain instruction rides the
	// preamble, and a bundle whose _USAGE.md carries a service map gets it
	// inlined.
	for _, w := range []string{"pick the ONE whose domain owns the question", "Services (route prefixes):", "- crm — customers and leads"} {
		if !strings.Contains(setup.Instructions, w) {
			t.Errorf("instructions missing %q", w)
		}
	}
}

func TestSyncToleratesFileThatCannotBePruned(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cliffhub")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stale.yaml"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := removeFile
	removeFile = func(string) error { return errors.New("EBUSY") }
	defer func() { removeFile = orig }()
	lines, log := recorder()
	setup, err := SyncChecked(root, []Connector{mkConnector(nil)}, log)
	if err != nil || setup.Instructions == "" {
		t.Fatalf("sync = %+v %v", setup, err)
	}
	if !exists(filepath.Join(dir, "stale.yaml")) {
		t.Error("locked file should linger")
	}
	for _, l := range *lines {
		if l.msg == "connector file pruned" {
			t.Error("a file that was not removed must not be logged as pruned")
		}
	}
}

func TestSyncReturnsEmptySetupForNoConnectors(t *testing.T) {
	setup := Sync(t.TempDir(), nil, noop)
	if setup != EmptySetup || setup.MCPConnectors != "" || setup.Instructions != "" {
		t.Errorf("setup = %+v", setup)
	}
}

// --- credentialHeader (ported TS cases) ---

func TestCredentialHeaderRendersEveryTemplateShape(t *testing.T) {
	cases := []struct{ tmpl, token, name, value string }{
		{"", "tok", "Authorization", "Bearer tok"},
		{"   ", "tok", "Authorization", "Bearer tok"},
		{"X-Api-Key: {token}", "mb_1", "X-Api-Key", "mb_1"},
		{"X-Api-Key", "mb_1", "X-Api-Key", "mb_1"},
		{"X-Api-Key:", "mb_1", "X-Api-Key", "mb_1"},
		{"Authorization: Token", "t", "Authorization", "Token t"},
		{"Authorization: Basic {token} extra", "b", "Authorization", "Basic b extra"},
		// Golden values from the TS.
		{"", "", "Authorization", "Bearer "},
		{"  :  ", "t", "", "t"},
		{":x", "t", "", "x t"},
		{"A: {token}{token}", "t", "A", "tt"},
		{"A:B:C", "t", "A", "B:C t"},
		{"\ufeffX-Key\u00a0:\u00a0{token}\u00a0", "t", "X-Key", "t"},
		{"Name: pre {token} post", "tok", "Name", "pre tok post"},
		{"NoColon", "t", "NoColon", "t"},
	}
	for _, c := range cases {
		if name, value := CredentialHeader(c.tmpl, c.token); name != c.name || value != c.value {
			t.Errorf("CredentialHeader(%q, %q) = (%q, %q), want (%q, %q)", c.tmpl, c.token, name, value, c.name, c.value)
		}
	}
}

func TestCredentialPayloadCarriesAuthHeader(t *testing.T) {
	setup := Sync(t.TempDir(), []Connector{
		{Slug: "metabase", Title: "Metabase", Description: "BI", BaseURL: "https://mb.example.net/api", EnvPrefix: "METABASE", Token: "mb_secret", AuthHeader: "X-Api-Key: {token}", Files: []File{{Name: "index.yml", Content: "schema: 1"}}},
		{Slug: "hub", Title: "Hub", Description: "ops", BaseURL: "https://hub.example.net", EnvPrefix: "HUB", Token: "tok", Files: []File{{Name: "index.yml", Content: "schema: 1"}}},
	}, noop)
	var creds []map[string]any
	if err := json.Unmarshal([]byte(setup.MCPConnectors), &creds); err != nil {
		t.Fatal(err)
	}
	if creds[0]["slug"] != "metabase" || creds[0]["authHeader"] != "X-Api-Key: {token}" {
		t.Errorf("metabase = %v", creds[0])
	}
	if _, ok := creds[1]["authHeader"]; ok || creds[1]["slug"] != "hub" {
		t.Errorf("hub = %v", creds[1])
	}
}

// --- fetchRunConnectors (ported TS cases) ---

func TestFetchRunConnectorsUsesRunToken(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"connectors":[{"slug":"hub"}]}`))
	}))
	defer srv.Close()
	got, err := FetchRunConnectors(context.Background(), srv.URL, "run-tok")
	if err != nil || !reflect.DeepEqual(got, []Connector{{Slug: "hub"}}) {
		t.Fatalf("got %+v %v", got, err)
	}
	if gotPath != "/api/v1/agent/run/connectors" || gotAuth != "Bearer run-tok" {
		t.Errorf("path %q auth %q", gotPath, gotAuth)
	}
}

func TestFetchRunConnectorsEmptyBodyAndRefusedFetch(t *testing.T) {
	body, status := `{}`, http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	for _, b := range []string{`{}`, `{"connectors":null}`, `null`} {
		body = b
		got, err := FetchRunConnectors(context.Background(), srv.URL, "t")
		if err != nil || got == nil || len(got) != 0 {
			t.Errorf("body %s = %#v %v", b, got, err)
		}
	}
	for _, s := range []int{http.StatusUnauthorized, http.StatusMultipleChoices, http.StatusInternalServerError} {
		status, body = s, `{"connectors":[{"slug":"x"}]}`
		_, err := FetchRunConnectors(context.Background(), srv.URL, "t")
		if err == nil || err.Error() != fmt.Sprintf("connectors fetch failed: %d", s) {
			t.Errorf("status %d: err = %v", s, err)
		}
	}
	// Any 2xx is ok.
	status, body = http.StatusAccepted, `{"connectors":[{"slug":"y","files":[{"name":"a","content":"b"}]}]}`
	got, err := FetchRunConnectors(context.Background(), srv.URL, "t")
	if err != nil || len(got) != 1 || got[0].Files[0] != (File{Name: "a", Content: "b"}) {
		t.Errorf("202 = %+v %v", got, err)
	}
	// A body that is not JSON is an error, as res.json() rejects.
	status, body = http.StatusOK, `{"connectors":`
	if _, err := FetchRunConnectors(context.Background(), srv.URL, "t"); err == nil {
		t.Error("malformed body must fail")
	}
}

func TestFetchRunConnectorsTransportFailures(t *testing.T) {
	// Bad base URL: the request cannot even be built.
	if _, err := FetchRunConnectors(context.Background(), "http://bad host\x7f", "t"); err == nil {
		t.Error("bad URL must fail")
	}
	// Nobody listening.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if _, err := FetchRunConnectors(context.Background(), url, "t"); err == nil {
		t.Error("closed server must fail")
	}
	// Body cut short of its declared length.
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte(`{"connectors":`))
	}))
	defer short.Close()
	if _, err := FetchRunConnectors(context.Background(), short.URL, "t"); err == nil {
		t.Error("truncated body must fail")
	}
	// Cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FetchRunConnectors(ctx, short.URL, "t"); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled = %v", err)
	}
}

// --- Go-port specifics: goldens from the TS and failure handling ---

func TestSyncInstructionsAndLogsMatchTS(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cliffhub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cliffhub", "stale.yaml"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var many []string
	for i := range 25 {
		many = append(many, fmt.Sprintf("- svc%d — thing %d", i, i))
	}
	lines, log := recorder()
	setup := Sync(root, []Connector{
		{Slug: "cliffhub", Title: "CliffHub", Description: "Team ops platform.", BaseURL: "https://api.example.com", EnvPrefix: "C", Token: `tok<&>"`, AuthHeader: "X-Api-Key: {token}",
			Files: []File{{Name: "index.yml", Content: "schema: 1"}, {Name: "_usage.MD", Content: "# U\n## Services\n" + strings.Join(many, "\n") + "\n## Next\n- no"}}},
		{Slug: "hub", Title: "Hub", BaseURL: "https://hub", EnvPrefix: "H", Files: []File{{Name: "_catalog.tsv", Content: "x"}}},
	}, log)

	var svc20 []string
	for i := range 20 {
		svc20 = append(svc20, fmt.Sprintf("- svc%d — thing %d", i, i))
	}
	want := "[connected services] This task uses the external service(s) below — the PRIORITY path.\nAnswer from the service API, not from chat history or the local machine.\n\nSeveral services are connected: pick the ONE whose domain owns the question and search\nonly inside it — touch a second only when the question genuinely spans both.\n\n## /cliffhub — CliffHub\nTeam ops platform.\nServices (route prefixes):\n" +
		strings.Join(svc20, "\n") +
		"\nStart with connector_lookup(connector: 'cliffhub', query: '<words from the question>') — one call\nreturns the matching endpoints, the chosen contract and its enum values. Then ONE complete\nconnector_call. The invoker's identity on this service: <ROOT>/cliffhub/_identity.json (grep the one field\nyou need). Full docs: <ROOT>/cliffhub — only for what lookup cannot answer.\n\n## /hub — Hub\n\nStart with connector_lookup(connector: 'hub', query: '<words from the question>') — one call\nreturns the matching endpoints, the chosen contract and its enum values. Then ONE complete\nconnector_call. The invoker's identity on this service: <ROOT>/hub/_identity.json (grep the one field\nyou need). Full docs: <ROOT>/hub — only for what lookup cannot answer.\n\nThe connector rules in your system prompt apply: connector_call is the only way in, first call\ncomplete, counts from meta, never cat a saved response file."
	if got := strings.ReplaceAll(setup.Instructions, root, "<ROOT>"); got != want {
		t.Errorf("instructions =\n%s\nwant\n%s", got, want)
	}
	// JSON.stringify bytes: no HTML escaping, empty authHeader omitted.
	if setup.MCPConnectors != `[{"slug":"cliffhub","title":"CliffHub","baseURL":"https://api.example.com","token":"tok<&>\"","authHeader":"X-Api-Key: {token}"},{"slug":"hub","title":"Hub","baseURL":"https://hub","token":""}]` {
		t.Errorf("payload = %s", setup.MCPConnectors)
	}
	wantLogs := []logLine{
		{"connector file pruned", map[string]any{"slug": "cliffhub", "file": "stale.yaml"}},
		{"connector synced", map[string]any{"slug": "cliffhub", "files": 2, "dir": filepath.Join(root, "cliffhub")}},
		{"connector bundle has no _USAGE.md", map[string]any{"slug": "hub"}},
		{"connector synced", map[string]any{"slug": "hub", "files": 1, "dir": filepath.Join(root, "hub")}},
	}
	if !reflect.DeepEqual(*lines, wantLogs) {
		t.Errorf("logs = %+v", *lines)
	}
	// _usage.MD (any case) is kept, never pruned.
	if !exists(filepath.Join(root, "cliffhub", "_usage.MD")) {
		t.Error("_usage.MD pruned")
	}
}

func TestSyncSingleConnectorGolden(t *testing.T) {
	root := t.TempDir()
	setup := Sync(root, []Connector{{Slug: "solo", Title: "Solo", Description: "One.", BaseURL: "b", EnvPrefix: "S", Token: "t"}}, nil)
	want := "[connected services] This task uses the external service(s) below — the PRIORITY path.\nAnswer from the service API, not from chat history or the local machine.\n\n## /solo — Solo\nOne.\nStart with connector_lookup(connector: 'solo', query: '<words from the question>') — one call\nreturns the matching endpoints, the chosen contract and its enum values. Then ONE complete\nconnector_call. The invoker's identity on this service: <ROOT>/solo/_identity.json (grep the one field\nyou need). Full docs: <ROOT>/solo — only for what lookup cannot answer.\n\nThe connector rules in your system prompt apply: connector_call is the only way in, first call\ncomplete, counts from meta, never cat a saved response file."
	if got := strings.ReplaceAll(setup.Instructions, root, "<ROOT>"); got != want {
		t.Errorf("instructions = %q", got)
	}
	if setup.MCPConnectors != `[{"slug":"solo","title":"Solo","baseURL":"b","token":"t"}]` {
		t.Errorf("payload = %s", setup.MCPConnectors)
	}
	// A nil Files list syncs as an empty bundle (the dir is still created).
	if !exists(filepath.Join(root, "solo")) {
		t.Error("dir missing")
	}
}

func TestSyncPayloadRoundTripsLineSeparators(t *testing.T) {
	// Go escapes U+2028/U+2029 where JSON.stringify writes them raw; the
	// decoded payload is identical.
	setup := Sync(t.TempDir(), []Connector{{Slug: "s", Token: "a\u2028b\u2029c"}}, noop)
	var creds []mcpCred
	if err := json.Unmarshal([]byte(setup.MCPConnectors), &creds); err != nil || creds[0].Token != "a\u2028b\u2029c" {
		t.Errorf("creds = %+v %v", creds, err)
	}
}

func TestSyncLeavesSymlinksAlone(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cliffhub")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "elsewhere"), filepath.Join(dir, "link.yaml")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	Sync(root, []Connector{mkConnector(nil)}, noop)
	if !exists(filepath.Join(dir, "link.yaml")) {
		t.Error("only regular files are pruned")
	}
}

func TestSyncFailuresDowngradeToEmptySetup(t *testing.T) {
	t.Run("mkdir", func(t *testing.T) {
		root := t.TempDir()
		blocker := filepath.Join(root, "file")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := SyncChecked(blocker, []Connector{mkConnector(nil)}, noop); err == nil {
			t.Error("SyncChecked must report the mkdir failure")
		}
		lines, log := recorder()
		if setup := Sync(blocker, []Connector{mkConnector(nil)}, log); setup != EmptySetup {
			t.Errorf("setup = %+v", setup)
		}
		if len(*lines) != 1 || (*lines)[0].msg != "connector sync failed; continuing without" || (*lines)[0].extra["error"] == "" {
			t.Errorf("logs = %+v", *lines)
		}
		// A nil logger is tolerated on the failure path too.
		if setup := Sync(blocker, []Connector{mkConnector(nil)}, nil); setup != EmptySetup {
			t.Errorf("nil log setup = %+v", setup)
		}
	})
	t.Run("write", func(t *testing.T) {
		// A directory sits where the doc goes: unwritable.
		root := t.TempDir()
		_ = os.MkdirAll(filepath.Join(root, "cliffhub", "x.md"), 0o700)
		setup, err := SyncChecked(root, []Connector{mkConnector(func(c *Connector) {
			c.Files = []File{{Name: "x.md", Content: "x"}}
		})}, noop)
		if err == nil || setup != EmptySetup {
			t.Errorf("setup = %+v err = %v", setup, err)
		}
	})
	t.Run("readdir", func(t *testing.T) {
		orig := readDir
		readDir = func(string) ([]os.DirEntry, error) { return nil, errors.New("EACCES") }
		defer func() { readDir = orig }()
		setup, err := SyncChecked(t.TempDir(), []Connector{mkConnector(nil)}, noop)
		if err == nil || err.Error() != "EACCES" || setup != EmptySetup {
			t.Errorf("setup = %+v err = %v", setup, err)
		}
	})
}

func TestSyncFilePermissions(t *testing.T) {
	// Same modes as Node's defaults: 0o666 files and 0o777 dirs, less umask.
	root := t.TempDir()
	Sync(root, []Connector{mkConnector(nil)}, noop)
	fi, err := os.Stat(filepath.Join(root, "cliffhub", "index.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o600 != 0o600 || fi.Mode().Perm()&0o111 != 0 {
		t.Errorf("file mode = %v", fi.Mode())
	}
	di, err := os.Stat(filepath.Join(root, "cliffhub"))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm()&0o700 != 0o700 {
		t.Errorf("dir mode = %v", di.Mode())
	}
}

// Slugs and file names come from the server; they become paths (and the
// slug's dir gets pruned), so the runner holds them to the server's own rules.
func TestSyncRefusesHostileSlugsAndNames(t *testing.T) {
	root := filepath.Join(t.TempDir(), "connectors")
	outside := filepath.Dir(root)
	_ = os.WriteFile(filepath.Join(outside, "precious"), []byte("x"), 0o600)
	lines, log := recorder()
	setup, err := SyncChecked(root, []Connector{
		mkConnector(func(c *Connector) { c.Slug = ".." }),
		mkConnector(func(c *Connector) { c.Slug = "a/../../x" }),
		mkConnector(func(c *Connector) { c.Slug = "Bad" }),
		mkConnector(func(c *Connector) {
			c.Files = []File{{Name: `..\evil`, Content: "x"}, {Name: "", Content: "x"}, {Name: "ok.md", Content: "fine"}}
		}),
	}, log)
	if err != nil || !strings.Contains(setup.MCPConnectors, `"slug":"cliffhub"`) || strings.Count(setup.MCPConnectors, `"slug"`) != 1 {
		t.Fatalf("setup = %+v err = %v", setup, err)
	}
	if !exists(filepath.Join(outside, "precious")) {
		t.Fatal("a hostile slug pruned files outside the connectors dir")
	}
	skipped := 0
	for _, l := range *lines {
		if l.msg == "connector skipped: invalid slug" {
			skipped++
		}
	}
	if skipped != 3 {
		t.Errorf("logs = %+v", *lines)
	}
	if exists(filepath.Join(root, "cliffhub", `..\evil`)) || !exists(filepath.Join(root, "cliffhub", "ok.md")) {
		t.Error("file name filtering")
	}
	if st, _ := os.Stat(filepath.Join(root, "cliffhub")); st.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %o", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Join(root, "cliffhub", "ok.md")); st.Mode().Perm() != 0o600 {
		t.Errorf("doc mode = %o", st.Mode().Perm())
	}
}
