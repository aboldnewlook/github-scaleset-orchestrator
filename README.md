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

# Optional: run each job in a sibling Docker container.
# Subprocess remains the default for existing installations.
# runner:
#   executor: docker
#   image: ghcr.io/aboldnewlook/github-scaleset-orchestrator:0.6.0
#   instance_id: my-unique-manager
#   memory_mb: 3072
#   cpus: 1.5
#   cache: none
#   cache_root: /mnt/cache/gso-job-cache
#   cache_uid: 1001
#   cache_gid: 1001

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
    # runner_cache: repo  # trusted jobs only; shares a persistent cache within this repo

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

4. **Docker job isolation.** With `runner.executor: docker`, each job runs in a sibling container that receives a single-use JIT config. It does not receive the manager PAT, config, cache, or Docker socket. The default subprocess executor filters sensitive environment variables, but jobs share the manager's user and filesystem; use Docker mode for untrusted workflows.

5. **Ephemeral runners.** Each runner handles one job. Docker job containers and their temporary workspaces are removed afterward. An optional per-repo tool cache persists by design; subprocess jobs can write outside their temporary workspace.

### Organization Runner Alternative

GitHub organization runners can use the narrower `organization_self_hosted_runners:write` permission. GSO currently creates repository scale sets and requires repo `administration:write`; it does not manage organization runner scale sets. Moving to an organization runner setup would require a different controller or future GSO support.

### Runner GITHUB_TOKEN

Each job gets its own `GITHUB_TOKEN`, automatically issued by GitHub Actions. In Docker mode, the manager PAT is outside the job container.

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

### Job timing events

The JSONL event history records `runner_startup_ms` on `job.started`, measured
from runner creation until GitHub reports that it picked up a job. It records
`job_duration_ms` on `job.completed`, measured between GitHub's start and
completion callbacks. `runner.completed` and `runner.failed` include
`runner_elapsed_ms`, measured from runner creation until its process or Docker
container exits. These fields work with either executor and can be inspected
with `gso log --type job.completed` or queried from `events.jsonl`.
Missing callbacks leave the corresponding duration absent. Runner startup is
not the time a job spent in GitHub's queue before GSO received the assignment.

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

### Docker job executor

Set `GSO_RUNNER_EXECUTOR=docker`, `GSO_RUNNER_IMAGE` to an image containing `/usr/local/bin/gso-job` and `/opt/gso-runner`, and `GSO_RUNNER_INSTANCE_ID` to a unique stable value. The stock gso image includes both. Mount `/var/run/docker.sock` **only into the manager**, and give its process permission to use the socket. The Docker socket gives the manager host-level control; keep its token and socket inaccessible to job containers. Pin the job image to a tested tag or digest.

The manager creates one sibling container per JIT runner. Jobs inherit only the image environment plus their JIT config, run as the image's non-root `runner` user, have all Linux capabilities dropped, and receive no manager volumes or host paths. Optional `GSO_RUNNER_MEMORY_MB` and `GSO_RUNNER_CPUS` set per-job limits. Set `max_runners` based on total host memory; for a 14.6 GiB host, start with two jobs at 3 to 4 GiB each.

`GSO_RUNNER_CACHE=none` is the default. To share Go and npm download/build caches within a trusted repository, set `runner_cache: repo` on that repo (including in `GSO_REPOS` JSON). Each repo has a separate hashed cache mounted at `/cache`; `GOCACHE`, `GOMODCACHE`, and `NPM_CONFIG_CACHE` point there. Jobs can change shared cache contents, so keep `runner_cache: none` for untrusted PR workflows. Named Docker volumes are used by default. Set `GSO_RUNNER_CACHE_ROOT` to an absolute SSD host path, mounted into the manager at the same path, to keep caches out of Docker's data-root; the manager creates hashed per-repo directories there.

The stock job image runs as UID/GID `1001:1001`. For a custom image with another numeric user, set `GSO_RUNNER_CACHE_UID` and `GSO_RUNNER_CACHE_GID` so a root manager can give new host cache directories to that user. A non-root manager needs write access to the configured cache root and should run under a UID that can write the created subdirectories.

At startup, the manager force-removes leftover containers labeled with its `instance_id`. Use a distinct ID for every concurrently running manager. Cancellation force-removes the container. Container logs are replayed after the job exits; live log streaming is not implemented yet.

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
4. A worker creates a temporary subprocess workspace or a sibling Docker container
5. The runner starts with the JIT config passed via environment variable
6. The runner registers with GitHub, picks up exactly one job, executes it, and exits
7. The temporary workspace or container is removed and the semaphore slot is released

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
