package mcp

import (
	"os"

	"github.com/DigitalTolk/ex-runners/internal/secretpaths"
	"path/filepath"
	"strings"
)

// Credential locations. A harness tool call that touches one always raises
// an approval card — no task profile or standing "always allow" waves it
// through. A prompt-injected agent reading the runner's account token or
// the user's SSH keys and posting them to chat is the attack this stops.

var (
	userHomeDir  = os.UserHomeDir
	evalSymlinks = filepath.EvalSymlinks
)

// protectedRoots are the absolute credential locations of this machine.
func (s *Server) protectedRoots() []string {
	var roots []string
	for _, p := range []string{s.env.RunnerHome, s.env.PrivateDir} {
		if p != "" {
			roots = append(roots, p)
		}
	}
	home, _ := userHomeDir()
	return secretpaths.Under(home, roots...)
}

// realPath resolves symlinks in p as far as the path exists, so a link
// planted inside the workspace can't stand in for a protected location.
func realPath(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for {
		if r, err := evalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// within reports whether p is root or below it.
func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// expandPath turns a tool's path argument into an absolute, symlink-resolved
// path: ~ and $HOME expand, relative paths resolve against base.
func expandPath(p, base string) string {
	if home, err := userHomeDir(); err == nil && home != "" {
		for _, prefix := range []string{"~", "$HOME", "${HOME}"} {
			if p == prefix || strings.HasPrefix(p, prefix+"/") {
				p = home + p[len(prefix):]
				break
			}
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return realPath(p)
}

// cwd is where the harness runs: the checkout in a task, else the workdir.
func (s *Server) cwd() string {
	if s.env.TaskDir != "" {
		return s.env.TaskDir
	}
	return s.env.WorkDir
}

// toolPaths are the path arguments of a harness tool call.
func toolPaths(input map[string]any) []string {
	var out []string
	for _, k := range []string{"file_path", "path", "notebook_path"} {
		if v, ok := input[k].(string); ok && v != "" {
			out = append(out, v)
		}
	}
	return out
}

// touchesProtected reports whether text (a shell command, a glob) names a
// credential location — by absolute path, or through ~ / $HOME.
func (s *Server) touchesProtected(text string) bool {
	if text == "" {
		return false
	}
	for _, root := range s.protectedRoots() {
		if strings.Contains(text, root) {
			return true
		}
	}
	for _, rel := range secretpaths.Home {
		for _, prefix := range []string{"~/", "$HOME/", "${HOME}/", `"$HOME"/`} {
			if strings.Contains(text, prefix+rel) {
				return true
			}
		}
	}
	return false
}

// inputTouchesProtected checks a harness tool call's paths (resolved) and
// free-text arguments (commands, glob patterns) against the credential list.
func (s *Server) inputTouchesProtected(input map[string]any) bool {
	roots := s.protectedRoots()
	for _, p := range toolPaths(input) {
		abs := expandPath(p, s.cwd())
		for _, root := range roots {
			if within(abs, realPath(root)) {
				return true
			}
		}
	}
	for _, k := range []string{"command", "pattern", "glob"} {
		if v, ok := input[k].(string); ok && s.touchesProtected(v) {
			return true
		}
	}
	return false
}

// inRunScope reports whether every path argument stays inside the run's own
// folders (the cwd and the workdir). A standing "always allow reads/edits"
// covers those — not the rest of the disk.
func (s *Server) inRunScope(input map[string]any) bool {
	var scopes []string
	for _, d := range []string{s.cwd(), s.env.WorkDir} {
		if d != "" {
			scopes = append(scopes, realPath(d))
		}
	}
	for _, p := range toolPaths(input) {
		abs := expandPath(p, s.cwd())
		inside := false
		for _, scope := range scopes {
			if within(abs, scope) {
				inside = true
				break
			}
		}
		if !inside {
			return false
		}
	}
	return true
}
