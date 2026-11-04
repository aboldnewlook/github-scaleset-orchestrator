package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	"github.com/actions/scaleset"
)

// Why a stale scale set is left alone. Only the first is final; the rest are
// retried because the situation can change (a listener exits, jobs finish).
const reasonNotPreInstance = "is not a pre-instance name (gso-<hostname>-<repo>), so it may belong to another tool; never auto-deleted"

const reasonCurrentForm = "has a current-form name (an instance-era set, possibly another instance's); never auto-deleted"

// cleanupEnabled reports whether this router may delete orphaned scale sets:
// the config allows it and a deleter is available.
func (o *Orchestrator) cleanupEnabled() bool {
	return o.cfg.CleanupStaleScaleSets && o.deleter != nil
}

// noCleanupReason says why auto-cleanup will not run, for the plain warning.
func (o *Orchestrator) noCleanupReason() string {
	if o.cfg.CleanupStaleScaleSets {
		return "auto-cleanup is unavailable (no scale-set deleter, e.g. a simulated GitHub)"
	}
	return "auto-cleanup is off (cleanup_stale_scale_sets: false / GSO_CLEANUP_STALE_SCALE_SETS=false)"
}

// cleanupStaleScaleSets removes orphaned pre-instance-era scale sets
// (gso-<hostname>-<repo>) from one repo, once, right after the current scale
// set exists, and retries the ones it had to skip with doubling backoff for
// CleanupRetries rounds (about 31 minutes with the defaults). It does not loop
// forever. It runs on the repo's supervisor context.
//
// A set is deleted only when ALL hold:
//
//   - it is gso-prefixed, in this configured repo, not this router's own
//     scale set, and its name is NOT instance-era (no "-<os>-<arch>" suffix),
//     so it is never a current-form set of any instance, and it has the
//     pre-instance shape gso-<hostname>-<repo short name> (it ends in
//     "-<repo short name>"), so another tool's gso-* set is left alone;
//   - GitHub's statistics are present and show no assigned, acquired or
//     running jobs and no busy runners, both in the listing and again in the
//     message session below (missing statistics or an error means skip);
//   - the router can open a message session on it (a 409 means a live
//     listener owns it: skip). The session is held across the
//     delete (so nothing can open the set in between) and closed after it.
//
// KIND alone cannot prove ownership: the single-container release used a
// random container ID as hostname, so its sets look like another instance's.
func (o *Orchestrator) cleanupStaleScaleSets(ctx context.Context, repo config.Repo, client ghapi.Client, current string, logger *slog.Logger) {
	wait := o.timing.CleanupBackoff
	reported := false
	for attempt := 0; ; attempt++ {
		skipped, retryable, listed := o.cleanupPass(ctx, repo, client, current, logger)
		if ctx.Err() != nil {
			return
		}
		if listed && !reported {
			reported = true
			o.reportStale(repo.Name, skipped, logger)
		}
		if listed && !retryable {
			return
		}
		if attempt >= o.timing.CleanupRetries {
			if len(skipped) > 0 {
				logger.Warn("stale scale sets remain after the cleanup retries; delete them by hand if no other gso instance owns them (docs/migrations.md)",
					"skipped", skippedList(skipped))
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait *= 2
	}
}

// cleanupPass lists the repo's scale sets once and deletes every one it can
// prove orphaned. skipped maps "name (#id)" to why the set was left alone;
// retryable says a later pass could change that; listed is false when the
// listing itself failed.
func (o *Orchestrator) cleanupPass(ctx context.Context, repo config.Repo, client ghapi.Client, current string, logger *slog.Logger) (skipped map[string]string, retryable, listed bool) {
	sets, err := client.ListRunnerScaleSets(ctx, 1)
	if err != nil {
		logger.Warn("listing scale sets for the stale cleanup", "error", err)
		return nil, true, false
	}
	skipped = make(map[string]string)
	for _, ss := range sets {
		if !strings.HasPrefix(ss.Name, naming.ScaleSetNamePrefix+"-") || ss.Name == current {
			continue
		}
		label := fmt.Sprintf("%s (#%d)", ss.Name, ss.ID)
		if naming.HasClassSuffix(ss.Name) {
			skipped[label] = reasonCurrentForm
			continue
		}
		// Every pre-instance release named its sets gso-<hostname>-<repo short name>.
		if !strings.HasSuffix(ss.Name, "-"+naming.RepoShortName(repo.Name)) {
			skipped[label] = reasonNotPreInstance
			continue
		}
		reason := o.deleteIfOrphaned(ctx, repo, client, ss, logger)
		if reason != "" {
			skipped[label] = reason
			retryable = true
		}
	}
	return skipped, retryable, true
}

// deleteIfOrphaned deletes ss when it can be proven unused. It returns "" when
// the set was deleted, else why it was kept.
func (o *Orchestrator) deleteIfOrphaned(ctx context.Context, repo config.Repo, client ghapi.Client, ss scaleset.RunnerScaleSet, logger *slog.Logger) string {
	if reason := busyReason(ss.Statistics); reason != "" {
		return reason
	}
	session, err := client.OpenSession(ctx, ss.ID, "gso-cleanup-"+o.instance)
	if err != nil {
		if ghapi.IsConflict(err) {
			return "a live listener holds its message session (409); not touched"
		}
		return fmt.Sprintf("could not open a message session to check for a live listener: %v", err)
	}
	// Hold the session across the delete, so nothing can open the set in
	// between, and close it afterwards even if ctx is cancelled. GitHub accepts
	// a delete while a session is open, and the set takes the session with it,
	// so a Close error after a successful delete is only logged.
	closeSession := func() error {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return session.Close(closeCtx)
	}
	// The session's statistics are the last read before the delete.
	if fresh := busyReason(session.Session().Statistics); fresh != "" {
		if err := closeSession(); err != nil {
			logger.Warn("closing the check session", "name", ss.Name, "id", ss.ID, "error", err)
		}
		return fresh
	}
	del, err := o.deleter(ctx, repo)
	if err == nil {
		err = del.DeleteRunnerScaleSet(ctx, ss.ID)
	}
	closeErr := closeSession()
	if err != nil {
		if closeErr != nil {
			logger.Warn("closing the check session", "name", ss.Name, "id", ss.ID, "error", closeErr)
		}
		return fmt.Sprintf("delete failed: %v", err)
	}
	if closeErr != nil {
		logger.Warn("closing the check session after the delete", "name", ss.Name, "id", ss.ID, "error", closeErr)
	}
	logger.Info("deleted stale scale set", "name", ss.Name, "id", ss.ID, "repo", repo.Name)
	o.publish(event.Event{
		Time: time.Now(), Type: event.EventScaleSetDeleted, Repo: repo.Name,
		Payload: mustMarshal(map[string]any{"id": ss.ID, "name": ss.Name, "reason": "stale_cleanup"}),
	})
	return ""
}

// busyReason is "" only when statistics are present and show no work. Missing
// statistics are treated as busy: fail closed.
func busyReason(st *scaleset.RunnerScaleSetStatistic) string {
	if st == nil {
		return "GitHub returned no statistics, so it cannot be shown idle"
	}
	if st.TotalAssignedJobs > 0 || st.TotalAcquiredJobs > 0 || st.TotalRunningJobs > 0 || st.TotalBusyRunners > 0 {
		return fmt.Sprintf("has jobs in flight (%d assigned, %d acquired, %d running, %d busy runners)",
			st.TotalAssignedJobs, st.TotalAcquiredJobs, st.TotalRunningJobs, st.TotalBusyRunners)
	}
	return ""
}

func skippedList(skipped map[string]string) []string {
	out := make([]string, 0, len(skipped))
	for _, label := range slices.Sorted(maps.Keys(skipped)) {
		out = append(out, label+": "+skipped[label])
	}
	return out
}

// reportStale warns, and publishes the stale_scale_sets alert, for the sets
// the first cleanup pass left alone, with the reason for each.
func (o *Orchestrator) reportStale(repo string, skipped map[string]string, logger *slog.Logger) {
	if len(skipped) == 0 {
		return
	}
	logger.Warn("other gso scale sets exist in this repo and were not auto-deleted; skipped sets are retried for a while, then delete them by hand if no other gso instance owns them (docs/migrations.md)",
		"skipped", skippedList(skipped))
	o.publish(event.Event{
		Time: time.Now(), Type: event.EventAlert, Repo: repo,
		Payload: mustMarshal(map[string]any{
			"reason":       "stale_scale_sets",
			"scale_sets":   slices.Sorted(maps.Keys(skipped)),
			"auto_cleanup": true,
			"skipped":      skipped,
		}),
	})
}
