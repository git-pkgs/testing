# git-pkgs testing

Test harness for benchmarking git-pkgs against real-world repositories.

## Usage

Add repos to `repos.txt` (one URL per line), then run:

```
go run .
```

Results are saved as JSON files in `results/`. Repos are cached in `repos/` for subsequent runs.

### Options

```
go run . --fresh              # Force fresh clone (ignore cache)
go run . https://github.com/foo/bar  # Test specific repo
```

### Aggregate results

```
go run ./cmd/aggregate        # Table output
go run ./cmd/aggregate -m     # Markdown output
```

## What it tests

For each repo, the harness runs `git pkgs init` and then times these commands:

- `list` - list current dependencies
- `blame` - show who added each dependency
- `history` - dependency changes over time
- `stale` - find outdated dependencies

Results include init time, database size, dependency count, and change count.
