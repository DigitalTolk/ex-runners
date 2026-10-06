// Package workspace is the Ex code workspace (plan-coding-agent.md): the
// fixed, user-visible folder where the coding agent keeps project checkouts —
// ~/ex-workspace by default (EX_WORKSPACE_ROOT overrides). One canonical clone
// per project, reused across tasks; a registry.json remembers what the agent
// learned about each project (default branch, setup/test/dev commands, port)
// so the next task skips the archaeology.
//
// Everything here is DETERMINISTIC runner code, not model behavior: clone or
// fetch, resolve the base branch, check out the task branch, report what
// happened. The credential never touches disk — git gets an inline credential
// helper through the environment of the child process only.
package workspace

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/DigitalTolk/ex-runners/internal/protocol"
	"github.com/DigitalTolk/ex-runners/internal/textutil"
)

// Registry -----------------------------------------------------------------

// RegistryProject is what the registry remembers about one repo. The JSON tags
// are the TS runner's, so either runner reads the registry.json the other
// wrote.
type RegistryProject struct {
	Dir           string `json:"dir"`
	DefaultBranch string `json:"defaultBranch,omitempty"`
	LastFetch     string `json:"lastFetch,omitempty"`
	SetupCmd      string `json:"setupCmd,omitempty"`
	TestCmd       string `json:"testCmd,omitempty"`
	DevCmd        string `json:"devCmd,omitempty"`
	Port          int    `json:"port,omitempty"`
	Notes         string `json:"notes,omitempty"`
}

// Registry is registry.json: every project the workspace knows, keyed by its
// GitLab path.
type Registry struct {
	Version  int                        `json:"version"`
	Projects map[string]RegistryProject `json:"projects"`
}

// absPath is filepath.Abs, swappable in tests (it fails only when the working
// directory cannot be determined).
var absPath = filepath.Abs

// Root is the workspace root: EX_WORKSPACE_ROOT when set (made absolute, like
// path.resolve), else ~/ex-workspace.
func Root() string {
	if override := os.Getenv("EX_WORKSPACE_ROOT"); textutil.JSTrim(override) != "" {
		if abs, err := absPath(override); err == nil {
			return abs
		}
		return filepath.Clean(override)
	}
	return filepath.Join(homeDir(), "ex-workspace")
}

func registryPath(root string) string {
	return filepath.Join(root, "registry.json")
}

// LoadRegistry reads registry.json; a missing or corrupt file is an empty
// registry (first run). A project entry with a mistyped field keeps the
// fields that do decode — the TS runner kept entries verbatim.
func LoadRegistry(root string) Registry {
	reg := Registry{Version: 1, Projects: map[string]RegistryProject{}}
	data, err := os.ReadFile(registryPath(root))
	if err != nil {
		return reg // first run → empty
	}
	var raw struct {
		Projects map[string]*RegistryProject `json:"projects"`
	}
	var typeErr *json.UnmarshalTypeError
	if err := json.Unmarshal(data, &raw); err != nil && !errors.As(err, &typeErr) {
		return reg // corrupt → empty
	}
	for k, p := range raw.Projects {
		if p != nil {
			reg.Projects[k] = *p
		}
	}
	return reg
}

// SaveRegistry writes registry.json (2-space indented, trailing newline — the
// TS runner's JSON.stringify(reg, null, 2) layout), creating the root.
func SaveRegistry(root string, reg Registry) error {
	if err := os.MkdirAll(root, 0o777); err != nil {
		return err
	}
	if reg.Version == 0 {
		reg.Version = 1
	}
	if reg.Projects == nil {
		reg.Projects = map[string]RegistryProject{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep "&&" in commands readable, as JSON.stringify does
	enc.SetIndent("", "  ")
	_ = enc.Encode(reg) // plain strings and ints: encoding cannot fail
	return os.WriteFile(registryPath(root), buf.Bytes(), 0o666)
}

// repoDir is a repo's checkout inside its PROJECT folder:
// <root>/<projectKey>/<repoName>. Grouping by product keeps a task's repos
// side by side (the harness runs with the project folder as cwd) and reads
// naturally in Finder: ~/ex-workspace/cliffhub/{cliffhub-2-backend,…}.
// safeSegment keeps a folder name inside its parent: allowed characters
// only, and never "." / ".." (server-side validation already forbids these;
// this is the belt to that brace).
func safeSegment(s, fallback string) string {
	clean := replaceUnsafe(s)
	if strings.Trim(clean, ".") == "" { // "", ".", "..", "...": never a real folder
		return fallback
	}
	return clean
}

// replaceUnsafe is the TS s.replace(/[^A-Za-z0-9._-]/g, '-'). A JS regex
// without the u flag sees a character outside the BMP as two UTF-16 code
// units, so it becomes two dashes there too.
func replaceUnsafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x80 && (r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'):
			b.WriteRune(r)
		case r > 0xFFFF:
			b.WriteString("--")
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// RepoDir is <root>/<projectKey>/<last segment of repoPath>, each part made
// safe (see safeSegment).
func RepoDir(root, projectKey, repoPath string) string {
	name := ""
	for _, seg := range strings.Split(repoPath, "/") {
		if seg != "" {
			name = seg
		}
	}
	return filepath.Join(root, safeSegment(projectKey, "project"), safeSegment(name, "repo"))
}

// ProjectDir is <root>/<projectKey> — the harness's cwd for a coding task.
func ProjectDir(root, projectKey string) string {
	return filepath.Join(root, safeSegment(projectKey, "project"))
}

// CommandUpdate is what register_project_commands learned. A nil field leaves
// the registry value unchanged (TS: undefined); an empty or blank string
// clears it; Port is only ever set (positive values), never cleared.
type CommandUpdate struct {
	SetupCmd, TestCmd, DevCmd, Notes *string
	Port                             *int
}

// UpdateProjectCommands persists what the agent learned
// (register_project_commands). It returns the updated entry — also when
// saving fails, alongside the error.
func UpdateProjectCommands(root, repoPath string, upd CommandUpdate) (RegistryProject, error) {
	reg := LoadRegistry(root)
	next, ok := reg.Projects[repoPath]
	if !ok {
		next = RegistryProject{Dir: ProjectDir(root, repoPath)}
	}
	for _, f := range []struct {
		in  *string
		out *string
	}{
		{upd.SetupCmd, &next.SetupCmd},
		{upd.TestCmd, &next.TestCmd},
		{upd.DevCmd, &next.DevCmd},
		{upd.Notes, &next.Notes},
	} {
		if f.in != nil {
			*f.out = textutil.Head(textutil.JSTrim(*f.in), 500) // "" clears (omitempty)
		}
	}
	if upd.Port != nil && *upd.Port > 0 {
		next.Port = *upd.Port
	}
	reg.Projects[repoPath] = next
	return next, SaveRegistry(root, reg)
}

// Path rendering --------------------------------------------------------------

// lookupUser is user.Current, swappable in tests.
var lookupUser = user.Current

// homeDir mirrors Node's os.homedir(): $HOME (USERPROFILE on Windows), else
// the account database.
func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	if u, err := lookupUser(); err == nil {
		return u.HomeDir
	}
	return ""
}

// ShortenHome renders a path with ~ for the home dir — chat-friendly.
func ShortenHome(p string) string {
	home := homeDir()
	if home != "" && strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}

// Small shared helpers -------------------------------------------------------

// logger never returns nil, so callers may pass a nil Logger.
func logger(log protocol.Logger) protocol.Logger {
	if log == nil {
		return protocol.NopLogger
	}
	return log
}

// setEnv sets key=val in a child environment, dropping every earlier entry
// for the key (envKeyEqual: case-insensitive on Windows).
func setEnv(env []string, key, val string) []string {
	return append(unsetEnv(env, key), key+"="+val)
}

func unsetEnv(env []string, key string) []string {
	out := env[:0]
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); !envKeyEqual(k, key) {
			out = append(out, kv)
		}
	}
	return out
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
