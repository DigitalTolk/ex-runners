// Package toolindex is the tool index: a light tool list for harnesses that
// load every MCP tool definition up front (codex). Claude Code already defers
// MCP tools behind ToolSearch; codex re-sends all 43 Ex definitions (~6.6k
// tokens) on every call although runs use a handful. The index advertises
// the tools runs actually use in full, and reaches the rest through two
// meta-tools: one returns a tool's full contract, the other calls it.
package toolindex

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/DigitalTolk/ex-runners/internal/textutil"
)

// ToolDef is one MCP tool definition. InputSchema holds the schema's RAW
// bytes: the MCP server serves schemas verbatim, preserving key order.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// coreTools stay fully advertised: together they were ~90% of the Ex tool
// calls across 77 recorded runs (2026-09-30), and the connector trio drives
// the flow agents are most often asked for.
var coreTools = map[string]bool{
	"post_message":     true,
	"get_thread":       true,
	"get_context":      true,
	"set_state":        true,
	"connector_lookup": true,
	"connector_call":   true,
	"use_connector":    true,
	"fetch_spill":      true,
	"request_approval": true,
}

const (
	ToolInfo = "ex_tool_info"
	ToolCall = "ex_tool_call"
)

// IsCore reports whether a tool stays fully advertised in index mode.
func IsCore(name string) bool { return coreTools[name] }

// jsSpace is the character class JavaScript's \s (and String.trim) match.
// Go's \s is ASCII-only and lacks \v, so the class is spelled out.
const jsSpace = `\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

var (
	jsSpaceRun = regexp.MustCompile(`[` + jsSpace + `]+`)
	// After jsSpaceRun has run, a plain space is the only whitespace left, so
	// this is the TS /[.!?](\s|$)/.
	sentenceEnd = regexp.MustCompile(`[.!?]( |$)`)
)

// Summary is a tool's first sentence, capped — enough to pick it by, not to
// call it with.
func Summary(description string) string {
	flat := strings.Trim(jsSpaceRun.ReplaceAllString(description, " "), " ")
	first := flat
	if loc := sentenceEnd.FindStringIndex(flat); loc != nil {
		first = flat[:loc[0]+1]
	}
	if textutil.Len(first) > 110 {
		return textutil.Head(first, 107) + "…"
	}
	return first
}

// Signature renders a tool's arguments compactly — "set_reminder(in_minutes?,
// remind_at?)" — so the catalog says what a tool takes without its schema.
// Properties are listed in schema SOURCE order.
func Signature(t ToolDef) string {
	s := parseSchema(t.InputSchema)
	required := make(map[string]bool, len(s.required))
	for _, k := range s.required {
		required[k] = true
	}
	args := make([]string, len(s.props))
	for i, p := range s.props {
		if required[p] {
			args[i] = p
		} else {
			args[i] = p + "?"
		}
	}
	return t.Name + "(" + strings.Join(args, ", ") + ")"
}

// The meta-tools' schemas, byte-for-byte what JSON.stringify produces for the
// TS object literals (key order as written there).
const (
	infoSchema = `{"type":"object","properties":{"names":{"type":"array","items":{"type":"string"},"description":"Tool names from the list."}},"required":["names"],"additionalProperties":false}`
	callSchema = `{"type":"object","properties":{"tool":{"type":"string","description":"Tool name from the ex_tool_info list."},"arguments":{"type":"object","description":"The tool's arguments, per its contract."}},"required":["tool"],"additionalProperties":false}`
)

// IndexedTools is the advertised set in index mode: core tools in full plus
// the two meta-tools. The catalog rides ex_tool_info — the entry point an
// agent looks at when the tools above don't cover the task; buried in the
// call tool's description, codex skimmed past it and told people it
// "can't access your channel list".
func IndexedTools(visible []ToolDef) []ToolDef {
	// Never nil: tools/list must marshal as [] even when nothing is visible.
	core := make([]ToolDef, 0, len(visible)+2)
	var catalog []string
	for _, t := range visible {
		if IsCore(t.Name) {
			core = append(core, t)
		} else {
			catalog = append(catalog, "- "+Signature(t)+": "+Summary(t.Description))
		}
	}
	if len(catalog) == 0 {
		return core
	}
	return append(core,
		ToolDef{
			Name: ToolInfo,
			Description: "MORE EX TOOLS — channels, DMs, people, reminders, reactions, pins, scheduled orders, search, " +
				"skills and more are listed below. Check this list before telling anyone you can't do something. " +
				"Pass the names you need to get each one's full contract (when to use it, exact arguments), then run " +
				"it with ex_tool_call.\n" + strings.Join(catalog, "\n"),
			InputSchema: json.RawMessage(infoSchema),
		},
		ToolDef{
			Name: ToolCall,
			Description: "Run a tool from the ex_tool_info list, AFTER reading its contract with ex_tool_info. A tool whose " +
				"contract you have not read, or a call missing required arguments, is not run — you get the contract back.",
			InputSchema: json.RawMessage(callSchema),
		},
	)
}

// Info renders the full contracts for the requested names and records them
// in described (when non-nil), which is what lets ex_tool_call run them.
// names is the raw decoded JSON argument: only an array's string elements
// count ([]string is accepted too, for Go callers).
func Info(visible []ToolDef, names any, described map[string]bool) string {
	var wanted []string
	switch v := names.(type) {
	case []any:
		for _, n := range v {
			if s, ok := n.(string); ok {
				wanted = append(wanted, s)
			}
		}
	case []string:
		wanted = v
	}
	if len(wanted) == 0 {
		return "ex_tool_info requires names"
	}
	parts := make([]string, len(wanted))
	for i, name := range wanted {
		t, ok := find(visible, name)
		if !ok {
			parts[i] = "## " + name + "\n(no such tool available to this run)"
			continue
		}
		if described != nil {
			described[t.Name] = true
		}
		parts[i] = "## " + t.Name + "\n" + t.Description + "\nArguments (JSON schema): " + schemaJSON(t.InputSchema)
	}
	return strings.Join(parts, "\n\n")
}

// Unwrapped is UnwrapCall's verdict; exactly one kind is set: Name (+Args)
// to run the tool, Contract to hand back instead, or Error.
type Unwrapped struct {
	Name     string
	Args     map[string]any
	Contract string
	Error    string
}

// UnwrapCall resolves an ex_tool_call into the real tool call. It runs only a
// tool the agent has read the contract of, with its required arguments
// present; otherwise it hands the contract back instead of a failed call —
// guessing arguments ("set_reminder {}") was the commonest waste in index mode.
func UnwrapCall(visible []ToolDef, args map[string]any, described map[string]bool) Unwrapped {
	// `name` is accepted too: having just read "## <name>" contracts, codex
	// reached for {"name": …} and burned a call on the error.
	raw := args["tool"]
	if _, ok := raw.(string); !ok {
		raw = args["name"]
	}
	name, _ := raw.(string)
	if name == "" {
		return Unwrapped{Error: "ex_tool_call requires tool"}
	}
	t, ok := find(visible, name)
	if !ok || name == ToolCall || name == ToolInfo {
		return Unwrapped{Error: "no such tool available to this run: " + name}
	}
	callArgs, _ := args["arguments"].(map[string]any)
	if callArgs == nil {
		callArgs = map[string]any{}
	}
	if !described[name] {
		return Unwrapped{Contract: "Not run: read " + name + "'s contract first, then call it again.\n\n" + Info(visible, []string{name}, described)}
	}
	var missing []string
	for _, k := range parseSchema(t.InputSchema).required {
		// Present-but-null counts as present (JS `=== undefined`).
		if _, ok := callArgs[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return Unwrapped{Contract: "Not run: missing required arguments " + strings.Join(missing, ", ") + ".\n\n" + Info(visible, []string{name}, nil)}
	}
	return Unwrapped{Name: name, Args: callArgs}
}

func find(visible []ToolDef, name string) (ToolDef, bool) {
	for _, d := range visible {
		if d.Name == name {
			return d, true
		}
	}
	return ToolDef{}, false
}

// schemaJSON renders a schema the way JSON.stringify prints the TS object:
// compact. The raw bytes are otherwise kept as served (key order, escapes).
// An absent schema reads as {} (no arguments); bytes that are not JSON at
// all are shown verbatim rather than dropped.
func schemaJSON(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

type schemaFields struct {
	props    []string // property names, source order, first occurrence kept
	required []string // string elements of `required`, in order
}

// parseSchema reads the two top-level schema fields the index needs, walking
// tokens so property names keep their SOURCE order (a Go map would sort
// them). As with JSON.parse, a repeated top-level key's last value wins and a
// repeated property name keeps its first position. Anything that is not a
// well-formed object yields no fields; a non-object `properties` or
// non-array `required` reads as absent.
func parseSchema(raw json.RawMessage) schemaFields {
	var out schemaFields
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return schemaFields{}
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return schemaFields{}
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return schemaFields{}
		}
		switch keyTok.(string) {
		case "properties":
			out.props = objectKeys(value)
		case "required":
			out.required = stringElements(value)
		}
	}
	if _, err := dec.Token(); err != nil {
		return schemaFields{}
	}
	return out
}

// objectKeys lists a JSON object's keys in source order, deduplicated; nil
// for anything but an object. value is already a complete, valid JSON value.
func objectKeys(value json.RawMessage) []string {
	dec := json.NewDecoder(bytes.NewReader(value))
	if tok, _ := dec.Token(); tok != json.Delim('{') {
		return nil
	}
	var keys []string
	seen := map[string]bool{}
	for dec.More() {
		keyTok, _ := dec.Token()
		var skip json.RawMessage
		_ = dec.Decode(&skip)
		k := keyTok.(string)
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	return keys
}

// stringElements returns the string elements of a JSON array, in order; nil
// for anything but an array.
func stringElements(value json.RawMessage) []string {
	var arr []any
	if json.Unmarshal(value, &arr) != nil {
		return nil
	}
	var out []string
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
