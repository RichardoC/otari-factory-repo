package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

// writeReviewResult points RESULT_PATH at a result file with the given stop
// reason and answer, the two fields the review report reads, and returns the
// path so a test can put the run record beside it.
func writeReviewResult(t *testing.T, stop, answer string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "result.json")
	data, err := json.Marshal(map[string]any{"stop": stop, "model": "zai-org/glm-5.3-flash", "answer": answer})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RESULT_PATH", path)
	return path
}

// writeTrajectory points TRAJECTORY_PATH at a run record holding the given
// lines, the fallback source the review report reads when the result's
// answer carries no findings.
func writeTrajectory(t *testing.T, lines string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trajectory.jsonl")
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRAJECTORY_PATH", path)
}

// reviewLedger points LEDGER_PATH at a two-call ledger worth 0.25 USD; the
// proxy_start marker line must not count toward the total.
func reviewLedger(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	lines := `{"kind":"proxy_start"}
{"model":"zai-org/glm-5.3-flash","cost_usd":0.25}
{"model":"zai-org/glm-5.3-flash","cost_usd":0}
`
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEDGER_PATH", path)
}

// reviewServer serves the fake API for the review tests: routes maps a path
// to a body, and diffBody is served on the pull request route when the
// client asks for a diff. The Accept header of the first pull request
// request is recorded in sawAccept when it is given.
func reviewServer(t *testing.T, routes map[string]string, diffBody string, sawAccept *string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/RichardoC/factory-repo/pulls/7" {
			if sawAccept != nil {
				*sawAccept = r.Header.Get("Accept")
			}
			if diffBody != "" && strings.Contains(r.Header.Get("Accept"), "diff") {
				_, _ = io.WriteString(w, diffBody)
				return
			}
		}
		body, ok := routes[r.URL.Path]
		if !ok {
			http.Error(w, "not served: "+r.URL.Path, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

// reviewFindingsAnswer is an answer carrying the whole contract: all three
// severities, an unknown severity word, every separator, a nested bullet,
// hostile text, and a bullet before and after the findings section.
const reviewFindingsAnswer = "Some prose first.\n\n" +
	"- [high] before.go:1 - a bullet before the section must not parse\n\n" +
	"## Summary\n\n- [high] also-before.go:2 - still before the section\n\n" +
	"### findings\n\n" +
	"- [high] app/x/main.go:42 - the mutex is released twice on the error path\n" +
	"* [Medium] app/y/queue.go:7 — the queue never drains when a send fails\n" +
	"- [low] docs/readme.md:3 : typo in the flag name\n" +
	"- [CRITICAL] app/z/z.go:1 – the universe ends\n" +
	"- [high] app/w/lock.go:5 -- | pipes | and <!-- html comments --> and `backticks`\n" +
	"  - [high] app/w/nested.go:9 - an indented bullet still parses\n\n" +
	"## Notes\n\n" +
	"- [high] after.go:1 - a bullet after the next header must not parse\n"

func TestParseFindings(t *testing.T) {
	findings, sawSection := parseFindings(reviewFindingsAnswer)
	if !sawSection {
		t.Fatal("sawSection = false, want true")
	}
	// The section still holds its six bullets, and the bullets written
	// outside it now parse as inline findings (issue #49) — in answer order,
	// so the section findings sit between the ones before and after it.
	if len(findings) != 9 {
		t.Fatalf("got %d findings, want 9: %+v", len(findings), findings)
	}
	want := []struct {
		severity   string
		path       string
		line       int
		text       string
		recognized bool
	}{
		{"high", "before.go", 1, "a bullet before the section must not parse", true},
		{"high", "also-before.go", 2, "still before the section", true},
		{"high", "app/x/main.go", 42, "the mutex is released twice on the error path", true},
		{"Medium", "app/y/queue.go", 7, "the queue never drains when a send fails", true},
		{"low", "docs/readme.md", 3, "typo in the flag name", true},
		{"CRITICAL", "app/z/z.go", 1, "the universe ends", false},
		{"high", "app/w/lock.go", 5, "| pipes | and <!-- html comments --> and `backticks`", true},
		{"high", "app/w/nested.go", 9, "an indented bullet still parses", true},
		{"high", "after.go", 1, "a bullet after the next header must not parse", true},
	}
	for i, w := range want {
		f := findings[i]
		if f.Severity != w.severity || f.Path != w.path || f.Line != w.line || f.Text != w.text || f.Recognized != w.recognized {
			t.Errorf("findings[%d] = %+v, want severity %q path %q line %d text %q recognized %t",
				i, f, w.severity, w.path, w.line, w.text, w.recognized)
		}
	}
	// Hostile text is kept verbatim, never truncated or rewritten.
	if findings[6].Text != "| pipes | and <!-- html comments --> and `backticks`" {
		t.Errorf("hostile text was rewritten: %q", findings[6].Text)
	}

	// CRLF endings parse the same as LF.
	findings, sawSection = parseFindings(strings.ReplaceAll(reviewFindingsAnswer, "\n", "\r\n"))
	if !sawSection || len(findings) != 9 || findings[0].Text != want[0].text {
		t.Errorf("CRLF parse = %d findings, sawSection %t, first text %q", len(findings), sawSection, findings[0].Text)
	}

	// No findings section at all, and nothing inline either.
	if findings, sawSection := parseFindings("Just an answer, no section, no header."); findings != nil || sawSection {
		t.Errorf("missing section = (%v, %t), want (nil, false)", findings, sawSection)
	}

	// A header-like word that is not the section: "Findings" inside a
	// sentence does not open it, and does not count as sawSection — but the
	// bullet under it is an inline finding now (issue #49).
	findings, sawSection = parseFindings("## Notes on findings so far\n\n- [high] a.go:1 - x")
	if sawSection {
		t.Error("prose header counted as sawSection")
	}
	if len(findings) != 1 || findings[0].Path != "a.go" || findings[0].Line != 1 {
		t.Errorf("prose header inline parse = %+v, want one a.go:1 finding", findings)
	}

	// Malformed bullets are ignored: none of these is a finding, under the
	// section contract or the inline one. A token followed by text that
	// opens like a path:line is dropped whole rather than read as a finding
	// whose text is the half-written location.
	malformed := []string{
		"- high app/x.go:42 - no brackets",
		"- [high app/x.go:42 - never closed",
		"- [] app/x.go:42 - empty severity",
		"- [high]app/x.go:42 - no space after the severity",
		"- [high] app/x.go:42",
		"- [high] app/x.go:42 no separator at all",
		"- [high] :42 - no path",
		"plain prose in the section",
		"- [high] app/x.go:42 - ",
		"- [x] a task-list bullet is not a finding",
		"- [ ] nor an empty one",
		"see [the diff](https://example.com/a:b) and [TODO] fix later",
		"[critical] an unknown severity with no location is prose, not a finding",
	}
	var b strings.Builder
	b.WriteString("## Findings\n\n")
	for _, m := range malformed {
		b.WriteString(m + "\n")
	}
	if findings, _ := parseFindings(b.String()); len(findings) != 0 {
		t.Errorf("malformed bullets produced %d findings: %+v", len(findings), findings)
	}

	// The lenient inline forms (issue #49): a located finding anywhere in a
	// line, with any separator and any case, and a finding with no location
	// when the severity is one of the three known words.
	inline := []struct {
		line       string
		severity   string
		path       string
		line_      int
		text       string
		recognized bool
	}{
		{"Findings: [high] app/x/main.go:42 - the mutex is released twice", "high", "app/x/main.go", 42, "the mutex is released twice", true},
		{"- [HIGH] app/y/queue.go:7 — em dash, capital, no section", "HIGH", "app/y/queue.go", 7, "em dash, capital, no section", true},
		{"[Medium] the queue never drains when a send fails", "Medium", "", 0, "the queue never drains when a send fails", true},
		{"[low] docs: the flag is undocumented", "low", "", 0, "docs: the flag is undocumented", true},
		{"[CRITICAL] app/z/z.go:1 - an unknown scale with a location still reports", "CRITICAL", "app/z/z.go", 1, "an unknown scale with a location still reports", false},
	}
	for _, w := range inline {
		findings, sawSection := parseFindings("prose before\n\n" + w.line + "\n\nprose after")
		if sawSection {
			t.Errorf("%q: inline parse set sawSection", w.line)
		}
		if len(findings) != 1 {
			t.Errorf("%q: got %d findings, want 1: %+v", w.line, len(findings), findings)
			continue
		}
		f := findings[0]
		if f.Severity != w.severity || f.Path != w.path || f.Line != w.line_ || f.Text != w.text || f.Recognized != w.recognized {
			t.Errorf("%q: parsed %+v, want severity %q path %q line %d text %q recognized %t",
				w.line, f, w.severity, w.path, w.line_, w.text, w.recognized)
		}
	}

	// Two findings written on one line both parse.
	findings, _ = parseFindings("[high] a.go:1 - first and [low] b.go:2 - second")
	if len(findings) != 2 || findings[0].Path != "a.go" || findings[1].Path != "b.go" {
		t.Errorf("two inline findings on one line = %+v, want a.go then b.go", findings)
	}

	// Several findings on one line, separated by `;` (issue #111): run
	// 39062136141 closed with one paragraph in exactly this shape, and
	// each finding must keep its own text, not swallow the rest of the
	// line. The `FINDINGS (n, …):` prefix is prose, not a finding.
	paragraph := "FINDINGS (3, 2 blocking): [medium] main.go:1155 - the mutex is released twice under the error path; " +
		"[medium] queue.go:20 - the queue never drains when a send fails; " +
		"[low] docs/readme.md:3 - typo in the flag name"
	findings, sawSection = parseFindings(paragraph)
	if sawSection {
		t.Error("a FINDINGS paragraph counted as sawSection")
	}
	if len(findings) != 3 {
		t.Fatalf("FINDINGS paragraph = %d findings, want 3: %+v", len(findings), findings)
	}
	wantParagraph := []struct {
		severity string
		path     string
		line     int
		text     string
	}{
		{"medium", "main.go", 1155, "the mutex is released twice under the error path"},
		{"medium", "queue.go", 20, "the queue never drains when a send fails"},
		{"low", "docs/readme.md", 3, "typo in the flag name"},
	}
	for i, w := range wantParagraph {
		f := findings[i]
		if f.Severity != w.severity || f.Path != w.path || f.Line != w.line || f.Text != w.text || !f.Recognized {
			t.Errorf("paragraph findings[%d] = %+v, want %v", i, f, w)
		}
	}

	// A `;` before a word that is not a severity is not a boundary: the
	// word is prose and stays in the text, never a finding.
	multiline := "The review is complete.\n\n" +
		"[high] a.go:1 - use the slow path; see the notes on [the queue](x) first; " +
		"[low] b.go:2 - second"
	findings, _ = parseFindings(multiline)
	if len(findings) != 2 || findings[0].Path != "a.go" || findings[0].Text != "use the slow path; see the notes on [the queue](x) first" ||
		findings[1].Path != "b.go" || findings[1].Text != "second" {
		t.Errorf("chained parse = %+v, want a.go with its full text then b.go", findings)
	}

	// A hostile FINDINGS paragraph — markdown, a mention, a very long
	// line — parses as findings and the renderer escapes and caps it,
	// never carrying it verbatim.
	hostileParagraph := "FINDINGS (2, 1 blocking): [high] " + strings.Repeat("x/", 200) + "deep.go:1 - | pipes | and <!-- html comments --> and @alice; " +
		"[low] b.go:2 - " + strings.Repeat("long ", 2000) + "text"
	findings, _ = parseFindings(hostileParagraph)
	if len(findings) != 2 || findings[0].Path != strings.Repeat("x/", 200)+"deep.go" || findings[1].Path != "b.go" {
		t.Fatalf("hostile paragraph = %+v", findings)
	}
	if findings[0].Text != "| pipes | and <!-- html comments --> and @alice" {
		t.Errorf("hostile paragraph text was rewritten: %q", findings[0].Text)
	}
	table := reviewTable(findings)
	assertReviewBodyClean(t, table)
	if !strings.Contains(table, strings.Repeat("x/", 12)) || len(table) > 4096 {
		t.Errorf("hostile paragraph table not capped: %d bytes", len(table))
	}

	// The same finding reached by both passes is kept once.
	findings, _ = parseFindings("## Findings\n\n- [high] app/x/main.go:42 - the mutex is released twice\n" +
		"[high] app/x/main.go:42 - the mutex is released twice\n")
	if len(findings) != 1 {
		t.Errorf("duplicate parse = %d findings, want 1: %+v", len(findings), findings)
	}

	// The cap counts section and inline findings together.
	b.Reset()
	b.WriteString("## Findings\n\n")
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&b, "- [low] app/cap.go:%d - section finding %d\n", i, i)
	}
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&b, "[low] app/cap.go:%d - inline finding %d\n", i+100, i)
	}
	findings, _ = parseFindings(b.String())
	if len(findings) != maxReviewFindings {
		t.Fatalf("mixed cap: got %d findings, want %d", len(findings), maxReviewFindings)
	}

	// An empty answer is no findings and no section.
	if findings, sawSection := parseFindings(""); findings != nil || sawSection {
		t.Errorf("empty answer = (%v, %t), want (nil, false)", findings, sawSection)
	}

	// A hostile line of brackets is scanned once, not once per bracket: the
	// one real finding buried in it still parses, and the scan stays linear.
	hostile := strings.Repeat("[[]] ", 20000) + "[high] app/x/main.go:42 - the mutex is released twice" + strings.Repeat(" [ ]", 20000)
	findings, _ = parseFindings(hostile)
	if len(findings) != 1 || findings[0].Path != "app/x/main.go" || findings[0].Line != 42 {
		t.Errorf("hostile bracket line = %+v, want the one real finding", findings)
	}

	// The cap: 60 valid bullets yield exactly maxReviewFindings.
	b.Reset()
	b.WriteString("## Findings\n\n")
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&b, "- [low] app/cap.go:%d - finding %d\n", i, i)
	}
	findings, _ = parseFindings(b.String())
	if len(findings) != maxReviewFindings {
		t.Fatalf("got %d findings, want the cap %d", len(findings), maxReviewFindings)
	}
	if findings[maxReviewFindings-1].Line != maxReviewFindings {
		t.Errorf("cap kept the wrong findings: last line = %d", findings[maxReviewFindings-1].Line)
	}
}

// TestParseFindingsSeverityWordInsideFindingText: a bracketed severity word
// inside a finding's own text must stay that finding's text (issue #335).
// The inline pass advanced only past the token it had just read, so it
// rescanned the text the finding had already consumed; a `[medium] severity`
// the reviewer quoted parsed again as the no-location form, minting a phantom
// blocking finding with no path. blockingFindings counted 2, and a `/codeaf
// fix` run started against a finding nobody wrote, spending a continuation
// slot and its credit.
func TestParseFindingsSeverityWordInsideFindingText(t *testing.T) {
	// The exact answer from the issue: one located finding that quotes the
	// word `medium` in its text. It is one finding, and it blocks once.
	answer := "## Findings\n\n- [high] app/x/main.go:42 - the mutex is released twice, so a [medium] severity is misreported by this check\n"
	findings, sawSection := parseFindings(answer)
	if !sawSection {
		t.Error("the findings section was not seen")
	}
	if len(findings) != 1 {
		t.Fatalf("quoted severity word = %d findings, want 1: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.Severity != "high" || f.Path != "app/x/main.go" || f.Line != 42 ||
		f.Text != "the mutex is released twice, so a [medium] severity is misreported by this check" {
		t.Errorf("finding was rewritten: %+v", f)
	}
	if blocking := blockingFindings(findings); len(blocking) != 1 {
		t.Errorf("blocking = %d findings, want 1: %+v", len(blocking), blocking)
	}

	// The same line read inline, outside the section: still one finding.
	findings, _ = parseFindings("Findings: [high] app/x/main.go:42 - a [medium] severity, and a [low] one too\n")
	if len(findings) != 1 || findings[0].Path != "app/x/main.go" ||
		findings[0].Text != "a [medium] severity, and a [low] one too" {
		t.Fatalf("inline quoted severity word = %+v, want the one high finding", findings)
	}
	if blocking := blockingFindings(findings); len(blocking) != 1 {
		t.Errorf("inline blocking = %d findings, want 1: %+v", len(blocking), blocking)
	}

	// A genuine chain still splits at its `;` boundary, with or without a
	// connective word after the semicolon (issue #111), and a located
	// finding later on the same line still parses (the lenient same-line
	// form the contract accepts).
	chains := []struct {
		line  string
		paths []string
		texts []string
	}{
		{
			"[high] a.go:1 - first; [medium] b.go:2 - second",
			[]string{"a.go", "b.go"},
			[]string{"first", "second"},
		},
		{
			"[high] a.go:1 - first; and [medium] b.go:2 - second",
			[]string{"a.go", "b.go"},
			[]string{"first", "second"},
		},
		{
			// No `;` at all: the located bullet on the same line still
			// parses, and the word before it stays the first finding's
			// text (the lenient same-line form, unchanged).
			"[high] a.go:1 - first and [low] b.go:2 - second",
			[]string{"a.go", "b.go"},
			[]string{"first and [low] b.go:2 - second", "second"},
		},
		{
			"[high] a.go:1 - the check reports a [medium] severity; [low] b.go:2 - second",
			[]string{"a.go", "b.go"},
			[]string{"the check reports a [medium] severity", "second"},
		},
	}
	for _, w := range chains {
		got, _ := parseFindings(w.line)
		if len(got) != len(w.paths) {
			t.Errorf("%q = %d findings, want %d: %+v", w.line, len(got), len(w.paths), got)
			continue
		}
		for i := range w.paths {
			if got[i].Path != w.paths[i] || got[i].Text != w.texts[i] {
				t.Errorf("%q findings[%d] = %+v, want path %q text %q", w.line, i, got[i], w.paths[i], w.texts[i])
			}
		}
	}

	// A hostile paragraph that quotes severities in every finding's text:
	// each finding keeps its own text and no phantom is minted.
	hostile := "FINDINGS (2, 2 blocking): [high] a.go:1 - treats [high] and [medium] and [low] as prose; " +
		"[medium] b.go:2 - also quotes [low] and [high] words"
	findings, _ = parseFindings(hostile)
	if len(findings) != 2 || findings[0].Path != "a.go" || findings[1].Path != "b.go" {
		t.Fatalf("hostile quoted-severity paragraph = %+v, want a.go then b.go", findings)
	}
	if findings[0].Text != "treats [high] and [medium] and [low] as prose" {
		t.Errorf("hostile paragraph text was rewritten: %q", findings[0].Text)
	}
	if findings[1].Text != "also quotes [low] and [high] words" {
		t.Errorf("hostile paragraph text was rewritten: %q", findings[1].Text)
	}
	if blocking := blockingFindings(findings); len(blocking) != 2 {
		t.Errorf("hostile paragraph blocking = %d, want 2: %+v", len(blocking), blocking)
	}
}

// TestParseFindingsFencedContractEcho: the harness's own output contract
// echoes back in a fence (issue #224) — the exact phantom that minted a
// blocking finding on app/zip/queue.go and started a fix run against a file
// that does not exist. The echo is inert because the bullet is the
// contract's example, whether or not the diff is known, and the unfenced
// echo keeps the issue #49 leniency unchanged.
func TestParseFindingsFencedContractEcho(t *testing.T) {
	// The reviewer quotes the contract the brief ended with, then answers.
	// The fence is exactly where a reviewer quotes output — and it is also
	// what the brief's own diff section uses, so the check must not depend
	// on the fence's info string.
	fenced := "Here is the output contract I was given, quoted verbatim:\n\n" +
		"```markdown\n" + reviewOutputContract + "```\n\n" +
		"## Findings\n\nnone\n"
	known := diffPaths("diff --git a/app/queue/main.go b/app/queue/main.go\n" +
		"--- a/app/queue/main.go\n+++ b/app/queue/main.go\n" +
		"@@ -1,0 +1,1 @@\n+package main\n")

	t.Run("fenced echo is inert", func(t *testing.T) {
		// The whole contract, echoed inside a fence: the example bullet is
		// dropped, and only it, because the second bullet carries no false
		// location — the spell-out is what makes a bullet a phantom.
		for _, tc := range []struct {
			name   string
			answer string
			known  map[string]bool
		}{
			{"with the diff known", fenced, known},
			{"without the diff", fenced, nil},
			{"bare fence", "```\n- [high] app/zip/queue.go:42 - the mutex is released twice under the error path, so a second request deadlocks\n```\n", known},
		} {
			findings, _ := parseFindingsIn(tc.answer, tc.known)
			for _, f := range findings {
				if f.Path == contractExamplePath && f.Line == contractExampleLine {
					t.Errorf("%s: the contract's example bullet was kept: %+v", tc.name, f)
				}
			}
			if blocking := blockingFindings(findings); len(blocking) != 0 {
				t.Errorf("%s: blocking = %+v, want none — the echo must start no fix run", tc.name, blocking)
			}
		}
	})

	t.Run("the unfenced echo is unchanged", func(t *testing.T) {
		// Issue #49's leniency is deliberate: an unfenced echo still parses
		// as a finding, so this issue cannot widen into dropping prose.
		unfenced := "Here is the output contract I was given:\n\n" + reviewOutputContract + "\n## Findings\n\nnone\n"
		findings, _ := parseFindingsIn(unfenced, known)
		if got := blockingFindings(findings); len(got) == 0 {
			t.Fatalf("the unfenced contract echo no longer blocks: %+v", findings)
		}
		for _, f := range findings {
			if f.Path == contractExamplePath && f.Line == contractExampleLine && f.fenced {
				t.Errorf("an unfenced finding was marked fenced: %+v", f)
			}
		}
	})
}

// TestParseFindingsFencedRealFindingKept: the maintainer's rule is not "skip
// every fenced line" (issue #224). A reviewer who reports inside a fence is
// reporting, so a fenced finding whose path the diff touches is kept and
// still blocks.
func TestParseFindingsFencedRealFindingKept(t *testing.T) {
	diff := "diff --git a/app/queue/main.go b/app/queue/main.go\n" +
		"--- a/app/queue/main.go\n+++ b/app/queue/main.go\n" +
		"@@ -1,0 +1,1 @@\n+package main\n"
	known := diffPaths(diff)
	if known == nil {
		t.Fatal("diffPaths dropped a real diff")
	}
	answer := "## Findings\n\n```\n" +
		"- [high] app/queue/main.go:42 - the lock is held across the send\n" +
		"```\n"
	findings, sawSection := parseFindingsIn(answer, known)
	if !sawSection || len(findings) != 1 {
		t.Fatalf("parse = %d findings, sawSection %t, want 1/true: %+v", len(findings), sawSection, findings)
	}
	if got := blockingFindings(findings); len(got) != 1 {
		t.Errorf("blocking = %+v, want the fenced real finding", got)
	}
	// A bullet written in the section, inside a closed fence, is always
	// read (issue #334): the quoted-path filter never applies to the
	// section, so even a bullet on a path the diff does not touch blocks —
	// the section is the contract the instructions promise.
	quoted := "## Findings\n\n```\n" +
		"- [high] app/elsewhere/other.go:7 - a finding about another change\n" +
		"```\n"
	findings, _ = parseFindingsIn(quoted, known)
	if len(blockingFindings(findings)) != 1 {
		t.Errorf("a fenced section finding outside the diff = %+v, want it kept and blocking", findings)
	}
	// With no diff known the same text is kept as well: the parser never
	// guesses, and no diff read may ever lose a finding.
	if findings, _ = parseFindingsIn(quoted, nil); len(blockingFindings(findings)) != 1 {
		t.Errorf("no diff known = %+v, want the finding kept", findings)
	}
}

// TestParseFindingsFencedSectionAlwaysRead (issue #334): the instructions
// promise "a finding written in the `## Findings` section is always read",
// but the quoted-path filter used to reach section bullets too, so a real
// blocking finding written in the section, inside a closed fence, on a path
// the diff did not touch was dropped and the review ended with the pass
// label. The section pass keeps it: only the contract-echo phantom — a file
// this repository does not have — is still a quote in the section.
func TestParseFindingsFencedSectionAlwaysRead(t *testing.T) {
	known := map[string]bool{"app/x/main.go": true}
	// The answer the issue reproduced: pre-fix it parsed to 0 findings and
	// returned the pass label, while the same bullet unfenced parsed to 1.
	reviewed := "## Findings\n\n```\n" +
		"- [high] app/other/y.go:7 - real blocking bug in a file the diff does not touch\n" +
		"```\n"
	findings, sawSection := parseFindingsIn(reviewed, known)
	if !sawSection {
		t.Fatal("the findings section inside the closed fence was missed")
	}
	if got := len(blockingFindings(findings)); got != 1 {
		t.Fatalf("blocking = %d, want 1 — the fenced section finding was dropped: %+v", got, findings)
	}
	if f := findings[0]; f.Path != "app/other/y.go" || !f.fenced || !f.quoted {
		t.Errorf("the kept finding = %+v, want the fenced bullet on app/other/y.go", f)
	}
	// The same bullet unfenced in the section parses to the same one
	// finding — the fence changes nothing in the section.
	unfenced := "## Findings\n\n" +
		"- [high] app/other/y.go:7 - real blocking bug in a file the diff does not touch\n"
	if findings, _ := parseFindingsIn(unfenced, known); len(blockingFindings(findings)) != 1 {
		t.Errorf("the unfenced section finding = %+v, want it kept too", findings)
	}
	// The section's promise covers findings in every written form, not
	// just bullets the section pass reads itself. A line inside the
	// section that parseFinding misses but the inline pass reads — prose
	// before the severity token, or a chained second finding — used to be
	// dropped by the quoted-path filter in the inline pass (the review
	// finding on review.go:189), so a blocking inline-form finding inside
	// a fenced section still ended in the pass label.
	inlineProse := "## Findings\n\n```\n" +
		"Summary: [medium] app/other/y.go:2 - the quoted-path filter must not reach the section\n" +
		"```\n"
	if findings, _ := parseFindingsIn(inlineProse, known); len(blockingFindings(findings)) != 1 {
		t.Errorf("an inline-form finding in a fenced section = %+v, want it kept and blocking", findings)
	}
	chained := "## Findings\n\n```\n" +
		"- [low] app/x/main.go:3 - a minor note ; [high] app/other/y.go:9 - the chained blocking finding\n" +
		"```\n"
	if findings, _ := parseFindingsIn(chained, known); len(blockingFindings(findings)) != 1 {
		t.Errorf("a chained finding in a fenced section = %+v, want the high kept and blocking", findings)
	}
	// Plan-level: the inline form blocks too, so the review that carried it
	// starts a fix run instead of stamping the pass label.
	parsedInline, _ := parseFindingsIn(inlineProse, known)
	serverInline := commentsServer(t, reportComments(0), http.StatusOK)
	ghEnv(t, serverInline)
	if action, body, _, err := reviewPlan(newGitHub(), 7,
		Result{Stop: stopDone, Model: "deepseek-ai/deepseek-v4.1-flash"}, true, parsedInline, true); err != nil {
		t.Fatal(err)
	} else if action != "fix" || strings.Contains(body, reviewPassedLabel) {
		t.Errorf("an inline-form finding in a fenced section = action %q, want the fix run with no pass label: %q", action, body)
	}
	// The contract echo inside the section is still a quote (issue #224
	// stands in the section, #334 or not): only the path filter is exempt.
	echo := "## Findings\n\n```\n" +
		"- [high] app/zip/queue.go:42 - " + contractExampleText + "\n```\n"
	if findings, _ := parseFindingsIn(echo, known); len(blockingFindings(findings)) != 0 {
		t.Errorf("the contract echo in a fenced section = %+v, want it dropped", findings)
	}
	// Outside the section the same closed fence still drops a bullet on an
	// untouched path: the fix is the section's exemption, not a wider one.
	plain := "I quoted the hunk:\n```\n- [high] app/other/y.go:7 - a bug in a file the diff does not touch\n```\n"
	if findings, _ := parseFindingsIn(plain, known); len(findings) != 0 {
		t.Errorf("a fenced prose bullet outside the section and the diff = %+v, want it dropped", findings)
	}
	// Plan-level: reviewPlan reads blockingFindings to choose between the
	// pass label and a fix run, and the reproduced answer now parses to a
	// blocking finding — so the review that carried it starts a fix run, and
	// `codeaf-review-passed` is never added (that was the false pass the
	// issue reported).
	parsed, _ := parseFindingsIn(reviewed, known)
	server := commentsServer(t, reportComments(0), http.StatusOK)
	ghEnv(t, server)
	action, body, used, err := reviewPlan(newGitHub(), 7,
		Result{Stop: stopDone, Model: "deepseek-ai/deepseek-v4.1-flash"}, true, parsed, true)
	if err != nil {
		t.Fatal(err)
	}
	if action != "fix" || used != 0 || !strings.HasPrefix(body, command+" fix:") {
		t.Errorf("a review of the reproduced answer = action %q used %d, want the fix run: %q", action, used, body)
	}
	if strings.Contains(body, reviewPassedLabel) {
		t.Errorf("the reproduced answer still earns the pass label: %q", body)
	}
}

// TestParseFindingsFenceState: a fence is tracked, not guessed from the
// line, so prose after a closing fence parses again. A delimiter line is not
// opaque (issue #224 follow-up finding review.go:120): the section and inline
// passes still run on it, judged with the state the line opens, so a finding
// written on a delimiter line is kept — as the parser before the fence
// tracking kept it.
func TestParseFindingsFenceState(t *testing.T) {
	answer := "```\n- [high] app/a.go:1 - quoted\n```\n" +
		"- [high] app/outside.go:2 - reported after the fence\n"
	if findings, _ := parseFindingsIn(answer, map[string]bool{"app/a.go": true, "app/outside.go": true}); len(findings) != 2 {
		// app/a.go is in the diff, so its fenced finding is kept; the
		// prose finding after the fence is kept too.
		t.Errorf("fence state = %+v, want both findings", findings)
	}
	// A finding on the opening delimiter line: the line opens the fence, and
	// it is parsed, so the bullet written on it is kept (the regression this
	// work order fixes — the state used to `continue` past the whole line).
	onDelimiter := "```- [high] app/fence.go:9 - the bullet shares the delimiter line\n"
	findings, _ := parseFindingsIn(onDelimiter, nil)
	if len(findings) != 1 || findings[0].Path != "app/fence.go" {
		t.Errorf("a finding on a delimiter line = %+v, want it kept", findings)
	} else if findings[0].fenced {
		t.Errorf("a finding on the opening delimiter line was marked fenced: %+v", findings[0])
	}
	// A finding written on the line that closes a fence is judged with the
	// state the line opens — the fence — so it is quoted text and the filters
	// read it as such; a bullet on an opening delimiter line is prose, and
	// nothing unfenced is ever dropped. The judgement is asserted through the
	// fenced flag parseFindingsIn sets, because a witness answer that carried
	// the same bullet would be read by its (possibly desynced) state rather
	// than by the rule under test.
	// (A bullet on the closing delimiter line is judged with the state the line
	// opens — the fence — so it is fenced text; the flag is what parseFindingsIn
	// sets for that line, and the test pins the two states a delimiter line can
	// be read in.)
	closer := parseProbeFinding("- [high] app/closed.go:9 - on the closing delimiter line", true)
	if !closer.fenced {
		t.Errorf("a finding on a closing delimiter line is not read as fenced text: %+v", closer)
	}
	opener := parseProbeFinding("```- [high] app/fence.go:9 - the bullet shares the delimiter line", false)
	if opener.fenced || dropQuotedFinding(opener, nil) {
		t.Errorf("a finding on an opening delimiter line was dropped: %+v", opener)
	}
	// A tilde fence works the same way, and its findings are fenced too.
	if findings, _ := parseFindingsIn("~~~\n- [high] app/zip/queue.go:42 - x\n~~~\n", map[string]bool{"app/z.go": true}); len(findings) != 0 {
		t.Errorf("tilde fence = %+v, want none", findings)
	}
	// The markers are not interchangeable: a backtick fence is not closed by
	// a tilde run. The rule is pinned calls to fenceRun.closes, not on a parse,
	// because the same parse cannot tell a run that failed to close the fence
	// from one that closed it and so left the following delimiter to open a
	// new one — what a syntactic parse of a quoted diff can see is stated in
	// TestParseFindingsFenceDesync.
	compat := fenceRun{open: true, marker: '`', indent: 0, length: 3}
	if compat.closes('~', 0, 3) {
		t.Error("a tilde run closed a backtick fence")
	}
	if compat.closes('`', 0, 2) {
		t.Error("a shorter run closed the fence it cannot close")
	}
	if compat.closes('`', 4, 3) {
		t.Error("a delimiter indented further than the fence closed it")
	}
	if !compat.closes('`', 0, 3) {
		t.Error("a matching run no longer closes the fence it opened")
	}
	// The indent is part of the match, not a bound: a fence opened three
	// spaces in is closed by a delimiter three spaces in, and not by one at
	// the margin (the start of an indented block, in the block-quote case).
	indented := fenceRun{open: true, marker: '`', indent: 3, length: 3}
	if !indented.closes('`', 3, 3) {
		t.Error("an indented fence is no longer closed by its own indentation")
	}
	if indented.closes('`', 0, 3) {
		t.Error("a delimiter at another indentation closed an indented fence")
	}
}

// parseProbeFinding reads one bullet the way parseFindingsIn would read it
// on a line whose fence state is already settled, so the tests can assert the
// judgement itself (fenced, and what dropQuotedFinding then does with it)
// without depending on how an answer happens to leave the state.
func parseProbeFinding(line string, fenced bool) Finding {
	f, ok := parseFinding(line)
	if !ok {
		return Finding{}
	}
	f.fenced = fenced
	return f
}

// TestParseFindingsFenceDesync: an echoed or unterminated fence must not
// desync the state for the prose that follows (issue #224 follow-up finding
// review.go:118). Only a run at least as long as the one that opened the
// fence, of the same marker character and not indented further, closes it;
// everything else is text inside the fence.
func TestParseFindingsFenceDesync(t *testing.T) {
	known := map[string]bool{"app/queue/main.go": true}
	// The desynced fence: an indented line inside the fence opens a backtick
	// run, and a later bare delimiter leaves a fence OPEN over the prose.
	// Before the fix, inFence was false again at the prose line only because
	// the indented line had toggled it — a state the quoted line chose.
	desynced := strings.Join([]string{
		"```",
		"    ```go",
		"        code from a quoted diff hunk",
		"```",
		"## Findings",
		"- [high] app/queue/main.go:42 - the mutex is released twice",
	}, "\n") + "\n"
	findings, _ := parseFindingsIn(desynced, known)
	if len(blockingFindings(findings)) != 1 {
		t.Errorf("prose after a desynced fence = %+v, want the HIGH kept and blocking", findings)
	}
	// The consequence the follow-up finding named: before the fix the same
	// answer, with the same diff, left blockingFindings empty, so the review
	// plan of a finished review returned the pass label (a false pass that
	// silently merges the HIGH) instead of the fix run the finding demands.
	// blockingFindings is what reviewPlan reads to choose between them, so
	// asserting it is the plan-level assertion.
	for _, f := range findings {
		if f.fenced && !strings.Contains(desynced, f.Text) {
			t.Errorf("a finding the fence does not contain was marked fenced: %+v", f)
		}
	}
	// The same answer with the indented line removed parses the same way:
	// the quoted hunk changes nothing about the prose.
	clean := strings.Join([]string{
		"```",
		"        code from a quoted diff hunk",
		"```",
		"## Findings",
		"- [high] app/queue/main.go:42 - the mutex is released twice",
	}, "\n") + "\n"
	plain, _ := parseFindingsIn(clean, known)
	if len(blockingFindings(plain)) != 1 || plain[0].fenced {
		t.Errorf("a plain fence changed the prose result = %+v", plain)
	}

	// An unterminated fence: the closing delimiter the reviewer forgot leaves
	// the fence open, and the section written after it must still parse.
	unterminated := "```\nquoted output the reviewer never closed\n" +
		"## Findings\n\n- [high] app/queue/main.go:42 - the mutex is released twice\n"
	// A fence opened and never closed contains everything after it, so the
	// section is read as fenced text — but it is not a quote: the path filter
	// is not applied to an unconfirmed region, so the finding is kept and
	// blocks even though its path is one the diff does not touch. That is the
	// false pass this rule removes: with the filters read as a quote, the
	// finding was dropped and the review ended with the pass label.
	open, _ := parseFindingsIn(unterminated, known)
	if len(blockingFindings(open)) != 1 {
		t.Errorf("an unterminated fence swallowed the prose that follows: %+v", open)
	} else if !open[0].fenced || open[0].quoted {
		t.Errorf("a finding after an unterminated fence = %+v, want it fenced and not read as a quote", open[0])
	}
	// The same answer about a path the diff does not touch is kept too: the
	// unterminated fence never earned the right to call it a quote.
	untouched := "```\nquoted output the reviewer never closed\n" +
		"## Findings\n\n- [high] app/elsewhere/other.go:7 - a finding about another change\n"
	if findings, _ := parseFindingsIn(untouched, known); len(blockingFindings(findings)) != 1 {
		t.Errorf("an unterminated fence dropped a finding on an untouched path: %+v", findings)
	}
	// A bullet inside the fence, on a path the diff does not touch, is the
	// quote the rule is for — but only once the reviewer closed the fence.
	// The two states differ in exactly that, with everything else equal.
	inFence := "```\n- [high] app/elsewhere/other.go:7 - a finding about another change"
	if findings, _ := parseFindingsIn(inFence+"\n", known); len(blockingFindings(findings)) != 1 {
		t.Errorf("an unterminated fence dropped a fenced finding: %+v", findings)
	}
	if findings, _ := parseFindingsIn(inFence+"\n```\n", known); len(findings) != 0 {
		t.Errorf("a closed fence no longer drops an untouched path: %+v", findings)
	}
	// With no diff in hand the unterminated block is kept as well: a failed
	// diff read never loses a finding.
	if findings, _ := parseFindingsIn(unterminated, nil); len(blockingFindings(findings)) != 1 {
		t.Errorf("an unterminated fence swallowed the prose with no diff known: %+v", findings)
	}

	// A shorter run inside the fence does not close the longer one that
	// opened it.
	shorter := "````\n```\n## Findings\n\n- [high] app/queue/main.go:42 - the mutex is released twice\n"
	if findings, _ := parseFindingsIn(shorter, known); len(blockingFindings(findings)) != 1 {
		t.Errorf("a shorter run closed the fence: %+v", findings)
	}
	// A four-space indentation is an indented code block, not a fence: it
	// neither opens nor closes one.
	indented := "    ```\n## Findings\n\n- [high] app/queue/main.go:42 - the mutex is released twice\n"
	if findings, _ := parseFindingsIn(indented, known); len(blockingFindings(findings)) != 1 {
		t.Errorf("an indented code block toggled the fence state: %+v", findings)
	}
	// The same indented line inside a fence does not close it either.
	insideIndented := "```\n    ```\n## Findings\n\n- [high] app/queue/main.go:42 - the mutex is released twice\n"
	if findings, _ := parseFindingsIn(insideIndented, known); len(blockingFindings(findings)) != 1 {
		t.Errorf("an indented delimiter inside a fence closed it: %+v", findings)
	}
}

// TestContractExampleConstants: the phantom's identity is the contract's own
// first bullet, so the constants dropQuotedFinding compares against must
// stay in step with reviewOutputContract and with what a parse of it yields.
func TestContractExampleConstants(t *testing.T) {
	field := fmt.Sprintf("[%s] %s:%d - %s",
		contractExampleSeverity, contractExamplePath, contractExampleLine, contractExampleText)
	if !strings.Contains(reviewOutputContract, field) {
		t.Fatalf("reviewOutputContract no longer holds %q", field)
	}
	f, ok := parseFinding("- " + field)
	if !ok {
		t.Fatalf("the contract's example bullet no longer parses: %q", field)
	}
	if f.Severity != contractExampleSeverity || f.Path != contractExamplePath ||
		f.Line != contractExampleLine || f.Text != contractExampleText {
		t.Fatalf("a parse of the contract's example = %+v, want the constants", f)
	}
}

// TestDoneWhenFindingIsActionable pins the format the review brief's
// acceptance-criteria bullet tells the reviewer to quote (issue #136):
// "[high] done-when: <bullet>". It carries no path:line location, so the
// section pass alone rejects it — but parseFindings runs the inline pass
// over every line, inside the findings section too, and textOnlyFinding
// reads it as a recognized high finding. Either way the reviewer's answer
// carries it, and it blocks.
func TestDoneWhenFindingIsActionable(t *testing.T) {
	bullet := "TestIssueReady bullets unmet"
	for name, answer := range map[string]string{
		"in the findings section": "## Findings\n\n- [high] done-when: " + bullet + "\n",
		"inline in prose":         "The acceptance criteria are unmet: [high] done-when: " + bullet + "\n",
	} {
		findings, _ := parseFindings(answer)
		if len(findings) != 1 {
			t.Fatalf("%s: got %d findings, want 1: %+v", name, len(findings), findings)
		}
		f := findings[0]
		if asciiLower(f.Severity) != "high" || !f.Recognized || f.Text != "done-when: "+bullet {
			t.Fatalf("%s: finding = %+v; want a recognized high done-when finding", name, f)
		}
		if got := blockingFindings(findings); len(got) != 1 {
			t.Fatalf("%s: blockingFindings = %+v; want the done-when finding to block", name, got)
		}
	}
}

// reviewHostileFindings is a finding set a hostile reviewer could have
// written: markdown, mentions and shell in the cells, and a path that must
// never reach the fix command line.
func reviewHostileFindings() []Finding {
	return []Finding{
		{Severity: "high", Path: "app/x/main.go", Line: 42, Text: "| pipes | <!-- html --> and @alice and [link](https://x)", Recognized: true},
		{Severity: "medium", Path: "a$(reboot).go", Line: 7, Text: "missing error check", Recognized: true},
	}
}

// reviewCappedServer is review-report's API when the pull request's comment
// listing cannot be read to its end (issue #416): every comments page claims
// another one, so getList stops at maxListPages with the page-cap error. The
// pull request route answers its diff when asked for it, so the findings in
// the answer are judged against a real diff. The counter is how many comment
// pages were asked for.
func reviewCappedServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var pages atomic.Int32
	diff := "diff --git a/app/x/main.go b/app/x/main.go\n" +
		"--- a/app/x/main.go\n+++ b/app/x/main.go\n" +
		"@@ -1,2 +1,3 @@\n context\n-removed\n+added\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveRepoID(w, r) {
			return
		}
		switch {
		case r.URL.Path == stubRepoPath+"/issues/7/comments":
			n := pages.Add(1)
			w.Header().Set("Link", idNextLink(r, fmt.Sprintf("/issues/7/comments?per_page=100&page=%d", n+1)))
			_, _ = io.WriteString(w, `[{"id":1,"body":"a comment","user":{"login":"alice","type":"User"},"author_association":"MEMBER"}]`)
		case r.URL.Path == stubRepoPath+"/pulls/7":
			if strings.Contains(r.Header.Get("Accept"), "diff") {
				_, _ = io.WriteString(w, diff)
				return
			}
			_, _ = io.WriteString(w, `{"number":7,"title":"t","state":"open","draft":false}`)
		default:
			http.Error(w, "not served: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, &pages
}

// TestReviewPlanDegradesOnListPageCap is issue #416's review-report half: the
// continuation count is read from the pull request's comment listing, and a
// thread longer than maxListPages pages made reviewPlan return getList's raw
// page-cap error, so review-report exited 1 before its gh pr comment and the
// findings — the only place the blocking severities and file:lines exist —
// never posted. The listing is truncated, not broken: the findings comment
// must still be built, on the budget-spent report-only path (a thread that
// long has certainly spent the continuations), with no error.
func TestReviewPlanDegradesOnListPageCap(t *testing.T) {
	hostile := reviewHostileFindings()
	t.Run("the plan reports the findings at the budget", func(t *testing.T) {
		server, pages := reviewCappedServer(t)
		ghEnv(t, server)
		action, body, used, err := reviewPlan(newGitHub(), 7, Result{Stop: stopDone, Model: "deepseek-ai/deepseek-v4.1-flash"},
			true, hostile, true)
		if err != nil {
			t.Fatalf("reviewPlan on a capped listing: %v, want the findings comment", err)
		}
		if action != "report-only" || used != maxContinuations {
			t.Fatalf("action = %q, used = %d; want report-only/%d on a capped listing", action, used, maxContinuations)
		}
		if strings.HasPrefix(body, command+" fix:") {
			t.Errorf("the capped listing starts a fix run: %q", body)
		}
		for _, want := range []string{
			fmt.Sprintf("automatic fix budget of %d", maxContinuations),
			"| severity | location | finding |",
			"app/x/main.go:42",
			"a$(reboot).go:7",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the findings comment is missing %q: %q", want, body)
			}
		}
		assertReviewBodyClean(t, body)
		if n := pages.Load(); n != maxListPages {
			t.Errorf("pages requested = %d, want exactly maxListPages (%d)", n, maxListPages)
		}
	})
	t.Run("review-report exits 0 with the findings comment written", func(t *testing.T) {
		oldArgs := os.Args
		t.Cleanup(func() { os.Args = oldArgs })
		os.Args = []string{"harness", "review-report", "7"}
		server, _ := reviewCappedServer(t)
		ghEnv(t, server)
		writeReviewResult(t, stopDone, "## Findings\n\n- [high] app/x/main.go:42 - the mutex is released twice\n")
		reviewLedger(t)
		comment := filepath.Join(t.TempDir(), "review-comment.md")
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatalf("review-report on a capped listing: %v, want exit 0", err)
		}
		if out != fmt.Sprintf("action=report-only\nused=%d\nblocking=1\n", maxContinuations) {
			t.Errorf("stdout = %q; want the report-only budget-spent line", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatalf("the findings comment was not written: %v", err)
		}
		if strings.HasPrefix(string(data), command+" fix:") {
			t.Errorf("the findings comment starts a fix run: %q", data)
		}
		for _, want := range []string{"automatic fix budget", "| severity | location | finding |", "app/x/main.go:42"} {
			if !strings.Contains(string(data), want) {
				t.Errorf("the findings comment is missing %q: %q", want, data)
			}
		}
		assertReviewBodyClean(t, string(data))
	})
}

func TestReviewPlan(t *testing.T) {
	hostile := reviewHostileFindings()
	// No result at all: report-only, nothing concluded, no label, no fix.
	server := commentsServer(t, reportComments(1), http.StatusOK)
	ghEnv(t, server)
	t.Setenv("LEDGER_PATH", "")
	action, body, used, err := reviewPlan(newGitHub(), 7, Result{}, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if action != "report-only" || used != 0 {
		t.Fatalf("no result: action = %q, used = %d", action, used)
	}
	if !strings.Contains(body, "produced no result") || strings.Contains(body, reviewPassedLabel) || strings.HasPrefix(body, command+" fix:") {
		t.Errorf("no-result body wrong: %q", body)
	}
	assertReviewBodyClean(t, body)

	// The whole decision table over a finished and an unfinished review.
	reports := map[int]int{1: 0, 2: 1} // comment count -> continuations used
	for n, wantUsed := range reports {
		server := commentsServer(t, reportComments(n), http.StatusOK)
		ghEnv(t, server)
		action, body, used, err := reviewPlan(newGitHub(), 7, Result{Stop: stopDone, Model: "deepseek-ai/deepseek-v4.1-flash"},
			true, hostile, true)
		if err != nil {
			t.Fatal(err)
		}
		if action != "fix" || used != wantUsed {
			t.Fatalf("reports=%d: action = %q, used = %d, want fix/%d", n, action, used, wantUsed)
		}
		if !strings.HasPrefix(body, "/codeaf fix: 2 blocking findings - high app/x/main.go:42; medium (unsafe path):7\n") {
			t.Errorf("fix body does not start with the command (reports=%d): %q", n, body)
		}
		if !strings.Contains(body, fmt.Sprintf("continuation %d of %d", wantUsed+1, maxContinuations)) {
			t.Errorf("fix body does not name the continuation (reports=%d): %q", n, body)
		}
		if !strings.Contains(body, "| severity | location | finding |") {
			t.Errorf("fix body carries no findings table: %q", body)
		}
		assertReviewBodyClean(t, body)
	}
	// The hostile path never reaches the fix command line unfiltered.
	if strings.Contains(body, "$(reboot)") {
		t.Errorf("the fix command line carries a hostile path: %q", body)
	}

	// Cap reached: report-only with the cap message, no command line.
	server = commentsServer(t, reportComments(3), http.StatusOK)
	ghEnv(t, server)
	action, body, used, err = reviewPlan(newGitHub(), 7, Result{Stop: stopDone}, true, hostile, true)
	if err != nil {
		t.Fatal(err)
	}
	if action != "report-only" || used != 2 {
		t.Fatalf("cap: action = %q, used = %d", action, used)
	}
	if strings.HasPrefix(body, command+" fix:") {
		t.Errorf("the cap body starts a fix run: %q", body)
	}
	for _, want := range []string{"automatic fix budget of 2", "/codeaf comment", "| severity | location | finding |"} {
		if !strings.Contains(body, want) {
			t.Errorf("cap body missing %q: %q", want, body)
		}
	}
	assertReviewBodyClean(t, body)

	// A finished review with an unrecognized severity: report-only, the raw
	// word shown, label withheld.
	server = commentsServer(t, reportComments(1), http.StatusOK)
	ghEnv(t, server)
	mixed := []Finding{{Severity: "critical", Path: "app/z/z.go", Line: 1, Text: "scale unknown", Recognized: false}}
	action, body, _, err = reviewPlan(newGitHub(), 7, Result{Stop: stopDone}, true, mixed, true)
	if err != nil {
		t.Fatal(err)
	}
	if action != "report-only" {
		t.Fatalf("unrecognized: action = %q", action)
	}
	if !strings.Contains(body, "withheld") || !strings.Contains(body, "`critical`") {
		t.Errorf("unrecognized body missing the note or the raw word: %q", body)
	}
	if strings.Contains(body, reviewPassedLabel+" label was added") {
		t.Errorf("unrecognized body added the label: %q", body)
	}
	assertReviewBodyClean(t, body)

	// A clean pass: the label, the table only when low findings exist.
	server = commentsServer(t, reportComments(1), http.StatusOK)
	ghEnv(t, server)
	action, body, _, err = reviewPlan(newGitHub(), 7, Result{Stop: stopDone}, true, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if action != "label" || !strings.Contains(body, reviewPassedLabel) {
		t.Fatalf("clean pass: action = %q, body = %q", action, body)
	}
	if strings.Contains(body, "| severity | location | finding |") {
		t.Errorf("a clean pass must carry no table: %q", body)
	}
	low := []Finding{{Severity: "low", Path: "docs/readme.md", Line: 3, Text: "typo", Recognized: true}}
	_, body, _, err = reviewPlan(newGitHub(), 7, Result{Stop: stopDone}, true, low, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, reviewPassedLabel) || !strings.Contains(body, "| severity | location | finding |") {
		t.Errorf("pass with low findings: table or label missing: %q", body)
	}

	// An unfinished review with blocking findings starts a fix run (issue
	// #49): findings parsed from either source act, whatever the stop reason.
	server = commentsServer(t, reportComments(1), http.StatusOK)
	ghEnv(t, server)
	action, body, used, err = reviewPlan(newGitHub(), 7, Result{Stop: stopDeadline}, true, hostile, true)
	if err != nil {
		t.Fatal(err)
	}
	if action != "fix" || used != 0 {
		t.Fatalf("early stop with blocking findings: action = %q, used = %d", action, used)
	}
	if !strings.HasPrefix(body, command+" fix: 2 blocking findings") || !strings.Contains(body, "`deadline`") {
		t.Errorf("early-stop fix body wrong: %q", body)
	}
	assertReviewBodyClean(t, body)

	// The same, when the result envelope is missing entirely and the
	// findings came from the run record: they still act.
	server = commentsServer(t, reportComments(1), http.StatusOK)
	ghEnv(t, server)
	action, body, _, err = reviewPlan(newGitHub(), 7, Result{}, false, hostile, false)
	if err != nil {
		t.Fatal(err)
	}
	if action != "fix" {
		t.Fatalf("no result with blocking findings: action = %q", action)
	}
	assertReviewBodyClean(t, body)

	// An unfinished review whose findings are not blocking stays advisory:
	// the findings shown, no label and no fix — but the report says the run
	// ended early after finishing its review, not that nothing was found
	// (issue #111).
	server = commentsServer(t, reportComments(1), http.StatusOK)
	ghEnv(t, server)
	lowOnly := []Finding{{Severity: "low", Path: "docs/readme.md", Line: 3, Text: "typo", Recognized: true}}
	action, body, _, err = reviewPlan(newGitHub(), 7, Result{Stop: stopDeadline}, true, lowOnly, false)
	if err != nil {
		t.Fatal(err)
	}
	if action != "report-only" {
		t.Fatalf("early stop with low findings: action = %q", action)
	}
	for _, want := range []string{"`deadline`", "after finishing its review", "| severity | location | finding |"} {
		if !strings.Contains(body, want) {
			t.Errorf("advisory early-stop body missing %q: %q", want, body)
		}
	}
	if strings.Contains(body, "No findings section") {
		t.Errorf("a finished review must not read as one with no findings: %q", body)
	}
	if strings.Contains(body, reviewPassedLabel) || strings.HasPrefix(body, command+" fix:") {
		t.Errorf("an advisory review must not label or fix: %q", body)
	}
	assertReviewBodyClean(t, body)

	// An unfinished review with no findings anywhere: advisory, and the
	// verdict says neither source carried findings.
	server = commentsServer(t, reportComments(1), http.StatusOK)
	ghEnv(t, server)
	action, body, _, err = reviewPlan(newGitHub(), 7, Result{Stop: stopIncomplete}, true, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if action != "report-only" {
		t.Fatalf("early stop without findings: action = %q", action)
	}
	if !strings.Contains(body, "no findings section and no inline findings were found in the answer or the run record") {
		t.Errorf("advisory body does not say where it looked: %q", body)
	}
	assertReviewBodyClean(t, body)

	// A failing comments fetch is an error, like continuePlan.
	server = commentsServer(t, nil, http.StatusInternalServerError)
	ghEnv(t, server)
	if _, _, _, err := reviewPlan(newGitHub(), 7, Result{Stop: stopDone}, true, hostile, true); err == nil {
		t.Error("a failing comment fetch must return an error")
	}
}

// TestReviewPlanWithholdsPassLabelWhenWorkerRunNotDone covers the review
// half of issue #237, as the maintainer triage narrowed it: the queue no
// longer re-picks a capped or unknown-stop pull request into a review run
// (#217 closed that path), so what decides whether a clean review may stamp
// codeaf-review-passed on work whose worker run stopped early is the pull
// request itself. An early-stopped run publishes its patch as a draft, and
// only a `done` stop (pr-mode) marks a pull request ready for review — a
// draft under review means the work is not known finished, and no clean
// review over the diff alone may say it is. The label is withheld exactly
// as an unrecognized severity withholds it: report-only, verdict naming why.
func TestReviewPlanWithholdsPassLabelWhenWorkerRunNotDone(t *testing.T) {
	// A clean review: the reviewer stopped done and no findings were parsed
	// from its answer or the run record.
	result := Result{Stop: stopDone, Model: "deepseek-ai/deepseek-v4.1-flash"}

	// The pull request whose worker run stopped early: it stays a draft
	// (pr-mode prints ready=false for every stop that is not done), and the
	// review that comes back clean must not add the pass label to it.
	draft := reviewServer(t, map[string]string{
		"/repos/RichardoC/factory-repo/pulls/7": `{"number":7,"title":"t","state":"open","draft":true}`,
	}, "", nil)
	ghEnv(t, draft)
	action, body, used, err := reviewPlan(newGitHub(), 7, result, true, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if action != "report-only" || used != 0 {
		t.Fatalf("draft: action = %q, used = %d, want report-only/0", action, used)
	}
	for _, want := range []string{"still a draft", "stopped early", "not known to be finished"} {
		if !strings.Contains(body, want) {
			t.Errorf("draft body does not say %q: %q", want, body)
		}
	}
	if strings.Contains(body, reviewPassedLabel) || strings.Contains(body, "merge with confidence") {
		t.Errorf("draft body still grants the pass label: %q", body)
	}
	assertReviewBodyClean(t, body)

	// The same clean review on a pull request that was published ready —
	// the worker run ended done — still labels, so the withhold is not a
	// blanket one.
	ready := commentsServer(t, reportComments(1), http.StatusOK)
	ghEnv(t, ready)
	action, body, used, err = reviewPlan(newGitHub(), 7, result, true, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if action != "label" || used != 0 {
		t.Fatalf("ready: action = %q, used = %d, want label/0", action, used)
	}
	if !strings.Contains(body, reviewPassedLabel) {
		t.Errorf("ready body does not add the pass label: %q", body)
	}

	// The pull request fetch is decisive: a failing one is an error like a
	// failing comments fetch, never a silent label.
	failing := reviewServer(t, nil, "", nil)
	ghEnv(t, failing)
	if _, _, _, err := reviewPlan(newGitHub(), 7, result, true, nil, false); err == nil {
		t.Error("a failing pull request fetch must return an error")
	}
}

// TestReviewPlanWithdrawsPassLabelOnEveryNonPassingOutcome pins the plan half
// of the label contract of issue #5: `label` is the only action that may
// leave codeaf-review-passed on the pull request, so every report-only path
// — no result at all, an early stop, an unrecognized severity, a draft, and
// blocking findings with the fix budget spent — must return anything but
// `label`. The workflow halves of the contract (the withdrawal calls) are
// pinned in TestWorkflowWithdrawsReviewPassedLabel.
func TestReviewPlanWithdrawsPassLabelOnEveryNonPassingOutcome(t *testing.T) {
	hostile := reviewHostileFindings()
	mixed := []Finding{{Severity: "critical", Path: "app/z/z.go", Line: 1, Text: "scale unknown", Recognized: false}}
	done := Result{Stop: stopDone, Model: "deepseek-ai/deepseek-v4.1-flash"}
	plan := func(t *testing.T, result Result, have bool, findings []Finding, sawSection bool) string {
		t.Helper()
		action, _, _, err := reviewPlan(newGitHub(), 7, result, have, findings, sawSection)
		if err != nil {
			t.Fatalf("reviewPlan: %v", err)
		}
		return action
	}
	nonPassing := []struct {
		name   string
		server func(t *testing.T)
		result Result
		have   bool
		find   []Finding
		saw    bool
	}{
		{"no result", func(t *testing.T) { ghEnv(t, commentsServer(t, reportComments(1), http.StatusOK)) }, Result{}, false, nil, false},
		{"early stop", func(t *testing.T) { ghEnv(t, commentsServer(t, reportComments(1), http.StatusOK)) }, Result{Stop: stopDeadline}, true, nil, false},
		{"unrecognized severity", func(t *testing.T) { ghEnv(t, commentsServer(t, reportComments(1), http.StatusOK)) }, done, true, mixed, true},
		{"draft", func(t *testing.T) {
			ghEnv(t, reviewServer(t, map[string]string{
				"/repos/RichardoC/factory-repo/pulls/7": `{"number":7,"title":"t","state":"open","draft":true}`,
			}, "", nil))
		}, done, true, nil, false},
		{"cap with blocking findings", func(t *testing.T) { ghEnv(t, commentsServer(t, reportComments(3), http.StatusOK)) }, done, true, hostile, true},
	}
	for _, tc := range nonPassing {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tc.server(t)
			if action := plan(t, tc.result, tc.have, tc.find, tc.saw); action == "label" {
				t.Errorf("%s: action = %q; the pass label must be withdrawn, so every outcome but label must not be it", tc.name, action)
			}
		})
	}
	// The control: a clean pass over a ready pull request is still the one
	// `label` outcome, so the table above discriminates rather than bans.
	t.Run("clean pass labels", func(t *testing.T) {
		ghEnv(t, commentsServer(t, reportComments(1), http.StatusOK))
		if action := plan(t, done, true, nil, false); action != "label" {
			t.Errorf("clean pass: action = %q, want label", action)
		}
	})
}

// TestReviewTableUnlocated renders the findings table for an unlocated
// finding — the inline form (issue #49) whose path and line are empty — and
// pins the whole table, because that cell used to render as `:0` (issue
// #190): a location a fix run and a reader both read, naming no file. The
// word `unlocated` is the explicit fallback, and no `:0` may appear
// anywhere in the table.
func TestReviewTableUnlocated(t *testing.T) {
	answer := "## Findings\n\n" +
		"- [medium] service.go wc -l 1200, split it\n" +
		"- [high] app/x/main.go:42 - the mutex is released twice\n"
	findings, sawSection := parseFindings(answer)
	if !sawSection || len(findings) != 2 {
		t.Fatalf("parse = %d findings, sawSection %t, want 2/true: %+v", len(findings), sawSection, findings)
	}
	// The first finding carries no location at all; the second does.
	if findings[0].Path != "" || findings[0].Line != 0 {
		t.Fatalf("first finding is not unlocated: %+v", findings[0])
	}
	if findings[1].Path != "app/x/main.go" || findings[1].Line != 42 {
		t.Fatalf("second finding lost its location: %+v", findings[1])
	}

	table := reviewTable(findings)
	want := "| severity | location | finding |\n|---|---|---|\n" +
		"| `medium` | unlocated | service.go wc -l 1200, split it |\n" +
		"| `high` | app/x/main.go:42 | the mutex is released twice |\n"
	if table != want {
		t.Errorf("unlocated table =\n%q\nwant\n%q", table, want)
	}
	if strings.Contains(table, ":0") {
		t.Errorf("the table renders :0 for an unlocated finding: %q", table)
	}
	assertReviewBodyClean(t, table)

	// The whole comment — table, command line and footer — carries no `:0`.
	t.Setenv("RESULT_PATH", "")
	t.Setenv("LEDGER_PATH", "")
	t.Setenv("TRAJECTORY_PATH", "")
	blocking := blockingFindings(findings)
	if len(blocking) != 2 {
		t.Fatalf("blocking = %d findings, want 2: %+v", len(blocking), blocking)
	}
	body := reviewFixBody(blocking, findings, 0, "zai-org/glm-5.3-flash", stopDone)
	if !strings.Contains(body, "| `medium` | unlocated |") {
		t.Errorf("the fix comment does not name the unlocated finding: %q", body)
	}
	if strings.Contains(body, ":0") {
		t.Errorf("the fix comment renders :0: %q", body)
	}
	assertReviewBodyClean(t, body)

	// A located finding still renders its own path and line unchanged.
	if got := reviewTable([]Finding{{Severity: stopDone, Path: "a.go", Line: 7}}); !strings.Contains(got, "| a.go:7 |") {
		t.Errorf("a located finding stopped rendering its location: %q", got)
	}

	// The parser is the only thing that builds findings, and it builds two
	// shapes: a location — a path and a line counting from one, both set —
	// or the inline form with neither. `parseFinding` refuses a path that
	// is empty (i > 0), refuses a line that does not count from one (n > 0)
	// and refuses a zero line, so no parse can yield a path with line zero
	// and none can yield a bare line (issue #190 review). The renderer keys
	// on the path alone; the line alone never decides.
	located := []string{"app/x/main.go:9", "0:1", "README.md:1"}
	for _, loc := range located {
		got, ok := parseFinding("[low] " + loc + " - text")
		if !ok {
			t.Errorf("parseFinding(%q) dropped a location-shaped finding", "[low] "+loc+" - text")
			continue
		}
		if got.Path == "" || got.Line <= 0 {
			t.Errorf("parseFinding(%q) built an incomplete location: %+v", "[low] "+loc+" - text", got)
		}
	}
	// A location the parser refuses — an empty path, a zero line — is not
	// read as a located finding; the text-only fallback stores it as the
	// inline form, with no path and no line, and it renders unlocated.
	for _, loc := range []string{":0", "a.go:0", ":"} {
		line := "[low] " + loc + " - text"
		if got, ok := parseFinding(line); ok {
			t.Errorf("parseFinding(%q) read a refused location as a location: %+v", line, got)
		}
		var f Finding
		for _, g := range parseInlineFindings(line) {
			if g.Path == "" {
				f = g
			}
		}
		if f.Path != "" || f.Line != 0 {
			t.Errorf("parseInlineFindings(%q) did not store it as the inline form: %+v", line, f)
		}
		if got := reviewLocation(f); got != "unlocated" {
			t.Errorf("reviewLocation(%q finding) = %q, want unlocated", line, got)
		}
	}

	// A half-built finding reaches the renderer only straight through. A
	// path that names no file (":" or digits) and an empty path never render
	// a location; a real path with no line keeps the path, so a file-level
	// finding still says which file, and never renders `:0`.
	for _, tc := range []struct {
		f    Finding
		want string
	}{
		{Finding{Severity: "low", Path: ":", Line: 0}, "unlocated"},  // an empty path with a zero line
		{Finding{Severity: "low", Path: "a.go", Line: 0}, "a.go"},    // a path with no line at all
		{Finding{Severity: "low", Path: "a.go", Line: -1}, "a.go"},   // a negative line is not a line
		{Finding{Severity: "high", Path: "", Line: 12}, "unlocated"}, // an unlocated finding a caller filled a line into
		{Finding{Severity: "low", Path: "0", Line: 0}, "unlocated"},  // `0:1`-shaped text names no file
	} {
		if cell := reviewLocation(tc.f); cell != tc.want {
			t.Errorf("reviewLocation(%+v) = %q, want %q", tc.f, cell, tc.want)
		}
		if got := reviewTable([]Finding{tc.f}); strings.Contains(got, ":0") || strings.Contains(got, "a.go:") {
			t.Errorf("reviewTable(%+v) renders a zero or empty line: %q", tc.f, got)
		}
	}

	// Nothing in a rendered findings comment can carry `:0`, whatever the
	// findings are: the location cell is the only place a line number is
	// written.
	all := []Finding{
		{Severity: "medium", Text: "no location at all"},
		{Severity: "low", Path: ":", Line: 0, Text: "a colon and no line"},
		{Severity: "high", Path: "svc.go", Line: 0, Text: "no line number"},
		{Severity: "low", Path: "ok.go", Line: 3, Text: "located"},
	}
	comment := reviewFixBody(blockingFindings(all), all, 0, "zai-org/glm-5.3-flash", stopDone)
	if strings.Contains(comment, ":0") {
		t.Errorf("the fix comment renders :0: %q", comment)
	}
	if !strings.Contains(comment, "| `low` | ok.go:3 | located |") {
		t.Errorf("the located finding lost its location in the comment: %q", comment)
	}
	assertReviewBodyClean(t, comment)
}

// assertReviewBodyClean checks the invariants every review comment must
// hold: no report marker, and no unescaped hostile text.
func assertReviewBodyClean(t *testing.T, body string) {
	t.Helper()
	if strings.Contains(body, reportMarker) {
		t.Errorf("the comment carries the run-report marker: %q", body)
	}
	if strings.Contains(body, "<!--") || strings.Contains(body, "| pipes |") || strings.Contains(body, "@alice") || strings.Contains(body, "[link](") {
		t.Errorf("unescaped hostile text leaked into the comment: %q", body)
	}
}

func TestReviewFooter(t *testing.T) {
	reviewLedger(t)
	t.Setenv("GITHUB_SERVER_URL", "https://github.com")
	ghEnv(t, commentsServer(t, nil, http.StatusOK)) // the artifacts call finds nothing here
	t.Setenv("GITHUB_RUN_ID", "55")
	body := reviewFooter("deepseek-ai/deepseek-v4.1-flash", stopDone)
	for _, want := range []string{
		"`deepseek-ai/deepseek-v4.1-flash`",
		"`done`",
		"metered spend this run: $0.2500",
		"[Workflow run](https://github.com/RichardoC/factory-repo/actions/runs/55)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("footer missing %q: %q", want, body)
		}
	}
	// A missing ledger reads as zero; a real read error as unknown, not as
	// zero; and the stop word is sanitized however it arrives. Since issue
	// #392 a ledger whose lines do not decode is no longer an error: the
	// bad lines are skipped and the spend reads as the lower bound $0.0000.
	t.Setenv("LEDGER_PATH", "")
	if body := reviewFooter("", ""); !strings.Contains(body, "$0.0000") || !strings.Contains(body, "`(unknown)`") {
		t.Errorf("footer with no ledger and no model: %q", body)
	}
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	if err := os.WriteFile(path, []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEDGER_PATH", path)
	if body := reviewFooter("", ""); !strings.Contains(body, "metered spend this run: $0.0000") {
		t.Errorf("footer with a ledger of torn lines: %q", body)
	}
	// A ledger that cannot be read at all still reads as unknown.
	t.Setenv("LEDGER_PATH", filepath.Join(t.TempDir(), "missing-dir", "ledger.jsonl"))
	// The directory does not exist, so readLedger reports it as missing, not
	// broken; use a directory as the path for a real read error.
	path = t.TempDir()
	t.Setenv("LEDGER_PATH", path)
	if body := reviewFooter("", ""); !strings.Contains(body, "metered spend this run: (unknown)") || strings.Contains(body, "$0.0000") {
		t.Errorf("footer with an unreadable ledger: %q", body)
	}
}

func TestReviewerModel(t *testing.T) {
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	// The worker is the base model; the mid model takes the review.
	got, err := reviewerModel(cfg, "zai-org/glm-5.3-flash")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "deepseek-ai/deepseek-v4.1-flash-1017" || got.Role != "mid" {
		t.Errorf("reviewer = %q (role %q), want the mid model", got.ID, got.Role)
	}
	// Whatever the worker is, the reviewer never is.
	for _, m := range cfg.Models {
		got, err := reviewerModel(cfg, m.ID)
		if err != nil {
			t.Fatalf("worker %q: %v", m.ID, err)
		}
		if got.ID == m.ID {
			t.Errorf("reviewer = worker model %q", got.ID)
		}
		if !containsModel(cfg.Models, got.ID) {
			t.Errorf("reviewer %q is not an allowed model", got.ID)
		}
	}
	// A single-model config cannot host a review: it fails loudly.
	single := Config{Models: []Model{{ID: "only/model", Role: "mid", Upstream: "u", Efforts: []string{"high"}}}}
	if _, err := reviewerModel(single, "only/model"); err == nil {
		t.Error("a single-model config must fail, not review itself")
	}
}

func containsModel(models []Model, id string) bool {
	for _, m := range models {
		if m.ID == id {
			return true
		}
	}
	return false
}

func TestRunReviewConfig(t *testing.T) {
	dir := t.TempDir()
	config := `{"api_key":"via-local-proxy","models.tiers.worker":"zai-org/glm-5.3-flash","task.parallel":2,"telemetry":false,"daily_budget_usd":40}`
	if err := os.WriteFile(filepath.Join(dir, "codeaf-config.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_DIR", dir)
	t.Setenv("PROXY_MODELS", "models.json") // the real catalog, next to this file

	out, err := captureStdout(t, runReviewConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("output does not end with a newline: %q", out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if got["models.tiers.worker"] != "deepseek-ai/deepseek-v4.1-flash-1017" {
		t.Errorf("worker tier = %v, want the reviewer model", got["models.tiers.worker"])
	}
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	if !containsModel(cfg.Models, fmt.Sprint(got["models.tiers.worker"])) {
		t.Errorf("worker tier %v is not an allowed model", got["models.tiers.worker"])
	}
	// Every other key is unchanged, in value and in kind.
	if got["task.parallel"] != float64(2) || got["telemetry"] != false || got["daily_budget_usd"] != float64(40) || got["api_key"] != "via-local-proxy" {
		t.Errorf("other keys changed: %v", got)
	}

	// Failures are loud: unreadable config, and no model left to review on.
	t.Setenv("HARNESS_DIR", filepath.Join(dir, "missing"))
	if _, err := captureStdout(t, runReviewConfig); err == nil {
		t.Error("a missing codeaf-config.json must fail")
	}
	single := t.TempDir()
	worker := `{"api_key":"via-local-proxy","models.tiers.worker":"only/model"}`
	if err := os.WriteFile(filepath.Join(single, "codeaf-config.json"), []byte(worker), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(single, "single-models.json"), []byte(`{"limits":{"plan":"big","weekly_credit_usd":20,"run_credit_usd":5,"overage_usd":0},"models":[{"id":"only/model","role":"mid","upstream":"u","efforts":["high"]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_DIR", single)
	t.Setenv("PROXY_MODELS", filepath.Join(single, "single-models.json"))
	if _, err := captureStdout(t, runReviewConfig); err == nil {
		t.Error("a single-model config must fail, not review itself")
	}
}

// TestCapReviewBodyAndDiffText holds the two cap helpers to their contracts
// directly (review follow-up on #38): a body cut never splits a rune and
// always ends in a note that names it, even when the budget is too small for
// any body text, and a re-capped diff keeps the fetch note whatever budget is
// left — including a budget of zero, where the note alone says why.
func TestCapReviewBodyAndDiffText(t *testing.T) {
	t.Run("capReviewBody", func(t *testing.T) {
		if got := capReviewBody("", "pull request body", 100); got != "" {
			t.Errorf("an empty body must stay empty, got %q", got)
		}
		if got := capReviewBody("small body", "pull request body", 100); got != "small body" {
			t.Errorf("a body under the cap must pass through, got %q", got)
		}
		body := strings.Repeat("ab\n", 200)
		got := capReviewBody(body, "pull request body", 100)
		if len(got) != 100 {
			t.Errorf("a cut body is %d bytes, want exactly the cap", len(got))
		}
		if !strings.HasPrefix(got, "ab\n") || !strings.HasSuffix(got, "(pull request body truncated at 100 bytes)\n") {
			t.Errorf("a cut body must keep its head and end in a note naming it, got %q", got)
		}
		// The omission branch: no room even for the truncation note, so the
		// note that names the section is all that survives.
		for _, max := range []int{0, 20} {
			got := capReviewBody(body, "closing issue body", max)
			if got != "\n\n(closing issue body omitted: the brief is over its size limit)\n" {
				t.Errorf("max=%d: a body with no room even for its note must keep the omission note, got %q", max, got)
			}
		}
		// The cut never splits a UTF-8 rune: the multibyte character just
		// before the cut point steps back whole.
		utf := strings.Repeat("x", 90) + "é" + strings.Repeat("y", 200)
		if got := capReviewBody(utf, "pull request body", 100); !utf8.ValidString(got) {
			t.Errorf("a cut body split a UTF-8 rune: %q", got[len(got)-10:])
		}
	})

	t.Run("cappedDiffText", func(t *testing.T) {
		note := diffFetchNote()
		if !strings.Contains(note, "(diff truncated at") {
			t.Fatalf("diffFetchNote changed shape: %q", note)
		}
		// A truncated fetch keeps the fetch note at the end, inside the budget.
		diff := strings.Repeat("+ a hostile diff line\n", 400)
		got := cappedDiffText(diff, 1000, true)
		if len(got) > 1000 {
			t.Errorf("a re-capped diff is %d bytes, budget 1000", len(got))
		}
		if !strings.HasPrefix(got, "+ a hostile diff line\n") || !strings.HasSuffix(got, note) {
			t.Errorf("a re-capped diff must keep its head and end in the fetch note, got %q", got)
		}
		// No room for any diff text: the note alone says why.
		if got := cappedDiffText(diff, len(note), true); got != note {
			t.Errorf("a budget of exactly the note must return the note alone, got %d bytes", len(got))
		}
		if got := cappedDiffText(diff, 0, true); got != note {
			t.Errorf("a zero budget must return the note alone, got %d bytes", len(got))
		}
		// Not truncated: the plain capDiff path, no fetch note appended.
		if got := cappedDiffText(diff, 1000, false); strings.Contains(got, "the rest was not fetched") {
			t.Errorf("a diff the fetch returned whole must not carry the fetch note: %q", got)
		}
		if got := cappedDiffText(diff, len(diff), false); got != diff {
			t.Errorf("a diff under the cap must pass through whole, got %d bytes", len(got))
		}
	})
}

func TestRunReviewBrief(t *testing.T) {
	routes := map[string]string{
		"/repos/RichardoC/factory-repo/pulls/7":           `{"number":7,"title":"Review target","body":"The change.\n\nCloses #3","state":"open","head":{"ref":"codeaf/issue-3-42"},"base":{"ref":"main"}}`,
		"/repos/RichardoC/factory-repo/issues/3":          `{"number":3,"title":"The task","body":"Build the thing the review judges against."}`,
		"/repos/RichardoC/factory-repo/issues/7/comments": `[]`,
	}
	var sawAccept string
	diffBody := "diff --git a/app/x/main.go b/app/x/main.go\n--- a/app/x/main.go\n+++ b/app/x/main.go\n@@ -1,2 +1,3 @@\n context\n-removed\n+added\n"
	server := reviewServer(t, routes, diffBody, &sawAccept)
	ghEnv(t, server)
	t.Setenv("NUMBER", "7")
	briefPath := filepath.Join(t.TempDir(), "nested", "review-brief.md")
	t.Setenv("BRIEF_PATH", briefPath)

	out, err := captureStdout(t, runReviewBrief)
	if err != nil {
		t.Fatal(err)
	}
	brief, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatalf("brief not written: %v", err)
	}
	if string(brief) != out {
		t.Errorf("stdout and BRIEF_PATH differ")
	}
	b := string(brief)
	if sawAccept != "application/vnd.github.v3.diff" {
		t.Errorf("diff fetch sent Accept %q", sawAccept)
	}
	for _, want := range []string{
		"You have ",            // the budget line opens the brief
		"independent reviewer", // the standing instructions
		"# The pull request under review: #7 — Review target", // number and title
		"Head branch `codeaf/issue-3-42`, base branch `main`", // the branches
		"## The issue this PR closes (#3) — The task",         // the linked issue
		"Build the thing the review judges against.",          // the issue's task text
		"# The diff under review",                             // the diff section
		"@@ -1,2 +1,3 @@",                                     // the diff body, hunk and all
		"## The previous CodeAF report",                       // the previous report section
		"# The output the harness parses",                     // the output contract
		"app/zip/queue.go:42",                                 // the concrete example bullet
		"single word `none`",                                  // the clean-review form
		// Issue #189: the reviewer's standing instructions say the PATH
		// harness is the pre-change build, so a harness change is reviewed
		// against a binary built from the diff's tree.
		reviewHarnessPreChangeSentence,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("brief missing %q", want)
		}
	}

	// Issue #224, doc honesty: the fence paragraph in review-instructions.md
	// states the two cases the parser drops a fenced finding in, so parse the
	// answer forms it names and hold the paragraph to what the code does. Each
	// case here is one sentence of that paragraph: the contract echo is dropped
	// with no diff in hand and with one, the path test needs the diff, a fenced
	// finding that is neither case is kept, nothing unfenced is dropped, and a
	// finding in the `## Findings` section is always read.
	diffKnown := map[string]bool{"app/x/main.go": true}
	echo := "- [high] app/zip/queue.go:42 - " + contractExampleText + "\n"
	// The paragraph is the other half of the contract: read it out of the same
	// file runReviewBrief reads, so a later edit that restores the false
	// caveat — `neither case applies when the harness could not read the
	// diff` — fails here instead of shipping. The echo case needs no diff,
	// and the paragraph must say so.
	doc, err := os.ReadFile(filepath.Join(harnessDir(), "review-instructions.md"))
	if err != nil {
		t.Fatalf("review-instructions.md not readable: %v", err)
	}
	for _, want := range []string{
		"which holds whether or not the\nharness could read the diff",     // the echo needs no diff
		"the harness read the diff and the diff does\nnot touch the path", // the path case needs the diff
		"nothing outside a fence is\never dropped",                        // nothing unfenced is dropped
		"`## Findings` section is\nalways read",                           // the section is always read
	} {
		if !strings.Contains(string(doc), want) {
			t.Errorf("the fence paragraph no longer states %q", want)
		}
	}
	if strings.Contains(string(doc), "neither case applies when the\nharness could not read the diff") {
		t.Error("the fence paragraph restores the false no-diff caveat")
	}

	for name, tc := range map[string]struct {
		answer     string
		known      map[string]bool
		wantBlocks int
	}{
		"fenced contract echo, diff not read": {
			"```markdown\n" + echo + "```\n\n## Findings\n\nnone\n", nil, 0},
		"fenced contract echo, diff read": {
			"```markdown\n" + echo + "```\n\n## Findings\n\nnone\n", diffKnown, 0},
		"fenced finding on a path the diff does not touch, diff read": {
			"```\n- [high] app/other/main.go:7 - a real bug in an untouched file\n```\n", diffKnown, 0},
		"fenced finding on a path the diff does not touch, no diff in hand": {
			"```\n- [high] app/other/main.go:7 - a real bug in an untouched file\n```\n", nil, 1},
		"fenced finding on a path the diff changes": {
			"```\n- [high] app/x/main.go:42 - a real bug on a changed path\n```\n", diffKnown, 1},
		"unfenced contract echo": {
			echo, diffKnown, 1},
		"finding in the findings section": {
			"## Findings\n\n- [high] app/x/main.go:42 - a real bug on a changed path\n", nil, 1},
	} {
		findings, _ := parseFindingsIn(tc.answer, tc.known)
		if got := len(blockingFindings(findings)); got != tc.wantBlocks {
			t.Errorf("%s: blockingFindings = %d, want %d: %+v", name, got, tc.wantBlocks, findings)
		}
	}

	// An unterminated fence is not a quote (issue #224 follow up), so it
	// cannot be the reason a report about an unchanged path is dropped: the
	// paragraph says so, and no finding is lost when the closing delimiter
	// never arrives. Every line after the opening marker is judged fenced —
	// that is what a fence that never closes means — so the answer must still
	// carry the finding under the section written after it.
	for name, answer := range map[string]string{
		"section after the fence": "I quoted the hunk:\n```\n+ the hunk\n" +
			"## Findings\n\n- [high] app/other/main.go:7 - a real bug in an untouched file\n",
		"bullet inline after the fence": "I quoted the hunk:\n```\n+ the hunk\n\n" +
			"- [high] app/other/main.go:7 - a real bug in an untouched file\n",
	} {
		findings, sawSection := parseFindingsIn(answer, diffKnown)
		if got := len(blockingFindings(findings)); got != 1 {
			t.Errorf("%s: blockingFindings = %d, want 1: %+v", name, got, findings)
		}
		if name == "section after the fence" && !sawSection {
			t.Errorf("%s: the findings section after an unterminated fence was missed", name)
		}
	}
	// A section header written inside a properly closed fence still opens the
	// section — the reviewer wrote it, and the section is the contract — and
	// (issue #334) it frees the bullets under it from the quoted-path
	// filter: a finding written in the section is always read, so this one,
	// whose path the diff does not touch, is kept and blocks instead of
	// ending the review with a false `codeaf-review-passed`.
	headerInFence := "```\n## Findings\n\n- [high] app/other/main.go:7 - a real bug in an untouched file\n```\n"
	if findings, sawSection := parseFindingsIn(headerInFence, diffKnown); len(blockingFindings(findings)) != 1 || !sawSection {
		t.Errorf("a header inside a closed fence = %+v sawSection=%t, want the section seen and its fenced finding kept", findings, sawSection)
	}
	// The same header inside a fence that never closes is not a quote, so the
	// finding under it is read and blocks.
	headerInOpenFence := "```\n## Findings\n\n- [high] app/other/main.go:7 - a real bug in an untouched file\n"
	if findings, sawSection := parseFindingsIn(headerInOpenFence, diffKnown); len(blockingFindings(findings)) != 1 || !sawSection {
		t.Errorf("a header inside an unterminated fence = %+v sawSection=%t, want the finding read", findings, sawSection)
	}
	// The same header outside any fence opens the section, and its finding is
	// read whatever the diff says: the section is the contract.
	if findings, sawSection := parseFindingsIn("## Findings\n\n- [high] app/other/main.go:7 - a real bug in an untouched file\n", diffKnown); len(findings) != 1 || !sawSection {
		t.Errorf("a section finding outside a fence = %+v sawSection=%t, want it read", findings, sawSection)
	}

	// A failing issue fetch degrades to a note and never fails the brief.
	delete(routes, "/repos/RichardoC/factory-repo/issues/3")
	briefPath = filepath.Join(t.TempDir(), "degraded.md")
	t.Setenv("BRIEF_PATH", briefPath)
	if _, err := captureStdout(t, runReviewBrief); err != nil {
		t.Fatalf("a failed issue fetch must not fail the brief: %v", err)
	}
	data, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "is unavailable") {
		t.Errorf("the degrade note is missing: %q", data)
	}

	// A huge headerless blob is capped as a plain prefix, with the byte-count note.
	big := strings.Repeat("+ line of a hostile diff\n", 4000) // ~96KB
	routes["/repos/RichardoC/factory-repo/issues/3"] = `{"number":3,"title":"The task","body":"x"}`
	ghEnv(t, reviewServer(t, routes, big, nil))
	briefPath = filepath.Join(t.TempDir(), "capped.md")
	t.Setenv("BRIEF_PATH", briefPath)
	if _, err := captureStdout(t, runReviewBrief); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), fmt.Sprintf("(diff truncated at %d bytes)", maxReviewDiffBytes)) {
		t.Errorf("the diff cap note is missing")
	}

	// A multi-file diff past the cap (issue #231) keeps every file header,
	// names the clipped files in the note and points the reviewer at the
	// checkout, so no file drops out of the brief unseen.
	multi := diffFile("app/one.go", 2000) + diffFile("app/two.go", 2000) + diffFile("app/three.go", 2000)
	if len(multi) <= maxReviewDiffBytes {
		t.Fatalf("test diff must exceed the cap: %d bytes", len(multi))
	}
	ghEnv(t, reviewServer(t, routes, multi, nil))
	briefPath = filepath.Join(t.TempDir(), "multi-capped.md")
	t.Setenv("BRIEF_PATH", briefPath)
	if _, err := captureStdout(t, runReviewBrief); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"diff --git a/app/one.go b/app/one.go",
		"diff --git a/app/two.go b/app/two.go",
		"diff --git a/app/three.go b/app/three.go",
		"read the full content of app/one.go, app/two.go, app/three.go on the checked-out PR head branch",
	} {
		if !strings.Contains(string(data), p) {
			t.Errorf("the capped brief lost %q", p)
		}
	}

	// Issue #38: a 65000-byte PR body and a 65000-byte closing-issue body
	// beside a ~59 KB diff must not push the diff and the output contract past
	// the whole-brief cap. Each body is capped on its own with a note naming
	// it, the diff stays whole (it fits under its own cap), and the contract
	// survives.
	padTo := func(prefix string, n int) string {
		s := strings.Repeat(prefix, n/len(prefix)+1)
		return s[:n]
	}
	prText := "Closes #3\n\n" + padTo("pr body line\n", 65000-len("Closes #3\n\n"))
	issueText := padTo("issue body line\n", 65000)
	bigDiff := diffFile("app/huge.go", 2500)
	if len(bigDiff) >= maxReviewDiffBytes || len(bigDiff) < 50000 {
		t.Fatalf("test diff is %d bytes, want ~59 KB under the %d cap", len(bigDiff), maxReviewDiffBytes)
	}
	bigRoutes := map[string]string{
		"/repos/RichardoC/factory-repo/pulls/7":           `{"number":7,"title":"Review target","body":` + fmt.Sprintf("%q", prText) + `,"state":"open","head":{"ref":"codeaf/issue-3-42"},"base":{"ref":"main"}}`,
		"/repos/RichardoC/factory-repo/issues/3":          `{"number":3,"title":"The task","body":` + fmt.Sprintf("%q", issueText) + `}`,
		"/repos/RichardoC/factory-repo/issues/7/comments": `[]`,
	}
	ghEnv(t, reviewServer(t, bigRoutes, bigDiff, nil))
	briefPath = filepath.Join(t.TempDir(), "big-bodies.md")
	t.Setenv("NUMBER", "7")
	t.Setenv("BRIEF_PATH", briefPath)
	if _, err := captureStdout(t, runReviewBrief); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	b = string(data)
	if len(b) > maxBriefBytes {
		t.Errorf("brief is %d bytes, cap is %d", len(b), maxBriefBytes)
	}
	if !strings.Contains(b, reviewOutputContract) {
		t.Errorf("the output contract was cut from the brief")
	}
	if !strings.Contains(b, bigDiff) && !strings.Contains(b, "(diff truncated at ") {
		t.Errorf("the diff is neither whole nor noted as truncated")
	}
	for _, name := range []string{"pull request body truncated", "closing issue body truncated"} {
		if !strings.Contains(b, name) {
			t.Errorf("the capped brief does not name the dropped section %q", name)
		}
	}

	// Review follow-up on #38, the overflow branch: four app AGENTS.md
	// sections (each capped at maxAppRulesBytes) in issueRules push the fixed
	// sections past maxBriefBytes even with empty bodies, so the diff gives
	// way. The brief stays within the cap, the output contract survives, the
	// diff carries a truncation note, and both bodies keep a note naming
	// them even though the room the fixed sections leave is far smaller than
	// either body.
	overPR := "Closes #3\n\n" + padTo("pr overflow line\n", 30000-len("Closes #3\n\n"))
	overIssue := "Areas: app/ovfa/queue.go app/ovfb/queue.go app/ovfc/queue.go app/ovfd/queue.go\n\n" +
		padTo("issue overflow line\n", 65000)
	overRoutes := map[string]string{
		"/repos/RichardoC/factory-repo/pulls/7":           `{"number":7,"title":"Review target","body":` + fmt.Sprintf("%q", overPR) + `,"state":"open","head":{"ref":"codeaf/issue-3-42"},"base":{"ref":"main"}}`,
		"/repos/RichardoC/factory-repo/issues/3":          `{"number":3,"title":"The task","body":` + fmt.Sprintf("%q", overIssue) + `}`,
		"/repos/RichardoC/factory-repo/issues/7/comments": `[]`,
	}
	for _, name := range []string{"ovfa", "ovfb", "ovfc", "ovfd"} {
		// ~24 KB each, capped to maxAppRulesBytes in the section: four of
		// them plus the ~59 KB diff overrun maxBriefBytes on their own.
		withAppRules(overRoutes, name, strings.Repeat("rule line for "+name+"\n", 1200))
	}
	ghEnv(t, reviewServer(t, overRoutes, bigDiff, nil))
	briefPath = filepath.Join(t.TempDir(), "overflow.md")
	t.Setenv("NUMBER", "7")
	t.Setenv("BRIEF_PATH", briefPath)
	if _, err := captureStdout(t, runReviewBrief); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	b = string(data)
	if len(b) > maxBriefBytes {
		t.Errorf("overflow brief is %d bytes, cap is %d", len(b), maxBriefBytes)
	}
	if !strings.Contains(b, "## Rules for app/ovfa") {
		t.Errorf("the overflow brief lost the app rules that push the base over the cap")
	}
	if !strings.Contains(b, reviewOutputContract) {
		t.Errorf("the output contract was cut from the overflow brief")
	}
	if !strings.Contains(b, "(diff truncated at ") {
		t.Errorf("the overflow brief does not note the diff truncation")
	}
	for _, name := range []string{"(pull request body ", "(closing issue body "} {
		if !strings.Contains(b, name) {
			t.Errorf("the overflow brief does not name the dropped section %q", name)
		}
	}

	// Review follow-up on #38: a PR with no linked issue gets more than half
	// the room for its body. The body ends in a unique marker and is longer
	// than room/2, so only the whole-body share keeps the marker: the old
	// room/2 share (about 43 KB here) cut the body mid-text with no issue
	// body to blame, losing the marker and adding the truncation note.
	freePR := padTo("free pr body line\n", 58000) + "END-OF-FREE-PR-BODY"
	freeRoutes := map[string]string{
		"/repos/RichardoC/factory-repo/pulls/7":           `{"number":7,"title":"No issue","body":` + fmt.Sprintf("%q", freePR) + `,"state":"open","head":{"ref":"feat"},"base":{"ref":"main"}}`,
		"/repos/RichardoC/factory-repo/issues/7/comments": `[]`,
	}
	ghEnv(t, reviewServer(t, freeRoutes, diffBody, nil))
	briefPath = filepath.Join(t.TempDir(), "free-pr.md")
	t.Setenv("BRIEF_PATH", briefPath)
	if _, err := captureStdout(t, runReviewBrief); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	b = string(data)
	if len(b) > maxBriefBytes {
		t.Errorf("brief is %d bytes, cap is %d", len(b), maxBriefBytes)
	}
	if !strings.Contains(b, "END-OF-FREE-PR-BODY") {
		t.Errorf("the end of the %d-byte PR body was cut: with no linked issue the body must get more than half the room", len(freePR))
	}
	if strings.Contains(b, "pull request body truncated") {
		t.Errorf("a PR body that fits under the room must not be truncated: %d bytes into a brief of %d", len(freePR), maxBriefBytes)
	}

	// Required inputs are refused loudly.
	t.Setenv("BRIEF_PATH", "")
	if _, err := captureStdout(t, runReviewBrief); err == nil {
		t.Error("an empty BRIEF_PATH must fail")
	}
	t.Setenv("NUMBER", "seven")
	t.Setenv("BRIEF_PATH", filepath.Join(t.TempDir(), "x.md"))
	if _, err := captureStdout(t, runReviewBrief); err == nil {
		t.Error("a non-numeric NUMBER must fail")
	}
}

func TestRunReviewReport(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	setArgs := func(pr string) { os.Args = []string{"harness", "review-report", pr} }

	findings := "- [high] app/x/main.go:42 - the mutex is released twice\n- [medium] app/y/x.go:7 - missing error check\n"
	comment := filepath.Join(t.TempDir(), "nested", "review-comment.md")

	t.Run("a fenced contract echo starts no fix run", func(t *testing.T) {
		// Issue #224, end to end: the reviewer echoes the output contract
		// inside a fence, and the report must read no finding out of it — the
		// phantom HIGH on app/zip/queue.go used to count as blocking and
		// start a fix run against a file that does not exist.
		diffBody := "diff --git a/app/x/main.go b/app/x/main.go\n" +
			"--- a/app/x/main.go\n+++ b/app/x/main.go\n" +
			"@@ -1,2 +1,3 @@\n context\n-removed\n+added\n"
		server := reviewServer(t, map[string]string{
			"/repos/RichardoC/factory-repo/pulls/7": `{"number":7,"title":"Review target","body":"The change.\n\nCloses #3","state":"open","head":{"ref":"codeaf/issue-3-42"},"base":{"ref":"main"}}`,
		}, diffBody, nil)
		t.Cleanup(server.Close)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopDone, "Here is the contract I was given:\n\n```markdown\n"+
			reviewOutputContract+"```\n\n## Findings\n\nnone\n")
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=label\nused=0\nblocking=0\n" {
			t.Errorf("the fenced echo still blocks: stdout = %q", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatalf("comment not written: %v", err)
		}
		if strings.HasPrefix(string(data), command+" fix:") {
			t.Errorf("the fenced echo started a fix run: %q", data)
		}
	})

	t.Run("a blocking review starts a fix run", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopDone, "## Findings\n\n"+findings)
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=fix\nused=0\nblocking=2\n" {
			t.Errorf("stdout = %q", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatalf("comment not written: %v", err)
		}
		if !strings.HasPrefix(string(data), "/codeaf fix: 2 blocking findings - high app/x/main.go:42; medium app/y/x.go:7\n") {
			t.Errorf("comment = %q", data)
		}
		assertReviewBodyClean(t, string(data))
	})

	t.Run("the cap turns the fix into a report", func(t *testing.T) {
		server := commentsServer(t, reportComments(3), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopDone, "## Findings\n\n"+findings)
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=report-only\nused=2\nblocking=2\n" {
			t.Errorf("stdout = %q", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(string(data), command+" fix:") {
			t.Errorf("the cap comment starts a fix run: %q", data)
		}
	})

	t.Run("an unrecognized severity withholds the label", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopDone, "## Findings\n\n- [critical] app/z/z.go:1 - scale unknown\n")
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=report-only\nused=0\nblocking=0\n" {
			t.Errorf("stdout = %q", out)
		}
	})

	t.Run("a clean review labels the pull request", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopDone, "## Findings\n\n")
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=label\nused=0\nblocking=0\n" {
			t.Errorf("stdout = %q", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), reviewPassedLabel) {
			t.Errorf("clean comment does not name the label: %q", data)
		}
	})

	t.Run("a draft pull request withholds the label (issue #237)", func(t *testing.T) {
		// The worker run that produced the patch stopped early and left the
		// pull request a draft; the review of the diff comes back clean, and
		// the report must not hand the pass label to unfinished work.
		server := reviewServer(t, map[string]string{
			"/repos/RichardoC/factory-repo/pulls/7": `{"number":7,"title":"t","state":"open","draft":true}`,
		}, "", nil)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopDone, "## Findings\n\n")
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=report-only\nused=0\nblocking=0\n" {
			t.Errorf("stdout = %q, want the withheld label", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatalf("comment not written: %v", err)
		}
		if strings.Contains(string(data), reviewPassedLabel) || !strings.Contains(string(data), "still a draft") {
			t.Errorf("comment = %q, want the draft verdict and no label", data)
		}
		assertReviewBodyClean(t, string(data))
	})

	t.Run("an early stop with blocking findings starts a fix run", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopDeadline, "## Findings\n\n"+findings)
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=fix\nused=0\nblocking=2\n" {
			t.Errorf("stdout = %q", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatalf("comment not written: %v", err)
		}
		if !strings.Contains(string(data), "`deadline`") {
			t.Errorf("fix comment does not name the stop reason: %q", data)
		}
		assertReviewBodyClean(t, string(data))
	})

	t.Run("an early stop with only low findings is report-only", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopDeadline, "## Findings\n\n- [low] docs/readme.md:3 - typo in the flag name\n")
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=report-only\nused=0\nblocking=0\n" {
			t.Errorf("stdout = %q", out)
		}
	})

	t.Run("inline findings act on an incomplete review", func(t *testing.T) {
		// Run 39010363221: the review wrote its findings inline, with no
		// findings section, and was read as a review with none.
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopIncomplete, "The review is finished. Findings: [high] app/x/main.go:42 - the mutex is released twice; and [medium] the queue never drains when a send fails.")
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=fix\nused=0\nblocking=2\n" {
			t.Errorf("stdout = %q", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatalf("comment not written: %v", err)
		}
		if !strings.Contains(string(data), "high app/x/main.go:42; medium\n\n") {
			t.Errorf("inline findings missing from the command line: %q", data)
		}
		assertReviewBodyClean(t, string(data))
	})

	t.Run("a severity word inside a finding's text mints no phantom fix", func(t *testing.T) {
		// Issue #335, end to end: the review's one finding quotes `[medium]`
		// in its own text. Before the fix the inline pass rescanned that
		// text, read a path-less medium finding out of it and started a fix
		// run against a finding nobody wrote.
		diffBody := "diff --git a/app/x/main.go b/app/x/main.go\n" +
			"--- a/app/x/main.go\n+++ b/app/x/main.go\n" +
			"@@ -1,2 +1,3 @@\n context\n-removed\n+added\n"
		server := reviewServer(t, map[string]string{
			"/repos/RichardoC/factory-repo/pulls/7":           `{"number":7,"title":"Review target","body":"The change.\n\nCloses #3","state":"open","head":{"ref":"codeaf/issue-3-42"},"base":{"ref":"main"},"draft":false}`,
			"/repos/RichardoC/factory-repo/issues/7/comments": "[]",
		}, diffBody, nil)
		t.Cleanup(server.Close)
		ghEnv(t, server)
		setArgs("7")
		// A low finding that quotes a blocking word: one finding, no block,
		// so the pass label lands and no fix command is written.
		writeReviewResult(t, stopDone, "## Findings\n\n"+
			"- [low] app/x/main.go:42 - the mutex is released twice, so a [medium] severity is misreported by this check\n")
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=label\nused=0\nblocking=0\n" {
			t.Errorf("a quoted severity word still blocks: stdout = %q", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatalf("comment not written: %v", err)
		}
		if strings.HasPrefix(string(data), command+" fix:") || strings.Contains(string(data), "blocking finding") {
			t.Errorf("a quoted severity word started a fix run: %q", data)
		}

		// The issue's own answer with a high finding: exactly one blocking
		// finding, and the fix command names it once, with no path-less
		// phantom beside it.
		server2 := reviewServer(t, map[string]string{
			"/repos/RichardoC/factory-repo/pulls/7":           `{"number":7,"title":"Review target","body":"The change.\n\nCloses #3","state":"open","head":{"ref":"codeaf/issue-3-42"},"base":{"ref":"main"},"draft":false}`,
			"/repos/RichardoC/factory-repo/issues/7/comments": "[]",
		}, diffBody, nil)
		t.Cleanup(server2.Close)
		ghEnv(t, server2)
		writeReviewResult(t, stopDone, "## Findings\n\n"+
			"- [high] app/x/main.go:42 - the mutex is released twice, so a [medium] severity is misreported by this check\n")
		reviewLedger(t)
		comment2 := filepath.Join(t.TempDir(), "review-comment.md")
		t.Setenv("REVIEW_COMMENT_PATH", comment2)
		out, err = captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=fix\nused=0\nblocking=1\n" {
			t.Errorf("stdout = %q, want exactly one blocking finding", out)
		}
		data, err = os.ReadFile(comment2)
		if err != nil {
			t.Fatalf("comment not written: %v", err)
		}
		if !strings.HasPrefix(string(data), "/codeaf fix: 1 blocking finding - high app/x/main.go:42\n") {
			t.Errorf("the fix command carries a phantom finding: %q", data)
		}
		if strings.Contains(string(data), "medium (unsafe path)") {
			t.Errorf("the phantom path-less medium finding reached the command line: %q", data)
		}
		assertReviewBodyClean(t, string(data))
	})

	t.Run("the run record's final result is a fallback", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		// The envelope's answer is empty; the review's text lives in the
		// root task's end record, as it did on run 39010363221.
		writeReviewResult(t, stopIncomplete, "")
		writeTrajectory(t, `{"kind":"step","step":1,"command":"go test ./...","observation":"ok"}
{"kind":"end","step":0,"steps":1,"result":"The review found two problems.\n\n[high] app/x/main.go:42 - the mutex is released twice\n[low] docs/readme.md:3 - typo"}
`)
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=fix\nused=0\nblocking=1\n" {
			t.Errorf("stdout = %q", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatalf("comment not written: %v", err)
		}
		if !strings.Contains(string(data), "| app/x/main.go:42 |") {
			t.Errorf("run-record finding missing from the table: %q", data)
		}
		assertReviewBodyClean(t, string(data))
	})

	t.Run("the fallback also answers a section with no bullets", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopIncomplete, "## Findings\n\nnone\n")
		writeTrajectory(t, `{"kind":"end","result":"[high] app/x/main.go:42 - the mutex is released twice\n"}
`)
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=fix\nused=0\nblocking=1\n" {
			t.Errorf("stdout = %q", out)
		}
	})

	t.Run("the run record beside the result is found without TRAJECTORY_PATH", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		resultPath := writeReviewResult(t, stopIncomplete, "")
		record := `{"kind":"end","result":"[high] app/x/main.go:42 - the mutex is released twice\n"}
`
		if err := os.WriteFile(filepath.Join(filepath.Dir(resultPath), trajectoryFileName), []byte(record), 0o644); err != nil {
			t.Fatal(err)
		}
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=fix\nused=0\nblocking=1\n" {
			t.Errorf("stdout = %q", out)
		}
	})

	t.Run("a hostile run record is read as data", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopIncomplete, "")
		writeTrajectory(t, `not json at all
{"kind":"end"}
{"kind":"end","result":42}
["kind","end"]
{"kind":"step","result":"[high] step.go:1 - not an ending"}
{"kind":"begin","result":"[high] begin.go:1 - not an ending either"}
{"kind":"end","result":"prose, then findings.\n\n- [high] app/x/main.go:42 - | pipes | and <!-- html comments --> and @alice\n"}
{"kind":"end"}trailing garbage on the same line
`)
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		// Only the real end line's findings act; the step and begin lines
		// never do, and the malformed lines are skipped, not fatal.
		if out != "action=fix\nused=0\nblocking=1\n" {
			t.Errorf("stdout = %q", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatalf("comment not written: %v", err)
		}
		assertReviewBodyClean(t, string(data))
	})

	t.Run("a long run record is read from its tail, still capped", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopIncomplete, "")
		// The end record is the record's last line (issue #111): the
		// 36-step review of run 39062136141 pushed it past a read capped
		// from the head, and the report threw the review away. Padding
		// before the ending no longer hides it — but the read stays
		// capped at maxTrajectoryBytes.
		padding := strings.Repeat("#", maxTrajectoryBytes) + "\n"
		writeTrajectory(t, padding+`{"kind":"end","result":"[high] app/x/main.go:42 - beyond the head, in the tail\n"}`)
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=fix\nused=0\nblocking=1\n" {
			t.Errorf("tail read: stdout = %q", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatalf("comment not written: %v", err)
		}
		assertReviewBodyClean(t, string(data))

		// What follows the end record can still push it out of reach: a
		// record ending in megabytes of garbage is read as its tail, the
		// end record outside the cap is never seen, and the garbage is
		// skipped, not fatal.
		trailing := strings.Repeat("{", 2*maxTrajectoryBytes) + "\n"
		writeTrajectory(t, padding+`{"kind":"end","result":"[high] app/x/main.go:42 - out of reach\n"}`+"\n"+trailing)
		out, err = captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=report-only\nused=0\nblocking=0\n" {
			t.Errorf("end record out of reach: stdout = %q", out)
		}
	})

	t.Run("the run that prompted issue #111 is reported as a review", func(t *testing.T) {
		// Run 39062136141: a 36-step review, an empty envelope answer, a
		// closing result holding one FINDINGS paragraph, stop incomplete.
		// The findings must act, not read as "no findings found".
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopIncomplete, "")
		var record strings.Builder
		for step := 1; step <= 36; step++ {
			fmt.Fprintf(&record, "{\"kind\":\"step\",\"step\":%d,\"command\":\"go test ./...\",\"observation\":\"ok\"}\n", step)
		}
		record.WriteString("{\"kind\":\"end\",\"step\":0,\"steps\":36,\"result\":\"" +
			"The review is complete. Reported in the answer under ## Findings.\\n\\n" +
			"FINDINGS (3, 2 blocking): [medium] main.go:1155 - the mutex is released twice under the error path; " +
			"[medium] queue.go:20 - the queue never drains when a send fails; " +
			"[low] docs/readme.md:3 - typo in the flag name\"}\n")
		writeTrajectory(t, record.String())
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=fix\nused=0\nblocking=2\n" {
			t.Fatalf("stdout = %q, want a fix run for the 2 blocking findings", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatalf("comment not written: %v", err)
		}
		body := string(data)
		for _, want := range []string{
			"| `medium` | main.go:1155 | the mutex is released twice under the error path |",
			"| `medium` | queue.go:20 | the queue never drains when a send fails |",
			"| `low` | docs/readme.md:3 | typo in the flag name |",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("finding table missing %q in %q", want, body)
			}
		}
		assertReviewBodyClean(t, body)
	})

	t.Run("an incomplete run whose review finished with only low findings", func(t *testing.T) {
		// Issue #111: the closing result holds a finished review, so the
		// report lists its findings and says the run ended incomplete
		// after finishing its review — never "no findings found".
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopIncomplete, "")
		writeTrajectory(t, `{"kind":"end","result":"FINDINGS (1, 0 blocking): [low] docs/readme.md:3 - typo in the flag name"}`)
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=report-only\nused=0\nblocking=0\n" {
			t.Fatalf("stdout = %q, want report-only", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatalf("comment not written: %v", err)
		}
		body := string(data)
		for _, want := range []string{"after finishing its review", "| `low` | docs/readme.md:3 | typo in the flag name |"} {
			if !strings.Contains(body, want) {
				t.Errorf("body missing %q: %q", want, body)
			}
		}
		if strings.Contains(body, "no findings section") {
			t.Errorf("a finished review must not read as one with no findings: %q", body)
		}
		assertReviewBodyClean(t, body)
	})

	t.Run("a long run record ships its tail, end record included", func(t *testing.T) {
		// The run job ships the record beside the result; the copy must
		// keep the record's ending (issue #111), where the end record of
		// a long run lives, not its head.
		home := t.TempDir()
		t.Setenv("CODEAF_HOME", home)
		recordDir := filepath.Join(home, "runs", "42", "tasks", "root")
		if err := os.MkdirAll(recordDir, 0o755); err != nil {
			t.Fatal(err)
		}
		padding := strings.Repeat("#", maxTrajectoryBytes) + "\n"
		record := padding + `{"kind":"end","result":"[high] app/x/main.go:42 - shipped from the tail\n"}` + "\n"
		if err := os.WriteFile(filepath.Join(recordDir, "trajectory.jsonl"), []byte(record), 0o644); err != nil {
			t.Fatal(err)
		}
		resultPath := writeReviewResult(t, stopIncomplete, "")
		t.Setenv("TRAJECTORY_PATH", "")
		if shipped := shipRunRecord(); shipped == "" {
			t.Fatal("shipRunRecord found nothing")
		}
		data, err := os.ReadFile(filepath.Join(filepath.Dir(resultPath), trajectoryFileName))
		if err != nil {
			t.Fatalf("record not shipped: %v", err)
		}
		if got := readClosing(filepath.Join(filepath.Dir(resultPath), trajectoryFileName)); got.Answer == "" {
			t.Errorf("the shipped copy lost the end record: %d bytes, tail starts %q", len(data), data[:40])
		}
	})

	t.Run("a run record with no findings stays advisory", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeReviewResult(t, stopIncomplete, "")
		writeTrajectory(t, `{"kind":"end","result":"The review is clean."}
`)
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=report-only\nused=0\nblocking=0\n" {
			t.Errorf("stdout = %q", out)
		}
	})

	t.Run("a missing result is report-only, not an error", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		t.Setenv("RESULT_PATH", filepath.Join(t.TempDir(), "absent.json"))
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		out, err := captureStdout(t, runReviewReport)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=report-only\nused=0\nblocking=0\n" {
			t.Errorf("stdout = %q", out)
		}
	})

	t.Run("required inputs and the argument are checked", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("seven; rm -rf /")
		writeReviewResult(t, stopDone, "")
		t.Setenv("REVIEW_COMMENT_PATH", comment)
		reviewLedger(t)
		if _, err := captureStdout(t, runReviewReport); err == nil {
			t.Error("a non-numeric PR argument must fail")
		}
		setArgs("7")
		t.Setenv("RESULT_PATH", "")
		if _, err := captureStdout(t, runReviewReport); err == nil {
			t.Error("an empty RESULT_PATH must fail")
		}
		writeReviewResult(t, stopDone, "")
		t.Setenv("LEDGER_PATH", "")
		if _, err := captureStdout(t, runReviewReport); err == nil {
			t.Error("an empty LEDGER_PATH must fail")
		}
		reviewLedger(t)
		t.Setenv("REVIEW_COMMENT_PATH", "")
		if _, err := captureStdout(t, runReviewReport); err == nil {
			t.Error("an empty REVIEW_COMMENT_PATH must fail")
		}
	})
}

func TestDecideDispatchReview(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY", "RichardoC/factory-repo")
	t.Setenv("GITHUB_RUN_ID", "42")
	labelled := []label{{Name: "codeaf"}}
	mkPR := func(number int, state string, labels []label, headRepo string) item {
		pr := item{Number: number, State: state, Labels: labels}
		pr.Head.Ref = "codeaf/issue-3-42"
		pr.Head.Repo = &struct {
			FullName string `json:"full_name"`
		}{FullName: headRepo}
		pr.Base.Ref = "main"
		return pr
	}
	openPR := mkPR(7, "open", labelled, "RichardoC/factory-repo")

	cases := []struct {
		name   string
		review string
		pr     item
		run    bool
		reason string
	}{
		{"labelled open pr", "7", openPR, true, "review dispatch"},
		{"unlabelled pr", "7", mkPR(7, "open", nil, "RichardoC/factory-repo"), false, "PR is not labelled codeaf"},
		{"closed pr", "7", mkPR(7, "closed", labelled, "RichardoC/factory-repo"), false, "PR is not open"},
		{"fork head", "7", mkPR(7, "open", labelled, "evil/factory-repo"), false, "PR head is in a fork; CodeAF only pushes to branches in RichardoC/factory-repo"},
		{"non-numeric input", "seven", openPR, false, "workflow_dispatch needs a numeric issue or pr input"},
	}
	for _, c := range cases {
		t.Run("review "+c.name, func(t *testing.T) {
			lookupPR := func(n int) (item, error) {
				if n != 7 {
					t.Fatalf("lookupPR(%d), want 7", n)
				}
				return c.pr, nil
			}
			lookupIssue := func(int) (item, error) { t.Fatal("unexpected issue lookup"); return item{}, nil }
			ev := event{Inputs: map[string]string{"review": c.review}}
			got, err := Decide("workflow_dispatch", ev, "codeaf", lookupPR, lookupIssue)
			if err != nil {
				t.Fatal(err)
			}
			if got.Run != c.run || got.Reason != c.reason {
				t.Fatalf("Decide = %+v, want run %t with reason %q", got, c.run, c.reason)
			}
			if c.run && (got.Mode != "review" || got.Number != 7 || got.Branch != openPR.Head.Ref || got.Base != openPR.Base.Ref) {
				t.Fatalf("Decide = %+v, want mode review with the PR's number and branches", got)
			}
		})
	}

	t.Run("a failing PR lookup is an error", func(t *testing.T) {
		lookupPR := func(int) (item, error) { return item{}, fmt.Errorf("500") }
		ev := event{Inputs: map[string]string{"review": "7"}}
		if _, err := Decide("workflow_dispatch", ev, "codeaf", lookupPR, func(int) (item, error) { return item{}, nil }); err == nil {
			t.Fatal("a failing PR lookup must be returned as an error")
		}
	})

	// Issue #41: a dispatch naming both pr and review is refused instead
	// of silently running the pr path — the run name would be built with a
	// different precedence than the mode, so the queue would hold the
	// wrong number in flight.
	t.Run("a dispatch naming both pr and review is refused", func(t *testing.T) {
		lookupPR := func(int) (item, error) { t.Fatal("unexpected PR lookup"); return item{}, nil }
		ev := event{Inputs: map[string]string{"pr": "7", "review": "7"}}
		got, err := Decide("workflow_dispatch", ev, "codeaf", lookupPR, func(int) (item, error) { return item{}, nil })
		if err != nil {
			t.Fatal(err)
		}
		if got.Run || got.Reason != "a dispatch takes only one of issue, pr and review; it names pr, review" {
			t.Fatalf("Decide = %+v, want a skip naming both inputs", got)
		}
	})
}

// TestParseFindingsFencedContractEchoParaphrased: the phantom is not only the
// exact echo (issue #224, follow-up finding 1). A reviewer who reprints the
// contract's example bullet with the incidental differences a reprint
// tolerates — a nudged line number, different spacing, a trailing stop — is
// still quoting the brief's own bullet, and must still mint no finding, in
// either the section or inline, with or without the diff in hand. The
// example is the one bullet in the brief that spells out app/zip/queue.go,
// a file this repository does not have.
func TestParseFindingsFencedContractEchoParaphrased(t *testing.T) {
	known := diffPaths("diff --git a/app/queue/main.go b/app/queue/main.go\n" +
		"--- a/app/queue/main.go\n+++ b/app/queue/main.go\n" +
		"@@ -1,0 +1,1 @@\n+package main\n")
	for _, tc := range []struct {
		name, bullet string
	}{
		{"the example itself", "- [high] app/zip/queue.go:42 - the mutex is released twice under the error path, so a second request deadlocks"},
		{"line nudged by one", "- [high] app/zip/queue.go:43 - the mutex is released twice under the error path, so a second request deadlocks"},
		{"severity case", "- [HIGH] app/zip/queue.go:42 - the mutex is released twice under the error path, so a second request deadlocks"},
		{"trailing stop", "- [high] app/zip/queue.go:42 - The mutex is released twice under the error path, so a second request deadlocks."},
		{"collapsed spacing", "- [high] app/zip/queue.go:42 - the mutex is released   twice under the error path, so a second request deadlocks"},
		{"no line number at all", "- [high] app/zip/queue.go - the mutex is released twice under the error path, so a second request deadlocks"},
		{"markdown emphasis", "- [high] app/zip/queue.go:42 - the mutex is released twice under the error path, so a second request `deadlocks`"},
	} {
		for _, answer := range []string{
			"```\n" + tc.bullet + "\n```\n",                // bare fence, inline pass
			"## Findings\n\n```\n" + tc.bullet + "\n```\n", // in the section under a fence
			"```markdown\n" + tc.bullet + "\n```\n## Findings\n\nnone\n",
		} {
			for _, k := range []map[string]bool{known, nil} {
				findings, _ := parseFindingsIn(answer, k)
				for _, f := range findings {
					if strings.EqualFold(f.Severity, "high") && f.Path == contractExamplePath {
						t.Errorf("%s: the paraphrased echo was kept: %+v", tc.name, f)
					}
				}
				if got := blockingFindings(findings); len(got) != 0 {
					t.Errorf("%s: blocking = %+v, want none — a quoted example must never start a fix run", tc.name, got)
				}
			}
		}
	}
	// The guard must not swallow a finding that only shares the path.
	real := "```\n- [high] app/zip/queue.go:42 - the goroutine leaks on shutdown\n```\n"
	findings, _ := parseFindingsIn(real, nil)
	if len(findings) != 1 || findings[0].Text != "the goroutine leaks on shutdown" {
		t.Errorf("a fenced finding on the example path with different text = %+v, want it kept", findings)
	}
}

// TestParseFindingsFencedOutsideDiffIsAQuote: the maintainer's second rule
// (issue #224, follow-up finding 2) — a fenced finding whose path the PR's
// diff does not touch is read as a quote about another change, not a report,
// and is dropped. The test distinguishes it from a real finding about a file
// the PR does not modify: the drop happens *only when the diff is known*. With
// no diff in hand nothing outside a fence changes and nothing fenced is
// judged by path, because the parser cannot tell a quote about another file
// from a real report about one — and losing that report is a false pass.
func TestParseFindingsFencedOutsideDiffIsAQuote(t *testing.T) {
	known := diffPaths("diff --git a/app/queue/main.go b/app/queue/main.go\n" +
		"--- a/app/queue/main.go\n+++ b/app/queue/main.go\n" +
		"@@ -1,0 +1,1 @@\n+package main\n")
	if known == nil {
		t.Fatal("diffPaths dropped a real diff")
	}
	// A fenced report about a file the PR changes: kept, and it blocks.
	if f, _ := parseFindingsIn("```\n- [high] app/queue/main.go:7 - the lock is held across the send\n```\n", known); len(f) != 1 {
		t.Errorf("a fenced finding on a diff path = %+v, want it kept", f)
	} else if len(blockingFindings(f)) != 1 {
		t.Errorf("a fenced finding on a diff path stopped blocking: %+v", f)
	}
	// A fenced finding about a file the PR does not touch: a quote, dropped.
	quoted := "```\n- [high] app/contracts/worker.go:9 - the retry loop never backs off\n```\n"
	if f, _ := parseFindingsIn(quoted, known); len(f) != 0 {
		t.Errorf("a fenced finding outside the diff = %+v, want it dropped as a quote", f)
	}
	// The identical text with no diff in hand is a report: kept. This is the
	// distinction the finding asked the tests to pin.
	if f, _ := parseFindingsIn(quoted, nil); len(f) != 1 {
		t.Errorf("outside the diff with no diff known = %+v, want it kept", f)
	}
	// Unfenced, the same absent path is never dropped: issue #49 leniency.
	unfenced := "- [high] app/contracts/worker.go:9 - the retry loop never backs off\n"
	if f, _ := parseFindingsIn(unfenced, known); len(f) != 1 {
		t.Errorf("an unfenced finding outside the diff = %+v, want it kept", f)
	}
	// A diff that touches nothing parseable leaves every finding alone.
	if f, _ := parseFindingsIn(quoted, diffPaths("")); len(f) != 1 {
		t.Errorf("an unparseable diff dropped a finding = %+v, want it kept", f)
	}
}

// TestParseFindingsFencedCappedDiffstatKeepsFinding (issue #37): diffstat
// parses at most maxDiffFiles files and counts the rest, but diffPaths
// discarded the count and presented the partial set as complete — a fenced
// finding on the 201st path was read as a quote about a path the diff does
// not touch and dropped, the false pass the path filter exists to prevent.
// A capped listing is now no listing: the known set is nil and the finding
// is kept and blocks.
func TestParseFindingsFencedCappedDiffstatKeepsFinding(t *testing.T) {
	last := maxDiffFiles + 1
	var b strings.Builder
	for i := 1; i <= last; i++ {
		b.WriteString(diffFile(fmt.Sprintf("app/dir%d/file%d.go", i, i), 2))
	}
	diff := b.String()
	if known := diffPaths(diff); known != nil {
		t.Errorf("a %d-file diff built a partial known set of %d paths, want nil", last, len(known))
	}
	// Paths unknown, the fenced bullet past the cap is a real report: kept,
	// and it starts the normal blocking path.
	bullet := fmt.Sprintf("```\n- [high] app/dir%d/file%d.go:7 - the lock is held across the send\n```\n", last, last)
	if f, _ := parseFindingsIn(bullet, diffPaths(diff)); len(f) != 1 {
		t.Errorf("a fenced finding past the file cap = %+v, want it kept", f)
	} else if len(blockingFindings(f)) != 1 {
		t.Errorf("a fenced finding past the file cap stopped blocking: %+v", f)
	}
	// At the cap exactly — maxDiffFiles files, none past it — the set is
	// complete, and a finding on the last parsed path blocks as usual.
	var c strings.Builder
	for i := 1; i <= maxDiffFiles; i++ {
		c.WriteString(diffFile(fmt.Sprintf("app/dir%d/file%d.go", i, i), 2))
	}
	known := diffPaths(c.String())
	if known == nil {
		t.Fatal("a diff at the file cap yielded no known set")
	}
	path200 := fmt.Sprintf("app/dir%d/file%d.go", maxDiffFiles, maxDiffFiles)
	if !known[path200] {
		t.Errorf("the known set is missing the %dth path %q", maxDiffFiles, path200)
	}
	bullet200 := fmt.Sprintf("```\n- [high] %s:7 - the lock is held across the send\n```\n", path200)
	if f, _ := parseFindingsIn(bullet200, known); len(f) != 1 || len(blockingFindings(f)) != 1 {
		t.Errorf("a fenced finding on the %dth path = %+v, want it kept and blocking", maxDiffFiles, f)
	}
}

// TestKnownPathsTruncatedDiffKeepsFinding (issue #37): a diff body stopped
// at diffReadCap (1 MiB) names only the files before the cut, so
// review-report must hand the parser nil — the same paths-unknown direction
// as a fetch that failed — and a real fenced finding on a file past the cut
// is kept. A complete diff still yields its paths, so the filter stays
// armed for the quotes it exists to catch.
func TestKnownPathsTruncatedDiffKeepsFinding(t *testing.T) {
	body := strings.Repeat("diff --git a/app/x.go b/app/x.go\n+++ b/app/x.go\n@@\n", 30000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	ghEnv(t, server)
	diff, truncated, err := newGitHub().diff(37)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !truncated {
		t.Fatal("a body over diffReadCap did not report the truncation")
	}
	if known := knownPaths(diff, truncated, 37); known != nil {
		t.Errorf("a truncated diff built a known set of %d paths, want nil", len(known))
	}
	bullet := "```\n- [high] app/past/the/cut.go:7 - the lock is held across the send\n```\n"
	if f, _ := parseFindingsIn(bullet, nil); len(f) != 1 || len(blockingFindings(f)) != 1 {
		t.Errorf("a fenced finding past the cut = %+v, want it kept and blocking", f)
	}
	complete := "diff --git a/app/queue/main.go b/app/queue/main.go\n" +
		"--- a/app/queue/main.go\n+++ b/app/queue/main.go\n" +
		"@@ -1,0 +1,1 @@\n+package main\n"
	if known := knownPaths(complete, false, 37); known == nil || !known["app/queue/main.go"] {
		t.Errorf("an untruncated diff lost its known set: %v", known)
	}
}

// TestParseFindingsFenceGuardDoesNotDropUnfenced: the whole fence machinery
// is inert outside a fence (issue #224). Every bullet the paraphrase guard
// and the diff filter would drop when fenced is kept when unfenced — the
// issue #49 leniency the maintainer asked to preserve, pinned to the same
// text the fenced cases drop.
func TestParseFindingsFenceGuardDoesNotDropUnfenced(t *testing.T) {
	known := diffPaths("diff --git a/app/queue/main.go b/app/queue/main.go\n" +
		"--- a/app/queue/main.go\n+++ b/app/queue/main.go\n" +
		"@@ -1,0 +1,1 @@\n+package main\n")
	for _, bullet := range []string{
		"- [high] app/zip/queue.go:42 - the mutex is released twice under the error path, so a second request deadlocks",
		"- [high] app/zip/queue.go:43 - the mutex is released twice under the error path, so a second request deadlocks",
		"- [high] app/contracts/worker.go:9 - the retry loop never backs off",
	} {
		answer := "## Findings\n\n" + bullet + "\n"
		findings, _ := parseFindingsIn(answer, known)
		if len(findings) != 1 {
			t.Errorf("unfenced %q = %+v, want it kept", bullet, findings)
			continue
		}
		if findings[0].fenced {
			t.Errorf("unfenced %q was marked fenced", bullet)
		}
		if len(blockingFindings(findings)) != 1 {
			t.Errorf("unfenced %q stopped blocking", bullet)
		}
	}
}

// diffFile builds one file's section of a unified diff: a `diff --git`
// header and `lines` body lines, each naming the file so a clipped section
// is recognisable.
func diffFile(path string, lines int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -1,3 +%d @@\n",
		path, path, path, path, lines+1)
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, "+ %s line %d\n", path, i)
	}
	return b.String()
}

// Issue #231: the review diff cap is file-aware. A six-file diff past the
// cap keeps every `diff --git` header, stays within the cap, names the
// clipped files in the note, and every file still contributes content.
func TestCapDiffKeepsEveryFileHeader(t *testing.T) {
	var files []string
	for i := 0; i < 6; i++ {
		files = append(files, diffFile(fmt.Sprintf("app/f%d.go", i), 3000))
	}
	diff := strings.Join(files, "")
	if len(diff) <= maxReviewDiffBytes {
		t.Fatalf("test diff must exceed the cap: %d bytes", len(diff))
	}
	got := capDiff(diff, maxReviewDiffBytes)
	if len(got) > maxReviewDiffBytes {
		t.Errorf("capped diff is %d bytes, over the %d cap", len(got), maxReviewDiffBytes)
	}
	for i := 0; i < 6; i++ {
		header := fmt.Sprintf("diff --git a/app/f%d.go b/app/f%d.go", i, i)
		if !strings.Contains(got, header) {
			t.Errorf("capped diff lost file %d's header %q", i, header)
		}
	}
	// Six equal ~120KB sections cannot all fit, so every file is clipped
	// and every one of them must be named in the truncation note, with the
	// instruction to read them on the checkout.
	if !strings.Contains(got, "read the full content of") {
		t.Errorf("truncation note missing the read-on-checkout instruction: %q", got[len(got)-300:])
	}
	if !strings.Contains(got, "on the checked-out PR head branch") {
		t.Errorf("truncation note missing the checkout pointer")
	}
	for i := 0; i < 6; i++ {
		if !strings.Contains(got, fmt.Sprintf("app/f%d.go", i)) {
			t.Errorf("truncation note does not name clipped file app/f%d.go", i)
		}
		if !strings.Contains(got, fmt.Sprintf("+ app/f%d.go line 0", i)) {
			t.Errorf("file %d contributes no content after the cap", i)
		}
	}
}

// Issue #231: one file larger than the whole cap is truncated with its
// name in the note, its header still kept.
func TestCapDiffSingleOversizedFile(t *testing.T) {
	diff := diffFile("app/big.go", 8000) // ~100KB, over the cap alone
	if len(diff) <= maxReviewDiffBytes {
		t.Fatalf("test diff must exceed the cap: %d bytes", len(diff))
	}
	got := capDiff(diff, maxReviewDiffBytes)
	if len(got) > maxReviewDiffBytes {
		t.Errorf("capped diff is %d bytes, over the %d cap", len(got), maxReviewDiffBytes)
	}
	if !strings.Contains(got, "diff --git a/app/big.go b/app/big.go") {
		t.Errorf("capped diff lost the file's header")
	}
	if !strings.Contains(got, "+ app/big.go line 0") {
		t.Errorf("the oversized file contributes no content after the cap")
	}
	if !strings.Contains(got, "read the full content of app/big.go on the checked-out PR head branch") {
		t.Errorf("truncation note does not name the clipped file or point at the checkout: %q", got[len(got)-200:])
	}
}

// The file-aware cap leaves diffs at or under the cap untouched, keeps a
// multi-file diff with room for every file identical, and falls back to a
// plain prefix cap for a diff with no file headers at all.
func TestCapDiffAllocationEdges(t *testing.T) {
	small := diffFile("app/s.go", 3) + diffFile("app/t.go", 3)
	if got := capDiff(small, len(small)); got != small {
		t.Errorf("a diff at the cap was altered: %q", got)
	}
	if got := capDiff(small, maxReviewDiffBytes); got != small {
		t.Errorf("a diff under the cap was altered: %q", got)
	}
	two := diffFile("app/a.go", 900) + diffFile("app/b.go", 900) // ~23KB
	got := capDiff(two, 12000)
	if len(got) != 12000 {
		t.Errorf("capped diff is %d bytes, want exactly the 12000 cap", len(got))
	}
	for _, p := range []string{"diff --git a/app/a.go b/app/a.go", "diff --git a/app/b.go b/app/b.go", "+ app/a.go line 0", "+ app/b.go line 0"} {
		if !strings.Contains(got, p) {
			t.Errorf("capped two-file diff lost %q", p)
		}
	}
	if !strings.Contains(got, "read the full content of app/a.go, app/b.go on the checked-out PR head branch") {
		t.Errorf("truncation note does not name both clipped files")
	}
	// A headerless blob caps as a plain prefix with the original note.
	blob := strings.Repeat("+ plain body line\n", 8000)
	got = capDiff(blob, 60000)
	if len(got) > 60000 {
		t.Errorf("headerless cap is %d bytes, over the cap", len(got))
	}
	if !strings.Contains(got, "(diff truncated at 60000 bytes)") {
		t.Errorf("headerless cap lost the byte-count note")
	}
	// The cap never splits a UTF-8 rune, in any file's body.
	utf := diffFile("app/utf.go", 100) + strings.Repeat("+ éééé\n", 20000)
	got = capDiff(utf, maxReviewDiffBytes)
	if len(got) > maxReviewDiffBytes {
		t.Errorf("utf8 cap is %d bytes, over the cap", len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("capped diff split a UTF-8 rune")
	}
}

// Issue #231 review: a diff that keeps one file whole while clipping another
// used to panic — the rune-backoff loop indexed the body at the cut before
// the cut < len(body) guard, and a file whose body fit its share put the cut
// at exactly len(body). The mixed shape is the one that crashed in review.
func TestCapDiffMixedWholeAndClippedFile(t *testing.T) {
	diff := diffFile("app/small.go", 3) + diffFile("app/huge.go", 4000)
	got := capDiff(diff, maxReviewDiffBytes)
	if len(got) > maxReviewDiffBytes {
		t.Errorf("mixed cap is %d bytes, over the 60000 cap", len(got))
	}
	for _, p := range []string{"diff --git a/app/small.go b/app/small.go", "diff --git a/app/huge.go b/app/huge.go"} {
		if !strings.Contains(got, p) {
			t.Errorf("mixed cap lost %q", p)
		}
	}
	// The small file's whole body is kept; the huge one is clipped and named.
	if !strings.Contains(got, "+ app/small.go line 2") {
		t.Errorf("the file kept whole lost content:\n%s", got)
	}
	if !strings.Contains(got, "read the full content of app/huge.go on the checked-out PR head branch") {
		t.Errorf("truncation note does not name the clipped file:\n%s", got[len(got)-300:])
	}
	if !utf8.ValidString(got) {
		t.Errorf("mixed cap split a UTF-8 rune")
	}
	// A file with an empty body (a rename or mode change, no hunks) is not a
	// special case either: its share is zero and its body is empty.
	rename := "diff --git a/app/old.go b/app/new.go\nsimilarity index 100%\nrename from app/old.go\nrename to app/new.go\n"
	if got := capDiff(rename+diffFile("app/huge.go", 4000), maxReviewDiffBytes); len(got) > maxReviewDiffBytes || !strings.Contains(got, "diff --git a/app/old.go b/app/new.go") {
		t.Errorf("rename-only section broke the cap: len %d", len(got))
	}
	// Forty files far past a cap that still fits their headers: the note
	// names at most capDiffMaxNoteFiles files and counts the rest, and the
	// total stays within the cap.
	var many strings.Builder
	for i := 0; i < 40; i++ {
		many.WriteString(diffFile(fmt.Sprintf("app/f%02d.go", i), 100))
	}
	manyDiff := capDiff(many.String(), 6000)
	if len(manyDiff) > 6000 {
		t.Errorf("many-file cap is %d bytes, over the 6000 cap", len(manyDiff))
	}
	if !strings.Contains(manyDiff, "and 10 more") {
		t.Errorf("many-file note does not count the files past its limit:\n%s", manyDiff[len(manyDiff)-400:])
	}
	// A diff whose headers alone exceed the cap has no sections to preserve:
	// it falls back to the plain prefix cap, still bounded and still noting
	// the byte count.
	if got := capDiff(many.String(), 200); len(got) > 200 || !strings.Contains(got, "(diff truncated at 200 bytes)") {
		t.Errorf("header-over-cap fallback is not a bounded prefix cap: len %d", len(got))
	}
}

// TestReviewPlanCapKeepsBlockingFinding is issue #385: the cap kept findings
// in arrival order, so an answer with more than maxReviewFindings findings
// lost whatever it wrote last. A verbose — or hostile, prompt-injected —
// review could list fifty [low] findings and then its only [high]; the cap
// dropped the high, blockingFindings came back empty and reviewPlan added
// codeaf-review-passed to a review that did not pass. The cap now keeps the
// findings that can change the decision, so the pass label cannot outrun a
// blocking finding written past it.
func TestReviewPlanCapKeepsBlockingFinding(t *testing.T) {
	// answerWith builds a findings section of n low bullets whose last bullet
	// carries severity: the only finding past the cap that matters.
	answerWith := func(severity string, n int) string {
		var b strings.Builder
		b.WriteString("## Findings\n\n")
		for i := 1; i < n; i++ {
			fmt.Fprintf(&b, "- [low] app/cap.go:%d - nit %d\n", i, i)
		}
		fmt.Fprintf(&b, "- [%s] app/cap.go:%d - the finding that matters\n", severity, n)
		return b.String()
	}
	const n = maxReviewFindings + 10 // 60 findings: the cap cuts ten of them

	for _, tc := range []struct {
		name     string
		severity string
		want     int    // blocking findings the parse must keep
		action   string // what reviewPlan must decide
	}{
		{"high last", "high", 1, "fix"},
		{"medium last", "medium", 1, "fix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			findings, sawSection := parseFindings(answerWith(tc.severity, n))
			if !sawSection {
				t.Fatal("sawSection = false, want true")
			}
			if len(findings) != maxReviewFindings {
				t.Fatalf("parse = %d findings, want the cap %d", len(findings), maxReviewFindings)
			}
			if got := len(blockingFindings(findings)); got != tc.want {
				t.Fatalf("blocking findings past the cap = %d, want %d: %+v", got, tc.want, findings)
			}
			kept := false
			for _, f := range findings {
				if f.Line == n && strings.Contains(f.Text, "the finding that matters") {
					kept = true
				}
			}
			if !kept {
				t.Errorf("the %s finding written past the cap was dropped: %+v", tc.severity, findings)
			}
			// The decision, not just the count: the label must not be added.
			server := commentsServer(t, reportComments(0), http.StatusOK)
			ghEnv(t, server)
			action, body, _, err := reviewPlan(newGitHub(), 7,
				Result{Stop: stopDone, Model: "deepseek-ai/deepseek-v4.1-flash"}, true, findings, sawSection)
			if err != nil {
				t.Fatal(err)
			}
			if action != tc.action {
				t.Fatalf("action = %q, want %q (the %s finding past the cap was dropped)", action, tc.action, tc.severity)
			}
			if strings.Contains(body, reviewPassedLabel) || !strings.HasPrefix(body, command+" fix:") {
				t.Errorf("a review whose only %s finding sits past the cap = %q, want the fix run and no pass label", tc.severity, body)
			}
			assertReviewBodyClean(t, body)
		})
	}

	// An unrecognized severity past the cap withholds the label too, exactly
	// as one under the cap already did.
	findings, sawSection := parseFindings(answerWith("critical", n))
	if len(blockingFindings(findings)) != 0 || !hasUnrecognizedSeverity(findings) {
		t.Fatalf("an unknown severity past the cap = %+v, want it kept and the scale unknown", findings)
	}
	server := commentsServer(t, reportComments(0), http.StatusOK)
	ghEnv(t, server)
	action, body, _, err := reviewPlan(newGitHub(), 7,
		Result{Stop: stopDone, Model: "deepseek-ai/deepseek-v4.1-flash"}, true, findings, sawSection)
	if err != nil {
		t.Fatal(err)
	}
	if action == "label" || strings.Contains(body, reviewPassedLabel) {
		t.Errorf("an unknown severity past the cap = action %q body %q, want the label withheld", action, body)
	}

	// The ranking changes nothing under the cap: an answer whose findings all
	// fit keeps arrival order, so a review reads as the reviewer wrote it.
	inOrder, _ := parseFindings("## Findings\n\n- [low] app/a.go:1 - first\n- [high] app/a.go:2 - second\n- [low] app/a.go:3 - third\n")
	if len(inOrder) != 3 || inOrder[0].Line != 1 || inOrder[1].Line != 2 || inOrder[2].Line != 3 {
		t.Errorf("an answer under the cap = %+v, want the findings in arrival order", inOrder)
	}
	// Ties keep arrival order at the cap as well: the retained low findings
	// are the ones written first, so the same answer parses the same way.
	ties, _ := parseFindings(answerWith("low", n))
	if len(ties) != maxReviewFindings || ties[maxReviewFindings-1].Line != maxReviewFindings {
		t.Errorf("an all-low answer at the cap = %d findings, last line %d, want the first %d in arrival order",
			len(ties), ties[len(ties)-1].Line, maxReviewFindings)
	}
}

// TestReviewPlanCapKeepsBlockingFindingEveryPath pins the cap's ranking
// (issue #385) on the paths the section-only test above leaves open: the
// inline (non-section) parse form, the first line past the cap, the shape of
// the displacement (an earlier high survives and only lows give way) and the
// honest verbose review, which must still pass.
func TestReviewPlanCapKeepsBlockingFindingEveryPath(t *testing.T) {
	// sectionAnswer builds a findings section of n bullets: every one a low
	// nit, except a single finding of the given severity at line at.
	sectionAnswer := func(severity string, at, n int) string {
		var b strings.Builder
		b.WriteString("## Findings\n\n")
		for i := 1; i <= n; i++ {
			if i == at {
				fmt.Fprintf(&b, "- [%s] app/cap.go:%d - the finding that matters\n", severity, i)
			} else {
				fmt.Fprintf(&b, "- [low] app/cap.go:%d - nit %d\n", i, i)
			}
		}
		return b.String()
	}
	// inlineAnswer writes the same findings in the inline (non-section)
	// form, one per line, so the parse path that never sees a section
	// header is exercised at the cap too.
	inlineAnswer := func(severity string, at, n int) string {
		var b strings.Builder
		b.WriteString("Review notes follow.\n\n")
		for i := 1; i <= n; i++ {
			if i == at {
				fmt.Fprintf(&b, "[%s] app/cap.go:%d - the finding that matters\n", severity, i)
			} else {
				fmt.Fprintf(&b, "[low] app/cap.go:%d - nit %d\n", i, i)
			}
		}
		return b.String()
	}
	const n = maxReviewFindings + 10 // 60 findings: the cap cuts ten of them
	server := commentsServer(t, reportComments(0), http.StatusOK)
	ghEnv(t, server)

	// The first finding past the cap is a high: it is kept, the action is a
	// fix run, and the comment names exactly that finding with no pass
	// label — the false pass the cap used to produce.
	findings, sawSection := parseFindings(sectionAnswer("high", maxReviewFindings+1, n))
	if len(findings) != maxReviewFindings || len(blockingFindings(findings)) != 1 {
		t.Fatalf("a high on the first line past the cap = %d findings, %d blocking, want the cap and the high kept",
			len(findings), len(blockingFindings(findings)))
	}
	action, body, _, err := reviewPlan(newGitHub(), 7,
		Result{Stop: stopDone, Model: "deepseek-ai/deepseek-v4.1-flash"}, true, findings, sawSection)
	if err != nil {
		t.Fatal(err)
	}
	if action != "fix" {
		t.Fatalf("action = %q, want fix", action)
	}
	if want := command + " fix: 1 blocking finding - high app/cap.go:51"; !strings.HasPrefix(body, want) {
		t.Errorf("the fix comment = %q, want it to start with %q", body, want)
	}
	if strings.Contains(body, reviewPassedLabel) {
		t.Errorf("the fix comment carries the pass label: %q", body)
	}
	assertReviewBodyClean(t, body)

	// The inline (non-section) parse path keeps a late high too: a review
	// that never reached the section cannot hide its blocking finding past
	// the cap by leaving the header out.
	inline, sawInline := parseFindings(inlineAnswer("high", n, n))
	if sawInline {
		t.Fatal("an inline-only answer reported a section header")
	}
	if len(blockingFindings(inline)) != 1 {
		t.Fatalf("an inline high past the cap = %d blocking findings, want 1: %+v", len(blockingFindings(inline)), inline)
	}
	action, _, _, err = reviewPlan(newGitHub(), 7,
		Result{Stop: stopDone, Model: "deepseek-ai/deepseek-v4.1-flash"}, true, inline, sawInline)
	if err != nil {
		t.Fatal(err)
	}
	if action != "fix" {
		t.Fatalf("an inline high past the cap: action = %q, want fix", action)
	}

	// Displacement keeps its shape: an earlier high survives a flood of
	// lows that fills the rest of the cap, and only lows give way — every
	// kept low is one that arrived before the high.
	var b strings.Builder
	b.WriteString("## Findings\n\n")
	fmt.Fprintf(&b, "- [high] app/cap.go:10 - the early high\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "- [low] app/cap.go:%d - nit %d\n", 100+i, i)
	}
	displaced, _ := parseFindings(b.String())
	if len(displaced) != maxReviewFindings {
		t.Fatalf("displacement = %d findings, want the cap", len(displaced))
	}
	keptHigh := false
	for _, f := range displaced {
		if f.Severity == "high" {
			keptHigh = true
			continue
		}
		// The flood of lows fills the rest of the cap in arrival order, so
		// the ones the cap gives way are the last to arrive: no low past
		// the forty-ninth nit survives.
		if f.Line > 100+maxReviewFindings-1 {
			t.Errorf("a low that arrived late survived the cap: %+v", f)
		}
	}
	if !keptHigh {
		t.Errorf("an earlier high did not survive the displacement: %+v", displaced)
	}

	// A clean verbose review still passes: sixty low findings truncate to
	// the cap, nothing blocks, and the label is added — the ranking only
	// rescues findings that change the decision, it does not punish length.
	clean, sawClean := parseFindings(sectionAnswer("", 0, n))
	if len(clean) != maxReviewFindings || len(blockingFindings(clean)) != 0 {
		t.Fatalf("a clean 60-low answer = %d findings, %d blocking, want the cap and no blocking",
			len(clean), len(blockingFindings(clean)))
	}
	action, body, _, err = reviewPlan(newGitHub(), 7,
		Result{Stop: stopDone, Model: "deepseek-ai/deepseek-v4.1-flash"}, true, clean, sawClean)
	if err != nil {
		t.Fatal(err)
	}
	if action != "label" || !strings.Contains(body, reviewPassedLabel) {
		t.Errorf("a clean 60-low answer = action %q, label present %v, want the pass label added", action, strings.Contains(body, reviewPassedLabel))
	}
}

// Issue #458: the diff fetch is bounded. A body larger than diffReadCap
// truncates with a visible note (an incomplete diff the reviewer can see,
// not an opaque runner failure), reports the truncation to its caller, and
// leaves valid UTF-8; a body under the cap comes back byte-for-byte
// unchanged and reports no truncation.
func TestDiffTruncatesOversizedBody(t *testing.T) {
	body := strings.Repeat("diff --git a/app/x.go b/app/x.go\n+++ b/app/x.go\n@@\n", 30000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	ghEnv(t, server)
	got, truncated, err := newGitHub().diff(458)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !truncated {
		t.Error("a body over the cap did not report the truncation")
	}
	if len(got) > diffReadCap+128 {
		t.Errorf("diff returned %d bytes, far over the %d cap", len(got), diffReadCap)
	}
	note := diffFetchNote()
	if !strings.HasSuffix(strings.TrimSpace(got), note) {
		t.Errorf("truncated diff does not end with the note %q", note)
	}
}

// A truncated fetch never splits a UTF-8 rune (issue #458 review finding):
// the cut backs off to a rune boundary, so the text handed to the reviewer
// is valid whatever byte the cap lands on.
func TestDiffTruncationKeepsUTF8Valid(t *testing.T) {
	// A three-byte rune at every alignment: padding of 0, 1 and 2 bytes puts
	// the cap inside one of them.
	for pad := 0; pad < 3; pad++ {
		body := strings.Repeat("x", diffReadCap-1+pad) + strings.Repeat("\u2192", 100000)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, body)
		}))
		ghEnv(t, server)
		got, truncated, err := newGitHub().diff(458)
		server.Close()
		if err != nil {
			t.Fatalf("pad %d: diff: %v", pad, err)
		}
		if !truncated {
			t.Fatalf("pad %d: a body over the cap did not report the truncation", pad)
		}
		if !utf8.ValidString(got) {
			t.Errorf("pad %d: the truncated diff is not valid UTF-8", pad)
		}
		if !strings.HasSuffix(strings.TrimSpace(got), diffFetchNote()) {
			t.Errorf("pad %d: the truncated diff does not end with the fetch note", pad)
		}
	}
}

// The non-200 error branch reads the body through a bound too (issue #458
// review finding): an enormous error body must not be read whole on its way
// into a 300-byte message.
func TestDiffErrorBodyIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, strings.Repeat("e", 4<<20))
	}))
	defer server.Close()
	ghEnv(t, server)
	_, truncated, err := newGitHub().diff(458)
	if err == nil {
		t.Fatal("a non-200 fetch must be an error")
	}
	if truncated {
		t.Error("a failed fetch reported a truncation")
	}
	if len(err.Error()) > 1024 {
		t.Errorf("the error message is %d bytes, way over the 300-byte excerpt it prints", len(err.Error()))
	}
}

func TestDiffUnderCapUnchanged(t *testing.T) {
	body := "diff --git a/app/x.go b/app/x.go\n+++ b/app/x.go\n@@ -1 +1 @@\n-1\n+2\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	ghEnv(t, server)
	got, truncated, err := newGitHub().diff(458)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if truncated {
		t.Error("an under-cap body reported a truncation")
	}
	if got != body {
		t.Errorf("a diff under the cap was rewritten: got %q, want %q", got, body)
	}
	if strings.Contains(got, "truncated") {
		t.Error("an under-cap diff carries a truncation note")
	}
}

// A fetch truncated at diffReadCap still shows its fetch note in the brief
// (issue #458 review finding): capDiff would cut the note off at ~1 MiB, so
// the brief reserves room for it instead.
func TestCappedDiffForBriefKeepsFetchNote(t *testing.T) {
	body := strings.Repeat("diff --git a/app/x.go b/app/x.go\n+++ b/app/x.go\n@@\n+"+strings.Repeat("y", 90)+"\n", 20000)
	if len(body) <= diffReadCap {
		t.Fatalf("test diff must exceed diffReadCap: %d bytes", len(body))
	}
	fetched, truncated, err := func() (string, bool, error) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, body)
		}))
		defer server.Close()
		ghEnv(t, server)
		return newGitHub().diff(458)
	}()
	if err != nil || !truncated {
		t.Fatalf("diff: truncated=%t err=%v", truncated, err)
	}
	got := cappedDiffForBrief(fetched, true)
	if len(got) > maxReviewDiffBytes {
		t.Errorf("the capped diff is %d bytes, over the %d cap", len(got), maxReviewDiffBytes)
	}
	if !strings.Contains(got, diffFetchNote()) {
		t.Errorf("the brief's capped diff lost the fetch note %q", diffFetchNote())
	}
	if !strings.HasSuffix(strings.TrimSpace(got), diffFetchNote()) {
		t.Errorf("the fetch note is not the last thing the reviewer reads")
	}
	// An untruncated fetch is capDiff's business alone.
	if small := "diff --git a/app/x.go b/app/x.go\n"; cappedDiffForBrief(small, false) != small {
		t.Error("an untruncated small diff was rewritten")
	}
}

// review-report must not drop a fenced finding on a file past the fetch cut
// as a quote (issue #458 review finding): a truncated diff names only the
// files before the cap, so it is treated as no diff at all and every finding
// is kept.
func TestReviewReportKeepsFindingsOnTruncatedDiff(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"harness", "review-report", "7"}
	// One small file, then a huge one the cap cuts inside, then a third
	// file wholly past the cut: app/x/main.go and app/late/main.go have
	// their headers in the fetched prefix, app/past/main.go does not, so a
	// diff-path filter built from the prefix would treat a finding on it as
	// a quote and drop it.
	late := "diff --git a/app/late/main.go b/app/late/main.go\n--- a/app/late/main.go\n+++ b/app/late/main.go\n@@ -1 +1 @@\n" +
		strings.Repeat("+ filler line for the oversized tail of the diff\n", 40000)
	past := "diff --git a/app/past/main.go b/app/past/main.go\n--- a/app/past/main.go\n+++ b/app/past/main.go\n@@ -1 +1,2 @@\n context\n+changed\n"
	diffBody := "diff --git a/app/x/main.go b/app/x/main.go\n--- a/app/x/main.go\n+++ b/app/x/main.go\n@@ -1,2 +1,3 @@\n context\n" + late + past
	server := reviewServer(t, map[string]string{
		"/repos/RichardoC/factory-repo/pulls/7":           `{"number":7,"title":"Review target","body":"The change.","state":"open","head":{"ref":"codeaf/issue-3-42"},"base":{"ref":"main"}}`,
		"/repos/RichardoC/factory-repo/issues/7/comments": `[]`,
	}, diffBody, nil)
	t.Cleanup(server.Close)
	ghEnv(t, server)
	// The finding sits in a closed fence outside a "## Findings" section:
	// that is where the diff-path filter applies (inside the section it is
	// exempt, issue #334), so it is the case a prefix-only path set would
	// wrongly drop as a quote.
	writeReviewResult(t, stopDone, "The one blocking problem:\n\n```\n- [high] app/past/main.go:2 - a real bug past the fetch cut\n```\n")
	reviewLedger(t)
	t.Setenv("REVIEW_COMMENT_PATH", filepath.Join(t.TempDir(), "comment.md"))
	out, err := captureStdout(t, runReviewReport)
	if err != nil {
		t.Fatalf("review-report on a truncated diff: %v", err)
	}
	if !strings.Contains(out, "blocking=1") {
		t.Errorf("stdout = %q; want the finding on the file past the cut to block", out)
	}
}
