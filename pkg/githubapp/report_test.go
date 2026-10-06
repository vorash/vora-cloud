package githubapp

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vorash/vora-cloud/pkg/project"
)

func linkedProject() project.Project {
	return project.Project{ID: uuid.New(), Slug: "web", Repo: "octo/web", GitHubInstallationID: 7, GitHubRepoID: 9}
}

func change(status project.Status, errText string) project.Deployment {
	return project.Deployment{ID: uuid.New(), CommitSHA: strings.Repeat("a", 40), Status: status, Error: errText}
}

func TestEachStatusOfADeploymentIsReportedOnItsCommit(t *testing.T) {
	for _, tc := range []struct {
		status       project.Status
		err          string
		state, descr string
	}{
		{project.StatusBuilding, "", "pending", "Building the image."},
		{project.StatusPushing, "", "pending", "Pushing the image."},
		{project.StatusDeploying, "", "pending", "Deploying."},
		{project.StatusReady, "", "success", "Deployed."},
		{project.StatusFailed, "the build failed:\n  ERROR: x", "failure", "Failed: the build failed: ERROR: x"},
		{project.StatusFailed, "superseded by a newer push", "failure", "Failed: superseded by a newer push"},
	} {
		t.Run(string(tc.status)+" "+tc.err, func(t *testing.T) {
			st := &fakeStatuses{}
			h := NewPushDeployments(&fakeQueue{}, st, "https://cloud.test")
			p := linkedProject()
			h.DeploymentChanged(t.Context(), change(tc.status, tc.err), p)
			h.Wait()
			if len(st.posts) != 1 {
				t.Fatalf("%d statuses", len(st.posts))
			}
			got := st.posts[0]
			if got.installation != 7 || got.repo != "octo/web" || got.sha != strings.Repeat("a", 40) {
				t.Errorf("posted to %+v", got)
			}
			if got.status.State != tc.state || got.status.Description != tc.descr || got.status.Context != "vora/web" ||
				got.status.TargetURL != "https://cloud.test/projects/"+p.ID.String() {
				t.Errorf("status %+v", got.status)
			}
		})
	}
}

// A deployment that was made by hand has no commit, and a project that is not
// tied to a repository has nowhere to report: neither is reported.
func TestOnlyDeploymentsOfACommitOnALinkedProjectAreReported(t *testing.T) {
	st := &fakeStatuses{}
	h := NewPushDeployments(&fakeQueue{}, st, "")
	byHand := change(project.StatusReady, "")
	byHand.CommitSHA = ""
	unlinked := linkedProject()
	unlinked.GitHubRepoID = 0
	h.DeploymentChanged(t.Context(), byHand, linkedProject())
	h.DeploymentChanged(t.Context(), change(project.StatusReady, ""), unlinked)
	h.DeploymentChanged(t.Context(), change(project.StatusQueued, ""), linkedProject()) // not a change to report
	h.Wait()
	if len(st.posts) != 0 {
		t.Errorf("reported %+v", st.posts)
	}
	// No poster at all (no GitHub App) is not an error either.
	NewPushDeployments(&fakeQueue{}, nil, "").DeploymentChanged(t.Context(), change(project.StatusReady, ""), linkedProject())
}

// GitHub keeps the last status it was given for a commit, so they must arrive in
// the order they happened: a late "building" must not hide a "ready".
func TestStatusesAreReportedInTheOrderTheyHappened(t *testing.T) {
	st := &fakeStatuses{release: make(chan struct{})}
	h := NewPushDeployments(&fakeQueue{}, st, "")
	p := linkedProject()
	for _, s := range []project.Status{project.StatusBuilding, project.StatusPushing, project.StatusDeploying, project.StatusReady} {
		h.DeploymentChanged(t.Context(), change(s, ""), p)
	}
	close(st.release)
	h.Wait()
	var got []string
	for _, post := range st.posts {
		got = append(got, post.status.State+":"+post.status.Description)
	}
	want := "pending:Building the image. pending:Pushing the image. pending:Deploying. success:Deployed."
	if strings.Join(got, " ") != want {
		t.Errorf("order %q, want %q", strings.Join(got, " "), want)
	}
}

func TestAQueuedPushIsDescribedAsWaitingForABuildWhenBuildsAreOn(t *testing.T) {
	for builds, want := range map[bool]string{false: "Queued. Builds are not available yet.", true: "Queued for a build."} {
		st := &fakeStatuses{}
		h := NewPushDeployments(&fakeQueue{out: queuedFor("web")}, st, "")
		if builds {
			h.WithBuilds()
		}
		if err := h.HandlePush(context.Background(), thePush); err != nil {
			t.Fatal(err)
		}
		h.Wait()
		if len(st.posts) != 1 || st.posts[0].status.Description != want {
			t.Errorf("builds=%v: %+v, want %q", builds, st.posts, want)
		}
	}
}
