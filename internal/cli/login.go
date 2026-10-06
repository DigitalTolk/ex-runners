package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/api"
	"github.com/DigitalTolk/ex-runners/internal/textutil"
)

// `ex-runner login <server>`: pair this machine with the user's account.
//
//  1. Listen on 127.0.0.1:<random port> for one callback.
//  2. Open the browser at <server>/runner/connect?port&state&challenge&name.
//     The signed-in user clicks Connect; the page asks the server for a
//     one-time code bound to our PKCE challenge…
//  3. …and sends the browser to http://127.0.0.1:<port>/callback?code&state.
//  4. We redeem code + verifier for the runner token.
//
// The token itself never passes through the browser, and the code is
// useless without the verifier that never left this process.

const (
	loginTimeout = 10 * time.Minute
	labelMax     = 64
)

// loginError is a failure whose message is already user-facing.
type loginError struct{ msg string }

func (e *loginError) Error() string { return e.msg }

var schemeRe = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://`)

// normalizeServer turns what the user typed into an origin. A bare host gets
// https://; anything that isn't http(s) is refused.
func normalizeServer(input string) (string, bool) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", false
	}
	if !schemeRe.MatchString(s) {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}

// machineLabel is the name this install shows under on the Runners page.
func machineLabel(hostname string) string {
	name := strings.TrimSpace(hostname)
	if strings.HasSuffix(strings.ToLower(name), ".local") {
		name = name[:len(name)-len(".local")]
	}
	name = strings.TrimSpace(textutil.Head(name, labelMax))
	if name == "" {
		return "this computer"
	}
	return name
}

func connectURL(server string, port int, state, challenge, label string) string {
	q := url.Values{}
	q.Set("port", fmt.Sprint(port))
	q.Set("state", state)
	q.Set("challenge", challenge)
	q.Set("name", label)
	return server + "/runner/connect?" + q.Encode()
}

// listen is a seam so tests can make the localhost listener fail.
var listen = net.Listen

type callbackResult struct{ code, err string }

type callbackServer struct {
	port   int
	result chan callbackResult
	once   sync.Once
	srv    *http.Server
	done   chan struct{}
}

const pageStyle = `body{font:16px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;display:grid;place-items:center;min-height:90vh;margin:0}` +
	`main{max-width:26rem;padding:2rem;text-align:center}h1{font-size:1.3rem;margin:0 0 .5rem}p{margin:0;color:#555}`

func page(title, body string) string {
	return `<!doctype html><meta charset="utf-8"><title>` + title + `</title><style>` + pageStyle +
		`</style><main><h1>` + title + `</h1><p>` + body + `</p></main>`
}

// startCallbackServer waits for exactly one valid callback. Requests with
// the wrong path or state are answered and ignored — they cannot end the
// login — and the listener closes as soon as a result (or the timeout)
// lands.
func startCallbackServer(state string, timeout time.Duration) (*callbackServer, error) {
	ln, err := listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	cs := &callbackServer{port: ln.Addr().(*net.TCPAddr).Port, result: make(chan callbackResult, 1), done: make(chan struct{})}
	cs.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/callback" || q.Get("state") != state {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "Not a valid ex-runner sign-in callback.")
			return
		}
		code, errParam := q.Get("code"), q.Get("error")
		if code == "" && errParam == "" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "Missing code.")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if code != "" {
			_, _ = io.WriteString(w, page("ex-runner is connected", "You can close this tab and return to your terminal."))
			cs.finish(callbackResult{code: code})
			return
		}
		_, _ = io.WriteString(w, page("Sign-in cancelled", "Nothing was connected. You can close this tab."))
		cs.finish(callbackResult{err: errParam})
	})}
	go func() {
		select {
		case <-time.After(timeout):
			cs.finish(callbackResult{err: "timeout"})
		case <-cs.done:
		}
	}()
	go func() { _ = cs.srv.Serve(ln) }()
	return cs, nil
}

func (cs *callbackServer) finish(r callbackResult) {
	cs.once.Do(func() {
		close(cs.done)
		cs.result <- r
		// finish usually runs inside the handler: Shutdown waits for that
		// response to reach the browser (Close would drop it mid-write and
		// the tab would show a connection error); Close then ends anything
		// still hanging after the grace period.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = cs.srv.Shutdown(ctx)
			_ = cs.srv.Close()
		}()
	})
}

type exchangeResponse struct {
	Token     string `json:"token"`
	ID        string `json:"id"`
	Label     string `json:"label"`
	ExpiresAt string `json:"expiresAt"`
	UserName  string `json:"userName"`
}

// exchangeCode redeems the one-time code for the runner token.
func exchangeCode(ctx context.Context, client *http.Client, server, code, verifier string) (*exchangeResponse, error) {
	body, _ := json.Marshal(map[string]string{"code": code, "verifier": verifier})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/api/v1/runner-tokens/exchange", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode > 299 {
		msg := fmt.Sprintf("the server answered %d", res.StatusCode)
		var parsed map[string]any
		if json.Unmarshal(raw, &parsed) == nil {
			if _, m := api.ErrorFields(parsed); m != "" {
				msg = m
			}
		}
		return nil, &loginError{"Sign-in failed: " + msg + "."}
	}
	var out exchangeResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode exchange response: %w", err)
	}
	return &out, nil
}

type loginDeps struct {
	openBrowser func(url string)
	out         func(line string)
	label       string
	timeout     time.Duration
	client      *http.Client
}

func login(ctx context.Context, server string, d loginDeps) (*Credentials, error) {
	verifier, err := randomB64(32) // 43 chars, inside RFC 7636's 43–128 range
	if err != nil {
		return nil, err
	}
	// state binds the browser's redirect to THIS login attempt: a callback
	// that arrives without it (a stray page poking at localhost) is ignored.
	state, err := randomB64(16)
	if err != nil {
		return nil, err
	}
	cb, err := startCallbackServer(state, d.timeout)
	if err != nil {
		return nil, err
	}
	link := connectURL(server, cb.port, state, challengeFor(verifier), d.label)
	d.out(fmt.Sprintf("Opening your browser to connect \"%s\" to %s…", d.label, server))
	d.out(fmt.Sprintf("If it doesn't open, visit this link on this computer:\n\n  %s\n", link))
	d.openBrowser(link)

	var outcome callbackResult
	select {
	case outcome = <-cb.result:
	case <-ctx.Done():
		cb.finish(callbackResult{err: "closed"})
		return nil, &loginError{"Sign-in was interrupted. Nothing was connected."}
	}
	if outcome.err != "" {
		if outcome.err == "timeout" {
			return nil, &loginError{"Timed out waiting for you to approve in the browser. Run `ex-runner login` again."}
		}
		return nil, &loginError{"Sign-in was cancelled in the browser. Nothing was connected."}
	}
	tok, err := exchangeCode(ctx, d.client, server, outcome.code, verifier)
	if err != nil {
		return nil, err
	}
	return &Credentials{
		Server:    server,
		Token:     tok.Token,
		TokenID:   tok.ID,
		Label:     tok.Label,
		User:      tok.UserName,
		ExpiresAt: tok.ExpiresAt,
	}, nil
}
