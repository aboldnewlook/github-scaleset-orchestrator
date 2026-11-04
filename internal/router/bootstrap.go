package router

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/fleet"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
)

// Defaults for the bootstrap loop; tests shorten them on the Router.
//
// An enrollment code lives 15 minutes (fleet's enrollmentTTL). The loop
// re-mints once less than the margin is left, so a worker that starts late
// still finds a code with plenty of time to redeem it.
const (
	defaultBootstrapPoll   = 10 * time.Second
	defaultBootstrapMargin = 2 * time.Minute
)

// bootstrapPlan is the validated workers.bootstrap config.
type bootstrapPlan struct {
	params   control.WorkersAddParams
	joinFile string
}

// newBootstrapPlan returns nil when bootstrap is off (name and join_file not
// both set). Otherwise it validates the config the way `gso workers add`
// does, so a bad config stops the router at startup rather than minting
// nothing forever.
func (r *Router) newBootstrapPlan() (*bootstrapPlan, error) {
	bc := r.cfg.Workers.Bootstrap
	if !bc.Enabled() {
		if bc.Name != "" || bc.JoinFile != "" {
			r.logger.Warn("workers.bootstrap needs both name and join_file; bootstrap is off", "name", bc.Name, "join_file", bc.JoinFile)
		}
		return nil, nil
	}
	if err := fleet.ValidWorkerName(bc.Name); err != nil {
		return nil, fmt.Errorf("workers.bootstrap.name %q: %w", bc.Name, err)
	}
	if _, err := r.parseBounds(bc.Classes, bc.Slots, nil); err != nil {
		return nil, fmt.Errorf("workers.bootstrap: %w", err)
	}
	advertise := strings.TrimSpace(r.cfg.Workers.Advertise)
	if advertise == "" {
		return nil, errors.New("workers.bootstrap: no advertise address: set workers.advertise (GSO_WORKERS_ADVERTISE)")
	}
	if err := worker.ValidJoinAddr(advertise); err != nil {
		return nil, fmt.Errorf("workers.bootstrap: advertise address %q: %w", advertise, err)
	}
	return &bootstrapPlan{
		params:   control.WorkersAddParams{Name: bc.Name, Classes: bc.Classes, Slots: bc.Slots},
		joinFile: bc.JoinFile,
	}, nil
}

type bootstrapState int

const (
	bootstrapPending bootstrapState = iota
	bootstrapEnrolled
	bootstrapRevoked
)

// bootstrapStateOf classifies the named worker. A live record wins over an old
// revoked one: an admin may re-enroll a revoked name with `gso workers add`.
func (r *Router) bootstrapStateOf(name string) bootstrapState {
	if _, ok := r.registry.ByName(name); ok {
		return bootstrapEnrolled
	}
	for _, rec := range r.registry.List() {
		if rec.Name == name && rec.Revoked {
			return bootstrapRevoked
		}
	}
	return bootstrapPending
}

// runBootstrap keeps a fresh join string in the plan's file until the named
// worker is enrolled, then removes it. It ends (closing r.bootstrapDone) when
// the worker is enrolled, when the name is revoked (revocation sticks: it is
// never re-enrolled automatically) or when ctx ends. Nothing here logs the
// join string or the code.
func (r *Router) runBootstrap(ctx context.Context, p *bootstrapPlan) {
	defer close(r.bootstrapDone)
	poll, margin := r.bootstrapPoll, r.bootstrapMargin
	if poll <= 0 {
		poll = defaultBootstrapPoll
	}
	if margin <= 0 {
		margin = defaultBootstrapMargin
	}
	name := p.params.Name
	var expires time.Time
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		switch r.bootstrapStateOf(name) {
		case bootstrapEnrolled:
			if err := os.Remove(p.joinFile); err != nil && !errors.Is(err, os.ErrNotExist) {
				r.logger.Warn("bootstrap: could not remove the join file", "name", name, "file", p.joinFile, "error", err)
			}
			r.logger.Info("bootstrap: worker is enrolled", "name", name)
			return
		case bootstrapRevoked:
			r.logger.Info("bootstrap: worker name is revoked and will not be re-enrolled automatically; run `gso workers add` to enroll it again", "name", name)
			return
		case bootstrapPending:
			if expires.IsZero() || time.Until(expires) < margin {
				e, err := r.mintBootstrap(ctx, p)
				if err != nil {
					r.logger.Warn("bootstrap: could not write a join string", "name", name, "file", p.joinFile, "error", err)
				} else {
					expires = e
					r.logger.Info("bootstrap: join string written", "name", name, "file", p.joinFile, "expires", expires)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// mintBootstrap creates an enrollment exactly as `gso workers add` does (which
// replaces the name's previous pending code) and writes the join string to the
// plan's file.
func (r *Router) mintBootstrap(ctx context.Context, p *bootstrapPlan) (time.Time, error) {
	res, err := r.AddWorker(ctx, p.params)
	if err != nil {
		return time.Time{}, err
	}
	if err := writeJoinFile(p.joinFile, res.Join); err != nil {
		return time.Time{}, err
	}
	return res.ExpiresAt, nil
}

// writeJoinFile replaces path atomically (temp file in the same directory,
// then rename) with mode 0600. A missing directory is created with mode 0700.
func writeJoinFile(path, join string) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".join-*")
	if err != nil {
		return fmt.Errorf("creating a temporary join file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil && !errors.Is(err, os.ErrPermission) {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(join + "\n"); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
