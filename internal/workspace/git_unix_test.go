//go:build !windows

package workspace

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

// gitSandbox isolates every git the test — and the code under test — runs
// from the developer's own git config, credential helpers (keychain) and
// workspace. It returns the fake home; the workspace root lives inside it.
func gitSandbox(t *testing.T) (home, root string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	global := filepath.Join(home, ".gitconfig")
	_ = os.WriteFile(global, []byte("[init]\n\tdefaultBranch = main\n[advice]\n\tdetachedHead = false\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "Ex Test")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "test@ex.local")
	}
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "EX_GIT_TOKEN"} {
		unsetenv(t, k)
	}
	root = filepath.Join(home, "ex-workspace")
	t.Setenv("EX_WORKSPACE_ROOT", root)
	return home, root
}

// runGit runs a setup git command and fails the test if it fails.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", name)
	runGit(t, dir, "commit", "-q", "-m", "add "+name)
	return runGit(t, dir, "rev-parse", "--short", "HEAD")
}

// forge is a directory of bare repos standing in for a GitLab host.
type forge struct{ dir, host string }

func newForge(t *testing.T) *forge {
	dir := t.TempDir()
	return &forge{dir: dir, host: "file://" + dir}
}

// repo creates <forge>/<path>.git with one commit per branch (the first is
// the default branch) and returns a seeded working clone for later pushes.
func (f *forge) repo(t *testing.T, path string, branches ...string) string {
	t.Helper()
	bare := filepath.Join(f.dir, path+".git")
	_ = os.MkdirAll(filepath.Dir(bare), 0o755)
	runGit(t, f.dir, "init", "-q", "--bare", "-b", branches[0], bare)
	seed := t.TempDir()
	runGit(t, seed, "init", "-q", "-b", branches[0])
	runGit(t, seed, "remote", "add", "origin", bare)
	commitFile(t, seed, "README.md", "# "+path+"\n")
	runGit(t, seed, "push", "-q", "origin", branches[0])
	for _, b := range branches[1:] {
		runGit(t, seed, "checkout", "-q", "-b", b, branches[0])
		commitFile(t, seed, b+".txt", b+"\n")
		runGit(t, seed, "push", "-q", "origin", b)
	}
	runGit(t, seed, "checkout", "-q", branches[0])
	return seed
}

type logLine struct {
	msg   string
	extra map[string]any
}

func captureLog() (protocol.Logger, func() []logLine) {
	var mu sync.Mutex
	var lines []logLine
	return func(msg string, extra map[string]any) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, logLine{msg, extra})
		}, func() []logLine {
			mu.Lock()
			defer mu.Unlock()
			return append([]logLine(nil), lines...)
		}
}

func findLog(lines []logLine, msg string) (logLine, bool) {
	for _, l := range lines {
		if l.msg == msg {
			return l, true
		}
	}
	return logLine{}, false
}

func taskOf(key string, repos ...protocol.TaskSpecRepo) *protocol.TaskSpec {
	return &protocol.TaskSpec{ID: "t1", ProjectKey: key, Repos: repos}
}

var isoMillis = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)

// --- Git ---

func TestGitTrimsStdoutAndFormatsFailuresLikeExecFile(t *testing.T) {
	gitSandbox(t)
	ctx := context.Background()
	out, err := Git(ctx, t.TempDir(), []string{"--version"}, nil)
	if err != nil || !strings.HasPrefix(out, "git version ") || strings.HasSuffix(out, "\n") {
		t.Fatalf("got %q, %v", out, err)
	}
	dir := t.TempDir()
	_, err = Git(ctx, dir, []string{"rev-parse", "--verify", "origin/nope"}, nil)
	want := "Command failed: git rev-parse --verify origin/nope\nfatal: not a git repository (or any of the parent directories): .git\n"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %q", err)
	}
	var ge *gitError
	var exitErr *exec.ExitError
	if !errors.As(err, &ge) || !errors.As(err, &exitErr) {
		t.Fatalf("err does not unwrap to the exit error: %#v", err)
	}
	// No args: "git" alone (bare `git` prints usage and exits 1).
	if _, err := Git(ctx, dir, nil, nil); err == nil || !strings.HasPrefix(err.Error(), "Command failed: git\n") {
		t.Fatalf("err = %v", err)
	}
	// A git that never started is not dressed up as "Command failed".
	_, err = Git(ctx, filepath.Join(dir, "missing"), []string{"status"}, nil)
	if err == nil || errors.As(err, &ge) {
		t.Fatalf("err = %v", err)
	}
}

func TestGitMaxBuffer(t *testing.T) {
	gitSandbox(t)
	orig := maxGitOutput
	t.Cleanup(func() { maxGitOutput = orig })
	maxGitOutput = 5
	if _, err := Git(context.Background(), t.TempDir(), []string{"--version"}, nil); err == nil || err.Error() != "stdout maxBuffer length exceeded" {
		t.Fatalf("err = %v", err)
	}
	if _, err := Git(context.Background(), t.TempDir(), []string{"rev-parse", "HEAD"}, nil); err == nil || err.Error() != "stderr maxBuffer length exceeded" {
		t.Fatalf("err = %v", err)
	}
}

func TestGitTimeoutTerminatesTheCommand(t *testing.T) {
	gitSandbox(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Git(ctx, t.TempDir(), []string{"-c", "alias.nap=!sleep 20", "nap"}, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "Command failed: git -c alias.nap=!sleep 20 nap\n") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("timeout took %v", d)
	}
	// Already-cancelled context: git never starts.
	done, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := Git(done, t.TempDir(), []string{"--version"}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestGitThatSucceededButLeftAHelperHoldingItsPipesSucceeds(t *testing.T) {
	gitSandbox(t)
	orig := gitWaitDelay
	t.Cleanup(func() { gitWaitDelay = orig })
	gitWaitDelay = 200 * time.Millisecond
	out, err := Git(context.Background(), t.TempDir(), []string{"-c", "alias.bg=!sleep 3 & echo started", "bg"}, nil)
	if err != nil || out != "started" {
		t.Fatalf("got %q, %v", out, err)
	}
}

// --- Prepare ---

func TestPrepareClonesThenReuses(t *testing.T) {
	_, root := gitSandbox(t)
	f := newForge(t)
	backendSeed := f.repo(t, "dt/backend", "main", "develop")
	f.repo(t, "dt/frontend", "main")
	task := taskOf("cliffhub",
		protocol.TaskSpecRepo{Path: "dt/backend", Role: "backend", Branch: "ex/task-1"},
		protocol.TaskSpecRepo{Path: "dt/frontend", Role: "frontend", Branch: "ex/task-1"},
	)
	log, lines := captureLog()
	cred := &GitCred{Host: f.host + "//"} // trailing slashes are trimmed
	res, err := Prepare(context.Background(), task, cred, log, root)
	if err != nil {
		t.Fatal(err)
	}
	pdir := filepath.Join(root, "cliffhub")
	if res.ProjectDir != pdir || len(res.Repos) != 2 {
		t.Fatalf("res = %+v", res)
	}
	for i, r := range res.Repos {
		name := []string{"backend", "frontend"}[i]
		dir := filepath.Join(pdir, name)
		if r.Dir != dir || !r.Cloned || r.Stashed || r.BaseBranch != "main" || r.Branch != "ex/task-1" || r.Error != "" {
			t.Fatalf("repo %d = %+v", i, r)
		}
		if r.HeadSha == "" || r.HeadSha != r.BaseSha {
			t.Fatalf("shas = %q %q", r.HeadSha, r.BaseSha)
		}
		if b := runGit(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); b != "ex/task-1" {
			t.Fatalf("on %q", b)
		}
		if r.Registry.Dir != dir || r.Registry.DefaultBranch != "main" || !isoMillis.MatchString(r.Registry.LastFetch) {
			t.Fatalf("registry = %+v", r.Registry)
		}
		// The clone URL is the bare host path — no credential anywhere in config.
		if u := runGit(t, dir, "remote", "get-url", "origin"); u != f.host+"/dt/"+name+".git" {
			t.Fatalf("origin = %q", u)
		}
	}
	wantNote := "📁 Workspace ready in `~/ex-workspace/cliffhub` on branch `ex/task-1`:\n" +
		"- dt/backend (backend): cloned into `~/ex-workspace/cliffhub/backend`, origin/main @ " + res.Repos[0].BaseSha + "\n" +
		"- dt/frontend (frontend): cloned into `~/ex-workspace/cliffhub/frontend`, origin/main @ " + res.Repos[1].BaseSha
	if res.Note != wantNote {
		t.Fatalf("note =\n%s\nwant\n%s", res.Note, wantNote)
	}
	if l, ok := findLog(lines(), "workspace: cloning"); !ok || l.extra["repo"] != "dt/backend" || l.extra["dir"] != filepath.Join(pdir, "backend") {
		t.Fatalf("clone log = %+v", l)
	}
	reg := LoadRegistry(root)
	if reg.Projects["dt/backend"].DefaultBranch != "main" || reg.Projects["dt/frontend"].Dir != filepath.Join(pdir, "frontend") {
		t.Fatalf("registry = %+v", reg)
	}

	// What the agent learned survives the next preparation.
	if _, err := UpdateProjectCommands(root, "dt/backend", CommandUpdate{TestCmd: ptr("make test")}); err != nil {
		t.Fatal(err)
	}
	// Upstream moves on; the second run fetches and reuses.
	newBase := commitFile(t, backendSeed, "later.txt", "later\n")
	runGit(t, backendSeed, "push", "-q", "origin", "main")
	log2, lines2 := captureLog()
	res2, err := Prepare(context.Background(), task, cred, log2, "")
	if err != nil {
		t.Fatal(err)
	}
	b := res2.Repos[0]
	if b.Cloned || b.BaseSha != newBase || b.HeadSha != res.Repos[0].HeadSha || b.Registry.TestCmd != "make test" {
		t.Fatalf("reuse = %+v", b)
	}
	if !strings.Contains(res2.Note, "- dt/backend (backend): reusing `~/ex-workspace/cliffhub/backend` (fetched), origin/main @ "+newBase) {
		t.Fatalf("note = %s", res2.Note)
	}
	if _, ok := findLog(lines2(), "workspace: fetching"); !ok {
		t.Fatal("no fetch log")
	}
	if _, ok := findLog(lines2(), "workspace: fetch failed; continuing with local refs"); ok {
		t.Fatal("fetch should have worked")
	}
}

func TestPrepareStashesDirtyWorkBeforeSwitchingBranches(t *testing.T) {
	_, root := gitSandbox(t)
	f := newForge(t)
	f.repo(t, "dt/backend", "main")
	cred := &GitCred{Host: f.host}
	repo := protocol.TaskSpecRepo{Path: "dt/backend", Role: "backend", Branch: "ex/task-1"}
	if _, err := Prepare(context.Background(), taskOf("shop", repo), cred, nil, root); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "shop", "backend")
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("edited\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("new\n"), 0o644)

	repo2 := repo
	repo2.Branch = "ex/task-2"
	res, err := Prepare(context.Background(), taskOf("shop", repo2), cred, nil, root)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Repos[0].Stashed || !strings.HasSuffix(res.Note, " — earlier uncommitted changes were stashed") {
		t.Fatalf("res = %+v", res)
	}
	if s := runGit(t, dir, "stash", "list"); !strings.Contains(s, "ex-autostash before ex/task-2") {
		t.Fatalf("stash list = %q", s)
	}
	if st := runGit(t, dir, "status", "--porcelain"); st != "" {
		t.Fatalf("tree not clean: %q", st)
	}
	// Back to the existing local branch: a plain checkout, nothing to stash.
	res, err = Prepare(context.Background(), taskOf("shop", repo), cred, nil, root)
	if err != nil || res.Repos[0].Stashed {
		t.Fatalf("res = %+v, %v", res, err)
	}
	if b := runGit(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); b != "ex/task-1" {
		t.Fatalf("on %q", b)
	}
}

func TestPrepareChecksOutABranchAnotherMachinePushed(t *testing.T) {
	_, root := gitSandbox(t)
	f := newForge(t)
	seed := f.repo(t, "dt/backend", "main")
	runGit(t, seed, "checkout", "-q", "-b", "ex/task-9")
	pushed := commitFile(t, seed, "work.txt", "work\n")
	runGit(t, seed, "push", "-q", "origin", "ex/task-9")
	res, err := Prepare(context.Background(), taskOf("shop", protocol.TaskSpecRepo{Path: "dt/backend", Role: "backend", Branch: "ex/task-9"}), &GitCred{Host: f.host}, nil, root)
	if err != nil {
		t.Fatal(err)
	}
	if r := res.Repos[0]; r.HeadSha != pushed || r.HeadSha == r.BaseSha {
		t.Fatalf("repo = %+v", r)
	}
}

func TestPrepareHonorsAnExplicitBaseBranch(t *testing.T) {
	_, root := gitSandbox(t)
	f := newForge(t)
	seed := f.repo(t, "dt/backend", "main", "develop")
	developSha := runGit(t, seed, "rev-parse", "--short", "develop")
	res, err := Prepare(context.Background(), taskOf("shop", protocol.TaskSpecRepo{Path: "dt/backend", Role: "backend", Branch: "ex/t", BaseBranch: " develop "}), &GitCred{Host: f.host}, nil, root)
	if err != nil {
		t.Fatal(err)
	}
	if r := res.Repos[0]; r.BaseBranch != "develop" || r.BaseSha != developSha || r.HeadSha != developSha || r.Registry.DefaultBranch != "develop" {
		t.Fatalf("repo = %+v", r)
	}
}

func TestPrepareFallsBackToTheUsualBaseBranches(t *testing.T) {
	_, root := gitSandbox(t)
	f := newForge(t)
	f.repo(t, "dt/legacy", "master")
	cred := &GitCred{Host: f.host}
	repo := protocol.TaskSpecRepo{Path: "dt/legacy", Role: "backend", Branch: "ex/t"}
	if _, err := Prepare(context.Background(), taskOf("old", repo), cred, nil, root); err != nil {
		t.Fatal(err)
	}
	// origin/HEAD that does not point into origin/… is ignored; the candidate
	// list finds master.
	dir := filepath.Join(root, "old", "legacy")
	runGit(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/heads/master")
	res, err := Prepare(context.Background(), taskOf("old", repo), cred, nil, root)
	if err != nil || res.Repos[0].BaseBranch != "master" {
		t.Fatalf("res = %+v, %v", res, err)
	}
}

func TestPrepareFailsWithoutAnyBaseBranch(t *testing.T) {
	_, root := gitSandbox(t)
	f := newForge(t)
	f.repo(t, "dt/odd", "trunk")
	cred := &GitCred{Host: f.host}
	repo := protocol.TaskSpecRepo{Path: "dt/odd", Role: "other", Branch: "ex/t"}
	if _, err := Prepare(context.Background(), taskOf("odd", repo), cred, nil, root); err != nil {
		t.Fatal(err) // origin/HEAD → trunk
	}
	dir := filepath.Join(root, "odd", "odd")
	runGit(t, dir, "config", "remote.origin.followRemoteHEAD", "never")
	runGit(t, dir, "remote", "set-head", "origin", "-d")
	_, err := Prepare(context.Background(), taskOf("odd", repo), cred, nil, root)
	if err == nil || err.Error() != "dt/odd: could not determine the base branch — pass base_branch when creating the task" {
		t.Fatalf("err = %v", err)
	}
}

func TestPrepareBaseBranchMissingOrFetchedOnDemand(t *testing.T) {
	_, root := gitSandbox(t)
	f := newForge(t)
	seed := f.repo(t, "dt/backend", "main")
	cred := &GitCred{Host: f.host}
	repo := protocol.TaskSpecRepo{Path: "dt/backend", Role: "backend", Branch: "ex/t", BaseBranch: "nope"}
	_, err := Prepare(context.Background(), taskOf("shop", repo), cred, nil, root)
	if err == nil || err.Error() != `dt/backend: base branch "nope" does not exist on origin` {
		t.Fatalf("err = %v", err)
	}
	// A base that would reach git as an option is refused before any fetch.
	marker := filepath.Join(t.TempDir(), "pwned")
	repo.BaseBranch = "--upload-pack=touch " + marker
	if _, err := Prepare(context.Background(), taskOf("shop", repo), cred, nil, root); err == nil || !strings.Contains(err.Error(), `refusing base branch name "--upload-pack=touch `) {
		t.Fatalf("option-like base: err = %v", err)
	}
	repo.BaseBranch = "-x"
	if _, err := Prepare(context.Background(), taskOf("shop", repo), cred, nil, root); err == nil || err.Error() != `dt/backend: refusing base branch name "-x"` {
		t.Fatalf("dash base: err = %v", err)
	}
	if exists(marker) {
		t.Fatal("git ran an option smuggled in as the base branch")
	}
	repo.BaseBranch = "nope"
	// The checkout exists now. Break its full fetch (a refspec for a branch
	// origin lacks) and add the base upstream afterwards: the general fetch
	// fails and is logged, the targeted `fetch origin <base>` brings it in.
	dir := filepath.Join(root, "shop", "backend")
	runGit(t, dir, "config", "--add", "remote.origin.fetch", "+refs/heads/missing:refs/remotes/origin/missing")
	runGit(t, seed, "checkout", "-q", "-b", "release")
	releaseSha := commitFile(t, seed, "release.txt", "r\n")
	runGit(t, seed, "push", "-q", "origin", "release")
	repo.BaseBranch = "release"
	log, lines := captureLog()
	res, err := Prepare(context.Background(), taskOf("shop", repo), cred, log, root)
	if err != nil {
		t.Fatal(err)
	}
	if r := res.Repos[0]; r.BaseBranch != "release" || r.BaseSha != releaseSha || r.HeadSha != releaseSha {
		t.Fatalf("repo = %+v", r)
	}
	l, ok := findLog(lines(), "workspace: fetch failed; continuing with local refs")
	if e, _ := l.extra["error"].(string); !ok || !strings.HasPrefix(e, "Error: Command failed: git fetch origin --prune\n") {
		t.Fatalf("fetch-failed log = %+v", l)
	}
}

func TestPrepareNeedsAConnectorToClone(t *testing.T) {
	_, root := gitSandbox(t)
	repo := protocol.TaskSpecRepo{Path: "dt/backend", Role: "backend", Branch: "ex/t"}
	want := "dt/backend: no checkout exists yet and no GitLab connector is installed — install the gitlab connector so I can clone"
	for _, cred := range []*GitCred{nil, {Token: "glpat-x"}} {
		if _, err := Prepare(context.Background(), taskOf("shop", repo), cred, nil, root); err == nil || err.Error() != want {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestPrepareKeepsUsingALegacyCheckout(t *testing.T) {
	_, root := gitSandbox(t)
	f := newForge(t)
	f.repo(t, "dt/web/backend", "main")
	legacy := filepath.Join(root, "dt", "web", "backend")
	_ = os.MkdirAll(filepath.Dir(legacy), 0o755)
	runGit(t, root, "clone", "-q", f.host+"/dt/web/backend.git", legacy)
	log, lines := captureLog()
	res, err := Prepare(context.Background(), taskOf("shop", protocol.TaskSpecRepo{Path: "dt/web/backend", Role: "backend", Branch: "ex/t"}), &GitCred{Host: f.host}, log, root)
	if err != nil {
		t.Fatal(err)
	}
	if r := res.Repos[0]; r.Dir != legacy || r.Cloned || r.Registry.Dir != legacy {
		t.Fatalf("repo = %+v", r)
	}
	if l, ok := findLog(lines(), "workspace: using legacy checkout location"); !ok || l.extra["dir"] != legacy || l.extra["repo"] != "dt/web/backend" {
		t.Fatalf("log = %+v", l)
	}
	if exists(filepath.Join(root, "shop", "backend")) {
		t.Fatal("cloned a second copy")
	}
}

func TestPrepareValidatesItsInputs(t *testing.T) {
	_, root := gitSandbox(t)
	ctx := context.Background()
	good := protocol.TaskSpecRepo{Path: "dt/backend", Role: "backend", Branch: "ex/t"}
	for key, want := range map[string]string{
		"Bad Key": `refusing to prepare a workspace for project key "Bad Key"`,
		"-shop":   `refusing to prepare a workspace for project key "-shop"`,
		"":        `refusing to prepare a workspace for project key ""`,
	} {
		if _, err := Prepare(ctx, taskOf(key, good), nil, nil, root); err == nil || err.Error() != want {
			t.Errorf("key %q: err = %v", key, err)
		}
	}
	if _, err := Prepare(ctx, nil, nil, nil, root); err == nil || err.Error() != `refusing to prepare a workspace for project key ""` {
		t.Errorf("nil task: err = %v", err)
	}
	if _, err := Prepare(ctx, taskOf("shop"), nil, nil, root); err == nil || err.Error() != "the task lists no repos" {
		t.Errorf("no repos: err = %v", err)
	}
	bad := []protocol.TaskSpecRepo{
		{Path: "noslash", Branch: "ex/t"},
		{Path: "dt/../etc", Branch: "ex/t"},
		{Path: "dt/x", Branch: "has space"},
		{Path: "dt/y", Branch: "a..b"},
		{Path: "dt/z", Branch: "--upload-pack=touch"},
	}
	_, err := Prepare(ctx, taskOf("shop", bad...), nil, nil, root)
	want := `noslash: refusing repo path "noslash"; dt/../etc: refusing repo path "dt/../etc"; dt/x: refusing branch name "has space"; dt/y: refusing branch name "a..b"; dt/z: refusing branch name "--upload-pack=touch"`
	if err == nil || err.Error() != want {
		t.Errorf("all bad: err = %v", err)
	}
}

func TestPrepareReportsAPartialFailurePerRepo(t *testing.T) {
	_, root := gitSandbox(t)
	f := newForge(t)
	f.repo(t, "dt/backend", "main")
	log, lines := captureLog()
	res, err := Prepare(context.Background(), taskOf("shop",
		protocol.TaskSpecRepo{Path: "dt/backend", Role: "backend", Branch: "ex/t"},
		protocol.TaskSpecRepo{Path: "dt/missing", Role: "frontend", Branch: "ex/t", BaseBranch: "main"},
	), &GitCred{Host: f.host}, log, root)
	if err != nil {
		t.Fatal(err)
	}
	m := res.Repos[1]
	dir := filepath.Join(root, "shop", "missing")
	if m.Error == "" || m.Dir != dir || m.Registry.Dir != dir || m.BaseBranch != "main" || m.Role != "frontend" || m.Cloned {
		t.Fatalf("failed repo = %+v", m)
	}
	wantPrefix := "Command failed: git clone --depth 50 --no-single-branch " + f.host + "/dt/missing.git " + dir + "\n"
	if !strings.HasPrefix(m.Error, wantPrefix) {
		t.Fatalf("error = %q", m.Error)
	}
	if !strings.Contains(res.Note, "\n- ⚠️ dt/missing: "+m.Error) || !strings.Contains(res.Note, "- dt/backend (backend): cloned into") {
		t.Fatalf("note = %s", res.Note)
	}
	if l, ok := findLog(lines(), "workspace: repo preparation failed"); !ok || l.extra["repo"] != "dt/missing" || l.extra["error"] != m.Error {
		t.Fatalf("log = %+v", l)
	}
}

func TestPrepareSurfacesGitAndDiskFailures(t *testing.T) {
	_, root := gitSandbox(t)
	f := newForge(t)
	f.repo(t, "dt/backend", "main")
	cred := &GitCred{Host: f.host}
	ctx := context.Background()
	repo := protocol.TaskSpecRepo{Path: "dt/backend", Role: "backend", Branch: "ex/t1"}
	if _, err := Prepare(ctx, taskOf("shop", repo), cred, nil, root); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "shop", "backend")
	lock := filepath.Join(dir, ".git", "index.lock")

	// A held index lock: the autostash of dirty work fails…
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("dirty\n"), 0o644)
	_ = os.WriteFile(lock, nil, 0o644)
	repo.Branch = "ex/t2"
	_, err := Prepare(ctx, taskOf("shop", repo), cred, nil, root)
	if err == nil || !strings.HasPrefix(err.Error(), "dt/backend: Command failed: git stash push -u -m ex-autostash before ex/t2\n") {
		t.Fatalf("stash: err = %v", err)
	}
	// …and so does the checkout of a clean tree.
	_ = os.Remove(lock)
	runGit(t, dir, "checkout", "--", "README.md")
	_ = os.WriteFile(lock, nil, 0o644)
	_, err = Prepare(ctx, taskOf("shop", repo), cred, nil, root)
	if err == nil || !strings.HasPrefix(err.Error(), "dt/backend: Command failed: git checkout -B ex/t2 origin/main\n") {
		t.Fatalf("checkout: err = %v", err)
	}
	_ = os.Remove(lock)

	// The registry cannot be written.
	reg := filepath.Join(root, "registry.json")
	_ = os.Remove(reg)
	_ = os.Mkdir(reg, 0o755)
	_, err = Prepare(ctx, taskOf("shop", repo), cred, nil, root)
	if err == nil || !strings.Contains(err.Error(), "registry.json") {
		t.Fatalf("registry: err = %v", err)
	}

	// The project folder cannot be made.
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o644)
	if _, err := Prepare(ctx, taskOf("shop", repo), cred, nil, file); err == nil {
		t.Fatal("want mkdir error")
	}
}

// --- BranchHasChanges / PushBranch ---

// preparedRepo clones dt/backend into a fresh workspace on branch ex/task-1.
func preparedRepo(t *testing.T) (dir, remote string, f *forge) {
	t.Helper()
	_, root := gitSandbox(t)
	f = newForge(t)
	f.repo(t, "dt/backend", "main")
	if _, err := Prepare(context.Background(), taskOf("shop", protocol.TaskSpecRepo{Path: "dt/backend", Role: "backend", Branch: "ex/task-1"}), &GitCred{Host: f.host}, nil, root); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "shop", "backend"), filepath.Join(f.dir, "dt", "backend.git"), f
}

func TestBranchHasChanges(t *testing.T) {
	dir, _, _ := preparedRepo(t)
	ctx := context.Background()
	if changed, err := BranchHasChanges(ctx, dir, "main", nil); err != nil || changed {
		t.Fatalf("untouched: %v, %v", changed, err)
	}
	commitFile(t, dir, "fix.txt", "fix\n")
	if changed, err := BranchHasChanges(ctx, dir, "main", &GitCred{}); err != nil || !changed {
		t.Fatalf("committed: %v, %v", changed, err)
	}
	// Any git failure reads as "no changes", as in TS…
	if changed, err := BranchHasChanges(ctx, dir, "nope", nil); err != nil || changed {
		t.Fatalf("bad base: %v, %v", changed, err)
	}
	// …unless the context ended.
	done, cancel := context.WithCancel(ctx)
	cancel()
	if changed, err := BranchHasChanges(done, dir, "main", nil); !errors.Is(err, context.Canceled) || changed {
		t.Fatalf("cancelled: %v, %v", changed, err)
	}
}

func TestPushBranch(t *testing.T) {
	dir, remote, _ := preparedRepo(t)
	ctx := context.Background()
	sha := commitFile(t, dir, "fix.txt", "fix\n")
	log, lines := captureLog()
	got, err := PushBranch(ctx, dir, "dt/backend", "ex/task-1", GitCred{}, log)
	if err != nil || got != sha {
		t.Fatalf("push = %q, %v", got, err)
	}
	if r := runGit(t, remote, "rev-parse", "--short", "ex/task-1"); r != sha {
		t.Fatalf("remote has %q", r)
	}
	if u := runGit(t, dir, "rev-parse", "--abbrev-ref", "@{u}"); u != "origin/ex/task-1" {
		t.Fatalf("upstream = %q", u)
	}
	if l, ok := findLog(lines(), "workspace: pushing"); !ok || l.extra["dir"] != dir || l.extra["branch"] != "ex/task-1" {
		t.Fatalf("log = %+v", l)
	}
	if changed, _ := BranchHasChanges(ctx, dir, "main", nil); !changed {
		t.Fatal("pushed branch should still differ from main")
	}
}

func TestPushBranchRefusesADirtyTree(t *testing.T) {
	dir, _, _ := preparedRepo(t)
	for i := 0; i < 25; i++ {
		_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.txt", i)), nil, 0o644)
	}
	_, err := PushBranch(context.Background(), dir, "dt/backend", "ex/task-1", GitCred{}, nil)
	if err == nil {
		t.Fatal("want dirty error")
	}
	lines := strings.Split(err.Error(), "\n")
	if lines[0] != "uncommitted changes in the workspace — commit them first:" || len(lines) != 21 || lines[1] != "?? f00.txt" || lines[20] != "?? f19.txt" {
		t.Fatalf("err = %q", err)
	}
}

func TestPushBranchRefusesTheWrongBranch(t *testing.T) {
	dir, _, _ := preparedRepo(t)
	runGit(t, dir, "checkout", "-q", "main")
	_, err := PushBranch(context.Background(), dir, "dt/backend", "ex/task-1", GitCred{}, nil)
	if err == nil || err.Error() != "HEAD is on main, not the task branch ex/task-1" {
		t.Fatalf("err = %v", err)
	}
}

func TestPushBranchSurfacesGitFailures(t *testing.T) {
	gitSandbox(t)
	ctx := context.Background()
	// Not a repo: status fails.
	_, err := PushBranch(ctx, t.TempDir(), "dt/backend", "ex/t", GitCred{}, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "Command failed: git status --porcelain\n") {
		t.Fatalf("status: err = %v", err)
	}
	// An empty repo: clean, but HEAD names no commit.
	empty := t.TempDir()
	runGit(t, empty, "init", "-q")
	_, err = PushBranch(ctx, empty, "dt/backend", "ex/t", GitCred{}, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "Command failed: git rev-parse --abbrev-ref HEAD\n") {
		t.Fatalf("rev-parse: err = %v", err)
	}
	// The remote is gone: push fails.
	dir, remote, _ := preparedRepo(t)
	commitFile(t, dir, "fix.txt", "fix\n")
	_ = os.RemoveAll(remote)
	_, err = PushBranch(ctx, dir, "dt/backend", "ex/task-1", GitCred{}, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "Command failed: git push -u origin ex/task-1\n") {
		t.Fatalf("push: err = %v", err)
	}
}

// --- the credential, end to end over smart HTTP ---

// smartHTTPForge serves the forge's bare repos through `git http-backend`
// behind HTTP basic auth that accepts only oauth2:<token> — what GitLab does
// with a personal access token.
func smartHTTPForge(t *testing.T, f *forge, token string) *httptest.Server {
	t.Helper()
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git --exec-path:", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if !exists(backend) {
		t.Skip("git-http-backend not installed")
	}
	h := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + f.dir, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + os.Getenv("HOME")}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "oauth2" || p != token {
			w.Header().Set("WWW-Authenticate", `Basic realm="gitlab"`)
			http.Error(w, "HTTP Basic: Access denied", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTheTokenReachesGitOnlyThroughTheEnvironment(t *testing.T) {
	_, root := gitSandbox(t)
	f := newForge(t)
	f.repo(t, "dt/backend", "main")
	runGit(t, filepath.Join(f.dir, "dt", "backend.git"), "config", "http.receivepack", "true")
	const token = "glpat-s3cret-token"
	srv := smartHTTPForge(t, f, token)
	ctx := context.Background()
	repo := protocol.TaskSpecRepo{Path: "dt/backend", Role: "backend", Branch: "ex/task-1"}

	// Anonymous: git may not prompt, so it fails at once.
	_, err := Prepare(ctx, taskOf("shop", repo), &GitCred{Host: srv.URL}, nil, root)
	if err == nil || !strings.Contains(err.Error(), "terminal prompts disabled") {
		t.Fatalf("anonymous: err = %v", err)
	}
	// A wrong token is refused.
	_, err = Prepare(ctx, taskOf("shop", repo), &GitCred{Host: srv.URL, Token: "glpat-wrong"}, nil, root)
	if err == nil || !strings.Contains(err.Error(), "Authentication failed") || strings.Contains(err.Error(), "glpat-wrong") {
		t.Fatalf("wrong token: err = %v", err)
	}
	// The user's own credential helper (osxkeychain, store…) must never see
	// the run's token — not asked for it, and never told to store it.
	helperLog := filepath.Join(t.TempDir(), "helper.log")
	runGit(t, t.TempDir(), "config", "--global", "credential.helper", "!f() { echo \"$1\" >> '"+helperLog+"'; cat >> '"+helperLog+"'; }; f")
	// The right token clones…
	cred := &GitCred{Host: srv.URL, Token: token}
	res, err := Prepare(ctx, taskOf("shop", repo), cred, nil, root)
	if err != nil {
		t.Fatal(err)
	}
	dir := res.Repos[0].Dir
	// …and is written nowhere: not in the remote URL, the repo config, or the registry.
	if u := runGit(t, dir, "remote", "get-url", "origin"); u != srv.URL+"/dt/backend.git" {
		t.Fatalf("origin = %q", u)
	}
	for _, p := range []string{filepath.Join(dir, ".git", "config"), filepath.Join(root, "registry.json")} {
		if b, _ := os.ReadFile(p); strings.Contains(string(b), token) {
			t.Fatalf("token written to %s", p)
		}
	}
	// Fetch and push use it too.
	if _, err := Prepare(ctx, taskOf("shop", repo), cred, nil, root); err != nil {
		t.Fatal(err)
	}
	sha := commitFile(t, dir, "fix.txt", "fix\n")
	if got, err := PushBranch(ctx, dir, "dt/backend", "ex/task-1", *cred, nil); err != nil || got != sha {
		t.Fatalf("push = %q, %v", got, err)
	}
	if changed, err := BranchHasChanges(ctx, dir, "main", cred); err != nil || !changed {
		t.Fatalf("changed = %v, %v", changed, err)
	}
	// Exported Git with the credential.
	if out, err := Git(ctx, dir, []string{"ls-remote", "origin", "ex/task-1"}, cred); err != nil || !strings.Contains(out, "refs/heads/ex/task-1") {
		t.Fatalf("ls-remote = %q, %v", out, err)
	}
	if b, _ := os.ReadFile(helperLog); strings.Contains(string(b), token) || strings.Contains(string(b), "store") {
		t.Fatalf("the user's credential helper saw the token:\n%s", b)
	}
}
