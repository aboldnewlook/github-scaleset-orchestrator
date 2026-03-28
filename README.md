# github-scaleset-orchestrator

A multi-repo GitHub Actions self-hosted runner orchestrator. Share self-hosted runners across your personal repositories from a single machine -- no GitHub organization required.

## The Problem

GitHub's self-hosted runners are designed for organizations. If you have several personal repos that need self-hosted CI (macOS builds, ARM testing, GPU workloads), your options are:

1. Run a dedicated idle runner per repo (wasteful)
2. Create an org just for runner sharing (clunky)
3. Use this tool

gso watches multiple repos for queued CI jobs via long-polling, spawns JIT ephemeral runners on demand, and tears them down when the job completes. No idle processes, no webhooks, no inbound ports.

## Architecture

```
                           GitHub Actions API
                                  |
                          (HTTPS long-poll)
                                  |
     +----------------------------+----------------------------+
     |                            |                            |
  Listener (repo-a)        Listener (repo-b)         Listener (repo-c)
     |                            |                            |
  Scaler (repo-a)           Scaler (repo-b)          Scaler (repo-c)
     |                            |                            |
     +----------------------------+----------------------------+
                                  |
                         Shared Semaphore
                     (concurrency = CPU count)
                                  |
                          Runner Workers
                    (ephemeral subprocesses)
```

**Package layout:**

```
cmd/                        Cobra CLI (root command, table formatting)
internal/
  config/                   YAML config loading, multi-source token resolution
  orchestrator/             Per-repo scale set listeners, shared concurrency
  scaler/                   Implements scaleset listener.Scaler, spawns runners
  runner/                   Binary download/caching, subprocess management
  event/                    Event bus for logging/TUI, JSONL persistence
  service/                  Business logic (Runtime for live state via control socket)
  control/                  Unix domain socket IPC protocol for daemon management
```

## Quick Start

### Prerequisites

- Go 1.26+
- A GitHub Fine-Grained Personal Access Token (see [Security](#security-considerations))

### Install

```sh
go install github.com/aboldnewlook/github-scaleset-orchestrator@latest
```

Or build from source:

```sh
git clone https://github.com/aboldnewlook/github-scaleset-orchestrator.git
cd github-scaleset-orchestrator
go build -o gso .
```

### Configure

Copy the example config:

```sh
cp config.example.yaml config.yaml
```

Edit `config.yaml` with your repos and token source:

```yaml
auth:
  keychain: default    # or env / file

max_runners: 4

labels:
  - self-hosted

repos:
  - name: youruser/repo-a
  - name: youruser/repo-b
```

Store your PAT in the OS keychain:

```sh
gso token -set default
# Enter your token at the prompt
```

### Run

```sh
gso -config config.yaml
```

The orchestrator will:

1. Download and cache the GitHub Actions runner binary (with SHA256 verification)
2. Create a scale set for each configured repo
3. Long-poll for queued jobs
4. Spawn JIT ephemeral runners as jobs arrive
5. Tear down runners after each job completes

Stop with Ctrl-C for graceful shutdown.

## Configuration Reference

```yaml
# Global auth -- used for repos that don't specify their own token.
# Resolution order: env -> keychain -> file
# The first non-empty value wins.
auth:
  env: GITHUB_TOKEN              # Read token from this environment variable
  # keychain: default            # Read from OS keychain (macOS Keychain,
                                 # GNOME Keyring, Windows Credential Manager)
  # file: /run/secrets/gh_token  # Read from file (Docker secrets, Vault, etc.)

# Maximum concurrent runner processes.
# Default: number of CPUs on the machine (runtime.NumCPU()).
max_runners: 4

# Labels applied to all runners, in addition to auto-detected OS and arch
# (e.g., "macOS", "ARM64").
labels:
  - self-hosted

# Repositories to watch for jobs.
repos:
  - name: owner/repo-a
    # Uses global auth

  - name: owner/repo-b
    # Per-repo token override (same resolution options as global auth)
    token:
      env: REPO_B_TOKEN

  - name: owner/repo-c
    token:
      keychain: repo-c
```

### Token Resolution

Tokens are resolved in order: environment variable, OS keychain, file. The first non-empty value wins. Per-repo tokens take precedence over global auth.

**Environment variable:** Set `env` to the name of an env var containing the PAT.

**OS Keychain:** Set `keychain` to an account name. Store the token with:

```sh
gso token -set <account-name>
```

Supported backends: macOS Keychain, GNOME Keyring (Linux), Windows Credential Manager.

**File:** Set `file` to a path. The file should contain just the token (whitespace is trimmed). Useful for Docker secrets or Vault agent.

## Security Considerations

See [docs/security.md](docs/security.md) for the full security deep-dive. Key points:

### Required Permission: `administration:write`

The Scale Set API requires a PAT with **`administration:write`** on each target repo. This is a powerful permission that also grants the ability to:

- Delete the repository
- Change repository visibility (public/private)
- Modify branch protection rules
- Manage deploy keys and webhooks

There is no way to scope a PAT to just "manage runners" at the repo level.

### Mitigations

1. **Fine-Grained PATs scoped to specific repos.** Create a PAT that only has access to the exact repos you list in config.yaml. If the token leaks, blast radius is limited to those repos.

2. **Per-repo tokens.** Use a separate PAT per repo (or group of repos) so a compromise of one token does not affect the others.

3. **OS keychain storage.** Tokens stored in the keychain are encrypted at rest and protected by your OS login credentials. Prefer `keychain` over `env` or `file`.

4. **Runner process isolation.** The runner subprocess never sees your PAT. It receives only a JIT config (a short-lived, single-use credential). Your PAT is used only by the orchestrator to call the Scale Set API.

5. **Ephemeral runners.** Each runner gets a fresh temp directory, runs one job, and is deleted. No persistent state accumulates.

### Recommendation: Use a Free GitHub Organization

If you can, create a free GitHub org and move (or fork) your repos there. Org-level runner management requires only `organization_self_hosted_runners:write`, which is a much narrower permission. It cannot delete repos or change visibility.

### Runner GITHUB_TOKEN

The runner subprocess gets its own `GITHUB_TOKEN` per job, automatically issued by GitHub Actions. Your PAT is never exposed to workflow code.

## Commands

The CLI is being migrated to Cobra. Current commands:

| Command | Description |
|---|---|
| `gso -config config.yaml` | Start the orchestrator |
| `gso token -set <account>` | Store a PAT in the OS keychain |
| `gso token -delete <account>` | Remove a PAT from the OS keychain |

Planned commands (under development):

| Command | Description |
|---|---|
| `start` | Start orchestrator daemon |
| `stop` | Graceful shutdown via control socket |
| `status` | Dashboard with per-repo runner stats |
| `scaleset list` | List scale sets across repos |
| `scaleset inspect <id>` | Show scale set details |
| `scaleset delete <id>` | Delete a scale set |
| `runner list` | List active runners |
| `runner recycle <name>` | Cancel and replace a runner |
| `runner remove <name>` | Remove a runner |
| `token set` | Store a token in the keychain |
| `token delete` | Remove a token from the keychain |
| `log` | Query event history |
| `health` | Check connectivity and token validity |
| `config check` | Validate configuration file |

## Docker Deployment

### docker-compose

```yaml
version: "3.8"
services:
  gso:
    build: .
    restart: unless-stopped
    volumes:
      - ./config.yaml:/app/config.yaml:ro
      - runner-cache:/app/.cache
    environment:
      - GITHUB_TOKEN=${GITHUB_TOKEN}
    # No inbound ports needed -- outbound HTTPS only

volumes:
  runner-cache:
```

### Unraid + Portainer

See [docs/unraid-portainer.md](docs/unraid-portainer.md) for a complete deployment guide using Portainer stacks.

## How It Works

### Scale Set API

gso uses GitHub's [Scale Set API](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners-with-actions-runner-controller/deploying-runner-scale-sets-with-actions-runner-controller) via the `actions/scaleset` Go library. This API was designed for Kubernetes-based runner controllers but works for any orchestrator.

The key advantage: **long-polling**. The orchestrator opens a persistent HTTPS connection to GitHub and is notified when jobs are queued. No webhooks, no inbound ports, no polling intervals.

### JIT Ephemeral Runners

When a job is queued:

1. The `Scaler` for that repo is called with the desired runner count
2. It checks the shared semaphore for available capacity
3. For each runner to create, it calls `GenerateJitRunnerConfig` to get a single-use credential
4. A `Worker` goroutine copies the cached runner binary to a fresh temp directory
5. The runner subprocess starts with the JIT config passed via environment variable
6. The runner registers with GitHub, picks up exactly one job, executes it, and exits
7. The temp directory is cleaned up and the semaphore slot is released

### Stale Session Recovery

If the orchestrator crashes, the Scale Set API may have a stale session. On restart, if a 409 Conflict is detected, the scale set is automatically deleted and recreated.

## Development

### Build

```sh
go build ./...
```

### Test

```sh
go test ./...
```

### Vet

```sh
go vet ./...
```

### Run

```sh
go run . -config config.yaml
```

### Dependencies

- [`actions/scaleset`](https://github.com/actions/scaleset) v0.2.0 -- Scale Set API client and listener
- [`zalando/go-keyring`](https://github.com/zalando/go-keyring) v0.2.8 -- Cross-platform keychain access
- [`spf13/cobra`](https://github.com/spf13/cobra) v1.10.2 -- CLI framework
- [`google/uuid`](https://github.com/google/uuid) v1.6.0 -- Runner name generation
- [`gopkg.in/yaml.v3`](https://pkg.go.dev/gopkg.in/yaml.v3) -- Config parsing

## License

See LICENSE file.
