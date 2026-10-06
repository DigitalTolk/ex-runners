package workspace

import (
	"encoding/json"
	"errors"
	"github.com/DigitalTolk/ex-runners/internal/textutil"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

// unsetenv removes k for the duration of the test (restored afterwards).
func unsetenv(t *testing.T, k string) {
	t.Helper()
	t.Setenv(k, "")
	_ = os.Unsetenv(k)
}

// envMap turns a child environment into a map (last entry wins, as exec does).
// envMap keeps only the git/ssh/EX_ keys: a failing assertion prints the
// map, and the rest of the environment is none of the test's business.
func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_") || strings.HasPrefix(k, "EX_") || strings.HasPrefix(k, "SSH_") {
			m[k] = v
		}
	}
	return m
}

// --- workspace registry (TS: describe('workspace registry')) ---

func TestRegistryStartsEmptyAndRoundTripsProjectFacts(t *testing.T) {
	tmp := t.TempDir()
	if got := LoadRegistry(tmp).Projects; len(got) != 0 {
		t.Fatalf("fresh registry = %v", got)
	}
	next, err := UpdateProjectCommands(tmp, "dt/booking-portal", CommandUpdate{TestCmd: ptr("npm test"), DevCmd: ptr("npm run dev"), Port: ptr(5273)})
	if err != nil {
		t.Fatal(err)
	}
	if next.Dir == "" {
		t.Fatal("dir not set")
	}
	p := LoadRegistry(tmp).Projects["dt/booking-portal"]
	if p.TestCmd != "npm test" || p.DevCmd != "npm run dev" || p.Port != 5273 {
		t.Fatalf("round trip = %+v", p)
	}
	// Empty string clears; other fields survive.
	if _, err := UpdateProjectCommands(tmp, "dt/booking-portal", CommandUpdate{DevCmd: ptr("")}); err != nil {
		t.Fatal(err)
	}
	after := LoadRegistry(tmp).Projects["dt/booking-portal"]
	if after.DevCmd != "" || after.TestCmd != "npm test" || after.Port != 5273 {
		t.Fatalf("after clear = %+v", after)
	}
}

func TestRegistrySurvivesACorruptFile(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "registry.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadRegistry(tmp).Projects; len(got) != 0 {
		t.Fatalf("corrupt registry = %v", got)
	}
	if err := SaveRegistry(tmp, Registry{Version: 1, Projects: map[string]RegistryProject{}}); err != nil {
		t.Fatal(err)
	}
	if got := LoadRegistry(tmp).Projects; got == nil || len(got) != 0 {
		t.Fatalf("saved empty registry = %v", got)
	}
}

func TestRegistryFileMatchesTheTSLayout(t *testing.T) {
	tmp := t.TempDir()
	if _, err := UpdateProjectCommands(tmp, "dt/booking-portal", CommandUpdate{
		TestCmd: ptr("npm ci && npm test <unit>"), DevCmd: ptr("npm run dev"), Port: ptr(5273),
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(tmp, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := json.Marshal(filepath.Join(tmp, "dt-booking-portal"))
	// JSON.stringify(reg, null, 2) + '\n' — no HTML escaping of && or <>.
	want := `{
  "version": 1,
  "projects": {
    "dt/booking-portal": {
      "dir": ` + string(dir) + `,
      "testCmd": "npm ci && npm test <unit>",
      "devCmd": "npm run dev",
      "port": 5273
    }
  }
}
`
	if string(raw) != want {
		t.Fatalf("registry.json =\n%s\nwant\n%s", raw, want)
	}
}

func TestRegistryReadsWhatTheTSRunnerWrote(t *testing.T) {
	tmp := t.TempDir()
	ts := `{
  "version": 1,
  "projects": {
    "dt/a": {"dir": "/w/a", "defaultBranch": "main", "lastFetch": "2026-09-01T10:00:00.000Z", "setupCmd": "make", "notes": "n", "port": 8080},
    "dt/b": {"dir": "/w/b", "port": "not-a-number", "testCmd": "go test ./..."},
    "dt/c": null
  },
  "extra": true
}`
	if err := os.WriteFile(filepath.Join(tmp, "registry.json"), []byte(ts), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := LoadRegistry(tmp)
	a := reg.Projects["dt/a"]
	if a != (RegistryProject{Dir: "/w/a", DefaultBranch: "main", LastFetch: "2026-09-01T10:00:00.000Z", SetupCmd: "make", Notes: "n", Port: 8080}) {
		t.Fatalf("dt/a = %+v", a)
	}
	// A mistyped field is dropped; the rest of the entry survives.
	if b := reg.Projects["dt/b"]; b.Dir != "/w/b" || b.TestCmd != "go test ./..." || b.Port != 0 {
		t.Fatalf("dt/b = %+v", b)
	}
	if _, ok := reg.Projects["dt/c"]; ok || len(reg.Projects) != 2 || reg.Version != 1 {
		t.Fatalf("registry = %+v", reg)
	}
}

func TestRegistryOddShapesReadAsEmpty(t *testing.T) {
	for _, body := range []string{`null`, `[]`, `{"projects": []}`, `{"projects": 5}`, `{"projects": null}`, `{}`, `"x"`} {
		tmp := t.TempDir()
		if err := os.WriteFile(filepath.Join(tmp, "registry.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		reg := LoadRegistry(tmp)
		if reg.Version != 1 || reg.Projects == nil || len(reg.Projects) != 0 {
			t.Errorf("%s → %+v", body, reg)
		}
	}
}

func TestSaveRegistryZeroValueAndErrors(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "nested", "root")
	if err := SaveRegistry(tmp, Registry{}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(tmp, "registry.json"))
	if string(raw) != "{\n  \"version\": 1,\n  \"projects\": {}\n}\n" {
		t.Fatalf("zero registry = %q", raw)
	}
	// Root under a regular file: MkdirAll fails.
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o644)
	if err := SaveRegistry(filepath.Join(file, "root"), Registry{}); err == nil {
		t.Fatal("want mkdir error")
	}
	// registry.json is a directory: the write fails.
	dirRoot := t.TempDir()
	_ = os.Mkdir(filepath.Join(dirRoot, "registry.json"), 0o755)
	if err := SaveRegistry(dirRoot, Registry{}); err == nil {
		t.Fatal("want write error")
	}
}

func TestUpdateProjectCommandsTrimsClipsAndKeepsUnsetFields(t *testing.T) {
	tmp := t.TempDir()
	long := strings.Repeat("é", 600)
	next, err := UpdateProjectCommands(tmp, "dt/x", CommandUpdate{
		SetupCmd: ptr("  \ufeffmake setup\n"), TestCmd: ptr(long), Notes: ptr("\u0085note"), Port: ptr(3000),
	})
	if err != nil {
		t.Fatal(err)
	}
	// JS trim() strips U+FEFF but not U+0085; slice(0, 500) counts characters.
	if next.SetupCmd != "make setup" || next.TestCmd != strings.Repeat("é", 500) || next.Notes != "\u0085note" {
		t.Fatalf("next = %+v", next)
	}
	if next.Dir != filepath.Join(tmp, "dt-x") {
		t.Fatalf("dir = %q", next.Dir)
	}
	// Blank clears, nil leaves alone, non-positive ports are ignored.
	next, err = UpdateProjectCommands(tmp, "dt/x", CommandUpdate{SetupCmd: ptr(" \t "), Port: ptr(0)})
	if err != nil || next.SetupCmd != "" || next.TestCmd == "" || next.Notes == "" || next.Port != 3000 {
		t.Fatalf("next = %+v, %v", next, err)
	}
	next, _ = UpdateProjectCommands(tmp, "dt/x", CommandUpdate{Port: ptr(-1)})
	if next.Port != 3000 {
		t.Fatalf("port = %d", next.Port)
	}
	// An existing entry keeps its own dir.
	reg := LoadRegistry(tmp)
	reg.Projects["dt/y"] = RegistryProject{Dir: "/custom/y", DefaultBranch: "develop"}
	if err := SaveRegistry(tmp, reg); err != nil {
		t.Fatal(err)
	}
	next, _ = UpdateProjectCommands(tmp, "dt/y", CommandUpdate{DevCmd: ptr("make dev")})
	if next.Dir != "/custom/y" || next.DefaultBranch != "develop" || next.DevCmd != "make dev" {
		t.Fatalf("existing = %+v", next)
	}
}

func TestUpdateProjectCommandsReportsASaveFailure(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o644)
	next, err := UpdateProjectCommands(filepath.Join(file, "root"), "dt/x", CommandUpdate{TestCmd: ptr("make test")})
	if err == nil || next.TestCmd != "make test" {
		t.Fatalf("got %+v, %v", next, err)
	}
}

func TestLaysReposOutPerProductAndNeverEscapesTheRoot(t *testing.T) {
	j := filepath.Join
	cases := []struct{ got, want string }{
		{ProjectDir("/root", "cliffhub"), j("/root", "cliffhub")},
		{RepoDir("/root", "cliffhub", "acme/web/cliffhub-2-frontend"), j("/root", "cliffhub", "cliffhub-2-frontend")},
		{RepoDir("/root", "cliffhub", "dt/../../etc"), j("/root", "cliffhub", "etc")},
		{RepoDir("/root", "cliffhub", "//"), j("/root", "cliffhub", "repo")},
		{ProjectDir("/root", ".."), j("/root", "project")},
		{RepoDir("/root", "cliffhub", "group/.."), j("/root", "cliffhub", "repo")},
		// Go-side extras: the JS regex replaces per UTF-16 code unit.
		{ProjectDir("/root", ""), j("/root", "project")},
		{ProjectDir("/root", "..."), j("/root", "project")},
		{ProjectDir("/root", "dt/booking portal"), j("/root", "dt-booking-portal")},
		{RepoDir("/root", "café", "g/r😀x"), j("/root", "caf-", "r--x")},
	}
	for i, c := range cases {
		if c.got != c.want {
			t.Errorf("%d: got %q want %q", i, c.got, c.want)
		}
	}
}

// --- git plumbing (TS: describe('git plumbing')) ---

func TestDerivesTheGitOriginFromAConnectorBaseURL(t *testing.T) {
	if got := GitHostFromBaseURL("https://gitlab.example.com/api/v4"); got != "https://gitlab.example.com" {
		t.Fatalf("got %q", got)
	}
	if got := GitHostFromBaseURL("not a url"); got != "" {
		t.Fatalf("got %q", got)
	}
}

// webURLCases were recorded from Node's new URL() (see the TS gitHostFromBaseURL
// and portOf): input → origin, port.
var webURLCases = []struct {
	in, host string
	port     int
}{
	{"https://gitlab.example.com/api/v4", "https://gitlab.example.com", 443},
	{"not a url", "", 0},
	{"", "", 0},
	{"https://GitLab.Example.COM:443/api/v4", "https://gitlab.example.com", 443},
	{"http://h:8080/x", "http://h:8080", 8080},
	{"http://h:080/x", "http://h", 80},
	{"https://u:p@gl.ex.com/api", "https://gl.ex.com", 443},
	{"http://", "", 0},
	{"mailto:x", "mailto://", 80},
	{"localhost:5273", "localhost://", 80},
	{"ftp://x:21/", "ftp://x", 80},
	{"ftp://x/", "ftp://x", 80},
	{"ws://x:80", "ws://x", 80},
	{"wss://x:443", "wss://x", 80},
	{"http://[::1]:5273/", "http://[::1]:5273", 5273},
	{"http://[::1]/", "http://[::1]", 80},
	{"  https://x.com/a  ", "https://x.com", 443},
	{"https://x\t.com/", "https://x.com", 443},
	{"foo://Bar:99/x", "foo://Bar:99", 99},
	{"foo://bar", "foo://bar", 80},
	{"http://x:65536/", "", 0},
	{"http://x:99999999999999999999/", "", 0},
	{"http://x:65535/", "http://x:65535", 65535},
	{"file:///tmp/x", "file://", 80},
	{"http://x:/", "http://x", 80},
	{"HTTPS://X.COM", "https://x.com", 443},
	{"http:foo", "http://foo", 80},
	{"http:///x", "http://x", 80},
	{"http:a b", "", 0},
	{"http://a b/", "", 0},
	{"nope", "", 0},
	{"https://x.com:443", "https://x.com", 443},
	{"https://x.com:80", "https://x.com:80", 80},
	{"http://x:0/", "http://x:0", 0},
	{"http://[::FFFF:1]:5/", "http://[::ffff:1]:5", 5},
	{"foo://", "foo://", 80},
	{"http://x:1a/", "", 0},
}

func TestWebURLMatchesNode(t *testing.T) {
	for _, c := range webURLCases {
		if got := GitHostFromBaseURL(c.in); got != c.host {
			t.Errorf("GitHostFromBaseURL(%q) = %q, want %q", c.in, got, c.host)
		}
		if got := PortOf(c.in); got != c.port {
			t.Errorf("PortOf(%q) = %d, want %d", c.in, got, c.port)
		}
	}
}

func TestInjectsAnInlineCredentialHelperViaEnvOnlyWhenATokenExists(t *testing.T) {
	for _, k := range []string{"GIT_CONFIG_COUNT", "EX_GIT_TOKEN", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1"} {
		unsetenv(t, k)
	}
	t.Setenv("GIT_CONFIG_PARAMETERS", "'credential.helper'='store'")
	withTok := envMap(GitEnv(&GitCred{Host: "https://GitLab.example.com:8443/", Token: "glpat-x"}, nil))
	want := map[string]string{
		"GIT_TERMINAL_PROMPT": "0", "EX_GIT_TOKEN": "glpat-x", "EX_GIT_PROTOCOL": "https", "EX_GIT_HOST": "gitlab.example.com:8443",
		"GIT_CONFIG_COUNT": "4",
		// Hooks and fsmonitor off, then the empty helper resets the user's
		// helpers before ours is added.
		"GIT_CONFIG_KEY_0": "core.hooksPath", "GIT_CONFIG_VALUE_0": os.DevNull,
		"GIT_CONFIG_KEY_1": "core.fsmonitor", "GIT_CONFIG_VALUE_1": "false",
		"GIT_CONFIG_KEY_2": "credential.helper", "GIT_CONFIG_VALUE_2": "",
		"GIT_CONFIG_KEY_3": "credential.helper",
	}
	for k, v := range want {
		if withTok[k] != v {
			t.Errorf("%s = %q, want %q", k, withTok[k], v)
		}
	}
	if _, inherited := withTok["GIT_CONFIG_PARAMETERS"]; inherited {
		t.Error("an inherited GIT_CONFIG_PARAMETERS survived")
	}
	if !strings.Contains(withTok["GIT_CONFIG_VALUE_3"], "username=oauth2") {
		t.Fatalf("helper = %q", withTok["GIT_CONFIG_VALUE_3"])
	}
	// The token itself never appears in the helper text (it is read from env).
	if strings.Contains(withTok["GIT_CONFIG_VALUE_3"], "glpat-x") {
		t.Fatal("token leaked into the helper")
	}
	// Without a token git runs as it always did: no config of ours at all.
	anon := envMap(GitEnv(nil, nil))
	if anon["GIT_TERMINAL_PROMPT"] != "0" || anon["GIT_CONFIG_PARAMETERS"] != "'credential.helper'='store'" {
		t.Errorf("anon = %v", anon)
	}
	for _, k := range []string{"GIT_CONFIG_COUNT", "EX_GIT_TOKEN", "GIT_CONFIG_KEY_0"} {
		if _, ok := anon[k]; ok {
			t.Errorf("anon has %s", k)
		}
	}
	// An empty token is anonymous too.
	if _, ok := envMap(GitEnv(&GitCred{Host: "https://h"}, nil))["EX_GIT_TOKEN"]; ok {
		t.Error("empty token set EX_GIT_TOKEN")
	}
}

func TestGitEnvLayering(t *testing.T) {
	t.Setenv("EX_WS_INHERITED", "parent")
	t.Setenv("GIT_ASKPASS", "/usr/bin/some-askpass")
	t.Setenv("GIT_CONFIG_COUNT", "3")
	env := GitEnv(&GitCred{Token: "tok"}, map[string]string{"EX_WS_INHERITED": "extra", "GIT_TERMINAL_PROMPT": "1", "EX_WS_NEW": "n"})
	m := envMap(env)
	if m["EX_WS_INHERITED"] != "extra" || m["EX_WS_NEW"] != "n" {
		t.Fatalf("extra did not override: %v", m)
	}
	// The fixed git settings beat both the parent env and extra.
	if m["GIT_TERMINAL_PROMPT"] != "0" || m["GIT_ASKPASS"] != "" || m["SSH_ASKPASS"] != "" || m["GIT_CONFIG_COUNT"] != "4" {
		t.Fatalf("fixed settings lost: %v", m)
	}
	// Every key appears once.
	seen := map[string]bool{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if seen[k] {
			t.Errorf("duplicate %s", k)
		}
		seen[k] = true
	}
}

func TestReadsThePortOutOfATestURL(t *testing.T) {
	for in, want := range map[string]int{
		"http://localhost:5273/bookings": 5273,
		"http://localhost/":              80,
		"https://app.example.net/x":      443,
		"":                               0, // TS: undefined
		"nope":                           0,
	} {
		if got := PortOf(in); got != want {
			t.Errorf("PortOf(%q) = %d, want %d", in, got, want)
		}
	}
}

// --- path rendering helpers (TS: describe('path rendering helpers')) ---

func TestRootHonorsTheEnvOverrideAndDefaultsUnderHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("EX_WORKSPACE_ROOT", "/custom/ws")
	if want, _ := filepath.Abs("/custom/ws"); Root() != want {
		t.Fatalf("Root() = %q", Root())
	}
	t.Setenv("EX_WORKSPACE_ROOT", "   ")
	if Root() != filepath.Join(home, "ex-workspace") {
		t.Fatalf("blank override: Root() = %q", Root())
	}
	unsetenv(t, "EX_WORKSPACE_ROOT")
	if Root() != filepath.Join(home, "ex-workspace") {
		t.Fatalf("unset: Root() = %q", Root())
	}
	// A relative override resolves against the cwd, like path.resolve.
	t.Setenv("EX_WORKSPACE_ROOT", "rel/ws/")
	wd, _ := os.Getwd()
	if Root() != filepath.Join(wd, "rel", "ws") {
		t.Fatalf("relative: Root() = %q", Root())
	}
	// No working directory to resolve against: the cleaned override.
	orig := absPath
	t.Cleanup(func() { absPath = orig })
	absPath = func(string) (string, error) { return "", errors.New("getwd: no such file or directory") }
	if Root() != filepath.Join("rel", "ws") {
		t.Fatalf("no cwd: Root() = %q", Root())
	}
}

func TestHomeFallsBackToTheAccountDatabase(t *testing.T) {
	unsetenv(t, "HOME")
	unsetenv(t, "USERPROFILE")
	unsetenv(t, "EX_WORKSPACE_ROOT")
	orig := lookupUser
	t.Cleanup(func() { lookupUser = orig })
	lookupUser = func() (*user.User, error) { return &user.User{HomeDir: "/accounts/me"}, nil }
	if Root() != filepath.Join("/accounts/me", "ex-workspace") {
		t.Fatalf("Root() = %q", Root())
	}
	if got := ShortenHome("/accounts/me/x"); got != "~/x" {
		t.Fatalf("ShortenHome = %q", got)
	}
	// No home at all: nothing to shorten, and the root is relative.
	lookupUser = func() (*user.User, error) { return nil, errors.New("no passwd entry") }
	if got := ShortenHome("/accounts/me/x"); got != "/accounts/me/x" {
		t.Fatalf("ShortenHome = %q", got)
	}
	if Root() != "ex-workspace" {
		t.Fatalf("Root() = %q", Root())
	}
}

func TestShortenHomeSwapsTheHomePrefixForTildeAndLeavesOtherPathsAlone(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if got := ShortenHome(filepath.Join(home, "ex-workspace", "x")); got != filepath.Join("~", "ex-workspace", "x") {
		t.Fatalf("got %q", got)
	}
	if got := ShortenHome("/etc/hosts"); got != "/etc/hosts" {
		t.Fatalf("got %q", got)
	}
}

func TestTailFileReturnsTheLastBytesOfAFileOrEmptyWhenUnreadable(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "dev.log")
	_ = os.WriteFile(f, []byte("abcdefghij"), 0o644)
	if got := TailFile(f, 4); got != "ghij" {
		t.Fatalf("got %q", got)
	}
	if got := TailFile(filepath.Join(tmp, "absent.log"), 4); got != "" {
		t.Fatalf("got %q", got)
	}
	// data.slice(-bytes) semantics: 0 is everything, negative drops a prefix,
	// and counting is per character, never splitting one.
	if got := TailFile(f, 0); got != "abcdefghij" {
		t.Fatalf("0: got %q", got)
	}
	if got := TailFile(f, -3); got != "defghij" {
		t.Fatalf("-3: got %q", got)
	}
	if got := TailFile(f, 99); got != "abcdefghij" {
		t.Fatalf("99: got %q", got)
	}
	u := filepath.Join(tmp, "u.log")
	_ = os.WriteFile(u, []byte("héllo wörld"), 0o644)
	if got := TailFile(u, 5); got != "wörld" {
		t.Fatalf("utf8: got %q", got)
	}
}

func TestJSTrimMatchesStringPrototypeTrim(t *testing.T) {
	if got := textutil.JSTrim(" \ufeff\u00a0\u2028ab\u3000\t\n"); got != "ab" {
		t.Fatalf("got %q", got)
	}
	if got := textutil.JSTrim("\u0085ab\u0085"); got != "\u0085ab\u0085" {
		t.Fatalf("U+0085 is not JS whitespace: %q", got)
	}
}

func TestLoggerNeverNil(t *testing.T) {
	logger(nil)("ignored", nil) // must not panic
	called := false
	logger(func(string, map[string]any) { called = true })("x", nil)
	if !called {
		t.Fatal("logger dropped a real Logger")
	}
}
