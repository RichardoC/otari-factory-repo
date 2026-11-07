package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// loadConfig validates the Big-plan limits: the plan must be named, the two
// credit caps must be positive, and the overage cap must not be negative (0
// is the default: stop rather than pay past the weekly credit).
func TestLoadConfigLimits(t *testing.T) {
	dir := t.TempDir()
	write := func(t *testing.T, limits string) string {
		t.Helper()
		path := filepath.Join(dir, "models.json")
		body := fmt.Sprintf(`{"limits":%s,"models":[{"id":"m","upstream":"hf:m","efforts":["high"],
			"input":0.1,"output":0.25,"cache_read":0.02,"api_input":0.15,"api_cached":0.04,"api_output":0.5}]}`, limits)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, tc := range []struct {
		name   string
		limits string
		want   string
	}{
		{"missing plan", `{"weekly_credit_usd":20,"run_credit_usd":5,"overage_usd":0}`, "limits.plan"},
		{"zero run credit", `{"plan":"big","weekly_credit_usd":20,"run_credit_usd":0,"overage_usd":0}`, "run_credit_usd"},
		{"zero weekly credit", `{"plan":"big","weekly_credit_usd":0,"run_credit_usd":5,"overage_usd":0}`, "weekly_credit_usd"},
		{"negative overage", `{"plan":"big","weekly_credit_usd":20,"run_credit_usd":5,"overage_usd":-1}`, "overage_usd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(write(t, tc.limits))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("loadConfig = %v, want an error naming %s", err, tc.want)
			}
		})
	}
	cfg, err := loadConfig(write(t, `{"plan":"big","weekly_credit_usd":20,"run_credit_usd":5,"overage_usd":0}`))
	if err != nil {
		t.Fatalf("valid limits rejected: %v", err)
	}
	if cfg.Limits.Plan != "big" || cfg.Limits.WeeklyCreditUSD != 20 || cfg.Limits.RunCreditUSD != 5 || cfg.Limits.OverageUSD != 0 {
		t.Fatalf("limits = %+v", cfg.Limits)
	}
	// Overage 0 is valid, and is the default when the field is missing.
	cfg, err = loadConfig(write(t, `{"plan":"big","weekly_credit_usd":20,"run_credit_usd":5}`))
	if err != nil || cfg.Limits.OverageUSD != 0 {
		t.Fatalf("overage default = %+v, %v", cfg.Limits, err)
	}
}

// overCap enforces, in order: the per-run credit cap, then the weekly credit
// cap — refused outright when overage is 0, allowed past it only up to the
// API-price overage cap.
func TestOverCapWeeklyAndOverage(t *testing.T) {
	newProxy := func() *proxy {
		// The run cap stays out of the way ($50): these cases test the weekly
		// credit and the overage caps; the run cap has its own case below.
		return &proxy{runCap: 50, weekCap: 20, weekSpent: 10}
	}
	// Under both caps: allowed.
	if got := newProxy().overCap(); got != "" {
		t.Errorf("under the caps: %q", got)
	}
	// Per-run credit cap.
	p := newProxy()
	p.runSpent = 50
	if got := p.overCap(); !strings.Contains(got, "per-run credit cap $50.00") {
		t.Errorf("run cap: %q", got)
	}
	// Weekly credit cap, overage disabled (the default): refused outright,
	// and the refusal names the cap and the week's spend.
	p = newProxy()
	p.runSpent = 10
	if got := p.overCap(); !strings.Contains(got, "weekly credit cap $20.00") || !strings.Contains(got, "overage disabled, refusing") {
		t.Errorf("weekly cap with overage 0: %q", got)
	}
	// Weekly credit cap with overage allowed: the call goes through, the
	// proxy marks itself as overage, and only an overage already at the cap
	// refuses (the whole call counts, so the check runs before the call).
	p = newProxy()
	p.overageCap = 0.001
	p.runSpent = 10
	if got := p.overCap(); got != "" {
		t.Errorf("overage allowed: %q", got)
	}
	if !p.overage {
		t.Error("overage not marked after the weekly credit was exhausted")
	}
	// No API-priced spend past the credit yet: still allowed.
	if got := p.overCap(); got != "" {
		t.Errorf("overage refused at $0 spent: %q", got)
	}
	p.overageApi = 0.001
	if got := p.overCap(); !strings.Contains(got, "API overage cap $0.00") || !strings.Contains(got, "$0.0010 of API-priced usage") {
		t.Errorf("overage cap: %q", got)
	}
}

// record prices every call twice — credit for the ledger, API price for the
// overage account — and counts a call against the overage cap only once the
// weekly credit is exhausted. Old ledger lines without api_cost_usd parse
// back as 0.
func TestRecordOverageAccounting(t *testing.T) {
	m := mustResolve(t, testModels(t), "zai-org/glm-5.3-flash")
	usage := Usage{PromptTokens: 1000, CompletionTokens: 200}
	usage.PromptTokensDetails = &struct {
		CachedTokens int `json:"cached_tokens"`
	}{CachedTokens: 400}
	// The relay sets usage.Cost before record runs; record reads it for the
	// credit and prices the API cost itself.
	usage.Cost = CostOf(m, usage) // $0.000206 at the API ("overage") rates
	// The weekly credit runs out during the second call: the third is the
	// first recorded as overage.
	p := &proxy{ledger: filepath.Join(t.TempDir(), "l.jsonl"), runCap: 5, weekCap: 0.0003, overageCap: 0.0002}
	p.record(m, &usage, false, false, time.Now(), 0, 0, charCounts{}, 0, 0, callTiming{}, 0)
	if p.overage || p.overageApi != 0 {
		t.Fatalf("first call counted as overage: overage=%v api=$%v", p.overage, p.overageApi)
	}
	p.record(m, &usage, false, false, time.Now(), 0, 0, charCounts{}, 0, 0, callTiming{}, 0)
	if !p.overage || p.overageApi != 0 {
		t.Fatalf("crossing call: overage=%v api=$%v, want the credit to run out with no overage yet", p.overage, p.overageApi)
	}
	p.record(m, &usage, false, false, time.Now(), 0, 0, charCounts{}, 0, 0, callTiming{}, 0)
	if p.overageApi != 0.000206 {
		t.Fatalf("overageApi = $%v, want $0.000206", p.overageApi)
	}
	// The cap is checked before the next call: $0.000206 spent past the
	// credit >= the $0.0002 overage cap.
	if got := p.overCap(); !strings.Contains(got, "API overage cap") {
		t.Fatalf("overCap = %q, want the API overage refusal", got)
	}
	lines, err := readLedger(p.ledger)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("%d ledger lines", len(lines))
	}
	for i, line := range lines {
		if line.CostUSD != 0.000206 || line.APICostUSD != 0.000206 {
			t.Errorf("line %d: credit $%v api $%v", i, line.CostUSD, line.APICostUSD)
		}
	}
	// An old line, written before the field existed, parses back as 0.
	var old LedgerLine
	if err := json.Unmarshal([]byte(`{"ts":"2026-10-01T00:00:00Z","model":"m","cost_usd":0.5,"stream":false,"usage_seen":true}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.CostUSD != 0.5 || old.APICostUSD != 0 {
		t.Fatalf("old line = %+v", old)
	}
}

// healthz keeps run_spent_usd (credit now) and reports the week's credit and
// the API-priced overage beside it.
func TestHealthzWeeklyAndOverage(t *testing.T) {
	p := &proxy{weekSpent: 1, overageCap: 0.001}
	p.runSpent = 2
	p.overageApi = 0.5
	server := httptest.NewServer(p)
	defer server.Close()
	response, err := http.Get(server.URL + "/v1/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var health struct {
		OK              bool     `json:"ok"`
		RunSpentUSD     *float64 `json:"run_spent_usd"`
		WeekSpentCredit *float64 `json:"week_spent_credit_usd"`
		OverageAPIUSD   *float64 `json:"overage_api_usd"`
	}
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if !health.OK || health.RunSpentUSD == nil || *health.RunSpentUSD != 2 ||
		health.WeekSpentCredit == nil || *health.WeekSpentCredit != 3 ||
		health.OverageAPIUSD == nil || *health.OverageAPIUSD != 0.5 {
		t.Fatalf("healthz = %+v", health)
	}
}

// writeQueueModels writes a models.json with one model of the given
// max_concurrent and an optional queue object; an empty queue omits the key.
func writeQueueModels(t *testing.T, queue string, maxConcurrent int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	q := ""
	if queue != "" {
		q = fmt.Sprintf(`,"queue":%s`, queue)
	}
	body := fmt.Sprintf(`{"limits":{"plan":"big","weekly_credit_usd":20,"run_credit_usd":5}%s,
		"models":[{"id":"m","upstream":"hf:m","efforts":["high"],"max_concurrent":%d,
		"input":0.1,"output":0.25,"cache_read":0.02,"api_input":0.15,"api_cached":0.04,"api_output":0.5}]}`,
		q, maxConcurrent)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadConfigQueue covers queue.max_parallel_runs: absent or 0 defaults
// to 2, a negative value is refused, and a value whose per-run slots would
// sum past a model's account-wide max_concurrent is refused.
func TestLoadConfigQueue(t *testing.T) {
	for _, queue := range []string{"", `{"max_parallel_runs":2}`, `{"max_parallel_runs":0}`} {
		cfg, err := loadConfig(writeQueueModels(t, queue, 2))
		if err != nil || cfg.Queue.MaxParallelRuns != 2 {
			t.Fatalf("queue %q: max_parallel_runs = %d, %v; want 2", queue, cfg.Queue.MaxParallelRuns, err)
		}
	}
	// An explicit value is kept, and exactly at the model's limit fits.
	cfg, err := loadConfig(writeQueueModels(t, `{"max_parallel_runs":2}`, 2))
	if err != nil || cfg.Queue.MaxParallelRuns != 2 {
		t.Fatalf("queue 2: %d, %v", cfg.Queue.MaxParallelRuns, err)
	}
	// Negative is refused.
	if _, err := loadConfig(writeQueueModels(t, `{"max_parallel_runs":-1}`, 2)); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("negative queue: %v", err)
	}
	// Past a model's max_concurrent the per-run slots would sum past the
	// account limit, and the config is refused.
	if _, err := loadConfig(writeQueueModels(t, `{"max_parallel_runs":3}`, 2)); err == nil || !strings.Contains(err.Error(), "sum past the account limit") {
		t.Fatalf("overflowing queue: %v", err)
	}
	// The committed models.json parses and is accepted as committed.
	if _, err := loadConfig("models.json"); err != nil {
		t.Fatalf("committed models.json: %v", err)
	}
}

// TestPerRunSlotsSweep sweeps max_concurrent 1..4 against max_parallel_runs
// 1..4: the per-run slots always number at least 1, and whenever they would
// sum past the model's account-wide max_concurrent, loadConfig refuses the
// config instead.
func TestPerRunSlotsSweep(t *testing.T) {
	for mc := 1; mc <= 4; mc++ {
		for mpr := 1; mpr <= 4; mpr++ {
			slots := perRunSlots(Model{ID: "m", MaxConcurrent: mc}, mpr)
			if slots < 1 {
				t.Errorf("perRunSlots(max_concurrent %d, max_parallel_runs %d) = %d; want at least 1", mc, mpr, slots)
				continue
			}
			_, err := loadConfig(writeQueueModels(t, fmt.Sprintf(`{"max_parallel_runs":%d}`, mpr), mc))
			if sum := slots * mpr; sum <= mc {
				if err != nil {
					t.Errorf("max_concurrent %d, max_parallel_runs %d: %d slots x %d = %d fits, but loadConfig refused: %v", mc, mpr, slots, mpr, sum, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "sum past the account limit") {
				t.Errorf("max_concurrent %d, max_parallel_runs %d: %d slots x %d = %d > %d, but loadConfig accepted", mc, mpr, slots, mpr, sum, mc)
			}
		}
	}
}

// writeAlertModels writes a models.json whose alerts block is alerts ("" for
// absent), everything else minimal and valid, and returns its path. It is
// writePlannerModels's shape: the block under test is the only variable.
func writeAlertModels(t *testing.T, alerts string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	a := ""
	if alerts != "" {
		a = fmt.Sprintf(`,"alerts":%s`, alerts)
	}
	body := fmt.Sprintf(`{"limits":{"plan":"big","weekly_credit_usd":20,"run_credit_usd":5}%s,
		"models":[{"id":"m","upstream":"hf:m","efforts":["high"],"max_concurrent":2,
		"input":0.1,"output":0.25,"cache_read":0.02,"api_input":0.15,"api_cached":0.04,"api_output":0.5}]}`, a)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAlertsConfig covers the alerts block (issue #131): the committed
// models.json is the source of truth for every threshold, a field the file
// does not carry takes the issue #131 default so an old models.json still
// loads, and a bad value -- negative, zero or absurd -- is refused with a
// message naming the field, the same handling limits, queue and planner get.
func TestAlertsConfig(t *testing.T) {
	// (a) The committed models.json loads, and yields every issue #131
	// threshold. The struct defaults are deliberately NOT what this asserts:
	// these numbers must be written in models.json, so a threshold moves by
	// editing the reviewed config file, not by rebuilding the harness.
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatalf("committed models.json: %v", err)
	}
	// The counts, the baselines and the window are pointers and the rest are
	// values, so the table reads each through derefNum: a nil where the file
	// must have written a number is a failure in itself (-1 is not a legal
	// threshold, so it cannot pass for one).
	derefNum := func(p *int) float64 {
		if p == nil {
			return -1
		}
		return float64(*p)
	}
	committed := []struct {
		field string
		got   float64
		want  float64
	}{
		{"hour_calls", derefNum(cfg.Alerts.HourCalls), 30},
		{"ttft_p50_ratio", cfg.Alerts.TTFTp50Ratio, 2.0},
		{"ttft_p50_ratio_baseline_hours", derefNum(cfg.Alerts.TTFTp50RatioBaselineHours), 168},
		{"ttft_p90_ms", float64(cfg.Alerts.TTFTp90Ms), 15000},
		{"decode_ratio", cfg.Alerts.DecodeRatio, 0.7},
		{"decode_min_tokens", derefNum(cfg.Alerts.DecodeMinTokens), 300},
		{"stall_gap_ms", float64(cfg.Alerts.StallGapMs), 30000},
		{"stall_pct", cfg.Alerts.StallPct, 5.0},
		{"rate_429_pct", cfg.Alerts.Rate429Pct, 1.0},
		{"reject_pct", cfg.Alerts.RejectPct, 1.0},
		{"slot_waits_per_run", derefNum(cfg.Alerts.SlotWaitsPerRun), 20},
		{"window_hours", derefNum(cfg.Alerts.WindowHours), 72},
		{"decode_baseline_hours", derefNum(cfg.Alerts.HourCallsBaselineHours), 168},
	}
	for _, c := range committed {
		if c.got != c.want {
			t.Errorf("committed models.json alerts.%s = %v, want %v", c.field, c.got, c.want)
		}
	}
	// The block is not a copy of the defaults: every field is written in the
	// file, so main.go cannot be the only place a threshold lives.
	raw, err := os.ReadFile("models.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Alerts map[string]any `json:"alerts"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("models.json: %v", err)
	}
	for _, p := range append([]string{"_comment"}, func() []string {
		out := make([]string, 0, len(committed))
		for _, c := range committed {
			out = append(out, c.field)
		}
		return out
	}()...) {
		if _, ok := file.Alerts[p]; !ok {
			t.Errorf("models.json alerts lacks %q: the threshold must live in the config, not only in Go", p)
		}
	}

	// An absent block, an empty one, and a block whose fields are all
	// legitimately zero (the float fields and reject_pct may be written as 0)
	// take the issue #131 defaults, field by field. The count-shaped fields
	// are deliberately NOT in this block: a written 0 there is refused, which
	// the (b) table below pins.
	defaults := Alerts{
		HourCalls: alertInt(30), TTFTp50Ratio: 2.0, TTFTp90Ms: 15000, DecodeRatio: 0.7,
		DecodeMinTokens: alertInt(300), StallGapMs: 30000, Rate429Pct: 1.0, RejectPct: 1.0,
		StallPct: 5.0, SlotWaitsPerRun: alertInt(20), WindowHours: alertInt(72),
		TTFTp50RatioBaselineHours: alertInt(168), HourCallsBaselineHours: alertInt(168),
	}
	// alertsEqual compares the pointer fields by value, so a default that
	// arrived as a different pointer is still the same threshold.
	alertsEqual := func(a, b Alerts) bool {
		v := func(p *int) int {
			if p == nil {
				return -1
			}
			return *p
		}
		return v(a.HourCalls) == v(b.HourCalls) && v(a.DecodeMinTokens) == v(b.DecodeMinTokens) &&
			v(a.SlotWaitsPerRun) == v(b.SlotWaitsPerRun) && v(a.WindowHours) == v(b.WindowHours) &&
			v(a.TTFTp50RatioBaselineHours) == v(b.TTFTp50RatioBaselineHours) &&
			v(a.HourCallsBaselineHours) == v(b.HourCallsBaselineHours) &&
			a.TTFTp50Ratio == b.TTFTp50Ratio && a.TTFTp90Ms == b.TTFTp90Ms &&
			a.DecodeRatio == b.DecodeRatio && a.StallGapMs == b.StallGapMs &&
			a.Rate429Pct == b.Rate429Pct && a.RejectPct == b.RejectPct && a.StallPct == b.StallPct
	}
	for _, block := range []string{"", `{}`, `{"ttft_p50_ratio":0,"ttft_p90_ms":0,"decode_ratio":0,
		"stall_gap_ms":0,"rate_429_pct":0,"reject_pct":0,"stall_pct":0}`} {
		cfg, err := loadConfig(writeAlertModels(t, block))
		if err != nil {
			t.Fatalf("alerts %q: %v", block, err)
		}
		if got := cfg.Alerts; !alertsEqual(got, defaults) {
			t.Errorf("alerts %q: got %+v; want the issue #131 defaults %+v", block, got, defaults)
		}
	}
	// An old models.json is the real case: a file written before the alerts
	// block existed has no block at all, and every count-shaped field comes
	// back defaulted rather than nil.
	old, err := loadConfig(writeAlertModels(t, ""))
	if err != nil {
		t.Fatalf("old models.json: %v", err)
	}
	for _, c := range []struct {
		name string
		got  *int
		want int
	}{
		{"hour_calls", old.Alerts.HourCalls, 30},
		{"decode_min_tokens", old.Alerts.DecodeMinTokens, 300},
		{"slot_waits_per_run", old.Alerts.SlotWaitsPerRun, 20},
		{"window_hours", old.Alerts.WindowHours, 72},
		{"ttft_p50_ratio_baseline_hours", old.Alerts.TTFTp50RatioBaselineHours, 168},
		{"decode_baseline_hours", old.Alerts.HourCallsBaselineHours, 168},
	} {
		if c.got == nil || *c.got != c.want {
			t.Errorf("a models.json without the alerts block left %s = %v, want the issue #131 default %d", c.name, c.got, c.want)
		}
	}

	// An explicit value is kept, and a partially set block defaults only
	// what it does not carry.
	cfg, err = loadConfig(writeAlertModels(t, `{"hour_calls":5,"ttft_p50_ratio":3.5,"window_hours":24,
		"slot_waits_per_run":2,"stall_gap_ms":45000,"decode_baseline_hours":48}`))
	if err != nil {
		t.Fatalf("explicit alerts: %v", err)
	}
	wantExplicit := defaults
	wantExplicit.HourCalls, wantExplicit.TTFTp50Ratio = alertInt(5), 3.5
	wantExplicit.WindowHours, wantExplicit.SlotWaitsPerRun = alertInt(24), alertInt(2)
	wantExplicit.StallGapMs, wantExplicit.HourCallsBaselineHours = 45000, alertInt(48)
	if !alertsEqual(cfg.Alerts, wantExplicit) {
		t.Fatalf("explicit alerts: got %+v; want the written values and the defaults for the rest %+v", cfg.Alerts, wantExplicit)
	}

	// (b) A bad value is an error naming the field. The two the issue names
	// come first: a negative stall_gap_ms, and a zero hour_calls -- which is
	// refused, not defaulted, because the file wrote it down (encoding/json
	// cannot tell an absent count from a written 0, so these fields are
	// pointers: nil is absent). Each zero below is that same case for the
	// field.
	for _, bad := range []struct {
		name  string
		block string
		field string
	}{
		{"negative stall gap", `{"stall_gap_ms":-1}`, "alerts.stall_gap_ms"},
		{"zero hour calls", `{"hour_calls":0}`, "alerts.hour_calls"},
		{"zero decode min tokens", `{"decode_min_tokens":0}`, "alerts.decode_min_tokens"},
		{"zero slot waits", `{"slot_waits_per_run":0}`, "alerts.slot_waits_per_run"},
		{"zero window", `{"window_hours":0}`, "alerts.window_hours"},
		{"zero ratio baseline", `{"ttft_p50_ratio_baseline_hours":0}`, "alerts.ttft_p50_ratio_baseline_hours"},
		{"absurd hour calls", `{"hour_calls":1000000}`, "alerts.hour_calls"},
		{"negative ttft p90", `{"ttft_p90_ms":-15000}`, "alerts.ttft_p90_ms"},
		{"absurd ttft p90", `{"ttft_p90_ms":600001}`, "alerts.ttft_p90_ms"},
		{"negative decode ratio", `{"decode_ratio":-0.7}`, "alerts.decode_ratio"},
		{"absurd decode ratio", `{"decode_ratio":4}`, "alerts.decode_ratio"},
		{"negative hour calls", `{"hour_calls":-30}`, "alerts.hour_calls"},
		{"negative min tokens", `{"decode_min_tokens":-300}`, "alerts.decode_min_tokens"},
		{"negative 429 rate", `{"rate_429_pct":-1}`, "alerts.rate_429_pct"},
		{"absurd 429 rate", `{"rate_429_pct":101}`, "alerts.rate_429_pct"},
		{"negative reject rate", `{"reject_pct":-0.1}`, "alerts.reject_pct"},
		{"absent reject rate", `{"reject_pct":-100}`, "alerts.reject_pct"},
		{"negative stall pct", `{"stall_pct":-5}`, "alerts.stall_pct"},
		{"absurd stall pct", `{"stall_pct":500}`, "alerts.stall_pct"},
		{"negative slot waits", `{"slot_waits_per_run":-20}`, "alerts.slot_waits_per_run"},
		{"absurd min tokens", `{"decode_min_tokens":10000001}`, "alerts.decode_min_tokens"},
		{"absurd slot waits", `{"slot_waits_per_run":100001}`, "alerts.slot_waits_per_run"},
		{"negative window", `{"window_hours":-72}`, "alerts.window_hours"},
		{"absurd window", `{"window_hours":8761}`, "alerts.window_hours"},
		{"negative ratio baseline", `{"ttft_p50_ratio_baseline_hours":-168}`, "alerts.ttft_p50_ratio_baseline_hours"},
		{"absurd ratio baseline", `{"ttft_p50_ratio_baseline_hours":99999}`, "alerts.ttft_p50_ratio_baseline_hours"},
		{"zero decode baseline", `{"decode_baseline_hours":0}`, "alerts.decode_baseline_hours"},
		{"negative decode baseline", `{"decode_baseline_hours":-1}`, "alerts.decode_baseline_hours"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			_, err := loadConfig(writeAlertModels(t, bad.block))
			if err == nil {
				t.Fatalf("alerts %s: loadConfig accepted it", bad.block)
			}
			if !strings.Contains(err.Error(), bad.field) {
				t.Fatalf("alerts %s: err = %v; want a message naming %s", bad.block, err, bad.field)
			}
		})
	}
	// A valid block is accepted, including the boundary values: 0.01 is the
	// smallest decode ratio, and an explicit 0 reject_pct refuses nothing.
	if _, err := loadConfig(writeAlertModels(t, `{"decode_ratio":0.01,"reject_pct":0,"ttft_p50_ratio":1}`)); err != nil {
		t.Fatalf("boundary alerts refused: %v", err)
	}
}
