package connectordocs

import (
	"github.com/DigitalTolk/ex-runners/internal/textutil"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var catalogTSV = strings.Join([]string{
	"meetings.upcoming\tGET upcoming-meetings\tread-only\tuser\tList the caller's next calendar events from Outlook.\tcalendar outlook today schedule",
	"meetings.show\tGET meetings/{meeting}\tread-only\tuser\tPoll one meeting for its processing status and summary text.\tstatus summary",
	"meetings_internal.logs\tGET meetings/{meeting}/logs\tread-only\tinternal\tRead a meeting's activity log.\tlog audit",
	"one_on_ones.index\tGET api/one-on-ones\tread-only\t\tList the 1:1 meetings visible to the current user.\t1:1 one-on-one",
}, "\n")

const meetingsYAML = `service: meetings
endpoints:
  - id: meetings.show
    method: GET
    path: "meetings/{meeting}"
    summary: Poll one meeting.

  # -------------------------------------------------------------------------
  - id: meetings.upcoming
    method: GET
    path: "upcoming-meetings"
    summary: List the caller's next calendar events from Outlook.
    filters:
      none: "Accepts no query parameters."

  # -------------------------------------------------------------------------
  - id: meetings.series_history
    method: GET
    path: "meetings/series-history"
`

const oneOnOnesYAML = `- id: one_on_ones.index
  method: GET
  path: api/one-on-ones
  params:
    - name: date
      type: enum
      values: enum:one_on_one_date_ranges
    - name: tab
      values: enum:one_on_one_list_tabs
- id: one_on_ones.show
  method: GET
`

const enumsYAML = `one_on_one_list_tabs:
  values:
  - mine
  - team
one_on_one_date_ranges:
  values:
  - all
  - last_7_days
  source: OneOnOneMeetingController.php:32
  notes: Applied to created_at, not meeting_date.
one_on_one_limits:
  values:
    per_page_max: 100
`

const usageMD = `# Using the MeetingMind API (connector: meetingmind)

Records meetings.

## Services — pick the OWNER first, search only inside it

- desktop (desktop.yaml) [routes: desktop.*] — Desktop release feed.
- meetings (meetings.yaml) [routes: meetings.*] — Meeting lifecycle for the desktop app.

Scope catalog greps by the [routes: …] prefixes.

## Workflow (in order)

1. Conventions: index.yml.
- not a service line
`

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fixtureDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "_catalog.tsv", catalogTSV)
	write(t, dir, "meetings.yaml", meetingsYAML)
	write(t, dir, "one-on-ones.yaml", oneOnOnesYAML)
	write(t, dir, "_enums.yaml", enumsYAML)
	write(t, dir, "index.yml", "schema: 1\n")
	return dir
}

func routeIDs(rows []CatalogRow) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, r.RouteID)
	}
	return out
}

func mustContain(t *testing.T, s string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("missing %q in:\n%s", w, s)
		}
	}
}

func mustNotContain(t *testing.T, s string, nots ...string) {
	t.Helper()
	for _, w := range nots {
		if strings.Contains(s, w) {
			t.Errorf("unexpected %q in:\n%s", w, s)
		}
	}
}

// --- connector-docs (ported TS cases) ---

func TestParsesSixColumnsAndSkipsBlankLines(t *testing.T) {
	rows := ParseCatalog(catalogTSV + "\n\n")
	if len(rows) != 4 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0].RouteID != "meetings.upcoming" || rows[0].MethodPath != "GET upcoming-meetings" || rows[0].Audience != "user" {
		t.Errorf("row0 = %+v", rows[0])
	}
	if rows[3].Audience != "" {
		t.Errorf("row3 audience = %q", rows[3].Audience)
	}
}

func TestRanksHitsHidesInternalAndHonoursServiceScope(t *testing.T) {
	rows := ParseCatalog(catalogTSV)
	hits := SearchCatalog(rows, "meetings today calendar", "")
	if routeIDs(hits)[0] != "meetings.upcoming" {
		t.Errorf("hits = %v", routeIDs(hits))
	}
	if slices.Contains(routeIDs(hits), "meetings_internal.logs") {
		t.Error("internal endpoint offered")
	}
	for _, svc := range []string{"one_on_ones", "one_on_ones.*"} {
		if got := routeIDs(SearchCatalog(rows, "meeting", svc)); !reflect.DeepEqual(got, []string{"one_on_ones.index"}) {
			t.Errorf("scope %q = %v", svc, got)
		}
	}
	if got := SearchCatalog(rows, "   ", ""); len(got) != 0 {
		t.Errorf("blank query = %v", got)
	}
	// Relevance cutoff: 'meetings today list' hits every meetings.* row on
	// "meetings"; only upcoming also carries "today" — the rest are dropped.
	ranked := routeIDs(SearchCatalog(rows, "meetings today list", ""))
	if ranked[0] != "meetings.upcoming" { // 3 words hit
		t.Errorf("ranked = %v", ranked)
	}
	if !slices.Contains(ranked, "one_on_ones.index") { // "meetings" + "List" — 2 words
		t.Errorf("ranked = %v", ranked)
	}
	if slices.Contains(ranked, "meetings.show") { // "meetings" only — cut
		t.Errorf("ranked = %v", ranked)
	}
	// With no multi-word hit anywhere, single-word matches all survive.
	if got := SearchCatalog(rows, "meetings", ""); len(got) <= 1 {
		t.Errorf("single word = %v", routeIDs(got))
	}
}

func TestExtractsIndentedBlockToNextSiblingDroppingSeparators(t *testing.T) {
	block := FindEndpointBlock(fixtureDir(t), "meetings.upcoming")
	if block == nil {
		t.Fatal("no block")
	}
	if block.File != "meetings.yaml" || block.Line != 9 || block.Truncated {
		t.Errorf("block = %+v", block)
	}
	mustContain(t, block.Text, `path: "upcoming-meetings"`, `none: "Accepts no query parameters."`)
	mustNotContain(t, block.Text, "series_history", "# ----")
}

func TestExtractsColumnZeroBlockStoppingAtNextTopLevelID(t *testing.T) {
	dir := fixtureDir(t)
	block := FindEndpointBlock(dir, "one_on_ones.index")
	if block == nil || block.File != "one-on-ones.yaml" {
		t.Fatalf("block = %+v", block)
	}
	mustContain(t, block.Text, "enum:one_on_one_date_ranges")
	mustNotContain(t, block.Text, "one_on_ones.show")
	if b := FindEndpointBlock(dir, "nope.missing"); b != nil {
		t.Errorf("missing = %+v", b)
	}
	if b := FindEndpointBlock(filepath.Join(dir, "does-not-exist"), "x"); b != nil {
		t.Errorf("no dir = %+v", b)
	}
}

func TestListsEnumRefsOnceInOrderAndPullsBlocks(t *testing.T) {
	block := FindEndpointBlock(fixtureDir(t), "one_on_ones.index")
	if got := EnumRefs(block.Text + "\nvalues: enum:one_on_one_date_ranges"); !reflect.DeepEqual(got, []string{"one_on_one_date_ranges", "one_on_one_list_tabs"}) {
		t.Errorf("refs = %v", got)
	}
	e, ok := EnumBlock(enumsYAML, "one_on_one_date_ranges")
	if !ok {
		t.Fatal("enum not found")
	}
	mustContain(t, e, "- last_7_days", "notes: Applied to created_at")
	mustNotContain(t, e, "one_on_one_limits")
	if s, ok := EnumBlock(enumsYAML, "missing"); ok || s != "" {
		t.Errorf("missing = %q %v", s, ok)
	}
}

func TestReadsOnlyServiceBulletsFromUsage(t *testing.T) {
	want := []string{
		"- desktop (desktop.yaml) [routes: desktop.*] — Desktop release feed.",
		"- meetings (meetings.yaml) [routes: meetings.*] — Meeting lifecycle for the desktop app.",
	}
	if got := ServicesFromUsage(usageMD); !reflect.DeepEqual(got, want) {
		t.Errorf("services = %q", got)
	}
	if got := ServicesFromUsage("no services here"); got == nil || len(got) != 0 {
		t.Errorf("none = %#v", got)
	}
}

func TestLookupOneHitReturnsRowsContractAndEnums(t *testing.T) {
	text, isErr := Lookup(LookupInput{Dir: fixtureDir(t), Slug: "cliffhub", Query: "one-on-one 1:1"})
	if isErr {
		t.Error("isError")
	}
	mustContain(t, text,
		"1 cliffhub endpoint(s) match",
		"--- contract: one_on_ones.index (one-on-ones.yaml:1) ---",
		"--- enums referenced",
		"- last_7_days",
		"Compose ONE complete connector_call",
	)
	// Golden: the TS implementation's exact output for this input.
	want := "1 cliffhub endpoint(s) match \"one-on-one 1:1\" (route_id | METHOD path | side_effects | summary):\none_on_ones.index | GET api/one-on-ones | read-only | List the 1:1 meetings visible to the current user.\n\n--- contract: one_on_ones.index (one-on-ones.yaml:1) ---\n- id: one_on_ones.index\n  method: GET\n  path: api/one-on-ones\n  params:\n    - name: date\n      type: enum\n      values: enum:one_on_one_date_ranges\n    - name: tab\n      values: enum:one_on_one_list_tabs\n\n--- enums referenced (the ONLY valid values) ---\none_on_one_date_ranges:\n  values:\n  - all\n  - last_7_days\n  source: OneOnOneMeetingController.php:32\n  notes: Applied to created_at, not meeting_date.\none_on_one_list_tabs:\n  values:\n  - mine\n  - team\n\nCompose ONE complete connector_call from this contract: every constraint in the question → a documented filter above."
	if text != want {
		t.Errorf("text =\n%q\nwant\n%q", text, want)
	}
}

func TestLookupSeveralHitsAskForRouteIDAndRouteAloneReturnsBlock(t *testing.T) {
	dir := fixtureDir(t)
	many, _ := Lookup(LookupInput{Dir: dir, Slug: "meetingmind", Query: "meeting"})
	mustContain(t, many, "endpoint(s) match", "call connector_lookup again with its route_id")
	mustNotContain(t, many, "--- contract")
	// Golden from TS (equal hits/bonus ordered by localeCompare).
	if want := "3 meetingmind endpoint(s) match \"meeting\" (route_id | METHOD path | side_effects | summary):\nmeetings.show | GET meetings/{meeting} | read-only | Poll one meeting for its processing status and summary text.\nmeetings.upcoming | GET upcoming-meetings | read-only | List the caller's next calendar events from Outlook.\none_on_ones.index | GET api/one-on-ones | read-only | List the 1:1 meetings visible to the current user.\nPick the one whose SCOPE matches the question and call connector_lookup again with its route_id for the contract."; many != want {
		t.Errorf("many = %q", many)
	}
	one, _ := Lookup(LookupInput{Dir: dir, Slug: "meetingmind", RouteID: "meetings.upcoming"})
	mustContain(t, one, "--- contract: meetings.upcoming (meetings.yaml:9) ---")
	mustNotContain(t, one, "--- enums")
	if want := "\n--- contract: meetings.upcoming (meetings.yaml:9) ---\n  - id: meetings.upcoming\n    method: GET\n    path: \"upcoming-meetings\"\n    summary: List the caller's next calendar events from Outlook.\n    filters:\n      none: \"Accepts no query parameters.\"\n\nCompose ONE complete connector_call from this contract: every constraint in the question → a documented filter above."; one != want {
		t.Errorf("one = %q", one)
	}
}

func TestLookupRefusesInternalReportsUnknownAndMissingInput(t *testing.T) {
	dir := fixtureDir(t)
	internal, isErr := Lookup(LookupInput{Dir: dir, Slug: "mm", RouteID: "meetings_internal.logs"})
	if isErr || internal != "meetings_internal.logs is audience: internal (machine-to-machine) — never call it; pick a user-facing endpoint." {
		t.Errorf("internal = %q %v", internal, isErr)
	}
	unknown, isErr := Lookup(LookupInput{Dir: dir, Slug: "mm", RouteID: "nope.missing"})
	if !isErr || unknown != `no contract block found for route_id "nope.missing" (not in the catalog either — check the spelling)` {
		t.Errorf("unknown = %q %v", unknown, isErr)
	}
	if text, isErr := Lookup(LookupInput{Dir: dir, Slug: "mm"}); !isErr || text != "connector_lookup needs query (words from the question) and/or route_id" {
		t.Errorf("empty = %q %v", text, isErr)
	}
	// Whitespace-only input is no input.
	if _, isErr := Lookup(LookupInput{Dir: dir, Slug: "mm", Query: " \t", RouteID: "\u00a0"}); !isErr {
		t.Error("blank input must be refused")
	}
	if text, isErr := Lookup(LookupInput{Dir: filepath.Join(dir, "nope"), Slug: "mm", Query: "x"}); !isErr || text != "no catalog found for mm — call use_connector first" {
		t.Errorf("no catalog = %q %v", text, isErr)
	}
	if text, isErr := Lookup(LookupInput{Dir: dir, Slug: "mm", Query: "zzzz", Service: "desktop"}); isErr || text != `no mm endpoints match "zzzz" in service desktop. Try fewer or different words, or drop the service scope.` {
		t.Errorf("scoped miss = %q %v", text, isErr)
	}
}

// --- connector-docs edges (ported TS cases) ---

func TestParseCatalogToleratesShortRows(t *testing.T) {
	rows := ParseCatalog("lonely\na.b\tGET /x\n")
	if len(rows) != 1 { // one-column line skipped
		t.Fatalf("rows = %+v", rows)
	}
	want := CatalogRow{RouteID: "a.b", MethodPath: "GET /x", Raw: "a.b\tGET /x"}
	if rows[0] != want {
		t.Errorf("row = %+v", rows[0])
	}
}

func TestFindEndpointBlockSkipsUnreadableYAMLAndStopsAtTopLevelKey(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "broken.yaml"), 0o755); err != nil { // a directory: reading fails → skipped
		t.Fatal(err)
	}
	write(t, dir, "svc.yaml", "eps:\n  - id: x.y\n    p: 1\n\ntop: v\n")
	block := FindEndpointBlock(dir, "x.y")
	if block == nil || block.Truncated {
		t.Fatalf("block = %+v", block)
	}
	mustContain(t, block.Text, "p: 1")
	mustNotContain(t, block.Text, "top: v")
	if block.Text != "  - id: x.y\n    p: 1" {
		t.Errorf("text = %q", block.Text)
	}
}

func TestCapsRunawayBlockAndSaysWhereTheRestLives(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "svc.yaml", "eps:\n  - id: big.one\n"+strings.Repeat("    k: v\n", 200))
	write(t, dir, "_catalog.tsv", "big.one\tGET /big\tread-only\tuser\tBig block\tbig\n")
	block := FindEndpointBlock(dir, "big.one")
	if block == nil || !block.Truncated {
		t.Fatalf("block = %+v", block)
	}
	if n := len(strings.Split(block.Text, "\n")); n != LookupMaxBlockLines {
		t.Errorf("lines = %d", n)
	}
	text, _ := Lookup(LookupInput{Dir: dir, Slug: "svc", RouteID: "big.one"})
	mustContain(t, text, "block truncated at 160 lines")
	mustContain(t, text, "… (block truncated at 160 lines; the rest is in "+filepath.Join(dir, "svc.yaml")+" from line 2)")
}

func TestEnumRefsCapsHowManyEnumsOneBlockPulls(t *testing.T) {
	var refs []string
	for i := range 12 {
		refs = append(refs, "enum:e"+strconv.Itoa(i))
	}
	if got := EnumRefs(strings.Join(refs, " ")); len(got) != LookupMaxEnums || got[7] != "e7" {
		t.Errorf("refs = %v", got)
	}
}

func TestLookupNamesFoundEnumsAndListsMissingOnes(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "_catalog.tsv", "p.q\tGET /pq\tread-only\tuser\tThing\tthing\n")
	write(t, dir, "svc.yaml", "- id: p.q\n  filter: enum:present enum:absent\n")
	write(t, dir, "_enums.yaml", "present:\n  - a\n  - b\n")
	text, _ := Lookup(LookupInput{Dir: dir, Slug: "svc", RouteID: "p.q"})
	mustContain(t, text, "--- enums referenced", "present:", "enum(s) not found in _enums.yaml: absent")
}

func TestLookupWithoutEnumsFileReportsEveryEnumMissing(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "_catalog.tsv", "p.q\tGET /pq\tread-only\tuser\tThing\tthing\n")
	write(t, dir, "svc.yaml", "- id: p.q\n  filter: enum:orphan\n")
	text, _ := Lookup(LookupInput{Dir: dir, Slug: "svc", RouteID: "p.q"})
	mustNotContain(t, text, "--- enums referenced")
	mustContain(t, text, "enum(s) not found in _enums.yaml: orphan")
	// An empty _enums.yaml finds nothing either.
	write(t, dir, "_enums.yaml", "")
	text, _ = Lookup(LookupInput{Dir: dir, Slug: "svc", RouteID: "p.q"})
	mustContain(t, text, "enum(s) not found in _enums.yaml: orphan")
}

func TestLookupReportsCatalogedRouteWithoutBlockNotAsError(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "_catalog.tsv", "ghost.route\tGET /g\tread-only\tuser\tGhost\tghost\n")
	write(t, dir, "svc.yaml", "- id: other.route\n  p: 1\n")
	text, isErr := Lookup(LookupInput{Dir: dir, Slug: "svc", RouteID: "ghost.route"})
	if isErr {
		t.Error("isError")
	}
	mustContain(t, text, `no contract block found for route_id "ghost.route"`)
	mustNotContain(t, text, "not in the catalog either")
}

func TestLookupUnscopedMissAndExplicitRouteSkipsPickPrompt(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "_catalog.tsv", "a.one\tGET /1\tread-only\tuser\tmeeting list\tmeeting\nb.two\tGET /2\tread-only\tuser\tmeeting board\tmeeting\n")
	write(t, dir, "svc.yaml", "- id: a.one\n  p: 1\n- id: b.two\n  p: 2\n")
	miss, _ := Lookup(LookupInput{Dir: dir, Slug: "svc", Query: "zzzz"})
	mustContain(t, miss, "Try fewer or different words.")
	mustNotContain(t, miss, "drop the service scope")
	if miss != `no svc endpoints match "zzzz". Try fewer or different words.` {
		t.Errorf("miss = %q", miss)
	}
	picked, _ := Lookup(LookupInput{Dir: dir, Slug: "svc", Query: "meeting", RouteID: "b.two"})
	mustContain(t, picked, "--- contract: b.two")
	mustNotContain(t, picked, "call connector_lookup again")
}

// --- Go-port specifics: golden behaviour captured from the TS ---

func TestLookupQueryPlusRouteAndScopedServiceGoldens(t *testing.T) {
	dir := fixtureDir(t)
	text, _ := Lookup(LookupInput{Dir: dir, Slug: "mm", Query: "meetings today list", RouteID: " meetings.show "})
	want := "2 mm endpoint(s) match \"meetings today list\" (route_id | METHOD path | side_effects | summary):\nmeetings.upcoming | GET upcoming-meetings | read-only | List the caller's next calendar events from Outlook.\none_on_ones.index | GET api/one-on-ones | read-only | List the 1:1 meetings visible to the current user.\n\n--- contract: meetings.show (meetings.yaml:3) ---\n  - id: meetings.show\n    method: GET\n    path: \"meetings/{meeting}\"\n    summary: Poll one meeting.\n\nCompose ONE complete connector_call from this contract: every constraint in the question → a documented filter above."
	if text != want {
		t.Errorf("query+route = %q", text)
	}
	// The service scope is trimmed, lowercased and stripped of ".*".
	scoped, _ := Lookup(LookupInput{Dir: dir, Slug: "mm", Query: "meeting", Service: " One_On_Ones.* "})
	mustContain(t, scoped, "1 mm endpoint(s) match \"meeting\"", "--- contract: one_on_ones.index (one-on-ones.yaml:1) ---")
}

func TestSearchCatalogTieBreakIsLocaleCompare(t *testing.T) {
	// Golden order from the TS: ICU puts "_" before "-" before "." and
	// lowercase before uppercase — not byte order.
	var lines []string
	for _, id := range []string{"b.x", "a_b.x", "a.b", "A.c", "a-b.x", "a.B", "Ab.x", "ab.x", "a1.x", "a10.x", "a9.x"} {
		lines = append(lines, id+"\tGET\t\tuser\tthing\t")
	}
	lines = append(lines, "\u0130nternal.x\tGET\t\t\u0130NTERNAL\tthing\t")
	got := routeIDs(SearchCatalog(ParseCatalog(strings.Join(lines, "\n")), "thing", ""))
	want := []string{"a_b.x", "a-b.x", "a.b", "a.B", "A.c", "a1.x", "a10.x", "a9.x", "ab.x", "Ab.x", "b.x", "\u0130nternal.x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %q", got)
	}
}

func TestSearchCatalogBonusAndCap(t *testing.T) {
	bon := "z.one\tGET /x\t\tuser\tother\tfoo bar\ny.foo\tGET /x\t\tuser\tother\tbar\nx.two\tGET /x\t\tuser\tfoo text\tbar"
	if got := routeIDs(SearchCatalog(ParseCatalog(bon), "foo bar", "")); !reflect.DeepEqual(got, []string{"x.two", "y.foo", "z.one"}) {
		t.Errorf("bonus order = %v", got)
	}
	var big []string
	for i := range 30 {
		big = append(big, "svc.r"+strconv.Itoa(i)+"\tGET /r"+strconv.Itoa(i)+"\t\tuser\tthing\t")
	}
	got := routeIDs(SearchCatalog(ParseCatalog(strings.Join(big, "\n")), "thing", ""))
	if len(got) != LookupMaxRows || got[0] != "svc.r0" || got[2] != "svc.r10" || got[19] != "svc.r26" {
		t.Errorf("capped = %v", got)
	}
}

func TestSearchCatalogServicePrefixShapes(t *testing.T) {
	rows := ParseCatalog("a.b.c\tGET\t\tuser\tthing\t\nab.c\tGET\t\tuser\tthing\t\n.x\tGET\t\tuser\tthing\t")
	cases := map[string][]string{
		"a.b.":  {"a.b.c"}, // one trailing "." dropped
		"A.B.*": {"a.b.c"}, // ".*" dropped, case folded
		"a":     {"a.b.c"},
		"   ":   {".x"}, // a blank (but given) scope is the bare "." prefix, as in the TS
		"":      {".x", "a.b.c", "ab.c"},
	}
	for svc, want := range cases {
		if got := routeIDs(SearchCatalog(rows, "thing", svc)); !reflect.DeepEqual(got, want) {
			t.Errorf("service %q = %v, want %v", svc, got, want)
		}
	}
}

func TestJavaScriptStringSemantics(t *testing.T) {
	// trim(): BOM and NBSP are whitespace, NEL is not (Go's TrimSpace is the
	// other way round).
	rows := ParseCatalog("\ufeffa.b\tGET /x \r\n\u00a0\t\u00a0\n\u0085\tx\n")
	want := []CatalogRow{
		{RouteID: "a.b", MethodPath: "GET /x", Raw: "\ufeffa.b\tGET /x \r"},
		{RouteID: "\u0085", MethodPath: "x", Raw: "\u0085\tx"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %+v", rows)
	}
	if textutil.JSTrim("\u2000\u200a x \u3000") != "x" || textutil.JSTrim("\u0085x") != "\u0085x" {
		t.Error("jsTrim")
	}
	// toLowerCase(): İ becomes "i" + U+0307, so "İNTERNAL" is not "internal".
	if jsLower("\u0130NTERNAL") != "i\u0307nternal" || jsLower("ABC") != "abc" {
		t.Errorf("jsLower = %q", jsLower("\u0130NTERNAL"))
	}
	rows = ParseCatalog("t.x\tGET\t\t\u0130NTERNAL\tthing\t\nu.x\tGET\t\tInternal\tthing\t")
	if got := routeIDs(SearchCatalog(rows, "thing", "")); !reflect.DeepEqual(got, []string{"t.x"}) {
		t.Errorf("audience = %v", got)
	}
}

func TestLocaleCompareMatchesNode(t *testing.T) {
	// Expected signs produced by Node's String.prototype.localeCompare.
	cases := []struct {
		a, b string
		want int
	}{
		{"meetings.show", "meetings_internal.logs", 1},
		{"aB", "Ab", -1},
		{"a", "A", -1},
		{"a\u0001b", "ab", 0},
		{"ab", "abc", -1},
		{"a-b", "a_b", 1},
		{"a.b", "a-b", 1},
		{"A", "b", -1},
		{"Zeta", "alpha", 1},
		{"x1", "x10", -1},
		{"x9", "x10", 1},
		{"a b", "ab", -1},
		{"\tA", "a", -1},
		{"a\u007f", "a", 0},
		{"one_on_ones.index", "one_on_ones_x.y", 1},
		{"Ab", "ab", 1},
		{"aBc", "abC", 1},
		{"e", "f", -1},
	}
	sign := func(n int) int {
		switch {
		case n < 0:
			return -1
		case n > 0:
			return 1
		}
		return 0
	}
	for _, c := range cases {
		if got := sign(localeCompare(c.a, c.b)); got != c.want {
			t.Errorf("localeCompare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	// Non-ASCII is approximated: after all ASCII, by code point.
	if localeCompare("\u00e9", "z") <= 0 || localeCompare("\u00e9", "\u00ea") >= 0 {
		t.Error("non-ASCII fallback")
	}
}

func TestFindEndpointBlockJSSemantics(t *testing.T) {
	// NBSP indentation counts as whitespace and is measured in characters;
	// CRLF lines keep their \r; .YML matches case-insensitively; _enums.yaml
	// and index.yml are never searched.
	dir := t.TempDir()
	write(t, dir, "b.YML", "eps:\n\u00a0\u00a0- id: \"q.r\"\r\n\u00a0\u00a0\u00a0\u00a0k: v\r\n\u00a0- id: s.t\n")
	write(t, dir, "a.yaml", "x: 1\n")
	write(t, dir, "_enums.yaml", "- id: q.r\n")
	write(t, dir, "index.yml", "- id: q.r\n")
	write(t, dir, "notes.txt", "- id: q.r\n")
	got := FindEndpointBlock(dir, "q.r")
	want := &EndpointBlock{File: "b.YML", Line: 2, Text: "\u00a0\u00a0- id: \"q.r\"\r\n\u00a0\u00a0\u00a0\u00a0k: v\r"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("block = %+v", got)
	}
	// Regex metacharacters in the route id are literal.
	if b := FindEndpointBlock(dir, "q.*"); b != nil {
		t.Errorf("metachar = %+v", b)
	}
	// A route id that is not valid UTF-8 matches nothing (and must not panic).
	if b := FindEndpointBlock(dir, "q\xffr"); b != nil {
		t.Errorf("invalid utf-8 = %+v", b)
	}
}

func TestFindEndpointBlockCharCapCountsCharacters(t *testing.T) {
	// 205-character lines of two-byte text: the 9000-char cap stops after 43
	// of them (a byte count would stop at about half that). Golden: 44 lines.
	dir := t.TempDir()
	write(t, dir, "s.yaml", "- id: big.c\n"+strings.Repeat("  k: "+strings.Repeat("\u00e9", 200)+"\n", 60))
	b := FindEndpointBlock(dir, "big.c")
	if b == nil || !b.Truncated || len(strings.Split(b.Text, "\n")) != 44 {
		t.Fatalf("block truncated=%v lines=%d", b != nil && b.Truncated, len(strings.Split(b.Text, "\n")))
	}
}

func TestFindEndpointBlockTrimsTrailingBlankLines(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "s.yaml", "- id: t.r\n  a: 1\n\n  \u00a0\n- id: t.s\n")
	if b := FindEndpointBlock(dir, "t.r"); b == nil || b.Text != "- id: t.r\n  a: 1" {
		t.Errorf("block = %+v", b)
	}
	// A marker alone keeps its one line.
	write(t, dir, "s.yaml", "- id: t.r\n- id: t.s\n")
	if b := FindEndpointBlock(dir, "t.r"); b == nil || b.Text != "- id: t.r" {
		t.Errorf("lone = %+v", b)
	}
}

func TestFindEndpointBlockFileOrderIsJSSort(t *testing.T) {
	// JS sorts by UTF-16 code units: an astral character (surrogates
	// D800–DFFF) sorts before U+FF5E, unlike byte/code-point order.
	dir := t.TempDir()
	write(t, dir, "\uff5e.yaml", "- id: dup.id\n  from: bmp\n")
	write(t, dir, "\U0001F600.yaml", "- id: dup.id\n  from: astral\n")
	if b := FindEndpointBlock(dir, "dup.id"); b == nil || b.File != "\U0001F600.yaml" {
		t.Errorf("block = %+v", b)
	}
}

func TestEnumBlockEdges(t *testing.T) {
	cases := []struct{ yaml, name, want string }{
		{"x:\n  - a\n\n\nnext:\n", "x", "x:\n  - a"},
		{"a.b: \t\n  - 1\n", "a.b", "a.b: \t\n  - 1"},
		{"a+b:\n  - 1\naab:\n  - 2\n", "a+b", "a+b:\n  - 1"},
	}
	for _, c := range cases {
		if got, ok := EnumBlock(c.yaml, c.name); !ok || got != c.want {
			t.Errorf("EnumBlock(%q) = %q %v", c.name, got, ok)
		}
	}
	got, _ := EnumBlock("x:\n"+strings.Repeat("  - v\n", 60), "x")
	if n := len(strings.Split(got, "\n")); n != LookupMaxEnumLines {
		t.Errorf("cap = %d lines", n)
	}
	if _, ok := EnumBlock("x:\n", "\xff"); ok {
		t.Error("invalid utf-8 name must not match")
	}
	if _, ok := EnumBlock("", "x"); ok {
		t.Error("empty yaml")
	}
}

func TestEnumRefsEdges(t *testing.T) {
	if got := EnumRefs("enum:a enum:b.c-d enum:a enum: enum:\u00e9"); !reflect.DeepEqual(got, []string{"a", "b.c-d"}) {
		t.Errorf("refs = %v", got)
	}
	if got := EnumRefs("nothing"); got == nil || len(got) != 0 {
		t.Errorf("none = %#v", got)
	}
}

func TestServicesFromUsageEdges(t *testing.T) {
	got := ServicesFromUsage("## services\n  -  one \n-x\n- two\n##Next\n- three\n## Other\n- four")
	if !reflect.DeepEqual(got, []string{"-  one", "- two", "- three"}) {
		t.Errorf("services = %q", got)
	}
	if got := ServicesFromUsage("##\u00a0SERVICES map\n- a\n##\tz\n- b"); !reflect.DeepEqual(got, []string{"- a"}) {
		t.Errorf("upper = %q", got)
	}
	// A non-unicode JS /i matches ASCII case variants only: long s (U+017F)
	// does not spell "services".
	if got := ServicesFromUsage("## \u017Fervices\n- a"); len(got) != 0 {
		t.Errorf("long s = %q", got)
	}
}

func TestReadOptional(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "f.txt", "hello")
	if s, ok := ReadOptional(filepath.Join(dir, "f.txt")); !ok || s != "hello" {
		t.Errorf("read = %q %v", s, ok)
	}
	if s, ok := ReadOptional(filepath.Join(dir, "missing")); ok || s != "" {
		t.Errorf("missing = %q %v", s, ok)
	}
	if _, ok := ReadOptional(dir); ok {
		t.Error("a directory is not a doc file")
	}
}
