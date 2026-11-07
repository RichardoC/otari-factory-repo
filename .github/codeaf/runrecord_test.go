package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeRecordRun(t *testing.T, dir, runID string, rec RunRecord, withRecord bool) {
	t.Helper()
	meta := RunMeta{Version: stateVersion, RunID: runID, Attempt: "1", BaseGeneration: 0, Date: rec.Date}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, metaFile), data)
	if !withRecord {
		return
	}
	data, err = json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, runRecordFile), data)
}

func TestRunRecordFromRun(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	result := `{"stop":"incomplete","ok":false}`
	trajectory := `{"kind":"step"}` + "\n" + `{"kind":"end","reason":"finished in the store","result":"all done"}` + "\n"
	ledger := strings.Join([]string{
		`{"ts":"2026-10-16T00:00:00Z","kind":"proxy_start"}`,
		`{"ts":"2026-10-16T00:01:00Z","start":"2026-10-16T00:00:55Z","prompt_tokens":100,"completion_tokens":10,"cost_usd":0.01,"api_cost_usd":0.02,"slot_wait_s":5}`,
		`{"ts":"2026-10-16T00:02:00Z","start":"2026-10-16T00:01:55Z","prompt_tokens":200,"completion_tokens":20,"cost_usd":0.02,"api_cost_usd":0.04}`,
		`{"ts":"2026-10-16T00:03:00Z","kind":"search","query_len":5,"results":3}`,
		`{"ts":"2026-10-16T00:04:00Z","kind":"status","status":429}`,
		`{"ts":"2026-10-16T00:05:00Z","kind":"status","status":500}`,
		`{"ts":"2026-10-16T00:06:00Z","kind":"unknown-kind","cost_usd":9}`,
	}, "\n") + "\n"
	patch := "diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml\n--- a/.github/workflows/ci.yml\n+++ b/.github/workflows/ci.yml\n@@ -1,1 +1,2 @@\n-a\n+b\n+c\ndiff --git a/app/x/main.go b/app/x/main.go\n--- a/app/x/main.go\n+++ b/app/x/main.go\n@@ -1,1 +1,1 @@\n-x\n+y\n"

	for _, tc := range []struct {
		name  string
		env   map[string]string
		files map[string]string
		want  RunRecord
	}{
		{
			name: "full",
			env: map[string]string{
				"MODE": "issue", "NUMBER": "501", "LANE": "3",
				"GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "2",
			},
			files: map[string]string{
				"codeaf-result.json":      result,
				"codeaf-trajectory.jsonl": trajectory,
				"ledger.jsonl":            ledger,
				"changes.patch":           patch,
			},
			want: RunRecord{
				RunID: "123", Attempt: "2", Mode: "issue", Lane: 3, Number: 501,
				Stop: "done", StoreFinished: true,
				OK:               func() *bool { f := false; return &f }(),
				Calls:            2,
				Searches:         1,
				SlotWaitSeconds:  5,
				HeartbeatCalls:   1,
				APICostUSD:       0.06,
				PromptTokens:     300,
				CompletionTokens: 30,
				Rate429:          1,
				CostUSD:          9.03, // the unknown-kind line costs money, as ledgerTotal counts it
				FilesChanged:     2,
				PatchBytes:       len(patch),
				TouchesHarness:   true,
				Date:             "2026-10-16",
			},
		},
		{
			name: "no patch, unknown mode, profile makes an experiment",
			env: map[string]string{
				"MODE": "pr", "PROFILE": "glm-next", "NUMBER": "x",
				"GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "1",
			},
			files: map[string]string{"ledger.jsonl": ledger},
			want: RunRecord{
				RunID: "123", Attempt: "1", Mode: "experiment", Number: 0,
				Stop: stopUnknown, CostUSD: 9.03, APICostUSD: 0.06,
				Calls: 2, Searches: 1, SlotWaitSeconds: 5, HeartbeatCalls: 1,
				PromptTokens: 300, CompletionTokens: 30, Rate429: 1,
				Date: "2026-10-16",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := t.TempDir()
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN", "GITHUB_REPOSITORY", "TRAJECTORY_PATH"} {
				t.Setenv(name, "")
			}
			t.Setenv("RESULT_PATH", filepath.Join(out, "codeaf-result.json"))
			t.Setenv("LEDGER_PATH", filepath.Join(out, "ledger.jsonl"))
			for name, data := range tc.files {
				writeTestFile(t, filepath.Join(out, name), []byte(data))
			}
			got := runRecordFromRun(now)
			got.WallSeconds = tc.want.WallSeconds // runWallTime asks the GitHub API; no figure in a test
			if !reflect.DeepEqual(got, tc.want) {
				gj, _ := json.Marshal(got)
				wj, _ := json.Marshal(tc.want)
				t.Errorf("record = %s\nwant   = %s", gj, wj)
			}
			if err := got.validate(); err != nil {
				t.Errorf("validate: %v", err)
			}
		})
	}

	// A result the run job never wrote, and a ledger that is not there:
	// every computable field stays zero, the stop falls back to unknown,
	// and packing still works.
	for name, value := range map[string]string{"GITHUB_TOKEN": "", "GH_TOKEN": "", "GITHUB_REPOSITORY": "", "TRAJECTORY_PATH": ""} {
		t.Setenv(name, value)
	}
	t.Setenv("RESULT_PATH", filepath.Join(t.TempDir(), "absent", "codeaf-result.json"))
	t.Setenv("LEDGER_PATH", filepath.Join(t.TempDir(), "absent.jsonl"))
	t.Setenv("MODE", "")
	t.Setenv("PROFILE", "")
	t.Setenv("GITHUB_RUN_ID", "123")
	t.Setenv("GITHUB_RUN_ATTEMPT", "1")
	got := runRecordFromRun(now)
	want := RunRecord{RunID: "123", Attempt: "1", Mode: "unknown", Stop: stopUnknown, Date: "2026-10-16"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("absent-everything record = %+v, want %+v", got, want)
	}
}

func TestRunRecordValidation(t *testing.T) {
	ok := RunRecord{RunID: "1", Attempt: "1", Mode: "issue", Stop: stopDone, Date: "2026-10-16"}
	if err := ok.validate(); err != nil {
		t.Fatalf("valid record: %v", err)
	}
	for _, tc := range []struct {
		name string
		bad  RunRecord
	}{
		{"lane over 99", RunRecord{RunID: "1", Attempt: "1", Mode: "issue", Lane: 100, Stop: stopDone, Date: "2026-10-16"}},
		{"negative lane", RunRecord{RunID: "1", Attempt: "1", Mode: "issue", Lane: -1, Stop: stopDone, Date: "2026-10-16"}},
		{"negative cost", RunRecord{RunID: "1", Attempt: "1", Mode: "issue", Stop: stopDone, CostUSD: -0.01, Date: "2026-10-16"}},
		{"unknown mode", RunRecord{RunID: "1", Attempt: "1", Mode: "issue; rm -rf /", Stop: stopDone, Date: "2026-10-16"}},
		{"unknown stop", RunRecord{RunID: "1", Attempt: "1", Mode: "issue", Stop: "$(id)", Date: "2026-10-16"}},
		{"wall over a day", RunRecord{RunID: "1", Attempt: "1", Mode: "issue", Stop: stopDone, WallSeconds: 90000, Date: "2026-10-16"}},
		{"number over the cap", RunRecord{RunID: "1", Attempt: "1", Mode: "issue", Number: 10_000_001, Stop: stopDone, Date: "2026-10-16"}},
		{"bad run id", RunRecord{RunID: "1; rm", Attempt: "1", Mode: "issue", Stop: stopDone, Date: "2026-10-16"}},
		{"bad date", RunRecord{RunID: "1", Attempt: "1", Mode: "issue", Stop: stopDone, Date: "yesterday"}},
	} {
		if err := tc.bad.validate(); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}

	// A valid record round-trips through records.jsonl: written as one
	// JSON line, read back equal.
	dir := t.TempDir()
	f := Files{Records: []RunRecord{ok}}
	if err := f.write(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, recordsFile))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n"); len(lines) != 1 {
		t.Fatalf("%d lines in records.jsonl", len(lines))
	}
	read, dropped := CleanRecords(data)
	if dropped != 0 || len(read) != 1 || read[0] != ok {
		t.Fatalf("round-trip: %+v dropped %d", read, dropped)
	}

	// A record file over the 16 KiB cap is refused, not read.
	big := t.TempDir()
	padded := ok
	data, _ = json.Marshal(padded)
	writeTestFile(t, filepath.Join(big, runRecordFile), append(data, []byte(strings.Repeat(" ", 20<<10))...))
	if _, err := readRunRecord(big); err == nil {
		t.Error("a 20 KiB record file was read")
	}

	// A field out of bounds in the packed file rejects the record.
	bad := t.TempDir()
	padded.Lane = 100
	data, _ = json.Marshal(padded)
	writeTestFile(t, filepath.Join(bad, runRecordFile), data)
	if _, err := readRunRecord(bad); err == nil {
		t.Error("an out-of-bounds record was read")
	}

	// A missing record is not an error: older packs merge as before.
	if rec, err := readRunRecord(t.TempDir()); err != nil || !reflect.DeepEqual(rec, RunRecord{}) {
		t.Errorf("missing record: (%+v, %v)", rec, err)
	}
}

func TestStateMergeAppendsRecordsAndCaps(t *testing.T) {
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	empty := filepath.Join(t.TempDir(), "no-bundle")
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("GITHUB_OUTPUT", filepath.Join(t.TempDir(), "output"))

	var dirs []string
	for i := 1; i <= 2010; i++ {
		dir := filepath.Join(t.TempDir(), "run-"+strings.Repeat("0", 4-len(strconv.Itoa(i)))+strconv.Itoa(i))
		writeRecordRun(t, dir, strconv.Itoa(i), RunRecord{
			RunID: strconv.Itoa(i), Attempt: "1", Mode: "issue", Stop: stopDone, Date: "2026-10-16",
		}, true)
		dirs = append(dirs, dir)
	}
	// A run packed before the record existed merges as before, without one.
	noRec := t.TempDir()
	writeRecordRun(t, noRec, "9999", RunRecord{}, false)
	dirs = append(dirs, noRec)

	if err := stateMerge(empty, out, dirs, cfg, now); err != nil {
		t.Fatal(err)
	}
	m := readManifestT(t, out, cfg, now)
	if len(m.Merged) != maxMergedRuns {
		t.Fatalf("%d runs merged, want the %d cap", len(m.Merged), maxMergedRuns)
	}
	data, err := os.ReadFile(filepath.Join(out, recordsFile))
	if err != nil {
		t.Fatal(err)
	}
	records, dropped := CleanRecords(data)
	if dropped != 0 {
		t.Errorf("%d record lines dropped", dropped)
	}
	if len(records) != maxRecordLines {
		t.Fatalf("%d records kept, want %d", len(records), maxRecordLines)
	}
	if records[0].RunID != "11" || records[len(records)-1].RunID != "2010" {
		t.Errorf("kept runs %s..%s, want the newest 2000, newest last (11..2010)", records[0].RunID, records[len(records)-1].RunID)
	}
	for _, r := range records {
		if r.RunID == "9999" {
			t.Error("the run without a record grew one")
		}
	}

	// Merging again adds nothing: the run keys dedupe.
	out2 := filepath.Join(t.TempDir(), "out2")
	if err := stateMerge(out, out2, dirs, cfg, now); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(out2, recordsFile))
	records, _ = CleanRecords(data)
	if len(records) != maxRecordLines {
		t.Errorf("re-merge kept %d records, want %d", len(records), maxRecordLines)
	}
}

// TestStateMergeIsIdempotentPastTheCap re-presents a run the manifest window
// has already forgotten (issue #312): the merge must recognise it by its key,
// leave the day's spend alone and not advance the generation for it.
func TestStateMergeIsIdempotentPastTheCap(t *testing.T) {
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	// One UTC day of maxMergedRuns+1 runs is far past the read-time day
	// ceiling (issue #311), which would cap the figure and hide a double
	// count; a weekly credit above it keeps the spend checks exact here.
	cfg.Limits.WeeklyCreditUSD = 10 * maxMergedRuns
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	empty := filepath.Join(t.TempDir(), "no-bundle")
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("GITHUB_OUTPUT", filepath.Join(t.TempDir(), "output"))
	writeRun := func(runID string) string {
		dir := t.TempDir()
		meta := RunMeta{Version: stateVersion, RunID: runID, Attempt: "1", BaseGeneration: 0, Date: "2026-10-16", RunUSD: 1}
		data, err := json.MarshalIndent(meta, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(dir, metaFile), data)
		return dir
	}

	var dirs []string
	for i := 1; i <= maxMergedRuns+1; i++ {
		dirs = append(dirs, writeRun(strconv.Itoa(i)))
	}
	first := dirs[0]
	if err := stateMerge(empty, out, dirs, cfg, now); err != nil {
		t.Fatal(err)
	}
	m := readManifestT(t, out, cfg, now)
	if len(m.Merged) != maxMergedRuns {
		t.Fatalf("%d runs merged, want the %d cap", len(m.Merged), maxMergedRuns)
	}
	if got := m.Spend["2026-10-16"]; got != float64(maxMergedRuns+1) {
		t.Fatalf("spend after the first merge = %v, want %d", got, maxMergedRuns+1)
	}
	// The window has forgotten run 1, which is exactly the trap.
	for _, key := range m.Merged {
		if key == "1-1" {
			t.Fatal("run 1-1 is still in the manifest window; the test proves nothing")
		}
	}

	// Re-present the forgotten run alongside a genuinely new one.
	fresh := writeRun("9999")
	out2 := filepath.Join(t.TempDir(), "out2")
	if err := stateMerge(out, out2, []string{first, fresh}, cfg, now); err != nil {
		t.Fatal(err)
	}
	again := readManifestT(t, out2, cfg, now)
	if got, want := again.Spend["2026-10-16"], m.Spend["2026-10-16"]+1; got != want {
		t.Errorf("spend after re-merging run 1-1 = %v, want %v (its dollars counted once)", got, want)
	}
	if got, want := again.Generation, m.Generation+1; got != want {
		t.Errorf("generation after re-merging run 1-1 = %d, want %d (only run 9999 advanced it)", got, want)
	}
	if again.Updated.IsZero() {
		t.Error("the repeat merge did not write the bundle")
	}

	// The dedup set outlives the manifest window.
	keys, err := readMergedKeys(out2)
	if err != nil {
		t.Fatalf("merged keys: %v", err)
	}
	found := false
	for _, key := range keys {
		if key == "1-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("run 1-1 is not in %s; the dedup set forgot it again", mergedFile)
	}

	// The repeat alone, one last time: a re-presented run changes nothing
	// at all — neither the day's spend nor the generation (issue #312 Done
	// when: "m.Spend for that day is unchanged and m.Generation did not
	// advance for the repeat").
	out3 := filepath.Join(t.TempDir(), "out3")
	if err := stateMerge(out2, out3, []string{first}, cfg, now); err != nil {
		t.Fatal(err)
	}
	lone := readManifestT(t, out3, cfg, now)
	if got, want := lone.Spend["2026-10-16"], again.Spend["2026-10-16"]; got != want {
		t.Errorf("spend after a lone re-merge of run 1-1 = %v, want %v unchanged", got, want)
	}
	if got, want := lone.Generation, again.Generation; got != want {
		t.Errorf("generation after a lone re-merge of run 1-1 = %d, want %d unchanged", got, want)
	}
}

func TestReadMergedKeysRefusesBsdKeys(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, mergedFile), []byte("1-1\n20; rm -rf /\n"))
	if _, err := readMergedKeys(dir); err == nil {
		t.Error("a malformed merged key was accepted")
	}
	// A missing file is an empty set: bundles from before issue #312 keep merging.
	if keys, err := readMergedKeys(t.TempDir()); err != nil || len(keys) != 0 {
		t.Errorf("missing %s: (%v, %v)", mergedFile, keys, err)
	}
	// A set well under the byte bound is kept whole. A set over it is
	// TestTrimMergedKeysBoundsTheSet, which actually crosses
	// maxMergedKeyBytes; a 1000-key set is only a few kilobytes and would
	// leave that loop unexecuted (issue #312 review).
	many := make([]string, 0, 1000)
	for i := 0; i < 1000; i++ {
		many = append(many, strconv.Itoa(i)+"-1")
	}
	if trimmed := trimMergedKeys(many); len(trimmed) != len(many) {
		t.Errorf("a %d-key set was trimmed to %d keys; only sets past the %d-byte bound are", len(many), len(trimmed), maxMergedKeyBytes)
	}
}

func TestStateLoadDropsBadRecordLines(t *testing.T) {
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	bundle := t.TempDir()
	valid := func(id string) string {
		data, _ := json.Marshal(RunRecord{RunID: id, Attempt: "1", Mode: "issue", Stop: stopDone, Date: "2026-10-16"})
		return string(data)
	}
	records := strings.Join([]string{valid("1"), `{"mode":"$(id)"}`, "not json at all", valid("2")}, "\n") + "\n"
	writeTestFile(t, filepath.Join(bundle, recordsFile), []byte(records))
	manifest := `{"version":1,"generation":3,"merged":[],"updated":"2026-10-16T11:00:00Z","spend":{}}`
	writeTestFile(t, filepath.Join(bundle, manifestFile), []byte(manifest))

	var logs strings.Builder
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	home := t.TempDir()
	if err := stateLoad(bundle, home, cfg, now); err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(logs.String(), "dropped 2 invalid run-record lines") {
		t.Errorf("log = %q, want the two bad lines counted", logs.String())
	}
	// The two valid records survive, and every run's log shows the shape.
	data, err := os.ReadFile(filepath.Join(home, recordsFile))
	if err != nil {
		t.Fatal(err)
	}
	if kept, dropped := CleanRecords(data); dropped != 0 || len(kept) != 2 || kept[0].RunID != "1" || kept[1].RunID != "2" {
		t.Errorf("home records = %+v dropped %d", kept, dropped)
	}
	if !strings.Contains(logs.String(), "records: 2 (last 7 days: issue 2 done/0 incomplete, review 0, plan 0)") {
		t.Errorf("log = %q, want the records shape line", logs.String())
	}
}

func TestStatePackWritesRunRecord(t *testing.T) {
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	empty := filepath.Join(t.TempDir(), "no-bundle")
	t.Setenv("LEDGER_PATH", filepath.Join(t.TempDir(), "none.jsonl"))
	out := pack(t, cfg, now, empty, "77", 0.25, func(home string) {
		t.Setenv("MODE", "pr")
		t.Setenv("NUMBER", "501")
		t.Setenv("LANE", "2")
	})
	rec, err := readRunRecord(out)
	if err != nil {
		t.Fatalf("packed record: %v", err)
	}
	if rec.RunID != "77" || rec.Mode != "pr" || rec.Number != 501 || rec.Lane != 2 || rec.Stop != stopUnknown || rec.Date != "2026-10-16" {
		t.Errorf("packed record = %+v", rec)
	}
}

func TestMetricsSummary(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	rec := func(mode, stop, date string, cost, wall float64, n429, number int, store bool) RunRecord {
		return RunRecord{RunID: "1", Attempt: "1", Mode: mode, Number: number, Stop: stop,
			StoreFinished: store, WallSeconds: wall, CostUSD: cost, Rate429: n429, Date: date}
	}
	records := []RunRecord{
		rec("issue", stopDone, "2026-10-15", 0.5, 100, 1, 0, false),
		rec("issue", stopDone, "2026-10-15", 1.25, 200, 1, 0, false),
		rec("issue", stopIncomplete, "2026-10-15", 0.1, 1300, 0, 0, false),
		rec("issue", stopDone, "2026-10-16", 0, 0, 0, 0, true),
		rec("pr", stopDone, "2026-10-16", 1, 60, 0, 501, false),
		rec("pr", stopDone, "2026-10-16", 1, 0, 0, 501, true),
		rec("pr", stopDone, "2026-10-16", 1, 0, 1, 502, false),
		rec("pr", stopDone, "2026-10-16", 1, 0, 0, 503, false),
		rec("review", stopDone, "2026-10-16", 0.25, 0, 0, 0, false),
		rec("review", stopDone, "2026-10-16", 0.25, 0, 0, 0, false),
		// Outside the 7-day window: counted nowhere. The stray 429s and
		// the second 501 would show if the filter leaked.
		rec("issue", stopDone, "2026-10-09", 9, 0, 5, 0, false),
		rec("pr", stopDone, "2026-10-09", 9, 0, 0, 777, false),
	}
	records[0].SlotWaitSeconds = 30
	got := metricsSummary(records, 7, now)
	for _, want := range []string{
		"## Run metrics: 10 runs in the last 7 days (through 2026-10-16)",
		"| mode | done | incomplete | total |",
		"| issue | 3 | 1 | 4 |",
		"| pr | 4 | 0 | 4 |",
		"| review | 2 | 0 | 2 |",
		"Store-finished runs: 2",
		"| issue | 3 | 8m53s | 3m20s |",
		"| pr | 1 | 1m0s | 1m0s |",
		"| issue | 4 | $0.4625 |",
		"| pr | 4 | $1.0000 |",
		"| review | 2 | $0.2500 |",
		"| 2026-10-15 | $1.8500 |",
		"| 2026-10-16 | $4.5000 |",
		"Upstream 429s: 3",
		"Slot wait total: 30s",
		"PRs with more than one `pr`-mode run: 1 (501)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary is missing %q\n%s", want, got)
		}
	}
	// The out-of-window records appear nowhere.
	for _, want := range []string{"2026-10-09", "Upstream 429s: 8", "12 runs", "777"} {
		if strings.Contains(got, want) {
			t.Errorf("summary contains %q, which is outside the window\n%s", want, got)
		}
	}
	// A one-day window keeps only today's runs.
	if got := metricsSummary(records, 1, now); !strings.Contains(got, "7 runs") || strings.Contains(got, "2026-10-15") {
		t.Errorf("one-day summary =\n%s", got)
	}
}

func TestLedgerRecordsUpstreamStatus(t *testing.T) {
	m := mustResolve(t, testModels(t), "zai-org/glm-5.3-flash")
	p := &proxy{ledger: filepath.Join(t.TempDir(), "l.jsonl")}
	usage := Usage{PromptTokens: 10, CompletionTokens: 1}
	p.record(m, &usage, false, false, time.Now(), 0, 0, charCounts{}, 0, 0, callTiming{}, 0)
	p.record(m, nil, false, true, time.Now(), 0, 0, charCounts{}, 0, 429, callTiming{}, 0)
	p.record(m, nil, false, false, time.Now(), 0, 0, charCounts{}, 0, 500, callTiming{}, 0)
	lines, err := readLedger(p.ledger)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("%d ledger lines", len(lines))
	}
	if lines[0].Status != 0 || lines[0].Kind != "" {
		t.Errorf("a 200 call recorded status %d kind %q", lines[0].Status, lines[0].Kind)
	}
	if lines[1].Kind != "status" || lines[1].Status != 429 || lines[1].CostUSD != 0 {
		t.Errorf("429 line = kind %q status %d cost %v", lines[1].Kind, lines[1].Status, lines[1].CostUSD)
	}
	if lines[2].Status != 500 {
		t.Errorf("500 line = status %d", lines[2].Status)
	}
}

// A run contributes only its own record: a records.jsonl packed in its
// artifact, or a record naming another run, never reaches the bundle.
func TestStateMergeTakesOnlyTheRunsOwnRecord(t *testing.T) {
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	empty := filepath.Join(t.TempDir(), "no-bundle")
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("GITHUB_OUTPUT", filepath.Join(t.TempDir(), "output"))

	flood := filepath.Join(t.TempDir(), "flood")
	writeRecordRun(t, flood, "1", RunRecord{RunID: "1", Attempt: "1", Mode: "issue", Stop: stopDone, Date: "2026-10-16"}, true)
	var lines []byte
	for i := 100; i < 110; i++ {
		data, _ := json.Marshal(RunRecord{RunID: strconv.Itoa(i), Attempt: "1", Mode: "issue", Stop: stopDone, Date: "2026-10-16"})
		lines = append(append(lines, data...), '\n')
	}
	writeTestFile(t, filepath.Join(flood, recordsFile), lines)

	impostor := filepath.Join(t.TempDir(), "impostor")
	writeRecordRun(t, impostor, "2", RunRecord{RunID: "3", Attempt: "1", Mode: "issue", Stop: stopDone, Date: "2026-10-16"}, true)

	if err := stateMerge(empty, out, []string{flood, impostor}, cfg, now); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, recordsFile))
	if err != nil {
		t.Fatal(err)
	}
	records, _ := CleanRecords(data)
	if len(records) != 1 || records[0].RunID != "1" {
		t.Errorf("records = %+v, want only run 1's own", records)
	}
}

// TestTrimMergedKeysBoundsTheSet drives the trim loop with a set that really
// crosses maxMergedKeyBytes — the handful-of-kilobytes sets every other test
// builds never execute it — and checks it drops the oldest keys and keeps the
// newest: the direction the bundle handed on depends on (issue #312 review).
func TestTrimMergedKeysBoundsTheSet(t *testing.T) {
	keys := []string{}
	total := 0
	for i := 1; total <= maxMergedKeyBytes; i++ {
		if i > 1_000_000 {
			t.Fatalf("the key set never reached the %d-byte bound", maxMergedKeyBytes)
		}
		key := strconv.Itoa(i) + "-1"
		keys = append(keys, key)
		total += len(key) + 1
	}

	trimmed := trimMergedKeys(keys)
	if len(trimmed) == len(keys) {
		t.Fatalf("a %d-byte set stayed %d keys; a set past the %d-byte bound must shrink", total, len(keys), maxMergedKeyBytes)
	}
	kept := 0
	for _, key := range trimmed {
		kept += len(key) + 1
	}
	if kept > maxMergedKeyBytes {
		t.Errorf("trimmed set is %d bytes, still over the %d bound", kept, maxMergedKeyBytes)
	}
	// Newest last, and the survivors are the input's tail unchanged — a
	// trim that kept the oldest head instead would fail both.
	if trimmed[len(trimmed)-1] != keys[len(keys)-1] || trimmed[0] != keys[len(keys)-len(trimmed)] {
		t.Errorf("trim kept %q..%q, want the input tail %q..%q", trimmed[0], trimmed[len(trimmed)-1],
			keys[len(keys)-len(trimmed)], keys[len(keys)-1])
	}

	// The bound holds at the write seam: whatever path writes a bundle, the
	// file it hands on must read back on the next pass.
	dir := t.TempDir()
	if err := writeMergedKeys(dir, keys); err != nil {
		t.Fatalf("writeMergedKeys: %v", err)
	}
	round, err := readMergedKeys(dir)
	if err != nil {
		t.Fatalf("%v: an over-bound set was written unreadable (the write seam must trim)", err)
	}
	if len(round) != len(trimmed) || round[len(round)-1] != keys[len(keys)-1] {
		t.Errorf("round trip kept %d keys, newest %q; want %d keys, newest %q",
			len(round), round[len(round)-1], len(trimmed), keys[len(keys)-1])
	}
}

// TestStateMergeHandsBackABoundedKeyFile covers the nothing-to-merge path with
// a bundle whose dedup set already fills the byte bound: seeding its legacy
// manifest window pushes the set past the bound, and what the merge hands
// back must still be readable on the next pass (issue #312 review; this was
// written untrimmed, a file readMergedKeys then refused).
func TestStateMergeHandsBackABoundedKeyFile(t *testing.T) {
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	legacy := t.TempDir()
	// A legacy bundle whose key file still fits (readMergedKeys refuses
	// anything bigger), filled right up to the bound. Run ids climb, so
	// the newest key is last.
	var b strings.Builder
	total, lastKey := 0, ""
	for i := 1; ; i++ {
		key := strconv.Itoa(i) + "-1"
		if total+len(key)+1 > maxMergedKeyBytes {
			break
		}
		b.WriteString(key)
		b.WriteByte('\n')
		total += len(key) + 1
		lastKey = key
	}
	writeTestFile(t, filepath.Join(legacy, mergedFile), []byte(b.String()))
	// The window knows two runs the file has not recorded; one is
	// re-presented, so nothing merges and the bundle is handed back.
	writeTestFile(t, filepath.Join(legacy, manifestFile),
		[]byte(`{"version":1,"generation":3,"merged":["9000001-1","9000002-1"],"updated":"2026-10-16T11:00:00Z","spend":{"2026-10-16":1.25}}`))
	run := t.TempDir()
	writeRecordRun(t, run, "9000001", RunRecord{RunID: "9000001", Attempt: "1", Mode: "issue", Stop: stopDone, Date: "2026-10-16"}, true)
	t.Setenv("GITHUB_OUTPUT", filepath.Join(t.TempDir(), "output"))

	out := filepath.Join(t.TempDir(), "out")
	if err := stateMerge(legacy, out, []string{run}, cfg, now); err != nil {
		t.Fatal(err)
	}
	keys, err := readMergedKeys(out)
	if err != nil {
		t.Fatalf("the handed-back bundle was refused by its own %s: %v", mergedFile, err)
	}
	if got, bound := keysBytes(keys), maxMergedKeyBytes; got > bound {
		t.Errorf("handed-back key set is %d bytes, over the %d bound", got, bound)
	}
	// The seeded window keys sit at the front of the set, so under the
	// bound they are the first the trim drops; they stay in m.Merged, which
	// reseeds them on the next merge. What must never be dropped is the
	// newest: the last file key.
	if len(keys) == 0 || keys[len(keys)-1] != lastKey {
		t.Errorf("last key = %q, want the newest file key %q", lastOf(keys), lastKey)
	}
	windowed := false
	for _, key := range readManifestT(t, out, cfg, now).Merged {
		if key == "9000001-1" {
			windowed = true
		}
	}
	if !windowed {
		t.Error("the window key that was trimmed out of the key file is gone from m.Merged too")
	}
	m := readManifestT(t, out, cfg, now)
	if m.Generation != 3 || m.Spend["2026-10-16"] != 1.25 {
		t.Errorf("no-op merge changed the bundle: %+v", m)
	}
}

// keysBytes totals a key set the way trimMergedKeys measures it: one key,
// one newline.
func keysBytes(keys []string) (total int) {
	for _, key := range keys {
		total += len(key) + 1
	}
	return total
}

func lastOf(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	return keys[len(keys)-1]
}

// TestMergeRecordsCap covers the cap on the bundle's run-record history
// (maxRecordLines, issue #140) at its seam: re-merging a bundle with records it
// already has adds none of them twice — the dedup-by-key the double-spend fix
// of issue #312 rests on — and the newest maxRecordLines are what stay.
// The name matches the issue #312 end-to-end check, which runs
// `go test -run "TestStateMerge|TestMergeRecordsCap"`.
func TestMergeRecordsCap(t *testing.T) {
	rec := func(id string) RunRecord {
		return RunRecord{RunID: id, Attempt: "1", Mode: "issue", Stop: stopDone, Date: "2026-10-16"}
	}
	base := make([]RunRecord, 0, maxRecordLines)
	for i := 1; i <= maxRecordLines; i++ {
		base = append(base, rec(strconv.Itoa(i)))
	}
	// Run 1 again plus two new runs: appended once, then capped at the
	// newest maxRecordLines.
	out := MergeRecords(base, []RunRecord{rec("1"), rec("2001"), rec("2002")})
	if len(out) != maxRecordLines {
		t.Fatalf("%d records, want the %d cap (2002 kept + capped, not 2003)", len(out), maxRecordLines)
	}
	if out[len(out)-1].RunID != "2002" || out[len(out)-2].RunID != "2001" {
		t.Errorf("newest records = ...%s, %s; want the new runs last", out[len(out)-2].RunID, out[len(out)-1].RunID)
	}
	if out[0].RunID != "3" {
		t.Errorf("oldest kept = %s, want 3 (run 1 deduped, 2 dropped by the cap)", out[0].RunID)
	}
	// Re-merging a bundle with itself must change nothing.
	same := MergeRecords(base, base)
	if len(same) != maxRecordLines || same[len(same)-1].RunID != "2000" {
		t.Errorf("re-merge: %d records, newest %s; want %d, 2000", len(same), lastOfRunRecords(same), maxRecordLines)
	}
}

func lastOfRunRecords(rs []RunRecord) string {
	if len(rs) == 0 {
		return ""
	}
	return rs[len(rs)-1].RunID
}

func TestRecordShape(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	rec := func(mode, stop, date string) RunRecord {
		return RunRecord{RunID: "1", Attempt: "1", Mode: mode, Stop: stop, Date: date}
	}

	// Records with issue, pr and review runs that ended done or
	// incomplete: only the issue records may feed the done/incomplete
	// figures, and only inside the seven-day window (2026-10-10 .. today).
	records := []RunRecord{
		rec("issue", stopDone, "2026-10-16"),
		rec("issue", stopDone, "2026-10-10"),
		rec("issue", stopIncomplete, "2026-10-14"),
		rec("issue", stopBudget, "2026-10-15"), // not done/incomplete
		rec("issue", stopDone, "2026-10-09"),   // before the window
		rec("issue", stopDone, "2026-10-17"),   // after today
		rec("pr", stopDone, "2026-10-15"),
		rec("pr", stopIncomplete, "2026-10-16"),
		rec("pr", stopDone, "2026-10-09"), // outside the window either way
		rec("review", stopDone, "2026-10-13"),
		rec("review", stopIncomplete, "2026-10-16"),
		rec("plan", stopIncomplete, "2026-10-12"),
		rec("experiment", stopDone, "2026-10-16"),
	}
	want := "issue 2 done/1 incomplete, review 2, plan 1"
	if got := recordShape(records, now); got != want {
		t.Errorf("recordShape = %q, want %q", got, want)
	}

	if got, want := recordShape(nil, now), "issue 0 done/0 incomplete, review 0, plan 0"; got != want {
		t.Errorf("recordShape(nil) = %q, want %q", got, want)
	}
}
