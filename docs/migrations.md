# Migrations

## Scale-set naming by instance (breaking)

gso used to name each repo's scale set `gso-<hostname>-<repo>` and to open its
message session as the hostname. Scale sets are now named
`gso-<instance>-<repo>-<os>-<arch>` (for example
`gso-unraid-myrepo-linux-x64`), where `<instance>` is `runner.instance_id`
when set and the sanitised hostname otherwise. The session owner is the same
instance ID. Names use `[a-z0-9-]` and stay within 45 characters; long names
are truncated with a short hash (see the naming rules in the design spec).

The class (`<os>-<arch>`) is `workers.default_class` when set, otherwise the
host's own class. A router on a Mac that serves Docker or simulator workers
should set `workers.default_class: linux-x64`.

Labels are the configured labels plus the class's OS and arch labels. They
match the old labels only when `workers.default_class` is unset, because the
labels now follow the class rather than the host.

Set an explicit `runner.instance_id` for containers that do not have a fixed
hostname (for example subprocess-mode containers); otherwise every restart
could pick a different name.

### Upgrade steps

1. Wait for running jobs to finish, then stop the old gso. Its listener holds
   the session for the old scale set.
2. Start the new gso. It creates the new scale set and then **cleans up the
   old `gso-<hostname>-<repo>` scale sets automatically** (see below). There is
   nothing to delete by hand.

If you keep several gso instances in the same repository, give each its own
`runner.instance_id`. After the upgrade gso logs a warning and publishes an
`alert` event with reason `stale_scale_sets` for any other `gso-` scale sets it
sees in a repo and did not delete.

### Automatic cleanup of old scale sets

With the old release's scale sets lingering, GitHub can still route jobs to
them while nothing serves them, so treat them as an outage risk until they are
gone. Once the router has ensured its own scale set for a repo, it deletes an
old scale set only when **all** of these hold:

- it is in a configured repo, its name starts with `gso-`, and it is not in
  the current naming form `gso-<instance>-<repo>-<os>-<arch>`. A current-form
  set of any instance, and any set not named `gso-...`, is never touched;
- GitHub reports statistics for it and they show no assigned, acquired or
  running jobs and no busy runners (missing statistics, or an error, means
  skip);
- the router can open a message session on it. A 409 means a live listener
  owns it, so it is kept. The router closes the session before deleting.

`KIND` cannot prove ownership: the old release ran in a container whose
hostname was a random container ID, so its sets show as `other` in
`gso scaleset list --all`, not `legacy`. That is why the rules above, not the
kind, decide.

Each deletion is logged at INFO (`deleted stale scale set`, with name, id and
repo) and published as a `scaleset.deleted` event. Sets it had to skip are
retried a few times with backoff (1, 2, 4, 8 and 16 minutes, so about 30
minutes after start), then left alone. The `stale_scale_sets` warning and
alert list each remaining set as `name (#id)` with the reason it was skipped
(`skipped` in the alert payload).

**Opt out.** Set `cleanup_stale_scale_sets: false` in the config file, or
`GSO_CLEANUP_STALE_SCALE_SETS=false`. The router then deletes nothing and
keeps only the warning and alert (which says auto-cleanup is off). The default
is `true`.

**Clean up manually.** For a set the router skipped, or with cleanup off, list
every gso scale set per repo and delete each stale one by id:

```sh
gso scaleset list --all --repo owner/name
gso scaleset delete <id> --repo owner/name
```

`KIND` is `current` (a name the new release uses), `legacy` (this host's old
name) or `other` (another gso instance's, or an orphan from an earlier
container hostname). Delete `legacy` and `other` scale sets only when no other
running gso instance owns them. A plain `gso scaleset list` shows only the
current name and cannot see orphans.

Auto-cleanup needs the real GitHub API; with a simulated GitHub
(`--github-sim`) it is unavailable and sets are only reported.

### 409 conflicts no longer delete the scale set

Previously, a 409 when opening the message session made gso delete and
recreate the scale set. gso no longer does that: runners placed on workers, or
adopted after a restart, belong to the scale set. A 409 is now retried every
30 seconds, up to 10 times, and then every 5 minutes, and the listener is
marked degraded when the retries run out. If you really need to recreate a
scale set after a 409, run:

```sh
gso scaleset reset <owner/repo> <class>   # e.g. linux-x64
```

This deletes this instance's scale set for that repo and class. The running
gso recreates it on its next listener retry. `reset` is the only command that
deletes a scale set for you, and it refuses rather than risk a live one:

- when this machine's gso reports the listener `running` (a `degraded` or
  `retrying` listener is the case it is for);
- when another gso, possibly on another host, holds the scale set's message
  session (stop it, or wait for the session to expire);
- when the scale set has busy runners or assigned, acquired or running jobs,
  or when GitHub returns no statistics for it;
- when the liveness probe fails for any reason other than a held session
  (unknown is treated as live);
- for any class other than the default class (`workers.default_class`, else
  the host's class): only that class is served until multi-class support, so
  nothing would recreate the scale set.

The liveness probe (a brief message session) is skipped only when the local
gso reports the listener for that exact scale set as `degraded` or
`retrying`. `--force` only skips the confirmation prompt; it does not
override a refusal.

Verify in the live test: idle runners registered to the scale set are
removed with it, and the real service returns statistics on both the lookup
and the probe session.

### Listener supervision

Each repo's listener is supervised. After a failure it restarts with backoff
from 1 second to 5 minutes, and it is marked `degraded` after 3 consecutive
failures (a `listener.degraded` event, then `listener.recovered` when it is
back). In-flight runners keep their slots across a restart. `gso status` lists
each listener's state, failure count and last error.

### Rollback

1. Stop the new release.
2. Run `gso scaleset list --all` and delete the `current` scale sets with
   `gso scaleset delete <id> --repo <repo>`. (`gso scaleset reset` refuses
   while a gso is live, so stop it first.)
3. Start the previous release. It recreates `gso-<hostname>-<repo>` on start.
   There is no other state to undo.
