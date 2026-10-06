// Package connectors is connector support: when the invoking message picked
// services with /slug tokens, pull the invoker's installed connector bundles
// from the backend (run-token scoped), sync the docs to disk, and produce:
//   - MCPConnectors: the credential payload for the MCP server (EX_CONNECTORS
//     env). API calls happen through the connector_call tool — auto-allowed,
//     URL pinned to the connector's base, token never visible to the shell.
//   - Instructions: a PRIORITY preamble for the top of the task prompt — the
//     auto-attached "connector skill". It teaches surgical doc access (grep
//     the catalog, read ONE endpoint block) so the multi-thousand-line YAMLs
//     never land in context wholesale.
//
// Each synced connector ships a server-generated _USAGE.md beside its docs so
// the agent can re-read the workflow locally mid-task without any prompt
// cost.
package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/DigitalTolk/ex-runners/internal/textutil"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/DigitalTolk/ex-runners/internal/connectordocs"
	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

// File is one document of a connector bundle.
type File struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// Connector is one installed connector shipped to the runner (mirrors the
// server's service.RunnerConnector): docs bundle + the invoker's token.
type Connector struct {
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Description string `json:"description"`
	BaseURL     string `json:"baseURL"`
	EnvPrefix   string `json:"envPrefix"`
	Token       string `json:"token"`
	// AuthHeader is the header template the token rides in — "X-Api-Key:
	// {token}" for Metabase API keys; empty = "Authorization: Bearer {token}".
	AuthHeader string `json:"authHeader,omitempty"`
	Files      []File `json:"files"`
}

// CredentialHeader renders a connector's authHeader template into the
// (name, value) pair that carries the token. Mirrors model.RenderAuthHeader
// on the server: "Name: prefix {token}", "Name: {token}", "Name: Prefix"
// (token appended) or a bare "Name" (token as the whole value).
func CredentialHeader(template, token string) (name, value string) {
	t := textutil.JSTrim(template)
	if t == "" {
		t = "Authorization: Bearer {token}"
	}
	rest := ""
	if colon := strings.Index(t, ":"); colon < 0 {
		name = textutil.JSTrim(t)
	} else {
		name = textutil.JSTrim(t[:colon])
		rest = textutil.JSTrim(t[colon+1:])
	}
	if rest == "" {
		return name, token
	}
	if strings.Contains(rest, "{token}") {
		return name, strings.ReplaceAll(rest, "{token}", token)
	}
	return name, rest + " " + token
}

// Setup is what a sync hands the run.
type Setup struct {
	// MCPConnectors is JSON for the MCP server's EX_CONNECTORS env var —
	// empty string when the run has no connectors.
	MCPConnectors string
	// Instructions is the task-priority preamble, prepended to the prompt.
	// Empty when none attached.
	Instructions string
}

// EmptySetup is the setup of a run without connectors.
var EmptySetup = Setup{}

// FetchRunConnectors pulls the run's connectors with the run token (the
// backend filters to the invoker's installs ∩ the message's /picks).
func FetchRunConnectors(ctx context.Context, baseURL, runToken string) ([]Connector, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/agent/run/connectors", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+runToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, fmt.Errorf("connectors fetch failed: %d", res.StatusCode)
	}
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	var body struct {
		Connectors []Connector `json:"connectors"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	if body.Connectors == nil {
		return []Connector{}, nil
	}
	return body.Connectors, nil
}

// File-system seams, swapped by tests to simulate failures the OS will not
// produce on demand (a locked file, an unreadable directory).
var (
	readDir    = os.ReadDir
	removeFile = os.Remove
)

// isUsage reports whether a bundle file is the connector's _USAGE.md (any
// case — the TS toLowerCase() and Go's ToLower agree on every name that can
// spell it).
func isUsage(name string) bool { return strings.ToLower(name) == "_usage.md" }

// preamble renders the compact task-top section for all attached connectors.
// Deliberately thin: the workflow rules ride the system prompt (one copy,
// the harness's connector rules); what belongs here is what is specific to
// THESE services — their service maps — and the one instruction that makes
// the first turn a connector_lookup instead of a file read.
func preamble(conns []Connector, dirs []string) string {
	lines := []string{
		"[connected services] This task uses the external service(s) below — the PRIORITY path.",
		"Answer from the service API, not from chat history or the local machine.",
		"",
	}
	if len(conns) > 1 {
		lines = append(lines,
			"Several services are connected: pick the ONE whose domain owns the question and search",
			"only inside it — touch a second only when the question genuinely spans both.",
			"",
		)
	}
	for i, c := range conns {
		dir := dirs[i]
		var services []string
		for _, f := range c.Files {
			if isUsage(f.Name) {
				services = connectordocs.ServicesFromUsage(f.Content)
				break
			}
		}
		lines = append(lines, "## /"+c.Slug+" — "+c.Title, c.Description)
		if len(services) > 0 {
			lines = append(lines, "Services (route prefixes):")
			lines = append(lines, services[:min(len(services), 20)]...)
		}
		lines = append(lines,
			"Start with connector_lookup(connector: '"+c.Slug+"', query: '<words from the question>') — one call",
			"returns the matching endpoints, the chosen contract and its enum values. Then ONE complete",
			"connector_call. The invoker's identity on this service: "+dir+"/_identity.json (grep the one field",
			"you need). Full docs: "+dir+" — only for what lookup cannot answer.",
			"",
		)
	}
	lines = append(lines,
		"The connector rules in your system prompt apply: connector_call is the only way in, first call",
		"complete, counts from meta, never cat a saved response file.",
	)
	return strings.Join(lines, "\n")
}

// mcpCred is one EX_CONNECTORS entry — the fields and names the TS payload
// carries (authHeader absent when empty, as JSON.stringify drops undefined).
type mcpCred struct {
	Slug       string `json:"slug"`
	Title      string `json:"title"`
	BaseURL    string `json:"baseURL"`
	Token      string `json:"token"`
	AuthHeader string `json:"authHeader,omitempty"`
}

// Sync writes each connector's docs + generated _USAGE.md under rootDir and
// returns the MCP credential payload + priority instructions. Tokens never
// touch disk and never enter the harness shell.
//
// A file-system failure aborts the sync (the TS threw); Sync then logs it
// and returns EmptySetup — the run works without API access rather than

// slugRe is the server's connector slug rule (service.connectorSlugRe).
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// failing, as the TS run loop did. Use SyncChecked to see the error.
func Sync(rootDir string, conns []Connector, log protocol.Logger) Setup {
	setup, err := SyncChecked(rootDir, conns, log)
	if err != nil {
		if log != nil {
			log("connector sync failed; continuing without", map[string]any{"error": err.Error()})
		}
		return EmptySetup
	}
	return setup
}

// SyncChecked is Sync reporting a file-system failure instead of absorbing
// it — for callers that surface it (the MCP server's use_connector answers
// "attached, but doc sync failed: …"). On error the setup is EmptySetup;
// files already written stay.
func SyncChecked(rootDir string, conns []Connector, log protocol.Logger) (Setup, error) {
	if log == nil {
		log = protocol.NopLogger
	}
	var synced []Connector
	var dirs []string
	for _, c := range conns {
		// The slug becomes a directory name (and that directory gets pruned):
		// hold it to the server's own slug rule rather than trusting it.
		if !slugRe.MatchString(c.Slug) {
			log("connector skipped: invalid slug", map[string]any{"slug": c.Slug})
			continue
		}
		dir := filepath.Join(rootDir, c.Slug)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return EmptySetup, err
		}
		hasUsage := false
		for _, f := range c.Files {
			// Names are validated server-side (no slashes); belt-and-braces here.
			if f.Name == "" || strings.ContainsAny(f.Name, `/\`) || strings.Contains(f.Name, "..") {
				continue
			}
			if isUsage(f.Name) {
				hasUsage = true
			}
			if err := os.WriteFile(filepath.Join(dir, f.Name), []byte(f.Content), 0o600); err != nil {
				return EmptySetup, err
			}
		}
		// _USAGE.md comes FROM THE BACKEND — server-generated, or
		// admin-authored in the bundle — so instruction tuning reaches every
		// user without an app update. There used to be a second copy of that
		// whole document here as a fallback, which meant the same connector
		// could be explained two different ways depending on which side
		// produced the file. The server owns it; if it ever arrives without
		// one, say so rather than inventing it.
		if !hasUsage {
			log("connector bundle has no _USAGE.md", map[string]any{"slug": c.Slug})
		}
		// Mirror, don't just overlay: a doc removed from the registry must
		// also vanish from warm thread dirs, or agents keep grepping deleted
		// files forever. Only regular files directly in this connector's dir.
		keep := map[string]bool{"_USAGE.md": true}
		for _, f := range c.Files {
			keep[f.Name] = true
		}
		entries, err := readDir(dir)
		if err != nil {
			return EmptySetup, err
		}
		for _, e := range entries {
			if !e.Type().IsRegular() || keep[e.Name()] {
				continue
			}
			// Best-effort: a locked file just lingers until the next sync.
			if removeFile(filepath.Join(dir, e.Name())) == nil {
				log("connector file pruned", map[string]any{"slug": c.Slug, "file": e.Name()})
			}
		}
		synced = append(synced, c)
		dirs = append(dirs, dir)
		log("connector synced", map[string]any{"slug": c.Slug, "files": len(c.Files), "dir": dir})
	}

	if len(synced) == 0 {
		return EmptySetup, nil
	}
	creds := make([]mcpCred, len(synced))
	for i, c := range synced {
		creds[i] = mcpCred{Slug: c.Slug, Title: c.Title, BaseURL: c.BaseURL, Token: c.Token, AuthHeader: c.AuthHeader}
	}
	// JSON.stringify does not HTML-escape; neither does this. (Encoding
	// string fields cannot fail.)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(creds)
	return Setup{
		MCPConnectors: strings.TrimSuffix(buf.String(), "\n"),
		Instructions:  preamble(synced, dirs),
	}, nil
}
