// Package ghsim simulates the parts of GitHub that gso talks to: runner scale
// sets, their message sessions and queues, JIT runner registration, and the
// REST calls for repository visibility and workflow runs. Jobs are injected by
// hand and claimed by fake runners, so a real router and real workers can be
// exercised without github.com.
//
// Not modelled: GitHub's capacity 0→N wake-up stall (ARC#4715), the real
// routing across several matching scale sets (the simulator's choice is a
// deterministic guess), rate limits and authentication.
package ghsim

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/timing"
)

// Job states reported in JobState.State.
const (
	JobQueued    = "queued"
	JobAssigned  = "assigned"
	JobRunning   = "running"
	JobCompleted = "completed"
	JobCancelled = "cancelled"
)

var (
	// ErrUnknownRunner reports a runner name the simulator does not know, or a wrong runner token.
	ErrUnknownRunner = errors.New("ghsim: unknown runner or wrong runner token")
	// ErrRunnerRemoved tells a waiting fake runner that RemoveRunner deregistered it.
	ErrRunnerRemoved = errors.New("ghsim: runner was removed")
	// ErrUnknownJob reports a job ID that is not bound to the calling runner.
	ErrUnknownJob = errors.New("ghsim: unknown job")
)

// Options configures a Model. Zero values select the defaults.
type Options struct {
	Now          func() time.Time
	PollWait     time.Duration // how long GetMessage blocks with nothing queued
	ClaimTimeout time.Duration // an assignment no runner claims is cancelled and requeued after this
	MaxReassign  int           // requeues before a job is cancelled for good
	SessionTTL   time.Duration // a session nobody polls expires after this; until then a new one gets 409
	ServerURL    string        // base URL fake runners call; written into JIT configs as serverUrl
}

func (o Options) withDefaults() Options {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.PollWait <= 0 {
		o.PollWait = timing.Scale(2 * time.Second)
	}
	if o.ClaimTimeout <= 0 {
		o.ClaimTimeout = timing.Scale(60 * time.Second)
	}
	if o.MaxReassign <= 0 {
		o.MaxReassign = 3
	}
	if o.SessionTTL <= 0 {
		o.SessionTTL = timing.Scale(2 * time.Minute)
	}
	return o
}

// JobSpec describes a job to inject.
type JobSpec struct {
	Repo     string        `json:"repo"`
	Labels   []string      `json:"labels"`
	Duration time.Duration `json:"duration"`
	Public   bool          `json:"public,omitempty"` // marks the repository public
	Fork     bool          `json:"fork,omitempty"`   // the run's head repository is a fork
	Event    string        `json:"event,omitempty"`  // default "push"
	Result   string        `json:"result,omitempty"` // what the fake runner reports; default "succeeded"
}

// JobState is a snapshot of one job.
type JobState struct {
	ID         int64     `json:"id"`
	RunID      int64     `json:"run_id"`
	Spec       JobSpec   `json:"spec"`
	State      string    `json:"state"`
	Result     string    `json:"result,omitempty"`
	ScaleSet   string    `json:"scale_set,omitempty"`
	Runner     string    `json:"runner,omitempty"`
	Reassigned int       `json:"reassigned,omitempty"`
	QueuedAt   time.Time `json:"queued_at"`
	AssignedAt time.Time `json:"assigned_at,omitzero"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// Claim is the job a fake runner received.
type Claim struct {
	JobID    int64         `json:"job_id"`
	RunID    int64         `json:"run_id"`
	Duration time.Duration `json:"duration"`
	Result   string        `json:"result"`
}

// RunnerInfo describes a runner the simulator minted.
type RunnerInfo struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Repo       string `json:"repo"`
	ScaleSetID int    `json:"scale_set_id"`
	State      string `json:"state"` // idle | busy | finished | removed
}

// Model is an in-memory GitHub. It is safe for concurrent use.
type Model struct {
	opts Options

	mu      sync.Mutex
	changed chan struct{} // closed and replaced on every state change

	nextScaleSetID int
	nextRunnerID   int64
	nextJobID      int64

	repos      map[string]*repoState
	scaleSets  map[int]*scaleSetState
	pending    map[string]*scaleSetState // controls applied before the set exists, by setKey
	sessions   map[uuid.UUID]int         // session ID → scale-set ID
	jobs       []*jobState               // in ID order
	runners    map[string]*runnerState   // current runner per name
	allRunners []*runnerState            // every runner ever minted, in mint order
}

type repoState struct {
	public    bool
	info      *ghapi.RepoInfo
	mints     int
	removed   []int64
	deleted   []int
	cancelled []int64
	failRuns  int
}

type scaleSetState struct {
	repo     string
	set      scaleset.RunnerScaleSet
	session  *sessionState
	failN    int
	failErr  error
	capacity int
	queue    []*scaleset.RunnerScaleSetMessage
	nextMsg  int
	override *scaleset.RunnerScaleSetStatistic
}

type sessionState struct {
	id       uuid.UUID
	owner    string
	lastPoll time.Time
	closed   bool
}

type jobState struct {
	JobState
	scaleSetID      int
	cancelRequested bool
}

func (j *jobState) snapshot() JobState {
	s := j.JobState
	s.Spec.Labels = append([]string(nil), j.Spec.Labels...)
	return s
}

type runnerState struct {
	id         int64
	name       string
	token      string
	repo       string
	scaleSetID int
	jobID      int64
	removed    bool
	finished   bool
}

// NewModel returns an empty simulated GitHub.
func NewModel(o Options) *Model {
	return &Model{
		opts:      o.withDefaults(),
		changed:   make(chan struct{}),
		repos:     make(map[string]*repoState),
		scaleSets: make(map[int]*scaleSetState),
		pending:   make(map[string]*scaleSetState),
		sessions:  make(map[uuid.UUID]int),
		runners:   make(map[string]*runnerState),
	}
}

func (m *Model) now() time.Time { return m.opts.Now() }

// notifyLocked wakes every waiter (long polls and runner claims).
func (m *Model) notifyLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}

func (m *Model) repoLocked(name string) *repoState {
	r := m.repos[name]
	if r == nil {
		r = &repoState{}
		m.repos[name] = r
	}
	return r
}

func setKey(repo, name string) string { return repo + "\x00" + name }

func splitRepo(repo string) (owner, name string) {
	owner, name, _ = strings.Cut(repo, "/")
	return owner, name
}

func validRepo(repo string) bool {
	owner, name, ok := strings.Cut(repo, "/")
	return ok && owner != "" && name != "" && !strings.Contains(name, "/")
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("ghsim: crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}

func (m *Model) scaleSetByNameLocked(repo, name string) *scaleSetState {
	for _, ss := range m.scaleSets {
		if ss.repo == repo && ss.set.Name == name {
			return ss
		}
	}
	return nil
}

// controlTargetLocked returns the named set, or a placeholder that
// CreateRunnerScaleSet adopts, so tests can script a set before the router creates it.
func (m *Model) controlTargetLocked(repo, name string) *scaleSetState {
	if ss := m.scaleSetByNameLocked(repo, name); ss != nil {
		return ss
	}
	key := setKey(repo, name)
	p := m.pending[key]
	if p == nil {
		p = &scaleSetState{repo: repo, set: scaleset.RunnerScaleSet{Name: name}}
		m.pending[key] = p
	}
	return p
}

// Push enqueues msg on the named scale set. A non-zero msg.MessageID is kept
// (tests script redeliveries with it); zero gets the next ID. A message without
// Statistics is delivered with the statistics of the moment.
func (m *Model) Push(repo, scaleSetName string, msg *scaleset.RunnerScaleSetMessage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *msg
	ss := m.controlTargetLocked(repo, scaleSetName)
	if cp.MessageID != 0 {
		ss.nextMsg = max(ss.nextMsg, cp.MessageID)
		ss.queue = append(ss.queue, &cp)
		m.notifyLocked()
		return
	}
	m.enqueueLocked(ss, &cp)
}

// SetStats replaces the computed statistics of the named scale set.
func (m *Model) SetStats(repo, scaleSetName string, st scaleset.RunnerScaleSetStatistic) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.controlTargetLocked(repo, scaleSetName).override = &st
}

// FailSessions makes the next n OpenSession calls on the named set return err.
func (m *Model) FailSessions(repo, scaleSetName string, err error, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ss := m.controlTargetLocked(repo, scaleSetName)
	ss.failN, ss.failErr = n, err
}

// ScaleSets returns the repository's scale sets in ID order.
func (m *Model) ScaleSets(repo string) []scaleset.RunnerScaleSet {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listScaleSetsLocked(repo)
}

// Runners returns the repository's registered runners (minted, not removed, not finished).
func (m *Model) Runners(repo string) []scaleset.RunnerReference {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []scaleset.RunnerReference
	for _, r := range m.allRunners {
		if r.repo == repo && !r.removed && !r.finished {
			out = append(out, scaleset.RunnerReference{ID: int(r.id), Name: r.name, RunnerScaleSetID: r.scaleSetID})
		}
	}
	return out
}

// Removed returns the runner IDs deleted through RemoveRunner.
func (m *Model) Removed(repo string) []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int64(nil), m.repoLocked(repo).removed...)
}

// MintCount returns how many JIT configs were minted for the repository.
func (m *Model) MintCount(repo string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.repoLocked(repo).mints
}

// LastCapacity returns the maxCapacity of the named set's latest poll.
func (m *Model) LastCapacity(repo, scaleSetName string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ss := m.scaleSetByNameLocked(repo, scaleSetName); ss != nil {
		return ss.capacity
	}
	return 0
}

// Deleted returns the IDs of scale sets deleted with DeleteScaleSet.
func (m *Model) Deleted(repo string) []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.repoLocked(repo).deleted...)
}

// DeleteScaleSet models `gso scaleset reset`: the set, its session and its idle
// runners go away, and its unclaimed assignments are requeued.
func (m *Model) DeleteScaleSet(repo string, id int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ss, ok := m.scaleSets[id]
	if !ok || ss.repo != repo {
		return fmt.Errorf("ghsim: scale set %d: %w", id, scaleset.NotFoundError)
	}
	if ss.session != nil {
		delete(m.sessions, ss.session.id)
	}
	delete(m.scaleSets, id)
	r := m.repoLocked(repo)
	r.deleted = append(r.deleted, id)
	for _, j := range m.jobs {
		if j.scaleSetID == id && j.State == JobAssigned {
			j.State, j.scaleSetID, j.ScaleSet, j.AssignedAt = JobQueued, 0, "", time.Time{}
		}
	}
	for _, rn := range m.allRunners {
		if rn.scaleSetID == id && rn.jobID == 0 {
			rn.removed = true
		}
	}
	m.assignLocked()
	m.notifyLocked()
	return nil
}

// SetRepo overrides the repository information Repo() returns.
func (m *Model) SetRepo(repo string, info ghapi.RepoInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := info
	m.repoLocked(repo).info = &cp
}

// Cancelled returns the run IDs cancelled in the repository.
func (m *Model) Cancelled(repo string) []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int64(nil), m.repoLocked(repo).cancelled...)
}

// FailRuns makes the next n WorkflowRun lookups in the repository fail.
func (m *Model) FailRuns(repo string, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.repoLocked(repo).failRuns = n
}

// Jobs returns every job in ID order.
func (m *Model) Jobs() []JobState {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tickLocked()
	out := make([]JobState, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, j.snapshot())
	}
	return out
}

// Job returns one job.
func (m *Model) Job(id int64) (JobState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.jobLocked(id); j != nil {
		return j.snapshot(), true
	}
	return JobState{}, false
}

// AllRunners returns every runner ever minted, in mint order.
func (m *Model) AllRunners() []RunnerInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RunnerInfo, 0, len(m.allRunners))
	for _, r := range m.allRunners {
		state := "idle"
		switch {
		case r.removed:
			state = "removed"
		case r.finished:
			state = "finished"
		case r.jobID != 0:
			state = "busy"
		}
		out = append(out, RunnerInfo{ID: r.id, Name: r.name, Repo: r.repo, ScaleSetID: r.scaleSetID, State: state})
	}
	sort.SliceStable(out, func(i, k int) bool { return out[i].ID < out[k].ID })
	return out
}
