# Security Model

This document describes the security considerations for running gso.

## Permission Model

### Required: `administration:write`

The GitHub Scale Set API requires a Personal Access Token with **`administration:write`** permission on each target repository. This is the minimum permission needed to create and manage runner scale sets.

Unfortunately, `administration:write` is a broad permission. It also grants the ability to:

- **Delete the repository**
- **Change repository visibility** (public to private or vice versa)
- **Modify branch protection rules**
- **Manage deploy keys**
- **Manage webhooks**
- **Access repository settings**

GitHub does not offer a narrower permission scope for runner management at the repository level.

### Organization Alternative: `organization_self_hosted_runners:write`

At the organization level, GitHub offers `organization_self_hosted_runners:write`, which is scoped specifically to runner management. It cannot delete repos, change visibility, or modify branch protection.

**Recommendation:** If possible, create a free GitHub organization and move or fork your repositories there. This allows you to use the narrower permission scope.

### PAT vs. GitHub App

| Approach | Permission | Scope | Notes |
|---|---|---|---|
| Fine-Grained PAT (repo-level) | `administration:write` | Specific repos | Broadest blast radius per repo, but limited to selected repos |
| Fine-Grained PAT (org-level) | `organization_self_hosted_runners:write` | Org runners | Narrowest permission, recommended |
| GitHub App | `administration:write` or org runners | Installation repos | More complex setup, better for teams |
| Classic PAT | `repo` (full) | All repos | Avoid -- too broad |

**Never use a classic PAT with `repo` scope.** It grants full access to all repositories the user can see, including private repos in other organizations.

## Token Lifecycle

### Storage

Tokens can be stored in three locations, resolved in priority order:

1. **Environment variable** (`env`): The token value is read from the named env var at startup. Suitable for CI environments and Docker containers. The token exists in process memory and the environment.

2. **OS keychain** (`keychain`): The token is stored in the platform's secure credential store:
   - **macOS**: Keychain Services (encrypted, protected by login password or Touch ID)
   - **Linux**: GNOME Keyring or KWallet (encrypted, session-scoped)
   - **Windows**: Credential Manager (encrypted, protected by user login)

   The keychain is the recommended storage for interactive use. Tokens are encrypted at rest.

3. **File** (`file`): The token is read from a file. Suitable for Docker secrets (`/run/secrets/...`), HashiCorp Vault agent, or other secret injection mechanisms. Ensure the file has restrictive permissions (`0600`).

### Resolution

Tokens are resolved at orchestrator startup and whenever a new scale set client is created. The resolution order is: env var, then keychain, then file. The first non-empty value wins.

Per-repo tokens take precedence over global auth. This allows blast radius reduction: if one repo's token is compromised, others are unaffected.

### Token Exposure

The PAT is used only by the orchestrator process to:

1. Create and manage scale sets (API calls)
2. Establish message session clients (long-polling)
3. Generate JIT runner configs (single-use credentials)

The PAT is **never** passed to the runner subprocess. The runner receives only the JIT config, which is a short-lived, single-use credential that allows it to register with GitHub and pick up exactly one job.

## Runner Isolation

### Process Isolation

Each runner executes as a separate OS process. The orchestrator does not run workflow code -- it only manages the lifecycle of runner subprocesses.

### Filesystem Isolation

Each runner gets its own temporary directory:

```
/tmp/gso-<repo>-<uuid>/
  bin/                    # Copy of the runner binary (hard-linked when possible)
  _work/                  # Job workspace (created by the runner)
  _diag/                  # Runner diagnostics
```

The temp directory is created before the runner starts and removed (`os.RemoveAll`) after the runner exits. No state persists between jobs.

### Credential Isolation

- The **PAT** stays in the orchestrator process. It is never set in the runner's environment.
- The **JIT config** (passed via `ACTIONS_RUNNER_INPUT_JITCONFIG`) is a single-use token that allows the runner to register with GitHub once. It cannot be reused.
- The **GITHUB_TOKEN** is issued by GitHub Actions to the runner for the duration of the job. It is scoped to the repository and has the permissions defined in the workflow YAML. Your PAT is never exposed to workflow code.

### Ephemeral Guarantees

Runners are configured with `DisableUpdate: true` and are JIT-only. They:

- Register with GitHub once
- Pick up exactly one job
- Execute the job
- Exit
- Are cleaned up (temp directory removed, semaphore slot released)

A runner cannot be reused for a second job. There is no persistent runner state.

## Network Model

### Outbound Only

gso makes outbound HTTPS connections only:

| Destination | Purpose |
|---|---|
| `github.com` | Scale Set API, JIT config generation |
| `api.github.com` | Runner release metadata |
| `github-releases.githubusercontent.com` | Runner binary download |
| `pipelines.actions.githubusercontent.com` | Runner message session (long-poll) |

No inbound ports are opened. No webhook endpoints are needed. The tool works behind NAT, firewalls, and home networks.

### DNS

The orchestrator resolves DNS for the above hostnames. If you run in a restricted network, ensure these domains are accessible.

## Hardening Recommendations

### Token Hygiene

1. **Use fine-grained PATs**, not classic PATs. Fine-grained PATs can be scoped to specific repositories.
2. **Use per-repo tokens** when practical. A compromise of one token does not affect other repos.
3. **Set token expiration.** Fine-grained PATs support expiration dates. Rotate regularly.
4. **Use the OS keychain** for interactive use. Avoid env vars in shell history.
5. **Use Docker secrets or Vault** for container deployments. Avoid baking tokens into images.

### System Hardening

1. **Run as a non-root user.** The orchestrator and runners do not need elevated privileges.
2. **Limit filesystem access.** The runner only needs write access to its temp directory and the runner cache directory.
3. **Monitor the event log.** The JSONL event store records all runner spawns, job starts, and completions. Review for anomalies.
4. **Set `max_runners` conservatively.** This limits the CPU and memory impact of concurrent jobs.

### Docker-Specific

1. **Do not run the container as root.** Use a non-root user in your Dockerfile.
2. **Use read-only root filesystem** with writable tmpfs mounts for `/tmp` and the cache directory.
3. **Do not mount the Docker socket** into the container unless your workflows specifically need Docker-in-Docker.
4. **Use Docker secrets** (`file: /run/secrets/gh_token`) instead of environment variables for token storage.

## Comparison: Deployment Models

| Model | Permissions | Complexity | Best For |
|---|---|---|---|
| gso + repo PAT | `administration:write` per repo | Low | Personal repos, small scale |
| gso + org PAT | `organization_self_hosted_runners:write` | Low | Free org, narrower permissions |
| GitHub App + gso | `administration:write` (installation) | Medium | Teams, automated token rotation |
| Actions Runner Controller (ARC) | Org-level or app | High | Kubernetes clusters, large scale |
| Dedicated runners per repo | `administration:write` per repo | Low | Single repo, always-on |

gso occupies the sweet spot between "one runner per repo" (wasteful) and "full Kubernetes ARC deployment" (complex). The main security trade-off is the `administration:write` permission at the repo level, which can be mitigated by using an organization.
