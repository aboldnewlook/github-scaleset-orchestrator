# Deploying gso on Unraid with Portainer

gso on Unraid runs as **two containers**:

| Container | Holds | Does |
|---|---|---|
| `gso-router` | the GitHub token, its state volume | Owns the scale sets and their message sessions, decides where each job runs, and mints a single-use runner config only when a worker accepts the job. It has **no Docker socket** and runs no jobs (`max_runners: 0`). |
| `gso-worker` | the Docker socket, its own credential | An enrolled worker like any other machine. It runs each job in a sibling container on Unraid's Docker engine, with **2 slots**. It has **no token**. |

The two never share a container. A job that escapes its container lands where there is no token, and a compromised router has no host socket. Other machines (a Linux box, or a Mac/PC running the Burst app) enroll the same way as the Unraid worker.

## Prerequisites

- Unraid 6.x or 7.x
- [Portainer](https://www.portainer.io/) installed (available in Unraid Community Applications)
- A GitHub fine-grained personal access token (step 1)
- A name or address for Unraid that your other machines can reach, e.g. `unraid.lan` or `192.168.1.10`

### Getting Portainer

1. Open the Unraid web UI.
2. Go to **Apps** (Community Applications).
3. Search for **Portainer** and install it.
4. Open Portainer (default: `http://your-unraid-ip:9000`).

## Step 1: Create a GitHub token

1. Go to [GitHub Settings > Fine-grained tokens](https://github.com/settings/personal-access-tokens/new).
2. Name it, e.g. `unraid-gso`.
3. Repository access: the repositories gso should serve.
4. Permissions:
   - **Administration**: Read and write (manages runners and scale sets).
   - **Metadata**: Read-only (added automatically). This is what lets the token `GET /repos/{owner}/{repo}`.
5. Generate the token and save it.

The router reads each repository's visibility (`GET /repos/{owner}/{repo}`) **before** it creates that repository's scale set. If the token cannot read it, the router does not start that repository's listener, and jobs for it stay queued. Check the router logs if a repository never gets a scale set.

Only the router ever sees this token.

## Step 2: Create the router config

Create `config.yaml` next to the compose file (or at `/mnt/user/appdata/gso/config.yaml`):

```yaml
auth:
  env: GITHUB_TOKEN

labels:
  - self-hosted

runner:
  instance_id: unraid     # stable: scale sets are named gso-unraid-<repo>-linux-x64

repos:
  - name: youruser/repo-a
  - name: youruser/repo-b
```

The compose file also sets the router-only settings through environment variables: `max_runners: 0`, `runner.instance_id` (`GSO_INSTANCE_ID`, default `unraid`), and `workers.listen`, `workers.advertise` and `workers.allow_cidrs`. Environment variables win over the file. Keep the instance id explicit and stable: a container hostname is not stable, and changing the id creates new scale sets ([migrations.md](migrations.md) describes what that costs).

### Multiple GitHub accounts

```yaml
auth:
  env: GITHUB_TOKEN       # default token
repos:
  - name: personal/repo-a
  - name: myorg/repo-c
    token:
      env: MYORG_TOKEN    # add MYORG_TOKEN to the router's environment
```

### Public repositories

The router reads each repository's visibility before it creates the scale set (see step 1). A public repository's jobs go to a remote worker only if that worker opted in with `--accept-public accept` (the compose worker does not, so on this stack a public repository's jobs are not run by it), and the job cache is forced off for public jobs. The router's own local member currently accepts public repositories, but this stack's router has no local slots (`max_runners: 0`). There are no router-level `public.*` settings yet. Read [security.md](security.md#public-repositories) before opting any worker in.

## Step 3: The compose file

Use [`docker-compose.yaml`](../docker-compose.yaml) from this repository as-is. It defines:

- `gso-router`: `gso start --config /etc/gso/config.yaml`, port `9200` (the worker listener), and the `gso-router-state` volume;
- `gso-worker`: `gso worker --router gso-router:9200 --executor docker --slots 2`, the Docker socket, `GSO_WORKER_IMAGE` set to the same image as the router, and the `gso-worker-state` volume;
- `gso-sim`: an optional GitHub simulator under the `sim` profile (see [Dry run with the simulator](#dry-run-with-the-simulator)).

Stack variables:

| Variable | Required | Meaning |
|---|---|---|
| `GSO_TAG` | yes | gso release tag, e.g. `0.9.0`. Router, worker and job image share it. |
| `GITHUB_TOKEN` | yes | The token from step 1. |
| `GSO_WORKERS_ADVERTISE` | yes | `host:port` other machines dial, e.g. `unraid.lan:9200`. Join strings carry it. |
| `GSO_WORKERS_ALLOW_CIDRS` | no | Source networks allowed to reach port 9200. Default `10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10,127.0.0.0/8` (gso's own default). Narrow it to your LAN `/24` plus the compose network (`172.16.0.0/12`). |
| `GSO_WORKER_JOIN` | first start | The Unraid worker's join string (step 5). Ignored once the worker is enrolled. |
| `GSO_INSTANCE_ID` | no | Overrides the default instance id `unraid`. |

## Step 4: Deploy the stack in Portainer

### Option A: From a Git repository

This works only from **your own repository** that holds `docker-compose.yaml` **and your `config.yaml` next to it**. The upstream gso repository has no `config.yaml`, so deploying it directly would fail: the router's config mount sets `create_host_path: false`, so the stack refuses to start rather than creating an empty directory in place of the file. If you do not want a repository of your own, use Option B.

1. **Stacks** > **Add stack**, name it `gso`.
2. Build method **Repository**: your repository URL, reference `refs/heads/main`, compose path `docker-compose.yaml`.
3. Under **Environment variables**, set `GSO_TAG`, `GITHUB_TOKEN` and `GSO_WORKERS_ADVERTISE`. Leave `GSO_WORKER_JOIN` empty for now.
4. **Deploy the stack**.

### Option B: Paste the compose file

1. **Stacks** > **Add stack**, name `gso`, build method **Web editor**, and paste `docker-compose.yaml`.
2. Set the same environment variables.
3. Change `source: ./config.yaml` in the router's volumes to the file's absolute path, e.g. `source: /mnt/user/appdata/gso/config.yaml`. Create that file first (step 2): the mount does not create it.
4. **Deploy the stack**.

The router starts and creates its scale sets. Until step 5 the worker exits with "not enrolled" and restarts every few seconds. That is expected.

## Step 5: Enroll the Unraid worker

Worker administration (`gso workers add|update|list|drain|revoke`) works **only over the router's local unix control socket**, never over the network. On Unraid that means running it inside the router container. From the Unraid terminal use `docker exec`:

```bash
docker exec gso-router gso workers add unraid --classes linux-x64 --slots 2
```

In Portainer's console for the `gso-router` container (**Containers** > `gso-router` > **Console**, command `/bin/sh`) there is no `docker` CLI, so run the bare form:

```bash
gso workers add unraid --classes linux-x64 --slots 2
```

It prints a join string such as `gso-join:v1:unraid.lan:9200:<pin>:<code>`. The code is single-use and expires after 15 minutes.

1. In Portainer, edit the stack and set `GSO_WORKER_JOIN` to that string.
2. **Update the stack** (re-pull not needed).
3. The worker logs show the router address and a short key fingerprint, then `enrolled`. From then on it reconnects with its stored credential and ignores `GSO_WORKER_JOIN`. You may clear the variable.

`--slots 2` suits an underpowered host. Raise it with `docker exec gso-router gso workers update unraid --classes linux-x64 --slots 4`. The worker's own `--slots` (2 in the compose file) must be raised too to use the new bound.

## Step 6: Verify

```bash
docker exec gso-router gso workers list                                        # unraid: available, 0/2 busy, linux-x64 (bare: gso workers list)
docker exec gso-router gso scaleset list --all --config /etc/gso/config.yaml   # scale sets per repo
```

`gso status --remote` does not show the worker list (by design, because worker data is local-socket only); use `docker exec ... gso workers list`. Commands that read the config inside the container need `--config /etc/gso/config.yaml`: gso has no default config path.

In each repository, **Settings > Actions > Runners** shows the scale set `gso-unraid-<repo>-linux-x64`. Push a commit to a workflow with `runs-on: self-hosted` and watch the worker's logs.

## Adding another Linux machine

On Unraid:

```bash
docker exec gso-router gso workers add linuxbox --classes linux-x64 --slots 4
```

On the Linux machine (Docker installed; the join string's address must be reachable from it):

```bash
docker run -d --name gso-worker --restart unless-stopped --user 0:0 \
  -e GSO_WORKER_JOIN='gso-join:v1:…' \
  -e GSO_WORKER_IMAGE=ghcr.io/aboldnewlook/github-scaleset-orchestrator:<tag> \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v gso-worker-state:/home/runner/.config/gso \
  ghcr.io/aboldnewlook/github-scaleset-orchestrator:<tag> \
  worker --executor docker --slots 4
```

Without `--router` it dials the advertise address from the join string. Make sure the machine's address is inside `GSO_WORKERS_ALLOW_CIDRS`. Macs and Windows PCs use the Burst desktop app, which takes the same join string.

## State and backups

| Volume | Contents | If lost |
|---|---|---|
| `gso-router-state` | `tls/` (control TLS), **`workers/tls/key.pem`** (the key every worker pins), `workers.json` (enrolled workers, hashed credentials) | **Losing `workers/tls/key.pem` means re-enrolling every worker**: their pin no longer matches. Losing `workers.json` revokes every worker. Back up this volume. |
| `gso-worker-state` | the worker credential (0600), the router pin, `runners.json` | Re-enroll this worker (`gso workers revoke unraid`, then `add` again). |

Runner state survives restarts: jobs running when you update or restart either container keep running and are re-attached.

## Network

- Port `9200` is the only inbound port. It is TLS 1.3, authenticated per worker, and limited to `GSO_WORKERS_ALLOW_CIDRS`. The Unraid worker connects over the compose network (Docker's `172.16.0.0/12`), so keep that range in the allowlist.
- **Check that the allowlist really filters.** Docker's userland proxy can hide client addresses, which makes the allowlist useless. Read [security.md](security.md#docker-published-ports-and-the-allowlist) and run its check after deploying.
- Workers need outbound HTTPS to GitHub, the same domains as before (see [security.md](security.md#network-model)).
- **Job containers sit on Docker's default bridge with access to your LAN.** For private repositories you trust, that is fine. Read [security.md](security.md#remote-workers) before opting in any public repository.

## Dry run with the simulator

You can run the whole stack, including enrolled workers and real job containers, without touching GitHub. The router talks to a simulated GitHub instead. See [simulator.md](simulator.md) for what the simulator models and how to inject jobs.

Set these stack variables, then redeploy:

| Variable | Value | Why |
|---|---|---|
| `COMPOSE_PROFILES` | `sim` | Starts the `gso-sim` service (or `docker compose --profile sim up -d`). |
| `GSO_GITHUB_SIM` | `http://172.17.0.1:9300` | The router's simulator URL. `GITHUB_TOKEN` can stay empty. |
| `GSO_SIM_ADVERTISE` | `http://172.17.0.1:9300` | What job containers dial, and the host the worker trusts in job configs. The worker needs it as a flag (`--github-sim`); the compose file passes it. A docker-executor worker ignores `GSO_GITHUB_SIM` in its environment. **Clear it after the dry run**: while it stays set, the production worker keeps trusting that host in job configs. |

All three use the same address: the Docker bridge gateway (`172.17.0.1` on a default Unraid). Job containers, the router and the worker all reach the simulator there. The `gso-sim` service runs on the host network and listens **only** on that address (`GSO_SIM_HOST`, default `172.17.0.1`), never on `0.0.0.0`, because the simulator speaks plain HTTP with no authentication; your LAN cannot reach it. If your default bridge uses another gateway, set `GSO_SIM_HOST`, `GSO_GITHUB_SIM` and `GSO_SIM_ADVERTISE` to it together. Enroll the worker as in step 5 first, then inject jobs from the Unraid shell, for example `docker exec gso-sim gso sim job --sim http://172.17.0.1:9300 --repo youruser/repo-a --duration 20s`. When you are done, remove `COMPOSE_PROFILES`, `GSO_GITHUB_SIM` and `GSO_SIM_ADVERTISE` and redeploy.

## Upgrading from the single-container deployment

The scale-set names change when you upgrade (`gso-<hostname>-<repo>` becomes `gso-<instance>-<repo>-linux-x64`). Follow [migrations.md](migrations.md); in short:

1. Stop the old `gso` container. Nothing is in flight once it stops.
2. Deploy this stack. Once the router has created its own scale set it deletes the old `gso-<hostname>-<repo>` scale sets by itself, but only when they have no jobs and no live listener; anything it skips is retried for about 30 minutes and reported in the router log and a `stale_scale_sets` alert.
3. To opt out, set `cleanup_stale_scale_sets: false` in `config.yaml` or `GSO_CLEANUP_STALE_SCALE_SETS=false` on the router. To clean up by hand (a skipped set, or cleanup off), run `gso scaleset list --all --config <path>` and then `gso scaleset delete <id> --repo <repo>`, from any machine with the config and token, or from the router container. See [migrations.md](migrations.md#automatic-cleanup-of-old-scale-sets).

Queued jobs wait for the few minutes this takes.

## Rolling back

Redeploy the previous single-container compose on the previous tag, with the Docker socket mounted and `max_runners` greater than 0. Its built-in runner serves jobs as before. Enrolled workers fail to connect and sit idle; revoke them later. Keep the `gso-router-state` volume if you might return to the split: it holds the pinned key.

## Using self-hosted runners in workflows

Nothing changes for existing workflows:

```yaml
jobs:
  build:
    runs-on: self-hosted          # lands on the default class (linux-x64), as before
```

## Migrating from myoung34/github-runner

| | myoung34/github-runner | gso |
|---|---|---|
| Containers | One per repository | Two in total: router and worker |
| Runners | Always running (idle) | Created per job (JIT, ephemeral) |
| API | Registration token | Scale Set API (long-poll) |
| Adding repositories | A new container | One line in `config.yaml` |
| Adding machines | A new container per repository | `gso workers add` plus one join string |
