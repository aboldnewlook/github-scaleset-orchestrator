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

Labels are unchanged: the configured labels plus the class's OS and arch
labels.

### Upgrade steps

1. Stop the old gso. Its listener holds the session for the old scale set.
2. In each repository, open Settings, Actions, Runners and delete the old
   `gso-<hostname>-<repo>` scale sets (the legacy scale sets). With identical
   labels and no session, GitHub may still route jobs to a stale scale set, so
   treat them as an outage risk until they are gone.
3. Start the new gso. It creates the new scale set.

If you keep several gso instances in the same repository, give each its own
`runner.instance_id`. After the upgrade gso logs a warning and publishes an
`alert` event with reason `stale_scale_sets` when it sees other `gso-` scale
sets in a repo; delete them only when no other gso instance owns them.

### 409 conflicts no longer delete the scale set

Previously, a 409 when opening the message session made gso delete and
recreate the scale set. gso no longer does that: runners placed on workers, or
adopted after a restart, belong to the scale set. A 409 is now retried every
30 seconds, up to 10 times, and then every 5 minutes, and the listener is
marked degraded when the retries run out. If you really need to recreate a
scale set, use `gso scaleset reset`.

### Listener supervision

Each repo's listener is supervised. After a failure it restarts with backoff
from 1 second to 5 minutes, and it is marked `degraded` after 3 consecutive
failures (a `listener.degraded` event, then `listener.recovered` when it is
back). In-flight runners keep their slots across a restart. `gso status` lists
each listener's state, failure count and last error.
