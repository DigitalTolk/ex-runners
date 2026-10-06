package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/textutil"
)

// Git plumbing ---------------------------------------------------------------

// GitCred is the clone/push credential: the requester's GitLab connector.
type GitCred struct {
	// Origin of the GitLab instance, e.g. https://gitlab.example.com (no path).
	Host  string
	Token string // personal access token; "" = anonymous
}

// GitHostFromBaseURL turns a connector base URL (…/api/v4) into the git
// origin ("" when it does not parse).
func GitHostFromBaseURL(baseURL string) string {
	u, ok := parseWebURL(baseURL)
	if !ok {
		return ""
	}
	return u.protocol + "//" + u.host
}

// credentialHelper reads the token from the child's own environment, so the
// helper text itself (which git may echo in traces) never carries it. It
// answers only `get` requests for exactly the connector's protocol and host:
// a remote, insteadOf or proxy rewritten inside the checkout gets nothing.
const credentialHelper = `!f() { test "$1" = get || exit 0; p=; h=; ` +
	`while IFS= read -r l && [ -n "$l" ]; do case "$l" in protocol=*) p="${l#protocol=}";; host=*) h="${l#host=}";; esac; done; ` +
	`[ "$p" = "$EX_GIT_PROTOCOL" ] && [ "$h" = "$EX_GIT_HOST" ] || exit 0; ` +
	`echo "username=oauth2"; echo "password=$EX_GIT_TOKEN"; }; f`

// GitEnv builds the child environment for git: no prompts ever. With a
// token it also locks the command down, because only commands that talk to
// the remote (clone, fetch, push) get one: hooks and fsmonitor off (both run
// code from the checkout, which the agent can edit, and would see the
// token), the user's own credential helpers reset (on success git asks each
// to `store` the token — macOS ships osxkeychain system-wide), and an inline
// helper that hands the token only to the connector's host. The settings ride
// GIT_CONFIG_COUNT (command-line level), so the checkout's own .git/config
// can't override them; nothing is written to disk and the token never
// appears in argv. Without a token the environment is as it always was.
// extra overrides the inherited environment; the fixed settings override
// extra.
func GitEnv(cred *GitCred, extra map[string]string) []string {
	env := os.Environ()
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = setEnv(env, k, extra[k])
	}
	env = setEnv(env, "GIT_TERMINAL_PROMPT", "0")
	env = setEnv(env, "GIT_ASKPASS", "")
	env = setEnv(env, "SSH_ASKPASS", "")
	if cred == nil || cred.Token == "" {
		return env
	}
	u, _ := parseWebURL(cred.Host)
	env = setEnv(env, "EX_GIT_TOKEN", cred.Token)
	env = setEnv(env, "EX_GIT_PROTOCOL", strings.TrimSuffix(u.protocol, ":"))
	env = setEnv(env, "EX_GIT_HOST", u.host)
	// An inherited GIT_CONFIG_PARAMETERS (`git -c` from a parent) is applied
	// after our entries and could re-add a helper or hooks.
	env = unsetEnv(env, "GIT_CONFIG_PARAMETERS")
	config := [][2]string{
		{"core.hooksPath", os.DevNull},
		{"core.fsmonitor", "false"},
		{"credential.helper", ""}, // the empty value RESETS the helper list
		{"credential.helper", credentialHelper},
	}
	env = setEnv(env, "GIT_CONFIG_COUNT", strconv.Itoa(len(config)))
	for i, kv := range config {
		env = setEnv(env, "GIT_CONFIG_KEY_"+strconv.Itoa(i), kv[0])
		env = setEnv(env, "GIT_CONFIG_VALUE_"+strconv.Itoa(i), kv[1])
	}
	return env
}

// checkRepoConfig refuses a checkout whose own .git/config could redirect a
// credentialed fetch or push: the agent can edit that file without a card,
// and url.*.insteadOf, http.* (proxy, TLS, extra headers), credential.*,
// include*, push URLs, ssh/proxy commands or a moved origin would carry the
// token — or the pushed code — somewhere else. Everything else a checkout may
// legitimately carry (husky's core.hooksPath, LFS settings, user.*) is left
// alone. wantOrigin "" skips the origin check.
func checkRepoConfig(ctx context.Context, dir, wantOrigin string) error {
	out, err := gitRun(ctx, dir, []string{"config", "--local", "--list", "-z"}, GitEnv(nil, nil), defaultGitTimeout)
	if err != nil {
		return err
	}
	var bad []string
	for _, entry := range strings.Split(out, "\x00") {
		if entry == "" {
			continue
		}
		key, val, _ := strings.Cut(entry, "\n")
		if redirectsRemote(key, val, wantOrigin) {
			bad = append(bad, key)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("refusing to use %s with your GitLab credential: its .git/config sets %s, which could send the push or the token elsewhere — remove those settings and retry", dir, strings.Join(bad, ", "))
	}
	return nil
}

// riskyConfigPrefixes reroute the transport or the credential, or (filter.*)
// run a command during the credentialed checkout.
var riskyConfigPrefixes = []string{"url.", "http.", "credential.", "include.", "includeif.", "protocol.", "filter."}

// lfsFilter is what `git lfs install --local` writes — the one filter a
// checkout may carry.
var lfsFilter = map[string]string{
	"filter.lfs.clean":    "git-lfs clean -- %f",
	"filter.lfs.smudge":   "git-lfs smudge -- %f",
	"filter.lfs.process":  "git-lfs filter-process",
	"filter.lfs.required": "true",
}

// riskyConfigKeys run a command of their own for the transport, or move
// config into a file the --local listing doesn't show.
var riskyConfigKeys = map[string]bool{
	"core.sshcommand": true, "core.gitproxy": true, "core.askpass": true, "extensions.worktreeconfig": true,
}

// The runner always names origin (`fetch origin`, `push -u origin <branch>`),
// so only origin's own settings decide where the credential goes.
func redirectsRemote(key, val, wantOrigin string) bool {
	lower := strings.ToLower(key)
	if want, isLFS := lfsFilter[lower]; isLFS && val == want {
		return false
	}
	if riskyConfigKeys[lower] {
		return true
	}
	for _, p := range riskyConfigPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	switch lower {
	case "remote.origin.pushurl", "remote.origin.proxy", "remote.origin.uploadpack", "remote.origin.receivepack", "remote.origin.vcs":
		return true
	case "remote.origin.url":
		return wantOrigin != "" && !sameRemote(val, wantOrigin)
	}
	return false
}

func sameRemote(a, b string) bool {
	norm := func(s string) string { return strings.TrimSuffix(strings.TrimRight(s, "/"), ".git") }
	return norm(a) == norm(b)
}

// RemoteURL is the https remote the runner clones a repo from.
func RemoteURL(cred *GitCred, repoPath string) string {
	if cred == nil || cred.Host == "" {
		return ""
	}
	return strings.TrimRight(cred.Host, "/") + "/" + repoPath + ".git"
}

const (
	defaultGitTimeout = 120 * time.Second
	cloneTimeout      = 15 * time.Minute
	fetchTimeout      = 5 * time.Minute
	pushTimeout       = 5 * time.Minute
)

var (
	// maxGitOutput is Node execFile's maxBuffer: more output than this on
	// stdout or stderr fails the command.
	maxGitOutput = 16 * 1024 * 1024
	// gitWaitDelay bounds how long a finished (or timed-out) git may keep its
	// output pipes open through a lingering child before Wait gives up.
	gitWaitDelay = 10 * time.Second
)

// Git runs `git args…` in dir with the credential's environment (GitEnv) and
// the default 2-minute timeout, returning stdout trimmed. A failed command's
// error reads "Command failed: git <args>\n<stderr>" (Node execFile's wording).
func Git(ctx context.Context, dir string, args []string, cred *GitCred) (string, error) {
	return gitRun(ctx, dir, args, GitEnv(cred, nil), defaultGitTimeout)
}

// gitError is a git command that ran and failed (non-zero exit, killed on
// timeout or cancellation). The message is Node execFile's.
type gitError struct {
	args   []string
	stderr string
	err    error
}

func (e *gitError) Error() string {
	cmd := "git"
	if len(e.args) > 0 {
		cmd += " " + strings.Join(e.args, " ")
	}
	return "Command failed: " + cmd + "\n" + e.stderr
}

func (e *gitError) Unwrap() error { return e.err }

// cappedBuffer collects one output stream up to maxGitOutput bytes. (The
// buffer is a field, not embedded: an embedded bytes.Buffer would promote
// ReadFrom, and io.Copy would bypass the cap.)
type cappedBuffer struct {
	buf  bytes.Buffer
	name string
	max  int
	over bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.max {
		b.over = true
		return 0, errors.New(b.name + " maxBuffer length exceeded")
	}
	return b.buf.Write(p)
}

// gitRun is the TS git(args, cwd, env, timeoutMs): stdout trimmed, the child
// stopped with SIGTERM on timeout (so git removes its lock files).
func gitRun(ctx context.Context, dir string, args []string, env []string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Cancel = func() error { return terminate(cmd.Process) }
	cmd.WaitDelay = gitWaitDelay
	stdout := &cappedBuffer{name: "stdout", max: maxGitOutput}
	stderr := &cappedBuffer{name: "stderr", max: maxGitOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	for _, b := range []*cappedBuffer{stdout, stderr} {
		if b.over {
			return "", errors.New(b.name + " maxBuffer length exceeded")
		}
	}
	if err != nil && cmd.ProcessState == nil {
		return "", err // never started: git missing, dir missing, context already done
	}
	// A git that succeeded but left a helper holding its pipes past
	// gitWaitDelay still succeeded (Node would simply have waited longer).
	if err != nil && (!errors.Is(err, exec.ErrWaitDelay) || !cmd.ProcessState.Success()) {
		return "", &gitError{args: args, stderr: stderr.buf.String(), err: err}
	}
	return textutil.JSTrim(stdout.buf.String()), nil
}

func gitOk(ctx context.Context, dir string, args []string, env []string) bool {
	_, err := gitRun(ctx, dir, args, env, defaultGitTimeout)
	return err == nil
}

// URL parsing ----------------------------------------------------------------

// webURL is the part of a WHATWG URL (JavaScript's new URL) the workspace
// reads: protocol ("https:"), host (hostname[:port], default port dropped)
// and port ("" when absent or the scheme's default).
type webURL struct {
	protocol, host, port string
}

// specialSchemes are WHATWG's special schemes with their default ports
// (file has none and may have an empty host).
var specialSchemes = map[string]string{"http": "80", "https": "443", "ws": "80", "wss": "443", "ftp": "21", "file": ""}

// parseWebURL mirrors new URL(raw) closely enough for origins and ports:
// leading/trailing C0-or-space trimmed and tabs/newlines dropped, a scheme
// required, special schemes need a host (any run of slashes after "http:"
// is accepted) and get a lower-cased host, ports must be 0–65535 and the
// scheme's default port is elided. Not mirrored: IDNA/punycode hosts, IPv6
// canonicalisation, backslashes as slashes.
func parseWebURL(raw string) (webURL, bool) {
	s := strings.TrimFunc(raw, func(r rune) bool { return r <= ' ' })
	s = strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(s)
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" {
		return webURL{}, false
	}
	def, special := specialSchemes[u.Scheme]
	if special && u.Scheme != "file" {
		// "http:host", "http:/host" and "http:///host" all mean http://host.
		if u, err = url.Parse(u.Scheme + "://" + strings.TrimLeft(s[len(u.Scheme)+1:], "/")); err != nil || u.Hostname() == "" {
			return webURL{}, false
		}
	}
	host := u.Hostname()
	if special {
		host = strings.ToLower(host)
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n > 65535 {
			return webURL{}, false
		}
		port = strconv.Itoa(n)
		if special && port == def {
			port = ""
		}
	}
	if port != "" {
		host += ":" + port
	}
	return webURL{protocol: u.Scheme + ":", host: host, port: port}, true
}
