package main

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Issue #410: the proxy copied upstream's usage block into the ledger line and
// priced it with no bound at all. ApiCostOf has no floor, so one
// usage{prompt_tokens:-100000000} priced at -60.0000 USD, p.runSpent dropped
// far below zero and every later overCap check passed: the run's credit cap
// stopped binding and the negative cost reached the report, the run record and
// the model stats. The usage block now goes through boundedUsage, which prices
// nothing it cannot trust and records the call like the existing "no usage"
// path.

// badUsageBlocks are the upstream usage blocks the bound must refuse: any
// negative figure, or a token count past maxUsageTokens.
var badUsageBlocks = []struct {
	name  string
	usage string
}{
	{"negative prompt tokens", `{"prompt_tokens":-100000000,"completion_tokens":200,"total_tokens":201}`},
	{"negative completion tokens", `{"prompt_tokens":1000,"completion_tokens":-5,"total_tokens":995}`},
	{"negative total tokens", `{"prompt_tokens":1000,"completion_tokens":200,"total_tokens":-1}`},
	{"negative cached tokens", `{"prompt_tokens":1000,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":-400}}`},
	{"negative reasoning tokens", `{"prompt_tokens":1000,"completion_tokens":200,"completion_tokens_details":{"reasoning_tokens":-7}}`},
	{"absurd prompt tokens", `{"prompt_tokens":10000001,"completion_tokens":200,"total_tokens":10000201}`},
	{"absurd completion tokens", `{"prompt_tokens":1000,"completion_tokens":99999999999,"total_tokens":10000000999}`},
}

// usageOf goes back out of a relayed payload or chunk as the caller would
// read it.
func usageOf(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var usage map[string]any
	if err := json.Unmarshal(raw, &usage); err != nil {
		t.Fatalf("relayed usage %s does not decode: %v", raw, err)
	}
	return usage
}

// TestRelayJSONBoundsUsage: a 200 whose usage block cannot be priced is
// recorded with no usage and no cost, runSpent does not move, and the caller
// still gets its 200 with upstream's block relayed unchanged (no negative cost
// written into it).
func TestRelayJSONBoundsUsage(t *testing.T) {
	m := mustResolve(t, testModels(t), "zai-org/glm-5.3")
	for _, tc := range badUsageBlocks {
		t.Run(tc.name, func(t *testing.T) {
			p := &proxy{ledger: filepath.Join(t.TempDir(), "ledger.jsonl"), runCap: 5, weekCap: 20}
			body := `{"choices":[{"message":{"content":"hi"}}],"model":"x","usage":` + tc.usage + `}`
			w := httptest.NewRecorder()
			p.relayJSON(w, strings.NewReader(body), m, time.Now(), 0, 0, 0, 0)
			if w.Code != http.StatusOK {
				t.Fatalf("caller saw %d %s, want the unchanged 200", w.Code, w.Body.String())
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
				t.Fatalf("relayed body %s does not decode: %v", w.Body.String(), err)
			}
			relayed := usageOf(t, payload["usage"])
			if _, priced := relayed["cost"]; priced {
				t.Errorf("relayed usage gained a cost: %s", payload["usage"])
			}
			var sent map[string]any
			if err := json.Unmarshal([]byte(tc.usage), &sent); err != nil {
				t.Fatal(err)
			}
			for field, want := range sent {
				if got := relayed[field]; !reflect.DeepEqual(got, want) {
					t.Errorf("relayed usage %s = %v, want upstream's %v (the block must pass through untouched)", field, got, want)
				}
			}
			lines, err := readLedger(p.ledger)
			if err != nil {
				t.Fatal(err)
			}
			if len(lines) != 1 {
				t.Fatalf("ledger holds %d lines, want exactly 1: %+v", len(lines), lines)
			}
			line := lines[0]
			if line.UsageSeen || line.CostUSD != 0 || line.APICostUSD != 0 || line.PromptTokens != 0 || line.CompletionTokens != 0 {
				t.Fatalf("line = %+v, want no usage and no cost for an unusable block", line)
			}
			p.mu.Lock()
			spent := p.runSpent
			p.mu.Unlock()
			if spent != 0 {
				t.Fatalf("runSpent = %v after an unusable block, want it unchanged at 0", spent)
			}
		})
	}
}

// TestRelayJSONPricesValidUsageExactlyAsBefore: the bound must not disturb a
// healthy call -- its usage, its cached split and its cost are recorded as
// today.
func TestRelayJSONPricesValidUsageExactlyAsBefore(t *testing.T) {
	m := mustResolve(t, testModels(t), "zai-org/glm-5.3")
	const usage = `{"prompt_tokens":1000,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":400}}`
	p := &proxy{ledger: filepath.Join(t.TempDir(), "ledger.jsonl"), runCap: 5, weekCap: 20}
	w := httptest.NewRecorder()
	p.relayJSON(w, strings.NewReader(`{"choices":[{"message":{"content":"hi"}}],"model":"x","usage":`+usage+`}`), m, time.Now(), 0, 0, 0, 0)
	if w.Code != http.StatusOK {
		t.Fatalf("caller saw %d %s", w.Code, w.Body.String())
	}
	// TestCostOf's figure for the same tokens on glm-5.3.
	wantCost := 0.001824
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if got, ok := usageOf(t, payload["usage"])["cost"].(float64); !ok || math.Abs(got-wantCost) > 1e-12 {
		t.Errorf("relayed usage cost = %v, want %v added as today", usageOf(t, payload["usage"])["cost"], wantCost)
	}
	lines, err := readLedger(p.ledger)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || !lines[0].UsageSeen || lines[0].PromptTokens != 1000 || lines[0].CachedTokens != 400 ||
		lines[0].CompletionTokens != 200 || math.Abs(lines[0].CostUSD-wantCost) > 1e-12 {
		t.Fatalf("ledger = %+v, want the valid usage and its cost recorded exactly as today", lines)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if math.Abs(p.runSpent-wantCost) > 1e-12 {
		t.Fatalf("runSpent = %v, want %v", p.runSpent, wantCost)
	}
}

// TestRewriteChunkBoundsUsage: the streaming path goes through the same bound,
// so a garbled usage chunk is relayed unchanged and prices nothing.
func TestRewriteChunkBoundsUsage(t *testing.T) {
	m := mustResolve(t, testModels(t), "zai-org/glm-5.3")
	for _, tc := range badUsageBlocks {
		t.Run(tc.name, func(t *testing.T) {
			chunk := []byte(`{"choices":[],"usage":` + tc.usage + `}`)
			out, usage, dropped, _, _ := RewriteChunk(chunk, m, &charCounts{})
			if usage != nil || !dropped {
				t.Fatalf("RewriteChunk returned usage %+v, dropped %v; want no usage and dropped for an unusable block", usage, dropped)
			}
			var relayed map[string]json.RawMessage
			if err := json.Unmarshal(out, &relayed); err != nil {
				t.Fatalf("relayed chunk %s does not decode: %v", out, err)
			}
			if _, priced := usageOf(t, relayed["usage"])["cost"]; priced {
				t.Errorf("relayed chunk gained a cost: %s", out)
			}
			if !strings.Contains(string(out), strings.TrimSuffix(strings.TrimPrefix(tc.usage, "{"), "}")) {
				t.Errorf("relayed chunk %s does not carry upstream's usage block %s unchanged", out, tc.usage)
			}
		})
	}
	// A valid chunk is still priced as today.
	out, usage, dropped, _, _ := RewriteChunk([]byte(`{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":400}}}`), m, &charCounts{})
	if usage == nil || dropped || math.Abs(usage.Cost-0.001824) > 1e-12 {
		t.Fatalf("valid chunk usage = %+v, want the priced block", usage)
	}
	if !strings.Contains(string(out), `"cost":0.001824`) {
		t.Errorf("valid chunk relayed as %s, want the cost added as today", out)
	}
}

// TestProxyGarbledUsageCannotLiftTheRunCap is the issue's reproduction, end to
// end: a 200 carrying usage{prompt_tokens:-100000000} used to price at
// -60.0000 USD, dropping runSpent below zero so that the $0.0005 run credit
// cap never refused again. Now the garbage call is recorded unpriced, runSpent
// stays 0, and the next honest call that crosses the cap still gets 402.
func TestProxyGarbledUsageCannotLiftTheRunCap(t *testing.T) {
	const badUsage = `{"prompt_tokens":-100000000,"completion_tokens":200,"total_tokens":-99999800}`
	const goodUsage = `{"prompt_tokens":1000,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":400}}`
	// TestCostOf prices goodUsage on glm-5.3 at $0.001824, past the cap.
	const wantGoodCost = 0.001824

	var mu sync.Mutex
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := calls
		calls++
		mu.Unlock()
		usage := badUsage
		if n > 0 {
			usage = goodUsage
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],"model":"x","usage":`+usage+`}`)
	}))
	defer upstream.Close()

	p := proxyFor145(t, "zai-org/glm-5.3", 20*time.Millisecond, upstream)
	p.runCap = 0.0005
	p.ledger = filepath.Join(t.TempDir(), "ledger.jsonl")
	server := httptest.NewServer(p)
	defer server.Close()

	post := func() (int, string) {
		t.Helper()
		response, err := http.Post(server.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"zai-org/glm-5.3","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(body)
	}

	// Call 1: a 200 with the hostile usage block. The caller's answer is
	// unchanged -- 200, upstream's block relayed as sent -- and the ledger
	// carries no cost for it.
	status, body := post()
	if status != http.StatusOK || !strings.Contains(body, `"prompt_tokens":-100000000`) || strings.Contains(body, `"cost"`) {
		t.Fatalf("garbled-usage call: %d %s, want the unchanged 200 with upstream's unmatched usage block", status, body)
	}
	lines, err := readLedger(p.ledger)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("ledger holds %d lines after the garbled call, want 1: %+v", len(lines), lines)
	}
	if line := lines[0]; line.UsageSeen || line.CostUSD != 0 || line.APICostUSD != 0 {
		t.Fatalf("garbled-usage line = %+v, want cost 0 and no usage -- not -60.0000 USD", line)
	}
	p.mu.Lock()
	spent := p.runSpent
	p.mu.Unlock()
	if spent != 0 {
		t.Fatalf("runSpent = %v after the garbled call, want it unchanged at 0", spent)
	}

	// Call 2: a valid call crossing the cap is still recorded in full.
	if status, body = post(); status != http.StatusOK || !strings.Contains(body, `"cost":0.001824`) {
		t.Fatalf("valid call: %d %s", status, body)
	}
	p.mu.Lock()
	spent = p.runSpent
	p.mu.Unlock()
	if math.Abs(spent-wantGoodCost) > 1e-12 {
		t.Fatalf("runSpent = %v after the valid call, want %v", spent, wantGoodCost)
	}

	// Call 3: past the $0.0005 cap, so the cap binds and the call is refused.
	if status, body = post(); status != http.StatusPaymentRequired {
		t.Fatalf("over cap: %d %s, want 402", status, body)
	}
}
