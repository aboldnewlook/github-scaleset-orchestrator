// Package fleet places runners on the router's members — an optional
// in-process local member and enrolled remote workers — and keeps the worker
// registry (design spec §4.3–§4.5, §5, §7, §8.2).
package fleet

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

const (
	enrollmentTTL  = 15 * time.Minute
	maxBoundsSlots = 256
)

// EnrollmentTTL is how long an enrollment code stays redeemable.
const EnrollmentTTL = enrollmentTTL

var (
	ErrInvalidCode     = errors.New("invalid or expired enrollment code")
	ErrUnauthenticated = errors.New("invalid worker credential")
	ErrRevoked         = errors.New("worker credential revoked")
	ErrNameTaken       = errors.New("worker name already enrolled")
	ErrBadName         = errors.New(`worker name must match ^[a-z0-9][a-z0-9-]{0,31}$ and its runner segment must not be "local"`)
	ErrUnknownWorker   = errors.New("unknown worker")
)

var workerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Bounds are the admin-declared limits of one worker credential (spec §4.3).
type Bounds struct {
	Classes  []runner.Class
	MaxSlots int
	Repos    []string // exact "owner/repo" or "owner/*"; empty = every router repo
}

type boundsJSON struct {
	Classes  []string `json:"classes"`
	MaxSlots int      `json:"max_slots"`
	Repos    []string `json:"repos,omitempty"`
}

// MarshalJSON stores classes as "linux-x64" strings.
func (b Bounds) MarshalJSON() ([]byte, error) {
	out := boundsJSON{MaxSlots: b.MaxSlots, Repos: b.Repos, Classes: make([]string, 0, len(b.Classes))}
	for _, c := range b.Classes {
		out.Classes = append(out.Classes, c.String())
	}
	return json.Marshal(out)
}

// UnmarshalJSON parses classes stored as "linux-x64" strings.
func (b *Bounds) UnmarshalJSON(data []byte) error {
	var in boundsJSON
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	b.MaxSlots, b.Repos, b.Classes = in.MaxSlots, in.Repos, nil
	for _, s := range in.Classes {
		c, err := runner.ParseClass(s)
		if err != nil {
			return err
		}
		b.Classes = append(b.Classes, c)
	}
	return nil
}

func validateBounds(b Bounds) error {
	if len(b.Classes) == 0 {
		return errors.New("bounds need at least one class")
	}
	if b.MaxSlots < 1 || b.MaxSlots > maxBoundsSlots {
		return fmt.Errorf("slots must be between 1 and %d", maxBoundsSlots)
	}
	for _, r := range b.Repos {
		owner, name, ok := strings.Cut(r, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return fmt.Errorf("repo %q must be owner/name or owner/*", r)
		}
	}
	return nil
}

// WorkerRecord is one enrolled worker. Only the SHA-256 of its secret is kept.
type WorkerRecord struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Segment    string    `json:"segment"`
	SecretHash string    `json:"secret_hash"`
	Bounds     Bounds    `json:"bounds"`
	MachineID  string    `json:"machine_id"`
	EnrolledAt time.Time `json:"enrolled_at"`
	Revoked    bool      `json:"revoked"`
}

type pendingEnrollment struct {
	name    string
	bounds  Bounds
	expires time.Time
}

// Registry is the persistent set of enrolled workers (<state>/workers.json)
// plus the in-memory, hash-only pending enrollment codes.
type Registry struct {
	mu      sync.Mutex
	path    string
	workers map[string]*WorkerRecord     // by worker ID
	codes   map[string]pendingEnrollment // by hex sha256(code)
}

type registryFile struct {
	Workers []WorkerRecord `json:"workers"`
}

// OpenRegistry loads path, or starts empty when it does not exist.
func OpenRegistry(path string) (*Registry, error) {
	r := &Registry{path: path, workers: map[string]*WorkerRecord{}, codes: map[string]pendingEnrollment{}}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading worker registry: %w", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("worker registry %s has mode %04o; it must not be accessible by group or others (chmod 600 %s)", path, info.Mode().Perm(), path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading worker registry: %w", err)
	}
	var f registryFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parsing worker registry %s: %w", path, err)
	}
	for i := range f.Workers {
		w := f.Workers[i]
		r.workers[w.ID] = &w
	}
	return r, nil
}

// ValidWorkerName reports whether name may be enrolled, with the same rule
// AddEnrollment applies.
func ValidWorkerName(name string) error {
	if !workerNameRE.MatchString(name) || naming.WorkerSegment(name) == naming.LocalWorkerSegment {
		return ErrBadName
	}
	return nil
}

// AddEnrollment creates a single-use code for name, valid for 15 minutes.
// Running it again for the same name replaces the pending code.
func (r *Registry) AddEnrollment(name string, b Bounds, now time.Time) (string, time.Time, error) {
	if !workerNameRE.MatchString(name) || naming.WorkerSegment(name) == naming.LocalWorkerSegment {
		return "", time.Time{}, ErrBadName
	}
	if err := validateBounds(b); err != nil {
		return "", time.Time{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for h, p := range r.codes {
		if now.After(p.expires) || p.name == name {
			delete(r.codes, h)
		}
	}
	if err := r.nameFreeLocked(name); err != nil {
		return "", time.Time{}, err
	}
	code, err := randomHex(16)
	if err != nil {
		return "", time.Time{}, err
	}
	expires := now.Add(enrollmentTTL)
	r.codes[hashHex(code)] = pendingEnrollment{name: name, bounds: b, expires: expires}
	return code, expires, nil
}

func (r *Registry) nameFreeLocked(name string) error {
	segment := naming.WorkerSegment(name)
	for _, w := range r.workers {
		if !w.Revoked && (w.Name == name || w.Segment == segment) {
			return ErrNameTaken
		}
	}
	return nil
}

// Enroll redeems a code. The admin-declared name always wins over
// requestedName; the caller logs a mismatch.
func (r *Registry) Enroll(code, requestedName, machineID string, now time.Time) (string, WorkerRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := hashHex(code)
	p, ok := r.codes[h]
	if !ok {
		return "", WorkerRecord{}, ErrInvalidCode
	}
	delete(r.codes, h) // single use, even when it has expired
	if now.After(p.expires) {
		return "", WorkerRecord{}, ErrInvalidCode
	}
	if err := r.nameFreeLocked(p.name); err != nil {
		return "", WorkerRecord{}, err
	}
	id, err := randomHex(8)
	if err != nil {
		return "", WorkerRecord{}, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", WorkerRecord{}, fmt.Errorf("generating worker secret: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)
	rec := WorkerRecord{
		ID: id, Name: p.name, Segment: naming.WorkerSegment(p.name), SecretHash: hashHex(secret),
		Bounds: p.bounds, MachineID: machineID, EnrolledAt: now.UTC(),
	}
	r.workers[id] = &rec
	if err := r.saveLocked(); err != nil {
		delete(r.workers, id)
		return "", WorkerRecord{}, err
	}
	_ = requestedName // the admin-declared name wins (spec §4.4)
	return id + "." + secret, rec, nil
}

// Authenticate checks a "<worker_id>.<secret>" credential: one map lookup and
// one constant-time comparison.
func (r *Registry) Authenticate(credential string) (WorkerRecord, error) {
	id, secret, ok := strings.Cut(credential, ".")
	if !ok || id == "" || secret == "" {
		return WorkerRecord{}, ErrUnauthenticated
	}
	r.mu.Lock()
	w := r.workers[id]
	var rec WorkerRecord
	if w != nil {
		rec = *w
	}
	r.mu.Unlock()
	if w == nil || subtle.ConstantTimeCompare([]byte(hashHex(secret)), []byte(rec.SecretHash)) != 1 {
		return WorkerRecord{}, ErrUnauthenticated
	}
	if rec.Revoked {
		return WorkerRecord{}, ErrRevoked
	}
	return rec, nil
}

// Update replaces the bounds of a non-revoked worker.
func (r *Registry) Update(name string, b Bounds) (WorkerRecord, error) {
	if err := validateBounds(b); err != nil {
		return WorkerRecord{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.byNameLocked(name)
	if w == nil {
		return WorkerRecord{}, ErrUnknownWorker
	}
	old := w.Bounds
	w.Bounds = b
	if err := r.saveLocked(); err != nil {
		w.Bounds = old
		return WorkerRecord{}, err
	}
	return *w, nil
}

// Revoke marks a worker's credential as refused from now on.
func (r *Registry) Revoke(name string) (WorkerRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.byNameLocked(name)
	if w == nil {
		return WorkerRecord{}, ErrUnknownWorker
	}
	w.Revoked = true
	if err := r.saveLocked(); err != nil {
		w.Revoked = false
		return WorkerRecord{}, err
	}
	return *w, nil
}

// ByName returns the non-revoked worker with that name.
func (r *Registry) ByName(name string) (WorkerRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w := r.byNameLocked(name); w != nil {
		return *w, true
	}
	return WorkerRecord{}, false
}

// List returns every record, revoked ones included, sorted by name.
func (r *Registry) List() []WorkerRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]WorkerRecord, 0, len(r.workers))
	for _, w := range r.workers {
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].EnrolledAt.Before(out[j].EnrolledAt)
	})
	return out
}

func (r *Registry) byNameLocked(name string) *WorkerRecord {
	for _, w := range r.workers {
		if !w.Revoked && w.Name == name {
			return w
		}
	}
	return nil
}

func (r *Registry) saveLocked() error {
	f := registryFile{Workers: make([]WorkerRecord, 0, len(r.workers))}
	for _, w := range r.workers {
		f.Workers = append(f.Workers, *w)
	}
	sort.Slice(f.Workers, func(i, j int) bool { return f.Workers[i].ID < f.Workers[j].ID })
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(r.path, data, 0o600)
}

// writeFileAtomic writes data to a temp file in path's directory with perm,
// then renames it over path.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op after a successful rename
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func randomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
