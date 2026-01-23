package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type CommandInfo struct {
	Elapsed    float64 `json:"elapsed_seconds"`
	Success    bool    `json:"success"`
	TimedOut   bool    `json:"timed_out,omitempty"`
	OutputSize int     `json:"output_size,omitempty"`
	DepCount   int     `json:"dependency_count,omitempty"`
}

type InitInfo struct {
	Elapsed  float64 `json:"elapsed_seconds"`
	Success  bool    `json:"success"`
	TimedOut bool    `json:"timed_out,omitempty"`
}

type RepoStats struct {
	Commits  int `json:"commits"`
	Branches int `json:"branches"`
}

type TestResult struct {
	Repo      string                 `json:"repo"`
	Name      string                 `json:"name"`
	TestedAt  string                 `json:"tested_at"`
	Version   string                 `json:"git_pkgs_version"`
	RepoStats *RepoStats             `json:"repo_stats,omitempty"`
	Init      *InitInfo              `json:"init,omitempty"`
	DBInfo    map[string]any         `json:"db_info,omitempty"`
	Commands  map[string]any         `json:"commands,omitempty"`
	RawStats  map[string]any         `json:"stats,omitempty"`
}

func formatSeconds(s float64) string {
	if s < 0 {
		return "-"
	}
	if s < 60 {
		return fmt.Sprintf("%.1fs", s)
	}
	mins := int(s) / 60
	secs := int(s) % 60
	return fmt.Sprintf("%dm%ds", mins, secs)
}

func formatBytes(b int64) string {
	if b < 0 {
		return "-"
	}
	if b < 1024 {
		return fmt.Sprintf("%dB", b)
	}
	if b < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(b)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(b)/1024/1024)
}

func formatNumber(n int) string {
	if n <= 0 {
		return "-"
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	return strings.Join(parts, ",")
}

func getDBSize(result TestResult) int64 {
	if result.DBInfo == nil {
		return -1
	}
	if v, ok := result.DBInfo["size_bytes"].(float64); ok {
		return int64(v)
	}
	if v, ok := result.DBInfo["size"].(float64); ok {
		return int64(v)
	}
	return -1
}

func getStats(result TestResult) map[string]any {
	// Check RawStats first (Go format)
	if result.RawStats != nil {
		return result.RawStats
	}
	// Fall back to commands.stats (Ruby format)
	if result.Commands != nil {
		if stats, ok := result.Commands["stats"].(map[string]any); ok {
			return stats
		}
	}
	return nil
}

func getDepCount(result TestResult) int {
	// Try stats first
	stats := getStats(result)
	if stats != nil {
		if v, ok := stats["current_dependencies"].(float64); ok && v > 0 {
			return int(v)
		}
		if v, ok := stats["current_deps"].(float64); ok && v > 0 {
			return int(v)
		}
		if current, ok := stats["current"].(map[string]any); ok {
			if v, ok := current["total"].(float64); ok && v > 0 {
				return int(v)
			}
		}
	}
	// Fall back to list.dependency_count
	if result.Commands != nil {
		if listAny, ok := result.Commands["list"].(map[string]any); ok {
			if v, ok := listAny["dependency_count"].(float64); ok {
				return int(v)
			}
		}
	}
	return 0
}

func getChangeCount(result TestResult) int {
	stats := getStats(result)
	if stats == nil {
		return 0
	}
	if v, ok := stats["total_changes"].(float64); ok {
		return int(v)
	}
	if changes, ok := stats["changes"].(map[string]any); ok {
		if v, ok := changes["total"].(float64); ok {
			return int(v)
		}
	}
	return 0
}

func loadResults(dir string) ([]TestResult, error) {
	pattern := filepath.Join(dir, "*.json")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no results found in %s", dir)
	}

	// Group by repo, keep most recent
	byRepo := make(map[string]TestResult)
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var result TestResult
		if err := json.Unmarshal(data, &result); err != nil {
			continue
		}
		if existing, ok := byRepo[result.Name]; !ok || result.TestedAt > existing.TestedAt {
			byRepo[result.Name] = result
		}
	}

	var results []TestResult
	for _, r := range byRepo {
		results = append(results, r)
	}
	sort.Slice(results, func(i, j int) bool {
		return strings.ToLower(results[i].Name) < strings.ToLower(results[j].Name)
	})

	return results, nil
}

func cmdResult(result TestResult, name string) string {
	if result.Commands == nil {
		return "-"
	}
	cmdAny, ok := result.Commands[name]
	if !ok {
		return "-"
	}
	cmd, ok := cmdAny.(map[string]any)
	if !ok {
		return "-"
	}

	if timedOut, ok := cmd["timed_out"].(bool); ok && timedOut {
		return "TIMEOUT"
	}
	if success, ok := cmd["success"].(bool); ok && !success {
		return "FAIL"
	}
	if elapsed, ok := cmd["elapsed_seconds"].(float64); ok {
		return formatSeconds(elapsed)
	}
	return "-"
}

func printTable(results []TestResult) {
	headers := []string{"Repo", "Commits", "Init", "DB Size", "Deps", "Changes", "list", "blame", "history"}

	// Build rows
	var rows [][]string
	for _, r := range results {
		initTime := "-"
		if r.Init != nil {
			if r.Init.TimedOut {
				initTime = "TIMEOUT"
			} else if !r.Init.Success {
				initTime = "FAIL"
			} else {
				initTime = formatSeconds(r.Init.Elapsed)
			}
		}

		commits := 0
		if r.RepoStats != nil {
			commits = r.RepoStats.Commits
		}

		row := []string{
			r.Name,
			formatNumber(commits),
			initTime,
			formatBytes(getDBSize(r)),
			formatNumber(getDepCount(r)),
			formatNumber(getChangeCount(r)),
			cmdResult(r, "list"),
			cmdResult(r, "blame"),
			cmdResult(r, "history"),
		}
		rows = append(rows, row)
	}

	// Calculate widths
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	// Print header
	for i, h := range headers {
		fmt.Printf("%-*s  ", widths[i], h)
	}
	fmt.Println()

	// Separator
	total := 0
	for _, w := range widths {
		total += w + 2
	}
	fmt.Println(strings.Repeat("-", total))

	// Print rows
	for _, row := range rows {
		for i, cell := range row {
			fmt.Printf("%-*s  ", widths[i], cell)
		}
		fmt.Println()
	}
}

func printMarkdown(results []TestResult) {
	headers := []string{"Repo", "Commits", "Init", "DB Size", "Deps", "Changes"}

	fmt.Printf("| %s |\n", strings.Join(headers, " | "))
	fmt.Printf("|%s|\n", strings.Repeat("---|", len(headers)))

	for _, r := range results {
		initTime := "-"
		if r.Init != nil {
			if r.Init.TimedOut {
				initTime = "TIMEOUT"
			} else if !r.Init.Success {
				initTime = "FAIL"
			} else {
				initTime = formatSeconds(r.Init.Elapsed)
			}
		}

		commits := 0
		if r.RepoStats != nil {
			commits = r.RepoStats.Commits
		}

		fmt.Printf("| %s | %s | %s | %s | %s | %s |\n",
			r.Name,
			formatNumber(commits),
			initTime,
			formatBytes(getDBSize(r)),
			formatNumber(getDepCount(r)),
			formatNumber(getChangeCount(r)),
		)
	}
}

func printCSV(results []TestResult, w *csv.Writer) {
	headers := []string{"repo", "commits", "init_seconds", "db_bytes", "deps", "changes", "list_seconds", "blame_seconds", "history_seconds"}
	w.Write(headers)

	for _, r := range results {
		commits := 0
		if r.RepoStats != nil {
			commits = r.RepoStats.Commits
		}

		initSeconds := 0.0
		if r.Init != nil && r.Init.Success {
			initSeconds = r.Init.Elapsed
		}

		row := []string{
			r.Name,
			fmt.Sprintf("%d", commits),
			fmt.Sprintf("%.3f", initSeconds),
			fmt.Sprintf("%d", getDBSize(r)),
			fmt.Sprintf("%d", getDepCount(r)),
			fmt.Sprintf("%d", getChangeCount(r)),
			fmt.Sprintf("%.3f", getCmdElapsed(r, "list")),
			fmt.Sprintf("%.3f", getCmdElapsed(r, "blame")),
			fmt.Sprintf("%.3f", getCmdElapsed(r, "history")),
		}
		w.Write(row)
	}
	w.Flush()
}

func getCmdElapsed(result TestResult, name string) float64 {
	if result.Commands == nil {
		return 0
	}
	cmdAny, ok := result.Commands[name]
	if !ok {
		return 0
	}
	cmd, ok := cmdAny.(map[string]any)
	if !ok {
		return 0
	}
	if elapsed, ok := cmd["elapsed_seconds"].(float64); ok {
		return elapsed
	}
	return 0
}

func main() {
	execDir, _ := os.Getwd()
	resultsDir := filepath.Join(execDir, "results")

	markdown := false
	csvOutput := ""
	for i, arg := range os.Args[1:] {
		if arg == "--markdown" || arg == "-m" {
			markdown = true
		} else if arg == "--csv" && i+2 < len(os.Args) {
			csvOutput = os.Args[i+2]
		} else if strings.HasPrefix(arg, "--csv=") {
			csvOutput = strings.TrimPrefix(arg, "--csv=")
		}
	}

	results, err := loadResults(resultsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	// CSV output to file
	if csvOutput != "" {
		f, err := os.Create(csvOutput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to create CSV file: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		w := csv.NewWriter(f)
		printCSV(results, w)
		fmt.Printf("Wrote %d results to %s\n", len(results), csvOutput)
		return
	}

	fmt.Println("git-pkgs Test Results")
	fmt.Printf("%d repos tested\n", len(results))
	if len(results) > 0 {
		fmt.Printf("Version: %s\n", results[0].Version)
	}
	fmt.Println()

	if markdown {
		printMarkdown(results)
	} else {
		printTable(results)
	}
}
