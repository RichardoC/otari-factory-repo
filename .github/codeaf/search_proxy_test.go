package main

// Proxy-side tests for the search endpoint (issue #58): this file alone owns
// the POST /v2/search contract — request validation, exactly {"query": ...}
// forwarded upstream, the key in the Authorization header and never in a log
// line, the response size cap, the per-run search cap, and the ledger line.
// The "harness search" CLI has its own tests; nothing here drives it.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// searchStubOK is a small successful upstream response: exactly 2 results
// (the count is asserted in TestSearchNeverLogsTheKey's log line).
const searchStubOK = `{"results":[` +
	`{"url":"https://example.com/a","title":"A","text":"alpha","published":"2026-10-17T00:00:00Z"},` +
	`{"url":"https://example.com/b","title":"B","text":"beta","published":"2026-10-18T00:00:00Z"}]}`

// searchStub is a stand-in for synthetic.new's /v2/search: it records every
// request (Authorization header and raw body) and answers with the configured
// status, Content-Type and body. Everything is mutex-guarded because the stub
// runs on the httptest server's goroutines while the test reads it.
type searchStub struct {
	srv         *httptest.Server
	mu          sync.Mutex
	auths       []string
	bodies      []string
	status      int
	contentType string
	body        string
}

// startSearchStub runs a stub answering every request with the given
// response, closed when the test ends.
func startSearchStub(t *testing.T, status int, contentType, body string) *searchStub {
	t.Helper()
	stub := &searchStub{status: status, contentType: contentType, body: body}
	stub.srv = httptest.NewServer(stub)
	t.Cleanup(stub.srv.Close)
	return stub
}

// answer changes what the stub serves from now on (for the error-path tests).
func (s *searchStub) answer(status int, contentType, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.contentType, s.body = status, contentType, body
}

func (s *searchStub) url() string { return s.srv.URL }

// requests returns what the stub has seen, one entry per request.
func (s *searchStub) requests() (auths, bodies []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auths...), append([]string(nil), s.bodies...)
}

func (s *searchStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

func (s *searchStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.auths = append(s.auths, r.Header.Get("Authorization"))
	s.bodies = append(s.bodies, string(raw))
	status, contentType, body := s.status, s.contentType, s.body
	s.mu.Unlock()
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// newSearchProxy builds a proxy the way the run job does, pointed at stub:
// the key stays in the proxy, the ledger is a fresh temp file, and the
// searches-per-run cap is set as given (0 falls back to the default).
func newSearchProxy(t *testing.T, key string, stub *searchStub, searchesPerRun int) *proxy {
	t.Helper()
	return &proxy{
		upstream:       stub.url(),
		apiKey:         key, // the Otari key; search must never see it
		searchKey:      key,
		ledger:         filepath.Join(t.TempDir(), "ledger.jsonl"),
		searchURL:      stub.url(),
		searchesPerRun: searchesPerRun,
	}
}

// postSearch drives one request through the real router with a recorder.
func postSearch(t *testing.T, p *proxy, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v2/search", strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, r)
	return rec
}

// captureLogs redirects the package log into a buffer until the test ends
// (restored via t.Cleanup: a defer inside this helper would fire when the
// helper returns, not when the test does). Use it wherever the proxy is
// expected to log, to keep the refusal noise out of the test output — and
// where the log itself is under test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return &buf
}

// proxyErrorBody is the JSON shape writeError produces.
type proxyErrorBody struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// TestSearchValidationReturns400: an empty query, a whitespace-only query, a
// missing query, a query past searchQueryMaxRunes, a non-JSON body and a body
// over searchBodyCap are all refused with 400 — and none of them is
// forwarded upstream.
func TestSearchValidationReturns400(t *testing.T) {
	stub := startSearchStub(t, http.StatusOK, "application/json", searchStubOK)
	p := newSearchProxy(t, "sk-test-58", stub, 0)
	captureLogs(t) // writeError logs every refusal; keep it out of the output

	tooManyRunes := strings.Repeat("é", searchQueryMaxRunes+1) // 1001 runes, 2002 bytes
	oversized := `{"query":"hi","pad":"` + strings.Repeat("p", searchBodyCap) + `"}`
	cases := []struct {
		name    string
		body    string
		message string // a substring of the expected error message
	}{
		{"empty query", `{"query": ""}`, "non-empty"},
		{"whitespace-only query", `{"query": " \t\n  "}`, "non-empty"},
		{"missing query field", `{}`, "non-empty"},
		{"1001 runes", `{"query": "` + tooManyRunes + `"}`, "too long"},
		{"not JSON", `search the docs for http.Client`, "must be JSON"},
		{"body over searchBodyCap", oversized, "too large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postSearch(t, p, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
			}
			var proxyError proxyErrorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &proxyError); err != nil || proxyError.Error.Message == "" {
				t.Fatalf("body %q is not the proxy's error shape: %v", rec.Body.String(), err)
			}
			if !strings.Contains(proxyError.Error.Message, tc.message) {
				t.Errorf("message %q does not mention %q", proxyError.Error.Message, tc.message)
			}
		})
	}
	if n := stub.count(); n != 0 {
		t.Errorf("upstream saw %d requests, want 0: refused searches must not be forwarded", n)
	}
}

// TestSearchQueryOf1000RunesIsForwarded: the boundary query — exactly
// searchQueryMaxRunes multibyte runes — is accepted and forwarded whole, so
// the limit counts runes, not bytes.
func TestSearchQueryOf1000RunesIsForwarded(t *testing.T) {
	stub := startSearchStub(t, http.StatusOK, "application/json", searchStubOK)
	p := newSearchProxy(t, "sk-test-58", stub, 0)
	captureLogs(t)

	query := strings.Repeat("é", searchQueryMaxRunes) // 1000 runes, 2000 bytes
	rec := postSearch(t, p, `{"query": "`+query+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	_, bodies := stub.requests()
	if len(bodies) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(bodies))
	}
	var forwarded struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &forwarded); err != nil {
		t.Fatalf("upstream body %q: %v", bodies[0], err)
	}
	if got := utf8.RuneCountInString(forwarded.Query); got != searchQueryMaxRunes {
		t.Errorf("forwarded query = %d runes, want %d", got, searchQueryMaxRunes)
	}
	if got := len(forwarded.Query); got != 2*searchQueryMaxRunes {
		t.Errorf("forwarded query = %d bytes, want %d (the limit is runes, not bytes)", got, 2*searchQueryMaxRunes)
	}
}

// TestSearchForwardsOnlyTheQueryField: whatever else a caller stuffs into the
// request, upstream receives exactly {"query": ...} — no "limit", no extra
// caller fields, nothing.
func TestSearchForwardsOnlyTheQueryField(t *testing.T) {
	stub := startSearchStub(t, http.StatusOK, "application/json", searchStubOK)
	p := newSearchProxy(t, "sk-test-58", stub, 0)
	captureLogs(t)

	rec := postSearch(t, p, `{"query":"hello search","limit":5,"foo":"bar"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	_, bodies := stub.requests()
	if len(bodies) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(bodies))
	}
	if want := `{"query":"hello search"}`; bodies[0] != want {
		t.Errorf("upstream got %q, want exactly %q", bodies[0], want)
	}
	var forwarded map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &forwarded); err != nil {
		t.Fatalf("upstream body %q: %v", bodies[0], err)
	}
	if len(forwarded) != 1 || forwarded["query"] != "hello search" {
		t.Errorf(`upstream got %v, want a map with only "query" = "hello search"`, forwarded)
	}
	for _, gone := range []string{`"limit"`, `"foo"`} {
		if strings.Contains(bodies[0], gone) {
			t.Errorf("upstream body still carries %s: %q", gone, bodies[0])
		}
	}
}

// TestSearchSendsBearerKeyUpstream: the proxy's key flows into exactly one
// place — the Authorization header of the upstream request.
func TestSearchSendsBearerKeyUpstream(t *testing.T) {
	const key = "sk-test-key-58-not-a-real-key"
	stub := startSearchStub(t, http.StatusOK, "application/json", searchStubOK)
	p := newSearchProxy(t, key, stub, 0)
	captureLogs(t)

	if rec := postSearch(t, p, `{"query":"docs for http.Client"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	auths, _ := stub.requests()
	if len(auths) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(auths))
	}
	if auths[0] != "Bearer "+key {
		t.Errorf("upstream Authorization = %q, want %q", auths[0], "Bearer "+key)
	}
}

// TestSearchNeverLogsTheKey: on the success path and on an upstream error
// path, the captured log carries no trace of the key.
func TestSearchNeverLogsTheKey(t *testing.T) {
	const key = "sk-SECRET-TEST-KEY-58"
	stub := startSearchStub(t, http.StatusOK, "application/json", searchStubOK)
	p := newSearchProxy(t, key, stub, 0)
	logs := captureLogs(t)

	if rec := postSearch(t, p, `{"query":"success path"}`); rec.Code != http.StatusOK {
		t.Fatalf("success path: status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	// Upstream starts failing; the proxy relays the error — and must still
	// not log the key.
	stub.answer(http.StatusInternalServerError, "application/json", `{"error":{"message":"upstream exploded"}}`)
	if rec := postSearch(t, p, `{"query":"error path"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("error path: status = %d, want the upstream 500 relayed (body: %s)", rec.Code, rec.Body.String())
	}

	// Both paths really did log (one "search: query ..." line each), so the
	// no-key assertion below is over real content, not an empty buffer.
	captured := logs.String()
	if got := strings.Count(captured, "search: query"); got != 2 {
		t.Errorf("captured %d search log lines, want one per path:\n%s", got, captured)
	}
	if !strings.Contains(captured, "upstream status 500") {
		t.Errorf("the error path left no log line; captured:\n%s", captured)
	}
	if strings.Contains(captured, key) {
		t.Errorf("the key leaked into the log:\n%s", captured)
	}
}

// TestSearchResponseOverCapIsRefused502: an upstream body bigger than
// searchResponseCap is refused with 502, not relayed truncated into invalid
// JSON.
func TestSearchResponseOverCapIsRefused502(t *testing.T) {
	stub := startSearchStub(t, http.StatusOK, "application/json", strings.Repeat("x", searchResponseCap+1))
	p := newSearchProxy(t, "sk-test-58", stub, 0)
	captureLogs(t)

	rec := postSearch(t, p, `{"query":"big response"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var proxyError proxyErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &proxyError); err != nil || proxyError.Error.Message == "" {
		t.Fatalf("body %q is not the proxy's error shape: %v", rec.Body.String(), err)
	}
	if !strings.Contains(proxyError.Error.Message, "too large") {
		t.Errorf("message %q does not say the response is too large", proxyError.Error.Message)
	}
}

// TestSearchResponseAtOrBelowCapIsRelayed: a body under the cap and a body at
// exactly searchResponseCap both reach the caller with the upstream status,
// the upstream Content-Type and the upstream bytes verbatim — including an
// upstream error status, which is relayed rather than rewritten.
func TestSearchResponseAtOrBelowCapIsRelayed(t *testing.T) {
	// A valid search response padded to exactly searchResponseCap bytes.
	const head = `{"results":[{"url":"https://example.com/padded","title":"Padded","published":"2026-10-17T00:00:00Z","text":"`
	const tail = `"}]}`
	atCap := head + strings.Repeat("a", searchResponseCap-len(head)-len(tail)) + tail
	if len(atCap) != searchResponseCap {
		t.Fatalf("test bug: padded body is %d bytes, want %d", len(atCap), searchResponseCap)
	}

	cases := []struct {
		name        string
		status      int
		contentType string
		body        string
	}{
		{"under the cap", http.StatusOK, "application/vnd.synthetic.search+json", searchStubOK},
		{"exactly at the cap", http.StatusOK, "application/json", atCap},
		{"upstream error below the cap", http.StatusForbidden, "application/json", `{"error":{"message":"forbidden by upstream"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := startSearchStub(t, tc.status, tc.contentType, tc.body)
			p := newSearchProxy(t, "sk-test-58", stub, 0)
			captureLogs(t)

			rec := postSearch(t, p, `{"query":"relay it"}`)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want the upstream status %d (body: %s)", rec.Code, tc.status, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != tc.contentType {
				t.Errorf("Content-Type = %q, want the upstream %q", got, tc.contentType)
			}
			if got := rec.Body.String(); got != tc.body {
				t.Errorf("body not relayed verbatim: got %d bytes, want %d", len(got), len(tc.body))
			}
		})
	}
}

// TestSearchPerRunCapReturns429: the first searchesPerRun searches go through,
// the next is refused with 429 and a message naming both the cap and its
// models.json key — and the refused search is not forwarded upstream.
func TestSearchPerRunCapReturns429(t *testing.T) {
	stub := startSearchStub(t, http.StatusOK, "application/json", searchStubOK)
	p := newSearchProxy(t, "sk-test-58", stub, 2)
	captureLogs(t)

	for i := 1; i <= 2; i++ {
		rec := postSearch(t, p, fmt.Sprintf(`{"query":"search %d"}`, i))
		if rec.Code != http.StatusOK {
			t.Fatalf("search %d: status = %d, want 200 (body: %s)", i, rec.Code, rec.Body.String())
		}
	}
	rec := postSearch(t, p, `{"query":"one too many"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body: %s)", rec.Code, rec.Body.String())
	}
	var proxyError proxyErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &proxyError); err != nil || proxyError.Error.Message == "" {
		t.Fatalf("429 body %q is not the proxy's error shape: %v", rec.Body.String(), err)
	}
	msg := proxyError.Error.Message
	if !strings.Contains(msg, "limits.searches_per_run") {
		t.Errorf("message %q does not name limits.searches_per_run", msg)
	}
	if !strings.Contains(msg, "2 searches per run") {
		t.Errorf("message %q does not name the cap itself", msg)
	}
	if n := stub.count(); n != 2 {
		t.Errorf("upstream saw %d requests, want 2: the capped search must not be forwarded", n)
	}
}

// TestSearchCapIsPerProxyInstance: the counter is the proxy instance's own,
// so a second instance starts from zero even with the same cap.
func TestSearchCapIsPerProxyInstance(t *testing.T) {
	stub := startSearchStub(t, http.StatusOK, "application/json", searchStubOK)
	first := newSearchProxy(t, "sk-test-58", stub, 1)
	second := newSearchProxy(t, "sk-test-58", stub, 1)
	captureLogs(t)

	if rec := postSearch(t, first, `{"query":"first instance"}`); rec.Code != http.StatusOK {
		t.Fatalf("first instance, search 1: status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if rec := postSearch(t, first, `{"query":"first instance again"}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("first instance, search 2: status = %d, want 429 (body: %s)", rec.Code, rec.Body.String())
	}
	if rec := postSearch(t, second, `{"query":"second instance"}`); rec.Code != http.StatusOK {
		t.Fatalf("second instance: status = %d, want 200 (the cap is per instance; body: %s)", rec.Code, rec.Body.String())
	}
}

// TestSearchInvalidRequestDoesNotConsumeTheCap: a request refused before the
// cap check leaves the run's allowance untouched.
func TestSearchInvalidRequestDoesNotConsumeTheCap(t *testing.T) {
	stub := startSearchStub(t, http.StatusOK, "application/json", searchStubOK)
	p := newSearchProxy(t, "sk-test-58", stub, 1)
	captureLogs(t)

	if rec := postSearch(t, p, `{"query":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid search: status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if rec := postSearch(t, p, `{"query":"still allowed"}`); rec.Code != http.StatusOK {
		t.Fatalf("valid search after an invalid one: status = %d, want 200 — the invalid request must not consume the cap (body: %s)", rec.Code, rec.Body.String())
	}
	if rec := postSearch(t, p, `{"query":"and now capped"}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second valid search: status = %d, want 429 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestSearchLedgerRecordsTheSearch: one ledger line per forwarded search —
// kind "search", the query's length in runes, the upstream result count, cost
// 0 — and never the query text itself.
func TestSearchLedgerRecordsTheSearch(t *testing.T) {
	const upstreamBody = `{"results":[` +
		`{"url":"https://example.com/a","title":"A","text":"alpha","published":"2026-10-17T00:00:00Z"},` +
		`{"url":"https://example.com/b","title":"B","text":"beta","published":"2026-10-18T00:00:00Z"},` +
		`{"url":"https://example.com/c","title":"C","text":"gamma","published":"2026-10-19T00:00:00Z"}]}`
	stub := startSearchStub(t, http.StatusOK, "application/json", upstreamBody)
	p := newSearchProxy(t, "sk-test-58", stub, 0)
	captureLogs(t)

	query := "héllo wörld" // 11 runes, 13 bytes: the ledger counts runes
	if rec := postSearch(t, p, `{"query":"`+query+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	lines, err := readLedger(p.ledger)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("ledger has %d lines, want 1: %+v", len(lines), lines)
	}
	line := lines[0]
	if line.Kind != "search" {
		t.Errorf("kind = %q, want %q", line.Kind, "search")
	}
	if want := utf8.RuneCountInString(query); line.QueryLen != want {
		t.Errorf("query_len = %d, want %d (runes, not bytes)", line.QueryLen, want)
	}
	if line.Results != 3 {
		t.Errorf("results = %d, want 3 (the upstream body's result count)", line.Results)
	}
	if line.CostUSD != 0 {
		t.Errorf("cost_usd = %v, want 0 (no documented price yet)", line.CostUSD)
	}

	// The query text itself must not land in the ledger — only its length.
	raw, err := os.ReadFile(p.ledger)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), query) {
		t.Errorf("the query text leaked into the ledger: %s", raw)
	}
}
