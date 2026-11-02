package router

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/fleet"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
)

var repoBoundPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/([A-Za-z0-9_.-]+|\*)$`)

// workerErr maps fleet.ErrUnknownWorker to a clear not-found error so the
// admin path never reports it as an internal failure.
func workerErr(name string, err error) error {
	if errors.Is(err, fleet.ErrUnknownWorker) {
		return fmt.Errorf("worker %q not found: %w", name, errWorkerNotFound)
	}
	return err
}

func (r *Router) workerRegistry() (*fleet.Registry, error) {
	if r.registry == nil {
		return nil, errWorkersDisabled
	}
	return r.registry, nil
}

// parseBounds validates admin-declared bounds (spec §4.4).
func (r *Router) parseBounds(classes []string, slots int, repos []string) (fleet.Bounds, error) {
	var b fleet.Bounds
	seen := map[runner.Class]bool{}
	for _, raw := range classes {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		c, err := runner.ParseClass(raw)
		if err != nil {
			return fleet.Bounds{}, fmt.Errorf("class %q: %w", raw, err)
		}
		if !seen[c] {
			seen[c] = true
			b.Classes = append(b.Classes, c)
		}
	}
	if len(b.Classes) == 0 {
		return fleet.Bounds{}, fmt.Errorf("at least one class is required (e.g. --classes linux-x64)")
	}
	if slots < 1 {
		return fleet.Bounds{}, fmt.Errorf("slots must be at least 1")
	}
	if limit := r.cfg.Workers.MaxSlotsPerWorker; limit > 0 && slots > limit {
		return fleet.Bounds{}, fmt.Errorf("slots %d exceeds workers.max_slots_per_worker (%d)", slots, limit)
	}
	b.MaxSlots = slots
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		if !repoBoundPattern.MatchString(repo) {
			return fleet.Bounds{}, fmt.Errorf("repo bound %q: want owner/repo or owner/*", repo)
		}
		b.Repos = append(b.Repos, repo)
	}
	return b, nil
}

// AddWorker creates a single-use enrollment and returns its join string.
// The join string carries the enrollment code: it is never logged.
func (r *Router) AddWorker(_ context.Context, p control.WorkersAddParams) (control.WorkersAddResult, error) {
	reg, err := r.workerRegistry()
	if err != nil {
		return control.WorkersAddResult{}, err
	}
	b, err := r.parseBounds(p.Classes, p.Slots, p.Repos)
	if err != nil {
		return control.WorkersAddResult{}, err
	}
	advertise := strings.TrimSpace(p.Advertise)
	if advertise == "" {
		advertise = strings.TrimSpace(r.cfg.Workers.Advertise)
	}
	if advertise == "" {
		return control.WorkersAddResult{}, fmt.Errorf("no advertise address: set workers.advertise (GSO_WORKERS_ADVERTISE) or pass --advertise host:port")
	}
	if _, _, err := net.SplitHostPort(advertise); err != nil {
		return control.WorkersAddResult{}, fmt.Errorf("advertise address %q: %w", advertise, err)
	}
	code, expires, err := reg.AddEnrollment(p.Name, b, time.Now())
	if err != nil {
		return control.WorkersAddResult{}, err
	}
	r.logger.Info("worker enrollment created", "name", p.Name, "classes", p.Classes, "slots", b.MaxSlots, "expires", expires)
	return control.WorkersAddResult{Join: worker.FormatJoin(advertise, r.pin, code), ExpiresAt: expires}, nil
}

// UpdateWorker replaces an enrolled worker's bounds; a live session is
// re-clamped by the fleet.
func (r *Router) UpdateWorker(_ context.Context, p control.WorkersUpdateParams) error {
	reg, err := r.workerRegistry()
	if err != nil {
		return err
	}
	b, err := r.parseBounds(p.Classes, p.Slots, p.Repos)
	if err != nil {
		return err
	}
	rec, err := reg.Update(p.Name, b)
	if err != nil {
		return workerErr(p.Name, err)
	}
	r.fleet.UpdateBounds(rec.ID, b)
	return nil
}

// ListWorkers merges the local member, the registry and live fleet state.
func (r *Router) ListWorkers() []control.WorkerLiveStatus {
	byID := map[string]fleet.MemberStatus{}
	var out []control.WorkerLiveStatus
	for _, m := range r.fleet.Members() {
		if m.Local {
			out = append(out, control.WorkerLiveStatus{
				Name: m.Name, Local: true, Connected: true, Availability: m.Availability,
				Slots: m.Slots, Busy: m.Busy, Classes: m.Classes, AcceptPublic: m.AcceptPublic,
			})
			continue
		}
		byID[m.WorkerID] = m
	}
	if r.registry != nil {
		for _, rec := range r.registry.List() {
			st := control.WorkerLiveStatus{Name: rec.Name, ID: rec.ID, Revoked: rec.Revoked, Availability: "offline", Slots: rec.Bounds.MaxSlots}
			for _, c := range rec.Bounds.Classes {
				st.Classes = append(st.Classes, c.String())
			}
			if m, ok := byID[rec.ID]; ok {
				st.Connected = m.Connected
				st.Availability = m.Availability
				st.Slots, st.Busy = m.Slots, m.Busy
				st.Classes = m.Classes
				st.AcceptPublic = m.AcceptPublic
				st.LastSeen = m.LastSeen
			}
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// DrainWorker sends a router-initiated drain (spec §4.4).
func (r *Router) DrainWorker(name, reason string) error {
	reg, err := r.workerRegistry()
	if err != nil {
		return err
	}
	rec, ok := reg.ByName(name) // non-revoked workers only
	if !ok {
		for _, old := range reg.List() {
			if old.Name == name && old.Revoked {
				return fmt.Errorf("worker %q is revoked", name)
			}
		}
		return workerErr(name, fleet.ErrUnknownWorker)
	}
	return r.fleet.DrainWorker(rec.ID, reason)
}

// RevokeWorker refuses the credential from now on and ends the worker's
// session and runners (spec §7 "Worker revoked").
func (r *Router) RevokeWorker(ctx context.Context, name string) error {
	reg, err := r.workerRegistry()
	if err != nil {
		return err
	}
	rec, err := reg.Revoke(name)
	if err != nil {
		return workerErr(name, err)
	}
	if err := r.fleet.RevokeWorker(ctx, rec.ID); err != nil {
		r.logger.Warn("revoked worker had no live session to end", "name", name, "error", err)
	}
	// fleet.RevokeWorker publishes the worker.revoked event.
	r.logger.Info("worker revoked", "name", name)
	return nil
}
