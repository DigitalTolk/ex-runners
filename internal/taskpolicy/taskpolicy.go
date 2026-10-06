// Package taskpolicy is the task permission profile (plan-coding-agent.md):
// inside a coding task's checkout, the routine work of coding — reading and
// editing files, running the package manager, tests, linters, local git — is
// auto-approved instead of raising an approval card per command. Anything
// that reaches OUTSIDE the workspace (other paths, the network beyond
// connectors, `git push`, destructive or privileged commands) still asks the
// requester.
//
// This decides auto-APPROVE vs ASK — never auto-deny. A false "ask" costs a
// click; a false "allow" would cost trust, so the rules lean conservative and
// every auto-approval is still visible in the run timeline.
//
// The policy is a defense against prompt injection: an issue, README or test
// output steering the agent toward credentials (~/.ex-runner, ~/.ssh, …) or
// toward code that runs outside the workspace.
//
// History and divergence: this began as a byte-for-byte port of the TS
// taskPolicyAllows. A security review then found bypasses the regex design
// could not see — paths through $HOME/$VAR/../symlinks, writes into .git,
// git global options, inline interpreters, wrappers, quoted or path-invoked
// programs — so the Go policy is now deliberately STRICTER than the TS
// original. Bash commands get two passes:
//
//  1. the TS regex rules (alwaysAsk) and the TS absolute-path scan
//     (pathsIn), unchanged, over the raw text;
//  2. a token-level analysis: a small shell lexer (quotes, escapes, $(…),
//     backticks, <(…), heredocs, redirections, operators) feeds an analyzer
//     that judges every simple command by its real program name — through
//     env/time/nohup/timeout/… wrappers and inside every command
//     substitution — and resolves every path argument against each directory
//     the shell may be in, following symlinks.
//
// Port notes for pass 1: the rules were written as JavaScript regexes. RE2's
// \s is narrower than JavaScript's and its "." wider, so every pattern goes
// through jsRegexp, which restores the JavaScript meaning (keep using it for
// new patterns); the one backreference (the here-doc quote pairing) is matched
// by hand in heredocMarker. Paths follow Node's path.resolve/basename,
// trimming follows String.prototype.trim and the length cap counts UTF-16
// code units, exactly as the TS did. The lexer splits words on JavaScript
// whitespace too, so a U+00A0 can never glue "curl" to its argument.
package taskpolicy

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/DigitalTolk/ex-runners/internal/textutil"
)

// Context is where a coding task runs (TS TaskPolicyContext).
type Context struct {
	TaskDir  string `json:"taskDir"`  // the project checkout the harness runs in
	StateDir string `json:"stateDir"` // the runner's per-thread state dir (mcp.json, connectors docs)
}

// Decision is auto-approve (Allow) or ask, with the reason the approval card
// shows (TS PolicyDecision).
type Decision struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason"`
}

// Commands whose first word makes a shell segment routine coding work.
var allowedBins = setOf(
	// package managers / runtimes
	"npm", "npx", "pnpm", "yarn", "bun", "node", "deno", "tsx", "ts-node",
	"pip", "pip3", "python", "python3", "poetry", "uv", "pytest", "pipenv",
	"go", "gofmt", "goimports", "cargo", "rustc", "rustfmt",
	"composer", "php", "artisan", "phpunit", "pest",
	"bundle", "ruby", "rake", "rails", "gem",
	"mvn", "gradle", "gradlew", "java", "javac", "kotlin",
	"dotnet", "swift", "make", "cmake", "ninja",
	// test/lint/build tools
	"vitest", "jest", "mocha", "eslint", "prettier", "tsc", "vite", "webpack", "esbuild", "rollup",
	"ruff", "black", "flake8", "mypy", "golangci-lint", "staticcheck",
	// read-only inspection
	"ls", "cat", "head", "tail", "less", "more", "grep", "egrep", "fgrep", "rg", "ag", "find", "fd",
	"sed", "awk", "wc", "sort", "uniq", "cut", "tr", "diff", "jq", "yq", "tree", "file", "stat",
	"echo", "printf", "pwd", "which", "type", "test", "true", "false", "env", "printenv", "date", "basename", "dirname",
	"du", "df", "sleep", "lsof", "ps", "paste", "column", "nl",
	// local dev-database clients (the data is the checkout's own dev store)
	"pg_isready", "psql", "mysql", "sqlite3", "redis-cli",
	// file ops (paths are checked separately)
	"mkdir", "touch", "cp", "mv", "rm", "ln", "chmod",
	// vcs
	"git",
	// local containers (docker run is caught above)
	"docker", "docker-compose",
	// shells running a script file (`sh -c` and stdin scripts still ask)
	"sh", "bash", "zsh",
)

type askRule struct {
	re  *regexp.Regexp
	why string
}

// Patterns that always ask, whatever else the command looks like. The
// sources are the TS regexes verbatim; jsRegexp gives them JavaScript's \s.
var alwaysAsk = []askRule{
	{jsRegexp(`(^|[\s;&|])sudo(\s|$)`), "privilege escalation"},
	{jsRegexp(`(^|[\s;&|])su(\s|$)`), "privilege escalation"},
	{jsRegexp(`\bgit\s+push\b`), "git push is the MR step (request_mr)"},
	{jsRegexp(`\bgit\s+remote\s+(add|set-url|remove|rm)\b`), "changing remotes"},
	{jsRegexp(`\bgit\s+(reset\s+--hard|clean\s+-[a-zA-Z]*[fdx]|checkout\s+--\s+\.|restore\s+(--staged\s+)?\.|branch\s+-D|push\s+--force)`), "mass-discarding local work"},
	{jsRegexp(`\brm\s+(-[a-zA-Z]*\s+)*(\/|~|\$HOME|\.\.)`), "rm outside the workspace"},
	{jsRegexp(`\b(curl|wget)\b[^|]*\|\s*(sh|bash|zsh|python|node)\b`), "piping downloads into a shell"},
	{jsRegexp(`(^|[\s;&|])(curl|wget|nc|ncat|telnet|ssh|scp|sftp|rsync)(\s|$)`), "network beyond connectors"},
	// docker compose / docker <inspect|logs|…> inside the checkout is how most
	// projects come up locally (the requester asked for docker-first setup);
	// bare `docker run` can mount anything and still asks.
	{jsRegexp(`(^|[\s;&|])docker\s+run(\s|$)`), "docker run can mount arbitrary paths"},
	{jsRegexp(`(^|[\s;&|])(kubectl|helm|terraform|aws|gcloud|az|vercel|netlify|heroku|fly|flyctl)(\s|$)`), "infrastructure tooling"},
	{jsRegexp(`(^|[\s;&|])(gh|glab)(\s|$)`), "forge CLI (use request_mr)"},
	{jsRegexp(`\bnpm\s+publish\b|\byarn\s+publish\b|\bpnpm\s+publish\b|\btwine\b|\bcargo\s+publish\b|\bgem\s+push\b`), "publishing packages"},
	{jsRegexp(`(^|[\s;&|])(shutdown|reboot|halt|mkfs|fdisk|diskutil|launchctl|systemctl|crontab|open|xdg-open|osascript)(\s|$)`), "system-level action"},
	{jsRegexp(`\bkill\s+-9\s+-1\b|\bkillall\b|\bpkill\b`), "killing arbitrary processes"},
	{jsRegexp(`(^|[\s;&|])dd(\s|$)`), "raw disk write"},
	{jsRegexp(`\bchmod\s+[0-7]*[2367][0-7]*\s+\/`), "chmod on system paths"},
	{jsRegexp(`>\s*\/(etc|usr|bin|sbin|var|System|Library)\/`), "writing to system paths"},
	{jsRegexp(`\$\(\s*(curl|wget)\b`), "downloading into a command"},
}

// jsDot is JavaScript's "." without the s flag: anything but a line
// terminator (RE2's "." only excludes \n).
const jsDot = `[^\n\r\x{2028}\x{2029}]`

// jsRegexp compiles a flagless JavaScript regex source for RE2, giving \s
// (inside or outside a character class) and "." their JavaScript meaning.
// Everything else these sources use — \b, \w, ^, $, classes, escaped
// punctuation — already means the same in both engines. It does not handle
// \S; none of the ported sources use it.
func jsRegexp(src string) *regexp.Regexp {
	var b strings.Builder
	inClass := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '\\' && i+1 < len(src):
			i++
			switch {
			case src[i] != 's':
				b.WriteByte(c)
				b.WriteByte(src[i])
			case inClass:
				b.WriteString(textutil.JSSpaceChars)
			default:
				b.WriteString("[" + textutil.JSSpaceChars + "]")
			}
		case c == '[' && !inClass:
			inClass = true
			b.WriteByte(c)
		case c == ']' && inClass:
			inClass = false
			b.WriteByte(c)
		case c == '.' && !inClass:
			b.WriteString(jsDot)
		default:
			b.WriteByte(c)
		}
	}
	return regexp.MustCompile(b.String())
}

// jsLength is String.prototype.length: UTF-16 code units, so a character
// outside the BMP counts twice — the same cap the TS enforced.
func jsLength(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xffff {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// getwd is os.Getwd, swappable so tests can cover the failure fallback.
var getwd = os.Getwd

// resolve mirrors Node's path.resolve: segments are taken right to left until
// one is absolute (empty ones are skipped), the working directory fills in
// otherwise, and the result is normalized with no trailing separator except
// on the root. Node throws when the working directory is gone; this falls
// back to the root instead (only reachable with a relative task dir).
func resolve(segments ...string) string {
	p := ""
	for i := len(segments) - 1; i >= 0; i-- {
		s := segments[i]
		if s == "" {
			continue
		}
		if p == "" {
			p = s
		} else {
			p = s + string(filepath.Separator) + p
		}
		if filepath.IsAbs(s) {
			return filepath.Clean(p)
		}
	}
	wd, err := getwd()
	if err != nil {
		wd = string(filepath.Separator)
	}
	return filepath.Join(wd, p)
}

// basename mirrors Node's path.basename: trailing separators are ignored and
// a path of only separators (or nothing) has the empty basename — unlike
// filepath.Base, which answers "/" and ".".
func basename(p string) string {
	end := len(p)
	for end > 0 && os.IsPathSeparator(p[end-1]) {
		end--
	}
	start := end
	for start > 0 && !os.IsPathSeparator(p[start-1]) {
		start--
	}
	return p[start:end]
}

func homeEnv() string { return os.Getenv("HOME") }

// Path prefixes that are fine to touch from a task shell.
func allowedRoots(ctx Context) []string {
	roots := []string{ctx.TaskDir, ctx.StateDir, "/tmp", "/private/tmp", "/var/folders", "/dev/null", "/dev/stdout", "/dev/stderr"}
	// Toolchain caches/binaries that package managers touch legitimately.
	if home := homeEnv(); home != "" {
		roots = append(roots,
			filepath.Join(home, ".npm"), filepath.Join(home, ".cache"), filepath.Join(home, ".pnpm-store"), filepath.Join(home, ".yarn"),
			filepath.Join(home, ".cargo"), filepath.Join(home, "go"), filepath.Join(home, ".local"), filepath.Join(home, ".nvm"),
			filepath.Join(home, ".composer"), filepath.Join(home, ".gem"), filepath.Join(home, ".m2"), filepath.Join(home, ".gradle"),
			filepath.Join(home, "Library", "Caches"), filepath.Join(home, "Library", "pnpm"),
		)
	}
	roots = append(roots, "/usr", "/bin", "/sbin", "/opt", "/etc/hosts", "/System/Library", "/Library/Developer", "/Applications/Xcode.app")
	return resolveAll(roots)
}

// Locations that count as a real touch even when the path does not exist on
// this machine: home directories (secret stores like ~/.ssh live there) and
// system roots. The "nonexistent ⇒ prose" heuristic below must never wave
// through a probe of ~/.ssh/id_rsa just because the host has no such file.
func sensitiveRoots() []string {
	roots := []string{"/etc", "/private/etc", "/Users", "/home", "/root", "/Applications", "/Library", "/System"}
	if home := homeEnv(); home != "" {
		roots = append(roots, home)
	}
	return resolveAll(roots)
}

func resolveAll(roots []string) []string {
	out := make([]string, len(roots))
	for i, r := range roots {
		out[i] = resolve(r)
	}
	return out
}

// insideAny reports whether p (resolved) is one of roots or below one. The
// roots are compared as given — a root with a trailing separator still
// contains its children.
func insideAny(p string, roots []string) bool {
	abs := resolve(p)
	sep := string(filepath.Separator)
	for _, r := range roots {
		prefix := r
		if !strings.HasSuffix(r, sep) {
			prefix = r + sep
		}
		if abs == r || strings.HasPrefix(abs, prefix) {
			return true
		}
	}
	return false
}

// pathToken is the TS /(?:^|[\s='"`(])((?:\/|~\/|~$)[^\s'"`;|&()<>]*)/g.
var pathToken = jsRegexp(`(?:^|[\s='"` + "`" + `(])((?:\/|~\/|~$)[^\s'"` + "`" + `;|&()<>]*)`)

// pathsIn extracts absolute and home-relative path tokens from a command.
func pathsIn(cmd string) []string {
	var out []string
	for _, m := range pathToken.FindAllStringSubmatch(cmd, -1) {
		p := m[1]
		if strings.HasPrefix(p, "~") {
			p = homeEnv() + p[1:]
		}
		// Strip a trailing punctuation the regex may have caught.
		p = strings.TrimRight(p, ".,:")
		// A bare "/" is prose ("approved / pending") or a delimiter argument
		// (`cut -d'/'`) — a real touch of the root is caught by alwaysAsk.
		if p != "" && p != "/" {
			out = append(out, p)
		}
	}
	return out
}

// Commands whose heredoc body is CODE they will execute — those bodies stay
// under every rule. For anything else (`git commit -F - <<EOF`, `cat > x
// <<EOF`) the body is stdin DATA: commit prose mentioning "/leave/overview"
// must not read as a filesystem touch.
var heredocInterpreters = setOf("sh", "bash", "zsh", "python", "python3", "node", "php", "ruby", "perl", "deno", "tsx")

// heredocHeadSplit is the TS head.split(/\n|&&|\|\||;|\|/) separator.
var heredocHeadSplit = regexp.MustCompile(`\n|&&|\|\||;|\|`)

func isWordByte(c byte) bool {
	return c == '_' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// heredocMarker finds the first here-doc marker at or after from — the TS
// regex /<<-?\s*(['"]?)(\w+)\1[^\n]*\n/ — and returns where it starts, where
// its line ends (just past the newline) and the tag; start is -1 when there
// is none. RE2 has no backreferences, so the quote pairing (\1) is checked by
// hand, accepting exactly what the backtracking engine accepts:
//   - "<<" may start anywhere, including inside "<<<" (a here-string's quoted
//     word then reads as a tag, as it did in the TS);
//   - an optional "-", then every JavaScript whitespace character (a newline
//     included) — giving any back leaves whitespace where a quote or word
//     must start, so greedy is the only way to succeed;
//   - an optional opening quote, which must close right after the word: on a
//     mismatch the engine retries without the quote, and \w+ cannot start on
//     a quote, so the attempt fails;
//   - the tag is the longest run of [A-Za-z0-9_];
//   - then the rest of that line through its newline (no newline: no match).
func heredocMarker(cmd string, from int) (start, end int, tag string) {
	for i := from; i+1 < len(cmd); i++ {
		if cmd[i] != '<' || cmd[i+1] != '<' {
			continue
		}
		j := i + 2
		if j < len(cmd) && cmd[j] == '-' {
			j++
		}
		for j < len(cmd) {
			r, size := utf8.DecodeRuneInString(cmd[j:])
			if !textutil.IsJSSpace(r) {
				break
			}
			j += size
		}
		var quote byte
		if j < len(cmd) && (cmd[j] == '\'' || cmd[j] == '"') {
			quote = cmd[j]
			j++
		}
		k := j
		for k < len(cmd) && isWordByte(cmd[k]) {
			k++
		}
		if k == j {
			continue
		}
		tag = cmd[j:k]
		if quote != 0 {
			if k >= len(cmd) || cmd[k] != quote {
				continue
			}
			k++
		}
		nl := strings.IndexByte(cmd[k:], '\n')
		if nl < 0 {
			continue
		}
		return i, k + nl + 1, tag
	}
	return -1, -1, ""
}

// indexFrom is JavaScript's s.indexOf(sub, from) for a non-empty sub.
func indexFrom(s, sub string, from int) int {
	if from > len(s) {
		return -1
	}
	if i := strings.Index(s[from:], sub); i >= 0 {
		return from + i
	}
	return -1
}

// StripHeredocs removes here-doc bodies (<< TAG … TAG) from the analyzed
// text, keeping the marker line (so the receiving command is still checked).
// With keepInterpreterBodies, bodies destined for an interpreter stay — the
// PATH check must still see `open('/etc/passwd')` inside a python heredoc —
// but segment analysis never keeps them: python/php code is not shell, and
// treating its lines as commands manufactured false asks ("import", "$e").
func StripHeredocs(cmd string, keepInterpreterBodies bool) string {
	var out strings.Builder
	idx := 0
	for {
		// Searching from idx means a marker inside a body already consumed is
		// never seen (the TS guarded m.index < idx for the same reason).
		start, bodyStart, tag := heredocMarker(cmd, idx)
		if start < 0 {
			break
		}
		end := -1
		for i := indexFrom(cmd, "\n"+tag, bodyStart-1); i != -1; i = indexFrom(cmd, "\n"+tag, i+1) {
			after := i + 1 + len(tag)
			if after >= len(cmd) || cmd[after] == '\n' || cmd[after] == '\r' || cmd[after] == ' ' {
				end = i
				break
			}
		}
		if end == -1 {
			break // unterminated — analyze the rest verbatim
		}
		head := cmd[idx:start]
		parts := heredocHeadSplit.Split(head, -1)
		target := basename(firstWord(parts[len(parts)-1]))
		out.WriteString(head)
		out.WriteString(cmd[start:bodyStart])
		if keepInterpreterBodies && heredocInterpreters[target] {
			out.WriteString(cmd[bodyStart : end+1+len(tag)]) // code: keep it
		}
		idx = end + 1 + len(tag)
	}
	out.WriteString(cmd[idx:])
	return out.String()
}

// Control-flow openers that carry a nested command in the SAME segment
// ("do cp a b", "{ ls") — skip the keyword and check what follows.
var keywordsWithCommand = setOf("if", "then", "else", "elif", "while", "until", "do", "!", "{")

// Syntax-only words a segment may consist of entirely.
var keywordsBare = setOf("for", "done", "fi", "esac", "case", "in", "[", "[[", "((", ":", "}", ")")

// Wrappers firstWord skips to find a heredoc's receiving command.
var wrappers = setOf("time", "nohup", "exec", "command", "nice")

var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

func firstWord(seg string) string {
	// Skip leading VAR=value assignments, wrappers, and control-flow keywords.
	tokens := strings.FieldsFunc(seg, textutil.IsJSSpace)
	i := 0
	for {
		for i < len(tokens) && assignment.MatchString(tokens[i]) {
			i++
		}
		for i < len(tokens) && wrappers[tokens[i]] {
			i++
		}
		if i < len(tokens) && keywordsWithCommand[tokens[i]] {
			i++
			continue
		}
		break
	}
	w := ""
	if i < len(tokens) {
		w = tokens[i]
	}
	for strings.HasPrefix(w, "(") || strings.HasPrefix(w, "{") {
		w = w[1:] // subshell/group opener
	}
	for strings.HasSuffix(w, ")") || strings.HasSuffix(w, "}") || strings.HasSuffix(w, ";") {
		w = w[:len(w)-1] // closers glued on: `|| true)`
	}
	return w
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

var relativeScriptPrefixes = []string{"./", "bin/", "scripts/", "vendor/bin/", "node_modules/.bin/"}

func setOf(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, s := range items {
		m[s] = true
	}
	return m
}

// ---------------------------------------------------------------------------
// Shell lexer: just enough of the shell grammar to see every command a line
// runs and every word it hands them.
// ---------------------------------------------------------------------------

// varMark brackets an expansion whose value the policy cannot know inside a
// word's value: mark("NAME") stands for $NAME, mark("(…)") for a command
// substitution's output. A value that STARTS with one is chosen at run time.
const varMark = "\x00"

func mark(name string) string { return varMark + name + varMark }

// display renders a value for a reason, its marks back as "$NAME".
func display(s string) string {
	parts := strings.Split(s, varMark)
	for i := 1; i < len(parts); i += 2 {
		parts[i] = "$" + parts[i]
	}
	return strings.Join(parts, "")
}

// word is one shell word after quote removal, with the expansions the policy
// can resolve already applied (~, ~user, $HOME, ${HOME}).
type word struct {
	val     string
	quoted  bool              // some part was quoted or escaped
	assign  bool              // NAME=value
	glob    bool              // an unquoted * ? or [ — the shell expands it
	brace   bool              // an unquoted { — possibly a brace expansion
	procSub bool              // <(…) / >(…): a /dev/fd path the shell makes
	subs    [][]simpleCommand // command substitutions inside, run first
}

type heredoc struct {
	delim  string
	strip  bool              // <<- drops leading tabs
	quoted bool              // a quoted delimiter: the body is not expanded
	subs   [][]simpleCommand // substitutions in an unquoted body
}

type redirect struct {
	op     string
	target word
	doc    *heredoc // for << and <<-
}

// simpleCommand is one command with its words and redirections, or (group
// ±1, no words) the opening or closing parenthesis of a subshell.
type simpleCommand struct {
	words  []word
	redirs []redirect
	sep    string // the operator after it: ";", "&&", "||", "|", "&", "\n", …
	group  int
}

type shellLexer struct {
	s    string
	i    int
	home string
	cmds []simpleCommand
	cur  simpleCommand
	docs []*heredoc // heredocs whose bodies start after the next newline
	ask  string     // a reason to ask found while lexing
}

// lexShell splits a command line into simple commands; a non-empty reason
// means something could not be analyzed (an unterminated substitution, …).
func lexShell(s, home string) ([]simpleCommand, string) {
	l := &shellLexer{s: s, home: home}
	l.list(false)
	return l.cmds, l.ask
}

func (l *shellLexer) at(p string) bool { return strings.HasPrefix(l.s[l.i:], p) }

func isDigits(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }

func isNameByte(c byte, first bool) bool {
	return c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || (!first && '0' <= c && c <= '9')
}

func isName(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isNameByte(s[i], i == 0) {
			return false
		}
	}
	return s != ""
}

// wordEnd reports whether s starts with something that ends a word.
func wordEnd(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return strings.IndexByte("\n;&|()<>", s[0]) >= 0 || textutil.IsJSSpace(r)
}

// list reads commands until the end or, when nested, the ")" closing a $(…)
// or <(…); it reports whether that ")" was found.
func (l *shellLexer) list(nested bool) bool {
	depth := 0
	for l.i < len(l.s) && l.ask == "" {
		c := l.s[l.i]
		r, size := utf8.DecodeRuneInString(l.s[l.i:])
		switch {
		case c == '\n':
			l.i++
			l.endWith("\n")
			l.bodies()
		case textutil.IsJSSpace(r):
			l.i += size
		case c == '#':
			if n := strings.IndexByte(l.s[l.i:], '\n'); n >= 0 {
				l.i += n
			} else {
				l.i = len(l.s)
			}
		case c == '<' || c == '>' || l.at("&>"):
			l.redirect()
		case c == ';' || c == '&' || c == '|':
			op := l.s[l.i : l.i+1]
			if l.i+1 < len(l.s) && strings.IndexByte(";&|", l.s[l.i+1]) >= 0 {
				op = l.s[l.i : l.i+2]
			}
			l.i += len(op)
			l.endWith(op)
		case c == '(':
			l.i++
			l.endWith("")
			l.cmds = append(l.cmds, simpleCommand{group: 1})
			depth++
		case c == ')':
			l.i++
			l.endWith(")")
			if depth == 0 {
				if nested {
					return true
				}
				continue // a stray ")" (case pattern, syntax error)
			}
			depth--
			l.cmds = append(l.cmds, simpleCommand{group: -1})
		default:
			l.word()
		}
	}
	l.endWith("")
	return false
}

// endWith closes the current command; the operator after a ")" belongs to
// the group.
func (l *shellLexer) endWith(op string) {
	if len(l.cur.words) > 0 || len(l.cur.redirs) > 0 {
		l.cur.sep = op
		l.cmds = append(l.cmds, l.cur)
	} else if n := len(l.cmds); n > 0 && l.cmds[n-1].sep == "" {
		l.cmds[n-1].sep = op
	}
	l.cur = simpleCommand{}
}

func (l *shellLexer) word() {
	w := l.readWord()
	if !w.quoted && isDigits(w.val) && l.i < len(l.s) && (l.s[l.i] == '<' || l.s[l.i] == '>') {
		l.redirect() // "2>": the digits were a file descriptor
		return
	}
	l.cur.words = append(l.cur.words, w)
}

func (l *shellLexer) readWord() word {
	var b strings.Builder
	var w word
	start := l.i
	for l.i < len(l.s) && l.ask == "" && !wordEnd(l.s[l.i:]) {
		c := l.s[l.i]
		switch c {
		case '\\':
			w.quoted = true
			l.i++
			if l.i < len(l.s) && l.s[l.i] == '\n' {
				l.i++ // line continuation
				continue
			}
			_, n := utf8.DecodeRuneInString(l.s[l.i:])
			b.WriteString(l.s[l.i : l.i+n])
			l.i += n
		case '\'':
			w.quoted = true
			end := strings.IndexByte(l.s[l.i+1:], '\'')
			if end < 0 {
				end = len(l.s) - l.i - 1 // unterminated: the rest
			}
			b.WriteString(l.s[l.i+1 : l.i+1+end])
			l.i = min(l.i+2+end, len(l.s))
		case '"':
			w.quoted = true
			l.i++
			l.double(&b, &w, '"')
		case '$':
			switch {
			case l.at("$'"):
				w.quoted = true
				l.ansiC(&b)
			case l.at(`$"`):
				l.i++ // $"…" is a translated "…"
			default:
				l.dollar(&b, &w)
			}
		case '`':
			l.backtick(&b, &w)
		case '~':
			if l.i == start || w.assign && l.s[l.i-1] == '=' {
				l.tilde(&b)
			} else {
				b.WriteByte(c)
				l.i++
			}
		case '=':
			w.assign = w.assign || !w.quoted && isName(b.String())
			b.WriteByte(c)
			l.i++
		case '*', '?', '[':
			w.glob = true
			b.WriteByte(c)
			l.i++
		case '{':
			w.brace = true
			b.WriteByte(c)
			l.i++
		default:
			_, n := utf8.DecodeRuneInString(l.s[l.i:])
			b.WriteString(l.s[l.i : l.i+n])
			l.i += n
		}
	}
	w.val = b.String()
	return w
}

// double reads the inside of "…" (term '"') or an unquoted heredoc body
// (term 0: to the end), expanding $ and backticks.
func (l *shellLexer) double(b *strings.Builder, w *word, term byte) {
	for l.i < len(l.s) && l.ask == "" {
		c := l.s[l.i]
		switch {
		case term != 0 && c == term:
			l.i++
			return
		case c == '\\' && l.i+1 < len(l.s) && strings.IndexByte("$`\"\\\n", l.s[l.i+1]) >= 0:
			if l.s[l.i+1] != '\n' {
				b.WriteByte(l.s[l.i+1])
			}
			l.i += 2
		case c == '$':
			l.dollar(b, w)
		case c == '`':
			l.backtick(b, w)
		default:
			b.WriteByte(c)
			l.i++
		}
	}
}

// ansiC reads $'…'. Escapes could spell anything ($'\x63url'), so a value
// with one is opaque.
func (l *shellLexer) ansiC(b *strings.Builder) {
	i := l.i + 2
	for i < len(l.s) && l.s[i] != '\'' {
		if l.s[i] == '\\' {
			i++
		}
		i++
	}
	body := l.s[l.i+2 : min(i, len(l.s))]
	l.i = min(i+1, len(l.s))
	if strings.Contains(body, `\`) {
		b.WriteString(mark("'…'"))
		return
	}
	b.WriteString(body)
}

// closing finds the bracket closing s[open], or -1.
func closing(s string, open int, oc, cc byte) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case oc:
			depth++
		case cc:
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return -1
}

// dollar reads one $ expansion.
func (l *shellLexer) dollar(b *strings.Builder, w *word) {
	rest := l.s[l.i+1:]
	switch {
	case strings.HasPrefix(rest, "(("): // arithmetic: a number
		end := closing(l.s, l.i+1, '(', ')')
		if end < 0 || strings.Contains(l.s[l.i+3:end], "$(") || strings.Contains(l.s[l.i+3:end], "`") {
			l.ask = "command substitution inside arithmetic"
			return
		}
		b.WriteString("0")
		l.i = end + 1
	case strings.HasPrefix(rest, "("):
		l.i += 2
		cmds, ok := l.nested()
		if !ok && l.ask == "" {
			l.ask = "unterminated command substitution"
		}
		w.subs = append(w.subs, cmds)
		b.WriteString(substValue(cmds))
	case strings.HasPrefix(rest, "{"):
		end := closing(l.s, l.i+1, '{', '}')
		if end < 0 || strings.Contains(l.s[l.i+2:end], "$(") || strings.Contains(l.s[l.i+2:end], "`") {
			l.ask = "command substitution inside ${…}"
			return
		}
		inner := l.s[l.i+2 : end]
		switch {
		case strings.HasPrefix(inner, "#"):
			b.WriteString("0") // a length
		case isName(inner):
			b.WriteString(l.param(inner))
		default:
			b.WriteString(mark("{" + inner + "}"))
		}
		l.i = end + 1
	case rest != "" && isNameByte(rest[0], true):
		n := 1
		for n < len(rest) && isNameByte(rest[n], false) {
			n++
		}
		b.WriteString(l.param(rest[:n]))
		l.i += 1 + n
	case rest != "" && strings.IndexByte("#?$!", rest[0]) >= 0:
		b.WriteString("0") // counts, statuses, pids
		l.i += 2
	case rest != "" && strings.IndexByte("@*-0123456789", rest[0]) >= 0:
		b.WriteString(mark(rest[:1]))
		l.i += 2
	default:
		b.WriteByte('$')
		l.i++
	}
}

func (l *shellLexer) param(name string) string {
	if name == "HOME" {
		return l.home
	}
	return mark(name)
}

// nested lexes a $(…) or <(…) body up to its closing ")".
func (l *shellLexer) nested() ([]simpleCommand, bool) {
	sub := &shellLexer{s: l.s, i: l.i, home: l.home}
	ok := sub.list(true)
	l.i, l.ask = sub.i, sub.ask
	return sub.cmds, ok
}

// substValue is what a command substitution stands for in a word: the
// directory for $(pwd), the repository root for $(git rev-parse
// --show-toplevel), otherwise an unknown value.
func substValue(cmds []simpleCommand) string {
	if len(cmds) == 1 && len(cmds[0].redirs) == 0 {
		var vals []string
		for _, w := range cmds[0].words {
			vals = append(vals, w.val)
		}
		switch strings.Join(vals, " ") {
		case "pwd":
			return mark("PWD")
		case "git rev-parse --show-toplevel":
			return mark("TOPLEVEL")
		}
	}
	return mark("(…)")
}

func (l *shellLexer) backtick(b *strings.Builder, w *word) {
	var inner strings.Builder
	for i := l.i + 1; i < len(l.s); i++ {
		c := l.s[i]
		if c == '`' {
			cmds, why := lexShell(inner.String(), l.home)
			l.i, l.ask = i+1, why
			w.subs = append(w.subs, cmds)
			b.WriteString(substValue(cmds))
			return
		}
		if c == '\\' && i+1 < len(l.s) && strings.IndexByte("`$\\", l.s[i+1]) >= 0 {
			i++
			c = l.s[i]
		}
		inner.WriteByte(c)
	}
	l.ask = "unterminated command substitution"
}

// tilde expands ~, ~user, ~+ and ~- at the start of a word (or right after
// an assignment's "="); ~user is that user's home next to ours.
func (l *shellLexer) tilde(b *strings.Builder) {
	j := l.i + 1
	if j < len(l.s) && (l.s[j] == '+' || l.s[j] == '-') {
		j++
	} else {
		for j < len(l.s) && (isNameByte(l.s[j], false) || l.s[j] == '.' || l.s[j] == '-') {
			j++
		}
	}
	if j < len(l.s) && l.s[j] != '/' && !wordEnd(l.s[j:]) {
		b.WriteByte('~') // ~"x", ~$x: not a tilde prefix
		l.i++
		return
	}
	switch user := l.s[l.i+1 : j]; user {
	case "":
		b.WriteString(l.home)
	case "+":
		b.WriteString(mark("PWD"))
	case "-":
		b.WriteString(mark("OLDPWD"))
	default:
		b.WriteString(filepath.Join(string(filepath.Separator), filepath.Dir(l.home), user))
	}
	l.i = j
}

var redirOps = []string{"&>>", "&>", "<<<", "<<-", "<<", "<>", "<&", "<", ">>", ">|", ">&", ">"}

func (l *shellLexer) redirect() {
	if l.at("<(") || l.at(">(") {
		l.i += 2
		cmds, ok := l.nested()
		if !ok && l.ask == "" {
			l.ask = "unterminated process substitution"
		}
		l.cur.words = append(l.cur.words, word{val: "/dev/fd/63", procSub: true, subs: [][]simpleCommand{cmds}})
		return
	}
	op := ""
	for _, o := range redirOps {
		if l.at(o) {
			op = o
			break
		}
	}
	l.i += len(op)
	for l.i < len(l.s) && l.s[l.i] != '\n' {
		r, size := utf8.DecodeRuneInString(l.s[l.i:])
		if !textutil.IsJSSpace(r) {
			break
		}
		l.i += size
	}
	t := l.readWord()
	if (op == "<&" || op == ">&") && (isDigits(t.val) || t.val == "-") {
		return // fd duplication: no file
	}
	r := redirect{op: op, target: t}
	if op == "<<" || op == "<<-" {
		r.doc = &heredoc{delim: t.val, strip: op == "<<-", quoted: t.quoted}
		l.docs = append(l.docs, r.doc)
	}
	l.cur.redirs = append(l.cur.redirs, r)
}

// bodies consumes the heredoc bodies that start after the newline just read.
// A body ends at the line equal to its delimiter — never later than the
// shell would end it, so nothing the shell runs is mistaken for data.
func (l *shellLexer) bodies() {
	docs := l.docs
	l.docs = nil
	for _, d := range docs {
		var lines []string
		for l.i < len(l.s) {
			line, next := l.s[l.i:], len(l.s)
			if n := strings.IndexByte(line, '\n'); n >= 0 {
				line, next = line[:n], l.i+n+1
			}
			text := line
			if d.strip {
				text = strings.TrimLeft(line, "\t")
			}
			if text == d.delim {
				l.i = next
				break
			}
			if strings.HasPrefix(text, d.delim+")") {
				// "EOF)" closes a $( … ) for some shells: stop here, so what
				// follows is analyzed as commands (the cautious reading).
				l.i += len(line) - len(text) + len(d.delim)
				break
			}
			lines = append(lines, line)
			l.i = next
		}
		if !d.quoted {
			sub := &shellLexer{s: strings.Join(lines, "\n"), home: l.home}
			var b strings.Builder
			var w word
			sub.double(&b, &w, 0)
			if sub.ask != "" {
				l.ask = sub.ask
				return
			}
			d.subs = w.subs
		}
	}
}

// ---------------------------------------------------------------------------
// Paths: symlinks and git internals
// ---------------------------------------------------------------------------

func splitPath(p string) []string {
	return strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == filepath.Separator })
}

// realPath is what the kernel opens for path p from directory base: it walks
// the components in order, following each symlink — a dangling final one
// too, since writing through it creates its target — BEFORE applying a later
// "..", so "link/../x" is judged where it really lands (resolve's lexical
// cleaning would drop the link). From the first missing component on, the
// rest is joined as written.
func realPath(base, p string) string {
	if !filepath.IsAbs(p) {
		p = base + string(filepath.Separator) + p
	}
	vol := filepath.VolumeName(p)
	done := vol + string(filepath.Separator)
	todo := splitPath(p[len(vol):])
	for hops := 0; len(todo) > 0; {
		c := todo[0]
		todo = todo[1:]
		switch c {
		case ".":
			continue
		case "..":
			done = filepath.Dir(done) // done has no links left: lexical is right
			continue
		}
		next := filepath.Join(done, c)
		fi, err := os.Lstat(next)
		if err != nil {
			return filepath.Join(append([]string{next}, todo...)...)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			done = next
			continue
		}
		if hops++; hops > 40 { // a loop: nothing can be read through it
			return filepath.Join(append([]string{next}, todo...)...)
		}
		target, _ := os.Readlink(next)
		if filepath.IsAbs(target) {
			vol = filepath.VolumeName(target)
			done, target = vol+string(filepath.Separator), target[len(vol):]
		}
		todo = append(splitPath(target), todo...)
	}
	return done
}

// withReal adds each root's real path, so a path that resolves through
// /tmp → /private/tmp still counts as inside.
func withReal(roots []string) []string {
	out := append([]string{}, roots...)
	for _, r := range roots {
		out = append(out, realPath(r, r))
	}
	return out
}

// inGitDir reports a path that is (or really is, real) inside a .git
// directory — hooks, config and info/attributes there run code on the next
// git command. The comparison ignores case: macOS volumes do.
func inGitDir(abs, real string) bool {
	for _, p := range []string{abs, real} {
		for _, c := range splitPath(p) {
			if strings.EqualFold(c, ".git") {
				return true
			}
		}
	}
	return false
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

const maxAlternatives = 64

// braces lists the brace expansions of s ({a,b} groups, nested too); past
// maxAlternatives the caller gives up and asks.
func braces(s string) []string {
	depth, open := 0, -1
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			if depth == 0 {
				open = i
			}
			depth++
		case '}':
			if depth == 0 {
				continue
			}
			if depth--; depth > 0 {
				continue
			}
			parts := splitTopLevel(s[open+1 : i])
			if len(parts) < 2 {
				continue // {x} or {1..3}: not a list
			}
			var out []string
			for _, p := range parts {
				out = append(out, braces(s[:open]+p+s[i+1:])...)
				if len(out) > maxAlternatives {
					return out
				}
			}
			return out
		}
	}
	return []string{s}
}

func splitTopLevel(s string) []string {
	var parts []string
	depth, from := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[from:i])
				from = i + 1
			}
		}
	}
	return append(parts, s[from:])
}

// ---------------------------------------------------------------------------
// The analyzer: judges each simple command the lexer found
// ---------------------------------------------------------------------------

type analyzer struct {
	taskDir   string
	cwds      []string // where the shell may be: a cd that can fail keeps the old dir
	pending   []string // dirs left by "cd x &&": possible again once the && chain ends
	groups    []snapshot
	roots     []string // allowedRoots and their real paths
	workspace []string // the checkout and the state dir (cd / source targets)
	writable  []string // where commands may write: the workspace and temp dirs
	sensitive []string
	vars      map[string]string   // literal values assigned earlier in the line
	loopVars  map[string][]string // for-loop variables over checked words
}

type snapshot struct {
	cwds, pending []string
	vars          map[string]string
	loopVars      map[string][]string
}

func newAnalyzer(ctx Context) *analyzer {
	workspace := resolveAll([]string{ctx.TaskDir, ctx.StateDir})
	return &analyzer{
		taskDir:   workspace[0],
		cwds:      []string{workspace[0]},
		roots:     withReal(allowedRoots(ctx)),
		workspace: withReal(workspace),
		writable: withReal(append(workspace, resolveAll([]string{"/tmp", "/private/tmp", "/var/folders",
			"/dev/null", "/dev/stdout", "/dev/stderr"})...)),
		sensitive: sensitiveRoots(),
		vars:      map[string]string{},
		loopVars:  map[string][]string{},
	}
}

func (a *analyzer) save() snapshot {
	return snapshot{a.cwds, a.pending, maps.Clone(a.vars), maps.Clone(a.loopVars)}
}

func (a *analyzer) restore(s snapshot) {
	a.cwds, a.pending, a.vars, a.loopVars = s.cwds, s.pending, s.vars, s.loopVars
}

func ask(why string) *Decision { return &Decision{Allow: false, Reason: why} }

func inline(what string) *Decision { return ask("inline code (" + what + ")") }

func varAsk(name string) *Decision { return ask("path through a variable ($" + name + ")") }

func union(a, b []string) []string {
	out := append([]string{}, a...)
	for _, s := range b {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// Separators that end an && chain: what follows runs whether or not a cd in
// the chain succeeded.
var chainEnds = setOf(";", "\n", "||", "&", ";;", ";&", ")", "")

func (a *analyzer) commands(cmds []simpleCommand) *Decision {
	prev := ""
	for _, c := range cmds {
		switch c.group {
		case 1:
			a.groups = append(a.groups, a.save())
		case -1:
			a.restore(a.groups[len(a.groups)-1]) // a subshell's cd and variables end with it
			a.groups = a.groups[:len(a.groups)-1]
		default:
			// A cd in a pipeline or background job runs in a subshell (or, in
			// zsh's last pipe stage, not): its directory is only possible.
			sure := c.sep == "&&" && prev != "|" && prev != "|&"
			if d := a.command(c, sure); d != nil {
				return d
			}
		}
		if chainEnds[c.sep] && c.group != 1 {
			a.cwds, a.pending = union(a.cwds, a.pending), nil
		}
		prev = c.sep
	}
	return nil
}

// nested judges commands that run in a subshell: a substitution, or a
// command line held in an environment variable.
func (a *analyzer) nested(cmds []simpleCommand) *Decision {
	s, groups := a.save(), a.groups
	a.groups = nil
	d := a.commands(cmds)
	a.restore(s)
	a.groups = groups
	return d
}

// expand substitutes what the analyzer knows — literals assigned earlier,
// $PWD, each value of a for-loop variable — and returns every resulting
// value plus the name of an unknown expansion a value starts with.
func (a *analyzer) expand(s string) ([]string, string) {
	outs, lead := []string{""}, ""
	for {
		i := strings.Index(s, varMark)
		n := -1
		if i >= 0 {
			n = strings.Index(s[i+1:], varMark)
		}
		if n < 0 {
			break // no (more) marks; a stray NUL from the text stays literal
		}
		name := s[i+1 : i+1+n]
		vals, ok := a.values(name)
		if !ok || len(outs)*len(vals) > maxAlternatives {
			vals, ok = []string{mark(name)}, false
		}
		next := make([]string, 0, len(outs)*len(vals))
		for _, o := range outs {
			o += s[:i]
			if !ok && o == "" && lead == "" {
				lead = name
			}
			for _, v := range vals {
				next = append(next, o+v)
			}
		}
		outs, s = next, s[i+2+n:]
	}
	for k := range outs {
		outs[k] += s
	}
	return outs, lead
}

func (a *analyzer) values(name string) ([]string, bool) {
	switch name {
	case "PWD":
		return a.cwds, true
	case "TOPLEVEL":
		// $(git rev-parse --show-toplevel) from inside a checkout that is a
		// repository root is that root, or a nested repository below it
		// (judging from the shallower one is the cautious side).
		top := exists(filepath.Join(a.taskDir, ".git")) && !slices.ContainsFunc(a.cwds, func(c string) bool {
			return !insideAny(c, []string{a.taskDir})
		})
		return []string{a.taskDir}, top
	}
	if v, ok := a.vars[name]; ok {
		return []string{v}, true
	}
	v, ok := a.loopVars[name]
	return v, ok
}

func (a *analyzer) assign(w string) {
	name, value, _ := strings.Cut(w, "=")
	delete(a.loopVars, name)
	delete(a.vars, name)
	if vals, _ := a.expand(value); len(vals) == 1 && !strings.Contains(vals[0], varMark) {
		a.vars[name] = vals[0]
	}
}

// each calls fn on every path a word's value can name — as written (abs,
// cleaned like Node's path.resolve) and as the kernel opens it (real) — from
// every directory the shell may be in, for each brace alternative and, for
// an unquoted glob, each match on disk now.
func (a *analyzer) each(p string, w word, fn func(abs, real string) *Decision) *Decision {
	alts := []string{p}
	if w.brace {
		if alts = braces(p); len(alts) > maxAlternatives {
			return ask("brace expansion too large to check")
		}
	}
	for _, cwd := range a.cwds {
		for _, alt := range alts {
			real := realPath(cwd, alt)
			if d := fn(resolve(cwd, alt), real); d != nil {
				return d
			}
			if !w.glob {
				continue
			}
			matches, _ := filepath.Glob(real)
			for _, m := range matches {
				if d := fn(m, realPath(m, m)); d != nil {
					return d
				}
			}
		}
	}
	return nil
}

type pathUse struct {
	links bool // a symlink inside the roots that leads out counts too
	root  bool // a bare "/" counts (recursive searches); otherwise it is prose or a delimiter
}

// touch judges one absolute path against the allowed roots. Outside them,
// a path whose location doesn't exist is prose ("/api/people"), as in the TS
// — except under sensitive roots, which always count.
func (a *analyzer) touch(abs, real string, use pathUse) *Decision {
	if !insideAny(abs, a.roots) {
		if filepath.Dir(abs) == abs {
			if !use.root {
				return nil
			}
		} else if !insideAny(abs, a.sensitive) {
			if parent := filepath.Dir(abs); !exists(abs) && (filepath.Dir(parent) == parent || !exists(parent)) {
				return nil
			}
		}
		return ask("touches " + display(abs) + " outside the workspace")
	}
	if use.links && !insideAny(real, a.roots) {
		return ask("touches " + display(abs) + " outside the workspace (a symlink to " + real + ")")
	}
	return nil
}

func (a *analyzer) touchWord(p string, w word, use pathUse) *Decision {
	return a.each(p, w, func(abs, real string) *Decision { return a.touch(abs, real, use) })
}

func (a *analyzer) gitInternals(p string, w word) *Decision {
	return a.each(p, w, func(abs, real string) *Decision {
		if inGitDir(abs, real) {
			return ask("editing git internals (" + display(p) + ")")
		}
		return nil
	})
}

// command judges one simple command.
func (a *analyzer) command(c simpleCommand, sure bool) *Decision {
	for _, w := range c.words {
		for _, s := range w.subs {
			if d := a.nested(s); d != nil {
				return d
			}
		}
	}
	for _, r := range c.redirs {
		subs := r.target.subs
		if r.doc != nil {
			subs = append(subs, r.doc.subs...)
		}
		for _, s := range subs {
			if d := a.nested(s); d != nil {
				return d
			}
		}
	}
	if d := a.redirects(c.redirs); d != nil {
		return d
	}
	words := c.words
	i := 0
	for ; i < len(words); i++ {
		w := words[i]
		if w.assign {
			name, value, _ := strings.Cut(w.val, "=")
			if d := a.envOverride(name, value); d != nil {
				return d
			}
			continue
		}
		if w.quoted || !keywordsWithCommand[w.val] {
			break
		}
	}
	if i == len(words) { // only assignments (and keywords): shell variables
		for _, w := range words {
			if w.assign {
				a.assign(w.val)
			}
		}
		return nil
	}
	if w := words[i]; !w.quoted && keywordsBare[w.val] {
		if w.val == "for" {
			return a.forLoop(words[i+1:])
		}
		return nil
	}
	return a.run(words[i:], c.redirs, sure)
}

func (a *analyzer) redirects(redirs []redirect) *Decision {
	for _, r := range redirs {
		if r.op == "<<" || r.op == "<<-" || r.op == "<<<" {
			continue // data
		}
		outs, lead := a.expand(r.target.val)
		if lead != "" {
			return varAsk(lead)
		}
		for _, p := range outs {
			if d := a.touchWord(p, r.target, pathUse{links: true}); d != nil {
				return d
			}
			if r.op != "<" {
				if d := a.gitInternals(p, r.target); d != nil {
					return d
				}
				if d := a.writes(r.target, ""); d != nil {
					return d
				}
			}
		}
	}
	return nil
}

// forLoop checks the list a loop variable takes its values from; when every
// item is known, later uses of the variable are checked with each value.
func (a *analyzer) forLoop(rest []word) *Decision {
	if len(rest) == 0 {
		return nil
	}
	name := rest[0].val
	delete(a.vars, name)
	delete(a.loopVars, name)
	if len(rest) < 2 || rest[1].val != "in" {
		return nil // "for x; do": the positional parameters
	}
	var vals []string
	for _, w := range rest[2:] {
		outs, lead := a.expand(w.val)
		if lead != "" {
			return nil
		}
		for _, p := range outs {
			if d := a.touchWord(p, w, pathUse{links: true}); d != nil {
				return d
			}
		}
		vals = append(vals, outs...)
	}
	if len(vals) <= maxAlternatives {
		a.loopVars[name] = vals
	}
	return nil
}

// ---------------------------------------------------------------------------
// Program rules
// ---------------------------------------------------------------------------

func isOption(v string) bool { return strings.HasPrefix(v, "-") && v != "-" }

// shortCluster is "-abc" (not "--long", not "-").
func shortCluster(v string) bool { return len(v) > 1 && v[0] == '-' && v[1] != '-' }

// operands are the words after the program that are not options ("--" ends
// the options).
func operands(args []word) []word {
	var out []word
	opts := true
	for _, w := range args[1:] {
		switch {
		case opts && w.val == "--":
			opts = false
		case opts && isOption(w.val):
		default:
			out = append(out, w)
		}
	}
	return out
}

func hasWord(args []word, vals ...string) bool {
	for _, w := range args[1:] {
		if slices.Contains(vals, w.val) {
			return true
		}
	}
	return false
}

// skipOptions returns the index of the first word from i on that is not an
// option (or one of valued's values).
func skipOptions(args []word, i int, valued map[string]bool) int {
	for i < len(args) {
		v := args[i].val
		if v == "--" {
			return i + 1
		}
		if !isOption(v) {
			return i
		}
		if valued[v] {
			i++
		}
		i++
	}
	return min(i, len(args))
}

var pythonName = regexp.MustCompile(`^python[0-9.]*$`)

// Program classes.
var (
	privileged = setOf("sudo", "su", "doas", "pkexec")
	network    = setOf("curl", "wget", "nc", "ncat", "netcat", "telnet", "ssh", "scp", "sftp", "rsync",
		"ftp", "socat", "nslookup", "dig", "host")
	shells = setOf("sh", "bash", "zsh", "dash", "ksh", "fish")
	// Programs that only print their arguments: no path checks on them.
	valueBuiltins = setOf("echo", "printf", "test", "true", "false", "sleep", "which", "type", "date", "pwd")
	// Programs whose operands are files, so an operand chosen at run time
	// ($X, $(…)) is a path through a variable.
	fileCommands = setOf("cat", "tac", "less", "more", "head", "tail", "wc", "nl", "od", "xxd", "strings",
		"base64", "file", "stat", "cp", "mv", "ln", "rm", "rmdir", "mkdir", "touch", "chmod", "tee",
		"truncate", "ls", "du", "tree", "diff", "cmp", "tar", "zip", "unzip")
	// …and their short options whose value is not a path (head -n $N).
	valueOptions = map[string]string{"head": "nc", "tail": "nc", "ls": "IwT", "du": "dBt", "tree": "LIP",
		"mkdir": "m", "touch": "td", "diff": "UCx", "stat": "fc"}
	// Recursive searches, for which a bare "/" means the whole disk.
	searchers = setOf("find", "fd", "grep", "egrep", "fgrep", "rg", "ag", "du", "tree")
	// Programs that write their operands: none may land in .git.
	gitWriters = setOf("cp", "mv", "ln", "install", "tee", "touch", "mkdir", "rmdir", "rm", "unlink",
		"chmod", "chown", "chgrp", "truncate", "sed")
	// Options that run a command or write a file.
	execOptions = map[string][]string{
		"find": {"-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls"},
		"fd":   {"-x", "--exec", "-X", "--exec-batch"},
		"rg":   {"--pre"},
		"sort": {"--compress-program"},
	}
	wrapperValues = map[string]map[string]bool{
		"nice": setOf("-n"), "stdbuf": setOf("-i", "-o", "-e"), "exec": setOf("-a"), "timeout": setOf("-s", "-k"),
	}
	envOptions = setOf("-i", "-", "--ignore-environment", "-0", "--null", "-v", "--debug")
	// Environment variables holding a command line the program will run…
	commandEnv = setOf("GIT_SSH_COMMAND", "GIT_SSH", "GIT_PROXY_COMMAND", "GIT_ASKPASS", "SSH_ASKPASS",
		"GIT_EXTERNAL_DIFF", "GIT_EDITOR", "GIT_SEQUENCE_EDITOR", "GIT_PAGER", "PAGER", "MANPAGER",
		"EDITOR", "VISUAL", "BROWSER")
	// …or redirecting which repository, config, shell startup or code loads.
	configEnv = setOf("GIT_DIR", "GIT_WORK_TREE", "GIT_EXEC_PATH", "GIT_TEMPLATE_DIR", "GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "BASH_ENV", "ENV",
		"ZDOTDIR", "PROMPT_COMMAND", "PS4", "CDPATH", "LESSOPEN", "LESSCLOSE", "PERL5OPT", "RUBYOPT")
	nodeOptionsLoad = jsRegexp(`(^|\s)(-r|--require|--import|--loader|--experimental-loader)(\s|=|$)`)
	versionFlags    = setOf("--version", "-V", "-v", "--help", "-h")
	inlineLong      = setOf("--eval", "--print")
	// Options that preload a module: fine, unless the "module" is a data: URL.
	moduleLong = setOf("--require", "--import", "--loader", "--experimental-loader", "--preload")
)

// interp describes how an interpreter takes code.
type interp struct {
	inline, valued, stop string // short-option letters: run given code / take a value / end options (python -m)
	stdin                bool   // with no script operand it runs stdin
	subcommands          bool   // "eval" / "repl" subcommands (deno, bun)
}

var interpreters = map[string]interp{
	"python":  {inline: "c", valued: "WX", stop: "m", stdin: true},
	"node":    {inline: "ep", valued: "Cr", stdin: true},
	"tsx":     {inline: "ep", valued: "Cr", stdin: true},
	"ts-node": {inline: "ep", valued: "r", stdin: true},
	"deno":    {subcommands: true},
	"bun":     {inline: "ep", valued: "r", subcommands: true},
	"ruby":    {inline: "er", valued: "ICE", stdin: true},
	"perl":    {inline: "eE", stdin: true},
	"php":     {inline: "rBREa", valued: "cdzt", stop: "fS", stdin: true},
}

// run judges one command from its program word on.
func (a *analyzer) run(args []word, redirs []redirect, sure bool) *Decision {
	outs, lead := a.expand(args[0].val)
	if lead != "" || args[0].procSub {
		return ask(`unrecognized command "` + display(args[0].val) + `"`)
	}
	name := outs[0]
	base := basename(name)
	if rest, wrapper, d := a.unwrap(base, args); wrapper {
		if d != nil || len(rest) == 0 {
			return d
		}
		return a.run(rest, redirs, sure)
	}
	names := []string{base}
	if strings.Contains(name, "/") { // what runs: ./x may be a symlink to /bin/sh
		if real := basename(realPath(a.cwds[0], name)); real != base {
			names = append(names, real)
		}
	}
	for _, n := range names {
		if d := a.deny(n, args, redirs); d != nil {
			return d
		}
	}
	switch base {
	case "cd":
		return a.cd(args, sure)
	case "source", ".":
		return a.source(args)
	case "export":
		for _, w := range args[1:] {
			if w.assign {
				name, value, _ := strings.Cut(w.val, "=")
				if d := a.envOverride(name, value); d != nil {
					return d
				}
				a.assign(w.val)
			}
		}
		return nil
	case "set", "unset":
		return nil
	}
	if strings.Contains(name, "/") {
		if d := a.touchWord(name, word{}, pathUse{}); d != nil {
			return d
		}
	}
	if !valueBuiltins[base] {
		if d := a.fileArgs(base, args); d != nil {
			return d
		}
	}
	if allowedBins[base] || pythonName.MatchString(base) {
		return nil
	}
	for _, p := range relativeScriptPrefixes {
		if strings.HasPrefix(name, p) {
			return nil
		}
	}
	return ask(`unrecognized command "` + display(args[0].val) + `"`)
}

// unwrap returns what a wrapper runs (wrapper=false: not a wrapper).
func (a *analyzer) unwrap(base string, args []word) (rest []word, wrapper bool, d *Decision) {
	switch base {
	case "env":
		return a.env(args)
	case "command":
		i := skipOptions(args, 1, nil)
		if hasWord(args[:i], "-v", "-V") {
			return nil, true, nil // a lookup, like which
		}
		return args[i:], true, nil
	case "builtin", "nohup", "time", "nice", "stdbuf", "exec", "timeout":
		i := skipOptions(args, 1, wrapperValues[base])
		if base == "timeout" {
			i = min(i+1, len(args)) // the duration
		}
		return args[i:], true, nil
	}
	return nil, false, nil
}

func (a *analyzer) env(args []word) ([]word, bool, *Decision) {
	for i := 1; i < len(args); i++ {
		w := args[i]
		switch {
		case w.val == "--":
			return args[i+1:], true, nil
		case w.assign:
			name, value, _ := strings.Cut(w.val, "=")
			if d := a.envOverride(name, value); d != nil {
				return nil, true, d
			}
		case w.val == "-u":
			i++
		case envOptions[w.val] || strings.HasPrefix(w.val, "--unset="):
		case isOption(w.val): // -S splits a string into a command, -C/-P move it
			return nil, true, ask(`env option "` + w.val + `" hides what runs`)
		default:
			return args[i:], true, nil
		}
	}
	return nil, true, nil // bare env prints the environment
}

// envOverride judges NAME=value before a program (or exported).
func (a *analyzer) envOverride(name, value string) *Decision {
	switch {
	case commandEnv[name]: // the value is a command line: judge it
		cmds, why := lexShell(value, homeEnv())
		if why != "" {
			return ask(why)
		}
		return a.nested(cmds)
	case configEnv[name], strings.HasPrefix(name, "LD_"), strings.HasPrefix(name, "DYLD_"),
		strings.HasPrefix(name, "GIT_CONFIG"), name == "NODE_OPTIONS" && nodeOptionsLoad.MatchString(value):
		return ask("environment override (" + name + ")")
	}
	return nil
}

// deny holds the rules keyed by program name.
func (a *analyzer) deny(name string, args []word, redirs []redirect) *Decision {
	if pythonName.MatchString(name) {
		name = "python"
	}
	if k, ok := interpreters[name]; ok {
		return a.interpreter(name, k, args, redirs)
	}
	switch {
	case privileged[name]:
		return ask("privilege escalation")
	case network[name], name == "openssl" && hasWord(args, "s_client"):
		return ask("network beyond connectors")
	case name == "xargs":
		return ask("xargs runs arbitrary commands")
	case name == "eval":
		return inline("eval")
	case shells[name]:
		return a.shell(name, args, redirs)
	case name == "git":
		return a.git(args)
	case name == "docker" && dockerRun(operands(args)):
		return ask("docker run can mount arbitrary paths")
	case publishes(name, operands(args)):
		return ask("publishing packages")
	}
	for _, o := range execOptions[name] {
		for _, w := range args[1:] {
			if w.val == o || strings.HasPrefix(w.val, o+"=") {
				return ask(name + " " + o + " runs commands or writes files")
			}
		}
	}
	if name == "find" && hasWord(args, "-delete") {
		return a.findDelete(args)
	}
	return nil
}

// findDelete: find -delete may only start inside the workspace or temp
// dirs (no start path: the current directory, always inside).
func (a *analyzer) findDelete(args []word) *Decision {
	for _, w := range args[1:] {
		v := w.val
		if v == "-H" || v == "-L" || v == "-P" {
			continue
		}
		if strings.HasPrefix(v, "-") || v == "(" || v == "!" {
			break // the expression starts
		}
		if d := a.writes(w, "find -delete outside the workspace"); d != nil {
			return d
		}
	}
	return nil
}

func dockerRun(ops []word) bool {
	return len(ops) > 0 && (ops[0].val == "run" || ops[0].val == "create" ||
		ops[0].val == "container" && len(ops) > 1 && (ops[1].val == "run" || ops[1].val == "create"))
}

func publishes(name string, ops []word) bool {
	switch {
	case len(ops) == 0:
		return false
	case name == "npm", name == "pnpm", name == "yarn", name == "bun", name == "cargo":
		return ops[0].val == "publish"
	}
	return name == "gem" && ops[0].val == "push"
}

// shell: sh -c and a script read from stdin are inline code; a script file
// is checked like any path.
func (a *analyzer) shell(name string, args []word, redirs []redirect) *Decision {
	for i := 1; i < len(args); i++ {
		v := args[i].val
		switch {
		case v == "--":
			return a.script(name, true, args, i+1, redirs)
		case !isOption(v) && !strings.HasPrefix(v, "+"):
			return a.script(name, true, args, i, redirs)
		case v == "-o" || v == "+o" || v == "--rcfile" || v == "--init-file":
			i++
		case shortCluster(v) && strings.ContainsAny(v[1:], "cs"):
			return inline(name + " " + v)
		}
	}
	return a.script(name, true, args, len(args), redirs)
}

func (a *analyzer) interpreter(name string, k interp, args []word, redirs []redirect) *Decision {
	for i := 1; i < len(args); i++ {
		v := args[i].val
		opt, _, _ := strings.Cut(v, "=")
		switch {
		case v == "--":
			return a.script(name, k.stdin, args, i+1, redirs)
		case inlineLong[opt]:
			return inline(name + " " + opt)
		case moduleLong[opt]:
			module := ""
			if _, val, ok := strings.Cut(v, "="); ok {
				module = val
			} else if i+1 < len(args) {
				i++
				module = args[i].val
			}
			if strings.HasPrefix(module, "data:") {
				return inline(name + " " + opt + " data:")
			}
			continue
		case !isOption(v):
			if k.subcommands && (v == "eval" || v == "repl") {
				return inline(name + " " + v)
			}
			return a.script(name, k.stdin, args, i, redirs)
		case !shortCluster(v):
			continue
		}
		for j := 1; j < len(v); j++ {
			switch ch := v[j]; {
			case strings.IndexByte(k.inline, ch) >= 0:
				return inline(name + " -" + string(ch))
			case strings.IndexByte(k.stop, ch) >= 0:
				return nil
			case strings.IndexByte(k.valued, ch) >= 0:
				if j == len(v)-1 {
					i++ // the value is the next word
				}
				j = len(v)
			}
		}
	}
	return a.script(name, k.stdin, args, len(args), redirs)
}

// script judges the program an interpreter runs: args[i], or stdin when
// there is none.
func (a *analyzer) script(name string, stdin bool, args []word, i int, redirs []redirect) *Decision {
	if i < len(args) {
		w := args[i]
		if _, lead := a.expand(w.val); w.procSub || w.val == "-" || lead != "" {
			return inline(name + " running a script chosen at run time")
		}
		return nil
	}
	if !stdin || slices.ContainsFunc(args[1:], func(w word) bool { return versionFlags[w.val] }) {
		return nil // --version, --help
	}
	for _, r := range redirs {
		if r.op == "<" {
			return nil // the script is that file
		}
	}
	return inline(name + " reading its script from stdin")
}

// git rules. The options between "git" and its subcommand (-C, -c,
// --git-dir, --work-tree, --exec-path, …) can redirect or reconfigure git
// and hide the subcommand from every rule below.
func (a *analyzer) git(args []word) *Decision {
	i := 1
	for ; i < len(args) && isOption(args[i].val); i++ {
		switch v := args[i].val; {
		case harmlessGitOptions[v]:
		case v == "-C" && i+1 < len(args):
			i++
			if d := a.gitDir(args[i]); d != nil {
				return d
			}
		case (v == "--version" || v == "--help") && len(args) == 2:
			return nil
		default:
			return ask("git global options (" + v + ")")
		}
	}
	if i == len(args) {
		return nil
	}
	args = args[i-1:] // as if the subcommand followed "git" directly
	sub, rest := args[1].val, args[2:]
	discard := ask("mass-discarding local work")
	switch sub {
	case "push":
		return ask("git push is the MR step (request_mr)")
	case "remote":
		if ops := operands(args[1:]); len(ops) > 0 && slices.Contains([]string{"add", "set-url", "remove", "rm"}, ops[0].val) {
			return ask("changing remotes")
		}
	case "reset":
		if hasWord(args[1:], "--hard") {
			return discard
		}
	case "clean":
		for _, w := range rest {
			if w.val == "--force" || shortCluster(w.val) && strings.ContainsAny(w.val[1:], "fdx") {
				return discard
			}
		}
	case "checkout", "restore", "switch":
		return a.gitCheckout(rest, discard)
	case "branch":
		del, force := hasWord(args[1:], "--delete"), hasWord(args[1:], "--force")
		for _, w := range rest {
			if shortCluster(w.val) {
				del = del || strings.ContainsAny(w.val[1:], "dD")
				force = force || strings.ContainsAny(w.val[1:], "fD")
			}
		}
		if del && force {
			return discard
		}
	case "config":
		return gitConfig(rest)
	case "submodule", "filter-branch", "filter-repo", "update-ref", "symbolic-ref", "worktree", "replace":
		return ask("git " + sub + " rewrites git refs or config")
	case "credential", "credential-store", "credential-cache", "credential-osxkeychain":
		return ask("git " + sub + " reads stored credentials")
	case "bisect":
		if ops := operands(args[1:]); len(ops) > 0 && ops[0].val == "run" {
			return ask("git bisect run runs commands")
		}
	}
	for _, o := range gitExecOptions[sub] {
		for _, w := range rest {
			if w.val == o || strings.HasPrefix(w.val, o+"=") || len(o) == 2 && strings.HasPrefix(w.val, o) {
				return ask("git " + sub + " " + o + " runs commands")
			}
		}
	}
	return nil
}

var harmlessGitOptions = setOf("--no-pager", "-P", "--no-optional-locks", "--literal-pathspecs")

// gitDir: git -C may only move into the workspace.
func (a *analyzer) gitDir(w word) *Decision {
	outs, lead := a.expand(w.val)
	if lead != "" {
		return varAsk(lead)
	}
	for _, p := range outs {
		if d := a.each(p, w, func(abs, real string) *Decision {
			if !insideAny(abs, a.workspace) || !insideAny(real, a.workspace) {
				return ask("git -C " + display(abs) + " outside the workspace")
			}
			return nil
		}); d != nil {
			return d
		}
	}
	return nil
}

// git subcommand options that run a program.
var gitExecOptions = map[string][]string{
	"rebase":    {"--exec", "-x"},
	"difftool":  {"--extcmd", "-x"},
	"grep":      {"--open-files-in-pager", "-O"},
	"fetch":     {"--upload-pack"},
	"pull":      {"--upload-pack"},
	"ls-remote": {"--upload-pack"},
	"clone":     {"--upload-pack", "-u", "--config", "-c", "--template"},
	"archive":   {"--exec", "--remote"},
}

// gitCheckout: forcing a checkout/switch, or a pathspec covering the whole
// tree (".", "..", "*", ":/" magic), throws away uncommitted work.
func (a *analyzer) gitCheckout(rest []word, discard *Decision) *Decision {
	opts := true
	for _, w := range rest {
		v := w.val
		switch {
		case opts && v == "--":
			opts = false
		case opts && isOption(v):
			if v == "--force" || v == "--discard-changes" || shortCluster(v) && strings.ContainsRune(v[1:], 'f') {
				return discard
			}
		case v == "*" || strings.HasPrefix(v, ":"):
			return discard
		default:
			for _, cwd := range a.cwds {
				if p := resolve(cwd, v); p == cwd || strings.HasPrefix(cwd, p+string(filepath.Separator)) {
					return discard
				}
			}
		}
	}
	return nil
}

var (
	gitConfigReads      = setOf("--get", "--get-all", "--get-regexp", "--get-urlmatch", "--list", "-l")
	gitConfigQualifiers = setOf("--show-origin", "--show-scope", "--local", "--global", "--system", "--worktree",
		"--null", "-z", "--name-only", "--includes", "--no-includes", "--bool", "--int", "--bool-or-int", "--path")
)

var gitIdentity = setOf("user.name", "user.email")

// gitConfig auto-approves reads — --get*, --list, `git config <key>`,
// `git config get|list …` — and setting this repository's user.name or
// user.email. Every other write (aliases, hooksPath, sshCommand, credential
// helpers, url rewrites, includes, …) asks.
func gitConfig(rest []word) *Decision {
	read, write := false, false
	var ops []string
	for _, w := range rest {
		v := w.val
		switch {
		case gitConfigReads[v]:
			read = true
		case gitConfigQualifiers[v] || strings.HasPrefix(v, "--type=") || strings.HasPrefix(v, "--default="):
		case isOption(v): // --unset, --add, --replace-all, --edit, -e, --file, …
			write = true
		default:
			ops = append(ops, v)
		}
	}
	switch {
	case !write && (read || len(ops) <= 1 || ops[0] == "get" || ops[0] == "list"):
		return nil
	case len(ops) == 2 && gitIdentity[strings.ToLower(ops[0])] &&
		!slices.ContainsFunc(rest, func(w word) bool { return isOption(w.val) && w.val != "--local" }):
		return nil // the commit identity, in this repository
	case slices.ContainsFunc(rest, func(w word) bool { return w.val == "--global" }):
		return ask("global git config")
	}
	return ask("git config write")
}

// cd moves the directories the shell may be in; it must stay in the
// checkout or the state dir. A bare cd goes to $HOME: out.
func (a *analyzer) cd(args []word, sure bool) *Decision {
	i := skipOptions(args, 1, nil)
	if i == len(args) {
		return ask("cd out of the workspace")
	}
	outs, lead := a.expand(args[i].val)
	switch {
	case lead != "":
		return varAsk(lead)
	case outs[0] == "":
		return nil // cd "" stays put
	case outs[0] == "-":
		return ask("cd out of the workspace")
	}
	var dirs []string
	for _, cwd := range a.cwds {
		for _, p := range outs {
			abs := resolve(cwd, p)
			if !insideAny(abs, a.workspace) || !insideAny(realPath(cwd, p), a.workspace) {
				return ask("cd out of the workspace")
			}
			dirs = append(dirs, abs)
		}
	}
	if sure {
		a.pending = union(a.pending, a.cwds)
		a.cwds = dirs
	} else {
		a.cwds = union(a.cwds, dirs)
	}
	return nil
}

// source runs a script in this shell: it must be inside the allowed roots
// (the workspace, temp dirs, toolchains such as ~/.nvm — not ~/.bashrc), and
// afterwards any variable may hold anything.
func (a *analyzer) source(args []word) *Decision {
	if len(args) < 2 {
		return nil
	}
	w := args[1]
	outs, lead := a.expand(w.val)
	if w.procSub || lead != "" {
		return inline("source of a script chosen at run time")
	}
	a.vars, a.loopVars = map[string]string{}, map[string][]string{}
	for _, p := range outs {
		if d := a.each(p, w, func(abs, real string) *Decision {
			if !insideAny(abs, a.roots) || !insideAny(real, a.roots) {
				return ask("sources " + display(abs) + " from outside the workspace")
			}
			return nil
		}); d != nil {
			return d
		}
	}
	return nil
}

// fileArgs checks a program's file operands.
func (a *analyzer) fileArgs(name string, args []word) *Decision {
	if gitWriters[name] && (name != "sed" || inPlace(args)) {
		for _, w := range operands(args) {
			outs, _ := a.expand(w.val)
			for _, p := range outs {
				if d := a.gitInternals(p, w); d != nil {
					return d
				}
			}
		}
	}
	why := ""
	if name == "rm" || name == "rmdir" || name == "unlink" {
		why = "rm outside the workspace"
	}
	for _, w := range writeTargets(name, args) {
		if d := a.writes(w, why); d != nil {
			return d
		}
	}
	switch {
	case why != "":
		return nil // rm's operands are only ever write targets
	case name == "ln" && (hasWord(args, "--symbolic") || slices.ContainsFunc(args[1:], func(w word) bool {
		return shortCluster(w.val) && strings.ContainsRune(w.val, 's')
	})):
		return a.symlink(args)
	}
	return a.args(name, args)
}

func inPlace(args []word) bool {
	return slices.ContainsFunc(args[1:], func(w word) bool {
		return strings.HasPrefix(w.val, "--in-place") || shortCluster(w.val) && strings.ContainsRune(w.val, 'i')
	})
}

var (
	writeAll  = setOf("rm", "rmdir", "unlink", "mv", "tee", "chmod", "chown", "touch", "mkdir", "rsync")
	writeLast = setOf("cp", "ln", "install")
)

// writeTargets are the operands a program writes: all of them, or the
// destination (last operand, -t DIR, --target-directory=DIR).
func writeTargets(name string, args []word) []word {
	ops := operands(args)
	switch {
	case writeAll[name], name == "install" && hasWord(args, "-d"):
		return ops
	case !writeLast[name]:
		return nil
	}
	for i, w := range args[1:] {
		if w.val == "-t" && i+2 < len(args) {
			return []word{args[i+2]}
		}
		if dir, ok := strings.CutPrefix(w.val, "--target-directory="); ok {
			w.val = dir
			return []word{w}
		}
	}
	if len(ops) < 2 {
		return nil // ln -s x: the link lands in the current directory
	}
	return ops[len(ops)-1:]
}

// writes: a write must land inside the workspace or temp dirs — not in the
// toolchain roots that are fine to read (~/.local/bin, /opt/homebrew/bin, …).
func (a *analyzer) writes(w word, why string) *Decision {
	outs, lead := a.expand(w.val)
	if lead != "" {
		return varAsk(lead)
	}
	for _, p := range outs {
		outside := ask(why)
		if why == "" {
			outside = ask("writes " + display(p) + " outside the workspace")
		}
		if w.quoted && strings.HasPrefix(p, "~") {
			return outside // '~': a target that only looks harmless
		}
		if d := a.each(p, w, func(abs, real string) *Decision {
			if !insideAny(abs, a.writable) || !insideAny(real, a.writable) {
				return outside
			}
			return nil
		}); d != nil {
			return d
		}
	}
	return nil
}

// symlink: ln -s resolves each target from the link's directory, which must
// not lead outside the allowed roots.
func (a *analyzer) symlink(args []word) *Decision {
	ops := operands(args)
	targets := ops
	if len(ops) > 1 {
		targets = ops[:len(ops)-1]
	}
	for _, cwd := range a.cwds {
		dir := cwd
		if len(ops) > 1 {
			dests, _ := a.expand(ops[len(ops)-1].val)
			dest := resolve(cwd, dests[0])
			if dir = filepath.Dir(dest); isDir(dest) {
				dir = dest
			}
		}
		for _, w := range targets {
			outs, lead := a.expand(w.val)
			if lead != "" {
				return varAsk(lead)
			}
			for _, p := range outs {
				if abs := resolve(dir, p); !insideAny(abs, a.roots) || !insideAny(realPath(dir, p), a.roots) {
					return ask("symlink to " + display(abs) + " outside the workspace")
				}
			}
		}
	}
	return nil
}

// args checks every argument that can name a file: literal paths (outside
// the roots, through .., through symlinks) and, for file programs, paths
// chosen at run time.
func (a *analyzer) args(name string, args []word) *Decision {
	use := pathUse{links: true, root: searchers[name]}
	opts := true
	for i := 1; i < len(args); i++ {
		w := args[i]
		v := w.val
		switch {
		case w.procSub:
		case opts && v == "--":
			opts = false
		case opts && isOption(v):
			// --file=PATH, -oPATH, -I../include
			p := ""
			if _, val, ok := strings.Cut(v, "="); ok {
				p = val
			} else if shortCluster(v) && len(v) > 2 && strings.IndexByte("/.~", v[2]) >= 0 {
				p = v[2:]
			}
			if outs, lead := a.expand(p); p != "" && lead == "" {
				for _, o := range outs {
					if d := a.touchWord(o, w, use); d != nil {
						return d
					}
				}
			}
		default:
			outs, lead := a.expand(v)
			if lead != "" {
				// $X/… is a path whatever the program; a bare $X is one for
				// file programs (unless it is an option's value: head -n $N).
				prev := args[i-1].val
				valueOpt := shortCluster(prev) && strings.IndexByte(valueOptions[name], prev[len(prev)-1]) >= 0
				slash := slices.ContainsFunc(outs, func(o string) bool { return strings.HasPrefix(o, mark(lead)+"/") })
				if slash || fileCommands[name] && !valueOpt {
					return varAsk(lead)
				}
				continue
			}
			for _, o := range outs {
				if d := a.touchWord(o, w, use); d != nil {
					return d
				}
			}
		}
	}
	return nil
}

// BashAllowed decides one Bash command.
func BashAllowed(command string, ctx Context) Decision {
	cmd := textutil.JSTrim(command)
	if cmd == "" {
		return Decision{Allow: false, Reason: "empty command"}
	}
	if jsLength(cmd) > 4000 {
		return Decision{Allow: false, Reason: "very long command"}
	}
	for _, rule := range alwaysAsk {
		if rule.re.MatchString(cmd) {
			return Decision{Allow: false, Reason: rule.why}
		}
	}
	// The TS path scan, with heredoc DATA removed so commit messages and file
	// payloads don't read as paths (interpreter bodies are code: kept).
	roots := allowedRoots(ctx)
	sensitive := sensitiveRoots()
	for _, p := range pathsIn(StripHeredocs(cmd, true)) {
		if insideAny(p, roots) {
			continue
		}
		if resolve(p) == string(filepath.Separator) {
			continue // "//" and friends: prose, not root access
		}
		// Only real filesystem locations count: a slash-token whose path AND
		// parent don't exist (`/api/people/employees` in a tinker script) is a
		// route or prose, not a touch — and a nonexistent direct child of "/"
		// ("/8" from code text) is prose too. Writes need an existing parent.
		// Sensitive roots skip this: they always read as a touch. (An
		// unstatable path reads as nonexistent, as fs.existsSync did.)
		if !insideAny(p, sensitive) {
			parent := filepath.Dir(resolve(p))
			if !exists(p) && (parent == string(filepath.Separator) || !exists(parent)) {
				continue
			}
		}
		return Decision{Allow: false, Reason: "touches " + p + " outside the workspace"}
	}
	// The token-level pass: every command the line runs, with its real
	// program name and real paths.
	cmds, why := lexShell(cmd, homeEnv())
	if why != "" {
		return Decision{Allow: false, Reason: why}
	}
	if d := newAnalyzer(ctx).commands(cmds); d != nil {
		return *d
	}
	return Decision{Allow: true, Reason: "routine work inside the workspace"}
}

// Allows is the approval_prompt hook (TS taskPolicyAllows): harness tool +
// input → allow (auto-approve) or ask (raise the card). Paths resolve
// through symlinks, and edits never reach into .git.
func Allows(toolName string, input map[string]any, ctx Context) Decision {
	str := func(k string) string { return textutil.Str(input, k) }
	roots := withReal(resolveAll([]string{ctx.TaskDir, ctx.StateDir, "/tmp", "/private/tmp"}))
	inside := func(p, base string) (abs, real string, ok bool) {
		abs, real = resolve(base, p), realPath(base, p) // "" → base: tools default to the workspace
		return abs, real, insideAny(abs, roots) && insideAny(real, roots)
	}
	switch toolName {
	case "Read":
		p := str("file_path")
		if _, _, ok := inside(p, ctx.TaskDir); ok {
			return Decision{Allow: true, Reason: "file inside the workspace"}
		}
		return Decision{Allow: false, Reason: "file outside the workspace: " + p}
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		key, kind := "file_path", "file"
		if toolName == "NotebookEdit" {
			key, kind = "notebook_path", "notebook"
		}
		p := str(key)
		abs, real, ok := inside(p, ctx.TaskDir)
		switch {
		case !ok && kind == "notebook":
			return Decision{Allow: false, Reason: "notebook outside the workspace"}
		case !ok:
			return Decision{Allow: false, Reason: "file outside the workspace: " + p}
		case inGitDir(abs, real):
			return Decision{Allow: false, Reason: "editing git internals: " + p}
		}
		return Decision{Allow: true, Reason: kind + " inside the workspace"}
	case "Glob", "Grep":
		_, base, ok := inside(str("path"), ctx.TaskDir)
		if toolName == "Glob" {
			alts := braces(str("pattern"))
			ok = ok && len(alts) <= maxAlternatives
			for _, alt := range alts {
				_, _, in := inside(alt, base)
				ok = ok && in
			}
		}
		if ok {
			return Decision{Allow: true, Reason: "search inside the workspace"}
		}
		return Decision{Allow: false, Reason: "search outside the workspace"}
	case "Bash":
		return BashAllowed(str("command"), ctx)
	default:
		return Decision{Allow: false, Reason: toolName + " is not part of the task profile"}
	}
}
