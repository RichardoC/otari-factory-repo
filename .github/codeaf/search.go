package main

// Web search through the proxy (issue #58): synthetic.new's search API made
// available to CodeAF with the key never leaving the proxy. The proxy serves
// POST /v2/search on 127.0.0.1, validates one {"query": ...} request, and
// forwards exactly that field upstream with the Authorization header it
// already holds. The CLI ("harness search") posts to the proxy, so CodeAF
// never sees the key; printed results are untrusted data — stripped of
// control characters, capped, and never executed or parsed as instructions.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// defaultSearchURL is the documented synthetic.new web-search endpoint
	// (2026-10-17, still under development). PROXY_SEARCH_URL overrides it,
	// mainly for tests.
	defaultSearchURL = "https://api.synthetic.new/v2/search"
	// searchQueryMaxRunes is the longest query the proxy forwards; the API
	// documents no limit, so the proxy sets one.
	searchQueryMaxRunes = 1000
	// searchBodyCap bounds how much of the request body is read at all: a
	// query is a short string, so anything near this is abuse, not a search.
	searchBodyCap = 64 << 10
	// searchResponseCap caps how much of the upstream response is relayed. A
	// bigger body is refused with 502 rather than relayed truncated into
	// invalid JSON.
	searchResponseCap = 1 << 20
	// defaultSearchesPerRun is the per-run search cap when models.json does
	// not set limits.searches_per_run.
	defaultSearchesPerRun = 50
	// searchResultTextCap caps one result's text in the CLI output.
	searchResultTextCap = 600
	// searchTimeout bounds the proxy's upstream search call. Chat calls may
	// legitimately run for minutes; a search must not.
	searchTimeout = 2 * time.Minute
	// cliErrorBodyCap caps how much of a proxy error body the CLI prints.
	cliErrorBodyCap = 2 << 10
)

// searchClient calls the upstream search endpoint. It is deliberately not
// the chat client (15-minute timeout): a search must fail fast.
var searchClient = &http.Client{Timeout: searchTimeout}

// searchResponse is the documented /v2/search response shape (2026-10-17):
// {"results": [{"url", "title", "text", "published"}]}. Fields the API adds
// later parse away unnoticed.
type searchResponse struct {
	Results []searchResult `json:"results"`
}

// searchResult is one hit. Everything in it is untrusted upstream text.
type searchResult struct {
	URL       string `json:"url"`
	Title     string `json:"title"`
	Text      string `json:"text"`
	Published string `json:"published"`
}

// search serves POST /v2/search (issue #58). The key flows into exactly one
// place, the Authorization header of the upstream request: it is never
// logged, never echoed in an error, and never sent to the caller, which
// reaches this endpoint on 127.0.0.1 without any key.
func (p *proxy) search(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, searchBodyCap+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "search: unreadable request body")
		return
	}
	if len(body) > searchBodyCap {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("search request body too large: over %d bytes", searchBodyCap))
		return
	}
	// Only "query" is read; every other field a caller sends is dropped
	// here and never forwarded.
	var request struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		writeError(w, http.StatusBadRequest, `search request must be JSON with a "query" string: {"query": "<text>"}`)
		return
	}
	query := request.Query
	// The query is never echoed into a log or an error — only its length is
	// recorded anywhere.
	if strings.TrimSpace(query) == "" {
		writeError(w, http.StatusBadRequest, `search needs a non-empty "query": {"query": "<text>"}`)
		return
	}
	runes := utf8.RuneCountInString(query)
	if runes > searchQueryMaxRunes {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("search query is too long: %d runes, at most %d", runes, searchQueryMaxRunes))
		return
	}
	// Per-run cap, under p.mu next to the ledger it guards. Every forwarded
	// attempt counts; a rejected or invalid request does not.
	limit := p.searchCap()
	p.mu.Lock()
	if p.searches >= limit {
		p.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("search cap reached: %d searches per run (limits.searches_per_run in models.json); the count resets with the next run", limit))
		return
	}
	p.searches++
	p.mu.Unlock()
	// Exactly the documented field goes upstream, nothing else.
	forwarded, err := json.Marshal(struct {
		Query string `json:"query"`
	}{Query: query})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "search: could not encode the query")
		return
	}
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.searchUpstream(), bytes.NewReader(forwarded))
	if err != nil {
		writeError(w, http.StatusBadGateway, "search: bad upstream URL")
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	// THE key flow: the only place p.searchKey is ever written. Search
	// stays on synthetic.new for now (chat goes through hosted Otari), so
	// this endpoint gets the synthetic.new key and never the Otari one; the
	// Otari key never leaves the chat upstream. Logs below carry the
	// query's length, the result count and the status — no key, no query
	// text.
	upstream.Header.Set("Authorization", "Bearer "+p.searchKey)
	response, err := searchClient.Do(upstream)
	if err != nil {
		writeError(w, http.StatusBadGateway, "search: upstream unreachable")
		return
	}
	defer response.Body.Close()
	upstreamBody, err := io.ReadAll(io.LimitReader(response.Body, searchResponseCap+1))
	if err != nil {
		writeError(w, http.StatusBadGateway, "search: reading the upstream response failed")
		return
	}
	if len(upstreamBody) > searchResponseCap {
		writeError(w, http.StatusBadGateway, "search response too large")
		return
	}
	results := countResults(upstreamBody)
	log.Printf("search: query %d runes, %d results, upstream status %d", runes, results, response.StatusCode)
	// One ledger line per forwarded search: kind "search", cost 0 until
	// synthetic.new documents a price. runSpent and overage accounting are
	// untouched (cost 0).
	p.mu.Lock()
	p.appendLedger(LedgerLine{Kind: "search", Time: time.Now().UTC(), Model: "web-search",
		CostUSD: 0, QueryLen: runes, Results: results})
	p.mu.Unlock()
	contentType := response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(upstreamBody)
}

// searchUpstream is where forwarded searches go: PROXY_SEARCH_URL when set
// (tests point it at a stub), otherwise the documented endpoint.
func (p *proxy) searchUpstream() string {
	if p.searchURL != "" {
		return p.searchURL
	}
	return defaultSearchURL
}

// searchCap is this run's search limit: limits.searches_per_run when set,
// otherwise defaultSearchesPerRun.
func (p *proxy) searchCap() int {
	if p.searchesPerRun > 0 {
		return p.searchesPerRun
	}
	return defaultSearchesPerRun
}

// countResults is how many results a search body carries, 0 when it does
// not parse: the count feeds the log line and the ledger only.
func countResults(body []byte) int {
	var parsed searchResponse
	if json.Unmarshal(body, &parsed) != nil {
		return 0
	}
	return len(parsed.Results)
}

// countSearches is how many web searches the ledger records (kind "search").
func countSearches(lines []LedgerLine) int {
	n := 0
	for _, l := range lines {
		if l.Kind == "search" {
			n++
		}
	}
	return n
}

// stripControls removes control characters from s, keeping \n and \t:
// printed search results are untrusted data (issue #58), and stray control
// bytes could corrupt the terminal they are printed to.
func stripControls(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// capRunes cuts s to at most n runes, on rune boundaries.
func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ---------------------------------------------------------------- search CLI

// searchUsageError is a caller mistake: runSearch prints the usage line and
// exits 2. Every other error returns through main's log.Fatal, which exits 1.
type searchUsageError struct{ message string }

func (e *searchUsageError) Error() string { return e.message }

// runSearch is the "search" command: POST the query to the running proxy
// (which holds the key) and print the results as plain text.
func runSearch() error {
	err := searchMain(os.Args[2:], os.Stdout)
	var usage *searchUsageError
	if errors.As(err, &usage) {
		fmt.Fprintln(os.Stderr, `usage: harness search "<query>" [--limit N]`)
		fmt.Fprintln(os.Stderr, "harness:", usage.message)
		os.Exit(2)
	}
	return err
}

// searchMain is runSearch's body, split out so tests can drive it against a
// stub proxy without process exits: results go to stdout, the error carries
// the message (usage-classified where the exit code is 2). The 429 case
// returns the proxy's own message, which runSearch prints and exits 1 on.
func searchMain(args []string, stdout io.Writer) error {
	const defaultLimit = 5
	limit := defaultLimit
	query, haveQuery := "", false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--limit":
			i++
			if i >= len(args) {
				return &searchUsageError{"--limit needs a number"}
			}
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return &searchUsageError{fmt.Sprintf("--limit: %q is not a number", args[i])}
			}
			limit = n
		case strings.HasPrefix(arg, "--limit="):
			raw := strings.TrimPrefix(arg, "--limit=")
			n, err := strconv.Atoi(raw)
			if err != nil {
				return &searchUsageError{fmt.Sprintf("--limit: %q is not a number", raw)}
			}
			limit = n
		case strings.HasPrefix(arg, "-"):
			return &searchUsageError{fmt.Sprintf("unknown flag %q", arg)}
		case !haveQuery:
			query, haveQuery = arg, true
		default:
			return &searchUsageError{"one query only; quote it if it contains spaces"}
		}
	}
	if !haveQuery || strings.TrimSpace(query) == "" {
		return &searchUsageError{`a query is required: harness search "<query>" [--limit N]`}
	}
	if limit < 1 {
		limit = 1
	}
	payload, err := json.Marshal(struct {
		Query string `json:"query"`
	}{Query: query})
	if err != nil {
		return fmt.Errorf("search: could not encode the query: %w", err)
	}
	// The proxy listens on 127.0.0.1 and holds the key; the CLI sends it
	// nothing but the query (meteredRunSpend uses the same pattern).
	url := "http://127.0.0.1:" + envOr("PROXY_PORT", "8787") + "/v2/search"
	// The proxy bounds its own upstream call (searchTimeout); this outer
	// bound only covers a hung proxy.
	client := &http.Client{Timeout: searchTimeout + 30*time.Second}
	response, err := client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("search: the proxy is not reachable at %s (is `harness proxy` running?): %v", url, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, searchResponseCap+1))
	if err != nil {
		return fmt.Errorf("search: reading the proxy response failed: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		// 429 is the per-run cap: the proxy's message says so and names the
		// config key; print it alone. Any other status: status plus a capped
		// body — the body is data, never instructions.
		if response.StatusCode == http.StatusTooManyRequests {
			var proxyError struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(body, &proxyError) == nil && proxyError.Error.Message != "" {
				return fmt.Errorf("search: %s", proxyError.Error.Message)
			}
		}
		return fmt.Errorf("search: proxy status %d: %s", response.StatusCode, capRunes(string(body), cliErrorBodyCap))
	}
	var parsed searchResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("search: the proxy sent malformed JSON: %v", err)
	}
	renderSearchResults(stdout, parsed.Results, limit)
	return nil
}

// renderSearchResults prints at most limit results, one block each: title,
// URL, published date, then the text capped at searchResultTextCap. Every
// field is untrusted upstream data: control characters other than \n and \t
// are stripped, and nothing is ever executed or parsed as instructions.
func renderSearchResults(w io.Writer, results []searchResult, limit int) {
	for i, r := range results {
		if i >= limit {
			break
		}
		published := stripControls(r.Published)
		if strings.TrimSpace(published) == "" {
			published = "-"
		}
		fmt.Fprintf(w, "[%d] %s\n", i+1, stripControls(r.Title))
		fmt.Fprintf(w, "URL: %s\n", stripControls(r.URL))
		fmt.Fprintf(w, "Published: %s\n", published)
		fmt.Fprintf(w, "%s\n", capRunes(stripControls(r.Text), searchResultTextCap))
		if i < limit-1 && i < len(results)-1 {
			fmt.Fprintln(w)
		}
	}
}
