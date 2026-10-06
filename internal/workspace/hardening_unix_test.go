//go:build !windows

package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

// The agent can edit anything inside a checkout without a card — including
// .git/config and .git/hooks. These tests plant what a prompt-injected agent
// would, then run the runner's own git paths.

func TestTamperedRepoConfigIsRefusedBeforeFetchAndPush(t *testing.T) {
	for _, c := range []struct{ name, key, val string }{
		{"insteadOf sends the remote elsewhere", "url.https://evil.example/.insteadOf", "file://"},
		{"a helper of the checkout's own", "credential.helper", "!cat >/dev/null"},
		{"a proxy", "http.proxy", "http://evil.example:8080"},
		{"an extra header", "http.extraHeader", "X-Leak: 1"},
		{"a clean filter", "filter.x.clean", "touch /tmp/x"},
		{"an include", "include.path", "/tmp/evil.config"},
		{"a separate push URL", "remote.origin.pushurl", "https://evil.example/x.git"},
		{"an ssh command", "core.sshCommand", "touch /tmp/x"},
		{"origin moved", "remote.origin.url", "https://evil.example/dt/backend.git"},
		{"hidden worktree config", "extensions.worktreeConfig", "true"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, _, f := preparedRepo(t)
			runGit(t, dir, "config", c.key, c.val)
			cred := &GitCred{Host: f.host, Token: "glpat-x"}
			want := "its .git/config sets " + strings.ToLower(c.key)
			_, err := Prepare(context.Background(), taskOf("shop", protocol.TaskSpecRepo{Path: "dt/backend", Role: "backend", Branch: "ex/task-1"}), cred, nil, filepath.Dir(filepath.Dir(dir)))
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), want) {
				t.Fatalf("Prepare: err = %v", err)
			}
			_, err = PushBranch(context.Background(), dir, "dt/backend", "ex/task-1", *cred, nil)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), want) {
				t.Fatalf("PushBranch: err = %v", err)
			}
		})
	}
}

// What real checkouts carry — husky's hooksPath, LFS, other remotes, branch
// tracking, user identity — must keep working with a credential.
func TestOrdinaryCheckoutSettingsStillWork(t *testing.T) {
	dir, _, f := preparedRepo(t)
	for _, kv := range [][2]string{
		{"core.hooksPath", ".husky/_"}, {"user.name", "Agent"}, {"remote.upstream.url", "https://elsewhere.example/x.git"},
		{"remote.upstream.pushurl", "https://elsewhere.example/x.git"}, {"branch.ex/task-1.remote", "origin"},
		{"filter.lfs.clean", "git-lfs clean -- %f"}, {"filter.lfs.smudge", "git-lfs smudge -- %f"},
		{"filter.lfs.process", "git-lfs filter-process"}, {"filter.lfs.required", "true"}, {"core.fsmonitor", "false"},
	} {
		runGit(t, dir, "config", kv[0], kv[1])
	}
	commitFile(t, dir, "fix.txt", "fix\n")
	cred := GitCred{Host: f.host, Token: "glpat-x"}
	for i := 0; i < 2; i++ { // push -u writes branch tracking; a second push must still pass
		if _, err := PushBranch(context.Background(), dir, "dt/backend", "ex/task-1", cred, nil); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	if _, err := Prepare(context.Background(), taskOf("shop", protocol.TaskSpecRepo{Path: "dt/backend", Role: "backend", Branch: "ex/task-1"}), &cred, nil, filepath.Dir(filepath.Dir(dir))); err != nil {
		t.Fatal(err)
	}
	// A non-standard LFS filter command is not the LFS filter.
	runGit(t, dir, "config", "filter.lfs.smudge", "touch /tmp/x")
	if err := checkRepoConfig(context.Background(), dir, ""); err == nil {
		t.Fatal("tampered lfs filter passed")
	}
	// Without a credential nothing is checked (nothing to leak).
	runGit(t, dir, "config", "http.proxy", "http://evil.example:8080")
	if _, err := PushBranch(context.Background(), dir, "dt/backend", "ex/task-1", GitCred{}, nil); err != nil {
		t.Fatalf("anonymous push: %v", err)
	}
}

func TestHooksNeverSeeTheToken(t *testing.T) {
	dir, _, f := preparedRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	for _, hook := range []string{"pre-push", "post-checkout", "reference-transaction", "post-merge"} {
		p := filepath.Join(dir, ".git", "hooks", hook)
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho "+hook+" \"$EX_GIT_TOKEN\" >> '"+marker+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cred := &GitCred{Host: f.host, Token: "glpat-s3cret"}
	if _, err := Prepare(context.Background(), taskOf("shop", protocol.TaskSpecRepo{Path: "dt/backend", Role: "backend", Branch: "ex/task-1"}), cred, nil, filepath.Dir(filepath.Dir(dir))); err != nil {
		t.Fatal(err)
	}
	commitFile(t, dir, "fix.txt", "fix\n")
	if _, err := PushBranch(context.Background(), dir, "dt/backend", "ex/task-1", *cred, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(marker)
	if strings.Contains(string(b), "s3cret") || strings.Contains(string(b), "pre-push") {
		t.Fatalf("a hook ran with the token present:\n%s", b)
	}
}

func TestLFSObjectsArePushedWhenHooksAreOff(t *testing.T) {
	dir, _, f := preparedRepo(t)
	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "lfs-calls")
	lfs := "#!/bin/sh\necho \"$*|${EX_GIT_TOKEN:+token}\" >> '" + calls + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "git-lfs"), []byte(lfs), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	_ = os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o644)
	runGit(t, dir, "add", ".gitattributes")
	runGit(t, dir, "commit", "-q", "-m", "lfs")
	if _, err := PushBranch(context.Background(), dir, "dt/backend", "ex/task-1", GitCred{Host: f.host, Token: "glpat-x"}, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(calls); strings.TrimSpace(string(b)) != "push origin ex/task-1|token" {
		t.Fatalf("git-lfs calls = %q", b)
	}
	// A failing LFS upload stops the push.
	_ = os.WriteFile(filepath.Join(bin, "git-lfs"), []byte("#!/bin/sh\nexit 2\n"), 0o755)
	if _, err := PushBranch(context.Background(), dir, "dt/backend", "ex/task-1", GitCred{Host: f.host, Token: "glpat-x"}, nil); err == nil || !strings.Contains(err.Error(), "git lfs push origin ex/task-1") {
		t.Fatalf("err = %v", err)
	}
	if !usesLFS(dir) {
		t.Error("usesLFS")
	}
	other := t.TempDir()
	_ = os.WriteFile(filepath.Join(other, ".lfsconfig"), nil, 0o644)
	if !usesLFS(other) || usesLFS(t.TempDir()) {
		t.Error("usesLFS .lfsconfig / none")
	}
}

func TestTheHelperAnswersOnlyTheConnectorsHost(t *testing.T) {
	gitSandbox(t)
	cred := &GitCred{Host: "https://gitlab.example.com", Token: "glpat-s3cret"}
	fill := func(input string) (string, error) {
		cmd := exec.Command("git", "credential", "fill")
		cmd.Dir = t.TempDir()
		cmd.Env = GitEnv(cred, nil)
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := fill("protocol=https\nhost=gitlab.example.com\n\n"); err != nil || !strings.Contains(out, "password=glpat-s3cret") {
		t.Fatalf("own host: %q, %v", out, err)
	}
	for _, other := range []string{
		"protocol=https\nhost=evil.example\n\n",
		"protocol=http\nhost=gitlab.example.com\n\n",          // downgraded scheme
		"protocol=https\nhost=gitlab.example.com:8443\n\n",    // another port
		"protocol=https\nhost=gitlab.example.com.evil.io\n\n", // suffix trick
	} {
		// No answer → git would prompt, and prompts are off: it fails, token-free.
		if out, err := fill(other); err == nil || strings.Contains(out, "glpat-s3cret") {
			t.Errorf("%q: %q, %v", other, out, err)
		}
	}
	// store/erase requests are ignored (nothing is echoed back or written).
	cmd := exec.Command("git", "credential", "approve")
	cmd.Dir = t.TempDir()
	cmd.Env = GitEnv(cred, nil)
	cmd.Stdin = strings.NewReader("protocol=https\nhost=gitlab.example.com\nusername=oauth2\npassword=glpat-s3cret\n\n")
	if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
		t.Errorf("approve: %q, %v", out, err)
	}
}

func TestSafeBranch(t *testing.T) {
	for _, ok := range []string{"main", "ex/task-1", "release/1.2", "feat_x", "a.b"} {
		if !safeBranch(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{
		"", "-x", "--upload-pack=x", "a..b", "has space", "HEAD", "refs/heads/x", "origin/main",
		"x.lock", "a/b.lock/c", "trailing/", "trailing.", "a//b", ".hidden", "a/.b", "@", "a@{1}",
	} {
		if safeBranch(bad) {
			t.Errorf("%q admitted", bad)
		}
	}
}

func TestCheckRepoConfigOutsideARepo(t *testing.T) {
	gitSandbox(t)
	if err := checkRepoConfig(context.Background(), t.TempDir(), ""); err == nil || !strings.HasPrefix(err.Error(), "Command failed: git config --local --list -z\n") {
		t.Fatalf("err = %v", err)
	}
}
