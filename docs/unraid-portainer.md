# Deploying gso on Unraid with Portainer

This guide walks through deploying gso on an Unraid server using Portainer for container management.

## Prerequisites

- Unraid 6.x or 7.x
- [Portainer](https://www.portainer.io/) installed (available in Unraid Community Applications)
- A GitHub Fine-Grained Personal Access Token with **Administration** (Read and write) permission

### Getting Portainer

If you don't have Portainer installed:

1. Open the Unraid web UI
2. Go to **Apps** (Community Applications)
3. Search for **Portainer** and install it
4. Once running, open Portainer (default: `http://your-unraid-ip:9000`)
5. Portainer Business Edition offers a [free license for up to 3 nodes](https://www.portainer.io/pricing) -- more than enough for a single Unraid server

## Step 1: Create a GitHub Token

1. Go to [GitHub Settings > Fine-grained tokens](https://github.com/settings/personal-access-tokens/new)
2. Set a descriptive name (e.g., `unraid-gso`)
3. Repository access: **All repositories** (or select specific ones)
4. Permissions:
   - **Administration**: Read and write
   - **Metadata**: Read-only (required, added automatically)
5. Click **Generate token** and save it

> **Note:** If you have repos across multiple GitHub accounts or orgs, create a separate token for each and use per-repo token overrides in `config.yaml`.

## Step 2: Create a Config File

Create a `config.yaml` for your repos. This can live in a git repo that Portainer pulls from, or be mounted from the Unraid filesystem.

```yaml
auth:
  token_env: GITHUB_TOKEN

max_runners: 4

labels:
  - self-hosted
  - unraid
  - linux
  - x64

repos:
  - name: youruser/repo-a
  - name: youruser/repo-b
  - name: youruser/repo-c
```

### Multiple GitHub accounts

If you have repos under different accounts or orgs, use per-repo token overrides:

```yaml
auth:
  token_env: GITHUB_TOKEN       # default token for most repos

repos:
  - name: personal/repo-a       # uses GITHUB_TOKEN
  - name: personal/repo-b       # uses GITHUB_TOKEN
  - name: myorg/repo-c          # uses a different token
    token:
      token_env: MYORG_TOKEN
```

Add both `GITHUB_TOKEN` and `MYORG_TOKEN` as environment variables in your Portainer stack.

## Step 3: Create a Docker Compose File

```yaml
version: "3.8"

services:
  gso:
    image: ghcr.io/aboldnewlook/github-scaleset-orchestrator:latest
    environment:
      - GITHUB_TOKEN=${GITHUB_TOKEN}
    volumes:
      - ./config.yaml:/etc/gso/config.yaml:ro
    restart: unless-stopped
```

## Step 4: Deploy the Stack in Portainer

### Option A: From a Git Repository

If your `docker-compose.yml` and `config.yaml` are in a git repo:

1. In Portainer, go to **Stacks** > **Add stack**
2. Name: `gso`
3. Build method: **Repository**
4. **Repository URL**: your repo URL
5. **Repository reference**: `refs/heads/main`
6. **Compose path**: `docker-compose.yml`
7. Enable **Authentication** if the repo is private
8. Under **Environment variables**, add:
   - `GITHUB_TOKEN` = your GitHub PAT
   - Any additional token env vars for multi-account setups
9. Click **Deploy the stack**

### Option B: Paste the Compose File

1. In Portainer, go to **Stacks** > **Add stack**
2. Name: `gso`
3. Build method: **Web editor**
4. Paste your `docker-compose.yml` content
5. Under **Environment variables**, add your token(s)
6. Click **Deploy the stack**

> **Note:** With Option B you'll need to manage `config.yaml` separately -- either bake it into the compose as a config, or mount it from a path on the Unraid filesystem (e.g., `/mnt/user/appdata/gso/config.yaml`).

## Step 5: Verify

1. In Portainer, click on the `gso` container
2. Click **Logs**
3. You should see gso creating scale sets and starting to long-poll for each repo

In your GitHub repos, go to **Settings** > **Actions** > **Runners** and you'll see scale sets registered.

## Updating

When a new version of gso is released:

1. In Portainer, go to your `gso` stack
2. Click **Pull and redeploy**

Or enable **Re-pull image** with a fetch interval to auto-update.

## Using Self-Hosted Runners in Workflows

In your GitHub Actions workflows, target your runners with matching labels:

```yaml
jobs:
  build:
    runs-on: [self-hosted, unraid]
    steps:
      - uses: actions/checkout@v4
      # ...
```

## Migrating from myoung34/github-runner

If you're currently running one `myoung34/github-runner` container per repo, gso replaces all of them with a single container. Key differences:

| | myoung34/github-runner | gso |
|---|---|---|
| Containers | One per repo | One total |
| Runners | Always running (idle) | Spawned on demand (JIT) |
| API approach | Registration token | Scale Set API (long-poll) |
| Runner lifecycle | Persistent | Ephemeral (one job, then deleted) |
| Adding repos | New container + config | Add a line to config.yaml |
| Concurrency | Per-container | Shared semaphore across all repos |
