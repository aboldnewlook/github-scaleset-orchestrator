package naming

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

// ScaleSetNamePrefix is the prefix used for all gso-managed scale set names.
const ScaleSetNamePrefix = "gso"

// RepoShortName extracts the repository name from an "owner/repo" string.
// If the input does not contain a slash it is returned as-is.
func RepoShortName(repo string) string {
	parts := strings.Split(repo, "/")
	if len(parts) == 2 {
		return parts[1]
	}
	return repo
}

// ScaleSetName builds the legacy (pre-remote-workers) scale set name,
// "<prefix>-<hostname>-<repoShortName>". gso no longer creates these; it
// uses the name to recognise them during the rename (docs/migrations.md).
func ScaleSetName(prefix, hostname, repo string) string {
	return fmt.Sprintf("%s-%s-%s", prefix, hostname, RepoShortName(repo))
}

// MaxNameLen caps scale-set and runner names. It is the envelope ARC tests
// (design spec §6.1, FEAS §6).
const MaxNameLen = 45

// LocalWorkerSegment is the worker segment of runners the router's
// in-process local member starts. Enrolled workers may not use it.
const LocalWorkerSegment = "local"

const (
	maxWorkerSegment    = 12
	maxRunnerRepo       = 14
	maxOverflowInstance = 10
	runnerSuffixLen     = 6
)

// Sanitize lowercases s and turns every character outside [a-z0-9-] into
// '-', then trims leading and trailing '-'. An empty result becomes "x".
func Sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	if out := strings.Trim(b.String(), "-"); out != "" {
		return out
	}
	return "x"
}

// InstanceID is the identity segment in scale-set names and the message
// session owner: runner.instance_id when configured, else the hostname.
func InstanceID(configured string) string {
	if strings.TrimSpace(configured) != "" {
		return Sanitize(configured)
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return Sanitize(h)
	}
	return "unknown"
}

// ScaleSetNameFor names the scale set for (repo × class):
// "gso-<instance>-<repoShort>-<os>-<arch>". Names longer than MaxNameLen
// become "gso-<inst≤10>-<repo-trunc>-<hash4>-<class>", where hash4 keeps
// repos with a common truncated prefix apart (design spec §6.1).
func ScaleSetNameFor(instance, repo string, class runner.Class) string {
	inst := Sanitize(instance)
	short := Sanitize(RepoShortName(repo))
	cls := class.String()
	if name := ScaleSetNamePrefix + "-" + inst + "-" + short + "-" + cls; len(name) <= MaxNameLen {
		return name
	}
	sum := sha256.Sum256([]byte(inst + "/" + strings.ToLower(repo)))
	hash4 := hex.EncodeToString(sum[:])[:4]
	inst = trimTo(inst, maxOverflowInstance)
	budget := MaxNameLen - len(ScaleSetNamePrefix+"-") - len(inst) - 1 - 1 - len(hash4) - 1 - len(cls)
	short = trimTo(short, budget)
	return fmt.Sprintf("%s-%s-%s-%s-%s", ScaleSetNamePrefix, inst, short, hash4, cls)
}

// HasClassSuffix reports whether name ends in "-<os>-<arch>" of a supported
// class, the shape of every instance-era scale-set name (ScaleSetNameFor).
func HasClassSuffix(name string) bool {
	for _, c := range runner.AllClasses {
		if strings.HasSuffix(name, "-"+c.String()) {
			return true
		}
	}
	return false
}

// ScaleSetPrefix is the prefix of every non-overflow scale-set name of an
// instance. Overflow names truncate the instance to 10 characters.
func ScaleSetPrefix(instance string) string {
	return ScaleSetNamePrefix + "-" + Sanitize(instance) + "-"
}

// WorkerSegment is the worker's part of its runner names: the sanitised
// name without hyphens, at most 12 characters. Removing hyphens makes the
// segment parseable (RunnerWorkerSegment).
func WorkerSegment(workerName string) string {
	seg := strings.ReplaceAll(Sanitize(workerName), "-", "")
	if len(seg) > maxWorkerSegment {
		seg = seg[:maxWorkerSegment]
	}
	if seg == "" {
		return "x"
	}
	return seg
}

// RunnerName names a runner placed on the worker with the given segment:
// "gso-<segment>-<repo≤14>-<6 random>". The router binds a reported runner
// to a worker by this segment (design spec §4.3).
func RunnerName(segment, repo string) string {
	short := trimTo(Sanitize(RepoShortName(repo)), maxRunnerRepo)
	// rand.Text is base32 (A-Z, 2-7); lowercased it stays within [a-z0-9].
	suffix := strings.ToLower(rand.Text()[:runnerSuffixLen])
	return ScaleSetNamePrefix + "-" + WorkerSegment(segment) + "-" + short + "-" + suffix
}

// RunnerWorkerSegment returns the worker segment of a runner name made by
// RunnerName.
func RunnerWorkerSegment(runnerName string) (string, bool) {
	rest, ok := strings.CutPrefix(runnerName, ScaleSetNamePrefix+"-")
	if !ok {
		return "", false
	}
	seg, _, ok := strings.Cut(rest, "-")
	if !ok || seg == "" {
		return "", false
	}
	return seg, true
}

func trimTo(s string, n int) string {
	if n < 1 {
		return "x"
	}
	if len(s) > n {
		s = s[:n]
	}
	if s = strings.TrimRight(s, "-"); s == "" {
		return "x"
	}
	return s
}
