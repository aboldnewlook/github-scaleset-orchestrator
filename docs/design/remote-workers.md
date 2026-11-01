# gso Remote Workers — Design Spec (v2)

| | |
|---|---|
| Status | **Proposed v2** (2026-11-01). Revised after a plan review, a security review, a feasibility review, and the owner's answers to the open questions. Owner decisions D1, D3, D4, D5 and D7 are final (2026-11-01); see §15. v1 is superseded; its deltas are listed under "Changes from v1". |
| Scope | The github-scaleset-orchestrator (gso) router and its workers, and the Burst Runner desktop app as a gso worker |
| Evidence base | **gso:** `origin/main` @ `8c4c5e6`; gso `file:line` cites are against that commit. **Library:** `actions/scaleset`, cited `v2/…` for v0.2.0 (current) and `v5/…` for v0.5.0 (target). **Burst:** the working tree at `forgejo-ci-unraid/desktop`, cited `desktop/…`. **Reviews:** a security review (cited `SEC-Fn`), a feasibility review (`FEAS §n`) and a plan review (`PLAN`). The reviews are held by the maintainer and are not in this repository. |
| Home | This file. Spike results: `docs/design/remote-workers-p0-results.md` (added after P0). |
| Labels | **VERIFIED** = read in code or docs. **INFERRED** = reasoned from verified facts. **UNKNOWN** = not established; see §15. |

---

## Changes from v1

| # | Change | Source |
|---|---|---|
| 1 | **The design targets `actions/scaleset` v0.5.0.** §5.2 and §7 are rewritten against the single `Scale(msg)` method. Messages are acked only after `Scale` succeeds, so every handler must tolerate redelivery. The scaler now caches statistics itself, deduplicates Started and Completed messages, and treats `JobCompleted{canceled}` with an empty `RunnerName` as a reassignment. A listener supervisor handles retry and degraded state. **Both repos move to Go ≥ 1.27.3.** | PLAN, FEAS §6 |
| 2 | **Per-job fork filtering is no longer a gate.** On github.com jobs are pushed as `JobAssigned`; there is no acquire step, and any idle runner can take any job. All policy now attaches to the **(repo × class) scale set**. An optional fork-cancel runs after the fact: it only ever denies and it fails closed. | PLAN, SEC-F1/F2, FEAS §1 |
| 3 | **Executor follows class.** Linux classes always use the docker executor. macOS and Windows classes use native only. Native is opt-in per worker, runs as a **dedicated low-privilege OS user**, and moves to a later phase (P5). | Owner decision 3, SEC-F1/F3 |
| 4 | **Unraid runs two containers:** a router with no Docker socket and `max_runners: 0`, plus a separate always-on `gso worker` container that is enrolled like any other worker and uses docker only. The v1 recommendation of "router-only, no Unraid execution" is dropped. §3 and §9 now include a compose sketch. | Owner decision 1 |
| 5 | **Public-repo policy has two layers, and the more restrictive one wins.** The router has a global default (off) with per-repo overrides. Each worker has a local opt-in that starts from a router-pushed default. Opted-in public repos run on docker only, for every job. GitHub's fork-PR approval setting is documented as the **real gate**. Visibility is re-checked on every `JobAssigned`. | Owner decision 2, SEC-F1/F6/F7 |
| 6 | **Worker credentials are bound at enrollment** to admin-declared classes, slots and repos. Advertised capabilities are clamped to those bounds. A second `OpenSession` is **rejected** while one is live (v1 superseded the old session). The credential is kept out of reach of the job user. | SEC-F3, owner decision 3 |
| 7 | **Workers verify every JIT config** before launching it: the host must be `github.com`, the repo must match the offer, and the repo must be on the worker's local allowlist. Offers are bound to the session. Runner names are bound to the worker. Adoption after a router restart is verified with `GetRunnerByName`. | SEC-F4/F5, PLAN |
| 8 | **A worker restart no longer kills jobs.** Workers persist runner records and re-attach on start. The docker executor separates *detach* (shutdown) from *kill* (cancel). Today both `Reconcile` and `defer remove` force-remove containers. | PLAN |
| 9 | **The join string carries an `advertise` address**, because a containerised router cannot discover its own LAN address. The worker verifier checks the **SPKI pin only** and ignores validity dates. The router reuses its key whenever it re-issues the cert. | PLAN |
| 10 | **Labels.** Only the *default class* (linux-x64) carries `self-hosted` and the other configured labels. Every class carries `gso`, its OS label and its arch label. Bare `runs-on: self-hosted` (as in prune) keeps landing exactly where it does today. New classes are targeted with `[gso, <OS>, <ARCH>]`. | FEAS §3 |
| 11 | **Capacity and wake-up.** Advertised capacity becomes `running + free` per class instead of total slots, which cuts the requeue churn. The capacity 0→N wake-up stall (ARC#4715) gets a provisional warm-runner mitigation and a spike. v1's claim that "`Reevaluate` makes it irrelevant" is withdrawn. | FEAS §2/§3 |
| 12 | **Phases.** v1's P2 was a flag-day. It is split into **P2a** (v0.5, `Scale`, 409 retry-without-delete; ships on today's Unraid), **P2b** (proto, fleet, enrollment, behind `workers.listen`) and **P2c** (per-class layout and public policy, `feat!`). A **cross-repo release gate** is added before Burst P4. Each phase has an honest rollback, including the fact that the Semaphore path is removed in P2b. | PLAN |
| 13 | **Hardening.** TLS `MinVersion` 1.3. `Enroll` is rate-limited. Credentials are formatted `<worker_id>.<secret>`. Slots are clamped. Redaction tests cover the JIT config, credentials and codes. The runner version and SHA-256 are pinned per gso release. File credentials looser than 0600 are refused. `workers add`/`revoke` work on the unix socket only. The session owner is `instance_id`. Job containers on the LAN are documented as having network access. | SEC hardening, SEC-F6/F8/F9, PLAN nits |
| 14 | **Answered or corrected.** Q6: polls are held about 50 s with a 2-minute cap. Q11: OS keychains are **not** a boundary against processes running as the same user. Q3: the code shows that `RemoveRunner` makes a JIT unusable, and never-connected ephemeral runners expire within ≤ 1 day. Names are capped at 45 characters with a sanitised charset. The rationale for rejecting "one scale set per repo with every class's labels" is fixed. | FEAS §5–§7, SEC-F3 |

---

## 0. Decision summary

**The router.** A single gso process is the router. It alone holds the PATs, owns the GitHub scale sets and their message sessions, and mints JIT runner configs.

**Workers.** A worker dials the router over TLS on the intranet and opens a session. Its capabilities are clamped to what the admin declared at enrollment. It receives offers on a server stream and accepts one with a unary call. The JIT config comes back in that call's response. The worker verifies the config against the offer and its own allowlist, then runs the GitHub runner itself. From there the runner talks to GitHub directly.

**Scale sets and labels.** There is one scale set per (repo × OS/arch class). Linux classes run in docker on any worker OS. macOS and Windows classes run natively, opt-in only, as a dedicated low-privilege user. Bare `self-hosted` keeps routing to linux-x64. Other classes are targeted with `[gso, macOS, ARM64]` and similar.

**On Unraid**, the router runs without a Docker socket, and a separate worker container does the Unraid share of execution.

**Trade-offs accepted:**
- The router gains an inbound port and a small persistent state.
- Two repos are now coupled through a public Go API and a shared Go toolchain floor.
- Every enrolled machine is trusted with the secrets of the jobs it runs.
- Public repos are refused unless both the router and the worker opt in. This is a breaking change.
- Native macOS/Windows jobs need a one-time admin setup on each machine.

---

## 1. As-built architecture

### 1.1 Intended — what the docs claim

| Claim | Where |
|---|---|
| "No inbound ports are opened." | `docs/security.md:117-128` |
| The PAT is used only by the orchestrator; the runner gets only a single-use JIT config. | `docs/security.md:62-70` |
| Configured labels are applied to all runners, "in addition to auto-detected OS/arch". | `config.example.yaml:27-29` |
| `max_runners` is the "maximum concurrent runner processes". | `config.example.yaml:10-11` |

### 1.2 As-built — what executes (unchanged from v1 unless marked)

**Process and executor**
- `gso start` is one process. It holds a control server and an orchestrator (`cmd/start.go:41-133`).
- It uses exactly one executor, `subprocess` or `docker` (`orchestrator.go:147-170`, `config.go:191-193`), behind the interface `Executor.Run(ctx, name, jit)` (`internal/runner/worker.go:20-23`).

**Scale sets and labels**
- There is one scale set per repo, named `gso-<os.Hostname()>-<repoShort>` (`orchestrator.go:142-145,229`, `naming.go:23-25`).
- `query.go` recomputes that name from the hostname in three places (`query.go:47-68,97-115,184-209`).
- Labels are `cfg.Labels` (default `[self-hosted]`) plus the gso process's own GOOS/GOARCH (`orchestrator.go:221-226,356-378`, `config.go:69`).
- The session owner string is the hostname (`orchestrator.go:267`).

**Capacity**
- One global `Semaphore(max_runners)` is shared by all repos (`orchestrator.go:172-175`, `scaler.go:25-100`).
- Every listener is given `MaxRunners = max_runners` (`orchestrator.go:310-317`).
- `max_runners` must be ≥ 1 (`config.go:188-190`).

**Scaler (v0.2 contract)**
- It creates `min(desired−existing, available)` runners (`scaler.go:147-192`).
- The JIT config is minted before the semaphore slot is acquired (`scaler.go:307-317`).
- Each runner's `worker.Run` blocks in its own goroutine (`scaler.go:342-377`).
- When a job completes, the slot is released before the runner process exits (`scaler.go:263-268`).

**Listener supervision.** Each repo's listener runs in its own goroutine. An error is pushed to `errCh`, and nothing restarts that repo's listener (`orchestrator.go:180-208`). **VERIFIED; new in v2.**

**409 handling.** On 409, gso deletes and recreates the scale set (`orchestrator.go:267-297`), matching on the string "409" or "Conflict" (`:270`).

**Executors**
- **Native:**
  - Temp dir plus `ACTIONS_RUNNER_INPUT_JITCONFIG` (`worker.go:32-66`); the environment is stripped of `GSO_*`/token variables (`worker.go:71-94`).
  - The runner comes from `releases/latest` (`manager.go:231-232`), and its checksum is enforced only if a `.sha256` asset exists (`manager.go:144`).
  - There is no `win` asset mapping and no `.zip` support (`manager.go:253-278`).
- **Docker:**
  - It uses a hard-coded `/var/run/docker.sock` (`docker.go:39-47`).
  - **Containers are force-removed when `Run` returns, including on ctx cancel** (`defer w.remove`, `docker.go:157`; ctx return `:171-174`).
  - **`Reconcile` force-removes every container carrying the instance label at start** (`docker.go:105-129`).
  - The container gets no `NetworkMode`, so it sits on the default bridge with LAN access (`docker.go:84-103`). **VERIFIED; v2 emphasis.**

**Image.** linux/amd64 only (`Dockerfile:16,34`, `docker.yml:35-44`; published `latest` is a single manifest). The same image serves as both manager and job image (`Dockerfile:69-79`).

**Control plane**
- One JSON request per connection, with a 30 s deadline (`server.go:294-322`).
- A single admin token covers every method (`server.go:28,83,312`).
- Self-signed cert, with no `MinVersion` set in `tls.Config` (`tls.go:41,120`; SEC hardening). The key and cert are regenerated only when the files are missing (`tls.go:25-48`). The client verifies with TOFU (`tls.go:203-256`).

**Credentials**
- The PAT is resolved only in `service.NewScaleSetClient` (`client.go:16-36`).
- File token sources only *warn* when permissions are loose (`internal/config/token.go:45-47`, SEC hardening).

**Other gaps**
- There is no public-repo visibility check anywhere in the code.
- goreleaser builds only linux and darwin (`.goreleaser.yml:9-14`). `.goreleaser.yaml` is an unused second config; goreleaser loads `.goreleaser.yml` (VERIFIED with `goreleaser check`).
- `go build ./...` is clean for windows/amd64, windows/arm64, darwin/arm64 and linux/arm64. **VERIFIED in this session.**

**Library v0.5.0 (target, VERIFIED)**
- `go 1.27.3` (`v5/go.mod:3`). Today gso is on `go 1.26.1` and Burst on `go 1.26.0`.
- The Scaler interface is `Scale(ctx, *RunnerScaleSetMessage) error` (`v5/listener/listener.go:101-123`). It is called with:
  - a synthetic initial message, `MessageID −1` (`:96-99, :138-143`);
  - **`nil` on an idle poll**, where the listener does *not* cache statistics (`:106-108`);
  - each real message otherwise.
- **The ack happens only after `Scale` returns nil. An error stops `Run` and the message is redelivered** (`:114-120, :169-179`).
- Messages carry `JobAssignedMessages`, `JobStartedMessages` and `JobCompletedMessages` (`v5/types.go:105-111`).
- Statistics carry `TotalAvailableJobs`, `TotalAcquiredJobs`, `TotalAssignedJobs` and `TotalRunningJobs` (`v5/types.go:133-137`).
- Polls are held about 50 s, with a 2-minute client cap (`v5/README.md:96,99`).
- An unclaimed assignment is cancelled and requeued "up to 3 times with incremental delays" (`v5/README.md:116`).
- `ListRunnerScaleSets` (`v5/client.go:467`), `GetRunner` (`:797`), `GetRunnerByName` (`:824`) and `RemoveRunner` (`:857`) are available. Conflict errors are typed as `ConflictError` (FEAS §4, `v5/errors.go:31`).
- On github.com, `JobAssigned` arrives with no acquire step, and `totalAcquiredJobs` reads 0. Acquisition happens on GitHub's side (FEAS §1: scaleset#75, #107, #133).

### 1.3 Findings that shape the design (ranked for this design)

| # | Finding | Evidence | Cost / disposition |
|---|---|---|---|
| F1 | **409 handling deletes the scale set.** Remote jobs would survive a router restart, and this deletion would then kill them. | `orchestrator.go:267-297` | HIGH. Fixed in P2a by retrying without deleting (§7). |
| F2 | **Listener death is permanent.** One repo's listener error is never retried. Under v0.5, *any* `Scale` error ends `Run`. | `orchestrator.go:180-208`, `v5/listener.go:169-171` | HIGH. Fixed in P2a with a supervisor plus a "Scale returns nil" policy (§5.2). |
| F3 | **Docker executor kills jobs on the executor's own restart or cancel.** | `docker.go:105-129, 157, 171-174` | HIGH for workers. P1 splits detach from kill; P2b adds attach. |
| F4 | **Identity is the hostname.** The documented compose sets no `hostname:` and has no state volume, so a recreated container gets new scale sets and a new cert (INFERRED). A restarted router then never sees its old session; it creates fresh scale sets and leaves orphans whose labels overlap. | `unraid-portainer.md:83-84`, `docker-compose.yaml:8-9`, `orchestrator.go:229` | MEDIUM. P2a moves to `instance_id` naming with an explicit migration (§6.3). |
| F5 | **Labels describe the manager host, not where the job executes.** | `orchestrator.go:225-226` vs `docker.go:100` | MEDIUM. The class model removes it (§6). |
| F6 | **Bare `runs-on: self-hosted` is in use** (prune) and would match every class. | FEAS §3 (`gh api …/runs/31050056283/jobs`) | MEDIUM. §6.2 routes it to the default class only. |
| F7 | **Image is amd64-only; no Windows native; docker socket path is pinned.** | `Dockerfile:16,34`, `manager.go:253-278`, `docker.go:40-42` | Blocks P4 and P5. Fixed in P1 and P5. |
| F8 | **JIT is minted before the slot; the runner binary floats on `latest`.** | `scaler.go:307-317`, `manager.go:231-232,144` | LOW / supply chain. Fixed in P1. |
| F9 | **Burst Forgejo gaps**: engine-wide busy count, token in argv, no-drain disconnect. | `desktop/internal/docker/docker.go:206,252`, `desktop/service.go:116-119` | Removed together with Forgejo (§11). |

---

## 2. Goals and non-goals

**Goals**
- **G1.** One router is the sole GitHub registration point and job router.
- **G2.** Workers connect when they have availability and drain when they don't.
- **G3.** Adding capacity takes one admin command on the router plus one paste on the machine.
- **G4.** The router process does no job execution and holds no Docker socket. Unraid's own compute is contributed by a separate, enrolled worker container.
- **G5.** Every OS/arch is supported. Linux classes run in docker on any worker; macOS and Windows classes run natively, opt-in.
- **G6.** PATs never leave the router.
- **G7.** In-flight jobs survive a router restart, a worker restart, and short worker network blips.
- **G8.** Public-repo work runs only where both the router and the worker have opted in, and only in docker.

**Non-goals** (each has a trigger in §16)
- Workers outside the intranet.
- Multi-router HA.
- Org-level scale sets.
- GitHub App auth.
- Bin-packing by CPU or memory.
- VM isolation for macOS.
- Caches shared across workers.
- Router-proxied runner traffic.
- Network egress filtering on desktop workers.

---

## 3. Architecture

```
                       GitHub (scale-set API, broker, job data plane)
                         ▲   ▲                                 ▲
     PAT: scale sets,    │   │ long-poll sessions              │ runners ⇄ GitHub directly
     JIT mint, run/repo  │   │ (one per repo×class)            │
     lookups             │   │                                 │
  ┌──────── Unraid (docker-compose) ───────────────────┐        │
  │ ┌──────────────────────────────┐  ┌───────────────┐│        │
  │ │ gso-router  (gso start)      │  │ gso-worker    ││        │
  │ │ max_runners: 0, NO docker.sock│◄─┤ (gso worker)  ││────────┤
  │ │ listeners, scalers, fleet    │  │ docker exec.  ││        │
  │ │ Connect :9200 gso.worker.v1  │  │ linux-x64     ││        │
  │ │ control socket (+ :9100 opt) │  │ docker.sock   ││        │
  │ └──────────────▲───────────────┘  └───────────────┘│        │
  └────────────────┼────────────────────────────────────┘        │
                   │ TLS 1.3, SPKI-pinned, intranet, control plane only
      ┌────────────┼──────────────┬──────────────────┐            │
 ┌────┴──────┐ ┌───┴────────┐ ┌───┴────────┐                       │
 │ Mac M1    │ │ Win PC     │ │ Linux box  │  ─────────────────────┘
 │ Burst     │ │ Burst      │ │ gso worker │
 │ docker:   │ │ docker:    │ │ docker:    │
 │ linux-arm64│ │ linux-x64  │ │ linux-x64  │
 │ native*:  │ │ native*:   │ └────────────┘
 │ macos-arm64│ │ windows-x64│   * opt-in, P5, dedicated OS user
 └───────────┘ └────────────┘
```

**Assumption, stated rather than verified:** every worker has outbound HTTPS to the GitHub domains listed in `docs/security.md:121-126`.

**Why the router and the Unraid worker are separate containers:** the PATs and the Docker socket (root-equivalent on the host, `docs/security.md:99`) never share a container. If a job escapes its container on Unraid, it lands where there are no PATs. If the router is compromised, there is no host socket to use.

---

## 4. Worker ↔ router protocol: `gso.worker.v1` over ConnectRPC

### 4.1 Packaging

| Item | Location | Notes |
|---|---|---|
| Proto source | `proto/gso/worker/v1/worker.proto`, `buf.yaml`, `buf.gen.yaml` | buf `STANDARD` lint. CI runs `buf breaking` against `main`. |
| Generated code (**public**) | `gen/gso/worker/v1/` (`workerv1`) and `…/workerv1connect/` | Committed. `task generate`; CI fails if the generated code differs. |
| Client and executors (**public**) | `worker/`, `runner/` (moved from `internal/runner`) | Imported by Burst. |
| Server | `internal/fleet/` | Implements `workerv1connect.WorkerServiceHandler`. |

**New dependencies:** `connectrpc.com/connect`, `google.golang.org/protobuf`, and the `buf` CLI (dev only).

**Import rule:** `gen/`, `worker/` and `runner/` must not import `internal/*`, cobra, viper, bubbletea or `actions/scaleset`. A `go list -deps` test enforces this.

### 4.2 Transport and trust anchor

**Listener**
- A separate listener, `workers.listen`, off by default (suggest `:9200`). Worker credentials therefore never reach admin methods.
- Connect protocol; HTTP/1.1 is fine, and HTTP/2 is used via ALPN when available.
- **`tls.Config{MinVersion: tls.VersionTLS13}`**. Today's `tls.go` sets no minimum (`tls.go:41,120`).

**Key and certificate (v2)**
- The worker listener has its **own key file**, `<state>/workers/tls/key.pem`. It is generated once and **never regenerated while it exists**.
- The certificate is re-issued from that same key when it expires (`tls.go:65`, one-year validity) or on demand.
- The worker's verifier is **SPKI pin only**: it matches the public key and ignores validity dates and hostnames. The pinned key is therefore the only trust anchor.
- If the key is lost, every worker must be re-enrolled. That is documented; see §9 for the state volume.

**Advertise address (v2)**
- `workers.advertise` (`host:port`) is the address written into join strings. A containerised router cannot discover its LAN address.
- `gso workers add` **refuses to print a join string** when no advertise address is configured or passed, and the error message says how to set one.
- A worker may override the dial address (for example `gso-router:9200` on the compose network). The pin still applies.

**Source allowlist**
- `workers.allow_cidrs`, enforced with `control.CheckIPAllowed` (`tls.go:277`).
- The default when empty is private ranges only: RFC1918, `100.64/10`, `fc00::/7` and loopback.
- *It is not defaulted to "the router's subnet"*, because inside a container that subnet is the docker bridge. The docs recommend narrowing it to the LAN subnet plus the compose network.

### 4.3 Service sketch (field numbers illustrative)

```proto
syntax = "proto3";
package gso.worker.v1;
option go_package = "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1;workerv1";
import "google/protobuf/duration.proto";
import "google/protobuf/timestamp.proto";

service WorkerService {
  rpc Enroll(EnrollRequest) returns (EnrollResponse);              // auth: one-time code only; rate-limited
  // All below: "Authorization: Bearer <worker_id>.<secret>"
  rpc OpenSession(OpenSessionRequest) returns (OpenSessionResponse);
  rpc Subscribe(SubscribeRequest) returns (stream Directive);      // offers, cancels, drain, keepalive
  rpc Heartbeat(HeartbeatRequest) returns (HeartbeatResponse);
  rpc AcceptOffer(AcceptOfferRequest) returns (AcceptOfferResponse);
  rpc RejectOffer(RejectOfferRequest) returns (RejectOfferResponse);
  rpc ReportRunner(ReportRunnerRequest) returns (ReportRunnerResponse);
  rpc CloseSession(CloseSessionRequest) returns (CloseSessionResponse);
}

enum OS   { OS_UNSPECIFIED = 0; OS_LINUX = 1; OS_MACOS = 2; OS_WINDOWS = 3; }
enum Arch { ARCH_UNSPECIFIED = 0; ARCH_X64 = 1; ARCH_ARM64 = 2; }
message Class { OS os = 1; Arch arch = 2; }
enum ExecutorKind { EXECUTOR_KIND_UNSPECIFIED = 0; EXECUTOR_KIND_NATIVE = 1; EXECUTOR_KIND_DOCKER = 2; }
message Executor { ExecutorKind kind = 1; repeated Class classes = 2; string detail = 3; }
message Capabilities { string worker_version = 1; uint32 slots = 2; repeated Executor executors = 3; }
// Router-enforced bounds, fixed at enrollment by the admin; capabilities are clamped to these.
message Bounds { repeated Class classes = 1; uint32 max_slots = 2; repeated string repos = 3; /* empty = all router repos */ }
enum Availability { AVAILABILITY_UNSPECIFIED = 0; AVAILABILITY_AVAILABLE = 1; AVAILABILITY_DRAINING = 2; AVAILABILITY_UNAVAILABLE = 3; }
enum RunnerPhase  { RUNNER_PHASE_UNSPECIFIED = 0; RUNNER_PHASE_STARTING = 1; RUNNER_PHASE_RUNNING = 2; RUNNER_PHASE_EXITED = 3; }
message RunnerState { string runner_name = 1; int64 runner_id = 2; string repo = 3; Class class = 4; RunnerPhase phase = 5; }
message PublicPolicy { bool worker_default_accept = 1; }   // pushed default; worker may override locally

message EnrollRequest  { string enrollment_code = 1; string worker_name = 2; string machine_id = 3; }
message EnrollResponse { string credential = 1; /* "<worker_id>.<secret>" */ Bounds bounds = 2; repeated string owner_scopes = 3; /* seeds the worker-local allowlist, e.g. "jrschumacher/*" */ }

message OpenSessionRequest  { string machine_id = 1; Capabilities capabilities = 2; Availability availability = 3; repeated RunnerState running = 4; bool accept_public = 5; /* effective local decision */ }
message OpenSessionResponse { string session_id = 1; Capabilities effective = 2; /* after clamp */ PublicPolicy policy = 3; google.protobuf.Duration heartbeat_interval = 4; google.protobuf.Duration offer_ttl = 5; repeated string rejected_runners = 6; /* failed adoption → worker kills them */ }

message SubscribeRequest { string session_id = 1; }
message Directive { oneof kind { Offer offer = 1; Cancel cancel = 2; Drain drain = 3; Keepalive keepalive = 4; } }
message Offer  { string offer_id = 1; /* 128-bit random, bound to (session, worker) */ string runner_name = 2; string repo = 3; Class class = 4; ExecutorKind executor = 5; bool public_repo = 6; google.protobuf.Timestamp expires_at = 7; }
message Cancel { string runner_name = 1; string reason = 2; }
message Drain  { string reason = 1; }
message Keepalive {}

message HeartbeatRequest  { string session_id = 1; Availability availability = 2; repeated RunnerState running = 3; bool accept_public = 4; }
message HeartbeatResponse { PublicPolicy policy = 1; }
message AcceptOfferRequest  { string session_id = 1; string offer_id = 2; }
message AcceptOfferResponse { string runner_name = 1; int64 runner_id = 2; string encoded_jit_config = 3; }
message RejectOfferRequest  { string session_id = 1; string offer_id = 2; string reason = 3; }
message RejectOfferResponse {}
message ReportRunnerRequest { string session_id = 1; string runner_name = 2; RunnerPhase phase = 3; int32 exit_code = 4; string error = 5; bool preempted = 6; }
message ReportRunnerResponse {}
message CloseSessionRequest { string session_id = 1; string reason = 2; }
message CloseSessionResponse {}
```

**Router-side checks that are part of the contract (v2)**

*Clamping (SEC-F3)*
- `effective = capabilities ∩ bounds`:
  - `slots ≤ min(bounds.max_slots, workers.max_slots_per_worker)`;
  - `NATIVE` executors may serve only `macos-*` and `windows-*`;
  - `DOCKER` executors may serve only `linux-*`.
- The clamped result is returned, so the app can show "macOS native not enrolled — ask the admin to run `gso workers update`".

*Single live session (SEC-F3)*
- An `OpenSession` for a worker whose existing session heartbeated within the lost threshold is rejected with `FailedPrecondition("session active")`, and an `alert` event is emitted.
- A legitimate worker calls `CloseSession` when it exits cleanly. After a crash it retries until its old session expires (≤ 30 s).
- v1 superseded the old session instead; that is withdrawn.

*Binding (SEC-F5)*
- `AcceptOffer` succeeds only for an `offer_id` issued to *this* session.
- `ReportRunner`, `Heartbeat.running` and `OpenSession.running` entries are honoured only for runners the router placed on *this worker*, or (after a restart) runners whose name carries this worker's name segment **and** that `GetRunnerByName` (`v5/client.go:824`) confirms exist in that repo's scale set.
- The total is capped at the worker's bounds.

**Why the JIT config is returned on accept (unchanged from v1):** it is minted only once a specific worker has committed to a slot.

### 4.4 Flows

**Enrollment**
```
admin (unix socket only, SEC-F8):
  docker exec gso-router gso workers add mac-m1 --classes linux-arm64 --slots 4 [--repos o/a,o/b]
  → gso-join:v1:<advertise host:port>:<spki-sha256>:<code>      (128-bit code, single use, 15 min, hash-only)
worker: shows host + short SPKI and asks for confirmation (SEC-F9) → Enroll(code, name, machine_id) over pinned TLS
router: rate limit (per source IP, e.g. 5/min, 30/h) → verify code → store {worker_id, name, sha256(secret), bounds}
        → return "<worker_id>.<secret>" once, plus bounds and owner_scopes
worker: stores the credential (§8.2), the pin, the bounds and the local allowlist (seeded from owner_scopes)
```
Other admin commands: `gso workers update <name> --classes … --slots … --repos …` and `gso workers revoke <name>`. Both work on the unix socket only.

**Offer → run → complete**
```
Worker                                   Router                                         GitHub
OpenSession(caps, running[], accept_public) ─► clamp; verify+adopt running[] (GetRunnerByName); then count free slots
Subscribe ═════════════════════════════════► stream (Keepalive 15 s)
Heartbeat 10 s ────────────────────────────► liveness, reconcile running[], returns PublicPolicy
                                           Scale(msg) for (repo,class): cache stats; desired = TotalAssignedJobs
                                           toCreate = min(desired − existing, fleet.Free(class, repoPolicy))
                                           fleet.Reserve → worker W
◄═════════════════ Offer{offer_id, runner_name, repo, class, executor, public_repo}
local checks: repo ∈ allowlist; public ⇒ local accept_public; executor/class enabled
AcceptOffer(offer_id) ─────────────────────► check binding → mint(runner_name) ─────────────► GenerateJitRunnerConfig
◄───────────────── {runner_id, encoded_jit_config}
verify JIT (§8.3): host github.com, repo == Offer.repo ∈ allowlist; else RejectOffer → router RemoveRunner
persist runner record → launch (docker: container; native: as dedicated user) ─────────────► runner takes a job
ReportRunner(RUNNING) ─────────────────────►
                                           Scale: JobStarted / JobCompleted (deduped) → slot freed early
ReportRunner(EXITED) ──────────────────────► record removed (first completion signal wins)
```

**Drain and cancel.** Unchanged from v1:
- Worker-initiated drain: a `DRAINING` heartbeat; when the deadline passes, the worker kills runners and reports `preempted`.
- Router-initiated drain: a `Drain` directive.
- `gso runner recycle` becomes a `Cancel` directive for remote runners.

### 4.5 Timers (constants in v1)

| Timer | Value | Notes |
|---|---|---|
| Heartbeat | 10 s | Heartbeats are the only liveness signal. A stream can look healthy behind a buffering proxy. |
| Worker lost | 30 s | This is also the window during which a second `OpenSession` is rejected. |
| Keepalive / re-subscribe | 15 s / 45 s | |
| Offer TTL | 15 s | Nothing has been minted at this point. |
| Start TTL (accepted → RUNNING) | 10 min | Covers a first image pull or runner download. |
| Router reconnect grace | 30 s | No new placements until workers re-report. |
| Worker backoff | 1 s → 15 s, jittered | |
| Listener supervisor backoff | 1 s → 5 min | §5.3 |

### 4.6 Errors and versioning (unchanged from v1)

**Errors:** `Unauthenticated`, `PermissionDenied` (revoked, outside the CIDR allowlist, or outside the bounds), `FailedPrecondition` (unknown or active session), `NotFound` (offer gone), `ResourceExhausted` (`Enroll` rate limit), `Unavailable`. **Connect error bodies never echo request headers or secrets.**

**Versioning:** changes within v1 are additive only, enforced by `buf breaking`. A breaking change means a new `gso.worker.v2` service.

---

## 5. Scheduling (rewritten against scaleset v0.5)

### 5.1 Model

**Classes.** A class is `(os, arch)`; there are six. Each class maps to exactly one executor kind:

| Class | Executor | Who can serve it |
|---|---|---|
| `linux-x64`, `linux-arm64` | **docker** | Any worker OS whose Docker engine runs that architecture natively (Docker Desktop on macOS and Windows runs Linux containers). |
| `macos-*`, `windows-*` | **native** | Opt-in per worker; P5. |

The router's legacy in-process local member keeps today's `subprocess`/`docker` choice, but only when `workers.listen` is unset.

**Consequence: a scale set never mixes executor kinds.** This is the floor that makes SEC-F1 impossible: any idle runner taking any job in its scale set gets the same executor.

**Fleet members:**
- remote workers;
- at most one in-process local member (`max_runners` slots, router host class). It exists for non-worker deployments. On Unraid it has 0 slots.

Each member has a clamped `slots` value, an `availability`, its classes, `busy` (non-terminal runners placed on it) and `accept_public`.

**Formulas:**
- `free(class, policy) = Σ (slots − busy)` over eligible AVAILABLE members.
- **`capacity(class) = runners(class, non-terminal) + free(class)`**, pushed through `SetMaxRunners` and sent to GitHub as `X-ScaleSetMaxCapacity`.
  - v1 advertised total slots. That over-assigned jobs on machines serving two classes, and each over-assigned job churned through the 3× requeue (FEAS §2, §7.5).
  - The residual over-assignment is bounded by concurrent decisions across classes on the same machine.

**`max_runners`** now means local-member slots only. `set_max_runners` resizes the local member.

### 5.2 The `Scale` handler (one scaler per repo × class)

```go
func (s *Scaler) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
    if msg != nil && msg.Statistics != nil {
        s.stats = *msg.Statistics // v0.5 does not cache; nil msg = idle poll (v5/listener.go:104-108)
    }
    if msg != nil {
        for _, a := range msg.JobAssignedMessages { // github.com pushes these; there is no acquire gate (FEAS §1)
            s.onAssigned(ctx, a) // visibility re-check (§8.4), opt-in fork-cancel (P2c), telemetry. NEVER placement.
        }
        for _, st := range msg.JobStartedMessages {
            if s.dedupe.seen(st) { continue } // redelivery after a crash/error
            s.onStarted(st)                   // idempotent: marks job-assigned on a known runner
        }
        for _, c := range msg.JobCompletedMessages {
            if s.dedupe.seen(c) { continue }
            if c.RunnerName == "" && c.Result == "canceled" { // reassignment (v5/README.md:116; PLAN), not a runner exit
                s.onReassigned(c); continue
            }
            s.onCompleted(c) // idempotent: unknown runner = no-op; frees the slot early (as scaler.go:263-268)
        }
    }
    s.reconcile(ctx) // placement below; failures are logged and evented, never returned
    return nil       // returning an error would stop this listener (v5/listener.go:169-171)
}
```

**What `reconcile` does:**
- `desired = s.stats.TotalAssignedJobs`. On github.com, acquisition is server-side, `TotalAcquiredJobs` reads 0 (FEAS §1, scaleset#75), and assignments are counted in `TotalAssignedJobs`.
- SEC-F2 suggested `TotalAcquiredJobs`. That does not apply to github.com, and SEC-F2's underlying concern (denied jobs inflating demand) disappears because v2 has no per-job deny. Spike (g) confirms the field semantics under v0.5.
- `toCreate = runnersToCreate(desired, existing, fleet.Free(class, repoPolicy))`. This is the existing function at `scaler.go:190-192`.
- For each runner to create:
  - `fleet.Reserve` (non-blocking; the member with the most free slots wins, ties go to name order).
  - `go res.Run(ctx, name, mint)`. `mint` wraps `GenerateJitRunnerConfig`, and the router calls it only on `AcceptOffer` (remote) or directly (local). Reserving before minting fixes F8.

**Idempotency without persistence**
- The dedupe set is an in-memory LRU of (kind, JobID, RunnerName, Result), holding about 2 000 entries or 1 h.
- After a router restart a redelivered `JobCompleted` is still correct. It frees an adopted runner that really did finish, and a duplicate `JobStarted` only affects telemetry. The handlers are idempotent by construction; the LRU just keeps events clean.

**Re-evaluation when capacity changes**
- When a class's capacity changes, the fleet calls `scaler.Reevaluate()`, which runs `reconcile` with the cached `stats`.
- This helps only when GitHub has *already* assigned jobs. GitHub assigns nothing to a scale set advertising capacity 0, and new capacity reaches GitHub only on the next poll (≤ ~50 s, `v5/README.md:96`). v1's claim that "`Reevaluate` makes it irrelevant" is withdrawn.

**Wake-up from capacity 0 (ARC#4715, provisional)**
- The risk: jobs queued while a class had capacity 0 stayed unassigned for more than 600 s after capacity returned.
- Provisional mitigation, pending spike (e): when a class's capacity goes 0→N, mint **one warm runner per (repo × class) that is eligible on that worker**, up to the worker's free slots. Each warm runner is removed (`RemoveRunner`) after 5 min idle.
  - Cost: idle slots on the waking machine for up to 5 min.
  - Warm runners on a public repo's scale set are docker-only and sit on opted-in workers, so they stay within policy.
- Alternative the spike evaluates: a permanent capacity floor of 1 per class, so the job is assigned and then requeues. Pick whichever the spike shows clears the queue.

**Polls do not wake for `JobStarted`** (scaleset#122, up to about 50 s late). Slot release on `JobCompleted` and on worker `EXITED` reports must tolerate that lag. The first completion signal wins.

### 5.3 Listener supervision and degrade policy (new)

**Run loop**
- Each (repo × class) listener runs inside a supervisor:
  1. `Run`.
  2. On a non-ctx error, back off from 1 s up to 5 min.
  3. Recreate the session, applying the 409 policy in §7.
  4. Start a new listener with the same scaler. The scaler keeps its runner map, so in-flight runners are unaffected.
- One repo's failures never stop other repos. Today the error goes to `errCh` and the listener is never restarted (`orchestrator.go:180-208`).

**Degraded state**
- After 3 consecutive failures, the repo × class is marked `degraded` in `live_status` with the last error. The supervisor keeps retrying at the 5 min cap.
- Capacity is unaffected.

**Shared HTTP client**
- All listeners share one HTTP client, configured through v0.5 `WithHTTPClient` with HTTP/2 `ReadIdleTimeout` and `PingTimeout` set.
- Reason: a dead HTTP/2 connection stalled a listener for about 25 min (scaleset#131), and with repos × classes listeners on one pool the impact multiplies.

### 5.4 Placement policy

1. The member must be AVAILABLE, serve the class with the executor that class requires, have free slots, and have the repo within its bounds.
2. **Public repos:** only members with `accept_public` effective. Public repos have linux-class scale sets only (§8.4).
3. The member with the most free slots wins; ties go to name order.

There are no priorities. The trigger to add them is a real complaint along the lines of "use the Linux box before the laptop".

### 5.5 Router and local slots

- `max_runners: 0` means no local member. In that case the runner download (`orchestrator.go:155-169`) and the docker executor are skipped, and **the router needs no Docker socket**.
- Validation becomes `max_runners ≥ 0`, and `0` requires `workers.listen`.
- The default stays `NumCPU` for backward compatibility; the Unraid compose sets `0`.
- The Unraid share of execution comes from the separate `gso-worker` container (§9), enrolled with conservative bounds (for example `--slots 1` or `2`, because the host is underpowered).

---

## 6. Scale-set layout, labels and `runs-on`

### 6.1 Layout

**One scale set per (repo × class)**, named `gso-<instance>-<repoShort>-<os>-<arch>`.
- `<instance>` is `runner.instance_id`, falling back to the hostname. It is introduced in P2a (§6.3).
- **Names are capped at 45 characters**, ARC's tested envelope (FEAS §6). The charset is `[a-z0-9-]`; anything else becomes `-`. If too long, the name is `gso-<inst≤10>-<repo-trunc>-<hash4>-<class>`.
- **Runner names** are `gso-<worker≤12>-<repo≤14>-<rand6>`, using the same charset and ≤ 45 characters. The worker segment is what binds a runner to a worker (§4.3).
- The **session owner** is `instance_id` (`orchestrator.go:267` passes the hostname today). This does not allow taking over a stale session, because conflicts are per scale set (FEAS §4), but it makes a 409's holder identifiable.

**Which classes exist**
- The **default class** is the router host's class unless `workers.default_class` is set. On Unraid it is linux-x64.
  - Before P2c, it is the only class.
  - From P2c on, other classes come from `workers.classes` (created eagerly) or are created lazily the first time a worker advertises them within its bounds.
- Classes are persisted in `<state>/classes.json`.
- Orphans are found with `ListRunnerScaleSets` (`v5/client.go:467`) through `gso scaleset list --all`, which filters on the `gso-<instance>-` prefix. This fixes `query.go` only seeing hostname-derived names.

**Public repos** get linux classes only (§8.4).

### 6.2 Labels and what users write (v2)

| Scale set | Labels |
|---|---|
| Default class (linux-x64) | `cfg.Labels` (default `self-hosted`) + `gso` + `Linux` + `X64` |
| Every other class | `gso` + `<OS>` + `<ARCH>`, **no `self-hosted`** |

Why: matching is a subset test, so any scale set carrying `self-hosted` matches bare `runs-on: self-hosted` (FEAS §3). The owner's workflows use the bare form (prune). With this scheme the bare form keeps landing on linux-x64 docker, which is exactly today's behaviour.

```yaml
runs-on: self-hosted                 # unchanged: default class (linux-x64)
runs-on: [self-hosted, Linux, X64]   # unchanged: default class
runs-on: [gso, Linux, X64]           # default class, explicit form
runs-on: [gso, Linux, ARM64]         # docker on M1 / ARM boxes   (P2c + multi-arch image)
runs-on: [gso, macOS, ARM64]         # native, opt-in             (P5)
runs-on: [gso, Windows, X64]         # native, opt-in             (P5)
```

**Migration note**
- `[self-hosted, macOS, ARM64]` (the GitHub convention) matches **nothing** under this scheme. It waits in the queue for up to 24 h and is then cancelled.
- P2c's release notes and a `gso` check that greps `.github/workflows/*.yml` in each configured repo flag every `runs-on` that matches no scale set. The check is advisory and needs the repo checked out locally or the contents API.
- Spike (f) confirms where bare `self-hosted` lands while per-class sets exist.

This is a decision the spec makes; the owner can override it (§15 D1).

### 6.3 Rename and the overlap window (explicit)

**P2a renames.** `gso-<host>-<repo>` becomes `gso-<instance>-<repo>-linux-x64`. The labels stay `[self-hosted, Linux, X64]` until P2c, when `gso` is added with `UpdateRunnerScaleSet` (PATCH, no rename).

Procedure:
1. Stop the old container. Today's executors die with it, so nothing is in flight.
2. Run `gso scaleset list --all`, then `gso scaleset delete` each old `gso-*` scale set, including orphans from earlier recreates (F4).
3. Start P2a.

**Window:** steps 1–3, typically a few minutes. Queued jobs wait.

**If step 2 is skipped**, two scale sets with identical labels coexist, and the old one has no session. Whether GitHub assigns jobs to a scale set with no session (and therefore no advertised capacity) is **UNKNOWN**, and spike (b) covers it. Until it is known, the overlap is treated as an outage risk, and the startup log warns about any non-current `gso-<instance>-*` scale set it finds.

**P2c adds classes.** New label sets do not overlap the default class, except on `gso`, which only matches when the workflow names an OS and arch too. There is no rename.

---

## 7. Failure handling (rewritten against v0.5)

**Rules**
- GitHub is the source of truth for job state, the router for placements, and each worker for its own processes.
- A runner record is removed by whichever completion signal arrives first: `JobCompleted` (deduped) or a worker's `EXITED` report.
- Under v0.5, messages are **redelivered** rather than lost, because the ack follows `Scale`. Handlers are idempotent (§5.2).

| Scenario | Detection | Router | Worker | Job outcome |
|---|---|---|---|---|
| **Offer not taken** | No accept within 15 s, or `RejectOffer` | Release the slot. Nothing was minted. | — | Re-placed on the next poll or `Reevaluate` |
| **JIT minted, runner never starts** | STARTING for more than 10 min, or the worker is lost before RUNNING, or the worker rejected the JIT verification | `RemoveRunner(runner_id)`. A removed runner cannot take a job: `TaskAgentNotFound` is non-retriable (FEAS §5). Drop the record. On `JobStillRunningError`, keep it. | Kill anything it launched | Re-placed |
| **Reassignment** (`JobCompleted{canceled}`, empty `RunnerName`) | `Scale` | Ignore it for runner accounting and emit an event. A requeued job reappears as a new `JobAssigned`, up to 3 times (`v5/README.md:116`). | — | Requeued by GitHub |
| **Duplicate or redelivered message** | Dedupe LRU | No-op | — | — |
| **`Scale` would fail** (mint error, GitHub 5xx, offer failure) | Inside `Scale` | Log, emit an event, **return nil**. The placement is retried on the next poll (≤ ~50 s) or on `Reevaluate`. | — | Delayed, not lost |
| **Listener `Run` exits** (`GetMessage` or ack error, session expired) | Supervisor | Back off, recreate the session (409 policy below), restart with the same scaler. Degraded after 3 failures (§5.3). | — | In-flight jobs unaffected. New jobs wait for that repo × class. |
| **GitHub session 409** | `ConflictError` (v5 typed; replaces the string match at `orchestrator.go:270`) | **Retry 10 × 30 s (ARC 0.13.1 prior art, FEAS §4) and never delete** while any adopted or placed runner belongs to the scale set. After that, mark it degraded and keep retrying at the 5 min cap. Delete and recreate only on the admin command `gso scaleset reset <repo> <class>`. | — | No destructive delete (F1). A live 409 (old container still up during a Portainer redeploy) resolves itself. |
| **Worker vanishes mid-job** | 30 s with no heartbeat | Mark lost and stop counting its slots. Runners **without** `JobStarted` → `RemoveRunner` after 60 s. Runners **with** `JobStarted` → keep (GitHub still counts them in `TotalAssignedJobs`) until `JobCompleted`, a reconnect report, or a 6 h cap. | On reconnect, reports `running[]` | A blip leaves the job unaffected. A dead machine leads GitHub to fail the job as runner-lost (INFERRED). |
| **Worker process restart** (app quit, update, crash) | Worker start | Normal `OpenSession` with `running[]`. The router verifies and adopts (§4.3). | **Attach, don't kill (v2).** Read `<state>/runners.json`; docker: re-attach to each recorded container by ID and wait on it; native: re-attach by PID and start time; remove only containers or processes **with no record**. Report phases. | Survives (G7). Needs P1's detach/kill split (F3). |
| **Worker drains** | `DRAINING` heartbeat | No new offers | Finish jobs; at the drain deadline kill them and report `preempted` | Preempted jobs fail. Accepted policy; the user controls the deadline. |
| **Router restart** | — | Supervisors restart the listeners (409 policy). For 30 s, place nothing new. Adopt `running[]` per worker **only after** name binding and a `GetRunnerByName` check, capped at bounds; failures go back as `rejected_runners`. | Keep running, retry with backoff, report on reconnect | Survives. Residual: a worker that reconnects after the grace period can cause up to its in-flight count of extra idle runners. They are bounded and get used by the next jobs. |
| **Second `OpenSession` while one is live** | Session table | Reject with `FailedPrecondition` and alert (v2; v1 superseded). | The legitimate client waits out its own stale session (≤ 30 s) | — |
| **Worker revoked** | Admin | `Cancel`, close the session, `RemoveRunner` where possible, refuse the credential from then on. | — | Stops future work only. A job already assigned runs to completion unless it is cancelled in GitHub. |
| **Repo turns public while running** | Visibility re-check on `JobAssigned` (cached 60 s) or the 10 min poll | **Quarantine:** capacity 0 for every class, `RemoveRunner` every idle runner, no new mints, emit an event. If the repo is opted in, the public policy applies instead. | — | Exposure is bounded to runners that were idle at the moment of the flip (§8.4) |
| **Capacity 0 → N (laptop wakes)** | Fleet | `Reevaluate`, plus warm runners (§5.2, spike e) | — | Pickup within one poll, if the spike confirms |

---

## 8. Security

### 8.1 Trust model (v2)

**Router**
- Holds the PATs (`administration:write`). These never go on the wire.
- **If the router is compromised, the attacker gets the PATs and can push work to every enrolled worker within that worker's local allowlist** (SEC-F4). That is why workers verify JIT configs (§8.3): it bounds the damage to repos under allowed owners.
- Enrollment and revocation run on the **unix socket only**, so leaking the TCP control token cannot enrol a rogue worker (SEC-F8). The admin CLI's remote TOFU can be closed with the existing `--trust-fingerprint` (`tls.go:230`).

**Worker**
- Receives JIT configs, so it receives the secrets of the jobs it runs. Enrolling a machine means trusting it with those secrets.

**Docker executor (the default everywhere)**
- Isolates processes and the filesystem: all capabilities dropped, `no-new-privileges`, PID limit, no host mounts (`docker.go:84-103`).
- **It does not isolate the network.** Job containers sit on the default bridge with LAN access (SEC-F6). For private repos (trusted code) that is accepted.
- Public repos: see §8.4.

**Native executor (opt-in, P5, macOS and Windows classes only)**
- Runs as a **dedicated low-privilege OS user** (§8.5), never as the desktop user.

### 8.2 Worker credentials (v2)

**Format.** `<worker_id>.<secret>`, where the secret is 256 bits. Authentication is one map lookup plus `subtle.ConstantTimeCompare`; there is no O(N) scan. The router stores only SHA-256 hashes, in `<state>/workers.json` (0600).

**Binding (SEC-F3).** Each credential carries its bounds (classes, max slots, repos). Advertised capabilities are clamped to them (§4.3). A second live session is rejected.

**Keychains are not a same-user boundary (Q11 is answered: yes, readable).**
- go-keyring on macOS reads through `/usr/bin/security`, and items it creates trust that tool (`zalando/go-keyring@v0.2.8/keyring_darwin.go:43-48,76-86`; SEC-F3).
- Windows `wincred` and the Linux Secret Service are also readable by any process of the same user.
- **So protection comes from keeping job code out of that user, not from the keychain:**
  - docker jobs have no access to the host user's keychain;
  - native jobs run as a different OS user (§8.5);
  - the keychain only protects against *other* users and against casual disclosure.

**Storage**

| Host | Where the credential lives |
|---|---|
| Burst | OS keychain |
| `gso worker` on a desktop | keychain, or a file source |
| `gso worker` on headless Linux or in a container | **file source** (`<state>/worker/credential`). There is no Secret Service there. |

File sources with permissions looser than 0600 are **refused**; today they only warn (`token.go:45-47`).

**Persistence attack (SEC-F3).** Burst's `beforeStart` and `afterStop` hooks are `/bin/sh -c` strings stored in a same-user-writable `settings.json` (`desktop/internal/shell/shell.go:28`, `desktop/internal/agent/settings.go:59`). This is irrelevant to docker jobs and blocked for native jobs by the dedicated user. It is noted here as the reason native must never run as the desktop user.

**Hardware-bound keys** (mTLS with a Secure Enclave, TPM or Windows Hello key) are the stronger fix. **Trigger:** any worker that runs native jobs for a repo the owner doesn't fully trust, or any worker outside the intranet.

### 8.3 JIT config handling (v2)

**Router side**
- Minted only inside the `AcceptOffer` handler, for a slot reserved for that session.
- Never logged and never persisted.
- `RemoveRunner` is called on every abandon path.

**Worker side, before launch (SEC-F4, owner decision 2)**
- Base64-decode the config. It is a JSON map of runner files; `.runner` carries `gitHubUrl` (INFERRED from `actions/runner` settings; confirmed in spike h).
- Require all of the following:
  - the host is `github.com`;
  - the `owner/repo` equals `Offer.repo`;
  - the repo is on the **worker-local allowlist**, seeded at enrollment from `owner_scopes` (for example `jrschumacher/*`, `aboldnewlook/*`) and editable only locally (a Burst setting, or a `gso worker` flag);
  - if `Offer.public_repo`, the local `accept_public` is effective.
- **If decoding fails or any check fails, reject and report.** This fails closed.
- In memory only. The config is passed through the environment, as today (`worker.go:52-54`, `docker.go:85`). On Docker Desktop it is visible through `docker inspect` to the socket owner, which is the same user (SEC hardening).

**Redaction**
- A test asserts that the JIT config, the bearer credential and the enrollment code never appear in slog output, events, or Connect error bodies.

### 8.4 Public repositories: two layers, most restrictive wins (owner decision 2)

**Router layer**
- `public.allow` (global default **false**), overridden per repo with `allow_public`.
- Visibility is read from `GET /repos/{o}/{r}` with that repo's PAT. Fine-grained PATs always include Metadata: read (documented GitHub behaviour, not called this session).
- It is checked at listener start, **on every `JobAssigned` (cached 60 s)**, and every 10 min.
- A public repo that is not allowed gets no scale sets. A repo that flips public is quarantined (§7).

**Worker layer**
- Each worker has an effective `accept_public`: the router-pushed `public.worker_default_accept` (default **false**), overridden locally.
- Burst exposes this as a tri-state setting: *Follow router* / *Accept* / *Refuse*.
- Router side: public jobs are offered only to workers reporting `accept_public`.
- Worker side: an offer with `public_repo` is re-checked locally (§8.3).

**When both layers allow it**
- **Docker only, for every job of that repo.** Public repos get scale sets for linux classes only, so no native scale set can exist for them, and any idle runner in their scale sets is a docker runner on an opted-in worker. SEC-F1 is closed by construction.
- The repo cache is forced to `none` (`docker.go:90-98`).

**The real gate is GitHub's fork-PR approval setting.** Document and recommend this repo setting: *Settings → Actions → General → "Approval for running fork pull request workflows from contributors" → "Require approval for all external contributors"* (exact wording to confirm in spike h).
- gso cannot stop an assigned job before a runner takes it. There is no acquire hook on github.com and no release API (FEAS §1–2).
- Docs must also warn that `pull_request_target`, `workflow_run`, `issue_comment` and `pull_request_review(_comment)` workflows can run fork-controlled input with secrets, so they should not target self-hosted labels on public repos (SEC-F7c).

**Network exposure of opted-in public jobs (SEC-F6)**
- Docker isolation does not cover the network.
- Recommend opting in only a **dedicated Linux worker on a segregated VLAN**, or a Linux worker whose job containers sit on a user-defined network with a `DOCKER-USER` policy (allow established traffic and 443 egress, drop RFC1918).
- Docker Desktop has no equivalent, so desktop workers should leave `accept_public` off. Burst shows that recommendation next to the toggle.

**Optional fork-cancel (opt-in, deny-only, fail-closed)**
- Enabled with `public.fork_cancel: true`.
- **Requires the PAT to have Actions: Read and write.** The documented PAT is `administration:write` only (`docs/security.md:7-20`).
- On `JobAssigned` for an opted-in public repo, the router calls `GET /repos/{o}/{r}/actions/runs/{WorkflowRunID}`.
- It **cancels the whole run** (`POST …/runs/{id}/cancel`) when any of the following holds:
  - `head_repository.full_name ≠ repository.full_name`;
  - `EventName` is in `public.fork_cancel_events`, which defaults to `pull_request_target, workflow_run, issue_comment, pull_request_review, pull_request_review_comment` (SEC-F7c);
  - the lookup fails after 3 attempts within 30 s (**fail closed**).
- It **never relaxes** executor or placement policy.
- Honest limit: a runner may pick the job up before the cancel lands. This limits damage after the fact; it does not prevent anything.

### 8.5 Native executor as a dedicated OS user (owner decision 3; P5)

**macOS**
- A one-time admin setup creates a hidden standard user (for example `_gsojob`) with `sysadminctl`, plus a root-owned launcher at a fixed path and a sudoers rule limited to that launcher as that user.
- The worker (desktop user, holding the credential) launches the runner through the launcher. The work dir lives under the job user's home.
- Kill targets that user's process group.
- **UNKNOWN:** whether Xcode simulators and UI tests work without a GUI login for that user. Spike in P5.

**Windows**
- A one-time admin setup creates a standard local user.
- The worker stores that user's random password with DPAPI in the desktop user's profile, which the job user cannot read, and launches with `CreateProcessWithLogonW`. A job object handles kill.

**Both**
- The job user cannot read the desktop user's keychain, files, `settings.json` or credential.
- Native also requires `--repos` bounds on the enrolment (SEC-F3 fix 1). That per-worker repo scoping ships **with** native, not later.

### 8.6 Other hardening

**Already specified in sections above:** TLS 1.3 minimum (§4.2); `Enroll` rate limit (§4.4); slot clamping, with `workers.max_slots_per_worker` defaulting to 16 (§4.3); Connect errors never echo headers (§4.6).

**Supply chain**
- The runner version and its SHA-256 are **pinned per gso release** and embedded in the binary. That replaces `releases/latest` (`manager.go:231-232`) and the optional checksum (`manager.go:144`).
- The docker job image is pinned by digest in each release.

**Join string**
- It is the root of trust (SEC-F9). The worker shows the host and a short SPKI before enrolling.
- Docs say to transfer it over a trusted channel. Its 15-minute single-use code limits replay.

**Docs.** `docs/security.md` is updated in three places:
- the inbound port;
- the worker trust model;
- the router/worker split as the recommended Unraid deployment.

---

## 9. Configuration and Unraid deployment (v2)

### 9.1 Router config

```yaml
max_runners: 0                         # local member slots; 0 ⇒ requires workers.listen
runner:
  instance_id: unraid                  # identity: scale-set names, session owner
workers:                               # absent ⇒ feature off, behaviour = P2a
  listen: ":9200"
  advertise: "unraid.lan:9200"         # REQUIRED to print join strings
  allow_cidrs: [192.168.1.0/24, 172.16.0.0/12]   # LAN + compose network; default = private ranges
  max_slots_per_worker: 16
  default_class: linux-x64             # default: router host class
  classes: []                          # eager classes (P2c); others appear lazily within bounds
public:
  allow: false                         # global default for repos
  worker_default_accept: false         # pushed to workers; they may override locally
  fork_cancel: false                   # needs PAT Actions: Read and write
  fork_cancel_events: [pull_request_target, workflow_run, issue_comment, pull_request_review, pull_request_review_comment]
repos:
  - name: owner/repo
    classes: [linux-x64, linux-arm64]  # optional allowlist
    allow_public: false                # per-repo override of public.allow
```

**Router state** lives in `os.UserConfigDir()/gso/`, which in the container is `/home/runner/.config/gso` (`Dockerfile:62,83`), and **must be on a volume**:
- `tls/` (control)
- `workers/tls/key.pem` (pinned key)
- `workers.json`
- `classes.json`

### 9.2 Unraid compose (sketch; replaces the gso-only compose in `docs/unraid-portainer.md` and `docker-compose.yaml`)

```yaml
services:
  gso-router:
    image: ghcr.io/aboldnewlook/github-scaleset-orchestrator:<pinned tag>
    command: ["start"]
    hostname: gso-router
    environment:
      GITHUB_TOKEN: ${GITHUB_TOKEN}
      GSO_MAX_RUNNERS: "0"
      GSO_RUNNER_INSTANCE_ID: unraid
      GSO_WORKERS_LISTEN: ":9200"
      GSO_WORKERS_ADVERTISE: ${GSO_WORKERS_ADVERTISE}     # e.g. unraid.lan:9200
    volumes:
      - ./config.yaml:/etc/gso/config.yaml:ro
      - gso-router-state:/home/runner/.config/gso
    ports: ["9200:9200"]
    restart: unless-stopped
    # deliberately NO /var/run/docker.sock

  gso-worker:
    image: ghcr.io/aboldnewlook/github-scaleset-orchestrator:<same tag>
    command: ["worker", "--router", "gso-router:9200", "--executor", "docker", "--slots", "2"]
    environment:
      GSO_WORKER_JOIN: ${GSO_WORKER_JOIN}                 # first start only; ignored once enrolled
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock         # job containers are siblings on Unraid's engine
      - gso-worker-state:/home/runner/.config/gso         # credential file, pin, runners.json
    depends_on: [gso-router]
    restart: unless-stopped
    # NO GITHUB_TOKEN, no config.yaml

volumes: { gso-router-state: {}, gso-worker-state: {} }
```

**Bootstrap**, done once:
1. Deploy the router.
2. Run `docker exec gso-router gso workers add unraid --classes linux-x64 --slots 2`.
3. Put the printed join string in the stack env as `GSO_WORKER_JOIN` and redeploy the worker.

**Network.** The worker's source IP is on the compose network, which is RFC1918 `172.16/12`, so `allow_cidrs` must include that network.

**Container user.** The worker container needs to reach the socket. Like today's docker mode, that may mean running as root or adding the socket's group (`docs/security.md:146`).

---

## 10. gso changes (module level)

| Package | Change | Phase |
|---|---|---|
| `go.mod` | `go 1.27.3`; `actions/scaleset v0.5.0` | P1 / P2a |
| `runner/` (**public**, moved) | `Executor` gains `Classes()`, `Attach(ctx, record)` and an explicit `Kill`. **ctx cancel means detach, not remove** (F3). Configurable docker endpoint. Pinned runner version and hash. Windows `.zip` and `win` mapping (P5). go-winio named-pipe dialer on Windows (P4). `Reconcile` becomes "remove only containers with no record". | P1 (P4, P5 parts) |
| `internal/scaler` | `Scale(msg)` handler, stats cache, dedupe, reassignment handling; reserve-before-mint; `Adopt`, `Reevaluate`, warm runners | P2a (fleet parts P2b/P2c) |
| `internal/orchestrator` | Listener supervisor and degraded state; typed 409 retry-without-delete; HTTP/2 health; `instance_id` naming and session owner; `ListRunnerScaleSets` | P2a |
| `proto/`, `gen/…` (**public**) | The contract | P2b |
| `worker/` (**public**) | Client: enrollment, session loop, offer verification (§8.3), `runners.json` persistence and attach, availability, drain with a hard deadline, status | P2b |
| `internal/fleet/` | Registry (`workers.json`, bounds), enrollment codes, rate limit, sessions (single live), clamp, placement, binding checks, adoption via `GetRunnerByName`, the local member (**replaces the Semaphore**) | P2b |
| `internal/config` | `workers.*`, `public.*`, `repos[].classes`, `repos[].allow_public`, `max_runners ≥ 0` | P2b / P2c |
| `internal/control`, `cmd/` | `workers add/update/list/drain/revoke` (unix socket only); `gso worker`; `gso scaleset list --all`, `reset`; workers in `live_status` | P2a / P2b |
| `internal/tui` | Workers view and degraded markers. **Coordinate with the owner's in-flight TUI branches.** | P2b |
| Labels, classes, policy | Per-class layout, the label scheme in §6.2, two-layer public policy, visibility re-check and quarantine, fork-cancel | P2c |
| `Dockerfile`, `docker.yml`, `.goreleaser.yml` | Multi-arch (`TARGETARCH`, `actions-runner-linux-${arch}`, buildx `linux/amd64,linux/arm64`, native arm64 CI runner preferred); windows in goreleaser | P1 |

---

## 11. Burst app changes (module level)

**Defaults (v2).** The docker executor is on and native is off on every OS. Native arrives in P5 and needs a one-time admin setup.

**What each machine provides by default:**
- **M1:** `linux-arm64`
- **Windows PC:** `linux-x64` (via go-winio)

| Module | Keeps | Changes |
|---|---|---|
| `internal/agent/policy.go` | **Unchanged** | — |
| `internal/agent/agent.go` | Tick loop, phases, retries, Awake, adopt-on-restart, `Backend` shape (`agent.go:13-22`) | `Backend` is implemented by the gso adapter:<br>• `Ping`: the router session and Docker are healthy.<br>• `Running`: AVAILABLE, or busy.<br>• `Start`: executors ready (image pulled), then `SetAvailability(AVAILABLE)`.<br>• `Drain`: `Client.Drain(now + DrainMinutes)`. This adds the hard-kill the app lacks today.<br>• `Jobs`: `Status().Busy`.<br>Strings change at `agent.go:235,283`. |
| `internal/agent/settings.go` | Mode, idle/AC/load/display policy, DrainMinutes, PollSeconds, hooks, DockerPath/DockerHost | **Remove:** the Forgejo fields and the runner config (`settings.go:23-24,30-32,50-51,95-109,192-209`).<br>**Add:** `RouterAddress`, `RouterPin`, `WorkerName`, `Enrolled`, `Allowlist` (owner/repo globs, local only), `AcceptPublic` (tri-state, default *Follow router*), `NativeExecutor` (default off, P5), `JobImage` (default: the digest of the gso release this build pins).<br>The credential lives in the **keychain**, never in `settings.json`. |
| `internal/docker` | CLI discovery and engine checks (`bin`, `extraPath`, `Env`, `HideWindow`, `Ping`, `Version`) | **Delete** the Forgejo container lifecycle (`docker.go:24-28,157-308`). **Add** engine-endpoint resolution (`docker context inspect`), which is passed to gso's docker executor. |
| `internal/gso` (**new**) | — | The adapter: executors, `worker.Client`, enrollment UI glue, `runners.json` in the app's config dir |
| `service.go`, `frontend/` | Info, Status, SetMode, SaveSettings, Logs, Autostart | **Setup:** paste the join string → show host and short SPKI → confirm.<br>**Settings:** allowlist, accept-public (with the recommendation to leave it off on desktops, §8.4), executor toggles (native disabled until P5, with a warning), and the effective classes after clamping.<br>**Quit:** `CloseSession`, but docker jobs keep running and are re-attached on the next launch (§7). |
| `internal/e2e` | — | Replace the real-Forgejo e2e with an in-process fake router built on the generated handler |
| `probe`, `awake`, `shell`, `tray.go` | **Unchanged** | — |
| `go.mod` | — | `go 1.27.3` plus a pinned gso release (the gate in §14). Whether Wails v3 works on Go 1.27.3 is UNKNOWN (spike i). |

---

## 12. Alternatives considered

| Alternative | Why it was plausible | Specific reason it lost |
|---|---|---|
| **Do nothing**: a full gso per machine | Zero new code | It copies an `administration:write` PAT to every laptop, needs a repo list and a token on each machine (against G3), and the routing across same-label scale sets is UNKNOWN. |
| **The router proxies runner traffic** | One egress point | The router host is underpowered, artifacts and logs would flow through Unraid, and GitHub offers no relay interface for the runner protocol. |
| **Persistent self-hosted runners** | GitHub-native | Not ephemeral, and there is no availability-driven scaling. |
| **One scale set per worker** | GitHub would route only to scale sets with capacity | It means N × M scale sets plus create/delete churn as laptops sleep, and workflows would still target shared labels. |
| **One scale set per repo carrying every class's labels** | Fewest scale sets | *Corrected rationale (FEAS §3):* `RequestLabels` does arrive in `JobAssigned` before any runner exists, but **any idle runner in a scale set takes any assigned job** (`v5/README.md:20`). A macOS runner minted for a macOS job could take a Linux job. Placement by class needs one scale set per class. |
| **Per-job fork filtering by not acquiring** (v1 amendment) | Precise policy | **There is no acquire gate on github.com.** Jobs are pushed as `JobAssigned`, and runners are not bound to jobs (FEAS §1, SEC-F1/F2). Policy must be per scale set. Fork-cancel survives only as an opt-in, deny-only extra. |
| **Mixing native and docker runners in one scale set** | Lets trusted jobs on public repos go native | Any idle runner takes any job, so the most permissive runner sets the effective policy (SEC-F1). v2 ties each class to one executor kind instead. |
| **Superseding a live session on reconnect** (v1) | Smooth recovery from network flaps | It lets a stolen credential silently take over a worker (SEC-F3). Rejecting costs a legitimate worker at most 30 s. |
| **Native executor as the desktop user** (v1) | No setup | The job can read the worker credential and plant `beforeStart` hooks (SEC-F3). The dedicated user costs a one-time admin step. |
| **Router in-process local member on Unraid** (v1-style) | One container | It puts the PATs and the Docker socket in one container. The owner chose the split (decision 1). |
| **Desired from `TotalAcquiredJobs`** (SEC-F2) | Excludes un-acquired jobs | On github.com it reads 0 (FEAS §1). With no per-job deny the concern is gone. Spike (g) confirms. |
| **Capacity = total slots** (v1) | Simple | Over-assignment churns through the 3× requeue (FEAS §2). v2 advertises running + free. |
| **Transport: plain JSON, extending the control socket, gRPC bidi, or WebSocket** | Fewer dependencies, or reuse | The owner chose Connect. Extending the control socket would mix admin and worker auth (`server.go:294-322,312`). Bidi needs HTTP/2 end to end. WebSocket has no schema. |
| **Worker auth: a shared secret, or mTLS now** | Simpler, or stronger | A shared secret has no per-worker revocation and leaks globally. mTLS adds a CA lifecycle in two apps. Hardware-bound mTLS is the trigger-gated next step (§8.2). |
| **Docker executor through the docker CLI** | Covers every desktop variant | It would be a second implementation. The router-side image has no CLI. go-winio covers Windows. |
| **`self-hosted` on every class** (v1) | The GitHub convention | Bare `self-hosted` would match every class, including laptop-native ones (FEAS §3, F6). |

---

## 13. Control socket → Connect?

**Unchanged from v1: keep it as-is.**

Migrate only when one of these becomes true:
- the TUI needs pushed events;
- a remote dashboard needs the admin API;
- managing two TLS ports becomes a burden.

If triggered, add `gso.control.v1` on the same Connect server with an admin-token interceptor, keep the JSON handler for one release, then remove it.

**New constraint in v2:** worker-administration methods stay **unix-socket-only** even after a migration (SEC-F8).

---

## 14. Phased delivery (v2)

**Order:** P0 → P1 → P2a → P2b → **P3** → P2c → P4 → P5.

P3 needs only P2b, so the Unraid split ships before the breaking changes in P2c. Every phase can be deployed and rolled back on its own.

| Phase | Ships | Validated by | Rollback (honest) |
|---|---|---|---|
| **P0: Spikes** (no code on `main`) | **(a)** 409: does it clear within 10 × 30 s? Is a live 409 seen during a Portainer redeploy? **(b)** Routing across matching scale sets, including a capacity-0 sibling and a **sessionless** sibling (the §6.3 overlap). **(c)** `RemoveRunner` → JIT unusable (confirmation run only). **(d)** Windows `run.cmd` with a JIT config in the environment. **(e)** Capacity 0→N pickup: warm runner vs a floor of 1 (ARC#4715). **(f)** Where bare `self-hosted` lands with per-class sets. **(g)** v0.5 on github.com: statistics semantics, `JobAssigned` delivery, reassignment shape. **(h)** Runs API permission (Actions: Read/Write), `workflow_run` `head_repository`, JIT blob format (`gitHubUrl`), the exact wording of the fork-approval setting. **(i)** Wails v3 on Go 1.27.3. | Written findings | n/a |
| **P1: Foundations, no behaviour change** | Go 1.27.3; `internal/runner` → public `runner/`; runner pin and hash; configurable docker endpoint; **detach-vs-kill split** (no behaviour change for gso: it still kills on shutdown); reserve-before-mint; **multi-arch image and tagged release**; goreleaser windows; refuse file credentials looser than 0600; TLS 1.3 minimum on control TCP | CI; today's Unraid deployment | Previous image tag. There is no state. |
| **P2a: Library and resilience** | scaleset v0.5; `Scale` rewrite (stats cache, dedupe, reassignment, return-nil policy); listener supervisor and degraded state; typed 409 retry-without-delete; HTTP/2 health; `instance_id` naming and session owner with the **§6.3 migration**; `gso scaleset list --all`/`reset`. **Semaphore and single-executor paths unchanged.** | Runs on **Unraid as deployed today** for at least a week, including a forced container recreate | Previous image tag. The old binary recreates `gso-<host>-<repo>` on start; delete the P2a-named scale sets with `gso scaleset delete`. No other state. |
| **P2b: Protocol and fleet** (behind `workers.listen`) | proto/gen/buf CI; `internal/fleet`; `gso worker`; `gso workers *`; enrollment with bounds; single live session; binding checks; verified adoption; worker JIT verification; `runners.json` and attach; advertise address; SPKI-only verifier and key reuse; `Enroll` rate limit; redaction tests. **The local in-process member replaces the Semaphore**, with golden tests asserting the same scale decisions as P2a for the same inputs. Layout stays at **default class only**. | Fake-router and fake-GitHub tests; a Linux box running `gso worker` against a test router | **Config:** unset `workers.listen` → local member only. Behaviour matches P2a, **but the Semaphore code is gone; this is not P2a code.** **Code:** the P2a image tag, which ignores `workers.json`; enrolled workers then fail to connect and sit idle. |
| **P3: Unraid split** | The compose in §9.2: the router (no socket, `max_runners: 0`) plus the `gso-worker` container; optionally a Linux box. Default class only, so **no workflow changes**. | Prune CI green on the split for a week; kill and restart each container mid-job to show G7 | Redeploy the previous single-container compose (socket mounted, `max_runners: N`) on the P2b tag. Its local member serves as before. |
| **P2c: Classes and public policy** (`feat!`) | Per-class layout and lazy classes; the §6.2 label scheme (the default class gains `gso` via PATCH); capacity = running + free; warm runners per spike (e); two-layer public policy with docker-only scale sets for public repos; visibility check on `JobAssigned` and quarantine; opt-in fork-cancel; `runs-on` audit in the release notes | Spikes (b)/(e)/(f) outcomes; test repos | P2b image tag, then delete the non-default-class scale sets with `gso scaleset list --all`. The public refusal can be relaxed per repo with `allow_public`, but only on P2c. |
| **Cross-repo gate** before P4 | A **tagged gso release** containing: the multi-arch image published by digest (linux/amd64 and linux/arm64); a module tag containing `gen/`, `worker/` and `runner/`; the `buf breaking` baseline frozen at `gso.worker.v1`. Burst pins that tag and digest. | Release checklist | — |
| **P4: Burst, docker-only** | Forgejo removed; gso adapter; macOS `linux-arm64` and Windows `linux-x64` (go-winio); keychain credential; join-code UI with host and SPKI confirmation; allowlist; accept-public tri-state; re-attach after quit | Fake-router e2e; the M1 and a Windows PC running real jobs | **There is no functional rollback to Forgejo** (decision 1). The fallback is capacity: set Mode Off or uninstall, run `gso workers revoke`, and the Unraid and Linux workers carry the load. The last Forgejo build stays downloadable only while a Forgejo instance exists (D6). |
| **P5: Native** (macOS and Windows classes, opt-in) | Dedicated job user setup (§8.5); launcher; Windows zip runner; native requires `--repos` bounds; macOS and Windows classes; simulator spike | macOS and Windows test repos | Disable native in the app, then `gso workers update --classes` without the native classes. |
| **P6** (trigger-gated) | Control → Connect, placement priorities, hardware-bound mTLS, egress policy tooling | — | — |

---

## 15. Owner decisions still open, and unknowns

**Decisions**

| # | Decision | Status (2026-11-01) |
|---|---|---|
| D1 | **Label scheme:** `self-hosted` only on the default class, `[gso, OS, ARCH]` for the rest (§6.2). The alternative keeps `self-hosted` everywhere and accepts the bare-label hazard. | **Decided:** as specified. Spike (f) validates it. |
| D2 | **Wake-up mitigation:** warm runner on 0→N vs a permanent capacity floor of 1 | **Open:** decided after spike (e) |
| D3 | **Fork-cancel:** accept a PAT with Actions: Read and write for the opted-in public repos, and decide whether the default `fork_cancel_events` list cancels `issue_comment`-style runs | **Decided:** off by default. If enabled, the default event list applies. |
| D4 | **Native setup:** a one-time admin step on each Mac or PC (creates a job user, sudoers rule or stored password) | **Decided:** accepted. Native stays opt-in and ships in P5. |
| D5 | **Unraid worker size:** `--slots` for an underpowered host | **Decided:** 1–2 slots |
| D6 | **Forgejo leftovers:** what happens to `forgejo-ci-unraid/unraid/`, `burst/`, `.github/workflows/burst-runner.yml` and the last Forgejo Burst build | **Open:** owner call |
| D7 | **Burst app ID:** rename `dev.forgejo.burst-runner` (resets macOS autostart and permissions) | **Decided:** rename. Lands in P4 with the Burst rewrite (`desktop/build/darwin/Info.plist`). |

**Unknowns**

| # | Unknown | Resolution |
|---|---|---|
| U1 | How long a 409 lasts; whether 10 × 30 s is enough | Spike (a). Retry-without-delete is safe either way. |
| U2 | Routing across several matching scale sets, including sessionless and capacity-0 siblings | Spike (b) |
| U3 | The capacity 0→N stall and its fix | Spike (e) |
| U4 | v0.5 statistics semantics on github.com | Spike (g) |
| U5 | The JIT blob carries `gitHubUrl` and repo in a stable form | Spike (h). Worker verification fails closed if not. |
| U6 | Deleting a scale set deregisters its runners (F1) | Still INFERRED. The design no longer depends on it. |
| U7 | Xcode and simulators under a non-GUI job user | P5 spike |
| U8 | Wails v3 on Go 1.27.3 | Spike (i) |

**Resolved since v1**
- Q3: `RemoveRunner` invalidates a JIT (by code); never-connected ephemeral runners expire within ≤ 1 day.
- Q5: names ≤ 45 characters.
- Q6: polls held about 50 s, 2-minute cap.
- Q8: `max_runners` defaults stay; the compose sets 0.
- Q11: keychains are readable by the same user.

---

## 16. Consequences

**What gets easier**
- Adding a machine takes one admin command and one paste.
- The router holds no Docker socket, and Unraid's compute sits in a container without PATs.
- Jobs survive restarts of the router and of workers.
- The listener now recovers from a failed repo instead of staying dead.
- Bare `self-hosted` workflows need no changes.
- macOS and Windows jobs become possible (P5).
- Busy counts are accurate and drains have a deadline.

**What gets harder**
- The router has an inbound port and persistent state (four items) that need a volume and backups. Losing `workers/tls/key.pem` means re-enrolling every worker.
- gso's public packages and its Go version floor (1.27.3) couple two repos, gated by tagged releases.
- New dependencies: Connect, protobuf, buf, and later go-winio.
- There are repos × classes sessions and supervisors.
- Debugging spans GitHub, the router and a worker.
- Public repos need two opt-ins plus a GitHub repo setting; fork-cancel needs a broader PAT.
- Native needs per-machine admin setup.
- Desktop workers expose the LAN to job containers. This is accepted for private repos and is why desktops are not recommended for public repos.

**What we can no longer do cheaply**
- Make incompatible wire changes (that needs `v2`).
- Leave the per-class layout once workflows use `[gso, OS, ARCH]`.
- Roll Burst back to Forgejo.

**Revisit when**

| Trigger | Response |
|---|---|
| A worker leaves the intranet, or native runs repos the owner doesn't fully trust | Hardware-bound mTLS, plus a stricter `Enroll` |
| A second person administers workers | Per-worker repo bounds become mandatory for every worker |
| Router CPU becomes visible on Unraid (INFERRED threshold: >~20 workers or >~50 sessions) | Measure first |
| A job needs hardware only some machines have | Custom per-worker labels as part of the class |
| Untrusted code on macOS | VM isolation |
| The TUI or a dashboard needs push events | Control → Connect (§13) |
| scaleset releases a listener API newer than v0.5 | Re-check the §5.2 contract before upgrading |

---

**Follow-ups:**
- Run `plan-review` on v2, focused on §5.2/§7 against v0.5 and on the P2a/P2b/P2c rollbacks.
- Run `security` on §4.3, §8 and §9.2.
