package orchestrator

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/buildinfo"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/scaler"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/timing"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

// SupervisorTiming is the listener supervisor's retry policy (design spec
// §5.3 and the 409 row of §7).
type SupervisorTiming struct {
	BackoffMin, BackoffMax, ConflictWait time.Duration
	ConflictRetries, DegradeAfter        int

	// Stale scale-set cleanup: skipped sets are retried CleanupRetries times,
	// the first after CleanupBackoff and each later one after twice the
	// previous wait.
	CleanupBackoff time.Duration
	CleanupRetries int
}

// DefaultSupervisorTiming: back off 1 s → 5 min; on a 409 wait 30 s, up to
// 10 times (ARC 0.13.1 prior art); degraded after 3 consecutive failures.
// Durations pass through timing.Scale (GSO_TIMING_SCALE, tests and simulator).
func DefaultSupervisorTiming() SupervisorTiming {
	return SupervisorTiming{
		BackoffMin:      timing.Scale(time.Second),
		BackoffMax:      timing.Scale(5 * time.Minute),
		ConflictWait:    timing.Scale(30 * time.Second),
		ConflictRetries: 10,
		DegradeAfter:    3,
		// 1 + 2 + 4 + 8 + 16 minutes: skipped sets are retried for ~31 minutes.
		CleanupBackoff: timing.Scale(time.Minute),
		CleanupRetries: 5,
	}
}

const (
	stateRunning  = "running"
	stateRetrying = "retrying"
	stateDegraded = "degraded"
)

// superviseRepo keeps one repo's listener running until ctx is cancelled.
// Each attempt ensures the scale set, opens a session and runs a listener with
// the repo's long-lived scaler, so in-flight runners keep their slots across
// restarts. A 409 is retried every ConflictWait without ever deleting the
// scale set; other failures back off from BackoffMin to BackoffMax. The
// listener is degraded after DegradeAfter consecutive failures, or once the
// 409 retries run out, and keeps retrying at BackoffMax.
func (o *Orchestrator) superviseRepo(ctx context.Context, repo config.Repo) {
	name := naming.ScaleSetNameFor(o.instance, repo.Name, o.class)
	logger := o.logger.With("repo", repo.Name, "class", o.class.String(), "scale_set", name)
	o.updateStatus(repo.Name, func(st *control.ListenerLiveStatus) {
		st.Class, st.ScaleSetName, st.State = o.class.String(), name, stateRetrying
	})

	var (
		client    ghapi.Client
		sc        *scaler.Scaler
		failures  int // consecutive failures other than 409
		conflicts int // consecutive 409 session conflicts
		degraded  bool
		checked   bool // stale scale-set check done
		cleaning  bool // stale scale-set cleanup launched
		backoff   = o.timing.BackoffMin
	)
	var cleanup sync.WaitGroup
	defer cleanup.Wait()
	started := func(scaleSetID int) {
		// The current scale set exists now: clean up orphans, once per process.
		if !cleaning && o.cleanupEnabled() {
			cleaning = true
			cleanup.Go(func() { o.cleanupStaleScaleSets(ctx, repo, client, name, logger) })
		}
		failures, conflicts, backoff = 0, 0, o.timing.BackoffMin
		// Publish before the status flips to running, so anything that waits for
		// "running" also sees the recovered event.
		if degraded {
			degraded = false
			logger.Info("listener recovered")
			o.publishListener(event.EventListenerRecovered, repo.Name, name, map[string]any{})
		}
		o.updateStatus(repo.Name, func(st *control.ListenerLiveStatus) {
			st.ScaleSetID, st.State, st.Failures, st.LastError = scaleSetID, stateRunning, 0, ""
		})
	}

	for {
		err := func() error {
			if client == nil {
				c, err := o.clientFor(ctx, repo)
				if err != nil {
					return err
				}
				client = c
			}
			if !checked && o.cleanupEnabled() {
				checked = true // the cleanup reports what it leaves alone
			}
			if !checked {
				// Only a successful listing counts, so a transient error does not
				// suppress the migration warning for the life of the process.
				checked = o.warnStaleScaleSets(ctx, client, repo.Name, map[string]bool{name: true}, logger)
			}
			return o.runListenerOnce(ctx, repo, client, name, &sc, logger, started)
		}()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("listener exited without an error")
		}

		var wait time.Duration
		if ghapi.IsConflict(err) {
			// Another session still holds this scale set, e.g. the old container
			// during a redeploy. Never delete the scale set: runners placed on
			// workers or adopted after a restart belong to it (spec §7, F1).
			conflicts++
			failures, backoff = 0, o.timing.BackoffMin // a new kind of failure restarts the other count
			wait = o.timing.ConflictWait
			if conflicts > o.timing.ConflictRetries {
				wait = o.timing.BackoffMax
			}
			logger.Warn("message session conflict; retrying without deleting the scale set", "attempt", conflicts, "error", err)
		} else {
			failures++
			conflicts = 0
			wait = backoff
			backoff = min(backoff*2, o.timing.BackoffMax)
			logger.Error("listener stopped; retrying", "failures", failures, "retry_in", wait, "error", err)
		}

		degrade := conflicts > o.timing.ConflictRetries || failures >= o.timing.DegradeAfter
		o.updateStatus(repo.Name, func(st *control.ListenerLiveStatus) {
			st.Failures, st.LastError, st.State = failures+conflicts, err.Error(), stateRetrying
			if degrade || degraded {
				st.State = stateDegraded
			}
		})
		if degrade && !degraded {
			degraded = true
			o.publishListener(event.EventListenerDegraded, repo.Name, name, map[string]any{
				"failures": failures + conflicts, "error": err.Error(),
			})
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// runListenerOnce runs one listener lifetime. It returns when the listener
// stops; the error tells the supervisor how to retry.
func (o *Orchestrator) runListenerOnce(
	ctx context.Context, repo config.Repo, client ghapi.Client, name string,
	sc **scaler.Scaler, logger *slog.Logger, started func(scaleSetID int),
) error {
	// Fail closed: nothing is created or listened on until the repo's
	// visibility is known, because a public repo's jobs must only reach members
	// that accept public repos (spec §5.4). A lookup error is an ordinary
	// retryable failure.
	info, err := client.Repo(ctx)
	if err != nil {
		return fmt.Errorf("looking up repository visibility for %s: %w", repo.Name, err)
	}
	public := !info.Private && info.Visibility == "public"
	ss, err := o.ensureScaleSet(ctx, client, name, logger)
	if err != nil {
		return err
	}
	client.SetSystemInfo(scaleset.SystemInfo{
		System: "gso", Version: buildinfo.Version, Subsystem: "orchestrator", ScaleSetID: ss.ID,
	})
	// The scaler and its scale set ID exist before the message session opens:
	// after a router restart GitHub can keep the old session (409) for longer
	// than the reconnect grace, and a reconnecting worker's running runners are
	// adopted only against a known scale set (spec §4.3).
	if *sc == nil {
		*sc = o.newScaler(repo, o.class, client)
		o.registerScaler(repo.Name, *sc)
	}
	s := *sc
	s.SetPublic(public)
	s.SetScaleSetID(ss.ID)
	session, err := client.OpenSession(ctx, ss.ID, o.instance)
	if err != nil {
		return fmt.Errorf("opening message session for %s: %w", name, err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := session.Close(closeCtx); err != nil {
			logger.Debug("closing message session", "error", err)
		}
	}()
	s.BindSession(session)

	l, err := listener.New(session, listener.Config{ScaleSetID: ss.ID, MaxRunners: s.Capacity(), Logger: logger})
	if err != nil {
		return fmt.Errorf("creating listener: %w", err)
	}
	s.SetCapacitySink(l)
	defer s.SetCapacitySink(nil)

	logger.Info("listener started", "scale_set_id", ss.ID)
	o.publish(event.Event{
		Time: time.Now(), Type: event.EventScaleSetCreated, Repo: repo.Name,
		Payload: mustMarshal(map[string]any{"id": ss.ID, "name": ss.Name}),
	})
	started(ss.ID)
	// ctx is the supervisor's long-lived context. Runner goroutines started by
	// Scale derive from it, so they survive this listener's exit.
	return l.Run(ctx, s)
}

// ensureScaleSet returns the named scale set, creating it with today's labels
// when it does not exist. A lookup error is a failure, not a reason to create.
func (o *Orchestrator) ensureScaleSet(ctx context.Context, client ghapi.Client, name string, logger *slog.Logger) (*scaleset.RunnerScaleSet, error) {
	ss, err := client.GetRunnerScaleSet(ctx, 1, name)
	if err != nil {
		return nil, fmt.Errorf("looking up scale set %s: %w", name, err)
	}
	if ss != nil {
		return ss, nil
	}
	logger.Info("creating scale set")
	ss, err = client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
		Name:          name,
		RunnerGroupID: 1,
		Labels:        o.labels(),
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	})
	if err != nil {
		return nil, fmt.Errorf("creating scale set %s: %w", name, err)
	}
	return ss, nil
}

// labels are unchanged until P2c: the configured labels plus the class's OS
// and arch labels (design spec §6.3).
func (o *Orchestrator) labels() []scaleset.Label {
	labels := make([]scaleset.Label, 0, len(o.cfg.Labels)+2)
	for _, l := range o.cfg.Labels {
		labels = append(labels, scaleset.Label{Name: l})
	}
	return append(labels, scaleset.Label{Name: o.class.OSLabel()}, scaleset.Label{Name: o.class.ArchLabel()})
}

// warnStaleScaleSets flags gso-prefixed scale sets in the repo that are not
// current. With identical labels and no session, whether GitHub still routes
// jobs to them is UNKNOWN (spike b), so the rename treats them as an outage
// risk (design spec §6.3, docs/migrations.md).
// It reports whether the listing succeeded.
func (o *Orchestrator) warnStaleScaleSets(ctx context.Context, client ghapi.Client, repo string, current map[string]bool, logger *slog.Logger) bool {
	sets, err := client.ListRunnerScaleSets(ctx, 1)
	if err != nil {
		logger.Warn("listing scale sets for the stale check", "error", err)
		return false
	}
	var stale []string
	for _, ss := range sets {
		if strings.HasPrefix(ss.Name, naming.ScaleSetNamePrefix+"-") && !current[ss.Name] {
			stale = append(stale, fmt.Sprintf("%s (#%d)", ss.Name, ss.ID))
		}
	}
	if len(stale) == 0 {
		return true
	}
	why := "auto-cleanup is off (cleanup_stale_scale_sets: false / GSO_CLEANUP_STALE_SCALE_SETS=false)"
	if o.cfg.CleanupStaleScaleSets {
		why = "auto-cleanup is unavailable (no scale-set deleter, e.g. a simulated GitHub)"
	}
	logger.Warn("other gso scale sets exist in this repo and were not auto-deleted; delete them if no other gso instance owns them (docs/migrations.md): "+why, "stale", stale)
	o.publish(event.Event{
		Time: time.Now(), Type: event.EventAlert, Repo: repo,
		Payload: mustMarshal(map[string]any{"reason": "stale_scale_sets", "scale_sets": stale, "auto_cleanup": false}),
	})
	return true
}

func (o *Orchestrator) updateStatus(repo string, f func(*control.ListenerLiveStatus)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	st := o.status[repo]
	if st == nil {
		st = &control.ListenerLiveStatus{Repo: repo}
		o.status[repo] = st
	}
	f(st)
}

// Listeners reports every supervised listener, sorted by repo and class.
func (o *Orchestrator) Listeners() []control.ListenerLiveStatus {
	o.mu.Lock()
	out := make([]control.ListenerLiveStatus, 0, len(o.status))
	for _, st := range o.status {
		out = append(out, *st)
	}
	o.mu.Unlock()
	// The advertised capacity is the fleet's, not max_runners: with remote
	// workers it is the slots of every eligible member. Asked outside o.mu
	// because the fleet calls back into the orchestrator.
	for i := range out {
		if out[i].State != stateRunning {
			continue
		}
		if class, err := runner.ParseClass(out[i].Class); err == nil {
			if s := o.scalerFor(out[i].Repo, class); s != nil {
				out[i].Capacity = s.Capacity()
			}
		}
	}
	slices.SortFunc(out, func(a, b control.ListenerLiveStatus) int {
		return cmp.Or(cmp.Compare(a.Repo, b.Repo), cmp.Compare(a.Class, b.Class))
	})
	return out
}

func (o *Orchestrator) publishListener(typ event.EventType, repo, scaleSet string, payload map[string]any) {
	payload["class"] = o.class.String()
	payload["scale_set"] = scaleSet
	o.publish(event.Event{Time: time.Now(), Type: typ, Repo: repo, Payload: mustMarshal(payload)})
}
