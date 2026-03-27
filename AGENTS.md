# AGENTS.md -- AI Coding Agent Context

This file provides context for AI coding agents working on the gso project.

## Project Overview

gso is a multi-repo GitHub Actions self-hosted runner orchestrator written in Go. It watches multiple personal repositories for queued CI jobs using GitHub's Scale Set API (long-polling), spawns JIT ephemeral runners on demand, and tears them down after each job completes.

**Goal:** Let individuals share self-hosted runners across personal repos without creating a GitHub organization.

## Architecture

### Package Responsibilities

| Package | Purpose |
|---|---|
| `cmd/` | Cobra CLI commands. `root.go` defines the root command and `--config` flag. `format.go` has table output helpers. |
| `internal/config/` | YAML config loading (`config.go`) and multi-source token resolution (`token.go`). `TokenSource` resolves tokens from env vars, OS keychain, or files. |
| `internal/orchestrator/` | Top-level `Run()` loop. Creates a scale set listener per repo, shares a concurrency semaphore. Handles stale session recovery (409 Conflict). |
| `internal/scaler/` | Implements `listener.Scaler` interface from `actions/scaleset`. One `Scaler` per repo. Acquires semaphore slots, generates JIT configs, spawns runner goroutines. |
| `internal/runner/` | `Manager` downloads and caches the runner binary with SHA256 verification. `Worker` creates temp directories and runs runner subprocesses. `extract.go` handles tar.gz extraction with zip-slip protection. |
| `internal/event/` | `Event` types, fan-out `Bus` with non-blocking publish, and `FileStore` for JSONL persistence. Decouples the orchestrator from output/logging concerns. |
| `internal/service/` | `Runtime` wraps live orchestrator state and dispatches control socket requests. Implements `HandleRequest` for methods like `live_status`, `recycle_runner`, `set_max_runners`, `shutdown`. |
| `internal/control/` | Unix domain socket IPC. `protocol.go` defines JSON-RPC-like `Request`/`Response` types and method constants. `socket.go` computes platform-appropriate socket paths. |

### Data Flow

```
GitHub API --> Listener (long-poll) --> Scaler.HandleDesiredRunnerCount
                                           |
                                    Semaphore.Acquire()
                                           |
                                    GenerateJitRunnerConfig (API call)
                                           |
                                    Worker.Run (subprocess in temp dir)
                                           |
                                    Semaphore.Release()
```

### Control Plane (daemon management)

```
CLI command --> Unix socket --> Runtime.HandleRequest --> OrchestratorState interface
```

The `OrchestratorState` interface decouples `Runtime` from the concrete `Orchestrator` type. This allows testing `Runtime` independently.

## Key Design Decisions

### Why Go

- The `actions/scaleset` library is Go-only -- it provides the Scale Set API client and listener framework
- Single binary deployment: `go build` produces one executable with no runtime dependencies
- Cross-compilation: `GOOS=linux go build` for Docker, native macOS build for local use
- Goroutines map naturally to per-repo listeners and per-job runner workers

### Why JIT Ephemeral Runners

- **Clean state:** Each job gets a fresh runner in a new temp directory. No cross-job contamination.
- **No idle resources:** Runners only exist while a job is running. CPU and memory are free between jobs.
- **Dynamic scaling:** The semaphore controls maximum concurrency. No need to pre-provision.
- **Security:** The JIT config is a single-use credential. The runner can never be reused for a different job.

### Why Scale Set API (not webhooks)

- **Long-polling:** No need for inbound ports, public URLs, or webhook secrets
- **No infrastructure:** Works behind NAT, firewalls, home networks
- **Built-in reliability:** The scaleset listener handles reconnection, message ordering, and session management
- **Official:** This is the same API used by GitHub's Actions Runner Controller (ARC) for Kubernetes

### Why Adapter Pattern (CLI today, TUI later)

- The `service.Runtime` type provides all daemon operations via a clean interface
- The control socket protocol uses JSON-RPC-like messages -- any client can connect
- Today: CLI commands send requests over the socket
- Tomorrow: A TUI (e.g., bubbletea) subscribes to the event bus and calls the same service methods
- The orchestrator does not know or care about its output mechanism

### Why Event Bus

- Decouples the orchestrator from output concerns (logging, TUI, persistence)
- Non-blocking fan-out: slow subscribers are dropped, never block the orchestrator
- JSONL persistence enables `log` command replay and post-mortem analysis
- Multiple subscribers: CLI logger, TUI, file store can all receive the same events

## The Service Layer Pattern

### Runtime (live state, requires running daemon)

`service.Runtime` wraps `OrchestratorState` and provides methods that operate on live data:

- `LiveStatus()` -- current runners per repo
- `RecycleRunner(name)` -- cancel a running runner
- `SetMaxRunners(count)` -- change concurrency limit
- `Shutdown()` -- graceful stop

Use Runtime when the command needs the daemon to be running (e.g., `status`, `stop`, `runner recycle`).

### Query (API-only, no daemon needed) -- planned

A `Query` service will wrap the `scaleset.Client` directly for operations that only need the GitHub API:

- List scale sets
- Inspect a scale set
- Delete a scale set
- Check token validity

Use Query when the command works without a running daemon (e.g., `scaleset list`, `health`, `config check`).

## Control Socket Pattern

The daemon listens on a Unix domain socket for JSON-RPC-like requests.

**Socket paths:**
- macOS: `$TMPDIR/gso-$UID.sock`
- Linux: `$XDG_RUNTIME_DIR/gso.sock` (fallback: `/tmp/gso-$UID.sock`)

**When to use the control socket:** Commands that need live orchestrator state (`status`, `stop`, `runner recycle/list`, `set_max_runners`).

**When to hit the API directly:** Commands that only need the GitHub API (`scaleset list/inspect/delete`, `health`, `config check`).

## Testing Approach

- **Table-driven tests** for pure logic (config validation, token resolution, event filtering)
- **Interface-based mocking** for service layer tests (`OrchestratorState` is an interface)
- **Integration-style tests** for event bus (`bus_test.go` tests publish/subscribe, fan-out, non-blocking behavior)
- Test files live next to the code they test (e.g., `internal/event/bus_test.go`)

Run all tests: `go test ./...`

## Common Agent Tasks

### Add a New CLI Command

1. Create `cmd/<command>.go`
2. Define a `cobra.Command` and register it with `rootCmd` in `init()`
3. If it needs the daemon: connect to the control socket and send a `Request`
4. If it needs the API: load config, resolve token, create `scaleset.Client`
5. Use `printTable()` from `cmd/format.go` for tabular output

### Add a New Event Type

1. Add a constant to `internal/event/event.go` (e.g., `EventTokenRefreshed EventType = "token.refreshed"`)
2. Publish the event from the relevant package using `bus.Publish(event.Event{...})`
3. The event bus, subscribers, and file store handle the rest automatically

### Add a New Control Socket Method

1. Add a method constant to `internal/control/protocol.go`
2. Add parameter/result types if needed
3. Add a case to `Runtime.HandleRequest` in `internal/service/runtime.go`
4. Implement the method on `Runtime`
5. If it needs orchestrator state, add a method to the `OrchestratorState` interface

### Add a New Service Method

1. Determine if it belongs on `Runtime` (needs live state) or `Query` (API-only)
2. Add the method to the appropriate service type
3. If `Runtime`, the method may need a new `OrchestratorState` interface method

## Files: Safe to Modify vs. Careful Coordination

### Safe to modify independently

- `cmd/*.go` -- Adding new commands does not affect other packages
- `internal/event/event.go` -- Adding event types is additive
- `internal/control/protocol.go` -- Adding methods/types is additive
- `config.example.yaml` -- Documentation only

### Modify with care

- `internal/config/config.go` -- Changes to `Config` struct affect all consumers. Adding fields is usually safe; renaming or removing fields is breaking.
- `internal/config/token.go` -- Token resolution logic is security-sensitive.
- `internal/orchestrator/orchestrator.go` -- Core loop. Changes here affect all repos. Test manually.
- `internal/scaler/scaler.go` -- Implements the `listener.Scaler` interface. Method signatures must match the `actions/scaleset` library.
- `internal/service/runtime.go` -- Changes to `OrchestratorState` interface require matching changes in the orchestrator.
- `main.go` -- Being migrated to use Cobra (`cmd/root.go`). The old flag-based entry point will be replaced.

### External dependency: `actions/scaleset` v0.2.0

The `listener.Scaler` interface is defined by this library. Do not change the method signatures of `HandleDesiredRunnerCount`, `HandleJobStarted`, or `HandleJobCompleted` in `internal/scaler/scaler.go` -- they must match the interface.
