// Command copyguard-replay audits Copy Guard cycle exports without touching a
// live exchange. It deliberately computes realized attribution only from
// reconciled, closed cycles and never guesses missing AI decisions.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type cycleExport struct {
	SchemaVersion int                        `json:"schema_version"`
	Cycle         replayCycle                `json:"cycle"`
	Attempts      []replayAttempt            `json:"attempts"`
	Events        []replayEvent              `json:"events"`
	WatchSamples  []map[string]any           `json:"watch_samples"`
	AICandidates  []json.RawMessage          `json:"ai_candidates"`
	AIAnalyses    []json.RawMessage          `json:"ai_analyses"`
	Raw           map[string]json.RawMessage `json:"-"`
}

type replayCycle struct {
	ID               int64   `json:"id"`
	Symbol           string  `json:"symbol"`
	Side             string  `json:"side"`
	Status           string  `json:"status"`
	AccountingStatus string  `json:"accounting_status"`
	ActualPnL        float64 `json:"actual_pnl"`
	BaselinePnL      float64 `json:"baseline_pnl"`
	Fees             float64 `json:"fees"`
	Slippage         float64 `json:"slippage"`
	StopCount        int     `json:"stop_count"`
	ReentryCount     int     `json:"reentry_count"`
	ClosedAt         any     `json:"closed_at"`
}

type replayAttempt struct {
	AttemptNo  int     `json:"attempt_no"`
	Status     string  `json:"status"`
	PnL        float64 `json:"pnl"`
	Fee        float64 `json:"fee"`
	Reconciled bool    `json:"reconciled"`
}

type replayEvent struct {
	Type     string         `json:"type"`
	Metadata map[string]any `json:"metadata"`
}

type attemptStats struct {
	Count   int     `json:"count"`
	Wins    int     `json:"wins"`
	Sum     float64 `json:"sum_pnl"`
	Median  float64 `json:"median_pnl"`
	WinRate float64 `json:"win_rate"`
}

type replayReport struct {
	Files                     int                  `json:"files"`
	FinalCycles               int                  `json:"final_cycles"`
	ExcludedCycles            int                  `json:"excluded_cycles"`
	ActualCopyGuardPnL        float64              `json:"actual_copy_guard_pnl"`
	BaselineNoGuardPnL        float64              `json:"baseline_no_guard_pnl"`
	StopOnlyPnL               float64              `json:"stop_only_pnl"`
	NetGuardEffect            float64              `json:"net_guard_effect"`
	StopOnlyVsBaseline        float64              `json:"stop_only_vs_baseline"`
	AllReentryContribution    float64              `json:"all_reentry_contribution"`
	Fees                      float64              `json:"fees"`
	Slippage                  float64              `json:"slippage"`
	Attempts                  map[int]attemptStats `json:"attempts"`
	AttemptAccountingMismatch int                  `json:"attempt_accounting_mismatch_cycles"`
	NegativeRecoveryTimes     int                  `json:"negative_recovery_times"`
	AmbiguousWatchCycles      int                  `json:"legacy_watch_cycles_without_attempt_no"`
	DuplicateReentryRequests  int                  `json:"duplicate_reentry_request_events"`
	AIReplayCycles            int                  `json:"ai_replay_cycles"`
	AIReplayComplete          bool                 `json:"ai_replay_complete"`
	AIReplayNote              string               `json:"ai_replay_note"`
	Schemas                   map[int]int          `json:"schemas"`
}

func main() {
	jsonOutput := flag.Bool("json", false, "print machine-readable JSON")
	flag.Parse()
	paths, err := expandPaths(flag.Args())
	if err != nil {
		fatal(err)
	}
	if len(paths) == 0 {
		paths, _ = filepath.Glob("rz/copy-guard-cycle-*.jsonl")
	}
	if len(paths) == 0 {
		fatal(fmt.Errorf("no cycle exports supplied or found under rz/"))
	}
	exports, err := readExports(paths)
	if err != nil {
		fatal(err)
	}
	report := analyze(exports)
	if *jsonOutput {
		b, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(b))
		return
	}
	printHuman(report)
}

func expandPaths(args []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, arg := range args {
		info, err := os.Stat(arg)
		if err == nil && info.IsDir() {
			matches, globErr := filepath.Glob(filepath.Join(arg, "copy-guard-cycle-*.jsonl"))
			if globErr != nil {
				return nil, globErr
			}
			for _, path := range matches {
				if !seen[path] {
					seen[path], out = true, append(out, path)
				}
			}
			continue
		}
		matches, globErr := filepath.Glob(arg)
		if globErr != nil {
			return nil, globErr
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("path does not exist: %s", arg)
		}
		for _, path := range matches {
			if !seen[path] {
				seen[path], out = true, append(out, path)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func readExports(paths []string) ([]cycleExport, error) {
	var out []cycleExport
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
		line := 0
		for scanner.Scan() {
			line++
			if strings.TrimSpace(scanner.Text()) == "" {
				continue
			}
			var item cycleExport
			if err := json.Unmarshal(scanner.Bytes(), &item); err != nil {
				_ = f.Close()
				return nil, fmt.Errorf("%s:%d: %w", path, line, err)
			}
			if item.SchemaVersion < 3 {
				_ = f.Close()
				return nil, fmt.Errorf("%s:%d: unsupported schema_version %d", path, line, item.SchemaVersion)
			}
			out = append(out, item)
		}
		if err := scanner.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		_ = f.Close()
	}
	return out, nil
}

func analyze(exports []cycleExport) replayReport {
	r := replayReport{Files: len(exports), Attempts: map[int]attemptStats{}, Schemas: map[int]int{}}
	values := map[int][]float64{}
	for _, item := range exports {
		r.Schemas[item.SchemaVersion]++
		if item.Cycle.AccountingStatus != "RECONCILED" || item.Cycle.ClosedAt == nil {
			r.ExcludedCycles++
			continue
		}
		r.FinalCycles++
		r.ActualCopyGuardPnL += item.Cycle.ActualPnL
		r.BaselineNoGuardPnL += item.Cycle.BaselinePnL
		r.Fees += item.Cycle.Fees
		r.Slippage += item.Cycle.Slippage
		attemptTotal := 0.0
		for _, attempt := range item.Attempts {
			if !attempt.Reconciled {
				continue
			}
			attemptTotal += attempt.PnL
			values[attempt.AttemptNo] = append(values[attempt.AttemptNo], attempt.PnL)
			if attempt.AttemptNo == 0 {
				r.StopOnlyPnL += attempt.PnL
			} else {
				r.AllReentryContribution += attempt.PnL
			}
		}
		if math.Abs(attemptTotal-item.Cycle.ActualPnL) > 1e-6 {
			r.AttemptAccountingMismatch++
		}
		for _, event := range item.Events {
			if event.Type == "WATCH_SUMMARY" && number(event.Metadata["first_recovery_seconds"]) < 0 {
				r.NegativeRecoveryTimes++
			}
		}
		r.DuplicateReentryRequests += duplicateReentryRequests(item.Events)
		if item.Cycle.ReentryCount > 0 && hasWatchWithoutAttempt(item.WatchSamples) {
			r.AmbiguousWatchCycles++
		}
		if item.SchemaVersion >= 4 && len(item.AICandidates) > 0 && len(item.AIAnalyses) > 0 {
			r.AIReplayCycles++
		}
	}
	r.NetGuardEffect = r.ActualCopyGuardPnL - r.BaselineNoGuardPnL
	r.StopOnlyVsBaseline = r.StopOnlyPnL - r.BaselineNoGuardPnL
	for attemptNo, xs := range values {
		stats := attemptStats{Count: len(xs), Sum: sum(xs), Median: median(xs)}
		for _, value := range xs {
			if value > 0 {
				stats.Wins++
			}
		}
		if stats.Count > 0 {
			stats.WinRate = float64(stats.Wins) / float64(stats.Count)
		}
		r.Attempts[attemptNo] = stats
	}
	r.AIReplayComplete = r.AIReplayCycles == r.FinalCycles && r.FinalCycles > 0
	if r.AIReplayComplete {
		r.AIReplayNote = "all finalized exports contain schema-v4 AI candidates and analyses"
	} else {
		r.AIReplayNote = "realized attribution is reproducible; historical AI decisions are not inferred when candidate analyses or point-in-time market snapshots are absent"
	}
	return r
}

func duplicateReentryRequests(events []replayEvent) int {
	seen := map[string]bool{}
	duplicates := 0
	for _, event := range events {
		if event.Type != "REENTRY_REQUESTED" {
			continue
		}
		b, _ := json.Marshal(event.Metadata)
		key := string(b)
		if seen[key] {
			duplicates++
		} else {
			seen[key] = true
		}
	}
	return duplicates
}

func hasWatchWithoutAttempt(samples []map[string]any) bool {
	for _, sample := range samples {
		if _, ok := sample["attempt_no"]; !ok {
			return true
		}
	}
	return false
}

func number(v any) float64 {
	n, _ := v.(float64)
	return n
}

func sum(xs []float64) float64 {
	total := 0.0
	for _, x := range xs {
		total += x
	}
	return total
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid]
	}
	return (cp[mid-1] + cp[mid]) / 2
}

func printHuman(r replayReport) {
	fmt.Printf("Copy Guard offline replay: %d finalized cycles (%d excluded)\n", r.FinalCycles, r.ExcludedCycles)
	fmt.Printf("  actual Copy Guard:      %+.6f USDT\n", r.ActualCopyGuardPnL)
	fmt.Printf("  no-guard baseline:      %+.6f USDT\n", r.BaselineNoGuardPnL)
	fmt.Printf("  net guard effect:       %+.6f USDT\n", r.NetGuardEffect)
	fmt.Printf("  stop-only counterfact:  %+.6f USDT (vs baseline %+.6f)\n", r.StopOnlyPnL, r.StopOnlyVsBaseline)
	fmt.Printf("  all reentry increment:  %+.6f USDT\n", r.AllReentryContribution)
	keys := make([]int, 0, len(r.Attempts))
	for key := range r.Attempts {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	for _, key := range keys {
		stats := r.Attempts[key]
		fmt.Printf("  attempt %d: count=%d wins=%d win_rate=%.1f%% sum=%+.6f median=%+.6f\n", key, stats.Count, stats.Wins, stats.WinRate*100, stats.Sum, stats.Median)
	}
	fmt.Printf("  audit: accounting_mismatch=%d negative_recovery=%d legacy_watch_ambiguous=%d duplicate_reentry_requests=%d\n", r.AttemptAccountingMismatch, r.NegativeRecoveryTimes, r.AmbiguousWatchCycles, r.DuplicateReentryRequests)
	fmt.Printf("  AI replay: %s (complete=%t, cycles=%d/%d)\n", r.AIReplayNote, r.AIReplayComplete, r.AIReplayCycles, r.FinalCycles)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "copyguard-replay:", err)
	os.Exit(1)
}
