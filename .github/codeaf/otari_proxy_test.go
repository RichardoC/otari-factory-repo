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
	"strings"
	"sync"
	"testing"
	"time"
)

// otariModels writes a minimal models.json naming the GLM-5.3-Flash model the
// e2e test calls, and returns its path. Same shape writePlannerModels writes.
func otariModels(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	body := `{"limits":{"plan":"big","weekly_credit_usd":20,"run_credit_usd":5},
		"models":[{"id":"zai-org/glm-5.3-flash","upstream":"hf:zai-org/GLM-5.3-Flash",
		"name":"GLM-5.3-Flash (synthetic.new via Otari)","role":"base","max_concurrent":2,
		"input":0.1,"output":0.25,"cache_read":0.02,"api_input":0.15,"api_cached":0.04,
		"api_output":0.5,"context_length":524288,"input_modalities":["text","image"],
		"efforts":["medium","high","max"]}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
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
	m := mustResolve(t, testModels(t), "zai-org/glm-5.3-flash")
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
		ledger: ledger, runCap: 5, weekCap: 20, models: testModels(t), client: upstream.Client()}
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

	m := mustResolve(t, testModels(t), "zai-org/glm-5.3-flash")
	wantCost := CostOf(m, Usage{PromptTokens: 100, CompletionTokens: 60})

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

// TestModelsIDRoleSetAgainstDefaultBranch: the model ids, roles and
// max_concurrent values are unchanged from the default branch's models.json,
// and every upstream still resolves to the Otari selector
// "synthetic.new:" + the synthetic.new model id. Skipped when no default
// branch ref is available (a shallow checkout without one).
func TestModelsIDRoleSetAgainstDefaultBranch(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the default branch's models.json")
	}
	// The module lives at .github/codeaf; the repository root is two up.
	repo := filepath.Join("..", "..")
	var base []byte
	for _, ref := range []string{"origin/main", "main"} {
		out, err := exec.Command("git", "-C", repo, "show", ref+":.github/codeaf/models.json").Output()
		if err == nil {
			base = out
			break
		}
	}
	if base == nil {
		t.Skip("no default-branch models.json available")
	}
	type row struct {
		Role          string `json:"role"`
		MaxConcurrent int    `json:"max_concurrent"`
		Upstream      string `json:"upstream"`
	}
	var baseRaw struct {
		Models []struct {
			ID string `json:"id"`
			row
		} `json:"models"`
	}
	if err := json.Unmarshal(base, &baseRaw); err != nil {
		t.Fatalf("default branch models.json: %v", err)
	}
	baseModels := map[string]row{}
	for _, mm := range baseRaw.Models {
		baseModels[mm.ID] = mm.row
	}
	var nowRaw struct {
		Models []struct {
			ID string `json:"id"`
			row
		} `json:"models"`
	}
	data, err := os.ReadFile("models.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &nowRaw); err != nil {
		t.Fatalf("models.json: %v", err)
	}
	if len(nowRaw.Models) != len(baseModels) {
		t.Errorf("models.json lists %d models, the default branch %d", len(nowRaw.Models), len(baseModels))
	}
	for _, mm := range nowRaw.Models {
		want, ok := baseModels[mm.ID]
		if !ok {
			t.Errorf("model %q is not on the default branch", mm.ID)
			continue
		}
		if mm.Role != want.Role {
			t.Errorf("model %q role = %q, the default branch has %q", mm.ID, mm.Role, want.Role)
		}
		if mm.MaxConcurrent != want.MaxConcurrent {
			t.Errorf("model %q max_concurrent = %d, the default branch has %d", mm.ID, mm.MaxConcurrent, want.MaxConcurrent)
		}
		if mm.Upstream != want.Upstream {
			t.Errorf("model %q upstream = %q, the default branch has %q", mm.ID, mm.Upstream, want.Upstream)
		}
		// Every upstream resolves to Otari's synthetic.new: selector.
		if !strings.HasPrefix(mm.Upstream, "synthetic.new:") &&
			otariSelector(mm.Upstream) != "synthetic.new:"+mm.Upstream {
			t.Errorf("model %q: upstream %q does not map to synthetic.new:%s", mm.ID, mm.Upstream, mm.Upstream)
		}
	}
}

// otariSelector is the selector RewriteRequest sends upstream: the Otari
// provider key "synthetic.new" plus the synthetic.new model id.
func otariSelector(upstream string) string {
	return "synthetic.new:" + upstream
}
