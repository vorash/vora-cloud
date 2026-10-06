package project

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"

	"github.com/vorash/vora-cloud/pkg/deploy"
	"github.com/vorash/vora-cloud/pkg/secrets"
)

const (
	// maxSlugLen keeps "p-<slug>", the namespace, within a 63 character DNS label.
	maxSlugLen = 61

	defaultPort      = 80
	defaultReplicas  = 1
	defaultCPUMillis = 250
	defaultMemoryMiB = 256

	// rolloutTimeout is how long a deployment may stay in "deploying".
	rolloutTimeout = 10 * time.Minute
)

// Service applies the ownership rule and the deployment life cycle on top of a
// Store and the per-region deployers. Every method that takes an owner treats
// another user's project as missing.
type Service struct {
	store   Store
	regions *deploy.Registry
	box     *secrets.Box
	// appsDomain is the base of app hosts, "<slug>.<region>.<appsDomain>".
	// Empty means apps are not exposed.
	appsDomain string
	// observer hears of every change of a deployment's status; nil hears nothing.
	observer Observer
}

func NewService(store Store, regions *deploy.Registry, box *secrets.Box, appsDomain string) *Service {
	return &Service{store: store, regions: regions, box: box, appsDomain: appsDomain}
}

// CreateInput is what a user chooses when creating a project. Zero values
// fall back to defaults.
type CreateInput struct {
	Name          string
	Slug          string
	Region        string
	Repo          string
	DefaultBranch string
	// GitHubInstallationID and GitHubRepoID link the project to a repository.
	// Set them only for a repository that was verified to belong to the owner's
	// installation; no API request carries them.
	GitHubInstallationID int64
	GitHubRepoID         int64
	BuildConfig          []byte
	Port                 int32
	Replicas             int32
	CPUMillis            int64
	MemoryMiB            int64
	HealthPath           string
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// CreateProject creates a project for owner. The region must be configured.
func (s *Service) CreateProject(ctx context.Context, owner uuid.UUID, in CreateInput) (*Project, error) {
	if in.Name == "" {
		return nil, invalid("name is required")
	}
	if len(in.Slug) > maxSlugLen || !deploy.ValidLabel(in.Slug) {
		return nil, invalid("slug %q must be a lowercase DNS label of at most %d characters", in.Slug, maxSlugLen)
	}
	if _, err := s.regions.Deployer(in.Region); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if _, err := ParseBuildSettings(in.BuildConfig); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if (in.GitHubInstallationID == 0) != (in.GitHubRepoID == 0) || in.GitHubInstallationID < 0 || in.GitHubRepoID < 0 {
		return nil, invalid("a GitHub link needs both an installation and a repository")
	}
	p := &Project{
		OwnerID: owner, Name: in.Name, Slug: in.Slug, Region: in.Region,
		Repo: in.Repo, DefaultBranch: in.DefaultBranch, BuildConfig: in.BuildConfig,
		GitHubInstallationID: in.GitHubInstallationID, GitHubRepoID: in.GitHubRepoID,
		Port: in.Port, Replicas: in.Replicas, CPUMillis: in.CPUMillis, MemoryMiB: in.MemoryMiB,
		HealthPath: in.HealthPath,
	}
	if p.DefaultBranch == "" {
		p.DefaultBranch = "main"
	}
	if len(p.BuildConfig) == 0 {
		p.BuildConfig = []byte("{}")
	}
	if p.Port == 0 {
		p.Port = defaultPort
	}
	if p.Replicas == 0 {
		p.Replicas = defaultReplicas
	}
	if p.CPUMillis == 0 {
		p.CPUMillis = defaultCPUMillis
	}
	if p.MemoryMiB == 0 {
		p.MemoryMiB = defaultMemoryMiB
	}
	if err := s.spec(p, "placeholder").Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := s.store.CreateProject(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Service) ListProjects(ctx context.Context, owner uuid.UUID) ([]Project, error) {
	return s.store.ListProjects(ctx, owner)
}

// ListSummaries lists the owner's projects, each with its newest deployment.
// A deployment is matched only to a project in the owner's own list.
func (s *Service) ListSummaries(ctx context.Context, owner uuid.UUID) ([]Summary, error) {
	projects, err := s.store.ListProjects(ctx, owner)
	if err != nil {
		return nil, err
	}
	latest, err := s.store.LatestDeployments(ctx, owner)
	if err != nil {
		return nil, err
	}
	out := make([]Summary, len(projects))
	for i, p := range projects {
		out[i].Project = p
		if d, ok := latest[p.ID]; ok {
			out[i].Latest = &d
		}
	}
	return out, nil
}

// owned loads a project and hides it from everyone but its owner.
func (s *Service) owned(ctx context.Context, owner, id uuid.UUID) (*Project, error) {
	p, err := s.store.GetProject(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.OwnerID != owner {
		return nil, ErrNotFound
	}
	return p, nil
}

func (s *Service) GetProject(ctx context.Context, owner, id uuid.UUID) (*Project, error) {
	return s.owned(ctx, owner, id)
}

// DeleteProject tears the project's namespace down, then removes the record.
// If the cluster call fails the record stays, so the delete can be retried.
func (s *Service) DeleteProject(ctx context.Context, owner, id uuid.UUID) error {
	p, err := s.owned(ctx, owner, id)
	if err != nil {
		return err
	}
	deployer, err := s.regions.Deployer(p.Region)
	if err != nil {
		return err
	}
	if err := deployer.DeleteProject(ctx, p.Slug); err != nil {
		return fmt.Errorf("delete project in %s: %w", p.Region, err)
	}
	return s.store.DeleteProject(ctx, id)
}

// ListDeployments returns one page of the project's deployments, newest first.
func (s *Service) ListDeployments(ctx context.Context, owner, project uuid.UUID, page Page) (DeploymentPage, error) {
	if _, err := s.owned(ctx, owner, project); err != nil {
		return DeploymentPage{}, err
	}
	limit := page.Limit
	switch {
	case limit < 0:
		return DeploymentPage{}, invalid("limit must not be negative")
	case limit == 0:
		limit = DefaultPageSize
	case limit > MaxPageSize:
		limit = MaxPageSize
	}
	var after *Cursor
	if page.Before != "" {
		c, err := ParseCursor(page.Before)
		if err != nil {
			return DeploymentPage{}, err
		}
		after = &c
	}
	// One more than asked for tells whether an older page exists.
	deps, err := s.store.ListDeployments(ctx, project, after, limit+1)
	if err != nil {
		return DeploymentPage{}, err
	}
	out := DeploymentPage{Deployments: deps}
	if len(deps) > limit {
		out.Deployments = deps[:limit]
		last := deps[limit-1]
		out.Next = Cursor{CreatedAt: last.CreatedAt, ID: last.ID}.String()
	}
	return out, nil
}

// GetDeployment loads a deployment through its project's owner.
func (s *Service) GetDeployment(ctx context.Context, owner, id uuid.UUID) (*Deployment, error) {
	d, err := s.store.GetDeployment(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := s.owned(ctx, owner, d.ProjectID); err != nil {
		return nil, err
	}
	return d, nil
}

// DeployInput describes what to run: an image that already exists.
type DeployInput struct {
	Image     string
	CommitSHA string
	Ref       string
}

// Deploy runs an existing image for the project as a new deployment. The
// result is the deployment record; a cluster error is recorded on it as
// "failed" instead of being returned, so the caller always has a record.
func (s *Service) Deploy(ctx context.Context, owner, project uuid.UUID, in DeployInput) (*Deployment, error) {
	p, err := s.owned(ctx, owner, project)
	if err != nil {
		return nil, err
	}
	if in.Image == "" {
		return nil, invalid("image is required")
	}
	return s.deploy(ctx, p, in)
}

// Redeploy runs the project's latest image again, which also picks up changed
// environment variables.
func (s *Service) Redeploy(ctx context.Context, owner, project uuid.UUID) (*Deployment, error) {
	p, err := s.owned(ctx, owner, project)
	if err != nil {
		return nil, err
	}
	latest, err := s.latestWithImage(ctx, project)
	if err != nil {
		return nil, err
	}
	if latest == nil {
		return nil, invalid("project has no deployment with an image to redeploy")
	}
	return s.deploy(ctx, p, DeployInput{Image: latest.Image, CommitSHA: latest.CommitSHA, Ref: latest.Ref})
}

// CanRedeploy says whether the project has an image to run again.
func (s *Service) CanRedeploy(ctx context.Context, owner, project uuid.UUID) (bool, error) {
	if _, err := s.owned(ctx, owner, project); err != nil {
		return false, err
	}
	latest, err := s.latestWithImage(ctx, project)
	return latest != nil, err
}

// latestWithImage is the project's newest deployment that has an image, or nil.
// A deployment queued by a push has none until it is built, so the newest
// deployment is not always the one to run again.
func (s *Service) latestWithImage(ctx context.Context, project uuid.UUID) (*Deployment, error) {
	const pageSize, maxPages = 50, 20
	var after *Cursor
	for range maxPages {
		deps, err := s.store.ListDeployments(ctx, project, after, pageSize) // newest first
		if err != nil {
			return nil, err
		}
		for i := range deps {
			if deps[i].Image != "" {
				return &deps[i], nil
			}
		}
		if len(deps) < pageSize {
			return nil, nil
		}
		last := deps[len(deps)-1]
		after = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return nil, nil
}

// Rollback deploys the image of an earlier deployment that became ready. The
// target must belong to the same project: an id from another project is
// reported as missing.
func (s *Service) Rollback(ctx context.Context, owner, project, target uuid.UUID) (*Deployment, error) {
	p, err := s.owned(ctx, owner, project)
	if err != nil {
		return nil, err
	}
	old, err := s.store.GetDeployment(ctx, target)
	if err != nil {
		return nil, err
	}
	if old.ProjectID != p.ID {
		return nil, ErrNotFound
	}
	if old.Status != StatusReady {
		return nil, invalid("deployment %s never became ready, so it is not a rollback target", old.ID)
	}
	if old.ImagePrunedAt != nil {
		return nil, invalid("the image of deployment %s was removed to save space, so it is not a rollback target", old.ID)
	}
	return s.deploy(ctx, p, DeployInput{Image: old.Image, CommitSHA: old.CommitSHA, Ref: old.Ref})
}

// PushInput is a push to a repository's default branch, as GitHub reported it.
type PushInput struct {
	InstallationID, RepoID int64
	Ref, CommitSHA         string
}

// Queued is a project and the deployment queued for it.
type Queued struct {
	Project    Project
	Deployment Deployment
}

// QueueFromPush queues a deployment of the pushed commit for every project tied
// to the repository by GitHub's installation and repository ids. It matches on
// the ids and on nothing else: the free-text repo of a project is a label that
// anyone can write, and routing on it would let a user claim someone else's
// repository. The deployment has no image yet; building one comes later, and
// until it exists the deployment stays queued. A newer push replaces an older
// queued one, which is failed as superseded, so redelivering a push or pushing
// twice in a row leaves one queued deployment.
func (s *Service) QueueFromPush(ctx context.Context, in PushInput) ([]Queued, error) {
	if in.InstallationID <= 0 || in.RepoID <= 0 || in.CommitSHA == "" {
		return nil, invalid("a push needs an installation, a repository and a commit")
	}
	projects, err := s.store.ListProjectsByGitHub(ctx, in.InstallationID, in.RepoID)
	if err != nil {
		return nil, err
	}
	var out []Queued
	for _, p := range projects {
		d := &Deployment{ProjectID: p.ID, CommitSHA: in.CommitSHA, Ref: in.Ref, Status: StatusQueued, Region: p.Region}
		if err := s.store.CreateDeployment(ctx, d); err != nil {
			return out, err
		}
		s.supersedeQueued(ctx, p.ID, d.ID)
		out = append(out, Queued{Project: p, Deployment: *d})
	}
	return out, nil
}

// supersedeQueued fails the project's older deployments that are still only
// queued. It leaves the ones that are building or deploying alone: they are
// serving, or about to.
func (s *Service) supersedeQueued(ctx context.Context, project, keep uuid.UUID) {
	deps, err := s.store.ListActive(ctx)
	if err != nil {
		log.Printf("project %s: list active deployments to supersede: %v", project, err)
		return
	}
	for i := range deps {
		if deps[i].ProjectID == project && deps[i].ID != keep && deps[i].Status == StatusQueued {
			if err := s.move(ctx, &deps[i], StatusFailed, "superseded by a newer push"); err != nil && !errors.Is(err, ErrConflict) {
				log.Printf("deployment %s: supersede: %v", deps[i].ID, err)
			}
		}
	}
}

func (s *Service) deploy(ctx context.Context, p *Project, in DeployInput) (*Deployment, error) {
	deployer, err := s.regions.Deployer(p.Region)
	if err != nil {
		return nil, err
	}
	env, err := s.env(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	spec := s.spec(p, in.Image)
	spec.Env = env
	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}

	d := &Deployment{
		ProjectID: p.ID, CommitSHA: in.CommitSHA, Ref: in.Ref, Status: StatusQueued,
		Image: in.Image, Region: p.Region,
	}
	if spec.Host != "" {
		d.URL = "https://" + spec.Host
	}
	if err := s.store.CreateDeployment(ctx, d); err != nil {
		return nil, err
	}
	return s.rollout(ctx, deployer, p, d, spec)
}

// rollout applies a deployment's spec and moves it on to "deploying", or to
// "failed" if the cluster refuses it. It serves a deployment made from an image
// (queued) and one that was built (pushing).
func (s *Service) rollout(ctx context.Context, deployer deploy.Deployer, p *Project, d *Deployment, spec deploy.Spec) (*Deployment, error) {
	// Apply first and move to "deploying" after. The reconciler reads the
	// cluster's rollout, and until the new spec is applied that is the old
	// rollout's status: a "deploying" row could be marked ready for it.
	if err := deployer.Deploy(ctx, spec); err != nil {
		log.Printf("deployment %s: apply to %s: %v", d.ID, p.Region, err)
		if err := s.move(ctx, d, StatusFailed, deployFailure(d, err)); err != nil {
			return nil, err
		}
		return d, nil
	}
	if err := s.move(ctx, d, StatusDeploying, ""); err != nil {
		return nil, err
	}
	s.supersede(ctx, p.ID, d.ID)
	return d, nil
}

// supersede fails the project's older unfinished deployments: the cluster runs
// only the newest spec, so an older one would otherwise be reported ready for
// a rollout that never carried its image.
func (s *Service) supersede(ctx context.Context, project, keep uuid.UUID) {
	// ListActive, not a page of the project's history: an unfinished
	// deployment must be found however many newer ones there are.
	deps, err := s.store.ListActive(ctx)
	if err != nil {
		log.Printf("project %s: list active deployments to supersede: %v", project, err)
		return
	}
	for i := range deps {
		if deps[i].ProjectID == project && deps[i].ID != keep {
			if err := s.move(ctx, &deps[i], StatusFailed, "superseded by a newer deployment"); err != nil && !errors.Is(err, ErrConflict) {
				log.Printf("deployment %s: supersede: %v", deps[i].ID, err)
			}
		}
	}
}

// move applies a legal status change to d and to the store.
func (s *Service) move(ctx context.Context, d *Deployment, to Status, errText string) error {
	if !CanMove(d.Status, to) {
		return fmt.Errorf("deployment %s: illegal move %s -> %s", d.ID, d.Status, to)
	}
	if err := s.store.Move(ctx, d.ID, d.Status, to, errText); err != nil {
		return err
	}
	d.Status, d.Error = to, errText
	s.notify(ctx, d)
	return nil
}

// spec maps a project and an image to what the cluster runs.
func (s *Service) spec(p *Project, image string) deploy.Spec {
	spec := deploy.Spec{
		Project: p.Slug, Name: p.Slug, Image: image,
		Port: p.Port, Replicas: p.Replicas, CPUMillis: p.CPUMillis, MemoryMiB: p.MemoryMiB,
		HealthPath: p.HealthPath,
	}
	if s.appsDomain != "" {
		spec.Host = p.Slug + "." + p.Region + "." + s.appsDomain
		spec.TLS = true
	}
	return spec
}

// Reconcile settles deployments that are still "deploying" by asking the
// cluster how the rollout is going. It is safe to call repeatedly, and it
// also picks up deployments left behind by a restart.
func (s *Service) Reconcile(ctx context.Context) {
	active, err := s.store.ListActive(ctx)
	if err != nil {
		log.Printf("reconcile: list active deployments: %v", err)
		return
	}
	for i := range active {
		d := &active[i]
		if d.Status != StatusDeploying {
			continue
		}
		if err := s.reconcileOne(ctx, d); err != nil && !errors.Is(err, ErrConflict) {
			log.Printf("reconcile deployment %s: %v", d.ID, err)
		}
	}
}

func (s *Service) reconcileOne(ctx context.Context, d *Deployment) error {
	p, err := s.store.GetProject(ctx, d.ProjectID)
	if err != nil {
		return err
	}
	deployer, err := s.regions.Deployer(d.Region)
	if err != nil {
		return err
	}
	st, err := deployer.Status(ctx, p.Slug, p.Slug)
	if err != nil {
		return err // the cluster may be briefly unreachable; try again later
	}
	switch st.State {
	case deploy.StateReady:
		return s.move(ctx, d, StatusReady, "")
	case deploy.StateFailed:
		log.Printf("deployment %s: rollout failed: %s: %s", d.ID, st.Reason, st.Message)
		return s.move(ctx, d, StatusFailed, rolloutFailure(d, st))
	}
	// Pending, rolling out, or not visible yet.
	if time.Since(d.UpdatedAt) > rolloutTimeout {
		return s.move(ctx, d, StatusFailed, fmt.Sprintf("rollout did not finish within %s", rolloutTimeout))
	}
	return nil
}

// Run reconciles every interval until ctx is done.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.Reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SetEnv stores an environment variable, encrypted. It applies on the next
// deployment.
func (s *Service) SetEnv(ctx context.Context, owner, project uuid.UUID, name, value string) error {
	if _, err := s.owned(ctx, owner, project); err != nil {
		return err
	}
	if !deploy.ValidEnvName(name) {
		return invalid("env name %q is not a valid variable name", name)
	}
	sealed, err := s.box.Seal(value)
	if err != nil {
		return err
	}
	return s.store.SetEnv(ctx, project, name, sealed)
}

// EnvNames lists variable names. Values are never returned.
func (s *Service) EnvNames(ctx context.Context, owner, project uuid.UUID) ([]string, error) {
	if _, err := s.owned(ctx, owner, project); err != nil {
		return nil, err
	}
	sealed, err := s.store.ListEnv(ctx, project)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(sealed))
	for n := range sealed {
		names = append(names, n)
	}
	return names, nil
}

func (s *Service) DeleteEnv(ctx context.Context, owner, project uuid.UUID, name string) error {
	if _, err := s.owned(ctx, owner, project); err != nil {
		return err
	}
	return s.store.DeleteEnv(ctx, project, name)
}

func (s *Service) env(ctx context.Context, project uuid.UUID) (map[string]string, error) {
	sealed, err := s.store.ListEnv(ctx, project)
	if err != nil {
		return nil, err
	}
	env := make(map[string]string, len(sealed))
	for name, b := range sealed {
		v, err := s.box.Open(b)
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", name, err)
		}
		env[name] = v
	}
	return env, nil
}
