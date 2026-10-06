package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// backend is a fake ex server: it records calls and answers per
// "METHOD path" (query string ignored), defaulting to 200 {}.
type backend struct {
	srv     *httptest.Server
	mu      sync.Mutex
	calls   []bcall
	answers map[string][]answer // consumed in order; the last one repeats
}

type bcall struct {
	method, path, query, auth, ctype string
	body                             map[string]any
	rawBody                          string
}

type answer struct {
	status int
	body   string
}

func newBackend(t *testing.T) *backend {
	b := &backend{answers: map[string][]answer{}}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		key := r.Method + " " + r.URL.Path
		b.mu.Lock()
		b.calls = append(b.calls, bcall{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), body, string(raw)})
		list := b.answers[key]
		a := answer{200, "{}"}
		if len(list) > 0 {
			a = list[0]
			if len(list) > 1 {
				b.answers[key] = list[1:]
			}
		}
		b.mu.Unlock()
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *backend) on(key string, answers ...answer) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.answers[key] = answers
}

func (b *backend) last() bcall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[len(b.calls)-1]
}

func (b *backend) to(key string) []bcall {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []bcall
	for _, c := range b.calls {
		if c.method+" "+c.path == key {
			out = append(out, c)
		}
	}
	return out
}

func testServer(t *testing.T, b *backend, over func(e *Env)) *Server {
	t.Helper()
	env := LoadEnv(func(string) string { return "" }, t.TempDir())
	env.BaseURL = b.srv.URL
	env.RunToken = "run-tok"
	if over != nil {
		over(&env)
	}
	s := NewServer(env)
	s.pollEvery = time.Millisecond
	return s
}

// do runs one tool and returns its text and error flag.
func do(s *Server, name string, args map[string]any) (string, bool) {
	r := s.safeCall(name, args)
	return r.Content[0].Text, r.IsError
}

func expect(t *testing.T, s *Server, name string, args map[string]any, wantErr bool, wantText string) {
	t.Helper()
	text, isErr := do(s, name, args)
	if isErr != wantErr || !strings.Contains(text, wantText) {
		t.Errorf("%s(%v) = (%q, err=%v); want err=%v containing %q", name, args, text, isErr, wantErr, wantText)
	}
}

func itoaT(n int) string { return strconv.Itoa(n) }

func time_after() <-chan time.Time { return time.After(10 * time.Millisecond) }
