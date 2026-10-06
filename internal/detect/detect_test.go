package detect

import (
	"encoding/json"
	"errors"
	"github.com/DigitalTolk/ex-runners/internal/textutil"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

// --- helpers ---

type logLine struct {
	msg   string
	extra map[string]any
}

type recorder struct {
	mu    sync.Mutex
	lines []logLine
}

func (r *recorder) log(msg string, extra map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, logLine{msg, extra})
}

func (r *recorder) msgs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, l := range r.lines {
		out = append(out, l.msg)
	}
	return out
}

// setup isolates every seam and the PATH cache, and points HOME at a temp
// dir so the home-relative fallback dirs are predictable.
func setup(t *testing.T) (home string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake executables are /bin/sh scripts")
	}
	savedGOOS, savedShell := goos, defaultShell
	savedShellTO, savedVersionTO, savedWait := shellTimeout, versionTimeout, waitDelay
	savedSys, savedUser := systemBinDirs, currentUser
	t.Cleanup(func() {
		goos, defaultShell = savedGOOS, savedShell
		shellTimeout, versionTimeout, waitDelay = savedShellTO, savedVersionTO, savedWait
		systemBinDirs, currentUser = savedSys, savedUser
		cachedPath = ""
	})
	cachedPath = ""
	// macOS scans each freshly written script on its first exec; on a busy
	// machine that alone can outlast the production timeouts. Tests about
	// timing out set their own.
	shellTimeout, versionTimeout = time.Minute, time.Minute
	systemBinDirs = []string{"/sys-a", "/sys-b"}
	home = filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	return home
}

func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func homeDirs(home string) []string {
	return []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "bin"),
		filepath.Join(home, ".claude", "local"),
		filepath.Join(home, ".codex", "bin"),
	}
}

func joinPath(parts ...[]string) string {
	var all []string
	for _, p := range parts {
		all = append(all, p...)
	}
	return strings.Join(all, string(os.PathListSeparator))
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

// --- LoginShellPath ---

func TestLoginShellPathAsksTheShellAndUnionsCommonDirs(t *testing.T) {
	home := setup(t)
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	t.Setenv("REC", calls)
	t.Setenv("SHELL", script(t, dir, "fake-shell", `printf '%s|' "$@" >> "$REC"
printf '%s\n' "$DISABLE_AUTO_UPDATE" >> "$REC"
printf '  /fake/one:/fake/two::/fake/one:/sys-b\n\n'
`))
	rec := &recorder{}
	got := LoginShellPath(rec.log)
	want := joinPath([]string{"/fake/one", "/fake/two", "/sys-b", "/sys-a"}, homeDirs(home))
	if got != want {
		t.Errorf("LoginShellPath = %q, want %q", got, want)
	}
	// Cached: a second call neither re-spawns the shell nor changes.
	if again := LoginShellPath(rec.log); again != got {
		t.Errorf("second call = %q", again)
	}
	if c := readFile(t, calls); c != "-ilc|echo -n \"$PATH\"|true\n" {
		t.Errorf("shell invocations = %q, want exactly one -ilc probe with DISABLE_AUTO_UPDATE=true", c)
	}
	if len(rec.lines) != 0 {
		t.Errorf("unexpected logs: %v", rec.msgs())
	}
}

func TestLoginShellPathFallsBackToProcessPath(t *testing.T) {
	for _, c := range []struct {
		name    string
		body    string // fake shell script; "" means SHELL points nowhere
		logged  bool
		errPart string
	}{
		{"shell exits non-zero", "printf '/ignored'\nexit 3\n", true, "exit status 3"},
		{"shell missing", "", true, "no such file or directory"},
		{"shell prints only whitespace", "printf '  \\n\\t'\n", false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := setup(t)
			t.Setenv("PATH", "/p1::/p2")
			shell := filepath.Join(t.TempDir(), "absent-shell")
			if c.body != "" {
				shell = script(t, t.TempDir(), "fake-shell", c.body)
			}
			t.Setenv("SHELL", shell)
			rec := &recorder{}
			got := LoginShellPath(rec.log)
			if want := joinPath([]string{"/p1", "/p2", "/sys-a", "/sys-b"}, homeDirs(home)); got != want {
				t.Errorf("LoginShellPath = %q, want %q", got, want)
			}
			if !c.logged {
				if len(rec.lines) != 0 {
					t.Errorf("unexpected logs: %v", rec.msgs())
				}
				return
			}
			if len(rec.lines) != 1 || rec.lines[0].msg != "login shell PATH probe failed; using process PATH" {
				t.Fatalf("logs = %v", rec.msgs())
			}
			if e, _ := rec.lines[0].extra["error"].(string); !strings.Contains(e, c.errPart) {
				t.Errorf("error detail = %q, want it to mention %q", e, c.errPart)
			}
		})
	}
}

// A hung profile must not hang startup — even when a background job it
// started keeps the output pipe open after the shell is killed.
func TestLoginShellPathTimesOut(t *testing.T) {
	home := setup(t)
	shellTimeout, waitDelay = 100*time.Millisecond, 100*time.Millisecond
	t.Setenv("PATH", "/p")
	t.Setenv("SHELL", script(t, t.TempDir(), "hung-shell", "/bin/sleep 3 &\n/bin/sleep 3\n"))
	rec := &recorder{}
	start := time.Now()
	got := LoginShellPath(rec.log)
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("probe took %v; the timeout did not hold", el)
	}
	if want := joinPath([]string{"/p", "/sys-a", "/sys-b"}, homeDirs(home)); got != want {
		t.Errorf("LoginShellPath = %q, want %q", got, want)
	}
	if len(rec.lines) != 1 {
		t.Fatalf("logs = %v", rec.msgs())
	}
	if e, _ := rec.lines[0].extra["error"].(string); !strings.Contains(e, "timed out after 100ms") {
		t.Errorf("error detail = %q", e)
	}
}

func TestLoginShellPathDefaultShell(t *testing.T) {
	setup(t)
	t.Setenv("SHELL", "")
	defaultShell = script(t, t.TempDir(), "zsh", "printf '/from/default'\n")
	if got := LoginShellPath(nil); !strings.HasPrefix(got, "/from/default"+string(os.PathListSeparator)) {
		t.Errorf("LoginShellPath = %q, want the default shell's answer first", got)
	}
}

func TestLoginShellPathOnWindowsIsTheProcessPath(t *testing.T) {
	setup(t)
	goos = "windows"
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	t.Setenv("REC", calls)
	t.Setenv("SHELL", script(t, dir, "fake-shell", "echo x >> \"$REC\"\n"))
	t.Setenv("PATH", `C:\bin;D:\tools`)
	if got := LoginShellPath(nil); got != `C:\bin;D:\tools` {
		t.Errorf("LoginShellPath = %q, want the process PATH verbatim (no union)", got)
	}
	t.Setenv("PATH", `E:\other`)
	if got := LoginShellPath(nil); got != `C:\bin;D:\tools` {
		t.Errorf("not cached: %q", got)
	}
	if readFile(t, calls) != "" {
		t.Error("the shell was spawned on windows")
	}
	// An empty PATH is not cached (the TS cache test is truthiness).
	cachedPath = ""
	t.Setenv("PATH", "")
	if got := LoginShellPath(nil); got != "" {
		t.Errorf("empty PATH = %q", got)
	}
	t.Setenv("PATH", `F:\later`)
	if got := LoginShellPath(nil); got != `F:\later` {
		t.Errorf("after empty = %q", got)
	}
}

func TestLoginShellPathProbesOnceUnderConcurrency(t *testing.T) {
	setup(t)
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	t.Setenv("REC", calls)
	t.Setenv("SHELL", script(t, dir, "fake-shell", "echo x >> \"$REC\"\n/bin/sleep 0.2\nprintf '/once'\n"))
	var wg sync.WaitGroup
	results := make([]string, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = LoginShellPath(protocol.NopLogger)
		}()
	}
	wg.Wait()
	for _, r := range results {
		if r != results[0] || !strings.HasPrefix(r, "/once") {
			t.Fatalf("results = %q", results)
		}
	}
	if n := strings.Count(readFile(t, calls), "x"); n != 1 {
		t.Errorf("shell spawned %d times, want 1", n)
	}
}

func TestCommonBinDirsHome(t *testing.T) {
	setup(t)
	t.Setenv("HOME", "")
	currentUser = func() (*user.User, error) { return &user.User{HomeDir: "/acct"}, nil }
	if got := commonBinDirs(); strings.Join(got, " ") != strings.Join(append([]string{"/sys-a", "/sys-b"}, homeDirs("/acct")...), " ") {
		t.Errorf("account-database home: %q", got)
	}
	currentUser = func() (*user.User, error) { return nil, errors.New("no account") }
	if got := commonBinDirs(); strings.Join(got, " ") != "/sys-a /sys-b" {
		t.Errorf("unknown home: %q", got)
	}
	// systemBinDirs itself is never appended to.
	if len(systemBinDirs) != 2 {
		t.Errorf("systemBinDirs mutated: %q", systemBinDirs)
	}
}

// --- FindExecutable ---

func TestFindExecutable(t *testing.T) {
	setup(t)
	a, b := t.TempDir(), t.TempDir()
	// Not executable in a, executable in b: b wins.
	if err := os.WriteFile(filepath.Join(a, "claude"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	script(t, b, "claude", "exit 0\n")
	sp := joinPath([]string{"", a, "", b + "/"})
	if got := FindExecutable("claude", sp); got != filepath.Join(b, "claude") {
		t.Errorf("FindExecutable = %q", got)
	}
	if got := FindExecutable("codex", sp); got != "" {
		t.Errorf("missing binary = %q, want \"\"", got)
	}
	if got := FindExecutable("claude", ""); got != "" {
		t.Errorf("empty search path = %q", got)
	}
	// Like fs.accessSync(X_OK), a searchable directory counts (its --version
	// then fails and is reported as found-but-broken).
	if err := os.Mkdir(filepath.Join(a, "codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := FindExecutable("codex", sp); got != filepath.Join(a, "codex") {
		t.Errorf("directory = %q", got)
	}
}

func TestFindExecutableWindowsExtensions(t *testing.T) {
	setup(t)
	goos = "windows"
	dir := t.TempDir()
	for _, n := range []string{"tool", "tool.bat", "tool.cmd"} {
		script(t, dir, n, "exit 0\n")
	}
	for _, c := range []struct{ remove, want string }{
		{"", "tool.cmd"}, // .exe absent, .cmd before .bat
		{"tool.cmd", "tool.bat"},
		{"tool.bat", "tool"},
	} {
		if c.remove != "" {
			if err := os.Remove(filepath.Join(dir, c.remove)); err != nil {
				t.Fatal(err)
			}
		}
		if got := FindExecutable("tool", dir); got != filepath.Join(dir, c.want) {
			t.Errorf("after removing %q: %q, want %q", c.remove, got, c.want)
		}
	}
}

// --- DetectHarnesses ---

func TestDetectHarnesses(t *testing.T) {
	setup(t)
	bin := t.TempDir()
	seen := filepath.Join(t.TempDir(), "seen")
	t.Setenv("SEEN", seen)
	// Bedrock is no longer advertised, whatever AWS credentials exist.
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	t.Setenv("AWS_PROFILE", "default")
	claude := script(t, bin, "claude", `printf '%s|%s' "$1" "$PATH" > "$SEEN"
printf '\n  2.1.4 (Claude Code)\nsecond line\n'
`)
	codex := script(t, bin, "codex", "echo boom >&2\nexit 1\n")
	cachedPath = bin // the login-shell PATH, already resolved

	rec := &recorder{}
	got := DetectHarnesses(rec.log)
	want := []Harness{{RunnerHarness: protocol.RunnerHarness{Name: "claude", Version: "2.1.4 (Claude Code)", Authed: true}, Path: claude}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("DetectHarnesses = %+v, want %+v", got, want)
	}
	// --version ran with the resolved PATH.
	if s := readFile(t, seen); s != "--version|"+bin {
		t.Errorf("claude saw %q", s)
	}
	msgs := rec.msgs()
	if len(msgs) != 2 || msgs[0] != "harness claude: "+claude || msgs[1] != "harness codex: found at "+codex+" but --version failed" {
		t.Fatalf("logs = %q", msgs)
	}
	if e, _ := rec.lines[1].extra["error"].(string); !strings.Contains(e, "exit status 1") {
		t.Errorf("codex error detail = %q", e)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `[{"name":"claude","version":"2.1.4 (Claude Code)","authed":true,"path":"`+claude+`"}]` {
		t.Errorf("JSON = %s", b)
	}
}

func TestDetectHarnessesNoneInstalled(t *testing.T) {
	setup(t)
	cachedPath = t.TempDir()
	rec := &recorder{}
	got := DetectHarnesses(rec.log)
	if got == nil || len(got) != 0 {
		t.Fatalf("DetectHarnesses = %#v, want an empty (non-nil) list", got)
	}
	if b, _ := json.Marshal(got); string(b) != "[]" {
		t.Errorf("JSON = %s", b)
	}
	if msgs := rec.msgs(); strings.Join(msgs, "\n") != "harness claude: not found on PATH\nharness codex: not found on PATH" {
		t.Errorf("logs = %q", msgs)
	}
}

func TestDetectHarnessesVersionTimeout(t *testing.T) {
	setup(t)
	versionTimeout, waitDelay = 100*time.Millisecond, 100*time.Millisecond
	bin := t.TempDir()
	claude := script(t, bin, "claude", "/bin/sleep 3\n")
	cachedPath = bin
	rec := &recorder{}
	start := time.Now()
	if got := DetectHarnesses(rec.log); len(got) != 0 {
		t.Fatalf("DetectHarnesses = %+v", got)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("probe took %v; the timeout did not hold", el)
	}
	if msgs := rec.msgs(); len(msgs) != 2 || msgs[0] != "harness claude: found at "+claude+" but --version failed" {
		t.Fatalf("logs = %q", msgs)
	}
	if e, _ := rec.lines[0].extra["error"].(string); !strings.Contains(e, "timed out after 100ms") {
		t.Errorf("claude error detail = %q", e)
	}
}

func TestDetectHarnessesVersionIsFirstTrimmedLine(t *testing.T) {
	setup(t)
	bin := t.TempDir()
	// A BOM and CRLF: JavaScript's trim drops the BOM; split("\n") keeps the CR.
	bom := string(rune(0xfeff))
	script(t, bin, "codex", "printf '"+bom+"codex-cli 0.9\\r\\nmore\\r\\n'\n")
	// Empty output is a working CLI with an empty version.
	script(t, bin, "claude", "exit 0\n")
	cachedPath = bin
	got := DetectHarnesses(nil)
	if len(got) != 2 || got[0].Name != "claude" || got[0].Version != "" || got[1].Name != "codex" || got[1].Version != "codex-cli 0.9\r" {
		t.Fatalf("DetectHarnesses = %+v", got)
	}
	if b, _ := json.Marshal(got[0]); !strings.HasPrefix(string(b), `{"name":"claude","authed":true,`) {
		t.Errorf("empty version JSON = %s (version is omitempty, like the TS optional field)", b)
	}
}

// End to end: an empty cache resolves the PATH through the shell first.
func TestDetectHarnessesResolvesLoginShellPath(t *testing.T) {
	setup(t)
	bin := t.TempDir()
	script(t, bin, "codex", "echo codex-cli 1.0\n")
	t.Setenv("SHELL", script(t, t.TempDir(), "fake-shell", "printf '%s' \""+bin+"\"\n"))
	got := DetectHarnesses(nil) // a nil logger is tolerated
	if len(got) != 1 || got[0].Name != "codex" || got[0].Version != "codex-cli 1.0" || got[0].Path != filepath.Join(bin, "codex") {
		t.Fatalf("DetectHarnesses = %+v", got)
	}
}

func TestTrimJS(t *testing.T) {
	ws := string([]rune{0xfeff, 0xa0, 0x2028, 0x2029, 0x3000, 0x2000, 0x200a, 0x1680, 0x202f, 0x205f, '\v', '\f'})
	if got := textutil.JSTrim(ws + " v1 " + ws); got != "v1" {
		t.Errorf("trimJS = %q", got)
	}
	nel := string(rune(0x85))
	if got := textutil.JSTrim(nel + "v1" + nel); got != nel+"v1"+nel {
		t.Errorf("trimJS stripped U+0085: %q", got)
	}
}
