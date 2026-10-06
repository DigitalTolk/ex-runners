// Package connectordocs is the connector doc lookup — the pure parsing
// behind the connector_lookup MCP tool.
//
// Before this existed an agent discovered an endpoint by hand: read
// _USAGE.md, grep _catalog.tsv, grep the service .yaml for `id: <route>`,
// Read ~60 lines at that offset, then grep _enums.yaml for each enum the
// block referenced. Five or six model turns (and five re-reads of the whole
// context) per connector before the first API call. connector_lookup does
// the same walk in-process and hands back one result: the matching catalog
// rows, the chosen endpoint's contract block, and the enum values it
// references.
package connectordocs

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/DigitalTolk/ex-runners/internal/textutil"
)

// CatalogRow is one line of a connector's _catalog.tsv.
type CatalogRow struct {
	RouteID     string `json:"routeId"`
	MethodPath  string `json:"methodPath"`
	SideEffects string `json:"sideEffects"`
	Audience    string `json:"audience"`
	Summary     string `json:"summary"`
	Keywords    string `json:"keywords"`
	Raw         string `json:"raw"`
}

// Caps keep one lookup result well inside what a single turn should carry:
// the point is to REPLACE several reads, not to dump a service file whole.
const (
	LookupMaxRows       = 20
	LookupMaxBlockLines = 160
	LookupMaxBlockChars = 9000
	LookupMaxEnums      = 8
	LookupMaxEnumLines  = 40
)

// The TS regexes use JavaScript's \s / \S, which cover Unicode spaces, \v
// and the BOM; Go's are ASCII-only (and lack \v), so the class is spelled
// out. Patterns below are written with ws/nws where the TS has \s/\S.

const (
	ws  = `[` + textutil.JSSpaceChars + `]`
	nws = `[^` + textutil.JSSpaceChars + `]`
)

var (
	queryWordSep = regexp.MustCompile(`[^a-z0-9_:/-]+`)
	yamlFile     = regexp.MustCompile(`\.[Yy][Aa]?[Mm][Ll]$`) // TS /\.ya?ml$/i
	siblingID    = regexp.MustCompile(`^(` + ws + `*)-` + ws + `*id:` + ws + `*` + nws)
	topLevel     = regexp.MustCompile(`^` + nws)
	separator    = regexp.MustCompile(`^` + ws + `*#` + ws + `*-{5,}` + ws + `*$`)
	enumRef      = regexp.MustCompile(`enum:([A-Za-z0-9_.-]+)`)
	// TS /^##\s+Services/i. A non-unicode JS /i matches only ASCII case
	// variants here, which Go's (?i) would widen (Unicode folding), so the
	// letters are spelled out.
	servicesHeading = regexp.MustCompile(`^##` + ws + `+[Ss][Ee][Rr][Vv][Ii][Cc][Ee][Ss]`)
	h2              = regexp.MustCompile(`^##` + ws)
	bullet          = regexp.MustCompile(`^` + ws + `*-` + ws + `+` + nws)
)

// jsLower is String.prototype.toLowerCase as far as ASCII matching can
// tell: Go maps U+0130 (İ) to a bare "i", JS to "i" + U+0307.
func jsLower(s string) string { return strings.ToLower(strings.ReplaceAll(s, "\u0130", "i\u0307")) }

// ParseCatalog reads the provider-generated _catalog.tsv. Columns: route_id,
// "METHOD path", side_effects, audience, summary, keywords — the same six
// the _USAGE.md workflow told the agent to cut with `cut -f1,2,5`.
func ParseCatalog(tsv string) []CatalogRow {
	var rows []CatalogRow
	for _, line := range strings.Split(tsv, "\n") {
		if textutil.JSTrim(line) == "" {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) < 2 {
			continue
		}
		col := func(i int) string {
			if i < len(cols) {
				return textutil.JSTrim(cols[i])
			}
			return ""
		}
		rows = append(rows, CatalogRow{
			RouteID:     col(0),
			MethodPath:  col(1),
			SideEffects: col(2),
			Audience:    col(3),
			Summary:     col(4),
			Keywords:    col(5),
			Raw:         line,
		})
	}
	return rows
}

// SearchCatalog ranks rows by how many query words hit the whole line (the
// same visibility a `grep -i` had), optionally scoped to a route prefix
// ("one_on_ones" matches one_on_ones.*). audience: internal endpoints are
// machine-to-machine and never offered — the old doc said so in prose, this
// enforces it. An empty service means no scope.
func SearchCatalog(rows []CatalogRow, query, service string) []CatalogRow {
	var words []string
	for _, w := range queryWordSep.Split(jsLower(query), -1) {
		// Words are ASCII-only (the separator class takes everything else),
		// so byte length is character length.
		if len(w) >= 2 {
			words = append(words, w)
		}
	}
	if len(words) == 0 {
		return []CatalogRow{}
	}
	prefix := ""
	if service != "" {
		// TS .replace(/\.\*?$/, ''): drop one trailing "." or ".*".
		s := jsLower(textutil.JSTrim(service))
		if strings.HasSuffix(s, ".*") {
			s = s[:len(s)-2]
		} else {
			s = strings.TrimSuffix(s, ".")
		}
		prefix = s + "."
	}
	type scoredRow struct {
		row         CatalogRow
		hits, bonus int
	}
	var scored []scoredRow
	for _, row := range rows {
		if jsLower(row.Audience) == "internal" {
			continue
		}
		routeID := jsLower(row.RouteID)
		if prefix != "" && !strings.HasPrefix(routeID, prefix) {
			continue
		}
		hay := jsLower(row.Raw)
		summary := jsLower(row.Summary)
		hits := 0  // distinct query words present anywhere on the line
		bonus := 0 // of those, how many land in the route id or summary
		for _, w := range words {
			if !strings.Contains(hay, w) {
				continue
			}
			hits++
			if strings.Contains(routeID, w) || strings.Contains(summary, w) {
				bonus++
			}
		}
		if hits > 0 {
			scored = append(scored, scoredRow{row, hits, bonus})
		}
	}
	// Array.prototype.sort is stable; so is this.
	sort.SliceStable(scored, func(i, j int) bool {
		a, b := scored[i], scored[j]
		if a.hits != b.hits {
			return a.hits > b.hits
		}
		if a.bonus != b.bonus {
			return a.bonus > b.bonus
		}
		return localeCompare(a.row.RouteID, b.row.RouteID) < 0
	})
	// Relevance cutoff: when some rows match two or more of the words, rows
	// matching just one are noise — "meetings today list" hit 18 rows in a
	// meetings-heavy catalog before this, all but one on the word "meetings".
	out := []CatalogRow{}
	for _, s := range scored {
		if len(out) == LookupMaxRows {
			break
		}
		if scored[0].hits >= 2 && s.hits < 2 {
			continue
		}
		out = append(out, s.row)
	}
	return out
}

// asciiPrimary holds the primary collation weight of each ASCII character
// under the CLDR root collation that Node's String.prototype.localeCompare
// uses (ICU, default options): whitespace, then punctuation and symbols in
// CLDR order (note "_" < "-" < "." — not ASCII order), digits, then letters
// with case ignored. 0 = completely ignorable (controls, DEL). The order was
// read off Node's own sort of all 128 characters.
var asciiPrimary = func() (w [128]uint16) {
	const order = "\t\n\v\f\r _-,;:!?.'\"()[]{}@*/\\&#%`^+<=>|~$0123456789"
	for i := 0; i < len(order); i++ {
		w[order[i]] = uint16(i + 1)
	}
	for i := range 26 {
		w['a'+i] = uint16(len(order) + 1 + i)
		w['A'+i] = uint16(len(order) + 1 + i)
	}
	return w
}()

// localeCompare orders two strings the way JS a.localeCompare(b) does for
// ASCII text: primary weights first (shorter prefix first), then case as a
// tertiary difference (lowercase before uppercase); 0 when they differ only
// in ignorable characters. Non-ASCII characters are approximated — they sort
// after every ASCII character, by code point, with no accent folding; route
// ids are ASCII in practice.
func localeCompare(a, b string) int {
	pa, ta := collationKey(a)
	pb, tb := collationKey(b)
	if c := slices.Compare(pa, pb); c != 0 {
		return c
	}
	return slices.Compare(ta, tb)
}

func collationKey(s string) (primary []uint32, tertiary []uint8) {
	for _, r := range s {
		if r >= 128 {
			primary = append(primary, 0x100+uint32(r))
			tertiary = append(tertiary, 0)
			continue
		}
		w := asciiPrimary[r]
		if w == 0 {
			continue
		}
		primary = append(primary, uint32(w))
		var t uint8
		if r >= 'A' && r <= 'Z' {
			t = 1
		}
		tertiary = append(tertiary, t)
	}
	return primary, tertiary
}

// EndpointBlock is one endpoint's contract block in a service YAML.
type EndpointBlock struct {
	File      string `json:"file"`
	Line      int    `json:"line"` // 1-based line of the `- id:` marker
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

// ReadOptional reads a doc file that may legitimately be absent (a KB with
// no enums, a connector attached before its sync finished) — false, not an
// error.
func ReadOptional(file string) (string, bool) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// jsSortStrings sorts like JS Array.prototype.sort() with no comparator: by
// UTF-16 code units (differs from byte order only for astral characters).
func jsSortStrings(s []string) {
	sort.SliceStable(s, func(i, j int) bool {
		return slices.Compare(utf16.Encode([]rune(s[i])), utf16.Encode([]rune(s[j]))) < 0
	})
}

// FindEndpointBlock locates `- id: <routeId>` in the service YAMLs and
// returns that endpoint's block: from the marker down to the next sibling
// `- id:` (same or shallower indent), a top-level key, or the cap. Comment
// separator lines between endpoints are dropped.
func FindEndpointBlock(dir, routeID string) *EndpointBlock {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range entries {
		if f := e.Name(); yamlFile.MatchString(f) && f != "_enums.yaml" && f != "index.yml" {
			files = append(files, f)
		}
	}
	jsSortStrings(files)
	// Compile can only fail on a route id that is not valid UTF-8 (one read
	// raw from a catalog file); such an id matches nothing.
	marker, err := regexp.Compile(`^(` + ws + `*)-` + ws + `*id:` + ws + `*['"]?` + regexp.QuoteMeta(routeID) + `['"]?` + ws + `*$`)
	if err != nil {
		return nil
	}
	for _, f := range files {
		content, ok := ReadOptional(filepath.Join(dir, f))
		if !ok {
			continue
		}
		lines := strings.Split(content, "\n")
		for i, line := range lines {
			m := marker.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			indent := textutil.Len(m[1])
			out := []string{line}
			truncated := false
			chars := textutil.Len(line)
			for _, l := range lines[i+1:] {
				if sib := siblingID.FindStringSubmatch(l); sib != nil && textutil.Len(sib[1]) <= indent {
					break
				}
				if indent > 0 && topLevel.MatchString(l) && !strings.HasPrefix(l, "#") {
					break // next top-level key
				}
				if separator.MatchString(l) {
					continue // "# -----" separators
				}
				n := textutil.Len(l)
				if len(out) >= LookupMaxBlockLines || chars+n > LookupMaxBlockChars {
					truncated = true
					break
				}
				out = append(out, l)
				chars += n + 1
			}
			return &EndpointBlock{File: f, Line: i + 1, Text: strings.Join(trimTrailingBlank(out), "\n"), Truncated: truncated}
		}
	}
	return nil
}

// trimTrailingBlank drops trailing blank lines, keeping at least one line.
func trimTrailingBlank(out []string) []string {
	for len(out) > 1 && textutil.JSTrim(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return out
}

// EnumRefs lists the `enum:<name>` references a block carries, in order of
// first appearance, deduplicated and capped.
func EnumRefs(block string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, m := range enumRef.FindAllStringSubmatch(block, -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
		if len(out) >= LookupMaxEnums {
			break
		}
	}
	return out
}

// EnumBlock extracts one top-level `<name>:` mapping from _enums.yaml text —
// its values plus the notes the KB author left (which value means "this
// week", which is applied to created_at…). Capped per enum.
func EnumBlock(enumsYAML, name string) (string, bool) {
	// Compile can only fail on a name that is not valid UTF-8: no match.
	start, err := regexp.Compile(`^` + regexp.QuoteMeta(name) + `:` + ws + `*$`)
	if err != nil {
		return "", false
	}
	lines := strings.Split(enumsYAML, "\n")
	for i, line := range lines {
		if !start.MatchString(line) {
			continue
		}
		out := []string{line}
		for j := i + 1; j < len(lines) && len(out) < LookupMaxEnumLines; j++ {
			if topLevel.MatchString(lines[j]) {
				break
			}
			out = append(out, lines[j])
		}
		return strings.Join(trimTrailingBlank(out), "\n"), true
	}
	return "", false
}

// ServicesFromUsage pulls the "## Services" bullet list out of a
// server-generated _USAGE.md — the one connector-specific part of that doc.
// The rest of it is workflow boilerplate that now lives in the system rules,
// so the attach result can carry this list inline instead of sending the
// agent off to read the file.
func ServicesFromUsage(usage string) []string {
	out := []string{}
	inServices := false
	for _, l := range strings.Split(usage, "\n") {
		if servicesHeading.MatchString(l) {
			inServices = true
			continue
		}
		if inServices && h2.MatchString(l) {
			break
		}
		if inServices && bullet.MatchString(l) {
			out = append(out, textutil.JSTrim(l))
		}
	}
	return out
}

// LookupInput is one connector_lookup request. Empty Query, RouteID or
// Service mean "not given".
type LookupInput struct {
	Dir     string // the connector's synced docs folder
	Slug    string
	Query   string
	RouteID string
	Service string
}

// Lookup renders the connector_lookup result: matching rows for a query,
// the contract block (plus referenced enums) for a route id — or for a query
// that matches exactly one endpoint, both at once.
func Lookup(in LookupInput) (text string, isError bool) {
	dir, slug := in.Dir, in.Slug
	query := textutil.JSTrim(in.Query)
	routeID := textutil.JSTrim(in.RouteID)
	if query == "" && routeID == "" {
		return "connector_lookup needs query (words from the question) and/or route_id", true
	}
	catalog, ok := ReadOptional(filepath.Join(dir, "_catalog.tsv"))
	if !ok {
		return "no catalog found for " + slug + " — call use_connector first", true
	}
	rows := ParseCatalog(catalog)
	var parts []string

	if query != "" {
		hits := SearchCatalog(rows, query, in.Service)
		if len(hits) == 0 {
			scope, drop := "", ""
			if in.Service != "" {
				scope = " in service " + in.Service
				drop = ", or drop the service scope"
			}
			parts = append(parts, "no "+slug+` endpoints match "`+query+`"`+scope+". Try fewer or different words"+drop+".")
		} else {
			parts = append(parts, strconv.Itoa(len(hits))+" "+slug+` endpoint(s) match "`+query+`" (route_id | METHOD path | side_effects | summary):`)
			for _, h := range hits {
				parts = append(parts, h.RouteID+" | "+h.MethodPath+" | "+h.SideEffects+" | "+h.Summary)
			}
			if routeID == "" && len(hits) == 1 {
				routeID = hits[0].RouteID
			} else if routeID == "" {
				parts = append(parts, "Pick the one whose SCOPE matches the question and call connector_lookup again with its route_id for the contract.")
			}
		}
	}

	if routeID != "" {
		var row *CatalogRow
		for i := range rows {
			if rows[i].RouteID == routeID {
				row = &rows[i]
				break
			}
		}
		if row != nil && jsLower(row.Audience) == "internal" {
			parts = append(parts, routeID+" is audience: internal (machine-to-machine) — never call it; pick a user-facing endpoint.")
			return strings.Join(parts, "\n"), false
		}
		block := FindEndpointBlock(dir, routeID)
		if block == nil {
			hint := ""
			if row == nil {
				hint = " (not in the catalog either — check the spelling)"
			}
			parts = append(parts, `no contract block found for route_id "`+routeID+`"`+hint)
			return strings.Join(parts, "\n"), row == nil
		}
		parts = append(parts, "", "--- contract: "+routeID+" ("+block.File+":"+strconv.Itoa(block.Line)+") ---", block.Text)
		if block.Truncated {
			parts = append(parts, "… (block truncated at "+strconv.Itoa(LookupMaxBlockLines)+" lines; the rest is in "+filepath.Join(dir, block.File)+" from line "+strconv.Itoa(block.Line)+")")
		}
		if refs := EnumRefs(block.Text); len(refs) > 0 {
			enums, _ := ReadOptional(filepath.Join(dir, "_enums.yaml"))
			var found, missing []string
			for _, name := range refs {
				// TS: `enums ? enumBlock(enums, name) : null` — an empty or
				// absent file finds nothing, which EnumBlock("") also does.
				if b, ok := EnumBlock(enums, name); ok {
					found = append(found, b)
				} else {
					missing = append(missing, name)
				}
			}
			if len(found) > 0 {
				parts = append(parts, "", "--- enums referenced (the ONLY valid values) ---")
				parts = append(parts, found...)
			}
			if len(missing) > 0 {
				parts = append(parts, "(enum(s) not found in _enums.yaml: "+strings.Join(missing, ", ")+")")
			}
		}
		parts = append(parts, "", "Compose ONE complete connector_call from this contract: every constraint in the question → a documented filter above.")
	}
	return strings.Join(parts, "\n"), false
}
