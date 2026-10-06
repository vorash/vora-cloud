package githubapp

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/vorash/vora-cloud/pkg/project"
)

// PushQueue queues deployments for a push (project.Service).
type PushQueue interface {
	QueueFromPush(ctx context.Context, in project.PushInput) ([]project.Queued, error)
}

// StatusPoster reports on a commit (App). Nil is allowed: nothing is reported.
type StatusPoster interface {
	PostStatus(ctx context.Context, installationID int64, repoFullName, sha string, s CommitStatus) error
}

// statusTimeout bounds the posting of one status.
const statusTimeout = 30 * time.Second

// PushDeployments handles a push to a default branch: it queues a deployment for
// each project tied to the repository, then reports on the commit. The queueing is
// what the webhook waits for, because GitHub gives a delivery ten seconds. Reports
// are made after the answer, one at a time and in order, so that a late "building"
// cannot overwrite a "ready"; a failure to post one is logged, never retried
// through the webhook: the deployments exist either way, and a redelivery would
// only queue them again.
type PushDeployments struct {
	queue     PushQueue
	statuses  StatusPoster
	publicURL string
	builds    bool

	once    sync.Once
	jobs    chan func(context.Context)
	reports sync.WaitGroup
}

// NewPushDeployments returns the handler. publicURL is the dashboard's origin,
// for the link on a status; empty leaves the link out.
func NewPushDeployments(queue PushQueue, statuses StatusPoster, publicURL string) *PushDeployments {
	return &PushDeployments{queue: queue, statuses: statuses, publicURL: publicURL}
}

// WithBuilds says that queued deployments are built, so a queued commit is
// described as waiting for a build and not as something that cannot happen.
func (p *PushDeployments) WithBuilds() *PushDeployments {
	p.builds = true
	return p
}

// Wait returns once the reports made so far are done.
func (p *PushDeployments) Wait() { p.reports.Wait() }

// report queues a status to be posted. It never blocks: a full queue drops the
// status and says so.
func (p *PushDeployments) report(f func(context.Context)) {
	p.once.Do(func() {
		p.jobs = make(chan func(context.Context), 256)
		go func() {
			for job := range p.jobs {
				ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
				job(ctx)
				cancel()
				p.reports.Done()
			}
		}()
	})
	p.reports.Add(1)
	select {
	case p.jobs <- f:
	default:
		p.reports.Done()
		log.Printf("github push: too many statuses waiting; one was dropped")
	}
}

func (p *PushDeployments) link(projectID uuid.UUID) string {
	if p.publicURL == "" {
		return ""
	}
	return p.publicURL + "/projects/" + projectID.String()
}

func (p *PushDeployments) HandlePush(ctx context.Context, push Push) error {
	queued, err := p.queue.QueueFromPush(ctx, project.PushInput{
		InstallationID: push.InstallationID, RepoID: push.RepoID, Ref: push.Ref, CommitSHA: push.CommitSHA,
	})
	if err != nil {
		return err
	}
	for _, q := range queued {
		log.Printf("github push: queued deployment %s of project %s for %s@%.7s", q.Deployment.ID, q.Project.Slug, push.RepoFullName, push.CommitSHA)
	}
	if p.statuses == nil || len(queued) == 0 {
		return nil
	}
	description := "Queued. Builds are not available yet."
	if p.builds {
		description = "Queued for a build."
	}
	for _, q := range queued {
		s := CommitStatus{State: "pending", Context: "vora/" + q.Project.Slug, Description: description, TargetURL: p.link(q.Project.ID)}
		p.report(func(ctx context.Context) {
			if err := p.statuses.PostStatus(ctx, push.InstallationID, push.RepoFullName, push.CommitSHA, s); err != nil {
				log.Printf("github push: reporting project %s: %v", q.Project.Slug, err)
			}
		})
	}
	return nil
}

// statusOf is the commit status for a deployment that has just changed.
func statusOf(d project.Deployment, slug string) (CommitStatus, bool) {
	s := CommitStatus{Context: "vora/" + slug}
	switch d.Status {
	case project.StatusBuilding:
		s.State, s.Description = "pending", "Building the image."
	case project.StatusPushing:
		s.State, s.Description = "pending", "Pushing the image."
	case project.StatusDeploying:
		s.State, s.Description = "pending", "Deploying."
	case project.StatusReady:
		s.State, s.Description = "success", "Deployed."
	case project.StatusFailed:
		s.State, s.Description = "failure", "Failed: "+strings.Join(strings.Fields(d.Error), " ")
	default:
		return CommitStatus{}, false
	}
	return s, true
}

// DeploymentChanged reports a deployment's new status on its commit. It is
// the project service's observer: a deployment made by hand, from an image, has
// no commit and is not reported.
func (p *PushDeployments) DeploymentChanged(_ context.Context, d project.Deployment, pr project.Project) {
	if p.statuses == nil || d.CommitSHA == "" || pr.GitHubRepoID == 0 || pr.Repo == "" {
		return
	}
	s, ok := statusOf(d, pr.Slug)
	if !ok {
		return
	}
	s.TargetURL = p.link(pr.ID)
	p.report(func(ctx context.Context) {
		if err := p.statuses.PostStatus(ctx, pr.GitHubInstallationID, pr.Repo, d.CommitSHA, s); err != nil {
			log.Printf("github deployment: reporting project %s: %v", pr.Slug, err)
		}
	})
}
