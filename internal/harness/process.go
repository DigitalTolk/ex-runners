package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/DigitalTolk/ex-runners/internal/protocol"
	"github.com/DigitalTolk/ex-runners/internal/textutil"
)

// orNop makes a nil logger safe to call.
func orNop(l protocol.Logger) protocol.Logger {
	if l == nil {
		return protocol.NopLogger
	}
	return l
}

// run is the Running both adapters return: a kill switch that remembers its
// reason, the child's pid once spawned, and the outcome once finished.
type run struct {
	log protocol.Logger

	mu           sync.Mutex
	killedReason string
	pid          int

	done chan struct{}
	out  protocol.RunOutcome
}

func newRun(log protocol.Logger) *run { return &run{log: log, done: make(chan struct{})} }

func (r *run) Wait() protocol.RunOutcome {
	<-r.done
	return r.out
}

// Kill records the reason (first one wins) and kills the tree if the child
// already exists; a child spawned later sees the reason and is killed then.
func (r *run) Kill(reason string) {
	r.mu.Lock()
	if r.killedReason != "" {
		r.mu.Unlock()
		return
	}
	r.killedReason = reason
	pid := r.pid
	r.mu.Unlock()
	KillTree(pid, r.log)
}

// started records the child's pid and returns the kill reason if Kill ran
// before the child existed.
func (r *run) started(pid int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pid = pid
	return r.killedReason
}

func (r *run) killed() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.killedReason
}

func (r *run) finish(o protocol.RunOutcome) {
	r.out = o
	close(r.done)
}

// runnerError is what the TS runner reported when a harness adapter threw
// before the CLI ever started (writing its per-run config): run.ts logged
// "run execution error" and failed the run as `runner_error: …` — never an
// infra retry. The Go adapters cannot throw, so they report it as the
// outcome themselves.
func runnerError(a protocol.Assignment, err error, log protocol.Logger) protocol.RunOutcome {
	log("run execution error", map[string]any{"runID": a.RunID, "error": err.Error()})
	return protocol.RunOutcome{Reason: "runner_error: " + err.Error()}
}

// cwdOf is `opts.cwd ?? opts.workDir`.
func cwdOf(opts RunOptions) string {
	if opts.Cwd != "" {
		return opts.Cwd
	}
	return opts.WorkDir
}

// harnessEnv is `{...process.env, PATH: searchPath, ...extra}`: os/exec keeps
// the last value of a duplicated key, so appending overrides.
func harnessEnv(searchPath string, extra ...string) []string {
	return append(append(os.Environ(), "PATH="+searchPath), extra...)
}

// stderrKeep bounds the stderr kept for the failure log. The log shows the
// first 500 characters; 4 KiB always holds at least that many.
const stderrKeep = 4096

// capWriter keeps the head of a stream and discards the rest (the child must
// never block on a full stderr pipe).
type capWriter struct{ buf []byte }

func (w *capWriter) Write(p []byte) (int, error) {
	if room := stderrKeep - len(w.buf); room > 0 {
		w.buf = append(w.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// head is the first 500 characters of stderr, as the TS failure log kept.
func (w *capWriter) head() string { return textutil.Head(string(w.buf), 500) }

// spawn starts one CLI in its own process group so cancellation kills the
// whole tree — CLIs spawn children, and a stray MCP server or helper
// outliving the run is a leak (plan-v2 §6). The prompt goes over stdin
// (argv has platform size limits and shows in `ps`; the bundle can be tens
// of KB).
func spawn(bin string, args []string, dir string, env []string, stdin string) (*exec.Cmd, io.Reader, *capWriter, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	setProcAttr(cmd)
	stdout, _ := cmd.StdoutPipe() // cannot fail: Stdout is unset and the command not started
	stderr := &capWriter{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	return cmd, stdout, stderr, nil
}

// readLines feeds fn every line of r, split the way Node's readline split
// them: at \n, \r or \r\n, with a final unterminated line delivered at EOF.
// Lines have no length cap — one tool result can be megabytes on one line.
func readLines(r io.Reader, fn func([]byte)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), math.MaxInt)
	sc.Split(splitLines)
	for sc.Scan() {
		fn(sc.Bytes())
	}
	// Should the scanner ever stop early, keep draining: a child blocked on
	// a full stdout pipe would never exit.
	_, _ = io.Copy(io.Discard, r)
}

func splitLines(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		if data[i] == '\n' {
			return i + 1, data[:i], nil
		}
		// \r: a \n right behind it belongs to the same break.
		if i+1 < len(data) {
			if data[i+1] == '\n' {
				return i + 2, data[:i], nil
			}
			return i + 1, data[:i], nil
		}
		if !atEOF {
			return 0, nil, nil // need the next byte to know
		}
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// decodeEvent parses one JSONL line. Like JSON.parse in the TS adapters, a
// line that is not JSON is noise (false); a field of an unexpected type is
// left unset rather than discarding the whole event (TS read the fields it
// knew and ignored what did not fit).
func decodeEvent(line []byte, v any) bool {
	err := json.Unmarshal(line, v)
	var typeErr *json.UnmarshalTypeError
	return err == nil || errors.As(err, &typeErr)
}

// exitCodeValue is the code the TS 'close' handler saw: null for a child
// that died by a signal (ExitCode reports -1 then).
func exitCodeValue(code int) any {
	if code < 0 {
		return nil
	}
	return code
}

// exitReason is `harness_exit_${String(code)}`.
func exitReason(code int) string {
	if code < 0 {
		return "harness_exit_null"
	}
	return "harness_exit_" + strconv.Itoa(code)
}

// jsonBytes encodes like JSON.stringify: no HTML escaping, no trailing
// newline. Only used for values that cannot fail to encode.
func jsonBytes(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}
