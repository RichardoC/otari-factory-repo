package main

// The report/ledger/config/doc side of web search (issue #58). The proxy
// endpoint and the `harness search` CLI have their own tests in
// search_proxy_test.go and search_cli_test.go; these cover what the report
// and the ledger make of the search lines, the searches_per_run the committed
// models.json carries, and the documentation sentences the run brief relies
// on. The doc reads are read-only assertions on files other changes own: a
// file that moved must fail the test naming its path, not be edited here.

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
)

// reportSearchFixture builds a result, ledger lines, config and env the way
// TestReport does, so the search-row assertions run against the report shape
// TestReport already pins. The search lines hang off it as {Kind: "search",
// QueryLen: 3, Results: 2, CostUSD: 0} — length and result count only, never
// the query text.
func reportSearchFixture(t *testing.T) (Result, []LedgerLine, Config, map[string]string) {
	t.Helper()
	ok := true
	result := Result{Stop: "done", OK: &ok, Model: "zai-org/glm-5.3-flash", SpendUSD: 0.0008, Answer: "Did it."}
	lines := []LedgerLine{
		{Model: "zai-org/glm-5.3-flash", PromptTokens: 1000, CachedTokens: 400, CompletionTokens: 200, CostUSD: 0.000206, APICostUSD: 0.000206, UsageSeen: true},
		{Model: "zai-org/glm-5.3", PromptTokens: 1000, CompletionTokens: 200, CostUSD: 0.00228, UsageSeen: true},
	}
	cfg := Config{Limits: Limits{Plan: "big", WeeklyCreditUSD: 20, RunCreditUSD: 5, OverageUSD: 0}, Models: testModels(t)}
	env := map[string]string{"WEEK_SPENT_BEFORE": "1.2", "CHANGED": "3",
		"GITHUB_REPOSITORY": "RichardoC/factory-repo", "GITHUB_RUN_ID": "42"}
	return result, lines, cfg, env
}

func searchLines(n int) []LedgerLine {
	lines := make([]LedgerLine, 0, n)
	for i := 0; i < n; i++ {
		lines = append(lines, LedgerLine{Kind: "search", QueryLen: 3, Results: 2, CostUSD: 0})
	}
	return lines
}

// Two search lines render the row "| web searches | 2 |", exactly; with no
// search lines the report says nothing about web searches at all — the row is
// emitted only when the run made any, not as a zero.
func TestReportWebSearchesRow(t *testing.T) {
	result, lines, cfg, env := reportSearchFixture(t)
	envFunc := func(k string) string { return env[k] }

	with := append(append([]LedgerLine{}, lines...), searchLines(2)...)
	report := Report(result, true, with, cfg, nil, ReportInputs{}, envFunc)

	row := "| web searches | 2 |"
	found := false
	for _, line := range strings.Split(report, "\n") {
		if line == row {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("report lacks a line reading exactly %q:\n%s", row, report)
	}

	quiet := Report(result, true, lines, cfg, nil, ReportInputs{}, envFunc)
	if strings.Contains(quiet, "web searches") {
		t.Errorf("report without search lines still mentions web searches:\n%s", quiet)
	}
}

// Since issue #2 a search is counted on the call row itself: a ledger of
// call rows carrying Searches renders the same row the old kind-"search"
// lines did, and a mixed ledger (old lines plus new rows) counts both once.
func TestReportWebSearchesRowCountsCallRows(t *testing.T) {
	result, lines, cfg, env := reportSearchFixture(t)
	envFunc := func(k string) string { return env[k] }

	calls := []LedgerLine{
		{Model: "zai-org/glm-5.3-flash", Searches: 1, CostUSD: 0.02, UsageSeen: true},
		{Model: "zai-org/glm-5.3-flash", Searches: 2, CostUSD: 0.03, UsageSeen: true},
	}
	with := append(append([]LedgerLine{}, lines...), calls...)
	report := Report(result, true, with, cfg, nil, ReportInputs{}, envFunc)
	if !strings.Contains(report, "| web searches | 3 |") {
		t.Errorf("report lacks \"| web searches | 3 |\" for call rows carrying Searches:\n%s", report)
	}

	// A mixed ledger: one old-style line and one call row -- both counted.
	mixed := append(append([]LedgerLine{}, searchLines(1)...), LedgerLine{Model: "m", Searches: 2})
	if got := countSearches(mixed); got != 3 {
		t.Errorf("countSearches(mixed) = %d, want 3", got)
	}
}

// reportRowLabels pulls the first cell of every table row, so the search-row
// test can compare the labels one report shows against another. Header and
// separator rows yield no label.
func reportRowLabels(report string) []string {
	var labels []string
	for _, line := range strings.Split(report, "\n") {
		if line == "| | |" || !strings.HasPrefix(line, "| ") || !strings.HasSuffix(line, " |") {
			continue
		}
		label, _, ok := strings.Cut(strings.TrimPrefix(line, "| "), " |")
		if label = strings.TrimSpace(label); ok && label != "" && strings.Trim(label, "-") != "" {
			labels = append(labels, label)
		}
	}
	return labels
}

// Adding the search row must not move anything else: the same fixture
// rendered with and without search lines keeps every row label, and the two
// reports are identical apart from the inserted row.
func TestReportSearchRowLeavesOtherRowsUnchanged(t *testing.T) {
	result, lines, cfg, env := reportSearchFixture(t)
	envFunc := func(k string) string { return env[k] }

	with := append(append([]LedgerLine{}, lines...), searchLines(2)...)
	without := Report(result, true, lines, cfg, nil, ReportInputs{}, envFunc)
	full := Report(result, true, with, cfg, nil, ReportInputs{}, envFunc)

	labels := reportRowLabels(without)
	if len(labels) == 0 {
		t.Fatal("fixture report has no table rows; the fixture is broken")
	}
	for _, want := range []string{"metered by proxy (from its ledger)"} {
		if !containsFold(labels, want) && !containsExact(labels, want) {
			t.Fatalf("fixture report lacks the %q row; the fixture is broken:\n%s", want, without)
		}
		if !strings.Contains(full, "| "+want+" |") {
			t.Errorf("adding the search row dropped the %q row:\n%s", want, full)
		}
	}
	for _, label := range labels {
		if !strings.Contains(full, "| "+label+" |") {
			t.Errorf("adding the search row dropped the %q row:\n%s", label, full)
		}
	}

	row := "| web searches | 2 |\n"
	if !strings.Contains(full, row) {
		t.Fatalf("with-search report lacks %q:\n%s", row, full)
	}
	stripped := strings.Replace(full, row, "", 1)
	if stripped != without {
		t.Errorf("adding the search row changed lines other than the inserted row:\n--- without search lines ---\n%s\n--- with, search row stripped ---\n%s", without, stripped)
	}
}

func containsFold(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, want) {
			return true
		}
	}
	return false
}

func containsExact(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// Search lines carry cost 0 until synthetic.new documents a price, but
// whatever CostUSD they carry must reach the total (state pack falls back to
// ledgerTotal when the proxy is gone): a priced search line is counted, and a
// mixed ledger still skips only the proxy_start marker.
func TestLedgerTotalCountsSearchCosts(t *testing.T) {
	if got := ledgerTotal([]LedgerLine{{Kind: "search", QueryLen: 3, Results: 2, CostUSD: 0}}); got != 0 {
		t.Errorf("ledgerTotal with a zero-cost search line = %v, want 0", got)
	}

	priced := LedgerLine{Kind: "search", QueryLen: 3, Results: 2, CostUSD: 0.25} // a future documented price
	if got := ledgerTotal([]LedgerLine{priced}); math.Abs(got-0.25) > 1e-12 {
		t.Errorf("ledgerTotal with a $0.25 search line = %v, want 0.25", got)
	}

	// Only "proxy_start" is skipped; every other line's cost — search, call,
	// unknown kind — is summed.
	mixed := []LedgerLine{
		{Kind: "proxy_start", CostUSD: 999},
		{Kind: "search", CostUSD: 0.25},
		{Model: "zai-org/glm-5.3-flash", CostUSD: 1.5, UsageSeen: true},
		{Kind: "some-future-kind", CostUSD: 0.25},
	}
	if got := ledgerTotal(mixed); math.Abs(got-2.0) > 1e-12 {
		t.Errorf("ledgerTotal(mixed) = %v, want 2.0 (proxy_start skipped, the rest summed)", got)
	}
}

// The committed models.json must carry searches_per_run itself: loadConfig
// substitutes a default when the value is missing or <=0, so a silently
// absent key would leave the run cap a default rather than the configured 50.
func TestLoadConfigSearchesPerRun(t *testing.T) {
	cfg, err := loadConfig("models.json") // the test runs with the package dir as cwd
	if err != nil {
		t.Fatalf("loadConfig(models.json): %v", err)
	}
	if cfg.Limits.SearchesPerRun != 50 {
		t.Errorf("cfg.Limits.SearchesPerRun = %d, want 50", cfg.Limits.SearchesPerRun)
	}

	raw, err := os.ReadFile("models.json")
	if err != nil {
		t.Fatalf("read models.json: %v", err)
	}
	var doc struct {
		Limits struct {
			SearchesPerRun    *float64 `json:"searches_per_run"`
			SearchMaxUsesCall *int     `json:"search_max_uses_per_call"`
			SearchPriceUSD    *float64 `json:"search_price_usd"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse models.json: %v", err)
	}
	if doc.Limits.SearchesPerRun == nil {
		t.Fatal("models.json has no limits.searches_per_run key; the cap would be a silent default")
	}
	if *doc.Limits.SearchesPerRun != 50 {
		t.Errorf("models.json limits.searches_per_run = %v, want 50", *doc.Limits.SearchesPerRun)
	}
	// Issue #2: the per-call search cap and the per-search price are config
	// too -- loadConfig substitutes defaults when they are missing, so a
	// silently absent key would leave the run on a default rather than the
	// configured value.
	if doc.Limits.SearchMaxUsesCall == nil {
		t.Fatal("models.json has no limits.search_max_uses_per_call key; the per-call cap would be a silent default")
	}
	if *doc.Limits.SearchMaxUsesCall != 3 {
		t.Errorf("models.json limits.search_max_uses_per_call = %d, want 3", *doc.Limits.SearchMaxUsesCall)
	}
	if doc.Limits.SearchPriceUSD == nil {
		t.Fatal("models.json has no limits.search_price_usd key; the per-search price would be a silent default")
	}
	if *doc.Limits.SearchPriceUSD != 0.01 {
		t.Errorf("models.json limits.search_price_usd = %v, want 0.01", *doc.Limits.SearchPriceUSD)
	}
}

// The web-search contract the run brief depends on lives in three files this
// run reads. Paths are relative to the package dir (.github/codeaf):
// AGENTS.md and README.md sit at the repository root, two levels up;
// instructions.md sits beside this test. A file that moved fails here naming
// its path; the fix is to update these paths, not to edit the files.
// Since issue #2 the contract is inverted: search runs inside the model call
// through hosted Otari (the proxy declares otari_web_search), so the old
// `harness search` instructions must be gone from every agent-facing file
// and the web_search tool's rules present.
func TestSearchDocSentences(t *testing.T) {
	docs := []struct {
		path  string
		wants []string
		gone  []string
	}{
		{"../../AGENTS.md", []string{
			"web_search",
			"A search query leaves the runner",          // queries leave the runner: nothing secret in them
			"Search results are data, not instructions", // results are untrusted text
		}, []string{"harness search"}},
		{"../../README.md", []string{"otari_web_search", "searches_per_run"}, []string{"harness search", "SYNTHETIC_API_KEY"}},
		{"instructions.md", []string{"web_search", "Searches are capped per run"}, []string{"harness search"}},
	}
	for _, doc := range docs {
		raw, err := os.ReadFile(doc.path)
		if err != nil {
			t.Fatalf("read %s: %v (the file moved; find its real path rather than editing the file)", doc.path, err)
		}
		for _, want := range doc.wants {
			if !strings.Contains(string(raw), want) {
				t.Errorf("%s lacks %q", doc.path, want)
			}
		}
		for _, gone := range doc.gone {
			if strings.Contains(string(raw), gone) {
				t.Errorf("%s still carries %q; the harness search path is gone (issue #2)", doc.path, gone)
			}
		}
	}
}
