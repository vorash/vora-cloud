package githubapp

import (
	"context"
	"log"
	"sync"
	"time"

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

// statusTimeout bounds the reporting of one push.
const statusTimeout = 30 * time.Second

// PushDeployments handles a push to a default branch: it queues a deployment
// for each project tied to the repository, then reports on the commit. The
// queueing is what the webhook waits for, because GitHub gives a delivery ten
// seconds. The report is made after the answer, and a failure of it is logged,
// never retried through the webhook: the deployments exist either way, and a
// redelivery would only queue them again.
type PushDeployments struct {
	queue     PushQueue
	statuses  StatusPoster
	publicURL string
	reports   sync.WaitGroup
}

// NewPushDeployments returns the handler. publicURL is the dashboard's origin,
// for the link on a status; empty leaves the link out.
func NewPushDeployments(queue PushQueue, statuses StatusPoster, publicURL string) *PushDeployments {
	return &PushDeployments{queue: queue, statuses: statuses, publicURL: publicURL}
}

// Wait returns once the reports of the pushes handled so far are done.
func (p *PushDeployments) Wait() { p.reports.Wait() }

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
	p.reports.Add(1)
	go func() {
		defer p.reports.Done()
		// The request that carried the push is over by now.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statusTimeout)
		defer cancel()
		for _, q := range queued {
			s := CommitStatus{State: "pending", Context: "vora/" + q.Project.Slug, Description: "Queued. Builds are not available yet."}
			if p.publicURL != "" {
				s.TargetURL = p.publicURL + "/projects/" + q.Project.ID.String()
			}
			if err := p.statuses.PostStatus(ctx, push.InstallationID, push.RepoFullName, push.CommitSHA, s); err != nil {
				log.Printf("github push: reporting project %s: %v", q.Project.Slug, err)
			}
		}
	}()
	return nil
}
