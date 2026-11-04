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
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
)

// Defaults for the bootstrap loop; tests shorten them on the Router.
//
// An enrollment code lives 15 minutes (fleet's enrollmentTTL). The loop
// re-mints once less than the margin is left, so a worker that starts late
// still finds a code with plenty of time to redeem it.
//
// The loop stops offering codes after the give-up time without an enrollment,
// so a sibling that never starts does not leave a fresh bearer code on disk
// for ever, and it backs off after a failed write instead of retrying every poll.
const (
	defaultBootstrapPoll    = 10 * time.Second
	defaultBootstrapMargin  = 2 * time.Minute
	defaultBootstrapGiveUp  = 24 * time.Hour
	defaultBootstrapBackoff = 5 * time.Minute
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
	if err := r.checkJoinFile(bc.JoinFile); err != nil {
		return nil, fmt.Errorf("workers.bootstrap.join_file %q: %w", bc.JoinFile, err)
	}
	return &bootstrapPlan{
		params:   control.WorkersAddParams{Name: bc.Name, Classes: bc.Classes, Slots: bc.Slots},
		joinFile: bc.JoinFile,
	}, nil
}

// checkJoinFile keeps the router from writing (and later deleting) a file that
// is not meant to be a join file: it must be an absolute, cleaned path and
// must not sit under the router's state directory (workers.json, the TLS key)
// or under /etc/gso (the config).
func (r *Router) checkJoinFile(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("must be an absolute path")
	}
	if filepath.Clean(path) != path {
		return fmt.Errorf("must be a clean path (no .., //, or trailing slash); use %q", filepath.Clean(path))
	}
	for _, dir := range []struct{ path, what string }{{r.stateDir, "the router state directory"}, {"/etc/gso", "/etc/gso"}} {
		if dir.path == "" {
			continue
		}
		if rel, err := filepath.Rel(dir.path, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("must not be inside %s; use a dedicated directory such as /bootstrap/join", dir.what)
		}
	}
	return nil
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
	giveUp, backoff := r.bootstrapGiveUp, r.bootstrapBackoff
	if poll <= 0 {
		poll = defaultBootstrapPoll
	}
	if margin <= 0 {
		margin = defaultBootstrapMargin
	}
	if giveUp <= 0 {
		giveUp = defaultBootstrapGiveUp
	}
	if backoff <= 0 {
		backoff = defaultBootstrapBackoff
	}
	name := p.params.Name
	deadline := time.Now().Add(giveUp)
	var expires, retryAt time.Time // our pending code's expiry; no minting before retryAt
	writeFailed := false
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		switch r.bootstrapStateOf(name) {
		case bootstrapEnrolled:
			r.bootstrapEnrolled(p)
			return
		case bootstrapRevoked:
			r.logger.Info("bootstrap: worker name is revoked and will not be re-enrolled automatically; run `gso workers add` to enroll it again", "name", name)
			return
		}
		if time.Now().After(deadline) {
			r.removeJoinFile(p)
			r.logger.Warn("bootstrap: gave up; the worker did not enroll in time and the join file was removed. Bootstrap stays off until the next router restart", "name", name, "after", giveUp)
			return
		}
		if !expires.IsZero() {
			// A code that is no longer the live one for this name was replaced
			// (an admin ran `gso workers add`) or lost: the file is stale.
			cur, ok := r.registry.PendingExpiry(name)
			switch {
			case ok && !cur.Equal(expires):
				r.logger.Warn("bootstrap: the pending enrollment was replaced (for example by `gso workers add`); writing a new join string, which replaces that one", "name", name)
				expires = time.Time{}
			case !ok:
				expires = time.Time{}
			}
		}
		if (expires.IsZero() || time.Until(expires) < margin) && !time.Now().Before(retryAt) {
			if r.bootstrapBeforeMint != nil {
				r.bootstrapBeforeMint()
			}
			e, err := r.mintBootstrap(ctx, p)
			switch {
			case err == nil:
				expires, writeFailed = e, false
				r.logger.Info("bootstrap: join string written", "name", name, "file", p.joinFile, "expires", expires)
			case errors.Is(err, fleet.ErrNameTaken):
				if r.bootstrapStateOf(name) == bootstrapEnrolled {
					r.bootstrapEnrolled(p) // enrolled between the check and the mint
					return
				}
				r.removeJoinFile(p)
				r.logger.Warn("bootstrap: cannot enroll this name because another worker has the same runner name segment; rename workers.bootstrap.name or revoke that worker. Bootstrap is stopping", "name", name, "conflicts_with", r.segmentOwner(name))
				return
			case errors.Is(err, errJoinFileWrite):
				expires = time.Time{}
				retryAt = time.Now().Add(backoff)
				if !writeFailed {
					writeFailed = true
					r.logger.Error("bootstrap: cannot write the join file. If the directory belongs to another user (for example a gso-bootstrap volume created by an older deploy and owned by root), remove the gso-bootstrap volume and redeploy. Retrying later", "name", name, "dir", filepath.Dir(p.joinFile), "file", p.joinFile, "retry_in", backoff, "error", err)
				}
			default:
				r.logger.Warn("bootstrap: could not create an enrollment", "name", name, "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Router) bootstrapEnrolled(p *bootstrapPlan) {
	r.removeJoinFile(p)
	r.logger.Info("bootstrap: worker is enrolled", "name", p.params.Name)
}

func (r *Router) removeJoinFile(p *bootstrapPlan) {
	if err := os.Remove(p.joinFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		r.logger.Warn("bootstrap: could not remove the join file", "name", p.params.Name, "file", p.joinFile, "error", err)
	}
}

// segmentOwner names the live worker whose runner segment equals name's.
func (r *Router) segmentOwner(name string) string {
	seg := naming.WorkerSegment(name)
	for _, rec := range r.registry.List() {
		if !rec.Revoked && rec.Segment == seg {
			return rec.Name
		}
	}
	return ""
}

var errJoinFileWrite = errors.New("writing the join file")

// mintBootstrap creates an enrollment exactly as `gso workers add` does (which
// replaces the name's previous pending code) and writes the join string to the
// plan's file. A write failure wraps errJoinFileWrite.
func (r *Router) mintBootstrap(ctx context.Context, p *bootstrapPlan) (time.Time, error) {
	res, err := r.AddWorker(ctx, p.params)
	if err != nil {
		return time.Time{}, err
	}
	if err := writeJoinFile(p.joinFile, res.Join); err != nil {
		return time.Time{}, fmt.Errorf("%w: %w", errJoinFileWrite, err)
	}
	return res.ExpiresAt, nil
}

// startBootstrap runs the loop under a context Run can cancel and join, so it
// never outlives Run (no stray writes into a directory the caller is removing).
func (r *Router) startBootstrap(ctx context.Context) {
	ctx, r.bootstrapCancel = context.WithCancel(ctx)
	go r.runBootstrap(ctx, r.bootstrap)
}

// stopBootstrap ends the loop and waits for it.
func (r *Router) stopBootstrap() {
	if r.bootstrapCancel == nil {
		return
	}
	r.bootstrapCancel()
	<-r.bootstrapDone
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
