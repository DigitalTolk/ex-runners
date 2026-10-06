package workspace

import (
	"context"
	"errors"
	"fmt"
	"github.com/DigitalTolk/ex-runners/internal/textutil"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

// Workspace preparation -----------------------------------------------------

// PreparedRepo is one repo of the task after preparation.
type PreparedRepo struct {
	Path       string          `json:"path"` // GitLab path
	Role       string          `json:"role"`
	Dir        string          `json:"dir"`
	Branch     string          `json:"branch"`
	BaseBranch string          `json:"baseBranch"`
	Cloned     bool            `json:"cloned"`
	HeadSha    string          `json:"headSha"`
	BaseSha    string          `json:"baseSha"`
	Stashed    bool            `json:"stashed"`
	Registry   RegistryProject `json:"registry"`
	Error      string          `json:"error,omitempty"` // set when this repo could not be prepared (others may be fine)
}

// PrepareResult is the prepared workspace of one task.
type PrepareResult struct {
	ProjectDir string         `json:"projectDir"` // cwd for the harness — the project folder holding every repo
	Repos      []PreparedRepo `json:"repos"`
	Note       string         `json:"note"` // the deterministic thread line
}

var (
	projectKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	repoPathRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)+$`)
	branchRe     = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
)

// Prepare (TS prepareWorkspace) makes every repo of the task ready inside the
// project folder: clone (or fetch), resolve the base branch, check out the
// task branch (creating it off the base on first use). Fails only when NO
// repo could be prepared; a partial failure is reported per repo so the agent
// can work on the rest and say what is missing. root "" is Root(); a nil log
// discards.
func Prepare(ctx context.Context, task *protocol.TaskSpec, cred *GitCred, log protocol.Logger, root string) (PrepareResult, error) {
	if task == nil {
		task = &protocol.TaskSpec{}
	}
	log = logger(log)
	if root == "" {
		root = Root()
	}
	if !projectKeyRe.MatchString(task.ProjectKey) {
		return PrepareResult{}, fmt.Errorf(`refusing to prepare a workspace for project key "%s"`, task.ProjectKey)
	}
	if len(task.Repos) == 0 {
		return PrepareResult{}, errors.New("the task lists no repos")
	}
	pdir := ProjectDir(root, task.ProjectKey)
	if err := os.MkdirAll(pdir, 0o777); err != nil {
		return PrepareResult{}, err
	}
	repos := make([]PreparedRepo, 0, len(task.Repos))
	for _, r := range task.Repos {
		p, err := prepareRepo(ctx, task, r, cred, log, root)
		if err != nil {
			why := err.Error()
			log("workspace: repo preparation failed", map[string]any{"repo": r.Path, "error": why})
			dir := RepoDir(root, task.ProjectKey, r.Path)
			p = PreparedRepo{
				Path: r.Path, Role: r.Role, Dir: dir, Branch: r.Branch, BaseBranch: r.BaseBranch,
				Registry: RegistryProject{Dir: dir}, Error: why,
			}
		}
		repos = append(repos, p)
	}
	lines := make([]string, 0, len(repos))
	failures := make([]string, 0, len(repos))
	for _, r := range repos {
		if r.Error != "" {
			failures = append(failures, r.Path+": "+r.Error)
			lines = append(lines, "- ⚠️ "+r.Path+": "+r.Error)
			continue
		}
		where := ShortenHome(r.Dir)
		how := "reusing `" + where + "` (fetched)"
		if r.Cloned {
			how = "cloned into `" + where + "`"
		}
		stash := ""
		if r.Stashed {
			stash = " — earlier uncommitted changes were stashed"
		}
		lines = append(lines, fmt.Sprintf("- %s (%s): %s, origin/%s @ %s%s", r.Path, r.Role, how, r.BaseBranch, r.BaseSha, stash))
	}
	if len(failures) == len(repos) {
		return PrepareResult{}, errors.New(strings.Join(failures, "; "))
	}
	note := "📁 Workspace ready in `" + ShortenHome(pdir) + "` on branch `" + task.Repos[0].Branch + "`:\n" + strings.Join(lines, "\n")
	return PrepareResult{ProjectDir: pdir, Repos: repos, Note: note}, nil
}

// safeBranch admits plain branch names only. A leading "-" would reach git
// as an option (`git fetch origin --upload-pack=…`), ".." is a range, and
// HEAD, refs/…, origin/…, *.lock, dot-led or empty components are names git
// refuses or reads as something other than a branch.
func safeBranch(name string) bool {
	if !branchRe.MatchString(name) || strings.HasPrefix(name, "-") || strings.Contains(name, "..") ||
		name == "HEAD" || strings.HasPrefix(name, "refs/") || strings.HasPrefix(name, "origin/") || strings.HasSuffix(name, ".") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func prepareRepo(ctx context.Context, task *protocol.TaskSpec, r protocol.TaskSpecRepo, cred *GitCred, log protocol.Logger, root string) (PreparedRepo, error) {
	if !repoPathRe.MatchString(r.Path) || strings.Contains(r.Path, "..") {
		return PreparedRepo{}, fmt.Errorf(`refusing repo path "%s"`, r.Path)
	}
	if !safeBranch(r.Branch) {
		return PreparedRepo{}, fmt.Errorf(`refusing branch name "%s"`, r.Branch)
	}
	dir := RepoDir(root, task.ProjectKey, r.Path)
	// Checkouts made by the single-repo layout (<root>/<group>/…/<repo>) hold
	// work that was never pushed — keep using them rather than cloning afresh
	// and losing the branch.
	if !exists(filepath.Join(dir, ".git")) {
		segs := []string{root}
		for _, seg := range strings.Split(r.Path, "/") {
			if seg != "" && seg != "." && seg != ".." {
				segs = append(segs, seg)
			}
		}
		legacy := filepath.Join(segs...)
		if legacy != dir && exists(filepath.Join(legacy, ".git")) {
			log("workspace: using legacy checkout location", map[string]any{"repo": r.Path, "dir": legacy})
			dir = legacy
		}
	}
	// Only commands that may talk to the remote get the credential: clone,
	// fetch, and checkout (Git LFS downloads file contents on checkout).
	env, netEnv := GitEnv(nil, nil), GitEnv(cred, nil)
	origin := RemoteURL(cred, r.Path)
	reg := LoadRegistry(root)
	cloned := false

	if !exists(filepath.Join(dir, ".git")) {
		if cred == nil || cred.Host == "" {
			return PreparedRepo{}, errors.New("no checkout exists yet and no GitLab connector is installed — install the gitlab connector so I can clone")
		}
		parent := filepath.Dir(dir)
		// parent is the project folder Prepare just made; should it vanish,
		// git's own error says why.
		_ = os.MkdirAll(parent, 0o777)
		log("workspace: cloning", map[string]any{"repo": r.Path, "dir": dir})
		if _, err := gitRun(ctx, parent, []string{"clone", "--depth", "50", "--no-single-branch", origin, dir}, netEnv, cloneTimeout); err != nil {
			return PreparedRepo{}, err
		}
		cloned = true
	} else {
		// A reused checkout is agent-editable: vet its config before git runs
		// in it with the credential.
		if cred != nil && cred.Token != "" {
			if err := checkRepoConfig(ctx, dir, origin); err != nil {
				return PreparedRepo{}, err
			}
		}
		log("workspace: fetching", map[string]any{"repo": r.Path, "dir": dir})
		if _, err := gitRun(ctx, dir, []string{"fetch", "origin", "--prune"}, netEnv, fetchTimeout); err != nil {
			// A reused checkout may still work offline (local-only history) — say
			// so instead of failing the task on a flaky network. ("Error: " is
			// how the TS runner's String(err) rendered it.)
			log("workspace: fetch failed; continuing with local refs", map[string]any{"error": "Error: " + err.Error()})
		}
	}

	// Base branch: the task's explicit base, else the remote HEAD, else the
	// usual suspects. Whatever we pick must exist on origin.
	base := textutil.JSTrim(r.BaseBranch)
	if base == "" {
		head, _ := gitRun(ctx, dir, []string{"symbolic-ref", "--short", "refs/remotes/origin/HEAD"}, env, defaultGitTimeout)
		if rest, ok := strings.CutPrefix(head, "origin/"); ok {
			base = rest
		}
	}
	if base == "" {
		for _, cand := range []string{"main", "master", "develop"} {
			if gitOk(ctx, dir, []string{"rev-parse", "--verify", "origin/" + cand}, env) {
				base = cand
				break
			}
		}
	}
	if base == "" {
		return PreparedRepo{}, errors.New("could not determine the base branch — pass base_branch when creating the task")
	}
	if !safeBranch(base) {
		return PreparedRepo{}, fmt.Errorf(`refusing base branch name "%s"`, base)
	}
	if !gitOk(ctx, dir, []string{"rev-parse", "--verify", "origin/" + base}, env) {
		_, _ = gitRun(ctx, dir, []string{"fetch", "origin", base}, netEnv, fetchTimeout)
		if !gitOk(ctx, dir, []string{"rev-parse", "--verify", "origin/" + base}, env) {
			return PreparedRepo{}, fmt.Errorf(`base branch "%s" does not exist on origin`, base)
		}
	}

	// Never clobber uncommitted work in a reused checkout: park it.
	current, _ := gitRun(ctx, dir, []string{"rev-parse", "--abbrev-ref", "HEAD"}, env, defaultGitTimeout)
	stashed := false
	if current != r.Branch {
		dirty, _ := gitRun(ctx, dir, []string{"status", "--porcelain"}, env, defaultGitTimeout)
		if dirty != "" {
			if _, err := gitRun(ctx, dir, []string{"stash", "push", "-u", "-m", "ex-autostash before " + r.Branch}, env, defaultGitTimeout); err != nil {
				return PreparedRepo{}, err
			}
			stashed = true
		}
	}
	var err error
	switch {
	case gitOk(ctx, dir, []string{"rev-parse", "--verify", r.Branch}, env):
		if current != r.Branch {
			_, err = gitRun(ctx, dir, []string{"checkout", r.Branch}, netEnv, defaultGitTimeout)
		}
	case gitOk(ctx, dir, []string{"rev-parse", "--verify", "origin/" + r.Branch}, env):
		// The branch already lives on origin (a previous machine pushed it).
		_, err = gitRun(ctx, dir, []string{"checkout", "-B", r.Branch, "origin/" + r.Branch}, netEnv, defaultGitTimeout)
	default:
		_, err = gitRun(ctx, dir, []string{"checkout", "-B", r.Branch, "origin/" + base}, netEnv, defaultGitTimeout)
	}
	if err != nil {
		return PreparedRepo{}, err
	}
	headSha, _ := gitRun(ctx, dir, []string{"rev-parse", "--short", "HEAD"}, env, defaultGitTimeout)
	baseSha, _ := gitRun(ctx, dir, []string{"rev-parse", "--short", "origin/" + base}, env, defaultGitTimeout)

	project := reg.Projects[r.Path] // missing → just {dir}, set below
	project.Dir = dir
	project.DefaultBranch = base
	project.LastFetch = time.Now().UTC().Format("2006-01-02T15:04:05.000Z") // Date.toISOString()
	reg.Projects[r.Path] = project
	if err := SaveRegistry(root, reg); err != nil {
		return PreparedRepo{}, err
	}

	return PreparedRepo{
		Path: r.Path, Role: r.Role, Dir: dir, Branch: r.Branch, BaseBranch: base, Cloned: cloned,
		HeadSha: headSha, BaseSha: baseSha, Stashed: stashed, Registry: project,
	}, nil
}

// BranchHasChanges reports whether the task branch carries commits beyond
// its base — untouched repos get no MR. A git failure reads as "no changes"
// (as in TS); the error is non-nil only when ctx ended, so a cancelled run
// is not mistaken for "nothing to push".
func BranchHasChanges(ctx context.Context, dir, base string, cred *GitCred) (bool, error) {
	n, err := gitRun(ctx, dir, []string{"rev-list", "--count", "origin/" + base + "..HEAD"}, GitEnv(nil, nil), defaultGitTimeout)
	if err != nil {
		return false, ctx.Err()
	}
	count, _ := strconv.Atoi(n)
	return count > 0, nil
}

// PushBranch pushes the task branch (sets upstream) and returns the short
// HEAD sha. Refuses a dirty tree — the agent must commit first; committing on
// its behalf would hide work — and a checkout whose config was tampered with
// (origin must still be repoPath on the connector's host).
func PushBranch(ctx context.Context, dir, repoPath, branch string, cred GitCred, log protocol.Logger) (string, error) {
	log = logger(log)
	env := GitEnv(nil, nil)
	dirty, err := gitRun(ctx, dir, []string{"status", "--porcelain"}, env, defaultGitTimeout)
	if err != nil {
		return "", err
	}
	if dirty != "" {
		lines := strings.Split(dirty, "\n")
		if len(lines) > 20 {
			lines = lines[:20]
		}
		return "", errors.New("uncommitted changes in the workspace — commit them first:\n" + strings.Join(lines, "\n"))
	}
	current, err := gitRun(ctx, dir, []string{"rev-parse", "--abbrev-ref", "HEAD"}, env, defaultGitTimeout)
	if err != nil {
		return "", err
	}
	if current != branch {
		return "", fmt.Errorf("HEAD is on %s, not the task branch %s", current, branch)
	}
	if cred.Token != "" {
		if err := checkRepoConfig(ctx, dir, RemoteURL(&cred, repoPath)); err != nil {
			return "", err
		}
	}
	netEnv := GitEnv(&cred, nil)
	log("workspace: pushing", map[string]any{"dir": dir, "branch": branch})
	// With a credential, hooks are off for the push (they'd see the token) —
	// so the LFS objects git-lfs's pre-push hook would upload go up here.
	if cred.Token != "" && usesLFS(dir) {
		if _, err := gitRun(ctx, dir, []string{"lfs", "push", "origin", branch}, netEnv, pushTimeout); err != nil {
			return "", err
		}
	}
	if _, err := gitRun(ctx, dir, []string{"push", "-u", "origin", branch}, netEnv, pushTimeout); err != nil {
		return "", err
	}
	return gitRun(ctx, dir, []string{"rev-parse", "--short", "HEAD"}, env, defaultGitTimeout)
}

// usesLFS reports whether the checkout tracks files with Git LFS.
func usesLFS(dir string) bool {
	if exists(filepath.Join(dir, ".lfsconfig")) {
		return true
	}
	b, err := os.ReadFile(filepath.Join(dir, ".gitattributes"))
	return err == nil && strings.Contains(string(b), "filter=lfs")
}
