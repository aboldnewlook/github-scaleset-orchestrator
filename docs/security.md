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

GSO currently creates repository scale sets and cannot use this organization runner permission. A different controller or future GSO support would be needed to use organization runners.

### PAT vs. GitHub App

| Approach | Permission | Scope | Notes |
|---|---|---|---|
| Fine-Grained PAT (repo-level) | `administration:write` | Specific repos | Broadest blast radius per repo, but limited to selected repos |
| Fine-Grained PAT (org-level) | `organization_self_hosted_runners:write` | Org runners | Narrower permission; not supported by current GSO |
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

The manager passes only a short-lived, single-use JIT config to the runner. In the default subprocess mode, the PAT is filtered from the child environment, but the job runs as the same OS user and may be able to inspect the manager process or read its files. Treat subprocess mode as suitable only for trusted workflow code. In Docker mode, the job container receives no manager PAT, config, cache, or Docker socket.

## Runner Isolation

### Subprocess Mode (Default)

Each runner executes as a separate OS process. This is a lifecycle boundary, not a security boundary: runner and manager share the same user, filesystem, and process environment on the host/container.

### Temporary Job Workspace

Each runner gets its own temporary directory:

```
/tmp/gso-<repo>-<uuid>/
  bin/                    # Copy of the runner binary (hard-linked when possible)
  _work/                  # Job workspace (created by the runner)
  _diag/                  # Runner diagnostics
```

The temp directory is removed after the runner exits. Workflow code can still write elsewhere the shared user can access; those changes may persist.

### Credential Isolation

- The **PAT** is filtered from the subprocess environment; this alone does not prevent same-user process or filesystem access. Docker mode keeps the PAT outside the job container.
- The **JIT config** (passed via `ACTIONS_RUNNER_INPUT_JITCONFIG`) is a single-use token that allows the runner to register with GitHub once. It cannot be reused.
- The **GITHUB_TOKEN** is issued by GitHub Actions to the runner for the duration of the job. It is scoped to the repository and has the permissions defined in the workflow YAML.

### Docker Mode

Each job runs in a sibling container under the image's non-root `runner` user. The manager alone holds the PAT and Docker socket. Job containers have no manager mounts, no Docker socket, all Linux capabilities dropped, and `no-new-privileges`. The only optional mount is a dedicated repository cache at `/cache`. Untrusted code can alter that cache, so disable it for untrusted PR workflows. The manager's Docker socket grants host-level control; protect the manager container and its configuration accordingly.

Docker mode uses a stable `instance_id` label to remove orphaned job containers at manager startup. Every manager needs a distinct ID. Container removal also discards its temporary workspace. Resource limits can be set with `runner.memory_mb` and `runner.cpus`; set `max_runners` based on total host capacity.

### Ephemeral Guarantees

Runners are configured with `DisableUpdate: true` and are JIT-only. They:

- Register with GitHub once
- Pick up exactly one job
- Execute the job
- Exit
- Are cleaned up (temp directory removed, semaphore slot released)

A runner cannot be reused for a second job. In Docker mode the job container is removed; optional repository caches persist by design. In subprocess mode, workflow writes outside the temporary workspace can persist.

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

1. **Run jobs as a non-root user.** Docker mode's job image does this by default. The manager needs access to the Docker socket and may need to run as root to open it.
2. **Limit filesystem access.** Docker job containers receive no manager filesystem mounts. In subprocess mode, use only trusted workflow code.
3. **Monitor the event log.** The JSONL event store records all runner spawns, job starts, and completions. Review for anomalies.
4. **Set `max_runners` conservatively.** This limits the CPU and memory impact of concurrent jobs.

### Docker-Specific

1. **Run job containers as a non-root user.** The stock image uses the `runner` user.
2. **Mount the Docker socket only into the manager in Docker mode.** The socket gives the manager host-level control; never pass it to jobs.
3. **Keep job mounts limited.** Only a dedicated `/cache` mount is supported, and it should be disabled for untrusted PR workflows.
4. **Use Docker secrets** (`file: /run/secrets/gh_token`) instead of environment variables for token storage.

## Comparison: Deployment Models

| Model | Permissions | Complexity | Best For |
|---|---|---|---|
| gso + repo PAT | `administration:write` per repo | Low | Personal repos, small scale |
| Organization runner controller | `organization_self_hosted_runners:write` | Varies | Free org, narrower permissions; requires another controller or future GSO support |
| GitHub App + gso | `administration:write` (installation) | Medium | Teams, automated token rotation |
| Actions Runner Controller (ARC) | Org-level or app | High | Kubernetes clusters, large scale |
| Dedicated runners per repo | `administration:write` per repo | Low | Single repo, always-on |

GSO currently uses repository scale sets, which require `administration:write` per repository. Organization runners offer a narrower permission, but require another controller or future GSO support.
