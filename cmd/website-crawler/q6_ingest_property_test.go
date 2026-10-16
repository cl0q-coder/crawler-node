package main

// Track Q rung Q6 (#283): ingest/upsert property tests — tests-only, no
// production code changes (the ingest path is Claude Code's trust lane).
//
// Three targets, mirroring the Q4/Q5 structure:
//
// (a) sorted-upsert dedup — the batch path in flushBatchTx: the 34-column
//     multi-row INSERT (upsertHead), the ON CONFLICT refresh (upsertTail),
//     the per-row arg list, and the sorted-emit determinism (sortedKeys).
// (b) SimHash fallback path (open PR #193) — the row-by-row fallback prepared
//     upsert in main() + flushRowByRow: the #193 invariant (every
//     EXCLUDED.* referenced in ON CONFLICT DO UPDATE SET must be a real
//     INSERT column, else Postgres NULLs it), placeholder/arg-count parity,
//     and simhash.Blocks round-trip properties.
// (c) conditional-GET 304 verdict path — touchStmt updates ONLY crawled_at
//     (304 must never rewrite content/simhash columns), he4ChangeObserved
//     304 verdicts, and the λ-aware 304 backoff math (never shrinks, 30d cap).
//
// Method: the batch SQL lives in package-level consts (upsertHead/upsertTail)
// and is asserted directly. The fallback upsert + touchStmt SQL live inline
// in main()'s db.Prepare calls, so these tests extract them with go/ast from
// main.go and assert the same invariants on the real source text — if the SQL
// is edited, the extraction or the property fails loudly instead of silently
// testing a stale copy.
//
// Q6 FINDING (reported on #283, not fixed — ingest fixes hold for Claude):
// the BATCH upsertTail does NOT refresh simhash/simhash_b0-b3/duplicate_of/
// sitemap_urls/sitemap_lastmod on conflict, while the row-by-row FALLBACK
// prepared upsert DOES. So a re-crawl flushed through the batch path keeps
// its ORIGINAL first-crawl fingerprint (and duplicate_of) forever, and the
// HE-4 "upsert overwrites the stored fingerprints" comments (flushBatchTx,
// he4FetchVerdicts) are wrong for the batch path. TestQ6BatchUpsertTailGaps
// pins the current gap set; if the gap set changes in either direction the
// test fails and the allowlist must be revisited.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"cl0q/internal/simhash"
)

// ---------------------------------------------------------------------------
// SQL-string property helpers (pure).
// ---------------------------------------------------------------------------

// q6InsertColumns parses the column list of "INSERT INTO website_info (<cols>)".
func q6InsertColumns(t *testing.T, sql string) []string {
	t.Helper()
	up := strings.ToUpper(sql)
	idx := strings.Index(up, "INSERT INTO WEBSITE_INFO")
	if idx < 0 {
		t.Fatalf("no INSERT INTO website_info found in SQL (%.80s...)", sql)
	}
	open := strings.Index(sql[idx:], "(")
	if open < 0 {
		t.Fatal("no column list paren in INSERT")
	}
	open += idx
	depth := 0
	for i := open; i < len(sql); i++ {
		switch sql[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				raw := sql[open+1 : i]
				var cols []string
				for _, c := range strings.Split(raw, ",") {
					c = strings.TrimSpace(strings.ToLower(c))
					if c != "" {
						cols = append(cols, c)
					}
				}
				return cols
			}
		}
	}
	t.Fatal("unbalanced parens in INSERT column list")
	return nil
}

var q6ExcludedRe = regexp.MustCompile(`EXCLUDED\.([a-z_][a-z_0-9]*)`)

// q6ExcludedRefs returns every EXCLUDED.<col> referenced in the SQL.
func q6ExcludedRefs(sql string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range q6ExcludedRe.FindAllStringSubmatch(strings.ToLower(sql), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// q6StripSQLComments removes -- line comments (upsertTail carries some).
func q6StripSQLComments(sql string) string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// q6SetAssignedCols returns the LHS columns assigned by a SET clause —
// either "ON CONFLICT ... DO UPDATE SET" (upserts) or a plain "UPDATE SET"
// (the 304 touch statement).
func q6SetAssignedCols(t *testing.T, sql string) []string {
	t.Helper()
	clean := q6StripSQLComments(sql)
	up := strings.ToUpper(clean)
	rest := ""
	if idx := strings.Index(up, "DO UPDATE SET"); idx >= 0 {
		rest = clean[idx+len("DO UPDATE SET"):]
	} else if idx := strings.Index(up, "UPDATE WEBSITE_INFO SET"); idx >= 0 {
		rest = clean[idx+len("UPDATE WEBSITE_INFO SET"):]
	} else {
		t.Fatal("no SET clause found in SQL")
	}
	var cols []string
	for _, part := range strings.Split(rest, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.Index(part, "=")
		if eq < 0 {
			t.Fatalf("SET assignment without '=': %q", part)
		}
		cols = append(cols, strings.ToLower(strings.TrimSpace(part[:eq])))
	}
	return cols
}

var q6PlaceholderRe = regexp.MustCompile(`\$(\d+)`)

// q6MaxPlaceholder returns the largest $N placeholder in the SQL.
func q6MaxPlaceholder(sql string) int {
	max := 0
	for _, m := range q6PlaceholderRe.FindAllStringSubmatch(sql, -1) {
		var n int
		for _, d := range m[1] {
			n = n*10 + int(d-'0')
		}
		if n > max {
			max = n
		}
	}
	return max
}

func q6Set(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// ---------------------------------------------------------------------------
// AST extraction: the fallback upsert + touchStmt live inline in main()'s
// db.Prepare calls. Extract the real literals so the tests pin the real SQL.
// ---------------------------------------------------------------------------

func q6ParseMainGo(t *testing.T) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	return f
}

// q6PrepareLiterals returns the string literals passed to X.Prepare(...) inside
// the named function.
func q6PrepareLiterals(t *testing.T, f *ast.File, funcName string) []string {
	t.Helper()
	var out []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != funcName {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Prepare" {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok {
				return true
			}
			s := lit.Value
			// Strip backticks of a raw string literal.
			s = strings.Trim(s, "`")
			out = append(out, s)
			return true
		})
	}
	return out
}

// q6FallbackUpsertSQL extracts the row-by-row fallback upsert SQL (the
// db.Prepare in main() whose INSERT covers the simhash family).
func q6FallbackUpsertSQL(t *testing.T) string {
	t.Helper()
	for _, s := range q6PrepareLiterals(t, q6ParseMainGo(t), "main") {
		up := strings.ToUpper(s)
		if strings.Contains(up, "INSERT INTO WEBSITE_INFO") &&
			strings.Contains(up, "SIMHASH") &&
			strings.Contains(up, "ON CONFLICT") {
			return s
		}
	}
	t.Fatal("fallback upsert SQL not found in main()'s db.Prepare calls — " +
		"the SQL moved; update the extraction")
	return ""
}

// q6TouchStmtSQL extracts the 304 touch statement (UPDATE ... crawled_at only).
func q6TouchStmtSQL(t *testing.T) string {
	t.Helper()
	for _, s := range q6PrepareLiterals(t, q6ParseMainGo(t), "main") {
		up := strings.ToUpper(s)
		if strings.Contains(up, "UPDATE WEBSITE_INFO SET") {
			return s
		}
	}
	t.Fatal("touchStmt SQL not found in main()'s db.Prepare calls — " +
		"the SQL moved; update the extraction")
	return ""
}

// q6CountCallArgs counts the args of the first X.Method(...) call inside the
// named function (e.g. upsert.Exec in flushRowByRow).
func q6CountCallArgs(t *testing.T, f *ast.File, funcName, recv, method string) int {
	t.Helper()
	count := -1
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != funcName {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || count >= 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != method {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || id.Name != recv {
				return true
			}
			count = len(call.Args)
			return false
		})
	}
	if count < 0 {
		t.Fatalf("%s.%s call not found in %s — code moved; update the extraction", recv, method, funcName)
	}
	return count
}

// q6CountAppendElts counts the elements appended in the first
// "args = append(args, ...)" statement inside the named function.
func q6CountAppendElts(t *testing.T, f *ast.File, funcName, varName string) int {
	t.Helper()
	count := -1
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != funcName {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 || count >= 0 {
				return true
			}
			lhs, ok := as.Lhs[0].(*ast.Ident)
			if !ok || lhs.Name != varName {
				return true
			}
			call, ok := as.Rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			fnid, ok := call.Fun.(*ast.Ident)
			if !ok || fnid.Name != "append" || len(call.Args) < 2 {
				return true
			}
			first, ok := call.Args[0].(*ast.Ident)
			if !ok || first.Name != varName {
				return true
			}
			count = len(call.Args) - 1
			return false
		})
	}
	if count < 0 {
		t.Fatalf("append to %s not found in %s — code moved; update the extraction", varName, funcName)
	}
	return count
}

// ---------------------------------------------------------------------------
// (a) sorted-upsert dedup — batch path.
// ---------------------------------------------------------------------------

// The batch INSERT must list exactly these 35 columns in this order; the 34
// row args cover domain..trap_suspect and crawled_at is bound to now().
var q6ExpectedBatchColumns = []string{
	"domain", "title", "description", "keywords", "canonical", "h1",
	"status_code", "content_type", "content_length", "latency_ms", "redirects",
	"outbound_links", "image_count", "word_count", "charset", "robots",
	"og_title", "og_image", "twitter_card", "page_language", "server_header",
	"fetch_error", "etag", "last_modified",
	"simhash", "simhash_b0", "simhash_b1", "simhash_b2", "simhash_b3",
	"duplicate_of", "sitemap_urls", "sitemap_lastmod",
	"domains_emitted_total", "trap_suspect", "crawled_at",
}

func TestQ6BatchUpsertHeadColumns(t *testing.T) {
	cols := q6InsertColumns(t, upsertHead)
	if len(cols) != len(q6ExpectedBatchColumns) {
		t.Fatalf("upsertHead column count = %d, want %d (%v)",
			len(cols), len(q6ExpectedBatchColumns), cols)
	}
	for i, want := range q6ExpectedBatchColumns {
		if cols[i] != want {
			t.Fatalf("upsertHead col %d = %q, want %q (full: %v)", i, cols[i], want, cols)
		}
	}
}

// Columns the batch ON CONFLICT SET is KNOWN not to refresh (Q6 finding,
// reported on #283 — ingest fixes hold for Claude). The test FAILS if this
// set changes in either direction, so a fix (or a new omission) forces a
// human to revisit the allowlist instead of silently changing behavior.
var q6BatchKnownGaps = []string{
	"simhash", "simhash_b0", "simhash_b1", "simhash_b2", "simhash_b3",
	"duplicate_of", "sitemap_urls", "sitemap_lastmod",
}

// Columns intentionally not refreshed on conflict in EITHER path:
// cumulative counters/flags, not per-crawl content.
var q6IntentionallyStatic = []string{"domains_emitted_total", "trap_suspect"}

func TestQ6BatchUpsertTailGaps(t *testing.T) {
	cols := q6Set(q6InsertColumns(t, upsertHead))
	assigned := q6Set(q6SetAssignedCols(t, upsertTail))

	// The #193 bug class, applied to the batch path: every EXCLUDED.* ref
	// must be a real INSERT column (else Postgres NULLs it on conflict).
	for _, ref := range q6ExcludedRefs(upsertTail) {
		if !cols[ref] {
			t.Errorf("upsertTail references EXCLUDED.%s but it is not an INSERT column "+
				"(conflict would NULL it — the #193 bug class)", ref)
		}
	}

	// missing = INSERT columns the SET never refreshes (besides the conflict
	// target `domain` and crawled_at, which is bound to now()).
	allowed := q6Set(append(append([]string{"domain", "crawled_at"},
		q6BatchKnownGaps...), q6IntentionallyStatic...))
	var missing []string
	for _, c := range q6ExpectedBatchColumns {
		if !assigned[c] && c != "domain" && c != "crawled_at" {
			missing = append(missing, c)
		}
	}
	for _, m := range missing {
		if !allowed[m] {
			t.Errorf("upsertTail newly omits %q on conflict (not in the known-gap "+
				"allowlist) — silent fingerprint/content freeze", m)
		}
	}
	// Pin the reported gap: it must still be EXACTLY the known set. If a fix
	// lands, this fails and the allowlist (and this comment) must be updated.
	knownSet := q6Set(q6BatchKnownGaps)
	for _, g := range q6BatchKnownGaps {
		if assigned[g] {
			t.Errorf("known gap %q is now refreshed by upsertTail — the #283 finding "+
				"is fixed; remove it from q6BatchKnownGaps", g)
		}
	}
	for _, m := range missing {
		if !knownSet[m] && !q6Set(q6IntentionallyStatic)[m] {
			t.Errorf("unexpected missing column %q outside the known-gap set", m)
		}
	}
	t.Logf("batch upsert conflict-refresh omits (known #283 gap): %v", missing)
}

func TestQ6BatchPerRowArgCount(t *testing.T) {
	// The row builder appends one arg per placeholder; placeholders are
	// numbered i*nc+j+1 with nc=34, and crawled_at is bound to now().
	const wantArgs = 34
	got := q6CountAppendElts(t, q6ParseMainGo(t), "flushBatchTx", "args")
	if got != wantArgs {
		t.Errorf("flushBatchTx appends %d args/row, want %d (placeholder/column mismatch → runtime SQL error)", got, wantArgs)
	}
	cols := q6InsertColumns(t, upsertHead)
	if len(cols)-1 != wantArgs { // -1: crawled_at ← now()
		t.Errorf("upsertHead has %d columns but rows bind %d args + now()", len(cols), wantArgs)
	}
}

// sortedKeys backs the discovery-emit path: "sorted, so the per-page cap
// keeps the same 150 every time" — the dedup-determinism property.
func TestQ6SortedKeysProperty(t *testing.T) {
	cases := []map[string]bool{
		nil,
		{},
		{"b": true},
		{"b": true, "a": true, "c": true},
		{"x.com": true, "a.com": true, "b.org": true},
	}
	for i, m := range cases {
		got := sortedKeys(m)
		if !sort.StringsAreSorted(got) {
			t.Errorf("case %d: sortedKeys output not sorted: %v", i, got)
		}
		if len(got) != len(m) {
			t.Errorf("case %d: sortedKeys dropped/added keys: %v vs %v", i, got, m)
		}
		seen := map[string]bool{}
		for _, k := range got {
			if !m[k] {
				t.Errorf("case %d: sortedKeys invented key %q", i, k)
			}
			if seen[k] {
				t.Errorf("case %d: sortedKeys duplicated key %q", i, k)
			}
			seen[k] = true
		}
		// Determinism: same map, repeated calls, identical order.
		again := sortedKeys(m)
		for j := range got {
			if got[j] != again[j] {
				t.Fatalf("case %d: sortedKeys not deterministic: %v vs %v", i, got, again)
			}
		}
	}
}

// A page's own stored fingerprint ALWAYS matches itself (IsDuplicate is
// reflexive), which is why the `dup != b.domain` guard in flushBatchTx is
// load-bearing: without it every re-crawl would mark itself duplicate_of.
func TestQ6SelfDupExclusionSemantics(t *testing.T) {
	fps := []uint64{0, 1, 0xAAAAAAAAAAAAAAAA, 0xFFFFFFFFFFFFFFFF, 0x1234567890abcdef}
	for _, fp := range fps {
		if !simhash.IsDuplicate(fp, fp) {
			t.Errorf("IsDuplicate(%#x, %#x) = false: not reflexive — the dup != b.domain guard's premise broke", fp, fp)
		}
		if simhash.Distance(fp, fp) != 0 {
			t.Errorf("Distance(%#x, itself) = %d, want 0", fp, simhash.Distance(fp, fp))
		}
		// Blocks of 0 and of all-ones are degenerate but must still round-trip.
		b := simhash.Blocks(fp)
		var re uint64
		re |= uint64(b[0]) << 48
		re |= uint64(b[1]) << 32
		re |= uint64(b[2]) << 16
		re |= uint64(b[3])
		if re != fp {
			t.Errorf("Blocks(%#x) does not reassemble: got %#x", fp, re)
		}
	}
}

// ---------------------------------------------------------------------------
// (b) SimHash fallback path (#193).
// ---------------------------------------------------------------------------

// The #193 regression guard on the CURRENT main: the row-by-row fallback
// prepared upsert (extracted from main()'s db.Prepare) must (1) list the
// simhash family + duplicate_of as INSERT columns, (2) refresh each of them
// in ON CONFLICT DO UPDATE SET, and (3) reference only real INSERT columns
// via EXCLUDED.* (the exact #193 bug: EXCLUDED for non-inserted columns is
// NULL, so a conflict would silently NULL the fingerprint).
func TestQ6FallbackUpsertSimhashCoverage(t *testing.T) {
	sql := q6FallbackUpsertSQL(t)
	cols := q6Set(q6InsertColumns(t, sql))

	required := []string{"simhash", "simhash_b0", "simhash_b1", "simhash_b2", "simhash_b3", "duplicate_of"}
	for _, c := range required {
		if !cols[c] {
			t.Errorf("fallback INSERT missing column %q (#193 not fixed on this branch?)", c)
		}
	}
	assigned := q6Set(q6SetAssignedCols(t, sql))
	for _, c := range required {
		if !assigned[c] {
			t.Errorf("fallback ON CONFLICT SET does not refresh %q (conflict would keep a stale fingerprint)", c)
		}
	}
	// The #193 bug class, on the fallback path itself.
	for _, ref := range q6ExcludedRefs(sql) {
		if !cols[ref] {
			t.Errorf("fallback references EXCLUDED.%s but it is not an INSERT column "+
				"(conflict would NULL it — the #193 bug class)", ref)
		}
	}
}

// The fallback INSERT binds $1..$34 (+NOW() for crawled_at); flushRowByRow's
// upsert.Exec must pass exactly 34 args or the DB errors at runtime.
func TestQ6FallbackPlaceholderArgParity(t *testing.T) {
	sql := q6FallbackUpsertSQL(t)
	if max := q6MaxPlaceholder(sql); max != 34 {
		t.Errorf("fallback upsert max placeholder = $%d, want $34", max)
	}
	cols := q6InsertColumns(t, sql)
	if len(cols) != 35 || cols[34] != "crawled_at" {
		t.Errorf("fallback INSERT has %d columns (last %q), want 35 ending in crawled_at", len(cols), cols[len(cols)-1])
	}
	n := q6CountCallArgs(t, q6ParseMainGo(t), "flushRowByRow", "upsert", "Exec")
	if n != 34 {
		t.Errorf("flushRowByRow upsert.Exec passes %d args, want 34 (placeholder mismatch → runtime SQL error)", n)
	}
}

// simhash.Blocks is the bridge between the Go fingerprint and the four
// simhash_b0..b3 index columns: it must round-trip exactly, including the
// int16 wrap the upsert args apply (Postgres smallint round-trips the bits).
func TestQ6SimhashBlocksRoundtrip(t *testing.T) {
	fps := []uint64{
		0, 1, 0x8000000000000000, 0xFFFFFFFFFFFFFFFF,
		0x0000800000008000, 0x7FFF7FFF7FFF7FFF, // int16-boundary block values
		0x1234567890abcdef,
	}
	for _, fp := range fps {
		b := simhash.Blocks(fp)
		var re uint64
		re |= uint64(b[0]) << 48
		re |= uint64(b[1]) << 32
		re |= uint64(b[2]) << 16
		re |= uint64(b[3])
		if re != fp {
			t.Errorf("Blocks(%#x) does not reassemble: got %#x", fp, re)
		}
		// int16 wrap used by the upsert args must be bit-preserving.
		for i, blk := range b {
			if uint16(int16(blk)) != blk {
				t.Errorf("Blocks(%#x)[%d] = %#x does not survive int16 wrap", fp, i, blk)
			}
		}
	}
}

// ns() sanitizes every text arg bound to the upsert: output must be valid
// UTF-8 with no NUL bytes (Postgres rejects both), or nil for blank input.
func TestQ6NsSanitizeProperty(t *testing.T) {
	inputs := []string{
		"", " ", "\t\n", "hello", "héllo wörld",
		"a\x00b", "\x00", "a\xffb", "\xff\xfe",
		string([]byte{0xed, 0xa0, 0x80}), // lone surrogate — invalid UTF-8
	}
	for _, in := range inputs {
		v := ns(in)
		if strings.TrimSpace(in) == "" {
			if v != nil {
				t.Errorf("ns(%q) = %v, want nil", in, v)
			}
			continue
		}
		// Inputs that sanitize down to blank (e.g. "\x00", "\xff\xfe")
		// legitimately return nil; anything else must be a clean string.
		s, ok := v.(string)
		if !ok {
			if v != nil {
				t.Errorf("ns(%q) returned non-string non-nil %T", in, v)
			}
			continue
		}
		if !utf8.ValidString(s) {
			t.Errorf("ns(%q) returned invalid UTF-8 %q", in, s)
		}
		if strings.ContainsRune(s, 0) {
			t.Errorf("ns(%q) returned NUL byte %q", in, s)
		}
	}
}

// ---------------------------------------------------------------------------
// (c) conditional-GET 304 verdict path.
// ---------------------------------------------------------------------------

// The 304 statement must refresh crawled_at and NOTHING else: a 304 means
// "content unchanged", so rewriting content/simhash columns here would be a
// data-corruption path (and would defeat the SimHash-delta change detection).
func TestQ6TouchStmtContentSafe(t *testing.T) {
	sql := q6TouchStmtSQL(t)
	assigned := q6SetAssignedCols(t, sql)
	if len(assigned) != 1 || assigned[0] != "crawled_at" {
		t.Errorf("touchStmt SET assigns %v, want exactly [crawled_at] "+
			"(304 path must never rewrite content columns)", assigned)
	}
	up := strings.ToUpper(q6StripSQLComments(sql))
	if !strings.Contains(up, "WHERE DOMAIN = $1") {
		t.Errorf("touchStmt lost its WHERE domain = $1 guard: %.120s", sql)
	}
}

// A 304 is a DEFINITIVE unchanged verdict regardless of fingerprints.
func TestQ6ChangeObserved304Property(t *testing.T) {
	pairs := [][2]uint64{
		{0, 0}, {0, 1}, {1, 0},
		{0xAAAAAAAAAAAAAAAA, 0xAAAAAAAAAAAAAAAA},
		{0xAAAAAAAAAAAAAAAA, 0xAAAAAAAAAAAAAAAA ^ 0xFF}, // would be "changed" as a 200
		{0xFFFFFFFFFFFFFFFF, 0},
	}
	for _, p := range pairs {
		changed, known := he4ChangeObserved(true, p[0], p[1])
		if changed || !known {
			t.Errorf("he4ChangeObserved(304, %#x, %#x) = (%v,%v), want (false,true)",
				p[0], p[1], changed, known)
		}
	}
}

// The 304/EWMA-unchanged backoff must NEVER shrink the interval (a 304 is
// evidence of stability) and must respect the 30-day cap.
func TestQ6BackoffNeverShrinksCapped(t *testing.T) {
	lambdas := []float64{0, 1.0 / 365, 1.0 / 120, 0.5, 2.0, 100}
	for _, lam := range lambdas {
		f := he4BackoffFactor(lam)
		if f < 1.1 || f > 1.25 {
			t.Errorf("he4BackoffFactor(%v) = %v, want within [1.1, 1.25]", lam, f)
		}
	}
	currents := []float64{0.5, 1, 5, 14, 27, 30, 60}
	for _, cur := range currents {
		for _, lam := range lambdas {
			got := t3NextIntervalLambda(cur, false, lam, 14)
			if got > t3IntervalCapDays {
				t.Errorf("unchanged: t3NextIntervalLambda(%v,false,%v) = %v exceeds cap %v",
					cur, lam, got, t3IntervalCapDays)
			}
			if cur*1.1 <= t3IntervalCapDays && got < cur*1.1-1e-9 {
				t.Errorf("unchanged: t3NextIntervalLambda(%v,false,%v) = %v shrank below %v*1.1 "+
					"(304 must never shorten the interval)", cur, lam, got, cur)
			}
		}
	}
}

// Contrast: the changed (SimHash-delta) path tightens but never below the
// 1-day floor. Guards the tighten/backoff asymmetry the 304 path relies on.
func TestQ6TightenFloorContrast(t *testing.T) {
	// Above the floor, a change tightens the interval.
	for _, cur := range []float64{2, 5, 30} {
		got := t3NextIntervalLambda(cur, true, 2.0, 14)
		if got < t3IntervalFloorDays {
			t.Errorf("changed: t3NextIntervalLambda(%v,true,2.0) = %v below floor %v",
				cur, got, t3IntervalFloorDays)
		}
		if got >= cur {
			t.Errorf("changed: t3NextIntervalLambda(%v,true,2.0) = %v did not tighten", cur, got)
		}
	}
	// Below the floor, tightening clamps UP to the 1-day floor (never below).
	if got := t3NextIntervalLambda(0.5, true, 2.0, 14); got != t3IntervalFloorDays {
		t.Errorf("changed: t3NextIntervalLambda(0.5,true,2.0) = %v, want floor %v",
			got, t3IntervalFloorDays)
	}
}

// ---------------------------------------------------------------------------
// Fuzz targets (run under the #276 CI fuzz umbrella: fuzz-ingest job).
// ---------------------------------------------------------------------------

func FuzzQ6SortedKeys(f *testing.F) {
	f.Add([]byte("b\x00a\x00c"))
	f.Add([]byte{})
	f.Add([]byte("dup\x00dup\x00dup"))
	f.Fuzz(func(t *testing.T, data []byte) {
		parts := strings.Split(string(data), "\x00")
		m := make(map[string]bool, len(parts))
		for _, p := range parts {
			m[p] = true
		}
		got := sortedKeys(m)
		if !sort.StringsAreSorted(got) {
			t.Fatalf("sortedKeys output not sorted: %q", got)
		}
		if len(got) != len(m) {
			t.Fatalf("sortedKeys changed key count: %d vs %d", len(got), len(m))
		}
	})
}

func FuzzQ6SimhashBlocks(f *testing.F) {
	f.Add([]byte{0x12, 0x34, 0x56, 0x78, 0x90, 0xab, 0xcd, 0xef})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		var b8 [8]byte
		copy(b8[:], data)
		var fp uint64
		for i := 0; i < 8; i++ {
			fp |= uint64(b8[i]) << (8 * i)
		}
		blks := simhash.Blocks(fp)
		var re uint64
		re |= uint64(blks[0]) << 48
		re |= uint64(blks[1]) << 32
		re |= uint64(blks[2]) << 16
		re |= uint64(blks[3])
		if re != fp {
			t.Fatalf("Blocks(%#x) does not reassemble: got %#x", fp, re)
		}
		for i, blk := range blks {
			if uint16(int16(blk)) != blk {
				t.Fatalf("Blocks(%#x)[%d] lost bits through int16 wrap", fp, i)
			}
		}
	})
}

func FuzzQ6NsSanitize(f *testing.F) {
	f.Add("hello")
	f.Add("a\x00b")
	f.Add("\xff\xfe invalid")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		v := ns(s)
		if strings.TrimSpace(s) == "" {
			if v != nil {
				t.Fatalf("ns(%q) = %v, want nil", s, v)
			}
			return
		}
		out, ok := v.(string)
		if !ok {
			// Inputs that sanitize down to blank legitimately return nil.
			if v != nil {
				t.Fatalf("ns(%q) returned non-string non-nil %T", s, v)
			}
			return
		}
		if !utf8.ValidString(out) {
			t.Fatalf("ns(%q) returned invalid UTF-8", s)
		}
		if strings.ContainsRune(out, 0) {
			t.Fatalf("ns(%q) returned NUL byte", s)
		}
	})
}

func FuzzQ6ChangeObserved304(f *testing.F) {
	f.Add(uint64(0), uint64(0))
	f.Add(uint64(0xAAAAAAAAAAAAAAAA), uint64(0xAAAAAAAAAAAAAAAA^0xFF))
	f.Fuzz(func(t *testing.T, a, b uint64) {
		changed, known := he4ChangeObserved(true, a, b)
		if changed || !known {
			t.Fatalf("he4ChangeObserved(304, %#x, %#x) = (%v,%v), want (false,true)", a, b, changed, known)
		}
	})
}
