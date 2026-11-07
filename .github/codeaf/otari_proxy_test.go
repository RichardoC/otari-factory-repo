package main

// Issue #1: chat completions go through hosted Otari (https://eu.api.otari.ai/api/v1)
// with the Otari key, web search stays on synthetic.new with the synthetic.new key,
// and the synthetic.new quota guard (/v2/quotas, min_credit_percent) is gone — Otari
// enforces the organisation's own budget. These tests prove the wiring end to end:
// the keys arrive on stdin (line 1 Otari, line 2 synthetic.new), each key reaches
// only its own upstream, the model is sent as Otari's "synthetic.new:" selector,
// Otari's own cost_usd is recorded for reference only, and no quota call is made.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// otariFixtureModel is the one model the Otari proxy tests use: the test's
// own fixture, built here rather than read from models.json (AGENTS.md: a
// test never hard-codes a value from it). The prices are distinct from the
// real catalog's on purpose, so any code path that priced against
// models.json instead of the config the proxy was given would fail these
// tests rather than pass by coincidence.
func otariFixtureModel() Model {
	return Model{
		ID:              "zai-org/glm-5.3-flash",
		Upstream:        "hf:zai-org/GLM-5.3-Flash",
		Name:            "GLM-5.3-Flash (synthetic.new via Otari)",
		Role:            "base",
		MaxConcurrent:   2,
		Input:           0.21,
		Output:          0.62,
		CacheRead:       0.05,
		APIInput:        0.31,
		APICached:       0.07,
		APIOutput:       0.83,
		ContextLength:   524288,
		InputModalities: []string{"text", "image"},
		Efforts:         []string{"medium", "high", "max"},
	}
}

// otariModels writes a minimal models.json holding otariFixtureModel, and
// returns its path. Same shape writePlannerModels writes.
func otariModels(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	cfg := Config{
		Limits: Limits{Plan: "big", WeeklyCreditUSD: 20, RunCreditUSD: 5},
		Models: []Model{otariFixtureModel()},
	}
	body, err := json.MarshalIndent(cfg, "", "\t")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// seenRequests records every request a stub saw: path, Authorization header
// and body. Safe for the proxy's startup and the test's calls concurrently.
type seenRequests struct {
	mu     sync.Mutex
	paths  []string
	auths  []string
	bodies []string
}

func (s *seenRequests) add(path, auth, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paths = append(s.paths, path)
	s.auths = append(s.auths, auth)
	s.bodies = append(s.bodies, body)
}

func (s *seenRequests) snapshot() (paths, auths, bodies []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.paths...), append([]string{}, s.auths...), append([]string{}, s.bodies...)
}

// TestProxyOtariTwoKeysEndToEnd runs the built harness binary as `proxy` with
// the two keys on stdin — the way the "Start pricing proxy" step passes them —
// pointed at a stub Otari chat upstream and a stub search endpoint, and shows:
// a chat call reaches /chat/completions with `Authorization: Bearer otari-test-key`
// and model `synthetic.new:hf:zai-org/GLM-5.3-Flash`; a /v2/search call reaches the
// search endpoint with `Authorization: Bearer synthetic-test-key`; neither key ever
// appears on the other endpoint; and the proxy makes no /v2/quotas request, so a
// quota that would previously have sat under min_credit_percent cannot refuse the
// call (the stub serves one anyway, and it is never asked).
func TestProxyOtariTwoKeysEndToEnd(t *testing.T) {
	needContractBin(t)

	const otariKey = "otari-test-key"
	const syntheticKey = "synthetic-test-key"

	chat := &seenRequests{}
	chatStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		chat.add(r.URL.Path, r.Header.Get("Authorization"), string(body))
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			w.Header().Set("Content-Type", "application/json")
			// Otari's usage shape: the OpenAI usage block plus a decimal-string
			// cost_usd, and a model field without the synthetic.new: prefix.
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],"model":"zai-org/GLM-5.3-Flash",`+
				`"usage":{"prompt_tokens":100,"completion_tokens":60,"cost_usd":"0.000085","pricing_source":"organization"}}`)
		case strings.HasSuffix(r.URL.Path, "/v2/quotas"):
			// A quota that would previously have refused (7.5% < the old 50%
			// floor). The proxy must never ask for it.
			_, _ = io.WriteString(w, `{"weeklyTokenLimit":{"percentRemaining":7.5,"maxCredits":"$20"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer chatStub.Close()

	search := &seenRequests{}
	searchStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		search.add(r.URL.Path, r.Header.Get("Authorization"), string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"title":"t","url":"https://example.com"}]}`)
	}))
	defer searchStub.Close()

	// A free port for the proxy to listen on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	cmd := exec.Command(contractBin, "proxy")
	cmd.Dir = t.TempDir()
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"PROXY_UPSTREAM=" + chatStub.URL,
		"PROXY_SEARCH_URL=" + searchStub.URL,
		"PROXY_MODELS=" + otariModels(t),
		"PROXY_LEDGER=" + ledger,
		"PROXY_WEEK_SPENT_USD=0",
		"PROXY_PORT=" + fmt.Sprint(port),
	}
	// The two keys go in on stdin, line 1 Otari, line 2 synthetic.new —
	// exactly what the workflow's printf feeds the proxy.
	cmd.Stdin = strings.NewReader(otariKey + "\n" + syntheticKey + "\n")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-done
	})

	// Wait for /healthz, as the workflow step does.
	addr := fmt.Sprintf("http://127.0.0.1:%d", port)
	healthy := false
	for i := 0; i < 100; i++ {
		resp, err := http.Get(addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			healthy = true
			break
		}
		select {
		case err := <-done:
			t.Fatalf("proxy exited early: %v; stderr:\n%s", err, stderr.String())
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !healthy {
		t.Fatalf("proxy never became healthy; stderr:\n%s", stderr.String())
	}

	// A chat call: the catalog id resolves, upstream sees the Otari bearer
	// key and the synthetic.new: selector, and the relayed usage keeps the
	// client's own usage shape.
	resp, err := http.Post(addr+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"zai-org/glm-5.3-flash","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	chatBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat call: status %d, body:\n%s", resp.StatusCode, chatBody)
	}
	var relayed struct {
		Usage struct {
			Cost float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(chatBody, &relayed); err != nil {
		t.Fatalf("chat response %q: %v", chatBody, err)
	}
	m := otariFixtureModel()
	wantCost := CostOf(m, Usage{PromptTokens: 100, CompletionTokens: 60})
	if relayed.Usage.Cost != wantCost {
		t.Errorf("relayed usage.cost = %v, want %v (api_* list prices)", relayed.Usage.Cost, wantCost)
	}

	// A search call: the search endpoint sees the synthetic.new key.
	sresp, err := http.Post(addr+"/v2/search", "application/json", strings.NewReader(`{"query":"otari docs"}`))
	if err != nil {
		t.Fatal(err)
	}
	searchBody, _ := io.ReadAll(sresp.Body)
	sresp.Body.Close()
	if sresp.StatusCode != http.StatusOK {
		t.Fatalf("search call: status %d, body:\n%s", sresp.StatusCode, searchBody)
	}

	chatPaths, chatAuths, chatBodies := chat.snapshot()
	searchPaths, searchAuths, _ := search.snapshot()

	// The chat call reached the chat upstream at /chat/completions with the
	// Otari key and the synthetic.new: selector model.
	var sawChat bool
	for i, path := range chatPaths {
		if strings.HasSuffix(path, "/chat/completions") {
			sawChat = true
			if chatAuths[i] != "Bearer "+otariKey {
				t.Errorf("chat upstream Authorization = %q, want %q", chatAuths[i], "Bearer "+otariKey)
			}
			var body struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal([]byte(chatBodies[i]), &body); err != nil {
				t.Errorf("chat upstream body %q: %v", chatBodies[i], err)
			} else if body.Model != "synthetic.new:hf:zai-org/GLM-5.3-Flash" {
				t.Errorf("chat upstream model = %q, want %q", body.Model, "synthetic.new:hf:zai-org/GLM-5.3-Flash")
			}
		}
	}
	if !sawChat {
		t.Errorf("the chat upstream never saw a /chat/completions call; paths: %v", chatPaths)
	}
	// The search call reached the search endpoint (PROXY_SEARCH_URL is the
	// endpoint itself, so the stub sees the root path) with the
	// synthetic.new key.
	if len(searchPaths) != 1 {
		t.Errorf("search endpoint saw %d requests (%v), want exactly one", len(searchPaths), searchPaths)
	} else if searchAuths[0] != "Bearer "+syntheticKey {
		t.Errorf("search endpoint Authorization = %q, want %q", searchAuths[0], "Bearer "+syntheticKey)
	}
	// Neither key ever appears on the other endpoint, and no /v2/quotas
	// request is made (the stub serves one; it is never asked).
	for i, auth := range chatAuths {
		if strings.Contains(auth, syntheticKey) {
			t.Errorf("the synthetic.new key reached the chat upstream: %q (path %s)", auth, chatPaths[i])
		}
		if strings.HasSuffix(chatPaths[i], "/v2/quotas") {
			t.Errorf("the proxy called /v2/quotas on the chat upstream; the quota guard is gone")
		}
	}
	for i, auth := range searchAuths {
		if strings.Contains(auth, otariKey) {
			t.Errorf("the Otari key reached the search endpoint: %q (path %s)", auth, searchPaths[i])
		}
	}

	// The ledger row records Otari's cost_usd for reference only, and the
	// call's cost still comes from the api_* list prices.
	lines, err := readLedger(ledger)
	if err != nil {
		t.Fatal(err)
	}
	var call *LedgerLine
	for i := range lines {
		if lines[i].Kind == "" && lines[i].Model == "zai-org/glm-5.3-flash" {
			call = &lines[i]
		}
	}
	if call == nil {
		t.Fatalf("no call row in the ledger:\n%v", lines)
	}
	if call.OtariCostUSD != 0.000085 {
		t.Errorf("ledger otari_cost_usd = %v, want 0.000085", call.OtariCostUSD)
	}
	if call.CostUSD != wantCost {
		t.Errorf("ledger cost_usd = %v, want %v (api_* list prices)", call.CostUSD, wantCost)
	}
}

// TestProxyStartFailsWithoutOtariKey: with no OTARI_API_KEY in the environment
// and nothing on stdin, starting the proxy fails with an error naming the
// variable.
func TestProxyStartFailsWithoutOtariKey(t *testing.T) {
	needContractBin(t)
	cmd := exec.Command(contractBin, "proxy")
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")} // no keys in the environment
	cmd.Stdin = strings.NewReader("")               // and nothing on stdin
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatal("harness proxy started without an Otari key, want a startup failure")
	}
	if exit, ok := err.(*exec.ExitError); !ok || exit.Success() {
		t.Fatalf("harness proxy: %v, want a non-zero exit", err)
	}
	if want := "OTARI_API_KEY"; !strings.Contains(stderr.String(), want) {
		t.Errorf("startup error %q does not name %s", stderr.String(), want)
	}
}

// TestOtariCostUSDStreamedToLedger: a streamed response whose final usage
// chunk carries cost_usd records otari_cost_usd in the ledger row, and cost
// is still computed from the api_* list prices. A non-numeric or absurd
// cost_usd is dropped, not recorded.
func TestOtariCostUSDStreamedToLedger(t *testing.T) {
	costUSD := "0.000085"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"hi"}}],"model":"x"}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":60,"cost_usd":"`+costUSD+`"}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	p := &proxy{upstream: upstream.URL, apiKey: "otari-key", searchKey: "synthetic-key",
		ledger: ledger, runCap: 5, weekCap: 20, models: []Model{otariFixtureModel()}, client: upstream.Client()}
	server := httptest.NewServer(p)
	defer server.Close()

	post := func(model string) {
		t.Helper()
		response, err := http.Post(server.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"`+model+`","stream":true,"messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}

	wantCost := CostOf(otariFixtureModel(), Usage{PromptTokens: 100, CompletionTokens: 60})

	// A good decimal cost_usd is recorded.
	post("zai-org/glm-5.3-flash")
	// A non-numeric cost_usd is dropped.
	costUSD = `not-a-number`
	post("zai-org/glm-5.3-flash")
	// An absurd cost_usd is dropped too (above the sane bound).
	costUSD = "999999999"
	post("zai-org/glm-5.3-flash")

	lines, err := readLedger(ledger)
	if err != nil {
		t.Fatal(err)
	}
	var calls []*LedgerLine
	for i := range lines {
		if lines[i].Kind == "" && lines[i].Model == "zai-org/glm-5.3-flash" {
			calls = append(calls, &lines[i])
		}
	}
	if len(calls) != 3 {
		t.Fatalf("ledger has %d call rows, want 3:\n%v", len(calls), lines)
	}
	if calls[0].OtariCostUSD != 0.000085 {
		t.Errorf("otari_cost_usd = %v, want 0.000085", calls[0].OtariCostUSD)
	}
	for i, call := range calls[1:] {
		if call.OtariCostUSD != 0 {
			t.Errorf("call %d (bad cost_usd %q): otari_cost_usd = %v, want nothing recorded", i+2, []string{`not-a-number`, `999999999`}[i], call.OtariCostUSD)
		}
	}
	for i, call := range calls {
		if call.CostUSD != wantCost {
			t.Errorf("call %d: cost_usd = %v, want %v (api_* list prices)", i+1, call.CostUSD, wantCost)
		}
	}
}

// TestRewriteRequestOtariSelector: RewriteRequest turns a catalog id's
// upstream into Otari's "synthetic.new:" + the synthetic.new model id, the
// provider:model selector that pins the call to Otari's synthetic.new
// provider key -- for the fixture model here, and by the same code path for
// every entry in models.json, whose upstream field is the synthetic.new id.
func TestRewriteRequestOtariSelector(t *testing.T) {
	m := otariFixtureModel()
	body := map[string]json.RawMessage{"model": json.RawMessage(`"` + m.ID + `"`)}
	if _, err := RewriteRequest(body, m); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := json.Unmarshal(body["model"], &got); err != nil {
		t.Fatalf("rewritten model %s: %v", body["model"], err)
	}
	if want := otariSelector(m.Upstream); got != want {
		t.Errorf("RewriteRequest model = %q, want %q", got, want)
	}
}

// TestOtariGatewayControlsStripped: Otari's chat API acts on gateway-only
// request fields (mcp_servers, mcp_server_ids, guardrails, tools_header,
// max_tool_iterations, session_label) and on tool entries it runs
// server-side -- every type except "function", which covers the otari_*
// tools and the native aliases (code_execution, code_interpreter,
// code_execution_<date>, web_search, web_search_<date>, web_search_preview,
// web_fetch_<date>). The agent's request is untrusted, so none of them may
// reach the upstream: each is sent here and asserted absent from the body
// the stub sees, while an ordinary function tool still arrives. (Issue #2
// adds the proxy's own controlled otari_web_search entry after this
// stripping.)
func TestOtariGatewayControlsStripped(t *testing.T) {
	var mu sync.Mutex
	var upstreamBodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		upstreamBodies = append(upstreamBodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

	p := &proxy{upstream: upstream.URL, apiKey: "otari-key", searchKey: "synthetic-key",
		ledger: filepath.Join(t.TempDir(), "ledger.jsonl"), runCap: 5, weekCap: 20,
		models: []Model{otariFixtureModel()}, client: upstream.Client()}
	server := httptest.NewServer(p)
	defer server.Close()

	gatewayFields := []string{
		"mcp_servers", "mcp_server_ids", "guardrails", "tools_header",
		"max_tool_iterations", "session_label",
	}
	values := map[string]string{
		"mcp_servers":         `[{"name":"evil","url":"https://evil.example/mcp","authorization_token":"tok"}]`,
		"mcp_server_ids":      `["srv_123"]`,
		"guardrails":          `[{"name":"g","url":"https://evil.example/guard"}]`,
		"tools_header":        `{"x-evil":"1"}`,
		"max_tool_iterations": `99`,
		"session_label":       `"sneaky-session"`,
	}
	// Every tool type that is not "function": Otari's own otari_* tools and
	// the native aliases the chat API accepts (the <date> suffixes stand for
	// a dated alias such as web_search_2026_11_07).
	nonFunctionTypes := []string{
		"otari_web_search", "otari_web_fetch", "otari_code_execution",
		"code_execution", "code_interpreter", "code_execution_2026_11_07",
		"web_search", "web_search_2026_11_07", "web_search_preview",
		"web_fetch_2026_11_07", "mcp",
	}
	functionTool := `{"type":"function","function":{"name":"list_files","parameters":{"type":"object"}}}`

	post := func(body string) {
		t.Helper()
		resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	lastBody := func() string {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if len(upstreamBodies) == 0 {
			t.Fatal("the upstream stub saw no request")
		}
		return upstreamBodies[len(upstreamBodies)-1]
	}

	// Each gateway field alone, with a function tool that must survive.
	for _, field := range gatewayFields {
		post(`{"model":"zai-org/glm-5.3-flash","messages":[],"tools":[` + functionTool + `],` +
			`"` + field + `":` + values[field] + `}`)
		body := lastBody()
		if strings.Contains(body, `"`+field+`"`) {
			t.Errorf("gateway field %s reached the upstream: %s", field, body)
		}
		if !strings.Contains(body, `"list_files"`) {
			t.Errorf("the ordinary function tool was lost with %s: %s", field, body)
		}
	}

	// All gateway fields at once, plus every non-function tool type beside
	// one function tool: only the function tool may arrive.
	parts := make([]string, 0, len(gatewayFields)+len(nonFunctionTypes)+1)
	for _, field := range gatewayFields {
		parts = append(parts, `"`+field+`":`+values[field])
	}
	tools := make([]string, 0, len(nonFunctionTypes)+1)
	for _, typ := range nonFunctionTypes {
		tools = append(tools, `{"type":"`+typ+`"}`)
	}
	tools = append(tools, functionTool)
	post(`{"model":"zai-org/glm-5.3-flash","messages":[],"tools":[` + strings.Join(tools, ",") + `],` +
		strings.Join(parts, ",") + `}`)
	body := lastBody()
	for _, field := range gatewayFields {
		if strings.Contains(body, `"`+field+`"`) {
			t.Errorf("gateway field %s reached the upstream: %s", field, body)
		}
	}
	for _, typ := range nonFunctionTypes {
		if strings.Contains(body, `"`+typ+`"`) {
			t.Errorf("tool type %s reached the upstream: %s", typ, body)
		}
	}
	if !strings.Contains(body, `"list_files"`) {
		t.Errorf("the ordinary function tool was stripped too: %s", body)
	}
	var sent struct {
		Tools []struct {
			Type string `json:"type"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("upstream body %s: %v", body, err)
	}
	if len(sent.Tools) != 1 || sent.Tools[0].Type != "function" {
		t.Errorf("upstream tools = %+v, want the one function tool", sent.Tools)
	}
}

// otariSelector is the selector RewriteRequest sends upstream: the Otari
// provider key "synthetic.new" plus the synthetic.new model id.
func otariSelector(upstream string) string {
	return "synthetic.new:" + upstream
}

// TestRewriteRequestAllowlistHardening: encoding/json matches keys
// case-insensitively, so the earlier blocklist stripping was bypassable --
// {"type":"otari_web_search","Type":"function"} decoded as a plain function
// and was forwarded with its original bytes, and MCP_SERVERS slipped past an
// exact-key delete of mcp_servers. RewriteRequest now rebuilds the body from
// the exact, case-sensitive chatRequestKeys allowlist and the tools and
// tool_choice in canonical form, none of which can be bypassed that way.
// Each request below is posted through the proxy and the exact body the
// upstream stub receives is compared field for field.
func TestRewriteRequestAllowlistHardening(t *testing.T) {
	var mu sync.Mutex
	var upstreamBodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		upstreamBodies = append(upstreamBodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

	p := &proxy{upstream: upstream.URL, apiKey: "otari-key", searchKey: "synthetic-key",
		ledger: filepath.Join(t.TempDir(), "ledger.jsonl"), runCap: 5, weekCap: 20,
		models: []Model{otariFixtureModel()}, client: upstream.Client()}
	server := httptest.NewServer(p)
	defer server.Close()

	post := func(body string) map[string]any {
		t.Helper()
		resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		mu.Lock()
		raw := upstreamBodies[len(upstreamBodies)-1]
		mu.Unlock()
		var got map[string]any
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatalf("upstream body %s: %v", raw, err)
		}
		return got
	}
	want := func(expected string) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal([]byte(expected), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	sameBody := func(got map[string]any, expected string) {
		t.Helper()
		if !reflect.DeepEqual(got, want(expected)) {
			t.Errorf("upstream body\n got %s\nwant %s", got, expected)
		}
	}
	noSmuggledKeys := func(body string, keys ...string) {
		t.Helper()
		for _, key := range keys {
			if strings.Contains(body, key) {
				t.Errorf("smuggled key %q reached the upstream: %s", key, body)
			}
		}
	}

	// Every bypass shape at once: the case-variant type tool, a tool with
	// both "type":"function" and a folded "TYPE" naming a server-side tool,
	// uppercase and mixed-case top-level gateway fields, and every field the
	// allowlist does not name. Only the one ordinary function tool and the
	// allowlisted fields may arrive.
	post(`{` +
		`"model":"zai-org/glm-5.3-flash",` +
		`"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[` +
		`{"type":"otari_web_search","Type":"function"},` +
		`{"type":"function","TYPE":"otari_code_execution","function":{"name":"steal"}},` +
		`{"type":"function","function":{"name":"list_files","parameters":{"type":"object"}}}` +
		`],` +
		`"tool_choice":{"type":"function","function":{"name":"otari_web_search"}},` +
		`"MCP_SERVERS":[{"url":"https://evil.example/mcp"}],` +
		`"Mcp_Servers":[{"url":"https://evil.example/mcp"}],` +
		`"Guardrails":[{"url":"https://evil.example/guard"}],` +
		`"web_search_options":{"search_context_size":"high"},` +
		`"user":"agent-123",` +
		`"metadata":{"run":"1"},` +
		`"safety_identifier":"agent-123",` +
		`"mcp_servers":[{"url":"https://evil.example/mcp"}],` +
		`"mcp_server_ids":["srv_1"],` +
		`"guardrails":[{"url":"https://evil.example/guard"}],` +
		`"tools_header":{"x-evil":"1"},` +
		`"max_tool_iterations":99,` +
		`"session_label":"sneaky",` +
		`"provider":{"order":["x"]},` +
		`"reasoning":{"effort":"medium"}}`)
	mu.Lock()
	raw := upstreamBodies[len(upstreamBodies)-1]
	mu.Unlock()
	noSmuggledKeys(raw, "otari_web_search", "otari_code_execution", "steal", "MCP_SERVERS",
		"Mcp_Servers", "Guardrails", "web_search_options", "agent-123", "safety_identifier",
		"mcp_servers", "mcp_server_ids", "guardrails", "tools_header", "max_tool_iterations",
		"session_label", "provider")
	sameBody(post(`{`+
		`"model":"zai-org/glm-5.3-flash",`+
		`"messages":[{"role":"user","content":"hi"}],`+
		`"tools":[`+
		`{"type":"otari_web_search","Type":"function"},`+
		`{"type":"function","TYPE":"otari_code_execution","function":{"name":"steal"}},`+
		`{"type":"function","function":{"name":"list_files","parameters":{"type":"object"}}}`+
		`],`+
		`"tool_choice":{"type":"function","function":{"name":"otari_web_search"}},`+
		`"MCP_SERVERS":[{"url":"https://evil.example/mcp"}],`+
		`"Mcp_Servers":[{"url":"https://evil.example/mcp"}],`+
		`"Guardrails":[{"url":"https://evil.example/guard"}],`+
		`"web_search_options":{"search_context_size":"high"},`+
		`"user":"agent-123",`+
		`"metadata":{"run":"1"},`+
		`"safety_identifier":"agent-123",`+
		`"mcp_servers":[{"url":"https://evil.example/mcp"}],`+
		`"mcp_server_ids":["srv_1"],`+
		`"guardrails":[{"url":"https://evil.example/guard"}],`+
		`"tools_header":{"x-evil":"1"},`+
		`"max_tool_iterations":99,`+
		`"session_label":"sneaky",`+
		`"provider":{"order":["x"]},`+
		`"reasoning":{"effort":"medium"}}`),
		`{"model":"synthetic.new:hf:zai-org/GLM-5.3-Flash",`+
			`"messages":[{"role":"user","content":"hi"}],`+
			`"tools":[{"type":"function","function":{"name":"list_files","parameters":{"type":"object"}}}],`+
			`"reasoning_effort":"medium"}`)

	// tool_choice follows the tools: the strings stay while tools remain,
	// a choice naming a kept tool is rebuilt canonically, and both are
	// dropped when no tools survive (including when none were sent).
	sameBody(post(`{"model":"zai-org/glm-5.3-flash","messages":[],"tools":[{"type":"function","function":{"name":"list_files"}}],"tool_choice":"required"}`),
		`{"model":"synthetic.new:hf:zai-org/GLM-5.3-Flash","messages":[],"tools":[{"type":"function","function":{"name":"list_files"}}],"tool_choice":"required"}`)
	// A tool_choice with a folded "TYPE" twin is as untrustworthy as such a
	// tool, so it is dropped outright, like the tool would be; a clean
	// choice naming a kept tool is rebuilt canonically, its extra fields gone.
	sameBody(post(`{"model":"zai-org/glm-5.3-flash","messages":[],"tools":[{"type":"function","function":{"name":"list_files"}}],"tool_choice":{"type":"function","function":{"name":"list_files","smuggled":"x"},"TYPE":"otari_web_search"}}`),
		`{"model":"synthetic.new:hf:zai-org/GLM-5.3-Flash","messages":[],"tools":[{"type":"function","function":{"name":"list_files"}}]}`)
	sameBody(post(`{"model":"zai-org/glm-5.3-flash","messages":[],"tools":[{"type":"function","function":{"name":"list_files"}}],"tool_choice":{"type":"function","function":{"name":"list_files","smuggled":"x"}}}`),
		`{"model":"synthetic.new:hf:zai-org/GLM-5.3-Flash","messages":[],"tools":[{"type":"function","function":{"name":"list_files"}}],"tool_choice":{"type":"function","function":{"name":"list_files"}}}`)
	sameBody(post(`{"model":"zai-org/glm-5.3-flash","messages":[],"tools":[{"type":"otari_web_fetch"}],"tool_choice":"required"}`),
		`{"model":"synthetic.new:hf:zai-org/GLM-5.3-Flash","messages":[]}`)
	sameBody(post(`{"model":"zai-org/glm-5.3-flash","messages":[],"tool_choice":"required"}`),
		`{"model":"synthetic.new:hf:zai-org/GLM-5.3-Flash","messages":[]}`)

	// An ordinary CodeAF request -- messages, a function tool, stream,
	// stream_options, reasoning effort and the plain sampling fields -- must
	// reach the stub with the same meaning: every allowlisted field passes
	// through, stream_options gains include_usage, and the model gets the
	// Otari selector.
	sameBody(post(`{`+
		`"model":"zai-org/glm-5.3-flash",`+
		`"messages":[{"role":"user","content":"list the files"}],`+
		`"tools":[{"type":"function","function":{"name":"list_files","parameters":{"type":"object"}}}],`+
		`"tool_choice":"auto",`+
		`"parallel_tool_calls":true,`+
		`"stream":true,`+
		`"stream_options":{"include_usage":true},`+
		`"temperature":0.7,"top_p":0.9,"max_tokens":1024,"max_completion_tokens":2048,`+
		`"stop":["\n"],"seed":7,"n":1,`+
		`"frequency_penalty":0.1,"presence_penalty":0.2,"logprobs":true,"top_logprobs":3,`+
		`"response_format":{"type":"text"},`+
		`"reasoning":{"effort":"medium"}}`),
		`{"model":"synthetic.new:hf:zai-org/GLM-5.3-Flash",`+
			`"messages":[{"role":"user","content":"list the files"}],`+
			`"tools":[{"type":"function","function":{"name":"list_files","parameters":{"type":"object"}}}],`+
			`"tool_choice":"auto",`+
			`"parallel_tool_calls":true,`+
			`"stream":true,`+
			`"stream_options":{"include_usage":true},`+
			`"temperature":0.7,"top_p":0.9,"max_tokens":1024,"max_completion_tokens":2048,`+
			`"stop":["\n"],"seed":7,"n":1,`+
			`"frequency_penalty":0.1,"presence_penalty":0.2,"logprobs":true,"top_logprobs":3,`+
			`"response_format":{"type":"text"},`+
			`"reasoning_effort":"medium"}`)
}
