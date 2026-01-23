package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
