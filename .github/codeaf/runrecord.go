package main

// Run records (issue #140): one bounded, validated line per run in the shared
// state bundle, and `harness metrics` to summarise them. Nothing here answers
// how the factory is doing until a run is packed and merged; the record is
// written by `harness state pack` (the run job, after CodeAF) and merged by
// `harness state merge` (the codeaf-state workflow, the bundle's only writer).
//
// The record is written by the run job, whose files the agent had a shell
// over, so it is untrusted input: every field is bounded on write and
// re-validated on every read, and a record that cannot be believed is left
// out — never the run it belongs to. Like everything in a bundle, a record is
// data: nothing in it is ever executed or interpolated.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	runRecordFile   = "run-record.json"
	recordsFile     = "records.jsonl"
	maxRecordBytes  = 16 << 10 // one packed record
	maxRecordsBytes = 8 << 20  // the bundle's records.jsonl
	maxRecordLines  = 2000     // the bundle keeps the newest 2000 runs
)

// Bounds no real run reaches. Lane and number mirror what the gate can
// dispatch; a wall over a day or a number past ten million is not a figure
// the factory produced.
const (
	maxRecordLane    = 99
	maxRecordNumber  = 10_000_000
	maxRecordSeconds = 86400
	maxRecordCount   = 10_000_000 // calls, searches, heartbeats, files, 429s
	maxRunTokens     = 1_000_000_000_000
)

// recordModes is the canonical mode order, for validation and for the
// metrics tables. "experiment" is any known mode that ran with a profile;
// "unknown" is a mode the gate did not set.
var recordModes = []string{"issue", "pr", "review", "plan", "experiment", "unknown"}

// recordStops is the canonical stop order, from continue.go's knownStops.
var recordStops = []string{
	stopDone, stopError, stopIncomplete, stopBudget, stopTurnCap,
	stopDeadline, stopPrice, stopQuestion, stopUnchecked, stopUnknown,
}

// RunRecord is one run, as the shared state bundle carries it. Every field is
// bounded on read (validate); a field the run job could not compute stays at
// its zero value.
type RunRecord struct {
	RunID   string `json:"run_id"`
	Attempt string `json:"attempt"`
	Mode    string `json:"mode"`
	Lane    int    `json:"lane"`
	Number  int    `json:"number"`
	// Stop is the run's real stop reason (effectiveStop): a run that finished
	// in the store reads done, and StoreFinished says so separately.
	Stop          string `json:"stop"`
	StoreFinished bool   `json:"store_finished"`
	OK            *bool  `json:"ok,omitempty"`
	// WallSeconds is the `run` job's wall time (runWallTime); 0 when it
	// could not be asked.
	WallSeconds float64 `json:"wall_seconds"`
	// Calls counts recorded 200 calls; Searches the search lines; the
	// status lines (upstream refusals) are counted in Rate429 only.
	Calls    int `json:"calls"`
	Searches int `json:"searches"`
	// SlotWaitSeconds sums the calls' queue time; HeartbeatCalls counts the
	// calls that queued at all (slot_wait_s > 0).
	SlotWaitSeconds  float64 `json:"slot_wait_seconds"`
	HeartbeatCalls   int     `json:"heartbeat_calls"`
	CostUSD          float64 `json:"cost_usd"`
	APICostUSD       float64 `json:"api_cost_usd"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	// Rate429 counts status lines with status 429: how often the proxy saw
	// upstream throttle this run.
	Rate429 int `json:"rate_429"`
	// FilesChanged and PatchBytes come from out/changes.patch's diffstat;
	// a run with no patch carries zeros. TouchesHarness is true when the
	// patch reaches under .github/ outside .github/codeaf/ — a change no
	// merged PR should carry without a maintainer's approval.
	FilesChanged   int  `json:"files_changed"`
	PatchBytes     int  `json:"patch_bytes"`
	TouchesHarness bool `json:"touches_harness"`
	// Date is the UTC day the run packed on.
	Date string `json:"date"`
}

func (r RunRecord) key() string { return r.RunID + "-" + r.Attempt }

// validate bounds every field. A record is data from the run job: one field
// out of bounds rejects the whole record.
func (r RunRecord) validate() error {
	switch {
	case !digits(r.RunID):
		return fmt.Errorf("run_id %q is not a run id", r.RunID)
	case !digits(r.Attempt):
		return fmt.Errorf("attempt %q is not an attempt", r.Attempt)
	case !slices.Contains(recordModes, r.Mode):
		return fmt.Errorf("mode %q", r.Mode)
	case r.Lane < 0 || r.Lane > maxRecordLane:
		return fmt.Errorf("lane %d", r.Lane)
	case r.Number < 0 || r.Number > maxRecordNumber:
		return fmt.Errorf("number %d", r.Number)
	case !knownStops[r.Stop] && r.Stop != stopUnknown:
		return fmt.Errorf("stop %q", r.Stop)
	case math.IsNaN(r.WallSeconds) || r.WallSeconds < 0 || r.WallSeconds > maxRecordSeconds:
		return fmt.Errorf("wall_seconds %v", r.WallSeconds)
	case math.IsNaN(r.SlotWaitSeconds) || r.SlotWaitSeconds < 0 || r.SlotWaitSeconds > maxRecordSeconds:
		return fmt.Errorf("slot_wait_seconds %v", r.SlotWaitSeconds)
	case !validUSD(r.CostUSD):
		return fmt.Errorf("cost_usd %v", r.CostUSD)
	case !validUSD(r.APICostUSD):
		return fmt.Errorf("api_cost_usd %v", r.APICostUSD)
	}
	for name, n := range map[string]int{
		"calls": r.Calls, "searches": r.Searches, "heartbeat_calls": r.HeartbeatCalls,
		"rate_429": r.Rate429, "files_changed": r.FilesChanged,
		"prompt_tokens": r.PromptTokens, "completion_tokens": r.CompletionTokens,
	} {
		if n < 0 || n > maxRecordCount && name != "prompt_tokens" && name != "completion_tokens" {
			return fmt.Errorf("%s %d", name, n)
		}
		if (name == "prompt_tokens" || name == "completion_tokens") && (n < 0 || n > maxRunTokens) {
			return fmt.Errorf("%s %d", name, n)
		}
	}
	if r.PatchBytes < 0 || r.PatchBytes > maxPatchRead {
		return fmt.Errorf("patch_bytes %d", r.PatchBytes)
	}
	if _, err := time.Parse(time.DateOnly, r.Date); err != nil {
		return fmt.Errorf("date %q", r.Date)
	}
	return nil
}

// recordMode names this run's mode from the job's env: MODE, with a non-empty
// PROFILE marking an experiment run (the lane ran a candidate config).
func recordMode() string {
	switch envOr("MODE", "") {
	case "issue", "pr", "review", "plan":
		if os.Getenv("PROFILE") != "" {
			return "experiment"
		}
		return envOr("MODE", "")
	}
	return "unknown"
}

// touchesHarness reports whether a diffstat path reaches under .github/
// outside .github/codeaf/ — the harness itself, which only a maintainer's
// approval publishes. A rename's two sides are checked separately.
func touchesHarness(path string) bool {
	for _, side := range strings.Split(path, " → ") {
		side = strings.TrimPrefix(strings.TrimSpace(side), "a/")
		if !strings.HasPrefix(side, ".github/") {
			continue
		}
		if rest := strings.TrimPrefix(side, ".github/"); !strings.HasPrefix(rest, "codeaf/") {
			return true
		}
	}
	return false
}

// runRecordFromRun builds the record for the run whose job environment this
// process carries (MODE, NUMBER, LANE, PROFILE, RESULT_PATH, LEDGER_PATH,
// GITHUB_RUN_ID, GITHUB_RUN_ATTEMPT). A field that cannot be computed is left
// at its zero value; nothing here fails the pack.
func runRecordFromRun(now time.Time) RunRecord {
	r := RunRecord{
		Mode: recordMode(),
		Stop: stopUnknown,
		Date: now.UTC().Format(time.DateOnly),
	}
	if id := envOr("GITHUB_RUN_ID", ""); digits(id) {
		r.RunID = id
	}
	if a := envOr("GITHUB_RUN_ATTEMPT", ""); digits(a) {
		r.Attempt = a
	}
	if n, err := strconv.Atoi(envOr("LANE", "0")); err == nil {
		r.Lane = n
	}
	if n, err := strconv.Atoi(os.Getenv("NUMBER")); err == nil {
		r.Number = n
	}
	// How the run ended: the result envelope, promoted by the run record's
	// closing line when CodeAF finished in the store (issue #134).
	var c closing
	if path := trajectoryPath(); path != "" {
		c = readClosing(path)
	}
	result, _, err := readResult()
	if err != nil {
		result = Result{}
	}
	r.Stop = sanitizeStop(effectiveStop(result, c))
	r.StoreFinished = c.StoreFinished
	r.OK = result.OK
	if d := runWallTime(); d != nil {
		r.WallSeconds = clampSeconds(d.Seconds())
	}
	if lines, err := readLedger(os.Getenv("LEDGER_PATH")); err == nil {
		for _, l := range lines {
			switch l.Kind {
			case "proxy_start":
			case "search":
				r.Searches++ // an old ledger's forwarded-search line
			case "status":
				if l.Status == 429 {
					r.Rate429++
				}
			case "":
				// A recorded call. Unknown kinds stay uncounted: they cost
				// money (ledgerTotal still sums them) but are not calls.
				r.Calls++
				r.Searches += l.Searches // searches Otari ran inside the call (issue #2)
				r.SlotWaitSeconds += clampSeconds(l.SlotWaitS)
				if l.SlotWaitS > 0 {
					r.HeartbeatCalls++
				}
				r.PromptTokens += clampTokens(l.PromptTokens)
				r.CompletionTokens += clampTokens(l.CompletionTokens)
			}
		}
		r.CostUSD = ledgerTotal(lines)
		for _, l := range lines {
			r.APICostUSD += l.APICostUSD
		}
		if !validUSD(r.CostUSD) {
			r.CostUSD = 0
		}
		if !validUSD(r.APICostUSD) {
			r.APICostUSD = 0
		}
	}
	// The patch's diffstat, when the run job wrote one beside the result.
	if dir := filepath.Dir(os.Getenv("RESULT_PATH")); dir != "" && dir != "." {
		if patch, err := readRegular(filepath.Join(dir, "changes.patch"), maxPatchRead); err == nil && patch != nil {
			diffs, more := diffstat(patch)
			r.FilesChanged = len(diffs) + more
			r.PatchBytes = len(patch)
			for _, d := range diffs {
				if touchesHarness(d.Path) {
					r.TouchesHarness = true
				}
			}
		}
	}
	return r
}

// readRunRecord reads a packed run's run-record.json. A missing record is
// (zero, nil): runs packed before this change merge as before. Anything else
// wrong — unreadable, over the cap, a field out of bounds — is an error, and
// the caller leaves the record out without refusing the run.
func readRunRecord(dir string) (RunRecord, error) {
	data, err := readRegular(filepath.Join(dir, runRecordFile), maxRecordBytes)
	if err != nil {
		return RunRecord{}, err
	}
	if data == nil {
		return RunRecord{}, nil
	}
	var r RunRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return RunRecord{}, fmt.Errorf("%s: %w", runRecordFile, err)
	}
	if err := r.validate(); err != nil {
		return RunRecord{}, fmt.Errorf("%s: %w", runRecordFile, err)
	}
	return r, nil
}

// CleanRecords keeps the records in records.jsonl that validate, in order,
// at most the last maxRecordLines. It mirrors CleanEvents: the file is data,
// a hostile line is dropped and counted, never fatal.
func CleanRecords(data []byte) (records []RunRecord, dropped int) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, maxRecordBytes+1), maxRecordBytes+1)
	for scanner.Scan() {
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var r RunRecord
		if json.Unmarshal(raw, &r) != nil || r.validate() != nil {
			dropped++
			continue
		}
		records = append(records, r)
	}
	if scanner.Err() != nil {
		// An over-long line ends the scan; what follows it is dropped.
		dropped++
	}
	if len(records) > maxRecordLines {
		dropped += len(records) - maxRecordLines
		records = records[len(records)-maxRecordLines:]
	}
	return records, dropped
}

// MergeRecords appends the run's records the bundle does not have, by run
// key, keeping the newest maxRecordLines. A packed run carries the bundle's
// records as it loaded them, so the dedup is what keeps a run merged twice
// from counting twice.
func MergeRecords(base, run []RunRecord) []RunRecord {
	seen := make(map[string]bool, len(base))
	for _, r := range base {
		seen[r.key()] = true
	}
	out := slices.Clone(base)
	for _, r := range run {
		if !seen[r.key()] {
			seen[r.key()] = true
			out = append(out, r)
		}
	}
	if len(out) > maxRecordLines {
		out = out[len(out)-maxRecordLines:]
	}
	return out
}

// ---------------------------------------------------------------- metrics

// runMetrics prints a Markdown summary of the bundle's run records:
// `harness metrics BUNDLE [DAYS]`.
func runMetrics() error {
	if len(os.Args) < 3 || len(os.Args) > 4 {
		return errors.New("usage: harness metrics BUNDLE [DAYS]")
	}
	days := 7
	if len(os.Args) == 4 {
		n, err := strconv.Atoi(os.Args[3])
		if err != nil || n <= 0 || n > 366 {
			return fmt.Errorf("DAYS %q is not a day count", os.Args[3])
		}
		days = n
	}
	data, err := readRegular(filepath.Join(os.Args[2], recordsFile), maxRecordsBytes)
	if err != nil {
		return err
	}
	records, dropped := CleanRecords(data)
	if dropped > 0 {
		log.Printf("warning: dropped %d invalid record lines", dropped)
	}
	fmt.Print(metricsSummary(records, days, time.Now()))
	return nil
}

// meanP50 is a slice's mean and its lower median (the middle value, rounded
// down on a tie): both deterministic, neither needs the full quartile work.
func meanP50(values []float64) (mean, p50 float64) {
	if len(values) == 0 {
		return 0, 0
	}
	sorted := slices.Clone(values)
	sort.Float64s(sorted)
	for _, v := range sorted {
		mean += v
	}
	return mean / float64(len(sorted)), sorted[(len(sorted)-1)/2]
}

// secondsDuration renders ledger seconds as the report renders a duration.
func secondsDuration(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}

// metricsSummary renders the records in the last `days` UTC days (today
// included) as Markdown. Only numbers and the record's own bounded words
// (mode, stop, date) are printed; a record carries no free text.
func metricsSummary(records []RunRecord, days int, now time.Time) string {
	today := now.UTC().Format(time.DateOnly)
	start := now.UTC().Add(-time.Duration(days-1) * 24 * time.Hour).Format(time.DateOnly)
	var recent []RunRecord
	for _, r := range records {
		if r.Date >= start && r.Date <= today {
			recent = append(recent, r)
		}
	}
	unit := "days"
	if days == 1 {
		unit = "day"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Run metrics: %d runs in the last %d %s (through %s)\n\n",
		len(recent), days, unit, today)

	modeTotal := map[string]int{}
	stopTotal := map[string]int{}
	modeStop := map[string]map[string]int{}
	storeFinished := 0
	for _, r := range recent {
		if modeStop[r.Mode] == nil {
			modeStop[r.Mode] = map[string]int{}
		}
		modeStop[r.Mode][r.Stop]++
		modeTotal[r.Mode]++
		stopTotal[r.Stop]++
		if r.StoreFinished {
			storeFinished++
		}
	}
	var stops []string
	for _, s := range recordStops {
		if stopTotal[s] > 0 {
			stops = append(stops, s)
		}
	}
	if len(recent) > 0 {
		b.WriteString("| mode |")
		for _, s := range stops {
			fmt.Fprintf(&b, " %s |", s)
		}
		b.WriteString(" total |\n")
		for i := 0; i <= len(stops); i++ {
			b.WriteString("|--:")
		}
		b.WriteString("|\n")
		for _, m := range recordModes {
			if modeTotal[m] == 0 {
				continue
			}
			fmt.Fprintf(&b, "| %s |", m)
			for _, s := range stops {
				fmt.Fprintf(&b, " %d |", modeStop[m][s])
			}
			fmt.Fprintf(&b, " %d |\n", modeTotal[m])
		}
		fmt.Fprintf(&b, "\nStore-finished runs: %d\n", storeFinished)
	}

	// Wall time by mode, over the runs that have a figure at all; 0 means
	// the run job could not ask the API.
	walls := map[string][]float64{}
	costs := map[string][]float64{}
	for _, r := range recent {
		if r.WallSeconds > 0 {
			walls[r.Mode] = append(walls[r.Mode], r.WallSeconds)
		}
		costs[r.Mode] = append(costs[r.Mode], r.CostUSD)
	}
	b.WriteString("\nWall time by mode:\n\n| mode | runs | mean | p50 |\n|--:|--:|--:|--:|\n")
	wallRows := 0
	for _, m := range recordModes {
		if len(walls[m]) == 0 {
			continue
		}
		mean, p50 := meanP50(walls[m])
		fmt.Fprintf(&b, "| %s | %d | %s | %s |\n", m, len(walls[m]),
			fmtDuration(secondsDuration(mean)), fmtDuration(secondsDuration(p50)))
		wallRows++
	}
	if wallRows == 0 {
		b.WriteString("| — | 0 | — | — |\n")
	}

	b.WriteString("\nMean cost by mode:\n\n| mode | runs | mean cost |\n|--:|--:|--:|\n")
	costRows := 0
	for _, m := range recordModes {
		if len(costs[m]) == 0 {
			continue
		}
		mean, _ := meanP50(costs[m])
		fmt.Fprintf(&b, "| %s | %d | %s |\n", m, len(costs[m]), money(mean))
		costRows++
	}
	if costRows == 0 {
		b.WriteString("| — | 0 | — |\n")
	}

	// Cost by day, oldest first.
	byDay := map[string]float64{}
	var days3 []string
	seenDay := map[string]bool{}
	for _, r := range recent {
		if !seenDay[r.Date] {
			seenDay[r.Date] = true
			days3 = append(days3, r.Date)
		}
		byDay[r.Date] += r.CostUSD
	}
	sort.Strings(days3)
	b.WriteString("\nCost by day:\n\n| day | cost |\n|--:|--:|\n")
	for _, d := range days3 {
		fmt.Fprintf(&b, "| %s | %s |\n", d, money(byDay[d]))
	}

	slotWait := 0.0
	rate429 := 0
	prRuns := map[int]int{}
	for _, r := range recent {
		slotWait += r.SlotWaitSeconds
		rate429 += r.Rate429
		if r.Mode == "pr" {
			prRuns[r.Number]++
		}
	}
	fmt.Fprintf(&b, "\nUpstream 429s: %d\n\nSlot wait total: %s\n", rate429, fmtDuration(secondsDuration(slotWait)))
	var repeat []int
	for n, count := range prRuns {
		if count > 1 {
			repeat = append(repeat, n)
		}
	}
	sort.Ints(repeat)
	fmt.Fprintf(&b, "\nPRs with more than one `pr`-mode run: %d", len(repeat))
	if len(repeat) > 0 {
		numbers := make([]string, len(repeat))
		for i, n := range repeat {
			numbers[i] = strconv.Itoa(n)
		}
		fmt.Fprintf(&b, " (%s)", strings.Join(numbers, ", "))
	}
	b.WriteString("\n")
	return b.String()
}

// recordShape is the one-line shape `state load` logs: how the last seven
// days ran, from the bundle's records. The done/incomplete figures count
// issue-mode runs only, so a `pr`, `review`, `plan` or `experiment` run that
// ended done or incomplete does not inflate the issue figures.
func recordShape(records []RunRecord, now time.Time) string {
	start := now.UTC().Add(-6 * 24 * time.Hour).Format(time.DateOnly)
	today := now.UTC().Format(time.DateOnly)
	review, plan := 0, 0
	done, incomplete := 0, 0
	for _, r := range records {
		if r.Date < start || r.Date > today {
			continue
		}
		switch r.Mode {
		case "issue":
			switch r.Stop {
			case stopDone:
				done++
			case stopIncomplete:
				incomplete++
			}
		case "review":
			review++
		case "plan":
			plan++
		}
	}
	return fmt.Sprintf("issue %d done/%d incomplete, review %d, plan %d",
		done, incomplete, review, plan)
}
