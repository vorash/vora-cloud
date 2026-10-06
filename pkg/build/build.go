// Package build turns a queued deployment into an image: it runs one build at a
// time as a Job in the cluster (ADR 006), reads its log, and hands the pushed
// image to the project service to deploy.
package build

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/vorash/vora-cloud/pkg/project"
)

// Spec is one build. Every field ends up in a build's arguments or its Job, so
// each is checked: a comma in a reference, for one, would add an option to the
// output.
type Spec struct {
	// ID is the deployment's id; the build's Job and Secret are named after it.
	ID string
	// Repo is "owner/name" on GitHub, and CommitSHA the commit to build.
	Repo, CommitSHA string
	// Context is the folder of the repository to build in ("" is the root) and
	// Dockerfile the path of the Dockerfile inside it.
	Context, Dockerfile string
	// PushRef is where BuildKit pushes the image, and CacheRef where it keeps its
	// cache. ImportCache says the cache exists: asking for one that does not makes
	// BuildKit log an error, although the build goes on.
	PushRef, CacheRef string
	ImportCache       bool
	// Token reads this one repository. It is given to the build's Secret and to nothing else.
	Token string
}

var (
	repoRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	commitRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	// host[:port]/path[:tag], with nothing a BuildKit option list could take apart.
	refRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?/[a-z0-9]([a-z0-9._/-]*[a-z0-9])?:[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
)

// Validate refuses a spec that could do more than build.
func (s Spec) Validate() error {
	if _, err := uuid.Parse(s.ID); err != nil {
		return fmt.Errorf("build: %q is not a deployment id", s.ID)
	}
	switch {
	case !repoRe.MatchString(s.Repo):
		return fmt.Errorf("build: %q is not a repository name", s.Repo)
	case !commitRe.MatchString(s.CommitSHA):
		return fmt.Errorf("build: %q is not a commit id", s.CommitSHA)
	case s.Context != "" && !project.ValidBuildPath(s.Context):
		return fmt.Errorf("build: %q is not a build context", s.Context)
	case !project.ValidBuildPath(s.Dockerfile):
		return fmt.Errorf("build: %q is not a Dockerfile path", s.Dockerfile)
	case !refRe.MatchString(s.PushRef):
		return fmt.Errorf("build: %q is not an image reference", s.PushRef)
	case !refRe.MatchString(s.CacheRef):
		return fmt.Errorf("build: %q is not an image reference", s.CacheRef)
	case s.Token == "":
		return errors.New("build: no token")
	}
	return nil
}

// State is where a build is.
type State int

const (
	Running State = iota
	Succeeded
	Failed
	// Missing means the cluster has no such build: it was never started, or was cleaned away.
	Missing
)

// Result is what the cluster says about a build.
type Result struct {
	State State
	// Reason says why it failed, as the cluster puts it.
	Reason string
	// StartedAt is when the build's Job started; zero while it has not.
	StartedAt time.Time
}

// Runner runs builds in one region's cluster.
type Runner interface {
	// Start creates the build's Secret and Job. It fails if either exists.
	Start(ctx context.Context, s Spec) error
	Status(ctx context.Context, id string) (Result, error)
	// Logs returns the end of the build's log, at most max bytes; empty if it has none yet.
	Logs(ctx context.Context, id string, max int) (string, error)
	// Cleanup removes the build's Job and Secret. It is safe to repeat.
	Cleanup(ctx context.Context, id string) error
	// Sweep removes every build's Job and Secret that keep refuses: what a
	// restart left behind.
	Sweep(ctx context.Context, keep func(id string) bool) error
}

// Registry says what a build pushed.
type Registry interface {
	// Digest returns the digest ("sha256:...") of repo:tag, or ErrNoImage.
	Digest(ctx context.Context, repo, tag string) (string, error)
}

// ErrNoImage means the registry has no such image.
var ErrNoImage = errors.New("build: the registry has no such image")

// Tokens mints the token a build reads its repository with.
type Tokens interface {
	RepoToken(ctx context.Context, installationID, repoID int64) (string, error)
}

// Deployments is the part of the project service the orchestrator drives.
type Deployments interface {
	PendingBuilds(ctx context.Context) ([]project.Build, error)
	StartBuild(ctx context.Context, id uuid.UUID) error
	BeginPush(ctx context.Context, id uuid.UUID) error
	FinishBuild(ctx context.Context, id uuid.UUID, image string) error
	FailBuild(ctx context.Context, id uuid.UUID, reason string) error
	RecordBuildLog(ctx context.Context, id uuid.UUID, log string) error
}

var (
	tokenRe = regexp.MustCompile(`(?:gh[opsur]_|github_pat_)[A-Za-z0-9_]{16,}`)
	authRe  = regexp.MustCompile(`(x-access-token:)[^@\s/]+@`)
)

// Redact removes anything in a log that looks like a GitHub token. A token never
// goes into an argument or the environment of a build, so none should be there;
// this is for the day one is.
func Redact(log string) string {
	return authRe.ReplaceAllString(tokenRe.ReplaceAllString(log, "***"), "${1}***@")
}

func parseID(id string) (uuid.UUID, error) { return uuid.Parse(id) }
