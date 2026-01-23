package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var timeouts = map[string]time.Duration{
	"clone":   5 * time.Minute,
	"init":    10 * time.Minute,
	"command": 2 * time.Minute,
	"history": 3 * time.Minute,
	"blame":   3 * time.Minute,
}

type CommandResult struct {
	Stdout   string  `json:"stdout,omitempty"`
	Stderr   string  `json:"stderr,omitempty"`
	ExitCode int     `json:"exit_code"`
	Elapsed  float64 `json:"elapsed_seconds"`
	TimedOut bool    `json:"timed_out,omitempty"`
}

type RepoStats struct {
	Commits  int `json:"commits"`
	Branches int `json:"branches"`
}

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
	Stderr   string  `json:"stderr,omitempty"`
}

type CloneInfo struct {
	Elapsed  float64 `json:"elapsed_seconds"`
	Success  bool    `json:"success"`
	TimedOut bool    `json:"timed_out,omitempty"`
}

type TestResult struct {
	Repo       string                 `json:"repo"`
	Name       string                 `json:"name"`
	TestedAt   string                 `json:"tested_at"`
	Version    string                 `json:"git_pkgs_version"`
	Clone      *CloneInfo             `json:"clone,omitempty"`
	RepoStats  *RepoStats             `json:"repo_stats,omitempty"`
	Init       *InitInfo              `json:"init,omitempty"`
	DBInfo     map[string]any         `json:"db_info,omitempty"`
	Manifests  int                    `json:"manifests"`
	Ecosystems []string               `json:"ecosystems,omitempty"`
	Commands   map[string]CommandInfo `json:"commands,omitempty"`
	RawStats   map[string]any         `json:"stats,omitempty"`
}

func runCommand(ctx context.Context, name string, args []string, dir string) CommandResult {
	start := time.Now()

	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}

	// Create new process group so we can kill all children
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Start()
	if err != nil {
		return CommandResult{
			Stderr:   err.Error(),
			ExitCode: -1,
			Elapsed:  time.Since(start).Seconds(),
		}
	}

	// Wait for completion or context cancellation
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	var waitErr error
	select {
	case waitErr = <-done:
		// Command completed normally
	case <-ctx.Done():
		// Timeout - kill the process group
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done // Wait for the process to actually exit
		return CommandResult{
			Stdout:   stdout.String(),
			Stderr:   "TIMEOUT",
			ExitCode: -1,
			Elapsed:  time.Since(start).Seconds(),
			TimedOut: true,
		}
	}

	elapsed := time.Since(start).Seconds()
	result := CommandResult{
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
		Elapsed: elapsed,
	}

	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = -1
		}
	}

	return result
}

func runCommandTimeout(name string, args []string, dir string, timeout time.Duration) CommandResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return runCommand(ctx, name, args, dir)
}

func runShell(cmd string, dir string, timeout time.Duration) CommandResult {
	return runCommandTimeout("sh", []string{"-c", cmd}, dir, timeout)
}

func testRepo(repoURL string, cacheDir string, forceClone bool) TestResult {
	parts := strings.Split(repoURL, "/")
	repoName := strings.TrimSuffix(parts[len(parts)-1], ".git")
	repoPath := filepath.Join(cacheDir, repoName)

	fmt.Printf("Testing %s...\n", repoName)

	result := TestResult{
		Repo:     repoURL,
		Name:     repoName,
		TestedAt: time.Now().UTC().Format(time.RFC3339),
		Commands: make(map[string]CommandInfo),
	}

	// Get version
	versionRes := runCommandTimeout("git", []string{"pkgs", "version"}, "", timeouts["command"])
	result.Version = strings.TrimSpace(versionRes.Stdout)

	// Clone or reuse existing
	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err == nil && !forceClone {
		fmt.Println("  Using cached clone...")
		// Pull latest
		pullRes := runCommandTimeout("git", []string{"pull", "--quiet"}, repoPath, timeouts["clone"])
		result.Clone = &CloneInfo{
			Elapsed:  pullRes.Elapsed,
			Success:  pullRes.ExitCode == 0,
			TimedOut: pullRes.TimedOut,
		}
	} else {
		if forceClone {
			os.RemoveAll(repoPath)
		}
		fmt.Println("  Cloning...")
		cloneRes := runCommandTimeout("git", []string{"clone", "--quiet", repoURL, repoPath}, "", timeouts["clone"])
		result.Clone = &CloneInfo{
			Elapsed:  cloneRes.Elapsed,
			Success:  cloneRes.ExitCode == 0,
			TimedOut: cloneRes.TimedOut,
		}
		if cloneRes.ExitCode != 0 {
			return result
		}
	}

	// Repo stats
	fmt.Println("  Gathering repo stats...")
	commitRes := runShell("git rev-list --count HEAD", repoPath, timeouts["command"])
	branchRes := runShell("git branch -r | wc -l", repoPath, timeouts["command"])

	commits, _ := strconv.Atoi(strings.TrimSpace(commitRes.Stdout))
	branches, _ := strconv.Atoi(strings.TrimSpace(branchRes.Stdout))
	result.RepoStats = &RepoStats{Commits: commits, Branches: branches}

	// Init
	fmt.Printf("  Running init (timeout: %s)...\n", timeouts["init"])
	initRes := runCommandTimeout("git", []string{"pkgs", "init"}, repoPath, timeouts["init"])
	result.Init = &InitInfo{
		Elapsed:  initRes.Elapsed,
		Success:  initRes.ExitCode == 0,
		TimedOut: initRes.TimedOut,
		Stderr:   initRes.Stderr,
	}
	if initRes.ExitCode != 0 {
		return result
	}

	// DB info
	fmt.Println("  Getting db info...")
	infoRes := runCommandTimeout("git", []string{"pkgs", "info", "--format=json"}, repoPath, timeouts["command"])
	if infoRes.ExitCode == 0 {
		var info map[string]any
		if err := json.Unmarshal([]byte(infoRes.Stdout), &info); err == nil {
			result.DBInfo = info
			// Extract manifests count
			if rowCounts, ok := info["row_counts"].(map[string]any); ok {
				if manifests, ok := rowCounts["manifests"].(float64); ok {
					result.Manifests = int(manifests)
				}
			}
			// Extract ecosystems
			if ecosystems, ok := info["ecosystems"].([]any); ok {
				for _, e := range ecosystems {
					if s, ok := e.(string); ok {
						result.Ecosystems = append(result.Ecosystems, s)
					}
				}
			}
		}
	}

	// Stats
	fmt.Println("  Getting stats...")
	statsRes := runCommandTimeout("git", []string{"pkgs", "stats", "--format=json"}, repoPath, timeouts["command"])
	if statsRes.ExitCode == 0 {
		var stats map[string]any
		if err := json.Unmarshal([]byte(statsRes.Stdout), &stats); err == nil {
			result.RawStats = stats
		}
	}

	// List
	fmt.Println("  Running list...")
	listRes := runCommandTimeout("git", []string{"pkgs", "list", "--format=json"}, repoPath, timeouts["command"])
	listInfo := CommandInfo{
		Elapsed:    listRes.Elapsed,
		Success:    listRes.ExitCode == 0,
		TimedOut:   listRes.TimedOut,
		OutputSize: len(listRes.Stdout),
	}
	if listRes.ExitCode == 0 {
		var deps []any
		if err := json.Unmarshal([]byte(listRes.Stdout), &deps); err == nil {
			listInfo.DepCount = len(deps)
		}
	}
	result.Commands["list"] = listInfo

	// Blame
	fmt.Printf("  Running blame (timeout: %s)...\n", timeouts["blame"])
	blameRes := runCommandTimeout("git", []string{"pkgs", "blame", "--format=json"}, repoPath, timeouts["blame"])
	result.Commands["blame"] = CommandInfo{
		Elapsed:  blameRes.Elapsed,
		Success:  blameRes.ExitCode == 0,
		TimedOut: blameRes.TimedOut,
	}

	// History
	fmt.Printf("  Running history (timeout: %s)...\n", timeouts["history"])
	historyRes := runCommandTimeout("git", []string{"pkgs", "history", "--format=json"}, repoPath, timeouts["history"])
	result.Commands["history"] = CommandInfo{
		Elapsed:    historyRes.Elapsed,
		Success:    historyRes.ExitCode == 0,
		TimedOut:   historyRes.TimedOut,
		OutputSize: len(historyRes.Stdout),
	}

	// Stale
	fmt.Println("  Running stale...")
	staleRes := runCommandTimeout("git", []string{"pkgs", "stale", "--format=json"}, repoPath, timeouts["command"])
	result.Commands["stale"] = CommandInfo{
		Elapsed:  staleRes.Elapsed,
		Success:  staleRes.ExitCode == 0,
		TimedOut: staleRes.TimedOut,
	}

	// Log
	fmt.Println("  Running log...")
	logRes := runCommandTimeout("git", []string{"pkgs", "log", "--format=json"}, repoPath, timeouts["command"])
	result.Commands["log"] = CommandInfo{
		Elapsed:    logRes.Elapsed,
		Success:    logRes.ExitCode == 0,
		TimedOut:   logRes.TimedOut,
		OutputSize: len(logRes.Stdout),
	}

	// Tree
	fmt.Println("  Running tree...")
	treeRes := runCommandTimeout("git", []string{"pkgs", "tree", "--format=json"}, repoPath, timeouts["command"])
	result.Commands["tree"] = CommandInfo{
		Elapsed:  treeRes.Elapsed,
		Success:  treeRes.ExitCode == 0,
		TimedOut: treeRes.TimedOut,
	}

	// Licenses
	fmt.Println("  Running licenses...")
	licensesRes := runCommandTimeout("git", []string{"pkgs", "licenses", "--format=json"}, repoPath, timeouts["command"])
	result.Commands["licenses"] = CommandInfo{
		Elapsed:  licensesRes.Elapsed,
		Success:  licensesRes.ExitCode == 0,
		TimedOut: licensesRes.TimedOut,
	}

	// Search (search for a common term)
	fmt.Println("  Running search...")
	searchRes := runCommandTimeout("git", []string{"pkgs", "search", "test", "--format=json"}, repoPath, timeouts["command"])
	result.Commands["search"] = CommandInfo{
		Elapsed:  searchRes.Elapsed,
		Success:  searchRes.ExitCode == 0,
		TimedOut: searchRes.TimedOut,
	}

	// Diff (compare HEAD with HEAD~10 if enough commits)
	fmt.Println("  Running diff...")
	diffRes := runCommandTimeout("git", []string{"pkgs", "diff", "HEAD~10", "HEAD", "--format=json"}, repoPath, timeouts["command"])
	result.Commands["diff"] = CommandInfo{
		Elapsed:  diffRes.Elapsed,
		Success:  diffRes.ExitCode == 0,
		TimedOut: diffRes.TimedOut,
	}

	// Outdated
	fmt.Println("  Running outdated...")
	outdatedRes := runCommandTimeout("git", []string{"pkgs", "outdated", "--format=json"}, repoPath, timeouts["history"])
	result.Commands["outdated"] = CommandInfo{
		Elapsed:  outdatedRes.Elapsed,
		Success:  outdatedRes.ExitCode == 0,
		TimedOut: outdatedRes.TimedOut,
	}

	// Re-run licenses and outdated with filled database
	fmt.Println("  Running licenses (warm)...")
	licensesWarmRes := runCommandTimeout("git", []string{"pkgs", "licenses", "--format=json"}, repoPath, timeouts["command"])
	result.Commands["licenses_warm"] = CommandInfo{
		Elapsed:  licensesWarmRes.Elapsed,
		Success:  licensesWarmRes.ExitCode == 0,
		TimedOut: licensesWarmRes.TimedOut,
	}

	fmt.Println("  Running outdated (warm)...")
	outdatedWarmRes := runCommandTimeout("git", []string{"pkgs", "outdated", "--format=json"}, repoPath, timeouts["history"])
	result.Commands["outdated_warm"] = CommandInfo{
		Elapsed:  outdatedWarmRes.Elapsed,
		Success:  outdatedWarmRes.ExitCode == 0,
		TimedOut: outdatedWarmRes.TimedOut,
	}

	return result
}

func saveResult(result TestResult, resultsDir string) error {
	if err := os.MkdirAll(resultsDir, 0755); err != nil {
		return err
	}

	filename := fmt.Sprintf("%s-%s.json", result.Name, time.Now().Format("20060102-150405"))
	filepath := filepath.Join(resultsDir, filename)

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}

	if err := os.WriteFile(filepath, data, 0644); err != nil {
		return err
	}

	fmt.Printf("  Saved to %s\n", filepath)
	return nil
}

func loadRepos(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var repos []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			repos = append(repos, line)
		}
	}
	return repos, scanner.Err()
}

// LoadedResult is used when reading results from JSON files for display
// Commands is map[string]any because JSON unmarshal doesn't know the CommandInfo type
type LoadedResult struct {
	Repo       string           `json:"repo"`
	Name       string           `json:"name"`
	TestedAt   string           `json:"tested_at"`
	Version    string           `json:"git_pkgs_version"`
	RepoStats  *RepoStats       `json:"repo_stats,omitempty"`
	Init       *InitInfo        `json:"init,omitempty"`
	DBInfo     map[string]any   `json:"db_info,omitempty"`
	Manifests  int              `json:"manifests"`
	Ecosystems []string         `json:"ecosystems,omitempty"`
	Commands   map[string]any   `json:"commands,omitempty"`
	RawStats   map[string]any   `json:"stats,omitempty"`
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

func getDBSize(result LoadedResult) int64 {
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

func getStats(result LoadedResult) map[string]any {
	if result.RawStats != nil {
		return result.RawStats
	}
	if result.Commands != nil {
		if stats, ok := result.Commands["stats"].(map[string]any); ok {
			return stats
		}
	}
	return nil
}

func getDepCount(result LoadedResult) int {
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
	if result.Commands != nil {
		if listAny, ok := result.Commands["list"].(map[string]any); ok {
			if v, ok := listAny["dependency_count"].(float64); ok {
				return int(v)
			}
		}
	}
	return 0
}

func getChangeCount(result LoadedResult) int {
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

func loadResults(dir string) ([]LoadedResult, error) {
	pattern := filepath.Join(dir, "*.json")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no results found in %s", dir)
	}

	byRepo := make(map[string]LoadedResult)
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var result LoadedResult
		if err := json.Unmarshal(data, &result); err != nil {
			continue
		}
		if existing, ok := byRepo[result.Name]; !ok || result.TestedAt > existing.TestedAt {
			byRepo[result.Name] = result
		}
	}

	var results []LoadedResult
	for _, r := range byRepo {
		results = append(results, r)
	}
	sort.Slice(results, func(i, j int) bool {
		return strings.ToLower(results[i].Name) < strings.ToLower(results[j].Name)
	})

	return results, nil
}

func cmdResult(result LoadedResult, name string) string {
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

func getCmdElapsed(result LoadedResult, name string) float64 {
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

func printTable(results []LoadedResult) {
	headers := []string{"Repo", "Commits", "Init", "DB Size", "Manifests", "Ecosystems", "Deps", "Changes", "list", "blame", "history", "stale", "log", "tree", "licenses", "licenses_warm", "search", "diff", "outdated", "outdated_warm"}

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

		ecosystems := strings.Join(r.Ecosystems, ",")
		if ecosystems == "" {
			ecosystems = "-"
		}

		row := []string{
			r.Name,
			formatNumber(commits),
			initTime,
			formatBytes(getDBSize(r)),
			formatNumber(r.Manifests),
			ecosystems,
			formatNumber(getDepCount(r)),
			formatNumber(getChangeCount(r)),
			cmdResult(r, "list"),
			cmdResult(r, "blame"),
			cmdResult(r, "history"),
			cmdResult(r, "stale"),
			cmdResult(r, "log"),
			cmdResult(r, "tree"),
			cmdResult(r, "licenses"),
			cmdResult(r, "licenses_warm"),
			cmdResult(r, "search"),
			cmdResult(r, "diff"),
			cmdResult(r, "outdated"),
			cmdResult(r, "outdated_warm"),
		}
		rows = append(rows, row)
	}

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

	for i, h := range headers {
		fmt.Printf("%-*s  ", widths[i], h)
	}
	fmt.Println()

	total := 0
	for _, w := range widths {
		total += w + 2
	}
	fmt.Println(strings.Repeat("-", total))

	for _, row := range rows {
		for i, cell := range row {
			fmt.Printf("%-*s  ", widths[i], cell)
		}
		fmt.Println()
	}
}

func printMarkdown(results []LoadedResult) {
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

func printCSV(results []LoadedResult, w *csv.Writer) {
	headers := []string{"repo", "commits", "init_seconds", "db_bytes", "manifests", "ecosystems", "deps", "changes", "packages", "versions", "list_seconds", "blame_seconds", "history_seconds", "stale_seconds", "log_seconds", "tree_seconds", "licenses_seconds", "licenses_warm_seconds", "search_seconds", "diff_seconds", "outdated_seconds", "outdated_warm_seconds"}
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

		ecosystems := strings.Join(r.Ecosystems, ";")

		row := []string{
			r.Name,
			fmt.Sprintf("%d", commits),
			fmt.Sprintf("%.3f", initSeconds),
			fmt.Sprintf("%d", getDBSize(r)),
			fmt.Sprintf("%d", r.Manifests),
			ecosystems,
			fmt.Sprintf("%d", getDepCount(r)),
			fmt.Sprintf("%d", getChangeCount(r)),
			fmt.Sprintf("%d", getPackageCount(r)),
			fmt.Sprintf("%d", getVersionCount(r)),
			fmt.Sprintf("%.3f", getCmdElapsed(r, "list")),
			fmt.Sprintf("%.3f", getCmdElapsed(r, "blame")),
			fmt.Sprintf("%.3f", getCmdElapsed(r, "history")),
			fmt.Sprintf("%.3f", getCmdElapsed(r, "stale")),
			fmt.Sprintf("%.3f", getCmdElapsed(r, "log")),
			fmt.Sprintf("%.3f", getCmdElapsed(r, "tree")),
			fmt.Sprintf("%.3f", getCmdElapsed(r, "licenses")),
			fmt.Sprintf("%.3f", getCmdElapsed(r, "licenses_warm")),
			fmt.Sprintf("%.3f", getCmdElapsed(r, "search")),
			fmt.Sprintf("%.3f", getCmdElapsed(r, "diff")),
			fmt.Sprintf("%.3f", getCmdElapsed(r, "outdated")),
			fmt.Sprintf("%.3f", getCmdElapsed(r, "outdated_warm")),
		}
		w.Write(row)
	}
	w.Flush()
}

func runResults(resultsDir string, args []string) {
	markdown := false
	csvOutput := ""
	for _, arg := range args {
		if arg == "--markdown" || arg == "-m" {
			markdown = true
		} else if after, found := strings.CutPrefix(arg, "--csv="); found {
			csvOutput = after
		}
	}

	results, err := loadResults(resultsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

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

func cleanUncommittedResults(resultsDir string) {
	// Check if results dir exists
	if _, err := os.Stat(resultsDir); os.IsNotExist(err) {
		return
	}

	// Get list of committed files in results dir
	cmd := exec.Command("git", "ls-files", resultsDir)
	output, _ := cmd.Output()
	committed := make(map[string]bool)
	for _, line := range strings.Split(string(output), "\n") {
		if line != "" {
			committed[line] = true
		}
	}

	// Remove any .json files that aren't committed
	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		fullPath := filepath.Join(resultsDir, entry.Name())
		if !committed[fullPath] {
			if err := os.Remove(fullPath); err == nil {
				fmt.Printf("Cleaned up: %s\n", entry.Name())
			}
		}
	}
}

func main() {
	execDir, _ := os.Getwd()
	resultsDir := filepath.Join(execDir, "results")
	reposFile := filepath.Join(execDir, "repos.txt")
	cacheDir := filepath.Join(execDir, "repos")

	// Handle "results" subcommand
	if len(os.Args) > 1 && os.Args[1] == "results" {
		runResults(resultsDir, os.Args[2:])
		return
	}

	// Parse flags
	forceClone := false
	skipClean := false
	var repos []string
	for _, arg := range os.Args[1:] {
		if arg == "--fresh" || arg == "-f" {
			forceClone = true
		} else if arg == "--no-clean" {
			skipClean = true
		} else if !strings.HasPrefix(arg, "-") {
			repos = append(repos, arg)
		}
	}

	if len(repos) == 0 {
		var err error
		repos, err = loadRepos(reposFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "No repos.txt found. Create one with repo URLs, one per line.\n")
			os.Exit(1)
		}
	}

	if len(repos) == 0 {
		fmt.Fprintf(os.Stderr, "No repos to test.\n")
		os.Exit(1)
	}

	// Create cache directory
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create cache dir: %v\n", err)
		os.Exit(1)
	}

	// Clean uncommitted results
	if !skipClean {
		cleanUncommittedResults(resultsDir)
	}

	fmt.Printf("Testing %d repo(s)...\n", len(repos))
	fmt.Printf("Cache: %s\n\n", cacheDir)

	for i, repo := range repos {
		fmt.Printf("[%d/%d] %s\n", i+1, len(repos), repo)
		result := testRepo(repo, cacheDir, forceClone)
		if err := saveResult(result, resultsDir); err != nil {
			fmt.Fprintf(os.Stderr, "  Failed to save: %v\n", err)
		}
		fmt.Println()
	}

	fmt.Printf("Done. Results saved to %s/\n", resultsDir)
}
