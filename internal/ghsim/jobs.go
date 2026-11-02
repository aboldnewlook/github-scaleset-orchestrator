package ghsim

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/actions/scaleset"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
)

// InjectJob queues a job, as if a workflow run had requested a runner.
func (m *Model) InjectJob(spec JobSpec) (JobState, error) {
	if !validRepo(spec.Repo) {
		return JobState{}, fmt.Errorf("ghsim: repo %q must be owner/name: %w", spec.Repo, scaleset.BadRequestError)
	}
	if spec.Duration < 0 {
		return JobState{}, fmt.Errorf("ghsim: negative job duration: %w", scaleset.BadRequestError)
	}
	var labels []string
	for _, l := range spec.Labels {
		if l = strings.TrimSpace(l); l != "" {
			labels = append(labels, l)
		}
	}
	if len(labels) == 0 {
		labels = []string{"self-hosted"}
	}
	spec.Labels = labels
	if spec.Event == "" {
		spec.Event = "push"
	}
	if spec.Result == "" {
		spec.Result = "succeeded"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	spec.Repo = m.canonLocked(spec.Repo)
	if spec.Public {
		m.repoLocked(spec.Repo).public = true
	}
	m.nextJobID++
	j := &jobState{JobState: JobState{ID: m.nextJobID, RunID: 1000 + m.nextJobID, Spec: spec, State: JobQueued, QueuedAt: m.now()}}
	m.jobs = append(m.jobs, j)
	m.assignLocked()
	m.notifyLocked()
	return j.snapshot(), nil
}

func (m *Model) jobLocked(id int64) *jobState {
	for _, j := range m.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

func claimOf(j *jobState) *Claim {
	return &Claim{JobID: j.ID, RunID: j.RunID, Duration: j.Spec.Duration, Result: j.Spec.Result}
}

func (m *Model) baseLocked(j *jobState, t scaleset.MessageType) scaleset.JobMessageBase {
	owner, name := splitRepo(j.Spec.Repo)
	return scaleset.JobMessageBase{
		JobMessageType:     scaleset.JobMessageType{MessageType: t},
		RunnerRequestID:    j.ID,
		RepositoryName:     name,
		OwnerName:          owner,
		JobID:              strconv.FormatInt(j.ID, 10),
		JobWorkflowRef:     j.Spec.Repo + "/.github/workflows/sim.yml@refs/heads/main",
		JobDisplayName:     fmt.Sprintf("sim job %d", j.ID),
		WorkflowRunID:      j.RunID,
		EventName:          j.Spec.Event,
		RequestLabels:      append([]string(nil), j.Spec.Labels...),
		QueueTime:          j.QueuedAt,
		ScaleSetAssignTime: j.AssignedAt,
		RunnerAssignTime:   j.StartedAt,
		FinishTime:         j.FinishedAt,
	}
}

// tickLocked requeues assignments no runner claimed within ClaimTimeout, the
// way GitHub cancels and requeues an unclaimed assignment.
func (m *Model) tickLocked() {
	now := m.now()
	requeued := false
	for _, j := range m.jobs {
		if j.State != JobAssigned || now.Sub(j.AssignedAt) <= m.opts.ClaimTimeout {
			continue
		}
		if ss := m.scaleSets[j.scaleSetID]; ss != nil {
			m.enqueueLocked(ss, &scaleset.RunnerScaleSetMessage{
				JobCompletedMessages: []*scaleset.JobCompleted{{Result: "canceled", JobMessageBase: m.baseLocked(j, scaleset.MessageTypeJobCompleted)}},
			})
		}
		j.Reassigned++
		j.scaleSetID, j.ScaleSet = 0, ""
		if j.Reassigned > m.opts.MaxReassign {
			j.State, j.Result, j.FinishedAt = JobCancelled, "canceled", now
		} else {
			j.State, j.AssignedAt = JobQueued, time.Time{}
			requeued = true
		}
	}
	if requeued {
		m.assignLocked()
	}
}

func (m *Model) firstUnclaimedLocked(scaleSetID int) *jobState {
	for _, j := range m.jobs {
		if j.State == JobAssigned && j.scaleSetID == scaleSetID && j.Runner == "" {
			return j
		}
	}
	return nil
}

// ClaimJob is what a runner does once connected: it blocks until a job assigned
// to its scale set is free, binds it and reports it started. It returns
// ErrRunnerRemoved when RemoveRunner deregisters the runner first.
func (m *Model) ClaimJob(ctx context.Context, runnerName, token string) (*Claim, error) {
	for {
		m.mu.Lock()
		m.tickLocked()
		r, err := m.runnerLocked(runnerName, token, false)
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		if r.jobID != 0 { // re-claim, e.g. after the fake runner lost its response
			c := claimOf(m.jobLocked(r.jobID))
			m.mu.Unlock()
			return c, nil
		}
		if j := m.firstUnclaimedLocked(r.scaleSetID); j != nil {
			j.State, j.Runner, j.StartedAt = JobRunning, r.name, m.now()
			r.jobID = j.ID
			if ss := m.scaleSets[r.scaleSetID]; ss != nil {
				m.enqueueLocked(ss, &scaleset.RunnerScaleSetMessage{
					JobStartedMessages: []*scaleset.JobStarted{{RunnerID: int(r.id), RunnerName: r.name, JobMessageBase: m.baseLocked(j, scaleset.MessageTypeJobStarted)}},
				})
			}
			c := claimOf(j)
			m.mu.Unlock()
			return c, nil
		}
		ch := m.changed
		m.mu.Unlock()
		if err := waitChange(ctx, ch, m.opts.PollWait); err != nil {
			return nil, err
		}
	}
}

// JobStatus reports whether the runner's job was cancelled.
func (m *Model) JobStatus(runnerName, token string, jobID int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.runnerLocked(runnerName, token, true); err != nil {
		return false, err
	}
	j := m.jobLocked(jobID)
	if j == nil || j.Runner != runnerName {
		return false, ErrUnknownJob
	}
	return j.cancelRequested, nil
}

// CompleteJob finishes the runner's job and deregisters the (ephemeral) runner.
// Repeating it is harmless.
func (m *Model) CompleteJob(runnerName, token string, jobID int64, result string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.runnerLocked(runnerName, token, true)
	if err != nil {
		return err
	}
	j := m.jobLocked(jobID)
	if j == nil || j.Runner != runnerName {
		return ErrUnknownJob
	}
	if j.State != JobRunning {
		return nil
	}
	if result == "" {
		result = j.Spec.Result
	}
	j.State = JobCompleted
	if result == "canceled" {
		j.State = JobCancelled
	}
	j.Result, j.FinishedAt = result, m.now()
	r.finished = true
	if ss := m.scaleSets[j.scaleSetID]; ss != nil {
		m.enqueueLocked(ss, &scaleset.RunnerScaleSetMessage{
			JobCompletedMessages: []*scaleset.JobCompleted{{Result: result, RunnerID: int(r.id), RunnerName: r.name, JobMessageBase: m.baseLocked(j, scaleset.MessageTypeJobCompleted)}},
		})
	}
	m.assignLocked()
	m.notifyLocked()
	return nil
}

// CancelRun cancels a workflow run: queued jobs end at once, assigned ones end
// with a canceled completion, and running ones are told through JobStatus.
func (m *Model) CancelRun(repo string, runID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	found := false
	for _, j := range m.jobs {
		if j.RunID != runID || !sameRepo(j.Spec.Repo, repo) {
			continue
		}
		found = true
		switch j.State {
		case JobQueued:
			j.State, j.Result, j.FinishedAt = JobCancelled, "canceled", now
		case JobAssigned:
			if ss := m.scaleSets[j.scaleSetID]; ss != nil {
				m.enqueueLocked(ss, &scaleset.RunnerScaleSetMessage{
					JobCompletedMessages: []*scaleset.JobCompleted{{Result: "canceled", JobMessageBase: m.baseLocked(j, scaleset.MessageTypeJobCompleted)}},
				})
			}
			j.State, j.Result, j.FinishedAt = JobCancelled, "canceled", now
		case JobRunning:
			j.cancelRequested = true
		}
	}
	if !found {
		return fmt.Errorf("ghsim: run %d in %s: %w", runID, repo, scaleset.NotFoundError)
	}
	r := m.repoLocked(repo)
	r.cancelled = append(r.cancelled, runID)
	m.assignLocked()
	m.notifyLocked()
	return nil
}

func (m *Model) repoInfo(repo string) ghapi.RepoInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.repoLocked(repo)
	if r.info != nil {
		return *r.info
	}
	if r.public {
		return ghapi.RepoInfo{FullName: r.name, Private: false, Visibility: "public"}
	}
	return ghapi.RepoInfo{FullName: r.name, Private: true, Visibility: "private"}
}

func (m *Model) workflowRun(repo string, runID int64) (ghapi.WorkflowRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.repoLocked(repo)
	if r.failRuns > 0 {
		r.failRuns--
		return ghapi.WorkflowRun{}, fmt.Errorf("ghsim: simulated failure looking up run %d", runID)
	}
	for _, j := range m.jobs {
		if j.RunID == runID && sameRepo(j.Spec.Repo, repo) {
			head := r.name
			if j.Spec.Fork {
				owner, name := splitRepo(r.name)
				head = "fork-" + owner + "/" + name
			}
			return ghapi.WorkflowRun{ID: runID, Event: j.Spec.Event, RepoFullName: r.name, HeadRepoFullName: head}, nil
		}
	}
	return ghapi.WorkflowRun{}, fmt.Errorf("ghsim: run %d in %s: %w", runID, repo, scaleset.NotFoundError)
}
