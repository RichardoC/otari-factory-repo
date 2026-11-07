package main

// Issue #2: web search runs inside the model call through hosted Otari. The
// proxy declares {"type":"otari_web_search","max_uses":K} on every chat call
// (dropping any otari_* tool the caller sent), counts the searches Otari
// reports in the response's otari_tool_calls, strips that field from what
// CodeAF receives, and charges max(list-price tokens + searches x
// limits.search_price_usd, Otari's cost_usd). These tests prove each Done-when
// bullet against an httptest Otari stub, streamed and non-streamed, in process
// (the binary-level wiring has its own test: TestProxyOtariKeyEndToEnd).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// searchFixtureModel is otariFixtureModel with a search-configured fixture:
// the caps the tests pin (3 per call, $0.01 a search, 4 a run) are the test's
// own fixture values, written into a models.json and loaded through
// loadConfig where the validator must accept them (AGENTS.md: a test never
// hard-codes a value from models.json).
func searchFixtureConfig(t *testing.T, maxUses int, price float64, perRun int) Config {
	t.Helper()
	return Config{
		Limits: Limits{
			Plan: "big", WeeklyCreditUSD: 20, RunCreditUSD: 5,
			SearchesPerRun:       perRun,
			SearchMaxUsesPerCall: maxUses,
			SearchPriceUSD:       price,
		},
		Models: []Model{otariFixtureModel()},
	}
}

// newSearchProxy builds an in-process proxy against a stub Otari upstream,
// with the search caps set the way runProxy sets them from loadConfig.
func newSearchProxy(t *testing.T, upstream *httptest.Server, cfg Config) (*proxy, *httptest.Server) {
	t.Helper()
	p := &proxy{
		upstream:       upstream.URL,
		apiKey:         "otari-key",
		ledger:         filepath.Join(t.TempDir(), "ledger.jsonl"),
		runCap:         cfg.Limits.RunCreditUSD,
		weekCap:        cfg.Limits.WeeklyCreditUSD,
		models:         cfg.Models,
		client:         upstream.Client(),
		searchesPerRun: cfg.Limits.SearchesPerRun,
		searchMaxUses:  cfg.Limits.SearchMaxUsesPerCall,
		searchPrice:    cfg.Limits.SearchPriceUSD,
	}
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	return p, server
}

// postChat sends one chat call to the proxy, non-streamed, with the given
// body, and returns the response bytes the client saw.
func postChat(t *testing.T, server *httptest.Server, body string) string {
	t.Helper()
	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat call: status %d, body:\n%s", resp.StatusCode, data)
	}
	return string(data)
}

// lastChatBody returns the body the stub upstream last received.
func lastChatBody(t *testing.T, bodies *[]string) string {
	t.Helper()
	if len(*bodies) == 0 {
		t.Fatal("the upstream stub saw no request")
	}
	return (*bodies)[len(*bodies)-1]
}

// upstreamTools decodes the tools array of an upstream request body.
func upstreamTools(t *testing.T, body string) []struct {
	Type    string `json:"type"`
	MaxUses int    `json:"max_uses"`
} {
	t.Helper()
	var sent struct {
		Tools []struct {
			Type    string `json:"type"`
			MaxUses int    `json:"max_uses"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("upstream body %s: %v", body, err)
	}
	return sent.Tools
}

// TestProxyDeclaresOtariWebSearch (Done-when 2): a chat call forwarded by the
// proxy carries the caller's function tools plus exactly one
// {"type":"otari_web_search","max_uses":3} and no other otari_* entry, even
// when the caller sent otari_web_fetch or otari_web_search with max_uses: 99.
func TestProxyDeclaresOtariWebSearch(t *testing.T) {
	captureLogs(t)
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	defer upstream.Close()
	_, server := newSearchProxy(t, upstream, searchFixtureConfig(t, 3, 0.01, 50))

	// The caller's hostile tools are dropped; the function tool survives.
	postChat(t, server, `{"model":"zai-org/glm-5.3-flash","messages":[],"tools":[`+
		`{"type":"function","function":{"name":"list_files"}},`+
		`{"type":"otari_web_fetch"},`+
		`{"type":"otari_web_search","max_uses":99}]}`)
	tools := upstreamTools(t, lastChatBody(t, &bodies))
	if len(tools) != 2 || tools[0].Type != "function" || tools[1].Type != "otari_web_search" || tools[1].MaxUses != 3 {
		t.Errorf("upstream tools = %+v, want [function, otari_web_search max_uses 3]", tools)
	}

	// A caller that sent no tools at all still gets the search declaration:
	// the array is created for it.
	postChat(t, server, `{"model":"zai-org/glm-5.3-flash","messages":[]}`)
	tools = upstreamTools(t, lastChatBody(t, &bodies))
	if len(tools) != 1 || tools[0].Type != "otari_web_search" || tools[0].MaxUses != 3 {
		t.Errorf("upstream tools = %+v, want exactly [otari_web_search max_uses 3]", tools)
	}
}

// TestProxySearchCapExhausted (Done-when 3): after the run has completed
// searches_per_run searches, the next forwarded call has no otari_web_search
// entry; when fewer than search_max_uses_per_call remain, max_uses equals
// what remains.
func TestProxySearchCapExhausted(t *testing.T) {
	captureLogs(t)
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],`+
			`"otari_tool_calls":[{"id":"otari_ws_1","type":"web_search","status":"completed","query":"q"},`+
			`{"id":"otari_ws_2","type":"web_search","status":"completed","query":"q"}],`+
			`"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	defer upstream.Close()
	// searches_per_run 4 with max_uses 3; the stub completes 2 searches per
	// call: call 1 is offered max_uses 3 and completes 2, call 2 is offered
	// max_uses 2 (what remains) and completes 2, call 3 gets none.
	_, server := newSearchProxy(t, upstream, searchFixtureConfig(t, 3, 0.01, 4))

	postChat(t, server, `{"model":"zai-org/glm-5.3-flash","messages":[]}`)
	if tools := upstreamTools(t, lastChatBody(t, &bodies)); len(tools) != 1 || tools[0].MaxUses != 3 {
		t.Errorf("call 1: upstream tools = %+v, want otari_web_search max_uses 3", tools)
	}

	postChat(t, server, `{"model":"zai-org/glm-5.3-flash","messages":[]}`)
	if tools := upstreamTools(t, lastChatBody(t, &bodies)); len(tools) != 1 || tools[0].Type != "otari_web_search" || tools[0].MaxUses != 2 {
		t.Errorf("call 2 (two searches left): upstream tools = %+v, want otari_web_search max_uses 2", tools)
	}

	postChat(t, server, `{"model":"zai-org/glm-5.3-flash","messages":[]}`)
	if tools := upstreamTools(t, lastChatBody(t, &bodies)); len(tools) != 0 {
		t.Errorf("call 3 (budget spent): upstream tools = %+v, want no tool at all", tools)
	}
}

// searchStreamChunks is the stub stream the streamed tests replay: an
// in_progress and a completed otari_tool_calls chunk for the same id (each
// with no choices but the search report), then the model's function-call
// chunk, then a usage chunk carrying Otari's cost_usd.
const searchStreamChunks = "" +
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[],"otari_tool_calls":[{"id":"otari_ws_1","type":"web_search","status":"in_progress","query":"docs"}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[],"otari_tool_calls":[{"id":"otari_ws_1","type":"web_search","status":"completed","query":"docs","sources":[{"url":"https://example.com","title":"Docs"}]}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"write_note","arguments":"{\"text\":\"found it\"}"}}]},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":60,"cost_usd":"0.000085"}}` + "\n\n" +
	"data: [DONE]\n\n"

// postStreamCall sends one streamed chat call and returns exactly what the
// client received, as one string of SSE lines.
func postStreamCall(t *testing.T, server *httptest.Server, body string) string {
	t.Helper()
	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("streamed chat call: status %d, body:\n%s", resp.StatusCode, data)
	}
	return string(data)
}

// TestProxyStreamedSearchCountedAndStripped (Done-when 4): a streamed call
// whose stub emits an in_progress and then a completed otari_tool_calls chunk
// for the same id, then a tool_calls chunk and a usage chunk with cost_usd,
// shows: the search total rises by 1; the ledger row has searches: 1; CodeAF
// receives no otari_tool_calls field and no empty chunk; and the tool_calls
// and usage reach CodeAF intact.
func TestProxyStreamedSearchCountedAndStripped(t *testing.T) {
	captureLogs(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, searchStreamChunks)
	}))
	defer upstream.Close()
	p, server := newSearchProxy(t, upstream, searchFixtureConfig(t, 3, 0.01, 50))

	got := postStreamCall(t, server, `{"model":"zai-org/glm-5.3-flash","stream":true,"messages":[]}`)

	if strings.Contains(got, "otari_tool_calls") {
		t.Errorf("the client received Otari's otari_tool_calls field:\n%s", got)
	}
	// No empty chunk: every relayed data line carries choices or usage, and
	// the two search-only chunks were dropped.
	for _, line := range strings.Split(got, "\n") {
		data, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []json.RawMessage `json:"choices"`
			Usage   json.RawMessage   `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("relayed chunk %q: %v", data, err)
		}
		if len(chunk.Choices) == 0 && len(chunk.Usage) == 0 {
			t.Errorf("the client received an emptied chunk (no choices, no usage):\n%s", data)
		}
	}
	if !strings.Contains(got, `"write_note"`) {
		t.Errorf("the function-call chunk did not reach the client intact:\n%s", got)
	}
	if !strings.Contains(got, `"cost_usd":"0.000085"`) {
		t.Errorf("the usage chunk did not reach the client intact:\n%s", got)
	}

	// The search total rose by 1, and the ledger row carries it.
	lines, err := readLedger(p.ledger)
	if err != nil {
		t.Fatal(err)
	}
	if p.searches != 1 {
		t.Errorf("p.searches = %d, want 1", p.searches)
	}
	var calls []*LedgerLine
	for i := range lines {
		if lines[i].Kind == "" && lines[i].Model == "zai-org/glm-5.3-flash" {
			calls = append(calls, &lines[i])
		}
	}
	if len(calls) != 1 {
		t.Fatalf("ledger has %d call rows, want 1:\n%v", len(calls), lines)
	}
	if calls[0].Searches != 1 {
		t.Errorf("ledger row searches = %d, want 1", calls[0].Searches)
	}
	// The cost: list-price tokens + one search at $0.01 -- Otari's own
	// cost_usd (0.000085) is smaller, so the computed figure wins.
	want := CostOf(otariFixtureModel(), Usage{PromptTokens: 100, CompletionTokens: 60}) + 0.01
	if diff := calls[0].CostUSD - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("ledger cost_usd = %v, want %v (list-price tokens + one $0.01 search)", calls[0].CostUSD, want)
	}
}

// TestProxyNonStreamedSearchCountedAndStripped (Done-when 5): the
// non-streamed equivalent of the streamed test above.
func TestProxyNonStreamedSearchCountedAndStripped(t *testing.T) {
	captureLogs(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"write_note","arguments":"{\"text\":\"found it\"}"}}]},"finish_reason":"tool_calls"}],`+
			`"otari_tool_calls":[{"id":"otari_ws_1","type":"web_search","status":"in_progress","query":"docs"},`+
			`{"id":"otari_ws_1","type":"web_search","status":"completed","query":"docs","sources":[{"url":"https://example.com","title":"Docs"}]}],`+
			`"usage":{"prompt_tokens":100,"completion_tokens":60,"cost_usd":"0.000085"}}`)
	}))
	defer upstream.Close()
	p, server := newSearchProxy(t, upstream, searchFixtureConfig(t, 3, 0.01, 50))

	got := postChat(t, server, `{"model":"zai-org/glm-5.3-flash","messages":[]}`)

	if strings.Contains(got, "otari_tool_calls") {
		t.Errorf("the client received Otari's otari_tool_calls field:\n%s", got)
	}
	if !strings.Contains(got, `"write_note"`) {
		t.Errorf("the function call did not reach the client intact:\n%s", got)
	}
	if !strings.Contains(got, `"cost_usd":"0.000085"`) {
		t.Errorf("the usage did not reach the client intact:\n%s", got)
	}

	lines, err := readLedger(p.ledger)
	if err != nil {
		t.Fatal(err)
	}
	if p.searches != 1 {
		t.Errorf("p.searches = %d, want 1", p.searches)
	}
	var calls []*LedgerLine
	for i := range lines {
		if lines[i].Kind == "" && lines[i].Model == "zai-org/glm-5.3-flash" {
			calls = append(calls, &lines[i])
		}
	}
	if len(calls) != 1 {
		t.Fatalf("ledger has %d call rows, want 1:\n%v", len(calls), lines)
	}
	if calls[0].Searches != 1 {
		t.Errorf("ledger row searches = %d, want 1 (the in_progress entry and the duplicate id count once)", calls[0].Searches)
	}
	want := CostOf(otariFixtureModel(), Usage{PromptTokens: 100, CompletionTokens: 60}) + 0.01
	if diff := calls[0].CostUSD - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("ledger cost_usd = %v, want %v (list-price tokens + one $0.01 search)", calls[0].CostUSD, want)
	}
}

// TestSearchCostIsLargerOfListPlusSearchesAndOtari (Done-when 6): the
// recorded cost is the larger of (list-price token cost + searches x
// search_price_usd) and Otari's cost_usd -- here the search-heavy call whose
// cost_usd is far above the tokens, and the plain call whose cost_usd is far
// below.
func TestSearchCostIsLargerOfListPlusSearchesAndOtari(t *testing.T) {
	captureLogs(t)
	costUSD := "0.000085"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],"otari_tool_calls":[{"id":"otari_ws_1","type":"web_search","status":"completed","query":"q"}],`+
			`"usage":{"prompt_tokens":100,"completion_tokens":60,"cost_usd":"`+costUSD+`"}}`)
	}))
	defer upstream.Close()
	p, server := newSearchProxy(t, upstream, searchFixtureConfig(t, 3, 0.01, 50))

	tokens := CostOf(otariFixtureModel(), Usage{PromptTokens: 100, CompletionTokens: 60})

	// A cheap cost_usd: tokens + one search wins.
	postChat(t, server, `{"model":"zai-org/glm-5.3-flash","messages":[]}`)
	// An expensive cost_usd: Otari's figure wins (it covers the search
	// rounds and any loop rounds the token counts miss).
	costUSD = "0.5"
	postChat(t, server, `{"model":"zai-org/glm-5.3-flash","messages":[]}`)

	lines, err := readLedger(p.ledger)
	if err != nil {
		t.Fatal(err)
	}
	var calls []*LedgerLine
	for i := range lines {
		if lines[i].Kind == "" && lines[i].Model == "zai-org/glm-5.3-flash" {
			calls = append(calls, &lines[i])
		}
	}
	if len(calls) != 2 {
		t.Fatalf("ledger has %d call rows, want 2:\n%v", len(calls), lines)
	}
	if diff := calls[0].CostUSD - (tokens + 0.01); diff > 1e-12 || diff < -1e-12 {
		t.Errorf("call 1 cost_usd = %v, want %v (tokens + one $0.01 search; otari 0.000085 is smaller)", calls[0].CostUSD, tokens+0.01)
	}
	if calls[1].CostUSD != 0.5 {
		t.Errorf("call 2 cost_usd = %v, want 0.5 (Otari's cost_usd, larger than tokens + one search)", calls[1].CostUSD)
	}
	if calls[1].OtariCostUSD != 0.5 {
		t.Errorf("call 2 otari_cost_usd = %v, want 0.5", calls[1].OtariCostUSD)
	}
}

// TestSearchCountIgnoresMalformedAndDuplicates: the count is the distinct
// completed ids, bounded to the max_uses declared; malformed entries are
// ignored, in_progress and failed never count.
func TestSearchCountIgnoresMalformedAndDuplicates(t *testing.T) {
	raw := json.RawMessage(`[
		{"id":"a","status":"completed"},
		{"id":"a","status":"completed"},
		{"id":"b","status":"in_progress"},
		{"id":"c","status":"failed"},
		{"status":"completed"},
		{"id":42,"status":"completed"},
		"junk",
		{"id":"d","status":"completed"}
	]`)
	if got := countCompletedSearches(raw, 10); got != 2 {
		t.Errorf("countCompletedSearches = %d, want 2 (distinct completed ids a and d; b, c and the malformed ignored)", got)
	}
	if got := countCompletedSearches(raw, 1); got != 1 {
		t.Errorf("countCompletedSearches with max_uses 1 = %d, want 1 (bounded)", got)
	}
	if got := countCompletedSearches(raw, 0); got != 0 {
		t.Errorf("countCompletedSearches with max_uses 0 = %d, want 0", got)
	}
}

// TestLoadConfigSearchLimitsDefaultsAndValidator (Done-when 4 of the change
// list): the new limits default when absent and the validator refuses a
// negative or absurd value naming the field.
func TestLoadConfigSearchLimitsDefaultsAndValidator(t *testing.T) {
	write := func(t *testing.T, limits string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "models.json")
		body := `{"limits":{"plan":"big","weekly_credit_usd":20,"run_credit_usd":5` + limits + `},"models":[{"id":"m","upstream":"u","efforts":["medium"],"api_input":1,"api_output":1}]}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	cfg, err := loadConfig(write(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.SearchMaxUsesPerCall != 3 {
		t.Errorf("default search_max_uses_per_call = %d, want 3", cfg.Limits.SearchMaxUsesPerCall)
	}
	if cfg.Limits.SearchPriceUSD != 0.01 {
		t.Errorf("default search_price_usd = %v, want 0.01", cfg.Limits.SearchPriceUSD)
	}

	for name, limits := range map[string]string{
		"negative max_uses": `,  "search_max_uses_per_call":-1`,
		"absurd max_uses":   `,  "search_max_uses_per_call":1000`,
		"negative price":    `,  "search_price_usd":-0.01`,
		"absurd price":      `,  "search_price_usd":5`,
	} {
		_, err := loadConfig(write(t, limits))
		if err == nil {
			t.Errorf("%s: loadConfig accepted the value, want a refusal", name)
			continue
		}
		want := "search_max_uses_per_call"
		if strings.Contains(name, "price") {
			want = "search_price_usd"
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %q does not name %s", name, err, want)
		}
	}
}

// TestProxyConcurrentSearchReservations (issue #11 Done-when 2): twenty
// streamed calls are forwarded at once with searches_per_run 10 and
// search_max_uses_per_call 3. The stub holds every response until all twenty
// requests have arrived, so every declaration is in flight together: their
// sum is the run cap and never more. Once the calls finish and release their
// reservations, the next call declares the per-call cap again.
func TestProxyConcurrentSearchReservations(t *testing.T) {
	captureLogs(t)
	const calls = 20
	var mu sync.Mutex
	var bodies []string
	var arrived sync.WaitGroup
	arrived.Add(calls)
	var seen atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		if seen.Add(1) <= calls {
			arrived.Done()
			// Hold every response until all twenty requests have been
			// forwarded, so all twenty reservations are in flight at the
			// same moment.
			arrived.Wait()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: [DONE]\n\n")
	}))
	defer upstream.Close()
	p, server := newSearchProxy(t, upstream, searchFixtureConfig(t, 3, 0.01, 10))

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(calls)
	for i := 0; i < calls; i++ {
		go func() {
			defer wg.Done()
			<-start
			resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json",
				strings.NewReader(`{"model":"zai-org/glm-5.3-flash","stream":true,"messages":[]}`))
			if err != nil {
				t.Errorf("concurrent call: %v", err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	close(start)
	wg.Wait()

	mu.Lock()
	got := append([]string{}, bodies...)
	mu.Unlock()
	if len(got) != calls {
		t.Fatalf("the upstream stub saw %d requests, want %d", len(got), calls)
	}
	declared := 0
	for _, body := range got {
		for _, tool := range upstreamTools(t, body) {
			if tool.Type != "otari_web_search" {
				continue
			}
			if tool.MaxUses > 3 {
				t.Errorf("a call declared max_uses %d, want at most search_max_uses_per_call 3", tool.MaxUses)
			}
			declared += tool.MaxUses
		}
	}
	if declared > 10 {
		t.Errorf("the concurrent calls declared %d searches in total, want at most searches_per_run 10", declared)
	}
	if declared != 10 {
		t.Errorf("the concurrent calls declared %d searches in total, want exactly the 10 of the run cap", declared)
	}

	// Every reservation is released once its call ends, so the next call sees
	// the full remaining budget again and declares the per-call cap.
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		reserved := p.reserved
		p.mu.Unlock()
		if reserved == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("p.reserved = %d after every call ended, want 0", reserved)
		}
		time.Sleep(time.Millisecond)
	}
	postStreamCall(t, server, `{"model":"zai-org/glm-5.3-flash","stream":true,"messages":[]}`)
	mu.Lock()
	last := bodies[len(bodies)-1]
	mu.Unlock()
	if tools := upstreamTools(t, last); len(tools) != 1 || tools[0].MaxUses != 3 {
		t.Errorf("the call after the release declared %+v, want otari_web_search max_uses 3", tools)
	}
}

// TestProxyOverageIncludesSearchCost (issue #11 Done-when 3): once the weekly
// credit is exhausted (overage_usd above 0), a call that completes two
// searches adds those searches' charge to the overage total on top of its
// API-priced tokens.
func TestProxyOverageIncludesSearchCost(t *testing.T) {
	captureLogs(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],`+
			`"otari_tool_calls":[{"id":"otari_ws_1","type":"web_search","status":"completed","query":"q"},`+
			`{"id":"otari_ws_2","type":"web_search","status":"completed","query":"q"}],`+
			`"usage":{"prompt_tokens":100,"completion_tokens":60}}`)
	}))
	defer upstream.Close()
	p, server := newSearchProxy(t, upstream, searchFixtureConfig(t, 3, 0.01, 50))
	// The weekly credit is exhausted by the first call; overage_usd (the API
	// overage cap) is above 0, so the next call is recorded as overage.
	p.weekCap = 0
	p.overageCap = 1
	postChat(t, server, `{"model":"zai-org/glm-5.3-flash","messages":[]}`)
	if !p.overage {
		t.Fatal("the proxy did not mark itself overage after the weekly credit was exhausted")
	}
	before := p.overageApi

	postChat(t, server, `{"model":"zai-org/glm-5.3-flash","messages":[]}`)

	usage := Usage{PromptTokens: 100, CompletionTokens: 60}
	want := ApiCostOf(otariFixtureModel(), usage) + 2*0.01
	if diff := (p.overageApi - before) - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("overageApi rose by %v, want %v (API-priced tokens + two $0.01 searches)", p.overageApi-before, want)
	}
}

// TestLoadConfigSearchPriceZeroIsFree (issue #11 Done-when 4): a written
// search_price_usd of 0 is a real price meaning free -- it loads as 0 and a
// search adds nothing to the recorded cost -- while an absent field still
// defaults to 0.01 (TestLoadConfigSearchLimitsDefaultsAndValidator) and a
// negative value is refused naming the field.
func TestLoadConfigSearchPriceZeroIsFree(t *testing.T) {
	captureLogs(t)
	model, err := json.Marshal(otariFixtureModel())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "models.json")
	body := `{"limits":{"plan":"big","weekly_credit_usd":20,"run_credit_usd":5,` +
		`"searches_per_run":50,"search_max_uses_per_call":3,"search_price_usd":0},` +
		`"models":[` + string(model) + `]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.SearchPriceUSD != 0 {
		t.Fatalf("search_price_usd: 0 loaded as %v, want 0 (free)", cfg.Limits.SearchPriceUSD)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],`+
			`"otari_tool_calls":[{"id":"otari_ws_1","type":"web_search","status":"completed","query":"q"},`+
			`{"id":"otari_ws_2","type":"web_search","status":"completed","query":"q"}],`+
			`"usage":{"prompt_tokens":100,"completion_tokens":60}}`)
	}))
	defer upstream.Close()
	p, server := newSearchProxy(t, upstream, cfg)
	postChat(t, server, `{"model":"zai-org/glm-5.3-flash","messages":[]}`)

	lines, err := readLedger(p.ledger)
	if err != nil {
		t.Fatal(err)
	}
	var calls []*LedgerLine
	for i := range lines {
		if lines[i].Kind == "" && lines[i].Model == "zai-org/glm-5.3-flash" {
			calls = append(calls, &lines[i])
		}
	}
	if len(calls) != 1 {
		t.Fatalf("ledger has %d call rows, want 1:\n%v", len(calls), lines)
	}
	want := CostOf(otariFixtureModel(), Usage{PromptTokens: 100, CompletionTokens: 60})
	if diff := calls[0].CostUSD - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("ledger cost_usd = %v, want %v (free searches add nothing)", calls[0].CostUSD, want)
	}
}
