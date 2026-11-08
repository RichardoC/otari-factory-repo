// review.go supports the independent review pass (issue #32): after publish
// opens or updates a pull request, a separate CodeAF run reviews it in
// read-only mode on a model other than the one that wrote the patch. This
// file parses the reviewer's findings, decides what follows — a fix run, a
// pass label, or a report-only comment — and renders that comment. Everything
// here is standard library only.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// reviewPassedLabel marks a pull request whose independent review found
// nothing blocking, so a person or a merge rule can merge with confidence.
const reviewPassedLabel = "codeaf-review-passed"

// maxReviewFindings caps how many findings are parsed from one review, so a
// hostile answer cannot grow the comment without bound; the rest are ignored.
const maxReviewFindings = 50

// maxReviewDiffBytes caps the diff embedded in the review brief.
const maxReviewDiffBytes = 60000

// Finding is one finding of the independent review, parsed from the
// machine-readable section of the reviewer's answer.
type Finding struct {
	Severity   string // as written: high, medium, low, or an unknown word
	Path       string
	Line       int
	Text       string
	Recognized bool // Severity is one of high, medium or low

	// fenced marks a finding read from inside a markdown code fence (issue
	// #224). Unset means the finding was read from prose, where nothing is
	// ever dropped. It is parser-internal: it says how the finding was
	// found, not what it is, so nothing renders it.
	fenced bool

	// quoted marks a fenced finding whose fence the parser saw closed
	// (issue #224 follow up): a quoted region is a fence that ends where
	// the reviewer wrote it to end. A fence left open — the reviewer never
	// wrote the closing delimiter — is not a confirmed quote, however much
	// it looks like one: only a match against the contract's own example
	// bullet can drop a finding inside it, never the path filter. Like
	// fenced it is parser-internal.
	quoted bool
}

var (
	// reviewSectionHeader opens the machine-readable section: a markdown
	// header whose first word is "findings", case-insensitive.
	reviewSectionHeader = regexp.MustCompile(`(?i)^#{1,6}\s*findings\b`)
	// markdownHeader is any markdown header line; it closes the section.
	markdownHeader = regexp.MustCompile(`^#{1,6}(\s|$)`)
	// chainedFinding opens the next finding after a `;` on the same line
	// (issue #111): optional whitespace, an optional short connective word
	// and its whitespace, then a recognized severity in brackets followed by
	// the whitespace before its location or text. An unrecognized word after
	// a `;` is prose, not a boundary.
	//
	// The connective is what a reviewer writes between two findings on one
	// line -- "; and [medium] ..." -- and it is why the boundary is read
	// before the text is taken (issue #335): the second finding's token sits
	// past the `;` that ends the first finding's text, so the exact boundary
	// the issue-#111 logic recognizes still splits the chain. Only these few
	// words are accepted, so ordinary prose after a `;` stays in the text.
	chainedFinding = regexp.MustCompile(`(?i)^\s{0,16}(?:(?:and|then|also|plus|next)[ \t]+)?\[(?:high|medium|low)\][ \t]`)
)

// parseFindings reads the findings out of a reviewer's answer (issues #32
// and #49). Two passes run over every line, sharing one cap:
//
//   - the section pass (issue #32): a markdown section whose header line
//     matches ^#{1,6}\s*findings\b case-insensitively, holding one bullet
//     per finding — [severity], a path, a line number, a separator and the
//     text; malformed bullets are ignored, and sawSection reports whether
//     the header was present at all;
//   - the inline pass (issues #49 and #111): findings written anywhere in
//     the answer as [severity] path:line - text — several to a line,
//     separated by `;` — or as [severity] text with no location, which only
//     counts for a recognized severity; the forms a review that never
//     reached the section still uses (run 39010363221 wrote
//     "Findings: [high] …" into its result and was read as a review with
//     no findings).
//
// The same finding found by both passes is kept once. Findings are capped at
// maxReviewFindings; text and path are never truncated at parse time — the
// comment renderer escapes and caps them.
//
// The cap keeps the findings that can change the review's decision (issue
// #385). Truncating in arrival order let a verbose or hostile answer list
// fifty [low] findings and then the one [high] that mattered, so the cap
// dropped the blocking finding and reviewPlan added the pass label to a
// review that did not pass. A finding is now kept ahead of the ones it
// outranks — high, then medium, then an unrecognized severity (which withholds
// the label too), then low, which only decides whether the comment carries a
// table — and a tie keeps the earlier finding, so an answer under the cap
// parses in arrival order exactly as before.
//
// The pass is fenced-code aware (issue #224): a finding read from inside a
// ``` or ~~~ fence is dropped when it is quoted text and kept when it is a
// report. parseFindingsIn carries that rule, and this wrapper asks for it
// with no diff in hand.
func parseFindings(answer string) (findings []Finding, sawSection bool) {
	return parseFindingsIn(answer, nil)
}

// parseFindingsIn is parseFindings with the pull request's changed paths
// known (issue #224). known is the set of paths the PR's diff touches, or
// nil when the diff is not at hand — a review with no known paths behaves
// exactly as before this issue.
//
// Fenced code is where a reviewer quotes rather than reports: the echoed
// output contract, the patch itself, another tool's output. The fence state
// is tracked by the run that opened it, so a quoted line that merely looks
// like a delimiter does not desync it — only a run of the same marker, at
// least as long, at the same indentation closes the fence — and a delimiter
// line is itself parsed, judged with the state it opens. A finding that
// falls inside a fence is dropped when it is a quote and kept when it is a
// report:
//
//   - it is a phantom echo of the harness's own contract when its bullet is
//     exactly the example bullet reviewOutputContract embeds — the only
//     bullet in the brief that spells out a file that does not exist;
//   - it is not real when the PR's diff does not touch its path — a file no
//     hunk changes is quoted text, not a place to fix;
//   - anything else is kept, because a reviewer may put a real finding in a
//     fence and dropping it would be a false pass.
//
// The quoted-path filter never applies to a finding the section pass read:
// a `## Findings` bullet is dropped only when it is the contract echo
// (issue #334) — the instructions promise the section is always read, and
// dropping a real blocking bullet there, because it sat inside a closed
// fence and named an untouched path, yielded a false pass label.
//
// Outside a fence nothing is dropped: the unfenced leniency of issue #49
// stands, contract echo and absent path included.
func parseFindingsIn(answer string, known map[string]bool) (findings []Finding, sawSection bool) {
	inSection := false
	// The fences the answer holds, computed once over the whole answer
	// before any finding is read. A quoted region is delimited by its
	// opening and its closing line, and whether the reviewer wrote the
	// closing line is known only after the region ends, while the findings
	// sit between the two (issue #224 follow up). So the state a finding on
	// a line is judged with is looked up from the fences that cover that
	// line, and a fence that never closes covers the rest of the answer as
	// an unconfirmed region rather than as a quote.
	spans := parseFences(strings.Split(answer, "\n"))
	for i, raw := range strings.Split(answer, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		// The fence state the lines of this answer are read in, and the
		// region a finding on this line falls in: a fence the parser saw
		// opened earlier, and the one the reviewer saw closed. A line that
		// is itself a delimiter is judged with the state it toggles to — a
		// closing delimiter line ends the region it is in, an opening
		// delimiter line starts it — which is the state the line opens, the
		// one the parser has when it reads the bullet written beside the
		// marker.
		inFence, quoted := fenceStateAt(spans, i)
		if _, span := fenceAt(spans, i); span != nil && span.open == i {
			// The line opens a fence: the lines after it are fenced, but the
			// bullet written beside the marker itself is prose — the state
			// the line opens with (issue #224).
			inFence, quoted = false, false
		}
		switch {
		case reviewSectionHeader.MatchString(line):
			sawSection = true
			inSection = true
			continue
		case markdownHeader.MatchString(line):
			inSection = false
			continue
		}
		if inSection {
			if f, ok := parseFinding(line); ok {
				f.fenced, f.quoted = inFence, quoted
				// A finding written in the section is always read (issue
				// #334): the quoted-path filter never reaches a section
				// finding, so a real blocking finding inside a closed fence
				// cannot end the review with the pass label. Only the
				// contract-echo phantom — the one bullet that names a file
				// this repository does not have — is still a quote here.
				// The inline pass below reads the same line too; this pass
				// has kept the finding first, so a duplicate copy there is
				// dropped — and the same exemption covers the inline pass
				// itself, for the written forms this bullet parser misses.
				if !quotedEcho(f) {
					findings = addFinding(findings, f)
				}
			}
		}
		for _, f := range parseInlineFindings(line) {
			f.fenced, f.quoted = inFence, quoted
			// The section's promise (issue #334) covers findings in
			// every written form: inside the section this pass is exempt
			// from the quoted-path filter too — only the contract-echo
			// phantom is still a quote there — so an inline-form finding
			// (prose before the severity token, a chained finding) inside
			// a fenced section cannot end the review with the pass label.
			// Outside the section the full filter stands.
			dropped := dropQuotedFinding(f, known)
			if inSection {
				dropped = quotedEcho(f)
			}
			if !dropped {
				findings = addFinding(findings, f)
			}
		}
	}
	return findings, sawSection
}

// fenceSpan is one code fence in the answer: the lines its delimiters sit on
// and whether the reviewer wrote the closing one. From its opening line to
// its closing line inclusive its lines are inside the fence; an unclosed
// fence runs from its opening line to the end of the answer.
type fenceSpan struct {
	open, close int  // line indices: close is -1 when the fence never closes
	closed      bool // the reviewer wrote the closing delimiter
}

// covers reports whether the line at index i is inside this fence.
func (s fenceSpan) covers(i int) bool {
	return i >= s.open && (s.close < 0 || i <= s.close)
}

// parseFences finds the code fences in the answer's lines (issue #224 follow
// up), before any finding is parsed: a fence opens on a delimiter line that
// is not already inside one and closes on the first later delimiter line
// that repeats its marker, is at least as long as the run that opened it and
// is indented no further (fenceRun.closes). A delimiter line that cannot
// close the fence it is in is text the fence quotes — an indented sample out
// of a diff hunk, a shorter run — and does not desync the state. A fence
// whose closing delimiter never arrives is returned with closed=false: it
// runs to the end of the answer, and the rule that reads it — an unterminated
// fence is not a confirmed quote — is applied by the caller.
func parseFences(lines []string) []fenceSpan {
	var spans []fenceSpan
	var open *fenceSpan
	var run fenceRun
	for i, raw := range lines {
		line := strings.TrimSuffix(raw, "\r")
		marker, indent, ok := fenceDelimiter(line)
		if ok == 0 {
			continue
		}
		length := fenceRunLength(strings.TrimLeft(line, " \t"))
		switch {
		case open == nil:
			run = fenceRun{open: true, marker: marker, indent: indent, length: length}
			spans = append(spans, fenceSpan{open: i, close: -1})
			open = &spans[len(spans)-1]
		case !run.closes(marker, indent, length):
			// A delimiter that cannot close the fence it is in is quoted
			// text, not a toggle: the fence state stays where it was.
		default:
			open.close, open.closed = i, true
			open, run = nil, fenceRun{}
		}
	}
	return spans
}

// fenceAt returns the fence covering the line at index i, and whether the
// line is inside a fence at all. The fences are disjoint, so the first cover
// is the only one.
func fenceAt(spans []fenceSpan, i int) (bool, *fenceSpan) {
	for j := range spans {
		if spans[j].covers(i) {
			return true, &spans[j]
		}
	}
	return false, nil
}

// fenceStateAt reports the fence state of the line at index i: whether it is
// inside a fence at all, and whether it is inside one the reviewer closed —
// the region the path filter may read as a quote.
func fenceStateAt(spans []fenceSpan, i int) (inFence, quoted bool) {
	if in, s := fenceAt(spans, i); in {
		return true, s.closed
	}
	return false, false
}

// fenceRun is the code fence the parser is inside, as the run that opened it
// (issue #224 follow up): the run's marker character, its indentation in
// spaces, and its length. A zero fenceRun is "not in a fence".
type fenceRun struct {
	open   bool
	marker byte
	indent int
	length int
}

// closes reports whether a delimiter line ends this fence, and so whether the
// fence state is toggled rather than a quoted line read as a delimiter. It
// follows the CommonMark rules for the run it is in: the closing delimiter
// repeats the opening marker character, is at least as long as the run that
// opened the fence, and is indented no further than the delimiter that opened
// it. A line that only looks like a delimiter — a shorter run, another
// marker, an indented sample out of a quoted diff hunk — is text inside the
// fence and leaves the state alone, which is what keeps a fence the reviewer
// wrote from being closed by the code it quotes.
func (f fenceRun) closes(marker byte, indent, length int) bool {
	return marker == f.marker && length >= f.length && indent == f.indent
}

// fenceDelimiter reports whether a line opens or closes a markdown code
// fence (issue #224): three or more backticks or tildes, indented at most
// three spaces as CommonMark allows — the indent rule stated here is the one
// the code applies, and a deeper indentation is an indented code block, text
// inside the fence. The marker and the run's length are returned for the
// caller to judge against the fence it is in: fenceDelimiter alone does not
// say that a line toggles the state, only that it looks like a delimiter.
func fenceDelimiter(line string) (marker byte, indent, ok int) {
	indent = 0
	for indent < len(line) && (line[indent] == ' ' || line[indent] == '\t') {
		indent++ // a tab counts as one column, the same leniency as the rest of the parser
	}
	if indent > 3 { // CommonMark: a fence may be indented at most three spaces
		return 0, 0, 0
	}
	if len(line) < indent+3 || (line[indent] != '`' && line[indent] != '~') {
		return 0, 0, 0
	}
	marker = line[indent]
	if fenceRunLength(line[indent:]) < 3 {
		return 0, 0, 0
	}
	return marker, indent, 1
}

// fenceRunLength is the length of the run of marker characters a fence
// delimiter opens with: the line has been trimmed of its indentation and
// fenceDelimiter has already checked that the run is three characters or
// more.
func fenceRunLength(line string) int {
	i := 0
	for i < len(line) && line[i] == line[0] {
		i++
	}
	return i
}

// quotedEcho reports whether a finding restated inside a fence is the
// harness's own output contract echoed back — the [high]
// app/zip/queue.go:42 example bullet, a file this repository does not have.
// It is the one thing a fenced finding can be dropped for even in the
// `## Findings` section (issue #334); everywhere else it is the first of
// the two quote cases dropQuotedFinding rules on.
func quotedEcho(f Finding) bool {
	return f.fenced && contractExampleFinding(f)
}

// dropQuotedFinding reports whether a fenced finding is quoted text rather
// than a report (issue #224). Nothing unfenced is ever dropped — the issue
// #49 leniency stands, so an unfenced echo of the contract still parses —
// and a fenced finding is dropped only when it is certainly a quote:
//
//   - it restates the output contract's own example (contractExampleFinding):
//     the one bullet in the brief that spells out a file that does not exist.
//     The test is not an exact string compare but a field-wise match that
//     tolerates the incidental differences of an echo — a line number nudged
//     in a paraphrase, whitespace, case, markdown emphasis, a trailing stop —
//     because a quote of the example is a quote however slightly the
//     reviewer reprinted it;
//   - the PR's diff is in hand and does not touch its path: a location no
//     hunk changes cannot be fixed here, so it is text about another change.
//
// The second test reads a region the parser saw closed, and only that
// (Finding.quoted): a fence left open is not a confirmed quote — the
// reviewer may have gone on to write the real report under it, every line
// judged fenced because the closing delimiter never arrived — so the verdict
// on its path is not drawn, and only the contract echo above can drop
// anything inside it. The first test holds in either state: an echo of the
// brief's example is a quote whether or not the reviewer forgot a delimiter.
//
// A fenced finding that is none of these is a real report and is kept, even
// on a path outside the diff, because the reviewer's answer is the only
// signal there is and a false pass costs it.
func dropQuotedFinding(f Finding, known map[string]bool) bool {
	if !f.fenced {
		return false
	}
	if quotedEcho(f) {
		return true // the phantom: the brief's example echoed back
	}
	if !f.quoted {
		// An open fence: the region ran on without its delimiter, so the
		// text inside it is not a quote the parser can trust, and dropping a
		// report that happens to sit on a path outside the diff would be the
		// false pass this rule exists to prevent.
		return false
	}
	// A path the diff does not touch cannot be fixed by this pull request.
	// The verdict is only drawn when the diff is known: with no diff in hand
	// the parser cannot tell a quote about another file from a real report
	// about one, and the failure to read the diff must never lose findings.
	if known != nil && f.Path != "" && !pathInDiff(f.Path, known) {
		return true
	}
	return false
}

// contractExampleFinding reports whether a fenced finding restates the
// output contract's own example bullet (issue #224). The match is exact on
// the severity and the path — an echo reprints the `[high]
// app/zip/queue.go` the brief spelled out, and a genuine finding about
// app/zip/queue.go is not a thing this repository has — and tolerant on the
// two fields a reviewer reprints with incidental changes: the location on
// the line, and the text's case, whitespace and trailing punctuation or
// markdown emphasis.
func contractExampleFinding(f Finding) bool {
	if !strings.EqualFold(f.Severity, contractExampleSeverity) {
		return false
	}
	path, text := strings.TrimSpace(f.Path), f.Text
	if path == "" {
		// A bullet without a line number (`[high] app/zip/queue.go - text`)
		// parses as an unlocated finding: the location never completed, so
		// the whole `app/zip/queue.go - text` sits in Text. Take the
		// example's path off the front when it is there, so an echo that
		// dropped the line number is recognised as the same quote.
		if !strings.HasPrefix(text, contractExamplePath) {
			return false
		}
		text = strings.TrimLeft(strings.TrimPrefix(text, contractExamplePath), ":- \t")
		return f.Line == 0 && equalQuotedText(text, contractExampleText)
	}
	if path != contractExamplePath {
		return false
	}
	// The line number is where a paraphrase drifts: an echo that renumbers
	// the example's line (43 for 42) is the same quote. Only a location
	// that is unset or within a step of the example's is read as the echo;
	// a plausible other line is left to the diff filter below.
	if f.Line != 0 && abs(f.Line-contractExampleLine) > contractExampleLineSlack {
		return false
	}
	return equalQuotedText(text, contractExampleText)
}

// contractExampleLineSlack bounds how far an echo may nudge the example's
// line number and still be read as the same quote (issue #224 finding 1).
const contractExampleLineSlack = 1

// equalQuotedText reports whether text restates quoted is the same as quote
// after the normalisation an echo tolerates: surrounding whitespace,
// internal runs of whitespace, ASCII case, markdown emphasis markers, and
// trailing sentence punctuation. A reviewer reprinting a bullet is not
// expected to reproduce its bytes.
func equalQuotedText(text, quoted string) bool {
	return normalizeQuotedText(text) == normalizeQuotedText(quoted)
}

// normalizeQuotedText folds the incidental differences an echo tolerates
// (issue #224): ASCII case, runs of whitespace, the markdown emphasis and
// code markers a reprint may wrap a word in, and trailing sentence
// punctuation. It is deliberately conservative — it never removes
// characters that could carry meaning.
func normalizeQuotedText(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer("`", "", "*", "", "_", "").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimRight(s, ".!,;:\"'")
}

// abs is the absolute value for the small line-number slack next to the
// contract's example line (issue #224).
func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// pathInDiff reports whether the diff touches the finding's path. The test
// is a substring one on purpose, in both directions: diffstat answers with
// the files a patch changes, and a hunk line inside a diff quotes the same
// path as a `path:line` location, so a substring covers both the file a
// finding names and a finding read out of an echoed hunk. A renamed file
// answers to both of its paths because diffPaths kept both.
//
// It errs toward keeping a finding, which is the safe direction: a false
// pass costs a reviewer's answer, a signal the maintainer asked never to
// lose. A finding is only ever judged against a diff that is in hand, and
// an exact path match is checked first so an unusual path that happens to
// be a substring of a diff path still matches itself.
func pathInDiff(path string, known map[string]bool) bool {
	if known[path] {
		return true
	}
	for p := range known {
		if strings.Contains(p, path) || strings.Contains(path, p) {
			return true
		}
	}
	return false
}

// diffPaths is the set of paths a pull request's diff touches, for
// parseFindingsIn. A renamed file answers to both of its paths, and an
// unparseable diff yields nil rather than an empty set: with no paths known
// the parser keeps every finding, which is the behaviour of the review that
// has no diff in hand.
func diffPaths(diff string) map[string]bool {
	if strings.TrimSpace(diff) == "" {
		return nil
	}
	diffs, more := diffstat([]byte(diff))
	if more > 0 {
		// diffstat parsed only the first maxDiffFiles files and counted the
		// rest; the set in hand is partial, and a fenced finding on any file
		// past the cap would be read as a quote about a path the diff does
		// not touch and dropped. A capped listing is no listing: report the
		// paths as unknown, which keeps every finding — the same safe
		// direction as a diff that is missing or unparseable.
		return nil
	}
	if len(diffs) == 0 {
		return nil
	}
	known := make(map[string]bool, len(diffs))
	for _, d := range diffs {
		path := d.Path
		if i := strings.Index(path, " \u2192 "); i >= 0 {
			known[path[:i]] = true
			path = path[i+len(" \u2192 "):]
		}
		known[path] = true
	}
	return known
}

// inlineSeverityToken is a bracketed single word anywhere in a line, the
// shape every inline finding opens with. The word is short and holds no
// spaces or brackets, so a markdown link, a footnote or a task list cannot
// pass for one.
// knownPaths is the known set review-report hands the findings parser: the
// paths the diff touches, or nil when the diff is missing, unparseable or
// incomplete. A diff stopped at diffReadCap (issue #458) names only the
// files before the cut, and diffstat at maxDiffFiles (this issue) only the
// files before the cap, so a set built from either would read a real fenced
// finding past the cut as a quote about a path the diff does not touch and
// drop it — the false pass the path filter exists to prevent. An incomplete
// diff is therefore no diff: the parser gets nil and keeps every finding,
// the same safe direction as a fetch that failed.
func knownPaths(diff string, truncated bool, pr int) map[string]bool {
	if truncated {
		fmt.Fprintf(os.Stderr, "review-report: the diff of %d stopped at %d bytes; findings are parsed without it\n", pr, diffReadCap)
		return nil
	}
	return diffPaths(diff)
}

var inlineSeverityToken = regexp.MustCompile(`\[[A-Za-z0-9_-]{1,24}\]`)

// locationShaped matches text that opens like a path:line location. A token
// followed by such text was meant as a located finding; when the location
// never completes — no separator, no line digits — the line is dropped
// rather than read as a finding whose text is the half-written location.
var locationShaped = regexp.MustCompile(`^\s*\S+:\d+`)

// parseInlineFindings reads the findings written inline on one line (issues
// #49 and #111), in the forms the lenient contract accepts: [severity]
// path:line - text — the shape the section bullets use, with or without a
// bullet — or [severity] text with no location, which only counts when the
// severity word is one of high, medium and low. Several findings may share
// one line, separated by `;` (issue #111): the run that prompted it wrote
// "FINDINGS (3, 2 blocking): [medium] a.go:1 - x; [medium] b.go:2 - y; [low]
// c.go:3 - z" as one paragraph, and each finding must keep its own text.
// Every bracketed token on the line gets its chance, but a token inside the
// text a finding has already taken is that finding's text (issue #335): a
// severity word a finding quotes never opens a second, path-less finding,
// which `blockingFindings` would count and send a fix run after. A located
// bullet inside that text still parses — the same-line form the lenient
// contract accepts — and a `;` before the next finding is a boundary as it
// always was.
func parseInlineFindings(line string) []Finding {
	var out []Finding
	// consumed is the index just past the text the last parsed finding took
	// for itself — its location and text, up to the next chain boundary
	// (issue #335). A token that starts before it sits inside that finding's
	// own text, and the scan is past that text already: a `[medium] severity`
	// the finding quotes would otherwise be read again and mint a second,
	// path-less finding that counts as blocking and starts a fix run nobody
	// wrote. Such a token can still open a finding when it is a complete
	// located bullet, the same-line form
	// `[high] a.go:1 - first and [low] b.go:2 - second` that the lenient
	// contract accepts; what it can never do is fall back to the no-location
	// form, which is the phantom's shape.
	consumed := 0
	for i := 0; i < len(line); {
		m := inlineSeverityToken.FindStringIndex(line[i:])
		if m == nil {
			break // no severity token anywhere in the rest of the line
		}
		start := i + m[0]
		token := line[i+m[0]+1 : i+m[1]-1]
		i += m[1] // every path consumes the token, whether it reads or not
		inText := start < consumed
		rest := line[i:]
		// A `;` that opens the next recognized severity ends this
		// finding (issue #111): the text before it is this finding's,
		// and the scan resumes after the `;`. An unrecognized word
		// after a `;` is not a boundary, so it stays in the text.
		segment, next := rest, -1
		if k := chainBoundary(rest); k >= 0 {
			segment, next = rest[:k], k+1
		}
		// end is where this finding's text ends: the chain boundary, or
		// the end of the line when the finding takes the rest of it.
		end := len(line)
		if next >= 0 {
			end = i + next - 1
		}
		if f, ok := parseFinding("[" + token + "]" + segment); ok {
			out = append(out, f)
			i += next
			consumed = end
			continue
		}
		if next >= 0 {
			// The chain still splits when the finding carries no
			// location: its text ends at the `;` too.
			if f, ok := textOnlyFinding(token, segment); ok && !inText {
				out = append(out, f)
			}
			i += next
			if !inText {
				consumed = end
			}
			continue
		}
		if inText {
			continue // a word inside a parsed finding's text is its text
		}
		if locationShaped.MatchString(rest) {
			continue // a broken location is dropped, never read as text
		}
		if f, ok := textOnlyFinding(token, rest); ok {
			out = append(out, f)
			consumed = end
		}
	}
	return out
}

// chainBoundary reports the index in s of the `;` that separates this
// finding from the next one on the same line, or -1 when no such `;`
// follows. A boundary is only possible where a `[` sits within the
// chainedFinding pattern's reach of the `;`, so a line of plain prose
// with semicolons is scanned without a regex match per semicolon.
func chainBoundary(s string) int {
	for k := 0; k < len(s); k++ {
		if s[k] != ';' {
			continue
		}
		after := s[k+1:]
		if r := strings.IndexByte(after, '['); r < 0 || r > 16 {
			continue
		}
		if chainedFinding.MatchString(after) {
			return k
		}
	}
	return -1
}

// textOnlyFinding reads the no-location form: a recognized severity word,
// whitespace, and text that opens with a letter or digit — the guard that
// keeps bracketed notes like [TODO] fix me, a task list or a stray :42 from
// becoming findings.
func textOnlyFinding(token, rest string) (Finding, bool) {
	lower := asciiLower(token)
	if lower != "high" && lower != "medium" && lower != "low" {
		return Finding{}, false
	}
	if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return Finding{}, false // the text needs whitespace after the severity
	}
	text := strings.TrimLeft(rest, " \t")
	if text == "" {
		return Finding{}, false
	}
	if r, _ := utf8.DecodeRuneInString(text); !unicode.IsLetter(r) && !unicode.IsDigit(r) {
		return Finding{}, false
	}
	return Finding{Severity: token, Text: text, Recognized: true}, true
}

// The ranks capReviewFindings orders findings by when the cap truncates,
// most decision-relevant first: a high or medium finding starts a fix run, an
// unrecognized severity withholds the pass label, and a low finding only
// decides whether the comment carries a table. Adding a rank means revisiting
// reviewPlan's branches; ranks that rank the same keep arrival order.
const (
	findingRankHigh    = iota // starts a fix run
	findingRankMedium         // starts a fix run
	findingRankUnknown        // withholds the pass label
	findingRankLow            // advisory only
)

// findingRank is the decision weight of a finding (issue #385). An
// unrecognized severity outranks low because it withholds the label; a low
// finding outranks nothing, because a review of only low findings can pass.
func findingRank(f Finding) int {
	if !f.Recognized {
		return findingRankUnknown
	}
	switch asciiLower(f.Severity) {
	case "high":
		return findingRankHigh
	case "medium":
		return findingRankMedium
	}
	return findingRankLow
}

// addFinding records one parsed finding unless the same finding is already
// held, and keeps the slice at or below maxReviewFindings (issue #385).
//
// While the findings fit, they are held in arrival order, exactly as the
// reviewer wrote them. Once adding one would pass the cap, the finding that
// gives way is the lowest-ranked one already held — ranking by findingRank,
// and among equals the one that arrived last — which is the decision this
// change exists for: the cap used to truncate in arrival order, so a verbose
// review, or a hostile prompt-injected one, could list fifty [low] findings
// and its only [high] at the end, the cap dropped the high, blockingFindings
// came back empty and reviewPlan added reviewPassedLabel to a review that did
// not pass. Making room by rank means what the cap drops is never a finding
// that would change the decision while a kept one would, however far into the
// answer the reviewer wrote it. An answer under the cap is untouched and still
// parses in arrival order.
func addFinding(findings []Finding, f Finding) []Finding {
	if duplicateFinding(findings, f) {
		return findings
	}
	if len(findings) < maxReviewFindings {
		return append(findings, f)
	}
	// At the cap: f displaces the weakest held finding, unless f is that one.
	worst, rank := 0, findingRank(findings[0])
	for i, g := range findings {
		if r := findingRank(g); r >= rank {
			worst, rank = i, r // ties go to the later finding
		}
	}
	if findingRank(f) >= rank {
		return findings // f is no more decision-relevant than what is held
	}
	findings[worst] = f
	return findings
}

// duplicateFinding reports whether the same finding is already held: the
// severity read case-insensitively, and path, line and text exactly. The
// section pass and the inline pass read the same bullets, so the double is
// dropped and the finding is kept once.
func duplicateFinding(findings []Finding, f Finding) bool {
	for _, g := range findings {
		if asciiLower(g.Severity) == asciiLower(f.Severity) && g.Path == f.Path && g.Line == f.Line && g.Text == f.Text {
			return true
		}
	}
	return false
}

// parseFinding parses one bullet of the findings section: an optional - or *
// bullet, then [severity], then whitespace, then the path (a run of non-space
// characters up to the colon before the line number), digits, then a
// separator — an em dash, an en dash, a double hyphen, a single hyphen or a
// colon, optionally surrounded by spaces — then the text, which is the rest
// of the line and is kept verbatim. Severity is matched case-insensitively
// against high, medium and low and sets Recognized; any other bracketed word
// still parses as a finding with Recognized=false, so a reviewer writing an
// unknown scale is reported rather than silently dropped.
func parseFinding(line string) (Finding, bool) {
	s := line
	if strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "* ") {
		s = s[2:]
	}
	if !strings.HasPrefix(s, "[") {
		return Finding{}, false
	}
	end := strings.IndexByte(s, ']')
	if end < 2 { // "[x]" is the shortest severity
		return Finding{}, false
	}
	severity := s[1:end]
	s = s[end+1:]
	if s == "" || (s[0] != ' ' && s[0] != '\t') {
		return Finding{}, false // the path needs whitespace after the severity
	}
	s = strings.TrimLeft(s, " \t")
	i := 0
	for i < len(s) && s[i] != ':' && s[i] != ' ' && s[i] != '\t' {
		i++
	}
	if i == 0 || i >= len(s) || s[i] != ':' {
		return Finding{}, false // no path, or no colon before the line number
	}
	path := s[:i]
	s = s[i+1:]
	j := 0
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j == 0 {
		return Finding{}, false // no line number
	}
	n, err := strconv.Atoi(s[:j])
	if err != nil || n <= 0 {
		return Finding{}, false
	}
	text, ok := findingText(s[j:])
	if !ok {
		return Finding{}, false
	}
	return Finding{
		Severity:   severity,
		Path:       path,
		Line:       n,
		Text:       text,
		Recognized: asciiLower(severity) == "high" || asciiLower(severity) == "medium" || asciiLower(severity) == "low",
	}, true
}

// findingText reads what follows the line number and returns the text: after
// the digits there must be the separator itself, or whitespace followed by
// it — an em dash, an en dash, a double hyphen, a single hyphen or a colon.
// What remains after the separator is the text.
func findingText(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	if s[0] == ' ' || s[0] == '\t' {
		k := 0
		for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
			k++
		}
		if k == len(s) {
			return "", false // spaces and nothing else
		}
		s = s[k:]
	}
	var text string
	switch {
	case strings.HasPrefix(s, "—"), strings.HasPrefix(s, "–"):
		text = s[len("—"):]
	case strings.HasPrefix(s, "--"):
		text = s[2:]
	case s[0] == '-', s[0] == ':':
		text = s[1:]
	default:
		return "", false // a real separator is required
	}
	if text = strings.TrimLeft(text, " \t"); text == "" {
		return "", false
	}
	return text, true
}

// blockingFindings are the findings that must be fixed before a pull request
// can pass the review: recognized severities high and medium (issue #32).
func blockingFindings(findings []Finding) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.Recognized && (asciiLower(f.Severity) == "high" || asciiLower(f.Severity) == "medium") {
			out = append(out, f)
		}
	}
	return out
}

// hasUnrecognizedSeverity reports whether any finding carries a severity word
// outside high, medium and low: the pass label is withheld until the scale
// the reviewer used is understood.
func hasUnrecognizedSeverity(findings []Finding) bool {
	for _, f := range findings {
		if !f.Recognized {
			return true
		}
	}
	return false
}

// hasLowFindings reports whether any finding is a recognized low.
func hasLowFindings(findings []Finding) bool {
	for _, f := range findings {
		if f.Recognized && asciiLower(f.Severity) == "low" {
			return true
		}
	}
	return false
}

// reviewerModel chooses the model for the reviewer seat (issue #32): the
// review must not share the worker's blind spots, so it never runs on the
// model that wrote the patch. Of the allowed models (models.json), the one
// with role "mid" whose id differs from the worker's is preferred, then the
// first allowed model with a different id. When every allowed model is the
// worker model the choice fails loudly: a review on the same model is
// exactly what issue #32 rules out.
func reviewerModel(cfg Config, workerID string) (Model, error) {
	var other *Model
	for i := range cfg.Models {
		m := cfg.Models[i]
		if m.ID == workerID {
			continue
		}
		if m.Role == "mid" {
			return m, nil
		}
		if other == nil {
			other = &m
		}
	}
	if other != nil {
		return *other, nil
	}
	return Model{}, fmt.Errorf("no allowed model differs from the worker model %q; the reviewer must not run on the worker model", workerID)
}

// ---------------------------------------------------------------- the decision

// reviewPlan decides what follows an independent review (issues #32 and #49):
//
//   - no result at all and no findings from either source: report-only —
//     nothing can be concluded;
//   - a result that is not stop "done": the review is incomplete, and an
//     incomplete review never labels — but findings parsed from the answer
//     or the run record still act (issue #49): blocking findings start a fix
//     run exactly as in a finished review. When findings exist the report
//     says the run ended early after finishing its review (issue #111);
//     only when neither source carries findings does it stay a plain
//     incomplete review, as it did before;
//   - a finished review with high or medium findings: a fix run, started as
//     a continuation and counted against the continuation cap (issue #31);
//     at the cap, report-only and a person takes over;
//   - a finished review with an unrecognized severity: report-only, and the
//     pass label is withheld because the scale was not understood;
//   - a finished review whose pull request is still a draft (issue #237):
//     report-only, and the pass label is withheld because a draft is the
//     pull request whose worker run stopped early — ready for review is
//     what a `done` stop marks — so the work under review is not known
//     finished, and no clean review over the diff alone may say otherwise.
//     A person or a continuation can finish the work and mark the pull
//     request ready, and the review can then run again;
//   - otherwise: label — a clean pass.
//
// The action is the label contract (issue #5): `label` is the only outcome
// that adds codeaf-review-passed, and every other action — `fix` and every
// report-only path above — means the label must not be on the pull request.
// The workflow withdraws it for any action other than label, and publish
// withdraws it when it pushes an update to the pull request: a pass given to
// one head commit never outlives the code it was given for, and the next
// clean review re-adds it.
//
// The comment body is built only from fixed sentences, counters and findings
// whose remote-derived strings are escaped and capped; it never carries the
// run-report marker, so it is never mistaken for a run report, and no raw
// answer text reaches it unescaped. A failing comments or pull request
// fetch is an error, like continuePlan — except a comment listing that stops
// at getList's page cap (issue #416): that is a truncated listing, not a
// failed one, so the findings comment is still built on the budget-spent
// report-only path rather than dropped.
func reviewPlan(gh github, pr int, result Result, haveResult bool, findings []Finding, sawSection bool) (action string, body string, used int, err error) {
	stop := sanitizeStop(result.Stop)
	blocking := blockingFindings(findings)
	if !haveResult && len(findings) == 0 {
		return "report-only", reviewReportBody(
			"The review produced no result: its output is missing or unparseable, so nothing can be concluded from this run. "+
				"No label is added and no fix run starts; a maintainer can re-run the review.",
			nil, result.Model, stopUnknown), 0, nil
	}
	if !readyForReview(stop) && len(blocking) == 0 {
		// Blocking findings start a fix run even on an incomplete review
		// (issue #49); what is left here is a review whose findings are
		// advisory, or none. Findings in the answer or the closing result
		// mean the review finished its work and only the run's stop fell
		// short (issue #111): the report lists them as the review's
		// findings, never as "no findings found" — but the pass label
		// still waits for a done stop.
		if len(findings) > 0 {
			verdict := fmt.Sprintf("The run stopped with reason `%s` after finishing its review: "+
				"the findings below were parsed from its answer or its closing result. "+
				"The run did not end `done`, so no label is added and no fix run starts.", stop)
			return "report-only", reviewReportBody(verdict, findings, result.Model, stop), 0, nil
		}
		verdict := fmt.Sprintf("The review stopped early (stop reason: `%s`), so this is an incomplete review: "+
			"no findings section and no inline findings were found in the answer or the run record, "+
			"nothing is labelled and no fix run starts.", stop)
		return "report-only", reviewReportBody(verdict, findings, result.Model, stop), 0, nil
	}
	if readyForReview(stop) && len(blocking) == 0 {
		if hasUnrecognizedSeverity(findings) {
			return "report-only", reviewReportBody(
				"The review found nothing blocking, but the pass label is withheld because at least one severity "+
					"was not recognized (high, medium or low are); a maintainer reads the findings below and decides.",
				findings, result.Model, stop), 0, nil
		}
		// A draft pull request never takes the pass label (issue #237): a
		// pull request is marked ready only when the run that produced the
		// patch stopped `done`, so a draft is the one whose worker run
		// stopped early — the work under review is not known finished, and
		// no clean review over the diff says otherwise. The review itself is
		// reported (advisory findings included), exactly as the
		// unrecognized-severity case above is; only the label waits, until a
		// person or a continuation finishes the work and the pull request
		// is ready for review.
		var pull item
		if err := gh.get(fmt.Sprintf("/pulls/%d", pr), &pull); err != nil {
			return "", "", 0, err
		}
		if pull.Draft {
			return "report-only", reviewReportBody(
				"Nothing blocking was found, but the pass label is withheld because the pull request is still a draft: "+
					"the run that produced the patch stopped early, so the work under review is not known to be finished. "+
					"A person who continues or finishes the work can mark the pull request ready, and the review can then run again.",
				findings, result.Model, stop), 0, nil
		}
		verdict := fmt.Sprintf("Nothing blocking was found: the %s label was added, so a person or a merge rule can merge with confidence.",
			reviewPassedLabel)
		if !hasLowFindings(findings) {
			return "label", reviewReportBody(verdict, nil, result.Model, stop), 0, nil
		}
		return "label", reviewReportBody(verdict, findings, result.Model, stop), 0, nil
	}
	// Blocking findings start a fix run, whether the review finished (issue
	// #32) or stopped early (issue #49): an incomplete review with findings
	// is a review with findings, not one with none.
	var comments []issueComment
	capped := false
	if err := gh.getList(fmt.Sprintf("/issues/%d/comments?per_page=100", pr), &comments); err != nil {
		if !isPageCapError(err) {
			return "", "", 0, err
		}
		// Issue #416: the listing is truncated, not broken, so it must
		// never cost the findings comment — the only place the blocking
		// severities and file:lines exist. A thread longer than
		// maxListPages pages has certainly spent the fix budget, so the
		// report-only path is taken with the findings in hand, exactly as
		// when the cap is counted; used reports the whole budget.
		capped = true
	}
	if capped {
		used = maxContinuations
	} else {
		used = continuationsUsed(countReports(comments))
	}
	if !capped && !continuationCapReached(used) {
		return "fix", reviewFixBody(blocking, findings, used, result.Model, stop), used, nil
	}
	return "report-only", reviewReportBody(fmt.Sprintf(
		"The automatic fix budget of %d continuations per pull request is spent, so no fix run starts; "+
			"a maintainer can continue by hand with a %s comment. The blocking findings above stay open for a person to act on.",
		maxContinuations, command), findings, result.Model, stop), used, nil
}

// reviewFixBody is the comment that starts a fix run: its first line is the
// command the workflow acts on, then the findings table and a verdict, then
// the footer.
func reviewFixBody(blocking, findings []Finding, used int, model, stop string) string {
	return reviewFixCommand(blocking) + "\n\n" +
		fmt.Sprintf("A fix run starts now, as continuation %d of %d on this pull request; "+
			"it receives the findings above and the issue the pull request closes, and nothing else to do beyond them.\n\n",
			used+1, maxContinuations) +
		reviewTable(findings) + "\n" + reviewFooter(model, stop)
}

// reviewReportBody is a report-only comment: a verdict paragraph, an optional
// findings table and the footer. It never starts a run and never adds a
// label.
func reviewReportBody(verdict string, findings []Finding, model, stop string) string {
	body := verdict + "\n\n"
	if len(findings) > 0 {
		body += reviewTable(findings) + "\n"
	}
	return body + reviewFooter(model, stop)
}

// reviewFixCommand is the first line of a fix comment: the /codeaf command
// the workflow acts on. It is built only from sanitized severity words and
// safe paths, so a hostile finding can neither inject shell nor break out of
// the command line. A finding with no location — the inline form (issue #49)
// — carries its severity word alone.
func reviewFixCommand(blocking []Finding) string {
	parts := make([]string, 0, len(blocking))
	for _, f := range blocking {
		if f.Path == "" && f.Line == 0 {
			parts = append(parts, asciiLower(f.Severity))
			continue
		}
		parts = append(parts, asciiLower(f.Severity)+" "+safeReviewPath(f.Path)+":"+strconv.Itoa(f.Line))
	}
	plural := "s"
	if len(blocking) == 1 {
		plural = ""
	}
	return fmt.Sprintf("%s fix: %d blocking finding%s - %s", command, len(blocking), plural, strings.Join(parts, "; "))
}

// safeReviewPath narrows a finding's path to the characters a repository
// path can contain — letters, digits, slash, dot, underscore, hyphen — and
// caps its length. Anything else, and it is replaced whole: the command line
// is parsed again by the fix run, and a quoted, spaced or metacharacter path
// must never carry injected text through it. The full path still appears in
// the table, escaped.
func safeReviewPath(path string) string {
	const maxPath = 200
	if len(path) > maxPath {
		return "(unsafe path)"
	}
	for i := 0; i < len(path); i++ {
		c := path[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '/' || c == '.' || c == '_' || c == '-'
		if !ok {
			return "(unsafe path)"
		}
	}
	return path
}

// reviewTable renders the findings as a markdown table. Every cell is
// escaped and capped: the severity through tokenCell (a known-shaped word in
// backticks, anything else escaped), the location and the text through
// escapeMD — so a hostile answer cannot inject markdown, mention anyone, or
// carry a raw HTML comment into the posted comment.
//
// A finding with no location — the inline form (issue #49) — has no file to
// name, so its location cell reads `unlocated` instead of the `:0` that a
// path:line of the empty path would otherwise render (issue #190): a fix run
// and a reader both read `:0` as a location, and it names no file.
func reviewTable(findings []Finding) string {
	var b strings.Builder
	b.WriteString("| severity | location | finding |\n|---|---|---|\n")
	for _, f := range findings {
		fmt.Fprintf(&b, "| %s | %s | %s |\n",
			tokenCell(f.Severity, 200), reviewLocation(f), escapeMD(f.Text, 200, true))
	}
	return b.String()
}

// reviewLocation renders one finding's location cell: `path:line` when the
// finding carries a location, and the fixed word `unlocated` when it does
// not, so no cell ever shows a location that names no file (issue #190).
// The path is escaped and capped like every other cell, so a hostile path
// cannot break out of the table or the comment.
//
// A finding is unlocated when its path is empty: the parser gives the
// inline form (issue #49) no path and no line, and every located finding it
// builds has both, so the line alone never decides.
func reviewLocation(f Finding) string {
	// The path decides, not the line: every finding the parser builds
	// without a location carries no path, so a line alone names no file.
	// Asking for a zero line too would let any caller that filled the line
	// in render a bare ":0" — the cell this issue exists to remove.
	if f.Path == "" {
		return "unlocated"
	}
	// A located cell is `path:line`; a zero line never renders, because a
	// line number counts from one. Two shapes reach here with line zero:
	// the straight-through path a test or a future caller may build, and
	// the location-shaped text the parser drops the number from — `:0`
	// itself parses as the path ":" (findingText accepts a `:` separator)
	// and `0:1` as the path "0" — both of which named no file either, so
	// they read `unlocated` rather than as a location (issue #190).
	// A path that is only separators and digits (":", "0") names no file
	// and reads `unlocated`; a real path with no line keeps its path, so a
	// file-level finding still says which file.
	if f.Line <= 0 {
		if strings.Trim(f.Path, ":0123456789") == "" {
			return "unlocated"
		}
		return escapeMD(f.Path, 200, true)
	}
	return escapeMD(f.Path, 200, true) + ":" + strconv.Itoa(f.Line)
}

// reviewFooter ends the review comment: the reviewer model, the sanitized
// stop word, this run's metered spend from the proxy ledger, and a link to
// the workflow run with its diagnostics artifact when one was uploaded. The
// ledger is written by the run job and the model id comes from its result,
// so both are treated as untrusted: an unreadable ledger reads as unknown,
// and the model id is escaped before it is rendered.
func reviewFooter(model, stop string) string {
	stop = sanitizeStop(stop) // defensive: callers pass sanitized words already
	var b strings.Builder
	b.WriteString("---\n\nReviewer model: ")
	if model == "" {
		b.WriteString("(unknown)")
	} else {
		b.WriteString("`" + escapeMD(model, 200, true) + "`")
	}
	b.WriteString(" · stop: `" + stop + "`")
	if lines, err := readLedger(os.Getenv("LEDGER_PATH")); err != nil {
		b.WriteString(" · metered spend this run: (unknown)")
	} else {
		b.WriteString(" · metered spend this run: " + money(ledgerTotal(lines)))
	}
	// The link is built from the runner's own variables and a digit-only
	// artifact id, like the run report's link; it is not remote text.
	server := os.Getenv("GITHUB_SERVER_URL")
	if server == "" {
		server = "https://github.com"
	}
	if repo, run := os.Getenv("GITHUB_REPOSITORY"), os.Getenv("GITHUB_RUN_ID"); repo != "" && run != "" {
		link := fmt.Sprintf("%s/%s/actions/runs/%s", server, repo, run)
		if id := diagnosticsArtifact(); digitsOnly(id) {
			link += "/artifacts/" + id
		}
		fmt.Fprintf(&b, " · [Workflow run](%s)", link)
	}
	b.WriteString("\n")
	return b.String()
}

// ---------------------------------------------------------------- commands

// The output contract's own example bullet, spelled out as the fields a
// parse of it yields (issue #224). It is the one bullet in the brief that
// names a file which does not exist, so a reviewer that echoes the contract
// back in a fence mints exactly this finding — the phantom a blocking
// verdict used to be built on. The constants below must stay in step with
// the first bullet of reviewOutputContract, and a test pins them to it.
const (
	contractExampleSeverity = "high"
	contractExamplePath     = "app/zip/queue.go"
	contractExampleLine     = 42
	contractExampleText     = "the mutex is released twice under the error path, so a second request deadlocks"
)

// reviewOutputContract restates, at the end of the review brief, the one
// part of the answer the harness parses (issue #32).
const reviewOutputContract = "# The output the harness parses\n\n" +
	"End your answer with a section exactly titled `## Findings`, holding one bullet per finding, " +
	"and write nothing after it:\n\n" +
	"- [high] app/zip/queue.go:42 - the mutex is released twice under the error path, so a second request deadlocks\n" +
	"- [low] the changelog does not mention the new flag\n\n" +
	"Severity is high, medium or low. A finding without a file and line is still one bullet. " +
	"When the review is clean, end with the section holding the single word `none` and nothing else:\n\n" +
	"## Findings\n\nnone\n"

// capDiffFloorBytes is the smallest body budget a file's section keeps when
// the cap forces allocation: every file in the diff contributes some
// content, even a many-file patch larger than the whole cap (issue #231).
const capDiffFloorBytes = 1024

// capDiffMaxNoteFiles bounds how many clipped file names the truncation
// note lists; past that the note says how many more there are, so a diff
// with thousands of files cannot grow the note without bound.
const capDiffMaxNoteFiles = 30

// diffSection is one file's slice of a unified diff: its `diff --git`
// header line (newline included) and the body up to the next header.
type diffSection struct {
	header string // the `diff --git a/x b/x` line, newline included
	body   string // everything up to the next `diff --git` line
	name   string // the file's path, from the header's b/ side
}

// splitDiffSections splits a unified diff at every `diff --git ` line. A
// diff with no such line yields no sections; text before the first header
// is kept as its own section with no name, so a leading run of context is
// never dropped silently either.
func splitDiffSections(diff string) []diffSection {
	start := 0
	if !strings.HasPrefix(diff, "diff --git ") {
		i := strings.Index(diff, "\ndiff --git ")
		if i < 0 {
			return nil
		}
		start = i + 1
	}
	var secs []diffSection
	for start < len(diff) {
		end := strings.Index(diff[start:], "\ndiff --git ")
		if end < 0 {
			end = len(diff)
		} else {
			end += start
		}
		sec := diff[start:end]
		hl := strings.IndexByte(sec, '\n')
		var s diffSection
		if hl < 0 {
			s.header = sec
		} else {
			s.header, s.body = sec[:hl+1], sec[hl+1:]
		}
		if strings.HasPrefix(s.header, "diff --git ") {
			// The header line reads `diff --git a/old b/new`; the b/ side
			// names the file. Quoted paths (spaces in the name) keep the
			// whole remainder, which still names the file.
			s.name = strings.TrimSuffix(s.header[len("diff --git "):], "\n")
			if i := strings.LastIndexByte(s.name, ' '); i >= 0 {
				bpath := s.name[i+1:]
				if strings.HasPrefix(bpath, "b/") {
					s.name = bpath[len("b/"):]
				}
			}
		}
		secs = append(secs, s)
		start = end + 1
	}
	return secs
}

// allocDiffShares fills share[i] with the body bytes section i may keep
// within budget: every file keeps a floor of content (scaled down when
// even the floors do not fit), a file whose whole body fits is kept whole,
// and the leftovers are redistributed to the files still short, largest
// first.
func allocDiffShares(secs []diffSection, budget int, share []int) {
	for i := range share {
		share[i] = 0
	}
	n := len(secs)
	if n == 0 {
		return
	}
	floor := capDiffFloorBytes
	if floor*n > budget {
		floor = budget / n
	}
	remaining := budget
	for i, s := range secs {
		k := min(len(s.body), floor)
		if k > remaining {
			k = max(remaining, 0)
		}
		share[i] = k
		remaining -= k
	}
	// Redistribution: the files still short split what is left equally,
	// largest first, until the budget runs out.
	short := make([]int, 0, n)
	for i, s := range secs {
		if share[i] < len(s.body) {
			short = append(short, i)
		}
	}
	sort.SliceStable(short, func(a, b int) bool { return len(secs[short[a]].body) > len(secs[short[b]].body) })
	for len(short) > 0 && remaining > 0 {
		per := remaining / len(short)
		if per == 0 {
			// Crumbs only: one byte to each of the largest files, in
			// order, until the budget is gone.
			for _, i := range short {
				if remaining == 0 {
					break
				}
				share[i]++
				remaining--
			}
			break
		}
		next := make([]int, 0, len(short))
		for _, i := range short {
			add := min(len(secs[i].body)-share[i], per)
			share[i] += add
			remaining -= add
			if share[i] < len(secs[i].body) {
				next = append(next, i)
			}
		}
		short = next
	}
}

// diffTruncationNote is what the capped diff ends with: the byte count the
// note always carried, plus the clipped files and the instruction to read
// them on the checked-out PR head branch (the maintainer's reduction of
// issue #231 — the review run has the checkout, so named files can be read
// even though the brief cannot carry them).
func diffTruncationNote(max int, clipped []string) string {
	note := fmt.Sprintf("(diff truncated at %d bytes)\n", max)
	if len(clipped) == 0 {
		return note
	}
	named := clipped
	extra := 0
	if len(named) > capDiffMaxNoteFiles {
		extra = len(named) - capDiffMaxNoteFiles
		named = named[:capDiffMaxNoteFiles]
	}
	note = fmt.Sprintf("(diff truncated at %d bytes; not all of every file is shown — read the full content of %s on the checked-out PR head branch",
		max, strings.Join(named, ", "))
	if extra > 0 {
		note += fmt.Sprintf(" and %d more", extra)
	}
	return note + ")\n"
}

// capDiff caps the diff embedded in the review brief. The cap is file-aware
// (issue #231): the diff is split into per-file sections, every `diff --git`
// header line is always kept, and the byte budget (the note included) is
// allocated across the files so each one contributes some content, the
// leftovers going to the largest files first. Sections whose body is cut
// short are named in the note. A diff too large even for its headers, or
// with no file headers at all, falls back to a plain prefix cap. The result
// never splits a UTF-8 rune and never exceeds max bytes.
func capDiff(diff string, max int) string {
	if len(diff) <= max {
		return diff
	}
	secs := splitDiffSections(diff)
	headerBytes := 0
	for _, s := range secs {
		headerBytes += len(s.header)
	}
	if len(secs) == 0 || headerBytes >= max {
		// No per-file sections to preserve — cap the text itself.
		return capDiffPrefix(diff, max)
	}
	share := make([]int, len(secs))
	contentBudget := max - headerBytes
	for round := 0; round < 16; round++ {
		allocDiffShares(secs, contentBudget, share)
		var b strings.Builder
		var clipped []string
		for i, s := range secs {
			cut := share[i]
			if cut > len(s.body) {
				cut = len(s.body) // a file short of its share is kept whole
			}
			// Back off to a rune boundary, never splitting one; a cut at
			// the end of the body has nothing to back off from.
			for cut > 0 && cut < len(s.body) && !utf8.RuneStart(s.body[cut]) {
				cut-- // never split a rune
			}
			b.WriteString(s.header)
			if cut < len(s.body) {
				b.WriteString(s.body[:cut])
				if s.name != "" {
					clipped = append(clipped, s.name)
				}
			} else {
				b.WriteString(s.body)
			}
		}
		out := b.String()
		note := diffTruncationNote(max, clipped)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out += "\n" // a clipped body may end mid-line
		}
		out += note
		if len(out) <= max {
			return out
		}
		// The note names more clipped files than this round's budget left
		// room for: shrink the content budget and re-allocate.
		contentBudget -= len(out) - max
		if contentBudget < 0 {
			break
		}
	}
	// Degenerate shapes (headers plus note past the cap): a bounded prefix
	// keeps the total within max, as before issue #231.
	return capDiffPrefix(diff, max)
}

// capDiffPrefix is the pre-#231 plain cap: a byte prefix with a note.
func capDiffPrefix(diff string, max int) string {
	note := fmt.Sprintf("\n(diff truncated at %d bytes)\n", max)
	cut := max - len(note)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(diff[cut]) {
		cut--
	}
	return diff[:cut] + note
}

// cappedDiffForBrief caps the fetched diff for the brief at its usual
// maxReviewDiffBytes budget (issue #458). A fetch that stopped at
// diffReadCap ends in diffFetchNote, and capDiff keeps a prefix of the whole
// text with a note of its own — so the fetch note would always land past the
// cap and never reach the reviewer, who would review an incomplete diff
// believing it complete. The note is therefore reserved out of the budget
// and re-appended after the cut.
func cappedDiffForBrief(diff string, truncated bool) string {
	return cappedDiffText(diff, maxReviewDiffBytes, truncated)
}

// cappedDiffText is cappedDiffForBrief with an explicit budget, so a
// re-cap at a smaller budget (the overflow path of runReviewBrief) keeps the
// fetch note too: a plain capDiff re-cap of the raw diff used to drop it,
// and the reviewer would read a re-capped prefix as the whole fetched diff.
func cappedDiffText(diff string, max int, truncated bool) string {
	if !truncated {
		return capDiff(diff, max)
	}
	note := diffFetchNote()
	if max <= len(note) {
		return note // no room for any diff text; the note alone says why
	}
	s := capDiff(diff, max-len(note)-1)
	if s != "" && s[len(s)-1] != '\n' {
		s += "\n" // capDiff's own note may end mid-line after the cut
	}
	return s + note
}

// reviewHarnessPreChangeSentence is the evidence rule review-instructions.md
// carries (issue #189): the `harness` on PATH predates the run, so a review
// of a .github/codeaf/ diff rebuilds from the diff's tree.
const reviewHarnessPreChangeSentence = "the `harness` binary on `PATH` was built from the default\nbranch before this run started"

// capReviewBody caps one untrusted review-brief body on its own (issue #38)
// so it cannot crowd the diff and the machine-parsed output contract out of
// the whole brief. The note names the section and counts against max. Even
// when max leaves no room for any body text — a room of zero, or less than
// the note itself — an omission note names the cut (review follow-up on
// #38): a body used to vanish without a mark, taking the issue's "Done
// when" with it. An empty body still returns empty, so a section that does
// not exist adds no note and no separator.
func capReviewBody(s, name string, max int) string {
	if s == "" {
		return ""
	}
	if len(s) <= max {
		return s
	}
	note := fmt.Sprintf("\n\n(%s truncated at %d bytes)\n", name, max)
	if max >= len(note) {
		cut := max - len(note)
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		return s[:cut] + note
	}
	return fmt.Sprintf("\n\n(%s omitted: the brief is over its size limit)\n", name)
}

// runReviewBrief implements `harness review-brief` (issue #32): the review
// run's brief. It mirrors runBrief's conventions — the budget line, the
// standing instructions from the harness directory, sections joined by a
// blank line, the whole brief capped, printed and written to BRIEF_PATH.
// Env NUMBER is the pull request to review.
func runReviewBrief() error {
	number, err := strconv.Atoi(os.Getenv("NUMBER"))
	if err != nil {
		return fmt.Errorf("NUMBER: %w", err)
	}
	briefPath := os.Getenv("BRIEF_PATH")
	if briefPath == "" {
		return errors.New("BRIEF_PATH is empty")
	}
	gh := newGitHub()
	standing, err := os.ReadFile(filepath.Join(harnessDir(), "review-instructions.md"))
	if err != nil {
		return err
	}
	budget := budgetLine()
	standingText := string(standing)

	var pr item
	if err := gh.get(fmt.Sprintf("/pulls/%d", number), &pr); err != nil {
		return err
	}
	prHeading := fmt.Sprintf("# The pull request under review: #%d — %s\n\nHead branch `%s`, base branch `%s`.\n\n",
		number, pr.Title, pr.Head.Ref, pr.Base.Ref)
	// What the review judges the diff against is the issue the pull request
	// closes (issue #32), read exactly as runBrief reads it: an API failure
	// degrades to a note and must not fail the brief.
	var issueHeading, issueBody, issueRules string
	if n := linkedIssue(pr.Body); n != 0 {
		var closes item
		if err := gh.get(fmt.Sprintf("/issues/%d", n), &closes); err != nil {
			issueHeading = fmt.Sprintf("(the issue this PR closes, #%d, is unavailable: %s)\n", n, escapeMD(err.Error(), 200, true))
		} else {
			issueHeading = fmt.Sprintf("## The issue this PR closes (#%d) — %s\n\n", n, escapeMD(closes.Title, 200, true))
			issueBody = closes.Body
			// Issue #139: the app rules travel with the issue, exactly as
			// runBrief embeds them for the worker.
			issueRules = appRulesSections(gh, issueAreas(closes))
		}
	}
	diff, truncated, err := gh.diff(number)
	if err != nil {
		return err
	}
	diffText := cappedDiffForBrief(diff, truncated)
	diffSection := "# The diff under review\n\n```diff\n" + diffText + "\n```"
	reportSection := previousReportSection(gh, number)

	// Issue #38: cap the two untrusted bodies on their own before joining.
	// The whole-brief cap used to cut through the diff and drop the output
	// contract, with only an anonymous note. The fixed sections are measured
	// first; the room they leave under maxBriefBytes is split between the
	// bodies, each cut carrying a note that names it. A short body leaves its
	// unused room to the other, and an empty optional section is omitted so it
	// adds no separator.
	build := func(prBody, issueText string) string {
		all := []string{
			budget, standingText,
			prHeading + prBody,
			issueHeading + issueText,
			issueRules,
			diffSection,
			reportSection,
			reviewOutputContract,
		}
		parts := make([]string, 0, len(all))
		for _, p := range all {
			if p != "" {
				parts = append(parts, p)
			}
		}
		return strings.Join(parts, "\n\n") + "\n"
	}
	base := build("", "")
	// The fixed sections can overrun the cap on their own — issueAreas can
	// pull several app AGENTS.md sections of up to maxAppRulesBytes each into
	// issueRules, and the standing instructions or the previous report can
	// grow. The diff then gives way first, always with its own truncation
	// note (and the fetch note when the fetch itself was truncated), so the
	// output contract still reaches the reviewer.
	if len(base) > maxBriefBytes {
		// overhead is the section's heading and code fence around diffText.
		overhead := len(diffSection) - len(diffText)
		room := maxBriefBytes - (len(base) - len(diffSection)) - overhead
		if room < capDiffFloorBytes {
			room = capDiffFloorBytes
		}
		// cappedDiffText, not capDiff: a re-cap of the raw diff must keep the
		// fetch note the first cap reserved (review follow-up on #38).
		diffText = cappedDiffText(diff, room, truncated)
		diffSection = "# The diff under review\n\n```diff\n" + diffText + "\n```"
		base = build("", "")
	}
	room := maxBriefBytes - len(base)
	if room < 0 {
		room = 0
	}
	// The PR body's share is half the room plus whatever the issue body does
	// not use, so a short or missing issue body leaves all of its half to the
	// PR body (review follow-up on #38).
	prBody := capReviewBody(pr.Body, "pull request body", max(room/2, room-len(issueBody)))
	issueBody = capReviewBody(issueBody, "closing issue body", room-len(prBody))
	brief := build(prBody, issueBody)
	// A base that nearly fills the cap leaves less room than the two notes
	// the bodies keep even then: the diff gives way for the notes rather than
	// the notes (or the contract behind them) being cut anonymously.
	for len(brief) > maxBriefBytes && len(diffText) > capDiffFloorBytes {
		excess := len(brief) - maxBriefBytes
		shrink := len(diffText) - capDiffFloorBytes
		if excess+128 < shrink {
			shrink = excess + 128 // one pass, with margin for note-size drift
		}
		diffText = cappedDiffText(diff, len(diffText)-shrink, truncated)
		diffSection = "# The diff under review\n\n```diff\n" + diffText + "\n```"
		brief = build(prBody, issueBody)
	}
	// Truncation keeps the head of each body, so the issue's "Done when" —
	// the tail of untrusted text of unknown shape — can be cut whatever
	// share the issue body gets: keeping it would mean parsing the body to
	// find the section, which the brief does not do. The note that names the
	// cut is the guarantee instead, and it survives every path above: the
	// diff pays for it before the whole-brief cap is ever reached.
	if len(brief) > maxBriefBytes {
		// Unreachable while the fixed sections stay bounded; retained so a
		// future unbounded section degrades to the old whole-brief note.
		brief = capBrief(brief, maxBriefBytes)
	}
	fmt.Print(brief)
	if err := os.MkdirAll(filepath.Dir(briefPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(briefPath, []byte(brief), 0o644)
}

// runReviewConfig implements `harness review-config` (issue #32): it prints
// the CodeAF config for the review run — codeaf-config.json with the worker
// tier replaced by the reviewer model chosen by reviewerModel — so the
// review never shares the model that wrote the patch. It fails loudly on an
// unreadable config and when no different model exists.
func runReviewConfig() error {
	cfgPath := filepath.Join(harnessDir(), "codeaf-config.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}
	worker, _ := config["models.tiers.worker"].(string)
	if worker == "" {
		return fmt.Errorf("%s: key models.tiers.worker is missing or not a string", cfgPath)
	}
	models, err := loadConfig(configPath())
	if err != nil {
		return err
	}
	reviewer, err := reviewerModel(models, worker)
	if err != nil {
		return err
	}
	config["models.tiers.worker"] = reviewer.ID
	out, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

// runReviewReport implements `harness review-report PR` (issue #32), in the
// publish job in review mode: it reads the review run's result, parses the
// findings, decides what follows and prints exactly three output lines —
// action=fix|label|report-only, used=<continuations already used>,
// blocking=<blocking findings> — and writes the comment body to
// REVIEW_COMMENT_PATH for the workflow to post.
func runReviewReport() error {
	if len(os.Args) < 3 {
		return errors.New("usage: harness review-report <PR>")
	}
	pr, ok := dispatchIssue(os.Args[2])
	if !ok {
		return fmt.Errorf("review-report: PR number %q is not a positive number of digits", os.Args[2])
	}
	for _, name := range []string{"RESULT_PATH", "LEDGER_PATH", "REVIEW_COMMENT_PATH"} {
		if os.Getenv(name) == "" {
			return fmt.Errorf("%s is empty", name)
		}
	}
	result, haveResult := Result{}, false
	if r, _, err := readResult(); err == nil {
		result, haveResult = r, true
	}
	// The envelope's answer can be empty while the review's text lives in
	// the run record (issue #49), which the run job shipped beside this
	// result in every mode (issue #134); the record also tells whether the
	// run finished in the store, promoting an incomplete stop to done. Both
	// are read as untrusted data.
	c := readClosing(trajectoryPath())
	// The PR's changed paths discriminate a quoted finding from a reported
	// one (issue #224): a location the diff does not touch is text the
	// reviewer quoted, not a place to fix. The diff fetch is best-effort —
	// this command already reports a review without one, so a failed or
	// unreadable fetch parses every finding, as it did before the issue.
	var known map[string]bool
	if diff, truncated, err := newGitHub().diff(pr); err != nil {
		fmt.Fprintf(os.Stderr, "review-report: reading the diff of %d: %v; findings are parsed without it\n", pr, err)
	} else {
		known = knownPaths(diff, truncated, pr)
	}
	findings, sawSection := parseFindingsIn(effectiveAnswer(result, c), known)
	if len(findings) == 0 {
		// The answer carried no findings (issue #49) — it may be empty, or
		// the review may have written them inline where the envelope never
		// looks. Fall back to the root task's final result in the run
		// record, which the run job shipped beside this result and which is
		// read as untrusted data.
		if answer := c.Answer; answer != "" {
			if tf, tsec := parseFindingsIn(answer, known); len(tf) > 0 {
				findings, sawSection = tf, sawSection || tsec
			}
		}
	}
	result.Stop = effectiveStop(result, c)
	action, body, used, err := reviewPlan(newGitHub(), pr, result, haveResult, findings, sawSection)
	if err != nil {
		return err
	}
	fmt.Printf("action=%s\nused=%d\nblocking=%d\n", action, used, len(blockingFindings(findings)))
	path := os.Getenv("REVIEW_COMMENT_PATH")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

// ---------------------------------------------------------------- the run record

// maxTrajectoryBytes caps the root task's run record, both when the run job
// ships it and when the report reads it: the record is written by the
// untrusted run job, so it is data of bounded size, never instructions.
const maxTrajectoryBytes = 2 << 20

// trajectoryFileName is the harness's name for the copy of the root task's
// run record that sits beside the result in the output artifact.
const trajectoryFileName = "codeaf-trajectory.jsonl"

// trajectoryPath names the run record the report falls back to (issue #49):
// TRAJECTORY_PATH when set, else the copy the run job's summary made beside
// the result.
func trajectoryPath() string {
	if p := os.Getenv("TRAJECTORY_PATH"); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(os.Getenv("RESULT_PATH")), trajectoryFileName)
}

// readRecordTail reads at most max bytes from the end of an open file
// (issue #111): the root task's end record is the last line of its run
// record, so a bounded read must start from the tail or a long run's
// ending is cut off. Files at or under max are read whole; a longer file
// gives its last max bytes, even if the first of them lands mid-line —
// the half line is malformed to the reader and skipped.
func readRecordTail(file *os.File, max int64) ([]byte, error) {
	size, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if size <= max {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return io.ReadAll(io.LimitReader(file, max))
	}
	if _, err := file.Seek(-max, io.SeekEnd); err != nil {
		return nil, err
	}
	buf := make([]byte, max)
	n, err := io.ReadFull(file, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return buf[:n], nil
}

// shipRunRecord copies the newest root task run record in CODEAF_HOME next to
// the result (issue #49): the review report runs in the publish job, which
// reads only this job's output artifact, so the record must ride it for the
// fallback to reach the report. It runs in the run job, on files the agent
// could have written: only regular files are taken, the copy holds the
// record's tail, capped like the report's read (issue #111) so the end
// record survives the copy of a long run, and the report reads the copy as
// data. Nothing here is fatal — the record is a fallback — so trouble is
// logged and answered with "".
func shipRunRecord() string {
	home, out := os.Getenv("CODEAF_HOME"), filepath.Dir(os.Getenv("RESULT_PATH"))
	if home == "" || out == "" {
		return ""
	}
	// filepath.Glob below stays silent on an unreadable directory: it
	// answers no matches for a permission error, so a runs dir the harness
	// cannot read — the agent user still owned CODEAF_HOME before the
	// workflow took the tree back (issue #62) — would ship no record and
	// say nothing. Check the directory directly so that failure is loud.
	runs := filepath.Join(home, "runs")
	if _, err := os.ReadDir(runs); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("warning: %s is unreadable, the run record cannot ship: %v", runs, err)
	}
	records, err := filepath.Glob(filepath.Join(runs, "*", "tasks", "root", "trajectory.jsonl"))
	if err != nil || len(records) == 0 {
		return ""
	}
	sort.Slice(records, func(a, b int) bool { // newest first: the run just finished
		sa, era := os.Stat(records[a])
		sb, erb := os.Stat(records[b])
		if era != nil || erb != nil {
			return era == nil
		}
		return sa.ModTime().After(sb.ModTime())
	})
	for _, record := range records {
		if s, err := os.Lstat(record); err != nil || !s.Mode().IsRegular() {
			continue // a planted symlink or directory is not a record
		}
		file, err := os.Open(record)
		if err != nil {
			log.Printf("warning: %s is unreadable: %v", record, err)
			continue
		}
		data, err := readRecordTail(file, maxTrajectoryBytes)
		file.Close()
		if err != nil {
			log.Printf("warning: %s is unreadable: %v", record, err)
			continue
		}
		if err := os.WriteFile(filepath.Join(out, trajectoryFileName), data, 0o644); err != nil {
			log.Printf("warning: the run record could not be shipped: %v", err)
			return ""
		}
		return record
	}
	return ""
}
