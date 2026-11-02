package ghsim

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"
)

// waitChange blocks until the model changes, d elapses, or ctx is done.
func waitChange(ctx context.Context, ch <-chan struct{}, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ch:
	case <-t.C:
	}
	return nil
}

func (m *Model) publicSetLocked(ss *scaleSetState) scaleset.RunnerScaleSet {
	out := ss.set
	out.Labels = append([]scaleset.Label(nil), ss.set.Labels...)
	st := m.statsLocked(ss)
	out.Statistics = &st
	return out
}

func (m *Model) sortedScaleSetsLocked(repo string) []*scaleSetState {
	var out []*scaleSetState
	for _, ss := range m.scaleSets {
		if ss.repo == repo {
			out = append(out, ss)
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].set.Name < out[k].set.Name })
	return out
}

func (m *Model) listScaleSetsLocked(repo string) []scaleset.RunnerScaleSet {
	var out []scaleset.RunnerScaleSet
	for _, ss := range m.scaleSets {
		if ss.repo == repo {
			out = append(out, m.publicSetLocked(ss))
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].ID < out[k].ID })
	return out
}

func (m *Model) getScaleSet(repo, name string) *scaleset.RunnerScaleSet {
	m.mu.Lock()
	defer m.mu.Unlock()
	ss := m.scaleSetByNameLocked(repo, name)
	if ss == nil {
		return nil
	}
	out := m.publicSetLocked(ss)
	return &out
}

func (m *Model) createScaleSet(repo string, in *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	if in == nil || strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("ghsim: a scale set needs a name: %w", scaleset.BadRequestError)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.scaleSetByNameLocked(repo, in.Name) != nil {
		return nil, fmt.Errorf("ghsim: scale set %q already exists in %s: %w", in.Name, repo, scaleset.BadRequestError)
	}
	key := setKey(repo, in.Name)
	ss := m.pending[key]
	delete(m.pending, key)
	if ss == nil {
		ss = &scaleSetState{repo: repo}
	}
	m.nextScaleSetID++
	ss.set = *in
	ss.set.ID = m.nextScaleSetID
	ss.set.Labels = append([]scaleset.Label(nil), in.Labels...)
	ss.set.CreatedOn = m.now()
	ss.set.Statistics = nil
	m.scaleSets[ss.set.ID] = ss
	m.repoLocked(repo)
	out := m.publicSetLocked(ss)
	m.notifyLocked()
	return &out, nil
}

func (m *Model) updateScaleSet(repo string, id int, in *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ss, ok := m.scaleSets[id]
	if !ok || ss.repo != repo {
		return nil, fmt.Errorf("ghsim: scale set %d: %w", id, scaleset.NotFoundError)
	}
	if in != nil {
		if in.Name != "" {
			ss.set.Name = in.Name
		}
		if in.Labels != nil {
			ss.set.Labels = append([]scaleset.Label(nil), in.Labels...)
		}
		ss.set.RunnerSetting = in.RunnerSetting
	}
	out := m.publicSetLocked(ss)
	m.assignLocked() // new labels may match queued jobs
	m.notifyLocked()
	return &out, nil
}

func (m *Model) listScaleSets(repo string) []scaleset.RunnerScaleSet {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listScaleSetsLocked(repo)
}

// liveSessionLocked returns the set's session if it is open and was polled within SessionTTL.
func (m *Model) liveSessionLocked(ss *scaleSetState) *sessionState {
	s := ss.session
	if s == nil || s.closed || m.now().Sub(s.lastPoll) > m.opts.SessionTTL {
		return nil
	}
	return s
}

func (m *Model) openSession(repo string, scaleSetID int, owner string) (scaleset.RunnerScaleSetSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ss, ok := m.scaleSets[scaleSetID]
	if !ok || ss.repo != repo {
		return scaleset.RunnerScaleSetSession{}, fmt.Errorf("ghsim: scale set %d: %w", scaleSetID, scaleset.NotFoundError)
	}
	if ss.failN > 0 {
		ss.failN--
		return scaleset.RunnerScaleSetSession{}, ss.failErr
	}
	if live := m.liveSessionLocked(ss); live != nil {
		return scaleset.RunnerScaleSetSession{}, fmt.Errorf("ghsim: scale set %d already has a session owned by %q: %w", scaleSetID, live.owner, scaleset.ConflictError)
	}
	if ss.session != nil {
		delete(m.sessions, ss.session.id)
	}
	s := &sessionState{id: uuid.New(), owner: owner, lastPoll: m.now()}
	ss.session = s
	m.sessions[s.id] = scaleSetID
	set := m.publicSetLocked(ss)
	stats := m.statsLocked(ss)
	m.notifyLocked()
	return scaleset.RunnerScaleSetSession{SessionID: s.id, OwnerName: owner, RunnerScaleSet: &set, Statistics: &stats}, nil
}

// sessionLocked resolves an open, unexpired session of repo.
func (m *Model) sessionLocked(repo string, sid uuid.UUID) (*scaleSetState, *sessionState, error) {
	id, ok := m.sessions[sid]
	ss := m.scaleSets[id]
	if !ok || ss == nil || ss.repo != repo || ss.session == nil || ss.session.id != sid || m.liveSessionLocked(ss) == nil {
		return nil, nil, fmt.Errorf("ghsim: session %s is not open: %w", sid, scaleset.MessageQueueTokenExpiredError)
	}
	return ss, ss.session, nil
}

func (m *Model) getMessage(ctx context.Context, repo string, sid uuid.UUID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	deadline := time.Now().Add(m.opts.PollWait)
	for {
		m.mu.Lock()
		ss, s, err := m.sessionLocked(repo, sid)
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		s.lastPoll = m.now()
		ss.capacity = maxCapacity
		m.tickLocked()
		m.assignLocked()
		if len(ss.queue) > 0 {
			msg := *ss.queue[0]
			if msg.Statistics == nil {
				st := m.statsLocked(ss)
				msg.Statistics = &st
			}
			m.mu.Unlock()
			return &msg, nil
		}
		ch := m.changed
		m.mu.Unlock()
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, nil
		}
		if err := waitChange(ctx, ch, remaining); err != nil {
			return nil, err
		}
	}
}

func (m *Model) deleteMessage(repo string, sid uuid.UUID, messageID int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ss, _, err := m.sessionLocked(repo, sid)
	if err != nil {
		return err
	}
	for i, msg := range ss.queue {
		if msg.MessageID == messageID {
			ss.queue = append(ss.queue[:i], ss.queue[i+1:]...)
			break
		}
	}
	return nil
}

func (m *Model) acquireJobs(repo string, sid uuid.UUID, ids []int64) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, _, err := m.sessionLocked(repo, sid); err != nil {
		return nil, err
	}
	return append([]int64(nil), ids...), nil
}

func (m *Model) closeSession(repo string, sid uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.sessions[sid]; ok {
		if ss := m.scaleSets[id]; ss != nil && ss.repo == repo && ss.session != nil && ss.session.id == sid {
			ss.session.closed = true
		}
		delete(m.sessions, sid)
		m.notifyLocked()
	}
	return nil
}

func (m *Model) enqueueLocked(ss *scaleSetState, msg *scaleset.RunnerScaleSetMessage) {
	ss.nextMsg++
	msg.MessageID = ss.nextMsg
	ss.queue = append(ss.queue, msg)
	m.notifyLocked()
}

func (m *Model) statsLocked(ss *scaleSetState) scaleset.RunnerScaleSetStatistic {
	if ss.override != nil {
		return *ss.override
	}
	var st scaleset.RunnerScaleSetStatistic
	for _, j := range m.jobs {
		if j.scaleSetID != ss.set.ID || ss.set.ID == 0 {
			continue
		}
		switch j.State {
		case JobAssigned:
			st.TotalAssignedJobs++
		case JobRunning:
			st.TotalAssignedJobs++
			st.TotalRunningJobs++
		}
	}
	for _, r := range m.allRunners {
		if r.scaleSetID != ss.set.ID || r.removed || r.finished {
			continue
		}
		st.TotalRegisteredRunners++
		if r.jobID != 0 {
			st.TotalBusyRunners++
		} else {
			st.TotalIdleRunners++
		}
	}
	return st
}

func (m *Model) activeLocked(scaleSetID int) int {
	n := 0
	for _, j := range m.jobs {
		if j.scaleSetID == scaleSetID && (j.State == JobAssigned || j.State == JobRunning) {
			n++
		}
	}
	return n
}

func labelsCover(have []scaleset.Label, want []string) bool {
	set := make(map[string]bool, len(have))
	for _, l := range have {
		set[strings.ToLower(l.Name)] = true
	}
	for _, w := range want {
		if !set[strings.ToLower(w)] {
			return false
		}
	}
	return true
}

// assignLocked hands queued jobs (FIFO) to the first matching scale set (by
// name) that has a live session and spare advertised capacity.
func (m *Model) assignLocked() {
	now := m.now()
	for _, j := range m.jobs {
		if j.State != JobQueued {
			continue
		}
		for _, ss := range m.sortedScaleSetsLocked(j.Spec.Repo) {
			if m.liveSessionLocked(ss) == nil || !labelsCover(ss.set.Labels, j.Spec.Labels) {
				continue
			}
			if ss.capacity-m.activeLocked(ss.set.ID) <= 0 {
				continue
			}
			j.State, j.scaleSetID, j.ScaleSet, j.AssignedAt = JobAssigned, ss.set.ID, ss.set.Name, now
			m.enqueueLocked(ss, &scaleset.RunnerScaleSetMessage{
				JobAssignedMessages: []*scaleset.JobAssigned{{JobMessageBase: m.baseLocked(j, scaleset.MessageTypeJobAssigned)}},
			})
			break
		}
	}
}
