# GitHub simulator

`gso sim` runs a stand-in for the parts of GitHub that gso uses, so a real
router and real workers can be exercised with no github.com, no PAT and no
workflows. It is for testing only.

## What it is

- `gso sim serve` is a separate process that plays GitHub: runner scale sets,
  one message session per scale set, message queues, JIT runner registration,
  repository visibility and workflow runs. It outlives router restarts, as
  GitHub does.
- The router uses it instead of github.com when started with
  `gso start --github-sim http://<sim>:9300` (or `github_sim:` in the config,
  or `GSO_GITHUB_SIM`). No token is needed.
- Jobs are injected by hand with `gso sim job`. A worker that receives one runs
  a **fake runner** that claims the job from the simulator, "works" for the
  requested duration and reports it done (see "Fake runners" below).

## Running it on one machine

```sh
gso sim serve --listen 127.0.0.1:9300 &
gso start --config router.yaml --github-sim http://127.0.0.1:9300 &
gso workers add laptop --classes linux-arm64 --slots 2 --advertise 127.0.0.1:9200
gso worker --join 'gso-join:v1:…' --executor sim --name laptop
gso sim job --sim http://127.0.0.1:9300 --repo o/a --duration 20s --count 3
gso sim jobs --sim http://127.0.0.1:9300 --watch
```

`router.yaml` needs `workers.listen`, `max_runners: 0` and a `repos:` list; the
repositories do not have to exist anywhere.

## Running it on Unraid

Start the compose stack's `sim` profile (see docs/unraid-portainer.md, "Dry run
with the simulator"). Publish port 9300 and pass
`--advertise http://<unraid LAN address>:9300`: job containers run on Docker's
default bridge, so they reach the simulator through the published port on the
LAN address, not through the compose service name.

## Injecting jobs

| Flag | Meaning |
|---|---|
| `--repo o/r` | Repository (required) |
| `--labels a,b` | `runs-on` labels; a scale set matches when its labels include all of them (default `self-hosted`) |
| `--duration 30s` | How long the fake runner works |
| `--count N` | Inject N identical jobs |
| `--public` | Mark the repository public (exercises the public-repo policy) |
| `--fork` | The run's head repository is a fork (exercises fork-cancel) |
| `--event e` | Workflow event, e.g. `pull_request_target` |
| `--result r` | Result the fake runner reports (default `succeeded`) |

`gso sim cancel --repo o/r --run <id>` cancels a run; a running fake runner
notices within a second and reports `canceled`.

## What is modelled and what is not

Modelled:
- one live session per scale set; a second session gets a 409 until the first
  is closed or has not polled for `--session-ttl` (this is what a router that
  was killed without closing its sessions sees on restart);
- messages are redelivered until acknowledged;
- no `JobAvailable` messages (github.com assigns jobs directly);
- an assignment no runner claims within `--claim-timeout` is cancelled and
  requeued, up to 3 times;
- runners are ephemeral: a finished runner is deregistered.

Not modelled:
- GitHub's capacity 0→N wake-up stall (ARC#4715). The simulator assigns as
  soon as capacity appears, so warm runners are **not** validated here.
- How GitHub chooses between several matching scale sets. The simulator picks
  the first by name with spare capacity; the real behaviour is spike (b).
- Rate limits, authentication, job logs, artifacts.

## Security

The simulator speaks plain HTTP without authentication and hands out runner
tokens to anyone who asks. Never expose its port beyond a test network.

## Worker trust settings

A simulator JIT config names `https://github.com/<owner>/<repo>` as its
`gitHubUrl`, but its `serverUrl` is the simulator's `--advertise` address. A
worker verifies a JIT before launching it, and `OfferCheck.TrustedHosts`
**replaces** the default list rather than extending it. To accept simulator JITs
the worker's `TrustedHosts` must list **both** `github.com` and the simulator
host (the host part of `--advertise`, for example `unraid.lan`). With only
`github.com` the worker rejects the JIT; with only the simulator host it rejects
real ones.

## Wire protocol

The simulator serves two APIs from one port:

- A plain JSON API under `/api/repos/{owner}/{name}`, `/runner/*` and `/admin/*`
  that `ghsim.NewRemoteFactory` and `gso sim job|jobs|cancel` use.
- GitHub's own wire protocol, enough for the unmodified `actions/scaleset`
  v0.5 client and listener: point a scaleset client's `GitHubConfigURL` at
  `http://<sim>:9300/<owner>/<repo>` (any host other than github.com is treated as
  GitHub Enterprise Server). The client's registration-token exchange, scale set,
  session (409 on a second one), message long poll (202 when idle),
  acknowledge, `acquirejobs`, JIT and runner calls all work, and
  `/api/v3/repos/{owner}/{name}[/actions/runs/{id}[/cancel]]` answer in the
  shapes `gso`'s REST calls read (`full_name`, `private`, `visibility`).

Repository names are matched ASCII case-insensitively, as on GitHub.

## Fake runners

A simulator JIT config looks like a real one (it passes the worker's checks:
host `github.com`, the offered repository, the local allowlist) but carries
the simulator's URL and a `gsoSim` marker. What runs it:

- **Docker workers** (the Unraid worker container, Burst, `gso worker
  --executor docker --image <gso image>`): the job image's
  `/usr/local/bin/gso-job` checks `gso sim is-sim-jit` first and runs
  `gso sim runner` instead of the actions runner. The container is created,
  attached to, re-attached after a worker restart and removed exactly as for a
  real job. The job image must be a gso image built from this branch, and the
  container must reach the simulator's `--advertise` URL. A docker worker needs
  the simulator host in its trusted hosts too: pass `--github-sim <url>`.
- **`gso worker --executor sim --github-sim <url>`**: runs `gso sim runner` as
  a local process in its own process group, so it survives a worker restart;
  the restarted worker re-attaches by PID. It needs no `--image`. Unix only;
  meant for tests and quick local runs.

`--github-sim` (or `GSO_GITHUB_SIM`) on `gso worker` puts both `github.com`
and the simulator's host into the worker's trusted hosts, as described under
"Worker trust settings". `gso start --github-sim <url>` points the router at
the same simulator.

`gso sim runner` claims a job, works for the job's `--duration`, checks for
cancellation about once a second, and reports the result. If its runner is
removed before a job arrives it exits 0, like an ephemeral runner that was
deregistered. Its HTTP timeout (one minute) deliberately exceeds the
simulator's 25 s (scaled) claim long-poll.

`GSO_TIMING_SCALE` (a float in (0, 1]) shortens every gso timer: heartbeats,
lost-worker detection, backoffs, the simulator's poll wait and session TTL.
It is for tests only. Never set it in production.
