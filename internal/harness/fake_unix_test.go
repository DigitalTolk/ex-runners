//go:build unix

package harness

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

// searchPath is what the fakes need to find cat/env/sleep.
const searchPath = "/usr/bin:/bin"

// fakeCLI is a stand-in `claude`/`codex`: a shell script that records its
// argv (NUL-separated), environment, working directory and stdin into dir,
// prints dir/stdout.jsonl, then runs the test's tail commands.
type fakeCLI struct {
	t   *testing.T
	dir string
	bin string
}

// fakeSpec describes a fake. pre runs before anything is recorded (the
// codex `exec --help` probe lives there); tail runs after stdout is printed.
type fakeSpec struct {
	name   string
	pre    string
	stdout string
	tail   string
	files  map[string]string // extra files in the fake's dir (help.txt, …)
}

func newFake(t *testing.T, s fakeSpec) *fakeCLI {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{"stdout.jsonl": s.stdout}
	for name, body := range s.files {
		files[name] = body
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\n" +
		"D=" + shQuote(dir) + "\n" +
		s.pre + "\n" +
		`printf '%s\0' "$@" > "$D/argv"` + "\n" +
		`env > "$D/env"` + "\n" +
		`pwd -P > "$D/cwd"` + "\n" +
		`cat > "$D/stdin"` + "\n" +
		`cat "$D/stdout.jsonl"` + "\n" +
		s.tail + "\n"
	bin := filepath.Join(dir, s.name)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &fakeCLI{t: t, dir: dir, bin: bin}
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (f *fakeCLI) read(name string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		f.t.Fatalf("fake %s: %v", name, err)
	}
	return string(b)
}

func (f *fakeCLI) ran() bool {
	_, err := os.Stat(filepath.Join(f.dir, "argv"))
	return err == nil
}

func (f *fakeCLI) argv() []string {
	return strings.Split(strings.TrimSuffix(f.read("argv"), "\x00"), "\x00")
}

func (f *fakeCLI) env() map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(f.read("env"), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			m[k] = v
		}
	}
	return m
}

func (f *fakeCLI) cwd() string { return strings.TrimSpace(f.read("cwd")) }

// waitFile polls until the fake wrote name (a pid file, say).
func (f *fakeCLI) waitFile(name string) string {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(filepath.Join(f.dir, name)); err == nil && bytes.HasSuffix(b, []byte("\n")) {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("fake never wrote %s", name)
	return ""
}

func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// alive reports whether pid still exists (a zombie counts as gone once its
// parent reaps it; poll with waitDead).
func alive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d survived", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitOutcome bounds Wait so a hung harness fails the test instead of the suite.
func waitOutcome(t *testing.T, r Running) protocol.RunOutcome {
	t.Helper()
	ch := make(chan protocol.RunOutcome, 1)
	go func() { ch <- r.Wait() }()
	select {
	case o := <-ch:
		return o
	case <-time.After(30 * time.Second):
		t.Fatal("harness never finished")
	}
	return protocol.RunOutcome{}
}
