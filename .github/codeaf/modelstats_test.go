// Tests for the deterministic core of `harness model-stats` (issue #131).
//
// The fixture is deliberately small and hand-checked: the percentiles, the
// rates and every alert are computed by hand in the test's own comment, so a
// change in the routine that shifts a figure fails here rather than in a
// report somebody reads later.
package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestModelStatsTables: a small fixture of ledger lines yields exact
// expected tables, percentiles and alerts. The rendered text is compared in
// full, not in substrings, so an extra table, a reordered column or a
// silently dropped alert all fail.
func TestModelStatsTables(t *testing.T) {
	cfg := modelStatsTestConfig(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	win := modelStatsWindowFrom(cfg, now)

	// The fixture, in one run (zip). Model ids are the two models.json
	// carries in the test config; the timestamps are inside the window
	// (now-72h .. now).
	//
	//   alpha: 6 calls, all completing.
	//     ttft 1000,1000,1000,1000,1000,1000 -> p50=p90=p99=1000 (nearest rank)
	//     decode: only completions with >= 300 tokens count. dur-ttft:
	//       c1 9000-1000=8000ms, 1000 tok -> 125.0 tok/s
	//       c2 113000-1000=112000ms, 1000 tok -> 8.92857 tok/s
	//       c3 29000-1000=28000ms, 1000 tok -> 35.71428 tok/s
	//       p50 of [125, 8.928571, 35.714286] sorted -> [8.928571, 35.714286, 125]
	//       nearest rank p50 = sorted[ceil(0.5*3)-1] = sorted[1] = 35.714286
	//     e2e tok/s: 1000/9 = 111.111; 1000/113 = 8.8495; 1000/29 = 34.4828 ...
	//       p50 = sorted[2] of 6 values.
	//   beta: 29 calls with ttft 2000 and a completion of 0 tokens on one,
	//     so beta has 28 completions, one abandoned slot wait (wait 5 s and
	//     no completion) and 29 calls. beta completes 1000 tokens in
	//     12000-2000=10000 ms -> 100.0 tok/s; one completion is under the
	//     300-token decode floor? No: 1000 >= 300, so all 28 count, and the
	//     p50 is 100.0. beta's e2e p50: 1000/12 = 83.333 tok/s, with the
	//     zero-token call at 0 -> p50 = sorted[14] = 83.333.
	lines := []string{
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":1000,"usage_seen":true,"duration_note":"ignored","ttft_ms":1000,"dur_ms":9000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":1000,"usage_seen":true,"ttft_ms":1000,"dur_ms":113000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":1000,"usage_seen":true,"ttft_ms":1000,"dur_ms":29000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":10,"usage_seen":true,"ttft_ms":1000,"dur_ms":2000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":10,"usage_seen":true,"ttft_ms":1000,"dur_ms":2000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":10,"usage_seen":true,"ttft_ms":1000,"dur_ms":2000}`,
	}
	// beta: 29 calls at 2026-10-21T10:00Z (one hour bucket, under the 30-call
	// hourly floor, so the hourly p50 alert cannot fire on it).
	for i := 0; i < 29; i++ {
		tok, seen := 1000, "true"
		if i == 0 {
			tok, seen = 0, "false"
		}
		lines = append(lines, fmt.Sprintf(
			`{"ts":"2026-10-21T10:00:00Z","model":"beta","completion_tokens":%d,"usage_seen":%s,"ttft_ms":2000,"dur_ms":12000}`,
			tok, seen))
		if i == 0 {
			// The abandoned wait: a call that queued 5 s for a slot and then
			// produced no completion at all.
			lines = append(lines, `{"ts":"2026-10-21T10:00:00Z","model":"beta","completion_tokens":0,"usage_seen":false,"ttft_ms":2000,"dur_ms":12000,"slot_wait_s":5}`)
		}
	}
	// A status marker: a 429 the proxy never completed.
	lines = append(lines, `{"ts":"2026-10-21T10:00:00Z","model":"beta","kind":"status","status":429}`)
	zr := modelStatsTestZip(t, "codeaf-ledger.jsonl", strings.Join(lines, "\n")+"\n")

	in := parseModelStatsZips([]*zip.Reader{zr}, cfg)
	if in.RunCount != 1 || in.Stats.Rows != 6+29+1 || in.Stats.Rejected != 0 {
		t.Fatalf("window: runs=%d rows=%d rejected=%d, want 1/36/0", in.RunCount, in.Stats.Rows, in.Stats.Rejected)
	}
	got := modelStatsReport(in, cfg, win, now)
	const goldenPath = "testdata/modelstats/tables.md"
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading %s: %v (the golden is the hand-checked rendering; regenerate it only from a hand-checked diff)", goldenPath, err)
	}
	if got != string(want) {
		t.Errorf("rendered report differs from %s\n--- got ---\n%s\n--- want ---\n%s", goldenPath, got, want)
	}
	// The figures the tables have to carry, stated independently of the
	// golden file so a regenerated golden cannot hide a wrong number.
	for _, wantLine := range []string{
		"| `alpha` | 6 | 1 | 1.0 s | 1.0 s | 1.0 s | 35.7 (8.9-125.0) | 34.5 | 0 | 0 | 0 | 0 | 0.00% | 0 | 0.0 | 0 |",
		"| `beta` | 30 | 1 | 2.0 s | 2.0 s | 2.0 s | 100.0 (100.0-100.0) | 83.3 | 0 | 1 | 1 | 0 | 0.00% | 1 | 5.0 | 1 |",
		"| 2026-10-21 10:00 | `beta` | 30 | 2.0 s |",
		"| 2026-10-21 11:00 | `alpha` | 6 | 1.0 s |",
		"Rows: 36 accepted, 0 rejected (0.00%, threshold 1.00%) across 1 run(s) with a ledger.",
		"`beta`: 1 abandoned slot wait(s)",
	} {
		if !strings.Contains(got, wantLine) {
			t.Errorf("report is missing %q\n%s", wantLine, got)
		}
	}
	if !strings.Contains(got, "### Alerts (3)") {
		t.Errorf("expected exactly one alert (the abandoned wait), got:\n%s", got)
	}
	if !strings.Contains(got, "`gamma`") {
		t.Errorf("a configured model with no calls was not reported in the tables:\n%s", got)
	}
}

// TestModelStatsStatusIsNotAMarkerUnlessKindSaysSo: the proxy writes a
// status line only with kind "status" (record, main.go), and a successful
// call carries no kind. A call that nevertheless records an explicit status
// -- the ledger this routine reads -- must still count as a call: keying the
// marker branch off the status number alone dropped it from every table and
// charged it to the error class (found while running the issue #131
// end-to-end check on a directory of hand-built zips).
func TestModelStatsStatusIsNotAMarkerUnlessKindSaysSo(t *testing.T) {
	cfg := modelStatsTestConfig(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	win := modelStatsWindowFrom(cfg, now)

	// One call with an explicit status 200, one marker, one plain call.
	lines := []string{
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","status":200,"completion_tokens":1000,"usage_seen":true,"ttft_ms":1000,"dur_ms":9000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","kind":"status","status":429}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":1000,"usage_seen":true,"ttft_ms":1000,"dur_ms":9000}`,
	}
	zr := modelStatsTestZip(t, "codeaf-ledger.jsonl", strings.Join(lines, "\n")+"\n")

	in := parseModelStatsZips([]*zip.Reader{zr}, cfg)
	// Both calls count; the marker is not a call and is not a rejection.
	if in.Stats.Rows != 2 || in.Stats.Rejected != 0 {
		t.Fatalf("status 200 call was not counted: rows=%d rejected=%d, want 2/0 (classes=%v)",
			in.Stats.Rows, in.Stats.Rejected, in.Stats.RejectedClass)
	}
	got := modelStatsReport(in, cfg, win, now)
	// The call's status 200 is a success: no error, no 429 from it. The
	// marker's 429 is the one 429.
	if !strings.Contains(got, "| `alpha` | 2 | 1 | 1.0 s |") {
		t.Errorf("the status-200 call is missing from the model table:\n%s", got)
	}
	// The status-200 call must not be charged to a class; the marker's 429
	// is the only 429 and the only error (Errs sums the 4xx/5xx/other/429
	// classes). Columns after e2e tok/s are: cut | 429 | error | stall.
	if !strings.Contains(got, "| 0 | 1 | 1 | 0 |") {
		t.Errorf("want cut=0, one 429 (the marker), one error (that same 429) and no stall, got:\n%s", got)
	}
	// 429 rate is 1 of (2 calls + 1 marker) = 33.33%: the alert must
	// still fire, and the denominator must include the marker as a call
	// the proxy never completed. The whole line is pinned so the wording
	// stays identical to the marker-only branch the alert loop grew for
	// issue #320.
	want := "`alpha`: 429 rate 33.33% above the 1.00% threshold (1 of 3); " +
		"consider lowering max_concurrent, not raising it"
	if !strings.Contains(got, want) {
		t.Errorf("the 429 alert did not fire on a 33.33%% rate, want %q:\n%s", want, got)
	}
}

// TestModelStatsThrottledToDeathFires429Alert: issue #320. A model whose
// every call was refused with 429 never has a call row -- a refusal is
// recorded only as a kind "status" marker -- so Calls is 0 and Four29 is
// the marker count. The 429-rate alert is the one that carries the
// operational advice ("consider lowering max_concurrent, not raising it"),
// and it must fire on the markers alone, with the marker count alone as
// the denominator, in place of the bare "no calls" line.
func TestModelStatsThrottledToDeathFires429Alert(t *testing.T) {
	cfg := modelStatsTestConfigWith(t, "delta")
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	win := modelStatsWindowFrom(cfg, now)

	// Two refused calls, recorded as markers only: no call row ever exists.
	lines := []string{
		`{"ts":"2026-10-21T11:00:00Z","model":"delta","kind":"status","status":429}`,
		`{"ts":"2026-10-21T11:01:00Z","model":"delta","kind":"status","status":429}`,
	}
	zr := modelStatsTestZip(t, "codeaf-ledger.jsonl", strings.Join(lines, "\n")+"\n")

	in := parseModelStatsZips([]*zip.Reader{zr}, cfg)
	if in.Stats.Rows != 0 {
		t.Fatalf("a marker produced a call row: rows=%d, want 0", in.Stats.Rows)
	}
	got := modelStatsReport(in, cfg, win, now)

	// The table still shows the throttling: zero calls, and the two
	// 429s are also the two errors (Errs sums the 4xx/5xx/other/429
	// classes: cut=0 | 429=2 | error=2 | stall=0).
	if !strings.Contains(got, "| `delta` | 0 | 0 |") {
		t.Errorf("the marker-only model is missing from the model table:\n%s", got)
	}
	if !strings.Contains(got, "| 0 | 2 | 2 | 0 |") {
		t.Errorf("want cut=0, two 429s and those same two errors in the table, got:\n%s", got)
	}
	// The alert names the model and divides by the marker count alone
	// (2 of 2 = 100%), in the same wording as the call-bearing case.
	want := "`delta`: 429 rate 100.00% above the 1.00% threshold (2 of 2); " +
		"consider lowering max_concurrent, not raising it"
	if !strings.Contains(got, want) {
		t.Errorf("the 429 alert did not fire on markers alone, want %q:\n%s", want, got)
	}
	if strings.Contains(got, "configured model with no calls in the window") {
		t.Errorf("a throttled model got the bare no-calls line instead of the 429 advice:\n%s", got)
	}
	// Exactly that one alert: no other per-model alert fires on markers.
	if !strings.Contains(got, "### Alerts (1)") {
		t.Errorf("want exactly the 429 alert for the marker-only model:\n%s", got)
	}
}

// TestModelStatsMarkersAreWindowFiltered: a status marker from outside the
// window must not count in the error classes or the 429-rate denominator.
// filterWindow used to copy every marker verbatim while rejecting
// out-of-window call rows, so a directory holding artifacts older than the
// 72 h window reported a 429 rate for hours the report does not cover --
// stale 429s inflated both the numerator-bound class table and the
// denominator counting markers as calls, which can push the rate down past
// the threshold and quieten the very alert the denominator exists for
// (issue #319). The decoder also has to keep the marker timestamp it used
// to drop, a line the proxy always records.
func TestModelStatsMarkersAreWindowFiltered(t *testing.T) {
	cfg := modelStatsTestConfig(t)
	now := time.Now().UTC()
	oneHourAgo := now.Add(-1 * time.Hour).Format(time.RFC3339)
	lines := []string{
		// Two in-window calls.
		fmt.Sprintf(`{"ts":%q,"model":"alpha","completion_tokens":1000,"usage_seen":true,"ttft_ms":1000,"dur_ms":9000}`, oneHourAgo),
		fmt.Sprintf(`{"ts":%q,"model":"alpha","completion_tokens":1000,"usage_seen":true,"ttft_ms":1000,"dur_ms":9000}`, oneHourAgo),
		// An in-window 429 marker: it must count everywhere.
		fmt.Sprintf(`{"ts":%q,"model":"alpha","kind":"status","status":429}`, oneHourAgo),
		// A 429 marker older than the 72 h window: it must count nowhere.
		fmt.Sprintf(`{"ts":%q,"model":"alpha","kind":"status","status":429}`, now.Add(-100*time.Hour).Format(time.RFC3339)),
	}
	zr := modelStatsTestZip(t, "codeaf-ledger.jsonl", strings.Join(lines, "\n")+"\n")

	in := parseModelStatsZips([]*zip.Reader{zr}, cfg)
	if in.Stats.Rejected != 0 || len(in.Stats.Markers) != 2 {
		t.Fatalf("decode: rows rejected=%d markers=%d, want 0/2 (both markers parse; the window is applied later)",
			in.Stats.Rejected, len(in.Stats.Markers))
	}
	win := modelStatsWindowFrom(cfg, now)
	filtered := filterWindow(in, win)
	if n := countMarkers(filtered, "alpha"); n != 1 {
		t.Fatalf("filterWindow kept %d marker(s) in the window, want the one in-window 429", n)
	}
	// The class table: the in-window 429 is the only 429 and the only
	// error; the stale one charges no class.
	for _, a := range modelAggregate(filtered, cfg, win) {
		if a.ID == "alpha" && (a.Calls != 2 || a.Four29 != 1 || a.Errs != 1) {
			t.Errorf("alpha aggregate: calls=%d four29=%d errs=%d, want 2/1/1 (the out-of-window 429 counted too)",
				a.Calls, a.Four29, a.Errs)
		}
	}
	got := modelStatsReport(in, cfg, win, now)
	// Columns after e2e tok/s are cut | 429 | error | stall (see
	// TestModelStatsStatusIsNotAMarkerUnlessKindSaysSo).
	if !strings.Contains(got, "| `alpha` | 2 | 1 |") {
		t.Errorf("the in-window calls are missing from the model table:\n%s", got)
	}
	if !strings.Contains(got, "| 0 | 1 | 1 | 0 |") {
		t.Errorf("the error classes count the stale marker, or miss the in-window one:\n%s", got)
	}
	// The 429 rate and its denominator count the in-window marker as one of
	// three calls (2 rows + 1 marker); with the bug the stale marker made it
	// 2 of 4.
	if !strings.Contains(got, "429 rate 33.33% above the 1.00% threshold (1 of 3)") {
		t.Errorf("the 429 alert does not state the in-window marker alone as its own count and denominator:\n%s", got)
	}
	if strings.Contains(got, "2 of 4") {
		t.Errorf("the stale marker leaked into the 429 count or the denominator:\n%s", got)
	}
}

// modelStatsTestConfig is a small config built in the test, never read from
// models.json (AGENTS.md: a test builds its own config).
func modelStatsTestConfig(t *testing.T) Config {
	t.Helper()
	alerts, err := validateAlerts(Alerts{})
	if err != nil {
		t.Fatalf("validateAlerts: %v", err)
	}
	return Config{Alerts: alerts, Models: []Model{{ID: "alpha"}, {ID: "beta"}, {ID: "gamma"}}}
}

// modelStatsTestConfigWith builds a config whose models are exactly the ids
// given. The shared test config carries three models, two of which have no
// calls, and "configured model with no calls in the window" is an alert of
// its own: a test that counts alerts states its own model set so the count
// is about the alert under test.
func modelStatsTestConfigWith(t *testing.T, ids ...string) Config {
	t.Helper()
	alerts, err := validateAlerts(Alerts{})
	if err != nil {
		t.Fatalf("validateAlerts: %v", err)
	}
	models := make([]Model, 0, len(ids))
	for _, id := range ids {
		models = append(models, Model{ID: id})
	}
	return Config{Alerts: alerts, Models: models}
}

// modelStatsTestZip builds an in-memory zip, the way a downloaded
// codeaf-output artifact arrives -- there is no fixture on disk and nothing
// is extracted.
func modelStatsTestZip(t *testing.T, entries ...string) *zip.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i+1 < len(entries); i += 2 {
		w, err := zw.Create(entries[i])
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := w.Write([]byte(entries[i+1])); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip reader: %v", err)
	}
	return zr
}

// TestModelStatsDeterministic: the same inputs yield byte-identical output,
// which is what makes the comment safe to edit in place.
func TestModelStatsDeterministic(t *testing.T) {
	cfg := modelStatsTestConfig(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	win := modelStatsWindowFrom(cfg, now)
	zr := modelStatsTestZip(t, "codeaf-ledger.jsonl", strings.Join([]string{
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":1000,"usage_seen":true,"ttft_ms":1000,"dur_ms":9000}`,
		`{"ts":"2026-10-21T10:00:00Z","model":"beta","completion_tokens":500,"usage_seen":true,"ttft_ms":1500,"dur_ms":5000}`,
	}, "\n")+"\n")
	first := modelStatsReport(parseModelStatsZips([]*zip.Reader{zr}, cfg), cfg, win, now)
	for i := 0; i < 3; i++ {
		again := modelStatsReport(parseModelStatsZips([]*zip.Reader{zr}, cfg), cfg, win, now)
		if again != first {
			t.Fatalf("report %d differs from the first rendering", i+2)
		}
	}
}

// TestModelStatsRejectsHostileRows: oversized lines, unknown models,
// negative or huge numbers, and markdown or @-mentions in any field are
// either rejected or never echoed, and the rejected count is reported.
func TestModelStatsRejectsHostileRows(t *testing.T) {
	cfg := modelStatsTestConfig(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	win := modelStatsWindowFrom(cfg, now)

	hostileMarkdown := "@everyone **pwn** [click](https://example.invalid) `code` <b>x</b>"
	lines := []string{
		// Accepted: one honest call, the baseline the reject count is read
		// against.
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":1000,"usage_seen":true,"ttft_ms":1000,"dur_ms":9000}`,
		// A free-text field carrying markdown and at-mentions, alongside the
		// valid fields: the row is accepted, and nothing of it is echoed.
		// Its kind is empty -- the proxy leaves kind empty on a call -- so it
		// is a call and not an unknown kind; the hostile text below is in
		// fields the report never reads.
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","upstream":"` + hostileMarkdown + `","completion_tokens":1000,"usage_seen":true,"ttft_ms":1500,"dur_ms":9000,"kind":""}`,
		// A kind the reader does not know, carrying hostile text: refused,
		// and the text is not echoed. (Not a row that "was accepted and never
		// echoed": an unknown kind is not a call.)
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":1000,"usage_seen":true,"ttft_ms":1500,"dur_ms":9000,"kind":"` + hostileMarkdown + `"}`,
		// An unknown model id, and an id that is not a string at all.
		`{"ts":"2026-10-21T11:00:00Z","model":"` + hostileMarkdown + `","ttft_ms":1000,"dur_ms":9000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":12345,"ttft_ms":1000,"dur_ms":9000}`,
		// Negative and huge numbers.
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","ttft_ms":-1000,"dur_ms":9000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","ttft_ms":9999999999999,"dur_ms":9000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":-5,"usage_seen":true,"ttft_ms":1000,"dur_ms":9000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":999999999999,"usage_seen":true,"ttft_ms":1000,"dur_ms":9000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","slot_wait_s":-1,"ttft_ms":1000,"dur_ms":9000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","slot_wait_s":1e12,"ttft_ms":1000,"dur_ms":9000}`,
		// A status outside 100..599.
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","kind":"status","status":999}`,
		// A first token after the call ended.
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","ttft_ms":9000,"dur_ms":1000}`,
		// No timestamp, a timestamp in another century, and a line that is
		// not JSON at all.
		`{"model":"alpha","ttft_ms":1000,"dur_ms":9000}`,
		`{"ts":"1901-01-01T00:00:00Z","model":"alpha","ttft_ms":1000,"dur_ms":9000}`,
		`{"ts":"<img src=x onerror=alert(1)>","model":"alpha"`,
		// An array and a bare string are not ledger lines.
		`[1,2,3]`,
		`"` + hostileMarkdown + `"`,
		// An unterminated line longer than the 64 KiB cap.
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","note":"` + strings.Repeat("A", 70*1024) + `"}`,
	}
	zr := modelStatsTestZip(t, "codeaf-ledger.jsonl", strings.Join(lines, "\n")+"\n")
	in := parseModelStatsZips([]*zip.Reader{zr}, cfg)
	got := modelStatsReport(in, cfg, win, now)

	// Two rows survive: the honest call and the one whose markdown sits in
	// fields the report never reads.
	if in.Stats.Rows != 2 {
		t.Errorf("accepted rows = %d, want 2:\n%s", in.Stats.Rows, got)
	}
	wantRejected := len(lines) - 2
	if in.Stats.Rejected != wantRejected {
		t.Errorf("rejected rows = %d, want %d", in.Stats.Rejected, wantRejected)
	}
	// The rejected count and its share are in the report, and over the 1%
	// threshold the report alerts.
	if !strings.Contains(got, fmt.Sprintf("%d rejected", wantRejected)) {
		t.Errorf("report does not carry the rejected count:\n%s", got)
	}
	if !strings.Contains(got, "above the 1.00% threshold") {
		t.Errorf("the rejected share should alert (it is far over 1%%):\n%s", got)
	}
	// Nothing hostile is echoed: not the markdown, not the at-mention, not
	// the model name, not the oversized line.
	for _, leaked := range []string{"everyone", "pwn", "click", "example.invalid", "onerror", "<b>", "<img", strings.Repeat("A", 100)} {
		if strings.Contains(got, leaked) {
			t.Errorf("report echoes hostile input %q:\n%s", leaked, got)
		}
	}
	// Every reason the reader states is its own text.
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "- ") && !strings.HasPrefix(l, "- **") && !strings.Contains(l, "rejected") &&
			!strings.Contains(l, "model not in") && !strings.Contains(l, "outside") &&
			!strings.Contains(l, "not a JSON") && !strings.Contains(l, "longer than") &&
			!strings.Contains(l, "timestamp") && !strings.Contains(l, "not finite") &&
			!strings.Contains(l, "line kind") &&
			!strings.Contains(l, "s:") && !strings.Contains(l, "skipped") {
			t.Errorf("unexpected reported reason %q", l)
		}
	}
}

// TestParseModelStatsZipBounds: a zip bomb and too many entries are refused
// without extracting.
func TestParseModelStatsZipBounds(t *testing.T) {
	allowed := map[string]bool{"alpha": true}
	b := defaultModelStatsBounds()

	// Too many entries: a zip with more files than any artifact has. It is
	// refused whole -- not read until the cap -- and named by count.
	many := []string{}
	for i := 0; i < 3; i++ {
		many = append(many, fmt.Sprintf("junk-%d.txt", i), "x")
	}
	manyZip := modelStatsTestZip(t, many...)
	tight := defaultModelStatsBounds()
	tight.MaxEntries = 2
	in := parseModelStatsWithBounds([]*zip.Reader{manyZip}, allowed, tight)
	if in.RunCount != 0 || in.Stats.Zips != 0 || in.Stats.ZipSkipped != 1 {
		t.Fatalf("many-entry zip: runs=%d zips=%d skipped=%d, want 0/0/1", in.RunCount, in.Stats.Zips, in.Stats.ZipSkipped)
	}
	if len(in.Stats.SkippedReason) != 1 || !strings.Contains(in.Stats.SkippedReason[0], "over the 2-entry cap") {
		t.Fatalf("many-entry zip reason = %v", in.Stats.SkippedReason)
	}

	// A zip bomb: a tiny compressed entry whose header claims far more than
	// the uncompressed cap. The reader must refuse it on the header alone,
	// so it is never decompressed.
	bomb := modelStatsTestZip(t, modelStatsLedgerName, strings.Repeat("A", 4096))
	if len(bomb.File) != 1 || bomb.File[0].UncompressedSize64 < 4096 {
		t.Fatalf("fixture is not a bomb: %+v", bomb.File)
	}
	small := defaultModelStatsBounds()
	small.MaxEntryUnc = 1024
	small.MaxEntryComp = 1 << 20
	in = parseModelStatsWithBounds([]*zip.Reader{bomb}, allowed, small)
	if len(in.Rows) != 0 {
		t.Fatalf("bomb produced %d rows, want 0", len(in.Rows))
	}
	if len(in.Stats.SkippedReason) != 1 || !strings.Contains(in.Stats.SkippedReason[0], "uncompressed size over the cap") {
		t.Fatalf("bomb reason = %v", in.Stats.SkippedReason)
	}

	// A compressed-size cap refuses before inflation too.
	comp := defaultModelStatsBounds()
	comp.MaxEntryComp = 1
	in = parseModelStatsWithBounds([]*zip.Reader{bomb}, allowed, comp)
	if len(in.Rows) != 0 || len(in.Stats.SkippedReason) != 1 ||
		!strings.Contains(in.Stats.SkippedReason[0], "compressed size over the cap") {
		t.Fatalf("compressed cap: rows=%d reasons=%v", len(in.Rows), in.Stats.SkippedReason)
	}

	// An entry the header accepts is still read under a cap: the reader never
	// inflates more than MaxEntryUnc bytes of it, and refuses it outright when
	// the stream reaches the cap. A zip whose deflate stream expands past
	// what its header promised (a hand-built archive, or one a future Go
	// version reports differently) therefore cannot inflate without bound.
	big := modelStatsTestZip(t, modelStatsLedgerName,
		strings.Repeat(`{"ts":"2026-10-21T11:00:00Z","model":"alpha","ttft_ms":1,"dur_ms":2}`+"\n", 500))
	liar := defaultModelStatsBounds()
	liar.MaxEntryUnc = 64 * int64(zipReaderUncompressedSize(big)) // the header passes...
	in = parseModelStatsWithBounds([]*zip.Reader{big}, allowed, liar)
	if len(in.Rows) == 0 {
		t.Fatalf("read cap: expected the accepted entry to be read, got reasons=%v", in.Stats.SkippedReason)
	}
	tiny := defaultModelStatsBounds()
	tiny.MaxEntryUnc = int64(zipReaderUncompressedSize(big))
	in = parseModelStatsWithBounds([]*zip.Reader{big}, allowed, tiny)
	if len(in.Rows) != 0 {
		t.Fatalf("read cap: %d rows past a cap the stream reaches, want 0", len(in.Rows))
	}
	if len(in.Stats.SkippedReason) == 0 || !strings.Contains(in.Stats.SkippedReason[0], "inflated past the cap") {
		t.Fatalf("read cap: reasons=%v", in.Stats.SkippedReason)
	}

	// And the ordinary caps hold: 20k rows in one run, and a bounded total.
	rows := []string{}
	for i := 0; i < b.MaxRunRows+10; i++ {
		rows = append(rows, `{"ts":"2026-10-21T11:00:00Z","model":"alpha","ttft_ms":1,"dur_ms":2}`)
	}
	run := modelStatsTestZip(t, modelStatsLedgerName, strings.Join(rows, "\n")+"\n")
	in = parseModelStatsWithBounds([]*zip.Reader{run}, allowed, b)
	if len(in.Rows) != b.MaxRunRows {
		t.Fatalf("row cap: got %d rows, want %d", len(in.Rows), b.MaxRunRows)
	}

	// Several ledger entries in one zip (issue #388): matched by base name,
	// the same ledger under several directories is read once per entry, so
	// the per-run cap has to bound the run across entries -- not the entry.
	// The excess is stated in SkippedReason like every other cap.
	entries := []string{}
	for i := 0; i < 4; i++ {
		entries = append(entries,
			fmt.Sprintf("copy-%d/%s", i, modelStatsLedgerName),
			strings.Join(rows, "\n")+"\n")
	}
	multi := modelStatsTestZip(t, entries...)
	in = parseModelStatsWithBounds([]*zip.Reader{multi}, allowed, b)
	if len(in.Rows) != b.MaxRunRows {
		t.Fatalf("multi-entry row cap: got %d rows, want %d", len(in.Rows), b.MaxRunRows)
	}
	capped := false
	for _, r := range in.Stats.SkippedReason {
		if strings.Contains(r, "per-run cap") {
			capped = true
		}
	}
	if !capped {
		t.Fatalf("multi-entry row cap: no reason states the excess, got %v", in.Stats.SkippedReason)
	}
	if in.Stats.Rows != b.MaxRunRows {
		t.Fatalf("multi-entry row cap: counted %d rows, want %d", in.Stats.Rows, b.MaxRunRows)
	}
}

// TestParseModelStatsZipCountCap: the zip-count cap refuses only the
// artifacts past it, not the whole window. The first MaxZips zips are read
// and the excess is counted as skipped with one bounded reason -- the same
// per-item refusal every other bound in the reader uses.
func TestParseModelStatsZipCountCap(t *testing.T) {
	allowed := map[string]bool{"alpha": true}
	b := defaultModelStatsBounds()
	b.MaxZips = 3

	line := `{"ts":"2026-10-21T11:00:00Z","model":"alpha","ttft_ms":1,"dur_ms":2}` + "\n"
	var zips []*zip.Reader
	for i := 0; i < b.MaxZips+1; i++ {
		zips = append(zips, modelStatsTestZip(t, modelStatsLedgerName, line))
	}

	// Over the cap: every zip up to it is read, the excess is one skip line.
	in := parseModelStatsWithBoundsSkipped(zips, nil, allowed, b)
	if in.Stats.Zips != b.MaxZips {
		t.Errorf("zips = %d, want %d (the cap must refuse only the excess)", in.Stats.Zips, b.MaxZips)
	}
	if in.Stats.ZipSkipped != 1 || len(in.Stats.SkippedReason) != 1 ||
		!strings.Contains(in.Stats.SkippedReason[0], "over the 3-zip cap") {
		t.Errorf("excess: skipped=%d reasons=%v, want one bounded reason", in.Stats.ZipSkipped, in.Stats.SkippedReason)
	}

	// At the cap: a window at or under it reads every zip and skips nothing.
	at := parseModelStatsWithBoundsSkipped(zips[:b.MaxZips], nil, allowed, b)
	if at.Stats.Zips != b.MaxZips || at.Stats.ZipSkipped != 0 || len(at.Stats.SkippedReason) != 0 {
		t.Errorf("at cap: zips=%d skipped=%d reasons=%v, want every zip read",
			at.Stats.Zips, at.Stats.ZipSkipped, at.Stats.SkippedReason)
	}
}

// zipReaderUncompressedSize is the size the reader sees on a test zip's
// single entry header, which the caps in these tests are set against.
func zipReaderUncompressedSize(zr *zip.Reader) uint64 {
	if len(zr.File) == 0 {
		return 0
	}
	return zr.File[0].UncompressedSize64
}

// modelStatsBaselineFixture builds the hours a baseline alert needs, in one
// run (zip), and returns the parsed input plus the window and clock the
// report is rendered with. The hours are laid out against the default 72 h
// window ending 2026-10-21 12:00 UTC:
//
//	2026-10-19 00:00  (60 h old)  the older part of the same window
//	2026-10-21 09:00  ( 3 h old)  the recent hour
//
// now-168h is 2026-10-14 12:00, well before the window starts, so the
// configured 168 h baseline reaches past the window and the routine has to
// measure it from the older part of the same window -- the case the 2x alert
// could never fire on before (every hour sat inside the 168 h cutoff, base
// was 0 and ratioUsable was false).
func modelStatsBaselineFixture(t *testing.T, cfg Config, oldHour, recentHour []string) (modelStatsInput, modelStatsWindow, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	win := modelStatsWindowFrom(cfg, now)
	lines := append(append([]string{}, oldHour...), recentHour...)
	zr := modelStatsTestZip(t, "codeaf-ledger.jsonl", strings.Join(lines, "\n")+"\n")
	return parseModelStatsZips([]*zip.Reader{zr}, cfg), win, now
}

// TestModelStatsTrailingP50AlertReachable: recent hours whose first-token p50
// is above 2x the older hours' p50 must emit the trailing-p50 alert at the
// DEFAULT 72 h window. Before the baseline was clamped to the window start,
// now-168h put every hour bucket inside the cutoff, base was 0,
// ratioUsable was false and the alert was unreachable -- this test emitted no
// alert at all.
func TestModelStatsTrailingP50AlertReachable(t *testing.T) {
	cfg := modelStatsTestConfigWith(t, "alpha")
	if cfg.Alerts.WindowHours == nil || *cfg.Alerts.WindowHours != 72 {
		t.Fatalf("this test is about the default window, got %v", cfg.Alerts.WindowHours)
	}
	if cfg.Alerts.TTFTp50RatioBaselineHours == nil || *cfg.Alerts.TTFTp50RatioBaselineHours != 168 {
		t.Fatalf("this test is about the default 168 h baseline, got %v", cfg.Alerts.TTFTp50RatioBaselineHours)
	}
	var oldHour, recentHour []string
	for i := 0; i < *cfg.Alerts.HourCalls; i++ { // older hour: p50 1000 ms
		oldHour = append(oldHour, `{"ts":"2026-10-19T00:00:00Z","model":"alpha","completion_tokens":10,"usage_seen":true,"ttft_ms":1000,"dur_ms":5000}`)
	}
	for i := 0; i < *cfg.Alerts.HourCalls; i++ { // recent hour: p50 5000 ms = 5x
		recentHour = append(recentHour, `{"ts":"2026-10-21T09:00:00Z","model":"alpha","completion_tokens":10,"usage_seen":true,"ttft_ms":5000,"dur_ms":9000}`)
	}
	in, win, now := modelStatsBaselineFixture(t, cfg, oldHour, recentHour)
	got := modelStatsReport(in, cfg, win, now)
	for _, want := range []string{
		// The baseline is the older hour's p50, measured inside the 72 h
		// window; the recent hour is 5000 ms against a 2x limit of 2000 ms.
		"- **`alpha`: 1 hour(s) with at least 30 calls above 2x the trailing 168.0 h first-token p50 " +
			"(`alpha` over 5.0 s vs a 1.0 s baseline; first was 2026-10-21 09:00 UTC with 30 calls)**",
		"### Alerts (1)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "hour(s) with at least 30 calls above") {
		t.Errorf("the 2x trailing-p50 alert did not fire at the default 72 h window:\n%s", got)
	}
}

// TestModelStatsTrailingP50AlertNotReachableBelowThreshold: the same fixture
// with the recent hour at 1.5x rather than 5x must NOT alert. This is what
// keeps the test above honest: it proves the alert fires on the ratio and not
// merely because recent and older hours exist.
func TestModelStatsTrailingP50AlertNotReachableBelowThreshold(t *testing.T) {
	cfg := modelStatsTestConfigWith(t, "alpha")
	var oldHour, recentHour []string
	for i := 0; i < *cfg.Alerts.HourCalls; i++ {
		oldHour = append(oldHour, `{"ts":"2026-10-19T00:00:00Z","model":"alpha","completion_tokens":10,"usage_seen":true,"ttft_ms":1000,"dur_ms":5000}`)
	}
	for i := 0; i < *cfg.Alerts.HourCalls; i++ {
		recentHour = append(recentHour, `{"ts":"2026-10-21T09:00:00Z","model":"alpha","completion_tokens":10,"usage_seen":true,"ttft_ms":1500,"dur_ms":6000}`)
	}
	in, win, now := modelStatsBaselineFixture(t, cfg, oldHour, recentHour)
	got := modelStatsReport(in, cfg, win, now)
	if strings.Contains(got, "above 2x the trailing") {
		t.Errorf("1.5x the baseline must not fire the 2x alert:\n%s", got)
	}
}

// TestModelStatsDecodeAlertReachable: a recent hour whose decode p50 sits
// below 0.7x the older hour's decode p50 must emit the decode alert. The old
// baseline was the very p50 the alert compared (x < 0.7x), so it could never
// fire.
func TestModelStatsDecodeAlertReachable(t *testing.T) {
	cfg := modelStatsTestConfigWith(t, "alpha")
	var oldHour, recentHour []string
	// Older hour: 1000 tokens in 11000-1000 = 10000 ms -> 100.0 tok/s.
	for i := 0; i < 3; i++ {
		oldHour = append(oldHour, `{"ts":"2026-10-19T00:00:00Z","model":"alpha","completion_tokens":1000,"usage_seen":true,"ttft_ms":1000,"dur_ms":11000}`)
	}
	// Recent hour: 1000 tokens in 21000-1000 = 20000 ms -> 50.0 tok/s,
	// below 0.7 x 100.0 = 70.0 tok/s.
	for i := 0; i < 3; i++ {
		recentHour = append(recentHour, `{"ts":"2026-10-21T09:00:00Z","model":"alpha","completion_tokens":1000,"usage_seen":true,"ttft_ms":1000,"dur_ms":21000}`)
	}
	in, win, now := modelStatsBaselineFixture(t, cfg, oldHour, recentHour)
	got := modelStatsReport(in, cfg, win, now)
	want := "`alpha`: decode p50 50.0 tok/s below 0.7x its 100.0 tok/s baseline"
	if !strings.Contains(got, want) {
		t.Errorf("report is missing the decode alert %q:\n%s", want, got)
	}
	// The table's own decode p50 is the window-wide figure the alert compares
	// -- 50.0 tok/s, the nearest-rank p50 of [100,100,100,50,50,50] -- and
	// the baseline is the older hour's 100.0. The alert fired on the trailing
	// baseline, not on the compared figure: the old decodeBaseline returned
	// exactly this 50.0, and 50.0 < 0.7*50.0 can never hold.
	if !strings.Contains(got, "| `alpha` | 6 | 1 | 1.0 s | 1.0 s | 1.0 s | 50.0 (50.0-100.0) | 47.6 |") {
		t.Errorf("the decode table does not carry the window-wide p50:\n%s", got)
	}
	if !strings.Contains(got, "### Alerts (1)") {
		t.Errorf("the decode fixture must emit exactly its one alert:\n%s", got)
	}
}

// TestModelStatsSingleHourEmitsNoBaselineAlerts: one complete hour is a
// single measurement. It is neither old nor recent relative to another hour,
// so it yields no baseline at all and NEITHER the trailing-p50 alert nor the
// decode alert fires -- even at 5x the p90 threshold's ratio and with a
// decode rate no other hour can judge.
func TestModelStatsSingleHourEmitsNoBaselineAlerts(t *testing.T) {
	cfg := modelStatsTestConfigWith(t, "alpha")
	var hour []string
	for i := 0; i < *cfg.Alerts.HourCalls; i++ {
		hour = append(hour, `{"ts":"2026-10-21T09:00:00Z","model":"alpha","completion_tokens":1000,"usage_seen":true,"ttft_ms":5000,"dur_ms":40000}`)
	}
	// One hour, and 5 h of it: every row is in the same bucket.
	in, win, now := modelStatsBaselineFixture(t, cfg, nil, hour)
	aggs := modelAggregate(in, cfg, win)
	if len(aggs) == 0 {
		t.Fatal("no aggregate for alpha")
	}
	var alpha *modelAgg
	for _, a := range aggs {
		if a.ID == "alpha" {
			alpha = a
		}
	}
	if alpha == nil || len(alpha.hours) != 1 {
		t.Fatalf("fixture is not a single hour: %+v", alpha)
	}
	if got := ttftBaseline(alpha, cfg, win, now); got != 0 {
		t.Errorf("ttft baseline for one complete hour = %v, want 0 (no baseline hour)", got)
	}
	if got := decodeBaseline(alpha, cfg, win, now); got != 0 {
		t.Errorf("decode baseline for one complete hour = %v, want 0 (no baseline hour)", got)
	}
	got := modelStatsReport(in, cfg, win, now)
	for _, leak := range []string{"above 2x the trailing", "decode p50"} {
		if strings.Contains(got, leak) {
			t.Errorf("a single complete hour emitted %q:\n%s", leak, got)
		}
	}
	if !strings.Contains(got, "**No alerts.**") {
		t.Errorf("a single complete hour must emit no alert at all:\n%s", got)
	}
	// 30 calls in one hour, so the hourly floor is met and the hour would be
	// judged -- if there were a second hour to judge it against.
	if !strings.Contains(got, "| 2026-10-21 09:00 | `alpha` | 30 | 5.0 s |") {
		t.Errorf("the single hour is not in the by-hour table:\n%s", got)
	}
}

// TestModelStatsCutCountedOnce: a cut row that produced no completion is
// one cut, not two. Found by review (finding 3): modelAggregate counted
// `r.Cut && r.TTFTMs > 0` and then called miss(r), which counted r.Cut
// again for the same non-completion row, so one cut+no-completion row read
// as Cuts=2 in the table while a cut row that did complete read as 1.
func TestModelStatsCutCountedOnce(t *testing.T) {
	cfg := modelStatsTestConfig(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	win := modelStatsWindowFrom(cfg, now)

	// One row only: a streaming call that was cut (the stream ended without
	// a [DONE] line) and produced no completion at all -- status 500, no
	// usage, zero tokens. That is exactly the review's probe.
	lines := []string{
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":0,"usage_seen":false,"ttft_ms":1000,"dur_ms":9000,"cut":true,"status":500}`,
	}
	zr := modelStatsTestZip(t, modelStatsLedgerName, strings.Join(lines, "\n")+"\n")
	in := parseModelStatsZips([]*zip.Reader{zr}, cfg)
	if in.Stats.Rows != 1 || in.Stats.Rejected != 0 {
		t.Fatalf("probe row: rows=%d rejected=%d (classes=%v), want 1/0", in.Stats.Rows, in.Stats.Rejected, in.Stats.RejectedClass)
	}
	// One row is one cut. The error class the row also carries (a 5xx) is
	// untouched by the fix: miss still owns that, and only that.
	aggs := modelAggregate(in, cfg, win)
	if len(aggs) == 0 {
		t.Fatalf("no aggregates")
	}
	a := aggs[0]
	if a.ID != "alpha" {
		t.Fatalf("first aggregate is %q, want alpha", a.ID)
	}
	if a.Calls != 1 || a.Cuts != 1 {
		t.Errorf("cut+no-completion row: calls=%d cuts=%d, want 1/1 (was cuts=2 before the fix)", a.Calls, a.Cuts)
	}
	if a.Fivexx != 1 || a.Errs != 1 {
		t.Errorf("the row's 5xx class was lost by the fix: fivexx=%d errs=%d, want 1/1", a.Fivexx, a.Errs)
	}
	got := modelStatsReport(in, cfg, win, now)
	// The table's cut column for that row, read as a whole line: n=1, one
	// run, cut=1, 429=0, error=1, stall=0.
	if !strings.Contains(got, "| `alpha` | 1 | 1 | 1.0 s | 1.0 s | 1.0 s | — | 0.0 | 1 | 0 | 1 | 0 | 0.00% | 0 | 0.0 | 0 |") {
		t.Errorf("the reported cut column is not 1 for the single cut row:\n%s", got)
	}
}

// TestModelStatsProxyMarkerLinesAreNotRows: the proxy's own ledger markers
// -- kind "proxy_start" (recordMarker, main.go), kind "search" (search.go:158,
// whose model is the fixed id "web-search") and kind "quotas" (no longer
// written now the quota guard is gone, but old ledgers still carry it) --
// are not rows about a model. Found by review (finding 5):
// they were decoded as model "" (or "web-search"), which is not in
// models.json, so every one of them was a rejection and a normal run tripped
// the >1% reject alert on the proxy's own bookkeeping.
func TestModelStatsProxyMarkerLinesAreNotRows(t *testing.T) {
	cfg := modelStatsTestConfig(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	win := modelStatsWindowFrom(cfg, now)

	lines := []string{
		// The one real call in the run.
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":1000,"usage_seen":true,"ttft_ms":1000,"dur_ms":9000}`,
		// proxy_start: no model field at all.
		`{"kind":"proxy_start"}`,
		// quotas: an opaque body, no model field. Not written any more, but
		// a ledger from before the quota guard was dropped still has one.
		`{"kind":"quotas","quotas":"{\"plan\":\"big\"}"}`,
		// search: the model the proxy writes is the fixed id web-search, not
		// a model in models.json.
		`{"kind":"search","model":"web-search","query_len":3,"results":2}`,
	}
	zr := modelStatsTestZip(t, modelStatsLedgerName, strings.Join(lines, "\n")+"\n")
	in := parseModelStatsZips([]*zip.Reader{zr}, cfg)

	if in.Stats.Rows != 1 || in.Stats.Rejected != 0 {
		t.Fatalf("proxy markers counted as rows: rows=%d rejected=%d classes=%v, want 1/0 (review probe was 1 accepted, 3 rejected = 75%%)",
			in.Stats.Rows, in.Stats.Rejected, in.Stats.RejectedClass)
	}
	got := modelStatsReport(in, cfg, win, now)
	if !strings.Contains(got, "Rows: 1 accepted, 0 rejected (0.00%, threshold 1.00%)") {
		t.Errorf("reject rate is not 0%% for a run carrying only proxy markers:\n%s", got)
	}
	if strings.Contains(got, "above the 1.00% threshold") {
		t.Errorf("the reject alert fired on the proxy's own marker lines:\n%s", got)
	}
	// The real call still reached the tables, and so did its hour.
	if !strings.Contains(got, "| `alpha` | 1 | 1 | 1.0 s | 1.0 s | 1.0 s | 125.0 (125.0-125.0) | 111.1 | 0 | 0 | 0 | 0 | 0.00% | 0 | 0.0 | 0 |") {
		t.Errorf("the real call is missing from the model table:\n%s", got)
	}
	if !strings.Contains(got, "| 2026-10-21 11:00 | `alpha` | 1 | 1.0 s |") {
		t.Errorf("the real call's hour is missing from the hourly table:\n%s", got)
	}
	// No model invented for the markers: web-search must never appear as a
	// model of its own.
	if strings.Contains(got, "web-search") {
		t.Errorf("the proxy's search line was reported as a model:\n%s", got)
	}

	// A "status" marker must keep working exactly as before: kept as a
	// marker for its error class, never a row and never a rejection.
	markerZip := modelStatsTestZip(t, modelStatsLedgerName, strings.Join([]string{
		lines[0],
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","kind":"status","status":429}`,
	}, "\n")+"\n")
	m := parseModelStatsZips([]*zip.Reader{markerZip}, cfg)
	if m.Stats.Rows != 1 || m.Stats.Rejected != 0 || len(m.Stats.Markers) != 1 {
		t.Fatalf("status marker: rows=%d rejected=%d markers=%d, want 1/0/1", m.Stats.Rows, m.Stats.Rejected, len(m.Stats.Markers))
	}
	if m.Stats.Markers[0].Status != 429 || m.Stats.Markers[0].Model != "alpha" {
		t.Fatalf("status marker lost its class: %+v", m.Stats.Markers[0])
	}
}

// TestModelStatsUnknownKindRefused: a kind that is neither a call (no kind),
// "status", "proxy_start", "quotas" nor "search" is refused, never silently
// accepted; and a row with no kind at all and an unknown model is still
// refused under the model class. The proxy markers are skipped before the
// model check, so that change must not become a hole a hostile row can slip
// through.
func TestModelStatsUnknownKindRefused(t *testing.T) {
	cfg := modelStatsTestConfig(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	win := modelStatsWindowFrom(cfg, now)

	lines := []string{
		// The honest call: the row the run really carries.
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":1000,"usage_seen":true,"ttft_ms":1000,"dur_ms":9000}`,
		// A line with no kind at all and an unknown model: refused under the
		// model class, exactly as before.
		`{"ts":"2026-10-21T11:00:00Z","model":"not-a-model-in-models.json","completion_tokens":1,"usage_seen":true,"ttft_ms":1000,"dur_ms":9000}`,
		// A line with no kind and no model at all: refused too.
		`{"ts":"2026-10-21T11:00:00Z","completion_tokens":1,"usage_seen":true,"ttft_ms":1000,"dur_ms":9000}`,
		// Kinds the reader does not know: a hostile line cannot borrow a real
		// model id and a made-up kind to skip the report, and it is not
		// silently accepted either.
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","kind":"totally-unknown-kind","ttft_ms":1000,"dur_ms":9000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","kind":"Proxy_Start","ttft_ms":1000,"dur_ms":9000}`,
	}
	zr := modelStatsTestZip(t, modelStatsLedgerName, strings.Join(lines, "\n")+"\n")
	in := parseModelStatsZips([]*zip.Reader{zr}, cfg)

	if in.Stats.Rows != 1 {
		t.Fatalf("accepted rows = %d, want 1 (only the honest call)", in.Stats.Rows)
	}
	// Four refusals: two under the model class (an unknown id, a missing
	// one), two under the unknown-kind class.
	if in.Stats.Rejected != 4 {
		t.Fatalf("rejected rows = %d, want 4: %v", in.Stats.Rejected, in.Stats.RejectedClass)
	}
	if got := in.Stats.RejectedClass[rejectUnknown]; got != 2 {
		t.Errorf("model-not-in-models.json class = %d, want 2 (one unknown id, one missing): %v", got, in.Stats.RejectedClass)
	}
	if got := in.Stats.RejectedClass[rejectUnknownKind]; got != 2 {
		t.Errorf("unknown-kind class = %d, want 2: %v", got, in.Stats.RejectedClass)
	}
	// The count is reported, and by class, in the comment.
	got := modelStatsReport(in, cfg, win, now)
	if !strings.Contains(got, "Rows: 1 accepted, 4 rejected (80.00%, threshold 1.00%)") {
		t.Errorf("the report does not carry the rejected count:\n%s", got)
	}
	if !strings.Contains(got, "- "+string(rejectUnknown)+": 2") {
		t.Errorf("the report does not carry the model class count:\n%s", got)
	}
	if !strings.Contains(got, "- "+string(rejectUnknownKind)+": 2") {
		t.Errorf("the report does not carry the unknown-kind class count:\n%s", got)
	}
	// Nothing a hostile line carried is echoed.
	for _, leaked := range []string{"totally-unknown-kind", "Proxy_Start", "not-a-model-in-models.json"} {
		if strings.Contains(got, leaked) {
			t.Errorf("report echoes hostile input %q:\n%s", leaked, got)
		}
	}
}

// TestModelStatsWaitedThenRefusedCountsSlotFigures: issue #342. A call that
// queued for a slot and was then refused — by upstream, or by the credit cap
// just after the wait — never reached the slot figures: the call-row gate
// also demanded a first token a refusal never has, and a status marker
// carried no slot_wait_s at all. The fixture is exactly the shape the issue
// names: a call row with slot_wait_s and no ttft_ms, plus a status-marker
// line carrying slot_wait_s, in one run.
func TestModelStatsWaitedThenRefusedCountsSlotFigures(t *testing.T) {
	cfg := modelStatsTestConfig(t)
	// Threshold 1, so two waits in one run fire the per-run alert; every
	// other threshold keeps its default.
	one := 1
	cfg.Alerts.SlotWaitsPerRun = &one
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	win := modelStatsWindowFrom(cfg, now)
	// The shape issue #342 names, in one run: a call row with slot_wait_s
	// and no ttft_ms -- a call that queued 120 s and died before a first
	// token -- plus a status-marker line carrying the wait its call queued
	// through before the proxy refused it with 402.
	lines := []string{
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","slot_wait_s":120}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","kind":"status","status":402,"slot_wait_s":7}`,
	}
	zr := modelStatsTestZip(t, "codeaf-ledger.jsonl", strings.Join(lines, "\n")+"\n")

	in := parseModelStatsZips([]*zip.Reader{zr}, cfg)
	if in.Stats.Rows != 1 || in.Stats.Rejected != 0 || len(in.Stats.Markers) != 1 {
		t.Fatalf("rows=%d rejected=%d markers=%d, want 1/0/1", in.Stats.Rows, in.Stats.Rejected, len(in.Stats.Markers))
	}
	var alpha *modelAgg
	for _, a := range modelAggregate(filterWindow(in, win), cfg, win) {
		if a.ID == "alpha" {
			alpha = a
		}
	}
	if alpha == nil {
		t.Fatal("no alpha aggregate")
	}
	// Both waits count, and neither call completed, so both are abandoned.
	// Runs are numbered from 1 (parseModelStatsZips counts the zips).
	if alpha.SlotCalls != 2 || len(alpha.SlotWaits) != 2 || alpha.Abandoned != 2 || alpha.slotWaitsPerRun[1] != 2 {
		t.Fatalf("slot figures: calls=%d waits=%d abandoned=%d perRun=%d, want 2/2/2/2",
			alpha.SlotCalls, len(alpha.SlotWaits), alpha.Abandoned, alpha.slotWaitsPerRun[1])
	}
	// The 7 s refused wait sorts below the 120 s row, so the p90 is 120.0
	// (nearest rank: sorted[ceil(0.9*2)-1] over [7, 120]).
	if summarize(alpha.SlotWaits).P90 != 120.0 {
		t.Fatalf("slot p90 = %v, want 120", summarize(alpha.SlotWaits).P90)
	}
	got := modelStatsReport(in, cfg, win, now)
	// The per-model row: n and runs from the one call row, then a slot
	// figure of 2 counting the refused call. The tail columns are
	// | slot waits | slot p90 | abandoned |.
	if !strings.Contains(got, "| `alpha` | 1 | 1 |") || !strings.Contains(got, "| 2 | 120.0 | 2 |") {
		t.Errorf("the waited-and-refused calls are missing from the slot figures:\n%s", got)
	}
	if want := "`alpha`: 2 slot waits in run 1, over the 1-per-run threshold"; !strings.Contains(got, want) {
		t.Errorf("the per-run slot-wait alert did not count the refused wait:\n%s", got)
	}
	if want := "`alpha`: 2 abandoned slot wait(s) \u2014 a call queued for a slot and produced no completion"; !strings.Contains(got, want) {
		t.Errorf("the abandoned alert did not count the waited refusals:\n%s", got)
	}
}

// Issue #456: the proxy now writes a marker for a call abandoned before it
// produced anything, and the report has to be able to see it -- that is the
// whole point of recording it. A queue abandon feeds the slot figures and the
// abandoned alert; a pre-header abandon is counted as its own stage. Neither
// is a refusal: an abandoned marker must not widen the 429-rate denominator
// or add an "other" error.
func TestModelStatsCountsAbandonedCalls(t *testing.T) {
	cfg := modelStatsTestConfigWith(t, "alpha")
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	win := modelStatsWindowFrom(cfg, now)
	lines := []string{
		// One real call that completed, so the model has a call row.
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":100,"usage_seen":true,"ttft_ms":1000,"dur_ms":2000}`,
		// The queue abandon: 40 s waiting for a slot, stage before_start.
		`{"ts":"2026-10-21T11:01:00Z","model":"alpha","kind":"abandoned","abandoned":"before_start","slot_wait_s":40}`,
		// The pre-header abandon: 90 s of header wait, stage before_headers.
		`{"ts":"2026-10-21T11:02:00Z","model":"alpha","kind":"abandoned","abandoned":"before_headers","header_ms":90000}`,
	}
	zr := modelStatsTestZip(t, "codeaf-ledger.jsonl", strings.Join(lines, "\n")+"\n")

	in := parseModelStatsZips([]*zip.Reader{zr}, cfg)
	if in.Stats.Rows != 1 || in.Stats.Rejected != 0 || len(in.Stats.Markers) != 2 {
		t.Fatalf("rows=%d rejected=%d markers=%d, want 1/0/2 (classes=%v)",
			in.Stats.Rows, in.Stats.Rejected, len(in.Stats.Markers), in.Stats.RejectedClass)
	}
	var alpha *modelAgg
	for _, a := range modelAggregate(filterWindow(in, win), cfg, win) {
		if a.ID == "alpha" {
			alpha = a
		}
	}
	if alpha == nil {
		t.Fatal("no alpha aggregate")
	}
	// The abandoned markers are not calls, and they are not errors either.
	if alpha.Calls != 1 || alpha.Errs != 0 || alpha.Four29 != 0 {
		t.Errorf("abandoned markers leaked into the call figures: calls=%d errs=%d 429=%d, want 1/0/0",
			alpha.Calls, alpha.Errs, alpha.Four29)
	}
	// The queue abandon carries a measured wait, so it reaches the slot
	// figures and counts abandoned.
	if alpha.SlotCalls != 1 || alpha.Abandoned != 1 || alpha.slotWaitsPerRun[1] != 1 {
		t.Errorf("queue abandon: slotCalls=%d abandoned=%d perRun=%d, want 1/1/1",
			alpha.SlotCalls, alpha.Abandoned, alpha.slotWaitsPerRun[1])
	}
	if alpha.AbandonedHeaders != 1 {
		t.Errorf("pre-header abandon: abandonedHeaders=%d, want 1", alpha.AbandonedHeaders)
	}
	got := modelStatsReport(in, cfg, win, now)
	if want := "`alpha`: 1 abandoned slot wait(s) \u2014 a call queued for a slot and produced no completion"; !strings.Contains(got, want) {
		t.Errorf("the abandoned-wait alert did not see the queue abandon:\n%s", got)
	}
	if want := "`alpha`: 1 call(s) abandoned before upstream headers"; !strings.Contains(got, want) {
		t.Errorf("the pre-header abandon is not reported:\n%s", got)
	}
}

// TestModelStatsRejectsUnknownAbandonStage: the stage decides which wait the
// marker is evidence for, so an unfamiliar one is refused rather than folded
// into the abandon figures.
func TestModelStatsRejectsUnknownAbandonStage(t *testing.T) {
	cfg := modelStatsTestConfigWith(t, "alpha")
	zr := modelStatsTestZip(t, "codeaf-ledger.jsonl",
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","kind":"abandoned","abandoned":"midnight","slot_wait_s":5}`+"\n")
	in := parseModelStatsZips([]*zip.Reader{zr}, cfg)
	if in.Stats.Rows != 0 || in.Stats.Rejected != 1 || len(in.Stats.Markers) != 0 {
		t.Fatalf("rows=%d rejected=%d markers=%d, want 0/1/0", in.Stats.Rows, in.Stats.Rejected, len(in.Stats.Markers))
	}
	if in.Stats.RejectedClass[rejectUnknownKind] != 1 {
		t.Errorf("refused under %v, want %v", in.Stats.RejectedClass, rejectUnknownKind)
	}
}
