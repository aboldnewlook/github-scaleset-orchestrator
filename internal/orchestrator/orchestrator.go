package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/runner"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/scaler"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/service"
	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

const scaleSetNamePrefix = "gso"

// Orchestrator manages scale sets and listeners for multiple repos.
type Orchestrator struct {
	cfg    *config.Config
	logger *slog.Logger
	bus    *event.Bus // may be nil
	sem    scaler.Semaphore

	mu      sync.Mutex
	scalers map[string]*scaler.Scaler // keyed by repo name
}

func New(cfg *config.Config, logger *slog.Logger, bus *event.Bus) *Orchestrator {
	return &Orchestrator{
		cfg:     cfg,
		logger:  logger,
		bus:     bus,
		scalers: make(map[string]*scaler.Scaler),
	}
}

// RunnersByRepo returns a snapshot of active runner names per repo.
func (o *Orchestrator) RunnersByRepo() map[string][]string {
	o.mu.Lock()
	defer o.mu.Unlock()

	result := make(map[string][]string, len(o.scalers))
	for repo, s := range o.scalers {
		result[repo] = s.Runners()
	}
	return result
}

// Semaphore returns the orchestrator's semaphore for capacity queries.
func (o *Orchestrator) Semaphore() scaler.Semaphore {
	return o.sem
}

// MaxRunners returns the configured maximum concurrent runners.
func (o *Orchestrator) MaxRunners() int {
	return o.cfg.MaxRunners
}

// AvailableSlots returns the number of free runner slots.
func (o *Orchestrator) AvailableSlots() int {
	if o.sem == nil {
		return o.cfg.MaxRunners
	}
	return o.sem.Available()
}

// SetMaxRunners updates the concurrency cap at runtime.
// Note: this does not persist across restarts.
func (o *Orchestrator) SetMaxRunners(count int) error {
	// We cannot resize a channel, so this is a best-effort approach.
	// For now, update the config value used by new listeners.
	o.cfg.MaxRunners = count
	return nil
}

// CancelRunner finds a runner by name across all repos and cancels it.
func (o *Orchestrator) CancelRunner(name string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()

	for _, s := range o.scalers {
		if s.CancelRunner(name) {
			return true
		}
	}
	return false
}

// Scalers returns the scaler map (for the Runtime service).
func (o *Orchestrator) Scalers() map[string]*scaler.Scaler {
	o.mu.Lock()
	defer o.mu.Unlock()

	// Return a shallow copy to avoid races.
	cp := make(map[string]*scaler.Scaler, len(o.scalers))
	for k, v := range o.scalers {
		cp[k] = v
	}
	return cp
}

// Run starts listeners for all configured repos and blocks until ctx is cancelled.
func (o *Orchestrator) Run(ctx context.Context) error {
	o.publish(event.Event{
		Time: time.Now(),
		Type: event.EventDaemonStarted,
	})

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}

	// Prepare runner binary
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = "/tmp"
	}
	cacheDir = cacheDir + "/gso"

	mgr, err := runner.NewManager(cacheDir, o.logger)
	if err != nil {
		return fmt.Errorf("creating runner manager: %w", err)
	}

	runnerDir, err := mgr.RunnerDir(ctx)
	if err != nil {
		return fmt.Errorf("preparing runner binary: %w", err)
	}
	o.logger.Info("runner binary ready", "dir", runnerDir)

	o.sem = scaler.NewSemaphore(o.cfg.MaxRunners)
	worker := runner.NewWorker(runnerDir, o.logger)

	var wg sync.WaitGroup
	errCh := make(chan error, len(o.cfg.Repos))

	for _, repo := range o.cfg.Repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := o.runRepo(ctx, repo, hostname, worker, o.sem); err != nil {
				o.logger.Error("repo listener failed", "repo", repo.Name, "error", err)
				errCh <- fmt.Errorf("%s: %w", repo.Name, err)
			}
		}()
	}

	// Wait for context cancellation in a separate goroutine so we can
	// emit the stopping event.
	go func() {
		<-ctx.Done()
		o.publish(event.Event{
			Time: time.Now(),
			Type: event.EventDaemonStopping,
		})
	}()

	wg.Wait()
	close(errCh)

	// Return first error if any
	for err := range errCh {
		return err
	}
	return nil
}

func (o *Orchestrator) runRepo(ctx context.Context, repo config.Repo, hostname string, worker *runner.Worker, sem scaler.Semaphore) error {
	logger := o.logger.With("repo", repo.Name)
	logger.Info("starting listener")

	client, err := service.NewScaleSetClient(ctx, o.cfg, repo, logger)
	if err != nil {
		return fmt.Errorf("creating client: %w", err)
	}

	// Build labels
	labels := make([]scaleset.Label, 0, len(o.cfg.Labels)+2)
	for _, l := range o.cfg.Labels {
		labels = append(labels, scaleset.Label{Name: l})
	}
	labels = append(labels, scaleset.Label{Name: osLabel()})
	labels = append(labels, scaleset.Label{Name: archLabel()})

	// Create or get scale set
	scaleSetName := fmt.Sprintf("%s-%s-%s", scaleSetNamePrefix, hostname, repoShortName(repo.Name))

	ss, err := client.GetRunnerScaleSet(ctx, 1, scaleSetName)
	if err != nil || ss == nil {
		logger.Info("creating new scale set", "name", scaleSetName)
		ss, err = client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
			Name:          scaleSetName,
			RunnerGroupID: 1,
			Labels:        labels,
			RunnerSetting: scaleset.RunnerSetting{
				DisableUpdate: true,
			},
		})
		if err != nil {
			return fmt.Errorf("creating scale set: %w", err)
		}
	}

	logger.Info("scale set ready", "id", ss.ID, "name", ss.Name)

	o.publish(event.Event{
		Time: time.Now(),
		Type: event.EventScaleSetCreated,
		Repo: repo.Name,
		Payload: mustMarshal(map[string]any{
			"id":   ss.ID,
			"name": ss.Name,
		}),
	})

	sysInfo := scaleset.SystemInfo{
		System:     "gso",
		Version:    "0.1.0",
		Subsystem:  "orchestrator",
		ScaleSetID: ss.ID,
	}
	client.SetSystemInfo(sysInfo)

	sessionClient, err := client.MessageSessionClient(ctx, ss.ID, hostname)
	if err != nil {
		// Handle stale session from a previous crash — delete and recreate the scale set
		if strings.Contains(err.Error(), "409") || strings.Contains(err.Error(), "Conflict") {
			logger.Warn("stale session detected, recreating scale set")
			if delErr := client.DeleteRunnerScaleSet(ctx, ss.ID); delErr != nil {
				return fmt.Errorf("deleting stale scale set: %w", delErr)
			}
			ss, err = client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
				Name:          scaleSetName,
				RunnerGroupID: 1,
				Labels:        labels,
				RunnerSetting: scaleset.RunnerSetting{
					DisableUpdate: true,
				},
			})
			if err != nil {
				return fmt.Errorf("recreating scale set: %w", err)
			}
			logger.Info("scale set recreated", "id", ss.ID, "name", ss.Name)
			sysInfo.ScaleSetID = ss.ID
			client.SetSystemInfo(sysInfo)

			sessionClient, err = client.MessageSessionClient(ctx, ss.ID, hostname)
			if err != nil {
				return fmt.Errorf("creating session client after recreation: %w", err)
			}
		} else {
			return fmt.Errorf("creating session client: %w", err)
		}
	}
	defer sessionClient.Close(ctx)

	s := scaler.New(repo.Name, ss.ID, client, worker, sem, logger, o.bus)

	// Store the scaler in the map
	o.mu.Lock()
	o.scalers[repo.Name] = s
	o.mu.Unlock()

	l, err := listener.New(sessionClient, listener.Config{
		ScaleSetID: ss.ID,
		MaxRunners: o.cfg.MaxRunners,
		Logger:     logger,
	})
	if err != nil {
		return fmt.Errorf("creating listener: %w", err)
	}

	return l.Run(ctx, s)
}

// publish sends an event to the bus if configured.
func (o *Orchestrator) publish(e event.Event) {
	if o.bus != nil {
		o.bus.Publish(e)
	}
}

func mustMarshal(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

func repoShortName(repo string) string {
	parts := strings.Split(repo, "/")
	if len(parts) == 2 {
		return parts[1]
	}
	return repo
}

func osLabel() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	default:
		return runtime.GOOS
	}
}

func archLabel() string {
	switch runtime.GOARCH {
	case "amd64":
		return "X64"
	case "arm64":
		return "ARM64"
	default:
		return runtime.GOARCH
	}
}
