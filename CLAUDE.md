# CLAUDE.md -- Claude Code Entry Point

Read @AGENTS.md for full project context, architecture, and design decisions.

## Build Commands

```sh
go build ./...          # Build all packages
go test ./...           # Run all tests
go vet ./...            # Static analysis
go run . -config config.yaml   # Run the orchestrator
```

## Important Notes

- `config.yaml` is gitignored -- it contains token references. Use `config.example.yaml` as a template.
- The project depends on `actions/scaleset` v0.2.0 (the Scale Set API client/listener library).
- Go version: 1.26.1
- Targets: macOS (native) and Linux (Docker)

## Key Packages

| Package | Description |
|---|---|
| `cmd/` | Cobra CLI commands and output formatting |
| `internal/config/` | YAML config loading and token resolution (env, keychain, file) |
| `internal/orchestrator/` | Per-repo scale set listeners with shared concurrency semaphore |
| `internal/scaler/` | `listener.Scaler` implementation -- spawns JIT ephemeral runners |
| `internal/runner/` | Runner binary download/caching and subprocess management |
| `internal/event/` | Fan-out event bus with JSONL file persistence |
| `internal/service/` | Runtime service -- live state access via control socket |
| `internal/control/` | Unix domain socket IPC protocol and platform socket paths |

## Current State

- The orchestrator core works: config loading, token resolution, scale set creation, long-polling, JIT runner spawning, and graceful shutdown.
- The CLI is being migrated from `main.go` (flag-based) to `cmd/` (Cobra-based).
- The event bus and file store are implemented and tested.
- The control socket protocol and Runtime service are implemented but not yet wired into the orchestrator's main loop.
- Planned commands (start, stop, status, scaleset, runner, token, log, health, config check) are not yet implemented as Cobra subcommands.
