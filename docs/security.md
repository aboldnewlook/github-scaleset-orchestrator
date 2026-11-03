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

3. **File** (`file`): The token is read from a file. Suitable for Docker secrets (`/run/secrets/...`), HashiCorp Vault agent, or other secret injection mechanisms. gso refuses a token file that group or others can read (anything looser than `0600`); fix it with `chmod 600 <file>`. Compose file secrets are bind mounts, so they keep the host file's mode and owner: make the host file `0600` and owned by uid 1001, the image's `runner` user. Swarm and Kubernetes secrets default to `0444` and must set `mode: 0400` (Swarm) or `defaultMode: 0400` (Kubernetes). The check is skipped on Windows, which has no POSIX mode bits.

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

## Remote Workers

A router (`gso start` with `workers.listen`) places jobs on enrolled workers (`gso worker`, or the Burst desktop app). The router keeps the tokens. A worker receives a single-use JIT config for each job it accepts.

- **Enrolling a machine trusts it with the secrets of every job it runs.** A worker sees each job's code and the secrets that job's workflow receives. Enroll only machines you would let run that code.
- **Bounds.** `gso workers add <name> --classes … --slots … [--repos …]` fixes what a credential may do. The router clamps everything the worker advertises to those bounds and accepts only one live session per credential. A second connection is refused and raises an `alert` event.
- **Router compromise is bounded by the workers.** Before launching anything, a worker decodes the JIT config and refuses it unless the host is `github.com`, the repository equals the offered one, and the repository matches the worker's **local** allowlist (seeded at enrollment, changed only on the worker). A compromised router can push work only to repositories each worker already allows.
- **The OS keychain is not a same-user boundary.** macOS Keychain, Windows Credential Manager and the Linux Secret Service are readable by any process running as the same user. The credential is protected by keeping job code out of that user: Docker jobs run in containers without access to the host user's keychain or files.
- **Docker isolation is process and filesystem isolation, not network isolation.** Job containers sit on Docker's default bridge and can reach your LAN: NAS shares, printers, admin UIs, and the router's ports (which are authenticated). That is accepted for private repositories you trust. Do not opt in public repositories on desktop workers.
- **The join string is the root of trust.** It carries the router address, the key pin and the enrollment code. Move it over a channel you trust. The worker shows the address and a short pin before enrolling.

### Public repositories

Anyone can open a pull request against a public repository, so its workflow code is untrusted. The router therefore:

- reads each repository's visibility (`GET /repos/{owner}/{repo}`) **before** creating its scale set, retrying on failure, and **does not start that repository's listener** while it cannot read it (fail closed). The token needs read access to the repository's metadata;
- offers a public repository's jobs to a remote worker **only if that worker opted in** with `--accept-public accept`;
- **forces the job cache off** for public jobs.

The router's own local member (a router with `max_runners` above 0) currently accepts public repositories. Router-level `public.*` settings are not available yet. Opt in public repositories only on workers that are disposable or isolated from your LAN.

### Simulator

`gso sim serve` binds `:9300` on all interfaces and has **no authentication** and no TLS. Anything that can reach it can inject jobs and read what the router sends. Never expose it beyond a trusted network: bind it to loopback or one internal address only (the compose `sim` profile runs it on the host network, listening only on the Docker bridge gateway address). It is for testing; see [simulator.md](simulator.md).

## Network Model

### Outbound Only

gso makes outbound HTTPS connections only:

| Destination | Purpose |
|---|---|
| `github.com` | Scale Set API, JIT config generation |
| `api.github.com` | Runner release metadata |
| `github-releases.githubusercontent.com` | Runner binary download |
| `pipelines.actions.githubusercontent.com` | Runner message session (long-poll) |

Without remote workers, no inbound ports are opened and no webhook endpoints are needed. The tool works behind NAT, firewalls, and home networks. Remote workers need the same outbound access, because each runner talks to GitHub directly.

### Inbound: the worker listener (remote workers only)

Setting `workers.listen` (e.g. `:9200`) opens **one** inbound port. It carries the router-to-worker control plane only, never runner traffic.

- **TLS 1.3 only.** Workers pin the listener's public key (SPKI SHA-256) from their join string. They ignore certificate dates and hostnames, so the pinned key at `<state>/workers/tls/key.pem` is the only trust anchor. It is generated once and never regenerated while it exists.
- **Source allowlist.** `workers.allow_cidrs` defaults to private ranges only: RFC1918, `100.64.0.0/10` (CGNAT), `fc00::/7` and loopback. The check runs on the client's source address **before** the TLS handshake. Narrow it to your LAN plus, in a container deployment, the compose network. The listener also caps open connections at 256.
- **Authentication.** Every call except `Enroll` carries a per-worker credential (`<worker_id>.<secret>`). The router stores only SHA-256 hashes. `Enroll` takes a single-use, 15-minute code, is rate-limited per source address (5/min, 30/h), and is the only call reachable before authentication.
- **Administration stays local.** `gso workers add/update/list/drain/revoke` work only on the router's unix socket, never on the TCP control port. A leaked `GSO_CONTROL_TOKEN` cannot enroll a worker. In a container, run them with `docker exec <router> gso workers ...`.

#### Docker published ports and the allowlist

The allowlist is only as good as the source address the router sees. When Docker publishes a port through its **userland proxy** (`docker-proxy`), the router sees the proxy's connection, not the real client: every peer then appears to come from the Docker bridge gateway (an address in `172.16.0.0/12`, such as the compose network's gateway, e.g. `172.18.0.1`). Because the compose network range is normally allowed, **the allowlist then filters nothing**, and anyone who can reach the port passes it. The remaining defences (TLS pin, per-worker credential, enrollment rate limit) still hold, but you have lost the network layer.

On the normal path, an `iptables` DNAT rule forwards the published port and the router sees the real client address. The proxy relays connections instead (and hides the client) on paths where DNAT does not apply, for example with Docker's `iptables` support turned off or on some loopback, hairpin and IPv6 paths. Whether your host is affected depends on its Docker configuration, so verify it rather than assume.

- **Prefer the default iptables DNAT**, or give the router **host networking** (`network_mode: host`, then `workers.listen` binds on the host and sees real peers).
- **Check it, positive.** Enroll a worker from a *different* machine on your LAN, then read the router log: the `worker enrolled` line carries `remote=<address>`, and that must be the machine's LAN address. If `remote` is the compose network's gateway (an address in `172.16.0.0/12`) for a peer that is not on the compose network, the source address is being hidden.
- **Check it, negative.** From a host **outside** your allowlist (another VLAN, or a phone on mobile data), run `openssl s_client -connect <host>:9200 -tls1_3`. If the allowlist works, the connection is closed before the TLS handshake and `openssl` reports an error without a certificate. If it prints the router's certificate, the allowlist is not filtering.
- If you cannot preserve source addresses, treat the port as unfiltered: restrict it with the host firewall (Unraid: do not forward it, bind it to the LAN interface) and rely on enrollment bounds and revocation.

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
2. **Mount the Docker socket only into the process that starts job containers**: the `gso worker` container in the split deployment, or the single gso container in the legacy one. Never mount it into the router or into jobs.
3. **Keep job mounts limited.** Only a dedicated `/cache` mount is supported, and it should be disabled for untrusted PR workflows.
4. **Use Docker secrets** (`file: /run/secrets/gh_token`) instead of environment variables for token storage.

## Recommended Unraid Deployment

Run the **router and a worker as two containers** ([unraid-portainer.md](unraid-portainer.md), `docker-compose.yaml`):

| | `gso-router` | `gso-worker` |
|---|---|---|
| GitHub token | yes | **no** |
| Docker socket (root-equivalent on the host) | **no** | yes |
| Runs jobs | no (`max_runners: 0`) | yes, in sibling containers |
| Inbound port | 9200 (workers), optionally 9100 (control) | none |

The token and the Docker socket never share a container. A job that escapes its container reaches a host socket but no token. A compromised router holds the tokens but cannot start containers on the host. The single-container layout (socket plus token in one container) remains supported for rollback and for hosts without other workers.

## Comparison: Deployment Models

| Model | Permissions | Complexity | Best For |
|---|---|---|---|
| gso + repo PAT | `administration:write` per repo | Low | Personal repos, small scale |
| Organization runner controller | `organization_self_hosted_runners:write` | Varies | Free org, narrower permissions; requires another controller or future GSO support |
| GitHub App + gso | `administration:write` (installation) | Medium | Teams, automated token rotation |
| Actions Runner Controller (ARC) | Org-level or app | High | Kubernetes clusters, large scale |
| Dedicated runners per repo | `administration:write` per repo | Low | Single repo, always-on |

GSO currently uses repository scale sets, which require `administration:write` per repository. Organization runners offer a narrower permission, but require another controller or future GSO support.
