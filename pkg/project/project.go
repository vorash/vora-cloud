// Package project holds projects and their deployments: who owns what, the
// deployment state machine, and the service that applies a deployment to the
// project's region.
package project

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrNotFound is returned for a missing project or deployment, and also for
	// one that belongs to another user, so ids cannot be probed.
	ErrNotFound = errors.New("not found")
	// ErrSlugTaken means another project already uses the slug.
	ErrSlugTaken = errors.New("slug is already taken")
	// ErrInvalid wraps a request that can never succeed as sent.
	ErrInvalid = errors.New("invalid request")
	// ErrConflict means a deployment changed state under the caller.
	ErrConflict = errors.New("deployment changed state concurrently")
)

// Status is where a deployment is in its life. It is separate from
// deploy.State, which describes only the rollout in the cluster.
type Status string

const (
	StatusQueued    Status = "queued"
	StatusBuilding  Status = "building"
	StatusPushing   Status = "pushing"
	StatusDeploying Status = "deploying"
	StatusReady     Status = "ready"
	StatusFailed    Status = "failed"
)

// next lists the legal moves. Deploying an existing image skips building and
// pushing. Any deployment that has not finished can fail; ready and failed
// are final.
var next = map[Status][]Status{
	StatusQueued:    {StatusBuilding, StatusDeploying, StatusFailed},
	StatusBuilding:  {StatusPushing, StatusFailed},
	StatusPushing:   {StatusDeploying, StatusFailed},
	StatusDeploying: {StatusReady, StatusFailed},
}

// CanMove reports whether a deployment may go from one status to another.
func CanMove(from, to Status) bool {
	for _, s := range next[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Final reports whether the status can no longer change.
func (s Status) Final() bool { return s == StatusReady || s == StatusFailed }

// Project is an app owned by one user. Its slug names the namespace and the
// public host, so it is unique across all owners.
type Project struct {
	ID      uuid.UUID
	OwnerID uuid.UUID
	Name    string
	Slug    string
	// Repo is a label: the API lets a user write anything there, so it never
	// decides which project a push belongs to. Only the two ids below do.
	Repo          string
	DefaultBranch string
	// GitHubInstallationID and GitHubRepoID tie the project to a GitHub
	// repository, both set or both zero. They are set only by the dashboard,
	// after it checked that the repository is one of the owner's own
	// installation's.
	GitHubInstallationID int64
	GitHubRepoID         int64
	Region               string
	BuildConfig          []byte // JSON, opaque until the build pipeline exists
	Port                 int32
	Replicas             int32
	CPUMillis            int64
	MemoryMiB            int64
	HealthPath           string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// Deployment is one attempt to run an image for a project.
type Deployment struct {
	ID         uuid.UUID
	ProjectID  uuid.UUID
	CommitSHA  string
	Ref        string
	Status     Status
	Image      string
	Region     string
	URL        string
	Error      string
	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
	UpdatedAt  time.Time
}

// Summary is a project with its newest deployment, nil if it has none.
type Summary struct {
	Project
	Latest *Deployment
}

// Store persists projects, deployments and encrypted env vars. Lookups by id
// are not scoped to an owner: the Service checks ownership, so the rule lives
// in one place and is covered by tests without a database.
type Store interface {
	CreateProject(ctx context.Context, p *Project) error // ErrSlugTaken
	GetProject(ctx context.Context, id uuid.UUID) (*Project, error)
	ListProjects(ctx context.Context, owner uuid.UUID) ([]Project, error)
	DeleteProject(ctx context.Context, id uuid.UUID) error
	// ListProjectsByGitHub returns the projects tied to a repository by GitHub's
	// ids, whoever owns them.
	ListProjectsByGitHub(ctx context.Context, installationID, repoID int64) ([]Project, error)

	CreateDeployment(ctx context.Context, d *Deployment) error
	GetDeployment(ctx context.Context, id uuid.UUID) (*Deployment, error)
	// ListDeployments returns at most limit of a project's deployments, newest
	// first, starting after the cursor (the newest if nil).
	ListDeployments(ctx context.Context, project uuid.UUID, after *Cursor, limit int) ([]Deployment, error)
	// LatestDeployments returns the newest deployment of each of the owner's
	// projects that has one, by project id.
	LatestDeployments(ctx context.Context, owner uuid.UUID) (map[uuid.UUID]Deployment, error)
	// ListActive returns deployments that have not reached a final status.
	ListActive(ctx context.Context) ([]Deployment, error)
	// SetImage records the image a built deployment runs, and the address it will
	// have. It does not change the status.
	SetImage(ctx context.Context, id uuid.UUID, image, url string) error
	// BuildLog and SetBuildLog read and replace a deployment's build log. The
	// log is not part of Deployment, so listing deployments never carries it.
	BuildLog(ctx context.Context, id uuid.UUID) (string, error)
	SetBuildLog(ctx context.Context, id uuid.UUID, log string) error
	// Move changes the status only if it is still "from" (ErrConflict
	// otherwise), and records the error text and finish time.
	Move(ctx context.Context, id uuid.UUID, from, to Status, errText string) error

	SetEnv(ctx context.Context, project uuid.UUID, name string, sealed []byte) error
	ListEnv(ctx context.Context, project uuid.UUID) (map[string][]byte, error)
	DeleteEnv(ctx context.Context, project uuid.UUID, name string) error
}
