// Package detect finds the agent CLIs the runner can drive, with the
// launchd-PATH fix (plan-v2 §6): a runner started as a background service
// (`brew services`, launchd, systemd) inherits a minimal PATH and will not see
// CLIs installed via a shell-profile-managed prefix (homebrew, nvm, volta,
// bun). We resolve the user's interactive-shell PATH once and search that.
//
// Only the claude and codex CLIs are detected: Bedrock agents run on the
// server, so the runner no longer advertises a "bedrock" harness.
package detect

import (
	"context"
	"fmt"
	"github.com/DigitalTolk/ex-runners/internal/textutil"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

// Harness is one detected agent CLI: what the runner advertises plus where
// the binary lives (TS DetectedHarness).
type Harness struct {
	protocol.RunnerHarness
	Path string `json:"path"`
}

// Seams for tests; production values mirror the TS.
var (
	goos         = runtime.GOOS
	defaultShell = "/bin/zsh"
	// 5s: a hung profile must not hang runner startup.
	shellTimeout   = 5 * time.Second
	versionTimeout = 10 * time.Second
	// waitDelay bounds how long a timed-out probe may linger after its
	// process is killed, waiting on output pipes a grandchild (a profile's
	// background job) still holds open.
	waitDelay = time.Second
	// Fallback locations probed when the shell PATH itself misses (rare, but
	// a broken profile shouldn't blind us completely); home-relative ones
	// are added by commonBinDirs.
	systemBinDirs = []string{"/opt/homebrew/bin", "/usr/local/bin"}
	currentUser   = user.Current
)

// homeDir is Node's os.homedir(): $HOME, else the account database. An
// unknown home yields "" (Node would join relative dirs onto it; we skip
// them instead).
func homeDir() string {
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	u, err := currentUser()
	if err != nil {
		return ""
	}
	return u.HomeDir
}

func commonBinDirs() []string {
	dirs := append([]string(nil), systemBinDirs...)
	if home := homeDir(); home != "" {
		dirs = append(dirs,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, "bin"),
			filepath.Join(home, ".claude", "local"),
			filepath.Join(home, ".codex", "bin"),
		)
	}
	return dirs
}

var (
	cacheMu    sync.Mutex
	cachedPath string
)

// LoginShellPath asks the user's own shell for its interactive PATH. Cached
// for the process lifetime — spawning a login shell is slow (~100-500ms with
// heavy profiles) and the answer doesn't change under us. Concurrent callers
// wait for the one probe instead of each spawning a shell.
func LoginShellPath(log protocol.Logger) string {
	log = orNop(log)
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if cachedPath != "" {
		return cachedPath
	}
	fallback := os.Getenv("PATH")
	if goos == "windows" {
		cachedPath = fallback
		return cachedPath
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = defaultShell
	}
	// -i so profile-managed prefixes (nvm, volta) land; -l for login-only
	// profiles.
	stdout, err := run(shellTimeout, shell, []string{"-ilc", `echo -n "$PATH"`},
		append(os.Environ(), "DISABLE_AUTO_UPDATE=true"))
	if err != nil {
		log("login shell PATH probe failed; using process PATH", map[string]any{"error": err.Error()})
		cachedPath = fallback
	} else if resolved := textutil.JSTrim(stdout); resolved != "" {
		cachedPath = resolved
	} else {
		cachedPath = fallback
	}
	// Union with the common dirs so one broken profile can't hide a CLI.
	sep := string(os.PathListSeparator)
	seen := map[string]bool{}
	var parts []string
	for _, dir := range append(strings.Split(cachedPath, sep), commonBinDirs()...) {
		if dir != "" && !seen[dir] {
			seen[dir] = true
			parts = append(parts, dir)
		}
	}
	cachedPath = strings.Join(parts, sep)
	return cachedPath
}

// FindExecutable searches the resolved PATH for a binary ("" when absent).
func FindExecutable(name, searchPath string) string {
	exts := []string{""}
	if goos == "windows" {
		exts = []string{".exe", ".cmd", ".bat", ""}
	}
	for _, dir := range strings.Split(searchPath, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		for _, ext := range exts {
			candidate := filepath.Join(dir, name+ext)
			if executable(candidate) {
				return candidate
			}
		}
	}
	return ""
}

// DetectHarnesses probes for the CLIs the runner can drive. "Found but not
// working" (--version fails) is reported distinctly from "not installed" —
// the fixes differ (plan-v2 §6).
func DetectHarnesses(log protocol.Logger) []Harness {
	log = orNop(log)
	searchPath := LoginShellPath(log)
	out := []Harness{}
	for _, name := range []string{"claude", "codex"} {
		bin := FindExecutable(name, searchPath)
		if bin == "" {
			log(fmt.Sprintf("harness %s: not found on PATH", name), nil)
			continue
		}
		stdout, err := run(versionTimeout, bin, []string{"--version"}, append(os.Environ(), "PATH="+searchPath))
		if err != nil {
			log(fmt.Sprintf("harness %s: found at %s but --version failed", name, bin), map[string]any{"error": err.Error()})
			continue
		}
		version, _, _ := strings.Cut(textutil.JSTrim(stdout), "\n")
		out = append(out, Harness{
			RunnerHarness: protocol.RunnerHarness{
				Name:    name,
				Version: version,
				// Auth probing without burning a model call is per-CLI and
				// flaky; report installed as authed and let the first run
				// surface auth errors legibly instead.
				Authed: true,
			},
			Path: bin,
		})
		log(fmt.Sprintf("harness %s: %s", name, bin), nil)
	}
	return out
}

// run executes name with args and env under a timeout and returns its
// stdout. A non-zero exit, a spawn failure or the timeout is an error. The
// timeout kills the process outright: an interactive shell ignores SIGTERM,
// so a gentler signal could leave a hung profile hanging.
func run(timeout time.Duration, name string, args, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	cmd.WaitDelay = waitDelay
	out, err := cmd.Output()
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("timed out after %s: %w", timeout, err)
	}
	return string(out), err
}

func orNop(log protocol.Logger) protocol.Logger {
	if log == nil {
		return protocol.NopLogger
	}
	return log
}
