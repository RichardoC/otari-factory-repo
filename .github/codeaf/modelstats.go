// The deterministic core of `harness model-stats` (issue #131): the model
// latency and throughput report the routine posts on its tracking issue.
//
// Every input is untrusted. The zips come from completed workflow runs --
// runs from a fork, or any file a run's jobs wrote -- and the ledger lines
// inside them carry free text (model names, upstream errors). So the reader
// reads a zip in memory with archive/zip, never extracts it and never runs
// anything out of it; it refuses an entry that would cost more than a bounded
// number of bytes before decompressing it; it decodes each line into a fixed
// struct; and it copies no free text from a line into the report, only
// values that validated against models.json and the bounded ranges below.
// Rejected rows are counted, and the count is part of the report.
//
// modelStatsReport is pure: the same entries and the same config yield
// byte-identical markdown, which is what makes the report postable on a
// tracking issue and diffable between runs.
package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func init() {
	registerCheck("model-stats", checkModelStats)
}

// The issue #131 bounds. The value is the bound; past it the input is
// refused, never truncated.
const (
	// modelStatsMaxEntries is how many files one run's zip may hold.
	modelStatsMaxEntries = 512
	// modelStatsMaxEntryUncompressed is the largest one entry may decompress
	// to, and modelStatsMaxEntryCompressed the largest compressed form the
	// reader will hand to the decompressor. A zip bomb fails the second: a
	// tiny entry claiming gigabytes is refused on its own header, before any
	// bytes are inflated.
	modelStatsMaxEntryUncompressed = 64 << 20
	modelStatsMaxEntryCompressed   = 16 << 20
	// modelStatsMaxLine is the longest ledger line the reader accepts; an
	// unterminated line longer than this is a hostile row, not a line.
	modelStatsMaxLine = 64 << 10
	// modelStatsMaxRunRows is how many ledger rows one run may contribute.
	modelStatsMaxRunRows = 20_000
	// modelStatsMaxRows bounds the whole window, across every run, so a pile
	// of large zips cannot exhaust the routine's memory.
	modelStatsMaxRows = 500_000
	// modelStatsMaxZips bounds how many zips one report reads.
	modelStatsMaxZips = 2_000
	// modelStatsMaxTimeMs and modelStatsMaxTokens are the integer ranges a
	// decoded row has to stay inside. 6 h is longer than any call the
	// account has ever made; 10M tokens is past every model's context.
	modelStatsMaxTimeMs = 6 * 60 * 60 * 1000
	modelStatsMaxTokens = 10_000_000
	// modelStatsMaxSlotWaitS bounds a slot wait: past ten minutes the figure
	// is a corrupt value, and an unbounded float would poison a percentile.
	modelStatsMaxSlotWaitS = 600.0
	// modelStatsMaxReason bounds the length of the reason a skipped zip or
	// entry is reported with. The reason is written by this file, never taken
	// from the input, and stays short enough for one line of markdown.
	modelStatsMaxReason = 120
	// modelStatsLedgerName is the ledger the proxy writes into a run's
	// codeaf-output artifact; every other entry is skipped.
	modelStatsLedgerName = "codeaf-ledger.jsonl"
	// modelStatsMaxMarkers counts the status/cut marker rows kept for the
	// error classes; past it the classes lose their sample, never the report.
	modelStatsMaxMarkers = 100_000
)

// rejectClass names why a row, a zip or an entry was refused. The classes
// are constants in this file and the reported text is built from them; no
// string from the input reaches the report.
type rejectClass string

const (
	rejectBadJSON    rejectClass = "line is not a JSON object"
	rejectOversize   rejectClass = "line longer than 64 KiB"
	rejectUnknown    rejectClass = "model not in models.json"
	rejectBadTime    rejectClass = "timestamp missing or out of range"
	rejectBadInteger rejectClass = "integer outside its allowed range"
	rejectBadStatus  rejectClass = "HTTP status outside 100..599"
	// rejectUnknownKind is a line whose kind is neither a call (no kind),
	// "status", "abandoned", nor one of the proxy markers (proxy_start,
	// quotas, search).
	// It is refused rather than silently accepted, and rather than charged to
	// the model-mismatch class it is not.
	rejectUnknownKind rejectClass = "line kind is not a call, a status or a proxy marker"
	rejectBadNumber   rejectClass = "number not finite or out of range"
)

// ledgerRow is one decoded, validated ledger line, reduced to the fields the
// report keeps: a model id that models.json allows, bounded integers, and
// timing. It carries no free text at all -- not the model name, not the
// upstream id, not an error string -- so nothing copied from a line can be
// echoed later.
type ledgerRow struct {
	Time       time.Time
	Model      string
	Run        int // index of the run (zip) the row came from
	TTFTMs     int64
	DurMs      int64
	MaxGapMs   int64
	SlotWaitS  float64
	Tokens     int
	UsageSeen  bool
	Cut        bool
	Status     int // 0: the line carries no upstream status
	completion bool
}

// ledgerMarker is a status line kept for the error-class rates. It carries
// the timestamp the window filter needs (issue #319) alongside the class:
// a marker from an hour the report does not cover must drop out of the
// error classes and the 429-rate denominator, not just the call rows. A
// marker can also carry the slot wait the refused call queued through
// (issue #342), because congestion that ends in a refusal is exactly the
// congestion the slot figures and the abandoned alert exist to report.
type ledgerMarker struct {
	Time   time.Time
	Model  string
	Run    int
	Status int
	// SlotWaitS is that wait, in seconds: 0 on a status line the proxy
	// refused without queueing -- a model it never allowed, a body it never
	// parsed, a call upstream answered outright.
	SlotWaitS float64
	// HeaderMs is the header wait an abandoned-before-headers marker carries
	// (issue #456). It is a fact about a call that never completed, so like
	// SlotWaitS it is reported and never a call timing.
	HeaderMs int64
	// Stage is the abandon stage on a kind "abandoned" marker (issue #456),
	// and empty on every status line. It is one of the two constants in
	// main.go, decided by the reader: an unfamiliar stage is refused rather
	// than folded into a figure.
	Stage string
}

// windowStats is everything the report needs that is not per-model.
type windowStats struct {
	Rows          int // rows accepted
	Rejected      int // rows refused, by any class
	RejectedClass map[rejectClass]int
	Zips          int
	ZipSkipped    int
	SkippedReason []string // bounded, built here, one per skipped zip or entry
	From          time.Time
	To            time.Time
	Markers       []ledgerMarker
}

// modelStatsInput is the validated window: the rows of every run, the run
// count, and the skip and reject bookkeeping the report states.
type modelStatsInput struct {
	Rows     []ledgerRow
	Stats    windowStats
	RunCount int
}

// addReject counts one refused row under its class.
func (in *modelStatsInput) addReject(class rejectClass) {
	in.Stats.Rejected++
	if in.Stats.RejectedClass == nil {
		in.Stats.RejectedClass = map[rejectClass]int{}
	}
	in.Stats.RejectedClass[class]++
}

// modelStatsBounds are the caps the reader enforces, as a struct so a test
// can hand the same reader a tiny zip with the real caps in place.
type modelStatsBounds struct {
	MaxEntries      int
	MaxEntryUnc     int64
	MaxEntryComp    int64
	MaxLine         int
	MaxRunRows      int
	MaxRows         int
	MaxSlotWaitS    float64
	MaxTimeMs       int64
	MaxTokens       int64
	MaxZips         int
	MaxMarkers      int
	LedgerEntryName string
}

func defaultModelStatsBounds() modelStatsBounds {
	return modelStatsBounds{
		MaxEntries:      modelStatsMaxEntries,
		MaxEntryUnc:     modelStatsMaxEntryUncompressed,
		MaxEntryComp:    modelStatsMaxEntryCompressed,
		MaxLine:         modelStatsMaxLine,
		MaxRunRows:      modelStatsMaxRunRows,
		MaxRows:         modelStatsMaxRows,
		MaxSlotWaitS:    modelStatsMaxSlotWaitS,
		MaxTimeMs:       modelStatsMaxTimeMs,
		MaxTokens:       modelStatsMaxTokens,
		MaxZips:         modelStatsMaxZips,
		MaxMarkers:      modelStatsMaxMarkers,
		LedgerEntryName: modelStatsLedgerName,
	}
}

// skipReason returns a bounded reason string, built from a prefix this file
// writes and, where useful, a bounded count. It never returns input text.
func skipReason(format string, args ...any) string {
	s := fmt.Sprintf(format, args...)
	if len(s) > modelStatsMaxReason {
		s = s[:modelStatsMaxReason-1] + "…"
	}
	return s
}

// ---------------------------------------------------------------- reading

// allowedModelSet is the set of model ids the report may keep. models.json's
// `id` is the id the proxy records on a line (main.go's record), so that is
// the id the report keys on.
func allowedModelSet(cfg Config) map[string]bool {
	set := make(map[string]bool, len(cfg.Models))
	for _, m := range cfg.Models {
		if m.ID != "" {
			set[m.ID] = true
		}
	}
	return set
}

// skippedArtifact names one artifact the caller could not turn into a zip,
// so the reader can count it and state the reason. The bytes that would not
// parse never reach the reader -- only this bounded, self-built record does,
// so a hostile download cannot put text of its own into the report.
//
// Index is the 1-based position of the artifact in the caller's own run
// order (a run number or a file number), or 0 for an artifact the caller
// already counted and reasoned about the way it wanted.
type skippedArtifact struct {
	Index  int
	Reason string
}

// parseModelStatsZips reads a set of in-memory zips -- the downloaded
// codeaf-output artifacts -- and returns the validated window they carry.
// Nothing is extracted and nothing is executed: every entry is read through
// the archive/zip reader, and an entry whose header claims more than the
// bounds allow is refused without being decompressed.
//
// Artifacts that yielded no zip at all -- a download that failed, bytes that
// are not a zip -- are handed in as skipped rather than dropped on the floor:
// an unreadable artifact is a fact about the window, and the report says how
// many there were and why. See parseModelStatsZipsSkipped.
func parseModelStatsZips(zips []*zip.Reader, cfg Config) modelStatsInput {
	return parseModelStatsZipsSkipped(zips, nil, cfg)
}

// parseModelStatsZipsSkipped is parseModelStatsZips with the artifacts the
// caller already knows are unreadable. The zip readers and the skipped
// records are two views of one run list: the i-th reader is the i-th
// artifact, and a skipped record with an Index of i+1 is that same artifact
// with no reader. Parsing stays a pure function of its inputs -- these
// slices and the bounds -- so the same inputs render the same report.
func parseModelStatsZipsSkipped(zips []*zip.Reader, skipped []skippedArtifact, cfg Config) modelStatsInput {
	return parseModelStatsWithBoundsSkipped(zips, skipped, allowedModelSet(cfg), defaultModelStatsBounds())
}

// parseModelStatsWithBounds is parseModelStatsZipsSkipped with the bounds
// made explicit, so TestParseModelStatsZipBounds can drive the real reader.
func parseModelStatsWithBounds(zips []*zip.Reader, allowed map[string]bool, b modelStatsBounds) modelStatsInput {
	return parseModelStatsWithBoundsSkipped(zips, nil, allowed, b)
}

// parseModelStatsWithBoundsSkipped is the reader both wrappers end in.
//
// A skipped artifact has no zip.Reader to inspect, so the reader cannot tell
// an empty artifact from an expired one by itself; it takes the caller's
// bounded reason, counts the artifact as skipped and reads on. An artifact
// that did parse but is refused by the bounds below (too many entries, an
// entry over a size cap) is counted here, with a reason this file writes.
func parseModelStatsWithBoundsSkipped(zips []*zip.Reader, skipped []skippedArtifact, allowed map[string]bool, b modelStatsBounds) modelStatsInput {
	in := modelStatsInput{Stats: windowStats{RejectedClass: map[rejectClass]int{}}}
	for _, s := range skipped {
		// A reason the caller writes is already bounded (skipReason); one it
		// forgets is replaced, never echoed, so no reason reaches the report
		// longer than the cap or assembled from input text.
		reason := skipReason("run %d: %s", s.Index, "artifact not read")
		if s.Reason != "" {
			reason = skipReason("%s", s.Reason)
		}
		in.Stats.ZipSkipped++
		in.Stats.SkippedReason = append(in.Stats.SkippedReason, reason)
	}
	for i, zr := range zips {
		if i >= b.MaxZips {
			// Over the cap: refuse only the artifacts past it and read on,
			// like every other bound in this reader -- never the whole
			// window. Each excess artifact counts as skipped; the reason is
			// one bounded line, written once at the first excess artifact.
			in.Stats.ZipSkipped++
			if i == b.MaxZips {
				in.Stats.SkippedReason = append(in.Stats.SkippedReason,
					skipReason("%d zips present, over the %d-zip cap", len(zips), b.MaxZips))
			}
			continue
		}
		if zr == nil {
			in.Stats.ZipSkipped++
			in.Stats.SkippedReason = append(in.Stats.SkippedReason, skipReason("run %d: no artifact", i+1))
			continue
		}
		if len(zr.File) == 0 {
			in.Stats.ZipSkipped++
			in.Stats.SkippedReason = append(in.Stats.SkippedReason, skipReason("run %d: empty artifact", i+1))
			continue
		}
		if len(zr.File) > b.MaxEntries {
			// Refused, not truncated: a zip with more entries than any real
			// codeaf-output artifact has is a hostile one, and reading part
			// of it would be reading whatever the writer chose to put first.
			in.Stats.ZipSkipped++
			in.Stats.SkippedReason = append(in.Stats.SkippedReason,
				skipReason("run %d: %d entries, over the %d-entry cap", i+1, len(zr.File), b.MaxEntries))
			continue
		}
		in.Stats.Zips++
		in.RunCount++
		run := in.RunCount
		rowsThisRun := 0
		for _, f := range zr.File {
			if filepath.Base(f.Name) != b.LedgerEntryName {
				continue // every other artifact entry is ignored, unread
			}
			// The header decides before any byte is inflated: a zip bomb
			// announces its size, and a reader that trusts it decompresses
			// the bomb to find out it was one.
			if int64(f.CompressedSize64) > b.MaxEntryComp {
				in.Stats.SkippedReason = append(in.Stats.SkippedReason,
					skipReason("run %d: entry compressed size over the cap", run))
				continue
			}
			if int64(f.UncompressedSize64) > b.MaxEntryUnc {
				in.Stats.SkippedReason = append(in.Stats.SkippedReason,
					skipReason("run %d: entry uncompressed size over the cap", run))
				continue
			}
			rc, err := f.Open()
			if err != nil {
				in.Stats.SkippedReason = append(in.Stats.SkippedReason,
					skipReason("run %d: entry unreadable", run))
				continue
			}
			// The read is bounded by the same cap the header check used, so
			// the two cannot drift apart: parseLedger stops at MaxEntryUnc+1
			// bytes and refuses the entry when it gets there, so nothing a
			// zip's header promised can make the reader inflate past the cap.
			// The run's remaining row budget travels with the read (issue
			// #388): one artifact may hold the ledger under more than one
			// directory, and a budget counted per entry would let every copy
			// count afresh, so the cap bounds the run across entries.
			rowsThisRun += in.parseLedger(rc, run, allowed, b, b.MaxEntryUnc+1, b.MaxRunRows-rowsThisRun)
			rc.Close()
		}
		// A run that contributed no rows at all is still a run the window
		// saw: "runs" in the report is runs with a ledger, and a run whose
		// rows were all rejected is exactly what the reject alert is for.
	}
	sortMarkers(&in)
	return in
}

// parseLedger decodes one ledger entry, line by line, into the window. A
// line that fails any check is refused and counted under its class; the
// decoder never aborts the run on one hostile line. maxBytes bounds the
// inflated input (the decompression-bomb guard). rowsLeft is what the run
// may still contribute after the entries read so far: once it is spent the
// entry stops and the excess is stated in SkippedReason, so a zip holding
// the ledger under several names cannot multiply one run's rows (issue
// #388).
func (in *modelStatsInput) parseLedger(r io.Reader, run int, allowed map[string]bool, b modelStatsBounds, maxBytes int64, rowsLeft int) int {
	data, err := io.ReadAll(io.LimitReader(r, maxBytes))
	if err != nil {
		in.Stats.SkippedReason = append(in.Stats.SkippedReason,
			skipReason("run %d: entry read failed", run))
		return 0
	}
	if int64(len(data)) >= maxBytes-1 {
		// The entry decompressed past the cap: its header lied about its
		// size, so nothing of it can be trusted -- not even its first line.
		in.Stats.SkippedReason = append(in.Stats.SkippedReason,
			skipReason("run %d: entry inflated past the cap", run))
		return 0
	}
	rows := 0
	for start := 0; start < len(data); {
		rest := data[start:]
		nl := bytes.IndexByte(rest, '\n')
		var line []byte
		if nl < 0 {
			line = rest
			start = len(data)
		} else {
			line = rest[:nl]
			start += nl + 1
		}
		line = bytes.TrimRight(line, "\r")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if len(line) > b.MaxLine {
			in.addReject(rejectOversize)
			continue
		}
		if in.Stats.Rows >= b.MaxRows {
			// The window cap has nothing left to give: the rest of this entry
			// is not read. The window's own figures in the report say so, so
			// unlike the per-run cap no reason line is needed here.
			return rows
		}
		if rowsLeft <= 0 || rows >= rowsLeft {
			// The run's row budget is spent, by this entry or an earlier one:
			// a zip may hold the ledger under several names, and a budget
			// counted per entry would let every copy count afresh (issue #388).
			// The rest of the entry is not read, and the excess is stated
			// like every other cap rather than silently dropped.
			in.Stats.SkippedReason = append(in.Stats.SkippedReason,
				skipReason("run %d: %d-row per-run cap reached, rest of entry skipped", run, b.MaxRunRows))
			return rows
		}
		rows++
		in.decodeLedgerLine(line, run, allowed, b)
	}
	return rows
}

// rawLedgerLine is the wire shape the decoder reads. It is deliberately a
// second type rather than LedgerLine: the routine wants seven fields, and
// every other field of LedgerLine would be text the report must not keep.
type rawLedgerLine struct {
	TS        json.RawMessage `json:"ts"`
	Model     json.RawMessage `json:"model"`
	Kind      string          `json:"kind"`
	TTFTMs    json.RawMessage `json:"ttft_ms"`
	DurMs     json.RawMessage `json:"dur_ms"`
	MaxGapMs  json.RawMessage `json:"max_gap_ms"`
	SlotWaitS json.RawMessage `json:"slot_wait_s"`
	HeaderMs  json.RawMessage `json:"header_ms"`
	Tokens    json.RawMessage `json:"completion_tokens"`
	UsageSeen bool            `json:"usage_seen"`
	Cut       bool            `json:"cut"`
	Status    json.RawMessage `json:"status"`
	// Abandoned is the stage a call was abandoned at (issue #456): set on a
	// kind "abandoned" marker only, empty on every other line.
	Abandoned string `json:"abandoned"`
}

// decodeLedgerLine validates one line and folds it into the window.
func (in *modelStatsInput) decodeLedgerLine(line []byte, run int, allowed map[string]bool, b modelStatsBounds) {
	var raw rawLedgerLine
	if err := json.Unmarshal(line, &raw); err != nil {
		in.addReject(rejectBadJSON)
		return
	}
	// The kinds the proxy writes about itself rather than about a model:
	// "proxy_start" (recordMarker, main.go) and "search" (the forwarded-
	// search line of issue #58: kind "search", model the fixed id
	// "web-search", not a model in models.json; issue #2 ended those lines --
	// searches now count on the call rows -- but old ledgers still carry
	// them, so they are skipped all the same).
	// "quotas" is no longer written -- the synthetic.new quota guard is
	// gone, so there is no recordQuotas -- but ledgers from before that
	// change still carry it, and it is skipped all the same. These are the
	// proxy speaking about itself, not a ledger row about a model, so they
	// are skipped whole: not a row, not a rejection. Deciding this before
	// the model check is what keeps a run that carries them from tripping
	// the reject alert.
	//
	// "status" and "abandoned" are the two non-call kinds the report keeps:
	// each becomes a marker, handled below. Every other kind -- and a line
	// with no kind at all -- has to be a call with a model models.json
	// allows; a kind that is neither a call nor one of the known ones is
	// refused below rather than silently accepted.
	switch raw.Kind {
	case "proxy_start", "quotas", "search":
		return
	}
	var model string
	if err := json.Unmarshal(raw.Model, &model); err != nil || model == "" || !allowed[model] {
		// One class for a missing, non-string or unknown model: the reader
		// never repeats what the line claimed, only that it was refused.
		in.addReject(rejectUnknown)
		return
	}
	if raw.Kind != "" && raw.Kind != "status" && raw.Kind != "abandoned" {
		// A kind the reader does not know is not a ledger row: the report
		// states it rather than folding it into a model figure.
		in.addReject(rejectUnknownKind)
		return
	}
	status, hasStatus, ok := decodeStatus(raw.Status)
	if !ok {
		in.addReject(rejectBadStatus)
		return
	}
	// The proxy writes a status line only with kind "status" (record, main.go)
	// and leaves both kind and status empty on a successful call. Gating on
	// the kind as well as the number keeps a successful call that happens to
	// carry an explicit status -- the ledger this report reads -- from being
	// filed as a marker and dropped from every count.
	if raw.Kind == "abandoned" {
		// An abandoned-call marker (issue #456): the proxy gave up on a call
		// before it produced any completion and wrote the wait it had
		// already measured -- the slot wait for a call abandoned while
		// queueing, the header wait for one abandoned while upstream's
		// headers were awaited. It is a marker and not a call row: no
		// completion was produced, so it joins no call or timing figure,
		// exactly as a status line does. What it does join is the slot
		// figures and the abandoned alert, which is what neither path could
		// reach before: a queue abandon left no line at all.
		stage := raw.Abandoned
		if stage != abandonedBeforeStart && stage != abandonedBeforeHeaders {
			// The stage decides which wait the line is evidence for, so an
			// unfamiliar one is refused rather than guessed at.
			in.addReject(rejectUnknownKind)
			return
		}
		ts, tsOK := decodeLedgerTS(raw.TS)
		if !tsOK {
			in.addReject(rejectBadTime)
			return
		}
		slot, slotOK := decodeSlotWait(raw.SlotWaitS, b.MaxSlotWaitS)
		if !slotOK {
			in.addReject(rejectBadNumber)
			return
		}
		header, headerOK := decodeMillis(raw.HeaderMs, b.MaxTimeMs)
		if !headerOK {
			in.addReject(rejectBadInteger)
			return
		}
		if len(in.Stats.Markers) < b.MaxMarkers {
			in.Stats.Markers = append(in.Stats.Markers, ledgerMarker{
				Time: ts, Model: model, Run: run, SlotWaitS: slot,
				HeaderMs: header, Stage: stage})
		}
		return
	}
	if raw.Kind == "status" {
		// A status marker (kind "status", issue #140) carries no call timing:
		// it feeds the 4xx/5xx classes and nothing else about the call. It
		// must carry a status number to say which class it belongs to, and
		// its timestamp is kept so the window can clip it (issue #319).
		if !hasStatus {
			in.addReject(rejectBadStatus)
			return
		}
		// The marker keeps the timestamp a status line always carries (the
		// proxy records one on every line), so filterWindow can window-clip
		// markers exactly as it clips rows: an unparseable time is refused
		// here as it is for a call row, under the same reject class.
		ts, tsOK := decodeLedgerTS(raw.TS)
		if !tsOK {
			in.addReject(rejectBadTime)
			return
		}
		// A status line may carry the slot wait the call queued through
		// before the refusal (issue #342): an upstream refusal records one
		// too, and so does the proxy credit-cap refusal after a wait.
		// Decoded and validated like a call row wait, so a forged wait is
		// refused rather than folded into the slot figures.
		slot, slotOK := decodeSlotWait(raw.SlotWaitS, b.MaxSlotWaitS)
		if !slotOK {
			in.addReject(rejectBadNumber)
			return
		}
		if len(in.Stats.Markers) < b.MaxMarkers {
			in.Stats.Markers = append(in.Stats.Markers, ledgerMarker{Time: ts, Model: model, Run: run, Status: status, SlotWaitS: slot})
		}
		return
	}
	ts, tsOK := decodeLedgerTS(raw.TS)
	if !tsOK {
		in.addReject(rejectBadTime)
		return
	}
	ttft, ok1 := decodeMillis(raw.TTFTMs, b.MaxTimeMs)
	dur, ok2 := decodeMillis(raw.DurMs, b.MaxTimeMs)
	gap, ok3 := decodeMillis(raw.MaxGapMs, b.MaxTimeMs)
	if !ok1 || !ok2 || !ok3 {
		in.addReject(rejectBadInteger)
		return
	}
	if dur != 0 && ttft > dur {
		// First token after the call ended is not a measurement.
		in.addReject(rejectBadInteger)
		return
	}
	slot, ok := decodeSlotWait(raw.SlotWaitS, b.MaxSlotWaitS)
	if !ok {
		in.addReject(rejectBadNumber)
		return
	}
	tokens, usageSeen, ok := decodeTokens(raw.Tokens, b.MaxTokens)
	if !ok {
		in.addReject(rejectBadInteger)
		return
	}
	in.Stats.Rows++
	in.Rows = append(in.Rows, ledgerRow{
		Time: ts, Model: model, Run: run,
		TTFTMs: ttft, DurMs: dur, MaxGapMs: gap, SlotWaitS: slot,
		Tokens: tokens, UsageSeen: usageSeen, Cut: raw.Cut, Status: status,
		completion: usageSeen && tokens > 0,
	})
	if in.Stats.From.IsZero() || ts.Before(in.Stats.From) {
		in.Stats.From = ts
	}
	if ts.After(in.Stats.To) {
		in.Stats.To = ts
	}
}

// decodeMillis reads an optional millisecond integer. Absent parses as 0,
// the way an old ledger line reads; anything outside 0..max is refused. A
// float that is not a whole number is refused too: the proxy writes ints,
// and a fractional millisecond is a line somebody else wrote.
func decodeMillis(raw json.RawMessage, max int64) (int64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, true
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	if n < 0 || n > max {
		return 0, false
	}
	return n, true
}

// decodeSlotWait reads slot_wait_s: an optional finite number in
// [0, max]. A negative wait, a NaN, an infinity or a value past the cap is
// refused.
func decodeSlotWait(raw json.RawMessage, max float64) (float64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, true
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > max {
		return 0, false
	}
	return f, true
}

// decodeTokens reads completion_tokens. Absent parses as 0 with usageSeen
// false, which is how a line written before usage was recorded reads; a
// negative or over-cap count is refused.
func decodeTokens(raw json.RawMessage, max int64) (int, bool, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false, true
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false, false
	}
	if n < 0 || n > max {
		return 0, false, false
	}
	return int(n), true, true
}

// decodeLedgerTS decodes the ts field a call row and a status marker both
// carry, to the range raw row decoding already enforced. Both line kinds
// share it so their bounds cannot drift apart.
func decodeLedgerTS(raw json.RawMessage) (time.Time, bool) {
	var ts time.Time
	if err := json.Unmarshal(raw, &ts); err != nil || ts.IsZero() {
		return time.Time{}, false
	}
	ts = ts.UTC()
	if ts.Year() < 2000 || ts.Year() > 2100 {
		return time.Time{}, false
	}
	return ts, true
}

// decodeStatus reads the upstream status. Absent, null or 0 means the line
// carries none (the ordinary recorded call); 100..599 is a status marker;
// anything else is refused, so a 999 or a negative never lands in a class.
func decodeStatus(raw json.RawMessage) (int, bool, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false, true
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false, false
	}
	if n == 0 {
		return 0, false, true
	}
	if n < 100 || n > 599 {
		return 0, false, false
	}
	return n, true, true
}

// sortMarkers orders the markers by model then run so the error classes are
// computed in a fixed order (they are counts, but a deterministic walk keeps
// the whole path reproducible).
func sortMarkers(in *modelStatsInput) {
	sort.SliceStable(in.Stats.Markers, func(i, j int) bool {
		if in.Stats.Markers[i].Model != in.Stats.Markers[j].Model {
			return in.Stats.Markers[i].Model < in.Stats.Markers[j].Model
		}
		return in.Stats.Markers[i].Run < in.Stats.Markers[j].Run
	})
}

// readModelStatsDir reads every *.zip in dir as an in-memory download. It is
// the live wrapper's reader; the core never touches the disk.
func readModelStatsDir(dir string, cfg Config) (modelStatsInput, error) {
	names, err := os.ReadDir(dir)
	if err != nil {
		return modelStatsInput{}, fmt.Errorf("model-stats: %w", err)
	}
	var zips []*zip.Reader
	var skipped []skippedArtifact
	files := 0
	for _, e := range names {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".zip") {
			continue
		}
		files++
		path := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil || info.Size() > modelStatsMaxEntryUncompressed*int64(modelStatsMaxZips) {
			skipped = append(skipped, skippedArtifact{Index: files, Reason: skipReason("artifact %d: over the size cap", files)})
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			skipped = append(skipped, skippedArtifact{Index: files, Reason: skipReason("artifact %d: unreadable", files)})
			continue
		}
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			// A file that is not a zip is skipped, not fatal -- one bad
			// download must not sink the report -- but it is COUNTED and
			// NAMED, not dropped on the floor: a file the workflow downloaded
			// and nothing read is exactly the silent hole this states. The
			// file's own name is untrusted text and is never echoed: the
			// reason says "artifact N", this program's own count.
			skipped = append(skipped, skippedArtifact{
				Index:  files,
				Reason: skipReason("artifact %d: not a zip (%d bytes, unreadable)", files, len(data)),
			})
			continue
		}
		zips = append(zips, zr)
	}
	if len(zips) == 0 {
		if len(skipped) > 0 {
			return modelStatsInput{}, fmt.Errorf("model-stats: every one of the %d codeaf-output zip(s) in %s was unreadable", len(skipped), filepath.Base(dir))
		}
		return modelStatsInput{}, errors.New("model-stats: no readable codeaf-output zip in " + filepath.Base(dir))
	}
	return parseModelStatsZipsSkipped(zips, skipped, cfg), nil
}

// ---------------------------------------------------------------- computing

// modelStatsWindow is the window the report covers.
type modelStatsWindow struct {
	Hours int
	From  time.Time // zero when unbounded
	To    time.Time
}

// percentiles is one distribution's nearest-rank summary. The method is the
// reference script's: over the sorted values, the p-th percentile is at
// index ceil(p/100*n)-1, clamped into range. It is a nearest-rank estimate,
// not an interpolation, so every figure is one of the measured values.
type percentiles struct {
	N       int
	P10     float64
	P50     float64
	P90     float64
	P99     float64
	hasData bool
}

// nearestRank returns the p-th percentile of the sorted values.
func nearestRank(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// summarize sorts a copy and computes the nearest-rank percentiles.
func summarize(values []float64) percentiles {
	p := percentiles{N: len(values)}
	if len(values) == 0 {
		return p
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)
	p.P10, p.P50, p.P90, p.P99 = nearestRank(sorted, 10), nearestRank(sorted, 50), nearestRank(sorted, 90), nearestRank(sorted, 99)
	p.hasData = true
	return p
}

// hourBucket is one UTC hour of the window, for the first-token-by-hour
// table and for the hourly alerts. Key is the hour as "2006-01-02 15:00".
type hourBucket struct {
	Key   string
	Start time.Time
	Calls int
	TTFT  []float64
	// Decode is this hour's decode tok/s cohort: the same completions that
	// feed a.Decode, kept per hour so the decode alert has a trailing
	// baseline of its own instead of the window-wide p50 it judges.
	Decode []float64
}

// modelAgg is one model's figures over the window. Every slice and map is
// built in a fixed order (rows arrive in run order, lines in file order), so
// the rendering is reproducible.
type modelAgg struct {
	ID        string
	Runs      map[int]bool
	HourCalls []int // index: hour from windowStart; the slot-wait alert reads it
	TTFT      []float64
	Decode    []float64
	E2E       []float64
	Calls     int
	Completes int
	Cuts      int
	Stalls    int
	Fourxx    int
	Fivexx    int
	Other     int
	Four29    int
	Errs      int
	SlotCalls int
	SlotWaits []float64
	Abandoned int
	// AbandonedHeaders counts calls abandoned while waiting for upstream's
	// response headers (issue #456): a slot held and a request issued, no
	// completion. Such a call may also have queued, in which case it is in
	// Abandoned too; this counts the stage, not the queue.
	AbandonedHeaders int
	byHour           map[string]*hourBucket
	hours            []*hourBucket
	// slotWaitsPerRun counts the calls that waited, per run: the alert fires
	// on more than the configured number of waits in one run.
	slotWaitsPerRun map[int]int
}

// run is the number of distinct runs that contributed a call for this model.
func (a *modelAgg) runCount() int { return len(a.Runs) }

// addHour appends an hour bucket in first-seen order.
func (a *modelAgg) addHour(ts time.Time) *hourBucket {
	key := ts.Format("2006-01-02 15:00")
	if b, ok := a.byHour[key]; ok {
		return b
	}
	b := &hourBucket{Key: key, Start: ts.Truncate(time.Hour)}
	a.byHour[key] = b
	a.hours = append(a.hours, b)
	return b
}

// modelAggregate folds the window's rows into one aggregate per model. Row
// order decides nothing: every figure is a count, a sum or a percentile of a
// set, so the same rows give the same aggregates whatever order they come in.
func modelAggregate(in modelStatsInput, cfg Config, win modelStatsWindow) []*modelAgg {
	byModel := map[string]*modelAgg{}
	order := []string{}
	get := func(id string) *modelAgg {
		if a, ok := byModel[id]; ok {
			return a
		}
		a := &modelAgg{ID: id, Runs: map[int]bool{}, byHour: map[string]*hourBucket{}, slotWaitsPerRun: map[int]int{}}
		byModel[id] = a
		order = append(order, id)
		return a
	}
	// A configured model with no calls in the window has to be reportable,
	// so every allowed id starts an (empty) aggregate -- in models.json order.
	for _, m := range cfg.Models {
		if m.ID != "" {
			get(m.ID)
		}
	}
	for _, r := range in.Rows {
		if !inWindow(r.Time, win) {
			continue
		}
		a := get(r.Model)
		a.Calls++
		a.Runs[r.Run] = true
		a.TTFT = append(a.TTFT, float64(r.TTFTMs))
		if r.SlotWaitS > 0 {
			// A wait exists the moment the call queued, whether or not it
			// ever reached upstream: the wait, not the first token, is the
			// measurement (issue #342). A row that waited and produced no
			// completion is the abandoned wait the alert names.
			a.SlotCalls++
			a.SlotWaits = append(a.SlotWaits, r.SlotWaitS)
			a.slotWaitsPerRun[r.Run]++
			if !r.completion {
				a.Abandoned++
			}
		}
		// A cut row is counted here and only here. The row that produced no
		// completion still goes through miss for its error class, so miss
		// must not touch Cuts: counting the same row in both places made one
		// cut+no-completion row read as Cuts=2.
		if r.Cut && r.TTFTMs > 0 {
			a.Cuts++
		}
		if r.MaxGapMs > 0 && r.MaxGapMs >= cfg.Alerts.StallGapMs {
			a.Stalls++
		}
		decodeOK := false
		decodeRate := 0.0
		if r.completion && r.Tokens >= *cfg.Alerts.DecodeMinTokens {
			// The decode and end-to-end figures both read the same cohort:
			// a completion long enough to measure the model's throughput
			// rather than the fixed overhead of a short one. A handful of
			// tokens measures the overhead, not the rate.
			if r.DurMs > 0 {
				a.E2E = append(a.E2E, float64(r.Tokens)*1000/float64(r.DurMs))
			}
			if r.DurMs > r.TTFTMs {
				decodeRate = float64(r.Tokens) * 1000 / float64(r.DurMs-r.TTFTMs)
				decodeOK = true
				a.Decode = append(a.Decode, decodeRate)
			}
		}
		if r.completion {
			a.Completes++
		} else {
			a.miss(r)
		}
		h := a.addHour(r.Time)
		h.Calls++
		h.TTFT = append(h.TTFT, float64(r.TTFTMs))
		if decodeOK {
			h.Decode = append(h.Decode, decodeRate)
		}
	}
	// Status markers: a 4xx/5xx class, and a 429 count that also counts the
	// 429s a call line carries. A marker is a call the proxy never completed,
	// so it is a row the error rate divides into.
	markerCalls := map[string]int{}
	for _, m := range in.Stats.Markers {
		a, ok := byModel[m.Model]
		if !ok {
			continue
		}
		if m.Stage != "" {
			// An abandoned-call marker (issue #456) carries no status and is
			// not a refusal: it feeds the abandon figures and nothing else,
			// so it stays out of markerCalls, the error classes and the
			// 429-rate denominators, which would otherwise read it as an
			// "other" error.
			if m.Stage == abandonedBeforeHeaders {
				a.AbandonedHeaders++
			}
			if m.SlotWaitS > 0 {
				a.SlotCalls++
				a.SlotWaits = append(a.SlotWaits, m.SlotWaitS)
				a.slotWaitsPerRun[m.Run]++
				a.Abandoned++
			}
			continue
		}
		markerCalls[m.Model]++
		// A status line that queued before its refusal (issue #342) was
		// abandoned mid-congestion, which is exactly what the abandoned
		// alert and the per-run slot-wait count describe, so its wait feeds
		// the same slot figures a waited call row feeds. A marker never
		// completed, so a waited one always counts abandoned.
		if m.SlotWaitS > 0 {
			a.SlotCalls++
			a.SlotWaits = append(a.SlotWaits, m.SlotWaitS)
			a.slotWaitsPerRun[m.Run]++
			a.Abandoned++
		}
		switch class := statusClass(m.Status); class {
		case "429":
			a.Four29++
		case "4xx":
			a.Fourxx++
		case "5xx":
			a.Fivexx++
		default:
			a.Other++
		}
	}
	out := make([]*modelAgg, 0, len(order))
	for _, id := range order {
		a := byModel[id]
		a.Errs = a.Fourxx + a.Fivexx + a.Other + a.Four29
		out = append(out, a)
	}
	return out
}

// miss records the error class of a call that produced no completion.
//
// It deliberately does not touch Cuts: modelAggregate already counted the
// cut for this row (r.Cut && r.TTFTMs > 0), so counting it again here made
// one row read as two. miss owns the error classes and nothing else.
func (a *modelAgg) miss(r ledgerRow) {
	switch statusClass(r.Status) {
	case "429":
		a.Four29++
	case "4xx":
		a.Fourxx++
	case "5xx":
		a.Fivexx++
	}
}

// statusClass maps an upstream status to one fixed class. Anything that is
// not a 4xx, a 429 or a 5xx is "other", and the status itself is never
// printed.
func statusClass(status int) string {
	switch {
	case status == 429:
		return "429"
	case status >= 400 && status <= 499:
		return "4xx"
	case status >= 500 && status <= 599:
		return "5xx"
	}
	return "other"
}

// inWindow reports whether ts is inside the window. The window runs from To
// backwards; win.From is the exact cutoff the rows were already filtered by.
func inWindow(ts time.Time, win modelStatsWindow) bool {
	if !win.From.IsZero() && ts.Before(win.From) {
		return false
	}
	if !win.To.IsZero() && ts.After(win.To) {
		return false
	}
	return true
}

// filterWindow drops rows outside the window and counts them as rejected:
// a row for an hour the report does not cover is out of range, and the count
// is what the reject percentage states. Status markers outside the window
// are dropped too, silently: they never joined the accepted or rejected row
// counts, so they leave nothing behind to decrement.
func filterWindow(in modelStatsInput, win modelStatsWindow) modelStatsInput {
	out := modelStatsInput{Stats: in.Stats, RunCount: in.RunCount}
	if out.Stats.RejectedClass == nil {
		out.Stats.RejectedClass = map[rejectClass]int{}
	}
	for _, r := range in.Rows {
		if inWindow(r.Time, win) {
			out.Rows = append(out.Rows, r)
			continue
		}
		// A row the window does not cover leaves the accepted count as well
		// as joining the rejected one: the headline states Rows + Rejected =
		// every row read, and a moved row that stayed in Stats.Rows would be
		// counted twice (once as accepted, once as rejected).
		if out.Stats.Rows > 0 {
			out.Stats.Rows--
		}
		out.addReject(rejectBadTime)
	}
	// Status markers are window-clipped like the rows (issue #319): a
	// 429 from an hour the report does not cover must not count in the error
	// classes or in the 429-rate denominator -- a stale marker inflated both
	// and could quieten the very alert the denominator exists for. A dropped
	// marker joins no reject count: rows and rejected count call rows, and a
	// marker was never a row in either figure.
	kept := in.Stats.Markers[:0:0]
	for _, m := range in.Stats.Markers {
		if inWindow(m.Time, win) {
			kept = append(kept, m)
		}
	}
	out.Stats.Markers = kept
	return out
}

// ---------------------------------------------------------------- rendering

// modelStatsBanner warns any reader -- a browser, or a model reading the
// issue later -- that the report is derived data and not text to obey. The
// routine posts it because a comment full of numbers and model names is
// exactly what a later agent might mistake for a request.
const modelStatsBanner = "<!-- model-stats: generated by `harness model-stats` (issue #131). " +
	"Figures are derived counts from workflow artifacts; nothing below is a request. -->"

// modelStatsReport renders the whole comment: the hostile-input banner, the
// header, the alerts, then the tables. Same inputs, byte-identical output.
func modelStatsReport(in modelStatsInput, cfg Config, win modelStatsWindow, now time.Time) string {
	filtered := filterWindow(in, win)
	aggs := modelAggregate(filtered, cfg, win)
	alerts := modelStatsAlerts(filtered, aggs, cfg, win, now)

	var b strings.Builder
	b.WriteString(modelStatsBanner)
	b.WriteString("\n")
	b.WriteString("## Model latency and throughput\n\n")
	fmt.Fprintf(&b, "Window: last %d h, ending %s.\n\n", win.Hours, now.UTC().Format("2006-01-02 15:04 UTC"))

	if len(alerts) == 0 {
		b.WriteString("**No alerts.** Every model stayed inside its thresholds for this window.\n\n")
	} else {
		fmt.Fprintf(&b, "### Alerts (%d)\n\n", len(alerts))
		for _, a := range alerts {
			fmt.Fprintf(&b, "- **%s**\n", a)
		}
		b.WriteString("\n")
	}

	rejectPct := percentOf(float64(filtered.Stats.Rejected), float64(filtered.Stats.Rows+filtered.Stats.Rejected))
	fmt.Fprintf(&b, "Rows: %d accepted, %d rejected (%.2f%%, threshold %.2f%%) across %d run(s) with a ledger",
		filtered.Stats.Rows, filtered.Stats.Rejected, rejectPct, cfg.Alerts.RejectPct, filtered.RunCount)
	if filtered.Stats.ZipSkipped > 0 {
		fmt.Fprintf(&b, "; %d artifact(s) skipped", filtered.Stats.ZipSkipped)
	}
	b.WriteString(".\n")
	if len(filtered.Stats.SkippedReason) > 0 {
		b.WriteString("\nSkipped, with the reader's own reason:\n\n")
		for _, r := range filtered.Stats.SkippedReason {
			fmt.Fprintf(&b, "- %s\n", r)
		}
	}
	if filtered.Stats.Rejected > 0 {
		b.WriteString("\nRejected rows by class:\n\n")
		classes := make([]rejectClass, 0, len(filtered.Stats.RejectedClass))
		for c := range filtered.Stats.RejectedClass {
			classes = append(classes, c)
		}
		sort.Slice(classes, func(i, j int) bool { return classes[i] < classes[j] })
		for _, c := range classes {
			fmt.Fprintf(&b, "- %s: %d\n", c, filtered.Stats.RejectedClass[c])
		}
	}
	b.WriteString("\n")

	renderPerModel(&b, aggs, filtered, cfg)
	renderByHour(&b, aggs)
	return b.String()
}

// renderPerModel writes one table row and one detail block per model.
func renderPerModel(b *strings.Builder, aggs []*modelAgg, in modelStatsInput, cfg Config) {
	b.WriteString("### Per model\n\n")
	b.WriteString("| model | n | runs | first token p50 | p90 | p99 | decode tok/s p50 (p10-p90) | e2e tok/s p50 | cut | 429 | error | stall | stall rate | slot waits | slot p90 | abandoned |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, a := range aggs {
		ttft := summarize(a.TTFT)
		decode := summarize(a.Decode)
		e2e := summarize(a.E2E)
		slots := summarize(a.SlotWaits)
		fmt.Fprintf(b, "| %s | %d | %d | %s | %s | %s | %s | %s | %d | %d | %d | %d | %s | %d | %s | %d |\n",
			modelIDCell(a.ID),
			a.Calls, a.runCount(),
			msCell(ttft.P50), msCell(ttft.P90), msCell(ttft.P99),
			decodeCell(decode), tokCell(e2e.P50),
			a.Cuts, a.Four29, a.Errs,
			a.Stalls, pctCell(a.Stalls, a.Calls),
			a.SlotCalls, tokCell(slots.P90), a.Abandoned)
	}
	b.WriteString("\n")
	b.WriteString("Percentiles are nearest-rank over the sorted values, so each figure is a measured value. ")
	b.WriteString("Decode tok/s is `completion_tokens / ((dur_ms - ttft_ms)/1000)` over completions of at least ")
	fmt.Fprintf(b, "%d tokens; end-to-end tok/s is `completion_tokens / (dur_ms/1000)`.\n", *cfg.Alerts.DecodeMinTokens)
	b.WriteString("A stall is a call whose longest silence between streamed lines reached the stall gap; ")
	b.WriteString("an abandoned slot wait is a call that waited for a slot and then produced no completion.\n\n")
}

// renderByHour writes the first-token p50 table by date and hour (UTC),
// oldest first, one row per hour that saw calls.
func renderByHour(b *strings.Builder, aggs []*modelAgg) {
	type hourKey struct {
		key  string
		when time.Time
	}
	var keys []hourKey
	seen := map[string]bool{}
	for _, a := range aggs {
		for _, h := range a.hours {
			if seen[h.Key] {
				continue
			}
			seen[h.Key] = true
			keys = append(keys, hourKey{key: h.Key, when: h.Start})
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if !keys[i].when.Equal(keys[j].when) {
			return keys[i].when.Before(keys[j].when)
		}
		return keys[i].key < keys[j].key
	})
	b.WriteString("### First token p50 by date and hour (UTC)\n\n")
	if len(keys) == 0 {
		b.WriteString("No calls in the window.\n")
		return
	}
	b.WriteString("| hour (UTC) | model | calls | first token p50 |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, k := range keys {
		for _, a := range aggs {
			h, ok := a.byHour[k.key]
			if !ok {
				continue
			}
			p := summarize(h.TTFT)
			fmt.Fprintf(b, "| %s | %s | %d | %s |\n", k.key, modelIDCell(a.ID), h.Calls, msCell(p.P50))
		}
	}
}

// modelIDCell renders a model id. The id came from models.json, but it is
// still run through escapeMD and bounded: a config is data too, and the
// report is posted on an issue.
func modelIDCell(id string) string {
	if id == "" {
		return "(unnamed)"
	}
	return "`" + escapeMD(id, 80, true) + "`"
}

// msCell renders a millisecond figure as seconds with one decimal, which is
// what the issue's own table shows. A zero-length distribution prints "—".
func msCell(ms float64) string {
	return fmt.Sprintf("%.1f s", ms/1000)
}

// tokCell renders a token rate.
func tokCell(v float64) string { return fmt.Sprintf("%.1f", v) }

// decodeCell renders "p50 (p10-p90)".
func decodeCell(p percentiles) string {
	if !p.hasData {
		return "—"
	}
	return fmt.Sprintf("%.1f (%.1f-%.1f)", p.P50, p.P10, p.P90)
}

// pctCell renders a count as a percentage of n.
func pctCell(count, n int) string {
	if n == 0 {
		return "—"
	}
	return fmt.Sprintf("%.2f%%", percentOf(float64(count), float64(n)))
}

// percentOf is count/n as a percentage, 0 when the denominator is empty.
func percentOf(count, n float64) float64 {
	if n <= 0 {
		return 0
	}
	return count * 100 / n
}

// avgMs is the mean of a set of milliseconds: the ratio alerts compare an
// hour's p50 against the mean of the baseline hours' p50s.
func avgMs(ms []float64) float64 {
	if len(ms) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range ms {
		sum += v
	}
	return sum / float64(len(ms))
}

// ---------------------------------------------------------------- alerts

// modelStatsAlerts returns the alert lines, in a fixed order: the window
// level alerts first (rejected rows), then per model in models.json order,
// each model's alerts in the order the issue lists them. Every threshold is
// read from cfg.Alerts, which loadConfig has already defaulted -- the
// routine hard-codes none of them.
func modelStatsAlerts(in modelStatsInput, aggs []*modelAgg, cfg Config, win modelStatsWindow, now time.Time) []string {
	var out []string
	rejectPct := percentOf(float64(in.Stats.Rejected), float64(in.Stats.Rows+in.Stats.Rejected))
	if rejectPct > cfg.Alerts.RejectPct {
		out = append(out, fmt.Sprintf("rejected rows: %.2f%% of %d row(s) rejected, above the %.2f%% threshold",
			rejectPct, in.Stats.Rows+in.Stats.Rejected, cfg.Alerts.RejectPct))
	}
	baselineHours := *cfg.Alerts.TTFTp50RatioBaselineHours
	for _, a := range aggs {
		if a.Calls == 0 {
			// Issue #320: a model throttled to death has no call row at all --
			// every attempt the proxy made came back 429 and was recorded as a
			// status marker only. The 429-rate alert is the one that carries
			// the operational advice, so it is evaluated here on the markers
			// alone (the denominator is the marker count; percentOf already
			// divides by Calls+markers, and Calls is 0), in the same wording
			// as below, instead of the bare "no calls" line.
			if a.Four29 > 0 {
				if rate := percentOf(float64(a.Four29), float64(countMarkers(in, a.ID))); rate > cfg.Alerts.Rate429Pct {
					out = append(out, fmt.Sprintf("%s: 429 rate %.2f%% above the %.2f%% threshold (%d of %d); "+
						"consider lowering max_concurrent, not raising it",
						modelIDCell(a.ID), rate, cfg.Alerts.Rate429Pct, a.Four29, countMarkers(in, a.ID)))
					continue
				}
			}
			out = append(out, fmt.Sprintf("%s: configured model with no calls in the window", modelIDCell(a.ID)))
			continue
		}
		// The trailing baseline: the per-hour first-token p50s of the hours
		// the model treats as history, averaged. It is computed over the
		// window's own rows -- when the configured baseline (168 h) is longer
		// than the window (72 h by default) it is the older part of the same
		// window, so a report rendered from a 72 h artifact set measures what
		// it has rather than inventing history it does not have. With no
		// baseline hour at all the ratio is not evaluable and cannot fire.
		base := ttftBaseline(a, cfg, win, now)
		limit := base * cfg.Alerts.TTFTp50Ratio
		ratioUsable := base > 0
		overCount := 0
		firstCount := 0
		var firstHour string
		for _, h := range a.hours {
			p := summarize(h.TTFT)
			if h.Calls < *cfg.Alerts.HourCalls {
				continue
			}
			if ratioUsable && p.P50 > limit {
				overCount++
				if firstCount == 0 {
					firstCount, firstHour = h.Calls, h.Key
				}
			}
			if p.P90 > float64(cfg.Alerts.TTFTp90Ms) {
				out = append(out, fmt.Sprintf("%s: hour %s UTC p90 %.1f s above the %.1f s threshold",
					modelIDCell(a.ID), h.Key, p.P90/1000, float64(cfg.Alerts.TTFTp90Ms)/1000))
			}
		}
		if overCount > 0 {
			// Both figures are seconds: the rest of the report states first
			// token times in seconds, and this line is read beside them.
			out = append(out, fmt.Sprintf("%s: %d hour(s) with at least %d calls above %gx the trailing %.1f h first-token p50 ("+
				"%s over %.1f s vs a %.1f s baseline; first was %s UTC with %d calls)",
				modelIDCell(a.ID), overCount, *cfg.Alerts.HourCalls, cfg.Alerts.TTFTp50Ratio, float64(baselineHours),
				modelIDCell(a.ID), avgMsOf(a, firstHour)/1000, base/1000, firstHour, firstCount))
		}
		decode := summarize(a.Decode)
		if base := decodeBaseline(a, cfg, win, now); base > 0 && decode.hasData && decode.P50 < cfg.Alerts.DecodeRatio*base {
			out = append(out, fmt.Sprintf("%s: decode p50 %.1f tok/s below %gx its %.1f tok/s baseline",
				modelIDCell(a.ID), decode.P50, cfg.Alerts.DecodeRatio, base))
		}
		if rate := percentOf(float64(a.Four29), float64(a.Calls+countMarkers(in, a.ID))); rate > cfg.Alerts.Rate429Pct {
			out = append(out, fmt.Sprintf("%s: 429 rate %.2f%% above the %.2f%% threshold (%d of %d); "+
				"consider lowering max_concurrent, not raising it",
				modelIDCell(a.ID), rate, cfg.Alerts.Rate429Pct, a.Four29, a.Calls+countMarkers(in, a.ID)))
		}
		if stallPct := percentOf(float64(a.Stalls), float64(a.Calls)); stallPct > cfg.Alerts.StallPct {
			out = append(out, fmt.Sprintf("%s: %.2f%% of turns stalled, above the %.2f%% threshold (%d of %d)",
				modelIDCell(a.ID), stallPct, cfg.Alerts.StallPct, a.Stalls, a.Calls))
		}
		worst, worstRun := 0, 0
		for run, n := range a.slotWaitsPerRun {
			if n > worst {
				worst, worstRun = n, run
			}
		}
		if worst > *cfg.Alerts.SlotWaitsPerRun {
			out = append(out, fmt.Sprintf("%s: %d slot waits in run %d, over the %d-per-run threshold",
				modelIDCell(a.ID), worst, worstRun, *cfg.Alerts.SlotWaitsPerRun))
		}
		if a.Abandoned > 0 {
			out = append(out, fmt.Sprintf("%s: %d abandoned slot wait(s) — a call queued for a slot and produced no completion",
				modelIDCell(a.ID), a.Abandoned))
		}
		if a.AbandonedHeaders > 0 {
			// A call abandoned while upstream's headers were awaited held
			// its slot and had already been sent upstream (issue #456): it
			// costs real queue time and is invisible in every call figure,
			// so it is reported even when no threshold is crossed.
			out = append(out, fmt.Sprintf("%s: %d call(s) abandoned before upstream headers — a slot held, a request issued and no completion",
				modelIDCell(a.ID), a.AbandonedHeaders))
		}
	}
	return out
}

// avgMsOf is the mean first-token p50 of the named hour bucket.
func avgMsOf(a *modelAgg, hourKey string) float64 {
	h, ok := a.byHour[hourKey]
	if !ok {
		return 0
	}
	return summarize(h.TTFT).P50
}

// baselineFloor is the shortest history a baseline is taken from when the
// window is shorter than the configured baseline: the older part of the
// window itself. An hour younger than this is the "recent" side the
// comparison judges, so it cannot also be the baseline.
const baselineFloor = time.Hour

// trailingBaseline is the trailing baseline a model's recent hours are judged
// against: the mean of the per-hour p50s of the hours it treats as history.
// perHour maps an hour key to that hour's cohort (first-token ms, or decode
// tok/s).
//
// Two cases, and no third:
//
//   - A model with hours older than baselineHours gives the honest figure the
//     issue asks for: hours older than now-baselineHours, in first-seen
//     order, averaged. This is the trailing 7-day p50 when models.json says
//     decode_baseline_hours/ttft_p50_ratio_baseline_hours is 168.
//   - now-baselineHours is before win.From -- the window is shorter than the
//     configured baseline, which is the default 72 h window against a 168 h
//     baseline. Then no hour in the window is old enough, and the baseline is
//     measured from the OLDER PART OF THE SAME WINDOW: the hours older than
//     half the window and at least baselineFloor (1 h) old. A 72 h window
//     therefore compares its recent hours against its own older hours, which
//     is a real measurement of the same window's rows and never data invented
//     from outside it. The cutoff is clamped to win.From for the same reason.
//
// Nothing else is eligible. With no baseline hour -- a single complete hour,
// or all history inside the recent half -- the baseline is empty and the
// caller must not alert: a report that judged one hour against itself would
// alert on noise, and one that invented hours would report what it never saw.
func trailingBaseline(a *modelAgg, now time.Time, win modelStatsWindow, baselineHours int, perHour func(*hourBucket) []float64) []float64 {
	now = now.UTC()
	from := win.From
	if !from.IsZero() && from.After(now.Add(-time.Duration(win.Hours)*time.Hour)) {
		from = now.Add(-time.Duration(win.Hours) * time.Hour)
	}
	cutoff := now.Add(-time.Duration(baselineHours) * time.Hour).UTC()
	clampToWindow := !from.IsZero() && cutoff.Before(from)
	if clampToWindow {
		// The baseline reaches further back than the window does, so the
		// older part of the window is the history: half the window back.
		cutoff = now.Add(-time.Duration(win.Hours) * time.Hour / 2).UTC()
		if cutoff.Before(from) {
			cutoff = from
		}
	}
	var base []float64
	for _, h := range a.hours {
		if !h.Start.Before(cutoff) {
			continue
		}
		if clampToWindow && now.Sub(h.Start) < baselineFloor {
			continue
		}
		values := perHour(h)
		if len(values) == 0 {
			continue
		}
		base = append(base, summarize(values).P50)
	}
	return base
}

// decodeBaseline is the decode-rate baseline a model's decode p50 is judged
// against: the mean of the trailing hours' decode p50s, older hours of the
// same window when the configured history is longer than the window (see
// trailingBaseline). A model with no baseline hour has none -- the alert must
// not fire, which is the bug this replaced: the old baseline was the very p50
// the alert compares, so "decode p50 below 0.7x its baseline" could never be
// true.
func decodeBaseline(a *modelAgg, cfg Config, win modelStatsWindow, now time.Time) float64 {
	hours := *cfg.Alerts.HourCallsBaselineHours
	return avgMs(trailingBaseline(a, now, win, hours, func(h *hourBucket) []float64 { return h.Decode }))
}

// ttftBaseline is the first-token baseline the hourly ratio alert compares
// an hour's p50 against: the mean of the trailing hours' p50s, measured the
// same way as decodeBaseline. The old figure was always 0 at the default
// 72 h window, because every hour bucket sat inside a 168 h cutoff, which
// made the ratio unmeasurable and the alert unreachable.
func ttftBaseline(a *modelAgg, cfg Config, win modelStatsWindow, now time.Time) float64 {
	hours := *cfg.Alerts.TTFTp50RatioBaselineHours
	return avgMs(trailingBaseline(a, now, win, hours, func(h *hourBucket) []float64 { return h.TTFT }))
}

// countMarkers is how many status markers the window carries for a model:
// the error rate's denominator counts a marker as a call the proxy made.
func countMarkers(in modelStatsInput, model string) int {
	n := 0
	for _, m := range in.Stats.Markers {
		// An abandoned marker (issue #456) is not a refusal: it must not
		// widen the denominator of the 429-rate alert, which is what the
		// markers are counted for here.
		if m.Model == model && m.Stage == "" {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- live wrapper

// checkModelStats implements the "model-stats" check routine (issue #131).
//
// Two ways in, one report:
//
//	CODEAF_MODEL_STATS_DIR set
//	    read the codeaf-output zips a workflow already downloaded into that
//	    directory (readModelStatsDir). The asset zip of the harness's own
//	    build -- the binary the workflow downloads alongside the artifacts
//	    -- is skipped by name, so a directory holding both still reports
//	    only ledger runs. This is what a job that has already fetched the
//	    artifacts uses.
//	CODEAF_MODEL_STATS_DIR unset
//	    the routine's own path (modelstats_cli.go): list the recent
//	    completed codeaf.yml runs through the API with an explicit page=
//	    parameter, skip forks and runs without a codeaf-output artifact, and
//	    read each artifact zip in memory. The job needs only actions: read
//	    and issues: write, which is the permission set issue #131 sets; a
//	    job that cannot list runs must set the directory instead.
func checkModelStats() (string, error) {
	now := time.Now().UTC()
	if env := os.Getenv("CODEAF_MODEL_STATS_NOW"); env != "" {
		if ts, err := time.Parse(time.RFC3339, env); err == nil {
			now = ts.UTC()
		}
	}
	if dir := os.Getenv("CODEAF_MODEL_STATS_DIR"); dir != "" {
		cfg, err := modelStatsLoadConfig()
		if err != nil {
			return "", fmt.Errorf("model-stats: %w", err)
		}
		in, err := readModelStatsDir(dir, cfg)
		if err != nil {
			return "", err
		}
		win := modelStatsWindowFrom(cfg, now)
		return modelStatsReport(in, cfg, win, now), nil
	}
	return runModelStatsRoutineAt(now)
}

// runModelStatsRoutineAt is the routine's API path at a given instant, so the
// window and the report agree on the clock (a test can pin it).
func runModelStatsRoutineAt(now time.Time) (string, error) {
	cfg, err := modelStatsLoadConfig()
	if err != nil {
		return "", fmt.Errorf("model-stats: %w", err)
	}
	win := modelStatsWindowFrom(cfg, now)
	in, err := readModelStatsFromRunsConfig(cfg, win)
	if err != nil {
		return "", err
	}
	return modelStatsReport(in, cfg, win, now), nil
}

// modelStatsWindowFrom is the window the routine reports over: WindowHours
// back from now, per models.json.
func modelStatsWindowFrom(cfg Config, now time.Time) modelStatsWindow {
	hours := *cfg.Alerts.WindowHours
	return modelStatsWindow{Hours: hours, From: now.Add(-time.Duration(hours) * time.Hour), To: now}
}
