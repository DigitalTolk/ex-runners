// Package secretpaths lists where credentials live on a developer machine.
// Agents must never reach these without a human: the Claude permission
// gateway raises a card for them, and Codex's sandbox profile denies them
// outright (Codex has no approval prompt to route to).
package secretpaths

import (
	"path/filepath"
	"sort"
)

// Home are credential locations relative to the user's home directory.
var Home = []string{
	".ex-runner", ".ssh", ".aws", ".azure", ".config/gcloud", ".codex", ".claude", ".claude.json",
	".config/gh", ".config/glab-cli", ".docker/config.json", ".kube", ".gnupg",
	".netrc", ".git-credentials", ".npmrc", ".pypirc", ".cargo/credentials.toml", "Library/Keychains",
}

// Under returns the absolute credential locations for home, plus extra
// (empty entries skipped), sorted and de-duplicated. home "" yields only
// extra.
func Under(home string, extra ...string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if home != "" {
		for _, rel := range Home {
			add(filepath.Join(home, filepath.FromSlash(rel)))
		}
	}
	for _, p := range extra {
		add(p)
	}
	sort.Strings(out)
	return out
}
