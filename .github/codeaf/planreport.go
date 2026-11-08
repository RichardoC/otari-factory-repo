// planreport.go is the publish side of the planner pass (issue #61): it runs
// in the publish job, reads the plan run's answer exactly as runReviewReport
// reads a review's (RESULT_PATH, falling back to the run record), parses the
// proposals with a machine parser, dedupes them against the open issues'
// titles, and writes the ones that survive as JSON for the workflow to open
// as issues. Everything parsed here comes from the untrusted run job, so it
// is read as data of bounded size: escaped, capped, and never executed.
// Standard library only.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// proposedLabel marks an issue the planner opened for a person to triage.
const proposedLabel = "codeaf-proposed"

// Parse caps. A hostile answer can carry 100k-character fields and a hundred
// proposals; every bound here is what keeps one answer from growing the
// output without limit.
const (
	maxProposalTitle   = 200  // runes, one line
	maxProposalField   = 3000 // runes per field
	maxProposalsParsed = 20   // hard ceiling; the run's own cap is min(max, this)
	maxProposalDeps    = 10   // "#N" tokens per proposal
	// maxAreaLabelLen is GitHub's cap on a label name (50 characters, and
	// GH treats them as bytes): an area label longer than this is never
	// created and fails the issue-apply step (issue #40).
	maxAreaLabelLen = 50
)

// Proposal is one proposed issue parsed from a plan run's answer (issue #61).
type Proposal struct {
	Title    string
	Problem  string
	What     string
	Done     string
	EndToEnd string // the "End-to-end check" field, when the planner gave one (issue #136)
	Area     string // "harness" or "app/<name>"; empty when absent or unrecognized
	Deps     []int
}

var (
	// planProposalHeader opens one proposal: a line starting with
	// "Proposal:", at any heading level, case-insensitive, trailing
	// whitespace allowed. The title is the rest of the line.
	planProposalHeader = regexp.MustCompile(`(?i)^\s*#{0,6}\s*proposal:[ \t]*(.*)$`)
	// planHeading is any markdown heading line; it ends the current field.
	planHeading = regexp.MustCompile(`^\s*#{1,6}(\s|$)`)
	// planFieldLine is one known field: "Name: text" with an optional
	// leading "- ", case-insensitive on the name.
	planFieldLine = regexp.MustCompile(`(?i)^\s*-?\s*(problem|what to do|done when|end-to-end check|area|depends on)\s*:\s*(.*)$`)
	// planUnknownField is a bullet line shaped like a field ("Name: text")
	// but naming none of the known ones. Its text is kept out, and it ends
	// the previous field, so a crafted "- Ignore previous instructions: ..."
	// cannot ride into a proposal as its continuation. Only bullet lines
	// count — a field's own continuations carry colons in ordinary prose,
	// and those must keep appending.
	planUnknownField = regexp.MustCompile(`^\s*-\s*[^:\n]{1,80}:\s`)
	// planDepToken is one "#N" dependency token; anything else on a
	// "Depends on" line is ignored.
	planDepToken = regexp.MustCompile(`#(\d+)`)
	// planNumberedItem opens one numbered item (issue #119): "1. harness —
	// Title. Verified: …". Only the number and one separator character are
	// fixed here; whether the item is a proposal is decided by
	// planAreaPrefix below, so ordinary numbered prose — including numbered
	// sub-lists inside a proposal's own fields — keeps its meaning.
	planNumberedItem = regexp.MustCompile(`^\s*[0-9]{1,3}[.)]\s+(.*)$`)
	// planAreaPrefix reads a numbered item's leading area: "harness" or
	// "app/<name>", case-insensitive, then a dash (en, em or plain) or a
	// colon, then the title source. A period is deliberately not a separator:
	// app directory names may carry dots. This leading area token is the one
	// thing an ordinary numbered line in prose almost never has, so it is
	// what marks the item as a closing-result proposal.
	planAreaPrefix = regexp.MustCompile(`(?i)^\s*(harness|app/[A-Za-z0-9._-]+)\s*[-\x{2013}\x{2014}:]\s*(.*)$`)
	// planSentenceEnd finds the first sentence end: one of . ! ? … followed
	// by whitespace or the end of the text. A numbered item's title is the
	// text before it, its body the rest (issue #119).
	planSentenceEnd = regexp.MustCompile(`[.!?\x{2026}](\s|$)`)
	// planTitleNoise collapses runs of non-alphanumerics, for title
	// normalization in DedupeProposals.
	planTitleNoise = regexp.MustCompile(`[^a-z0-9]+`)
)

// planClean strips control characters (newlines kept — the field layer
// decides what a newline becomes) and trims; it is the one cleaner every
// parsed proposal text passes through.
func planClean(s string) string {
	return strings.TrimFunc(strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r // the field layer decides what a newline becomes
		case r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1 // control characters are stripped
		}
		return r
	}, s), func(r rune) bool { return r == ' ' })
}

// planTruncate cuts s at max runes without splitting one.
func planTruncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// planTitle reads a proposal title: one line (newlines become spaces),
// control characters stripped, capped.
func planTitle(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(planTruncate(planClean(s), maxProposalTitle))
}

// normalizeArea maps an Area field to "harness" or "app/<name>"; anything
// else — a made-up area, a path, prose — answers "". The area becomes a
// label, so only the two shapes the factory understands may pass.
func normalizeArea(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case s == "harness":
		return "harness"
	case strings.HasPrefix(s, "app/"):
		name := strings.TrimPrefix(s, "app/")
		if name != "" && !strings.ContainsAny(name, " \t\n/") {
			return "app/" + name
		}
	}
	return ""
}

// ParseProposals reads the proposals out of a plan run's answer (issue #61).
// Line by line, sharing one cap, min(max, maxProposalsParsed):
//
//   - a line matching planProposalHeader closes any proposal being read and
//     starts a new one with the rest of the line as its title;
//   - a known field line ("- Problem: ...", case-insensitive) appends its
//     text to that field of the current proposal — a repeated field keeps
//     both texts, so nothing a planner wrote is silently dropped;
//   - an unknown field line is kept out and ends the previous field;
//   - any other non-empty line appends to the previous field, until the
//     next field, proposal or heading; lines before the first proposal are
//     prose and ignored;
//   - at close, a proposal whose Problem and What are both empty is
//     dropped, the title is made single-line, every text is truncated to
//     its cap, Area is normalized, and Depends on keeps at most
//     maxProposalDeps "#N" tokens.
func ParseProposals(answer string, max int) []Proposal {
	if max > maxProposalsParsed {
		max = maxProposalsParsed
	}
	var out []Proposal
	var cur *Proposal
	field := ""  // the open field's name, or ""
	numSrc := "" // a numbered item's pending title source, "" for none
	// closeCur closes the proposal being read. A numbered item's title is
	// split from its pending source first (issue #119): the text up to the
	// first sentence end is the title, the rest rides ahead of the body
	// the continuation lines gathered. Then the keep rule and the caps
	// apply exactly as to a proposal in the section format.
	closeCur := func() {
		if cur == nil {
			return
		}
		if numSrc != "" {
			title, rest := planSplitSentence(numSrc)
			cur.Title = planTitle(title)
			if rest != "" {
				cur.Problem = strings.TrimSpace(rest + "\n" + cur.Problem)
			}
			numSrc = ""
		}
		if planKeeps(*cur) && len(out) < max {
			out = append(out, planFinish(*cur))
		}
		cur = nil
	}
	appendTo := func(name, line string) {
		switch name {
		case "problem":
			cur.Problem = strings.TrimSpace(cur.Problem + "\n" + line)
		case "what to do":
			cur.What = strings.TrimSpace(cur.What + "\n" + line)
		case "done when":
			cur.Done = strings.TrimSpace(cur.Done + "\n" + line)
		case "end-to-end check":
			cur.EndToEnd = strings.TrimSpace(cur.EndToEnd + "\n" + line)
		case "area":
			cur.Area = strings.TrimSpace(cur.Area + "\n" + line)
		case "depends on":
			cur.Deps = appendPlanDeps(cur.Deps, line)
		}
	}
	lines := strings.Split(answer, "\n")
	// Issue #339: the fences are computed once over the whole answer, before
	// any line is parsed, with the same span rule the review pass uses
	// (issue #224). A closed fenced block — the brief's own output contract
	// echoed back, a quoted sample, a patch — is quoted text, not answer
	// prose, so nothing inside one may open a proposal or a field. Its lines
	// still belong to the field that is open around it: a proposal's End-
	// to-end check or What to do may carry its command or sample in a fence,
	// and dropping it would leave the issue short of the section the queue's
	// gate reads. An unterminated fence is not a confirmed quote (the review
	// pass's rule too): it runs to the end of the answer and would otherwise
	// swallow every proposal after it, so its lines parse as usual.
	spans := parseFences(lines)
	for i, raw := range lines {
		if len(out) >= max {
			break // cap reached: the rest of the answer is ignored
		}
		line := strings.TrimSuffix(raw, "\r")
		if _, quoted := fenceStateAt(spans, i); quoted {
			// A quoted line opens nothing; it is text of the open field.
			if cur != nil && field != "" && strings.TrimSpace(line) != "" {
				appendTo(field, planClean(line))
			}
			continue
		}
		if m := planProposalHeader.FindStringSubmatch(line); m != nil {
			closeCur()
			if len(out) >= max {
				break
			}
			cur = &Proposal{Title: planTitle(m[1])}
			field = ""
			continue
		}
		// Issue #119: a numbered item shaped "1. harness — Title. Body" is
		// a proposal in the closing-result form. The leading area token is
		// what tells it apart from ordinary numbered prose; its body — the
		// rest of the line and every continuation line up to the next item
		// or heading — lands in Problem, where planKeeps can see it.
		if m := planNumberedItem.FindStringSubmatch(line); m != nil {
			if a := planAreaPrefix.FindStringSubmatch(m[1]); a != nil {
				closeCur()
				if len(out) >= max {
					break
				}
				cur = &Proposal{Area: a[1]}
				numSrc = a[2]
				field = "problem"
				continue
			}
		}
		if cur == nil {
			continue // prose before the first proposal header
		}
		if planHeading.MatchString(strings.TrimSpace(line)) {
			field = ""
			continue
		}
		if m := planFieldLine.FindStringSubmatch(line); m != nil {
			field = strings.ToLower(m[1])
			appendTo(field, planClean(m[2]))
			continue
		}
		if planUnknownField.MatchString(strings.TrimSpace(line)) {
			field = "" // unknown field: kept out, and it ends the previous one
			continue
		}
		if field == "" || strings.TrimSpace(line) == "" {
			continue
		}
		appendTo(field, planClean(line))
	}
	closeCur()
	return out
}

// planSplitSentence splits a numbered item's title source at its first
// sentence end (issue #119): the title is the text before it, the body the
// rest. With no sentence end the whole source is the title and the body is
// whatever the item's continuation lines gathered.
func planSplitSentence(s string) (title, rest string) {
	if loc := planSentenceEnd.FindStringSubmatchIndex(s); loc != nil {
		return strings.TrimSpace(s[:loc[0]]), strings.TrimSpace(s[loc[0]+1:])
	}
	return strings.TrimSpace(s), ""
}

// planKeeps reports whether a proposal survives parse: a proposal that says
// neither what is wrong nor what to do is noise, not a proposal.
func planKeeps(p Proposal) bool {
	return strings.TrimSpace(p.Problem) != "" || strings.TrimSpace(p.What) != ""
}

// planFinish truncates the texts, makes the title single-line, normalizes
// the area and caps the dependencies — the shape a proposal takes from here
// on, so later stages never re-trust the raw answer.
func planFinish(p Proposal) Proposal {
	p.Title = planTitle(p.Title)
	p.Problem = planTruncate(strings.TrimSpace(p.Problem), maxProposalField)
	p.What = planTruncate(strings.TrimSpace(p.What), maxProposalField)
	p.Done = planTruncate(strings.TrimSpace(p.Done), maxProposalField)
	p.EndToEnd = planTruncate(strings.TrimSpace(p.EndToEnd), maxProposalField)
	p.Area = normalizeArea(p.Area)
	if len(p.Deps) > maxProposalDeps {
		p.Deps = p.Deps[:maxProposalDeps]
	}
	return p
}

// appendPlanDeps reads the "#N" tokens out of one "Depends on" line (or one
// of its continuations) and appends them, capped at maxProposalDeps in
// total; digits only, so "#12x" stops at 12 and prose is ignored.
func appendPlanDeps(deps []int, line string) []int {
	for _, m := range planDepToken.FindAllStringSubmatch(line, -1) {
		if len(deps) >= maxProposalDeps {
			break
		}
		if n, err := strconv.Atoi(m[1]); err == nil {
			deps = append(deps, n)
		}
	}
	return deps
}

// normalizeTitle folds a title for comparison: lowercase, every run of
// non-alphanumerics collapsed to one space, trimmed. "Fix: the queue!" and
// "fix the queue" then name the same work.
func normalizeTitle(s string) string {
	return strings.TrimSpace(planTitleNoise.ReplaceAllString(strings.ToLower(s), " "))
}

// DedupeProposals drops a proposal whose normalized title equals a
// normalized open-issue title, or equals an earlier kept proposal's, and
// answers how many it held back. It never edits proposal text: the
// dedupe's only power is to leave a duplicate unopened.
func DedupeProposals(props []Proposal, open []item) (kept []Proposal, held int) {
	seen := make(map[string]bool, len(open)+len(props))
	for _, it := range open {
		seen[normalizeTitle(it.Title)] = true
	}
	for _, p := range props {
		key := normalizeTitle(p.Title)
		if seen[key] {
			held++
			continue
		}
		seen[key] = true
		kept = append(kept, p)
	}
	return kept, held
}

// ProposalLabels is the label set for an opened proposal: codeaf-proposed
// always, plus the area label when the proposal carries a known area. It is
// NEVER codeaf-queued — that label is the trust gate: only a person with
// triage access, or Claude acting for the owner, promotes a proposal to the
// queue, and this code, fed by the untrusted run job, must never grant work
// to itself. Proposal text is untrusted data, so no label is ever read out
// of it.
func ProposalLabels(p Proposal) []string {
	labels := []string{proposedLabel}
	switch {
	case p.Area == "harness":
		labels = append(labels, "area:harness")
	case strings.HasPrefix(p.Area, "app/"):
		// The label name is capped like any other GitHub label (issue #40):
		// truncate the app name so the full label fits, rather than dropping
		// the label or letting an over-long one break opening the issue.
		// The body keeps rendering the untruncated Area.
		name := planTruncate(strings.TrimPrefix(p.Area, "app/"), maxAreaLabelLen-len("area:app/"))
		labels = append(labels, "area:app/"+name)
	}
	return labels
}

// proposalE2EPlaceholder is the text a proposal without an "End-to-end
// check" field gets under the heading (issue #136): a fixed string, so the
// section is there and a person can see it must be filled in before the
// issue is queued.
const proposalE2EPlaceholder = "(fill in before queueing: the one command or run that proves the seams work)"

// approvalNote is the exact sentence ProposalBody appends when a gated path is
// named and the planner did not itself state the approval requirement.
const approvalNote = "Note: changes under .github/ outside .github/codeaf/ (workflows, CODEOWNERS) need a maintainer's approval before merging."

// ProposalBody renders one proposal as the body of the issue the publish job
// opens: a fixed markdown template, every untrusted text passed through
// escapeMD (the title on one line), the dependencies rendered as plain
// numbers. When the Problem or What names a path under .github/ outside
// .github/codeaf/ and the proposal does not itself state the approval
// requirement, the note that such changes need a maintainer's approval is
// appended — the
// planner was told to state it, and the factory does not rely on it having
// listened. The path rule is underGithub (patchsplit.go), the same one the
// patch gate and the publish job's package step apply, so .github/CODEOWNERS
// gets the note too.
func ProposalBody(p Proposal) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Proposal: %s\n\n", escapeMD(p.Title, maxProposalTitle, true))
	fmt.Fprintf(&b, "**Problem**\n\n%s\n\n", escapeMD(p.Problem, maxProposalField, false))
	fmt.Fprintf(&b, "**What to do**\n\n%s\n\n", escapeMD(p.What, maxProposalField, false))
	// The acceptance sections (issue #136): "Done when" renders the
	// proposal's field as bullets, "End-to-end check" the planner's field
	// when it gave one and the placeholder otherwise.
	fmt.Fprintf(&b, "## Done when\n\n")
	for _, line := range strings.Split(p.Done, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			fmt.Fprintf(&b, "- %s\n", escapeMD(line, maxProposalField, false))
		}
	}
	e2e := p.EndToEnd
	if strings.TrimSpace(e2e) == "" {
		e2e = proposalE2EPlaceholder
	}
	fmt.Fprintf(&b, "\n## End-to-end check\n\n%s\n", escapeMD(e2e, maxProposalField, false))
	if p.Area != "" {
		fmt.Fprintf(&b, "\n**Area**: %s\n", escapeMD(p.Area, 100, true))
	}
	if len(p.Deps) > 0 {
		nums := make([]string, 0, len(p.Deps))
		for _, n := range p.Deps {
			nums = append(nums, fmt.Sprintf("#%d", n))
		}
		fmt.Fprintf(&b, "\n**Depends on**: %s\n", strings.Join(nums, ", "))
	}
	text := strings.ToLower(p.Problem + "\n" + p.What)
	if namesApprovalPath(text) && !statesApprovalRequirement(text) {
		b.WriteString("\n" + approvalNote + "\n")
	}
	return b.String()
}

// statesApprovalRequirement reports whether text states the maintainer-approval
// requirement itself, rather than merely using the word "approval" (issue #4):
// a bare substring let an unrelated phrase like "the approval job" defeat the
// note. text is already lower-cased. The phrases are the ways the planner is
// told to phrase the requirement. A proposal that merely names the
// codeaf-harness environment does not state it (issue #4, owner decision):
// it must still carry the note.
func statesApprovalRequirement(text string) bool {
	for _, phrase := range []string{
		"maintainer's approval",
		"maintainer’s approval", // curly apostrophe variant
		"approval before merging",
	} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// namesApprovalPath reports whether text names a path that underGithub guards:
// under .github/ but not .github/codeaf/. text is already lower-cased. A
// mention starts at ".github/" and runs over path characters; trailing
// punctuation or a closing slash is not part of the path. Keeping the
// predicate on underGithub rather than on the ".github/workflows" spelling
// means the note follows the gate for every path it protects.
func namesApprovalPath(text string) bool {
	const marker = ".github/"
	for rest := text; ; {
		i := strings.Index(rest, marker)
		if i < 0 {
			return false
		}
		path := rest[i:]
		if j := strings.IndexFunc(path, func(r rune) bool { return !approvalPathChar(r) }); j >= 0 {
			path = path[:j]
		}
		path = strings.TrimRight(path, "/.")
		if underGithub(path) {
			return true
		}
		rest = rest[i+len(marker):]
	}
}

// approvalPathChar reports whether r can appear in a lower-cased path mention.
func approvalPathChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
		r == '/' || r == '.' || r == '-' || r == '_'
}

// planProposalOut is one entry of the JSON the publish workflow reads.
type planProposalOut struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels"`
}

// planOpenItems reads the open issues from OPEN_ISSUES_PATH — the JSON that
// `gh api --paginate repos/$REPO/issues?state=open&per_page=100` prints,
// produced by the workflow and read here as untrusted data. Past one page of
// open issues and pull requests the file holds several top-level arrays back
// to back, which decodeItems decodes as one stream (issue #426). A missing,
// unreadable or malformed file degrades to no open issues: the dedupe then
// keeps everything, which is the safe direction for proposals (a person
// still triages each one).
func planOpenItems() []item {
	path := os.Getenv("OPEN_ISSUES_PATH")
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	items, err := decodeItems(data)
	if err != nil {
		return nil
	}
	return items
}

// runPlanReport implements `harness plan-report` (issue #61), in the publish
// job: it reads the plan run's answer (RESULT_PATH, falling back to the root
// task's result in the run record, exactly as runReviewReport does), parses
// the proposals, dedupes them against the open issues' titles, writes the
// kept ones to PROPOSALS_PATH as a JSON array of {title, body, labels} and
// prints one line, "proposals=N held=M" — N proposals to open, M held back
// as duplicates.
func runPlanReport() error {
	for _, name := range []string{"RESULT_PATH", "PROPOSALS_PATH"} {
		if os.Getenv(name) == "" {
			return fmt.Errorf("%s is empty", name)
		}
	}
	result, _, err := readResult()
	// The result envelope's answer may be empty while the plan's text lives
	// in the run record (issue #49's shape), which the run job ships beside
	// the result in every mode (issue #134); read both as data.
	c := readClosing(trajectoryPath())
	answer := effectiveAnswer(result, c)
	props := ParseProposals(answer, planMaxProposals())
	kept, held := DedupeProposals(props, planOpenItems())
	out := make([]planProposalOut, 0, len(kept))
	for _, p := range kept {
		out = append(out, planProposalOut{Title: p.Title, Body: ProposalBody(p), Labels: ProposalLabels(p)})
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	path := os.Getenv("PROPOSALS_PATH")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("proposals=%d held=%d\n", len(kept), held)
	return nil
}
