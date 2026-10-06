package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/connectordocs"
	"github.com/DigitalTolk/ex-runners/internal/connectors"
	"github.com/DigitalTolk/ex-runners/internal/protocol"
	"github.com/DigitalTolk/ex-runners/internal/textutil"
)

// fetchRunConnectors is a seam over the connector payload fetch.
var fetchRunConnectors = connectors.FetchRunConnectors

func (s *Server) connectorNames() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.env.Connectors))
	for k := range s.env.Connectors {
		names = append(names, k)
	}
	if len(names) == 0 {
		return "none"
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (s *Server) connector(slug string) (connectorCred, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, found := s.env.Connectors[slug]
	return c, found
}

// attachedResult is what the model reads right after use_connector
// succeeds. It carries the one connector-specific part of the server's
// _USAGE.md — the service map — inline and points at connector_lookup, so
// the agent's next turn is a lookup rather than a file read.
func attachedResult(slug, title, dir string) string {
	lines := []string{fmt.Sprintf("attached: %s (%s).", slug, title)}
	if usage, found := connectordocs.ReadOptional(filepath.Join(dir, "_USAGE.md")); found {
		if services := connectordocs.ServicesFromUsage(usage); len(services) > 0 {
			if len(services) > 20 {
				services = services[:20]
			}
			lines = append(lines, "Services (route prefixes for connector_lookup's service scope):")
			lines = append(lines, services...)
		}
	}
	lines = append(lines,
		fmt.Sprintf("Next: connector_lookup(connector: '%s', query: '<words from the question>') — one call returns ", slug)+
			"the matching endpoints, the chosen contract and its enum values; then ONE complete connector_call.",
		fmt.Sprintf("Who the invoker is on this service: %s (grep the one field you need). ", filepath.Join(dir, "_identity.json"))+
			fmt.Sprintf("Full docs: %s — only for what lookup cannot answer.", dir),
	)
	return strings.Join(lines, "\n")
}

func (s *Server) useConnector(args map[string]any) toolResult {
	slug, reason := str(args, "connector"), str(args, "reason")
	if strings.TrimSpace(slug) == "" || strings.TrimSpace(reason) == "" {
		return fail("use_connector requires connector and reason")
	}
	if _, attached := s.connector(slug); attached {
		return ok(slug + " is already attached — call connector_call directly")
	}
	first := s.callBackend(http.MethodPost, "/api/v1/agent/run/use-connector", map[string]any{"connector": slug, "reason": reason})
	if !first.ok {
		return s.failure(first)
	}
	status := fmt.Sprint(first.data["status"])
	title := slug
	if t, isStr := first.data["title"].(string); isStr {
		title = t
	}
	if status == "ask" {
		// Consent gate. The BACKEND raises the card and hands back its id, so
		// the approval it later verifies is the one the human actually saw —
		// nothing the model writes can stand in for it. An older server sends
		// no id, and we compose the card ourselves.
		var v verdict
		if gate, isStr := first.data["approvalID"].(string); isStr && gate != "" {
			v = s.pollApproval(gate, time.Time{})
		} else {
			summary, _ := first.data["summary"].(string)
			if summary == "" {
				summary = fmt.Sprintf("Use the %s connector (%s) for this task: %s", title, slug, reason)
			}
			v = s.awaitApproval(summary, "", nil, "")
		}
		if v.err != "" {
			return fail(v.err)
		}
		if v.state != "approved" {
			return fail(fmt.Sprintf("the invoker did not approve using %s — do NOT try to reach this service another way; answer from what you have or say what is missing [retryable=false]", slug))
		}
		second := s.callBackend(http.MethodPost, "/api/v1/agent/run/use-connector", map[string]any{"connector": slug, "reason": reason, "approvalID": v.approvalID})
		if !second.ok {
			return s.failure(second)
		}
		status = fmt.Sprint(second.data["status"])
	}
	if status != "attached" {
		msg := stringOr(first.data["message"], "connector not available")
		return fail(msg + " — do NOT try to reach this service another way [retryable=false]")
	}
	// Attached server-side — pull the payload, sync docs beside the run's
	// workdir, and arm connector_call with the credential.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rows, err := fetchRunConnectors(ctx, s.env.BaseURL, s.env.RunToken)
	if err != nil {
		return fail(fmt.Sprintf("attached, but doc sync failed: %s — retry use_connector once", err.Error()))
	}
	if _, err := connectors.SyncChecked(filepath.Join(s.env.WorkDir, "connectors"), rows, protocol.NopLogger); err != nil {
		return fail(fmt.Sprintf("attached, but doc sync failed: %s — retry use_connector once", err.Error()))
	}
	s.mu.Lock()
	for _, r := range rows {
		s.env.Connectors[r.Slug] = connectorCred{Slug: r.Slug, Title: r.Title, BaseURL: r.BaseURL, Token: r.Token, AuthHeader: r.AuthHeader}
	}
	_, armed := s.env.Connectors[slug]
	s.mu.Unlock()
	// The server withholds a connector whose stored credential failed its
	// check (expired session, revoked key) — say so instead of "attached".
	if !armed {
		return fail(fmt.Sprintf("%s was approved, but its stored credential is expired or unavailable, so it can't be used in this run — tell the invoker to reconnect %s on the Connectors page. Do NOT try to reach this service another way [retryable=false]", slug, title))
	}
	return ok(attachedResult(slug, title, filepath.Join(s.env.WorkDir, "connectors", slug)))
}

func (s *Server) connectorLookup(args map[string]any) toolResult {
	slug := str(args, "connector")
	if _, attached := s.connector(slug); !attached {
		return fail(fmt.Sprintf("%q is not attached to this task (attached: %s) — call use_connector first", slug, s.connectorNames()))
	}
	text, isErr := connectordocs.Lookup(connectordocs.LookupInput{
		Dir:     filepath.Join(s.env.WorkDir, "connectors", slug),
		Slug:    slug,
		Query:   str(args, "query"),
		RouteID: str(args, "route_id"),
		Service: str(args, "service"),
	})
	return result(text, isErr)
}

// maxConnectorBody bounds one API response read into memory.
var maxConnectorBody = 64 << 20

var allowedMethods = map[string]bool{"GET": true, "POST": true, "PATCH": true, "PUT": true, "DELETE": true}

func (s *Server) connectorCall(args map[string]any) toolResult {
	slug := str(args, "connector")
	cred, attached := s.connector(slug)
	if !attached {
		return fail(fmt.Sprintf("unknown connector %q — attached to this task: %s", slug, s.connectorNames()))
	}
	method := strings.ToUpper(str(args, "method"))
	if method == "" {
		method = "GET"
	}
	if !allowedMethods[method] {
		return fail("unsupported method " + method)
	}
	// Gated watcher modes are read-only against external services too —
	// deterministic, matching the hidden posting surface.
	if s.env.Gated && method != "GET" {
		return fail(fmt.Sprintf("this %s run is read-only: only GET calls are allowed", s.env.ActionMode))
	}
	rawPath := str(args, "path")
	if strings.TrimSpace(rawPath) == "" || strings.Contains(rawPath, "://") {
		return fail("path must be a relative endpoint path from the docs (no host)")
	}
	// URL is pinned to the connector's base — the token can go nowhere else.
	u, err := url.Parse(strings.TrimRight(cred.BaseURL, "/") + "/" + strings.TrimLeft(rawPath, "/"))
	if err != nil {
		return fail(fmt.Sprintf("connector_call failed before reaching %s: %s", slug, err.Error()))
	}
	if q, isObj := args["query"].(map[string]any); isObj {
		values := u.Query()
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			values.Set(k, textutil.JSString(q[k]))
		}
		u.RawQuery = values.Encode()
	}
	var body io.Reader
	_, hasBody := args["body"]
	if hasBody {
		raw, _ := json.Marshal(args["body"]) // decoded JSON always re-encodes
		body = bytes.NewReader(raw)
	}
	// u came out of url.Parse and method is allow-listed, so this can't fail.
	req, _ := http.NewRequestWithContext(context.Background(), method, u.String(), body)
	// Anonymous connectors ship an empty token — send no credential header
	// rather than a bare "Bearer ". The header shape is the connector's
	// (Metabase: X-Api-Key).
	client := *s.http
	if cred.Token != "" {
		k, v := connectors.CredentialHeader(cred.AuthHeader, cred.Token)
		req.Header.Set(k, v)
		// Go drops Authorization on a redirect to another host, but not a
		// custom header (Metabase's X-Api-Key): drop ours the same way.
		client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if next.URL.Host != via[0].URL.Host {
				next.Header.Del(k)
			}
			return nil
		}
	}
	req.Header.Set("Accept", "application/json")
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := client.Do(req)
	if err != nil {
		return fail(fmt.Sprintf("connector_call failed before reaching %s: %s", slug, err.Error()))
	}
	defer func() { _ = res.Body.Close() }()
	rawBody, _ := io.ReadAll(io.LimitReader(res.Body, int64(maxConnectorBody)+1))
	if len(rawBody) > maxConnectorBody {
		return fail(fmt.Sprintf("connector_call: the %s response is larger than %d MB — narrow the query (filters, fields, page size)", slug, maxConnectorBody>>20))
	}
	text := string(rawBody)
	search := ""
	if u.RawQuery != "" {
		search = "?" + u.RawQuery
	}
	head := fmt.Sprintf("HTTP %d — %s %s%s (%d chars)", res.StatusCode, method, u.EscapedPath(), search, textutil.Len(text))
	// Audit trail: every raw response is published as a run artifact so the
	// invoker can inspect what the agent actually saw. Best-effort and
	// non-blocking: the tool result never waits on it.
	content := textutil.Head(text, 63*1024)
	if content == "" {
		content = "(empty body)"
	}
	go func() {
		defer func() { _ = recover() }()
		s.callBackend(http.MethodPost, "/api/v1/agent/run/artifacts", map[string]any{
			"kind":    "api_response",
			"title":   fmt.Sprintf("%s %s%s → %d", method, u.EscapedPath(), textutil.Head(search, 120), res.StatusCode),
			"content": content,
		})
	}()
	if res.StatusCode == http.StatusUnauthorized {
		return fail(fmt.Sprintf("%s\nThe stored %s credential was rejected — tell the invoker to reconnect the %s connector. Do not retry.", head, cred.Title, slug))
	}
	// Non-2xx still returns the body (error envelopes carry the reason).
	// Error bodies bypass the spill store, so clip them defensively.
	if res.StatusCode >= 400 {
		return fail(head + "\n" + textutil.Head(text, 4000))
	}
	// Large JSON never rides the prompt: save it locally and return only
	// pagination meta + key shape. The agent extracts the few fields it
	// needs from the file with capped shell commands. Non-JSON (or an
	// unwritable disk) falls back to the spill store.
	if textutil.Len(text) > 1500 {
		if summary, isJSON := shapeSummary(rawBody); isJSON {
			if saved, wrote := s.saveResponse(slug, rawBody); wrote {
				return ok(fmt.Sprintf("%s → saved: %s\n%s\n", head, saved, summary) +
					"[Full body is in the saved file. Extract ONLY the fields you need with capped " +
					"shell commands (grep/python | head); counts come from the meta above. Do NOT " +
					"read or cat the whole file. A 200 is not proof your filter applied — first " +
					"CHECK the result matches your request (does the total fit your filter? do the " +
					"rows carry the value you filtered on?); if it looks off, probe the saved file " +
					"to see why before concluding or trying another call.]")
			}
		}
	}
	return ok(head + "\n" + text)
}

// saveResponse writes a large API body under responses/ in the run workdir.
// Files persist with the thread workdir, so warm follow-ups can re-read
// earlier results for free.
func (s *Server) saveResponse(slug string, body []byte) (string, bool) {
	dir := filepath.Join(s.env.WorkDir, "responses")
	if os.MkdirAll(dir, 0o700) != nil {
		return "", false
	}
	s.mu.Lock()
	s.responseSeq++
	file := filepath.Join(dir, fmt.Sprintf("%s-%d.json", slug, s.responseSeq))
	s.mu.Unlock()
	if os.WriteFile(file, body, 0o600) != nil {
		return "", false
	}
	return file, true
}

// shapeSummary renders a JSON body's structure without its bulk: scalar and
// small-object top-level values verbatim (pagination meta survives whole),
// arrays as length + first-item keys. Key order is the body's own.
func shapeSummary(body []byte) (string, bool) {
	trimmed := bytes.TrimSpace(body)
	if !json.Valid(trimmed) {
		return "", false
	}
	switch trimmed[0] {
	case '[':
		var items []json.RawMessage
		_ = json.Unmarshal(trimmed, &items)
		return fmt.Sprintf("array[%d]%s", len(items), itemKeys(items)), true
	case '{':
		parts := []string{}
		for _, e := range orderedEntries(trimmed) {
			switch e.raw[0] {
			case '[':
				var items []json.RawMessage
				_ = json.Unmarshal(e.raw, &items)
				parts = append(parts, fmt.Sprintf("%s: array[%d]%s", e.key, len(items), itemKeys(items)))
			case '{':
				if len(e.raw) <= 400 {
					parts = append(parts, e.key+": "+string(e.raw))
				} else {
					parts = append(parts, fmt.Sprintf("%s: object(%d keys)", e.key, len(orderedEntries(e.raw))))
				}
			default:
				parts = append(parts, e.key+": "+string(e.raw))
			}
		}
		return textutil.Head(strings.Join(parts, "\n"), 1600), true
	default:
		return "", false // a bare scalar has no shape worth summarizing
	}
}

func itemKeys(items []json.RawMessage) string {
	if len(items) == 0 || len(items[0]) == 0 || items[0][0] != '{' {
		return ""
	}
	entries := orderedEntries(items[0])
	keys := make([]string, 0, len(entries))
	for i, e := range entries {
		if i == 25 {
			break
		}
		keys = append(keys, e.key)
	}
	return " — item keys: " + strings.Join(keys, ",")
}

type entry struct {
	key string
	raw json.RawMessage // compacted
}

// orderedEntries lists a JSON object's members in source order (a Go map
// would sort them), each value compacted like JSON.stringify would print it.
func orderedEntries(obj json.RawMessage) []entry {
	dec := json.NewDecoder(bytes.NewReader(obj))
	_, _ = dec.Token() // {
	var out []entry
	for dec.More() {
		tok, _ := dec.Token()
		key, _ := tok.(string)
		var raw json.RawMessage
		_ = dec.Decode(&raw)
		var compact bytes.Buffer
		_ = json.Compact(&compact, raw)
		out = append(out, entry{key: key, raw: compact.Bytes()})
	}
	return out
}
