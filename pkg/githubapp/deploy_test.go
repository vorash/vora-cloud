package githubapp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vorash/vora-cloud/pkg/project"
)

type fakeQueue struct {
	mu     sync.Mutex
	inputs []project.PushInput
	out    []project.Queued
	err    error
}

func (f *fakeQueue) QueueFromPush(_ context.Context, in project.PushInput) ([]project.Queued, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, in)
	return f.out, f.err
}

type posted struct {
	installation int64
	repo, sha    string
	status       CommitStatus
}

type fakeStatuses struct {
	mu      sync.Mutex
	posts   []posted
	err     error
	release chan struct{} // when set, PostStatus waits for it
}

func (f *fakeStatuses) PostStatus(_ context.Context, installation int64, repo, sha string, s CommitStatus) error {
	if f.release != nil {
		<-f.release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, posted{installation, repo, sha, s})
	return f.err
}

var thePush = Push{InstallationID: 7, RepoID: 9, RepoFullName: "octo/web", Ref: "refs/heads/main", CommitSHA: strings.Repeat("a", 40)}

func queuedFor(slugs ...string) []project.Queued {
	var out []project.Queued
	for _, s := range slugs {
		out = append(out, project.Queued{Project: project.Project{ID: uuid.New(), Slug: s}, Deployment: project.Deployment{ID: uuid.New()}})
	}
	return out
}

func TestAPushQueuesAndIsReportedOnTheCommit(t *testing.T) {
	q := &fakeQueue{out: queuedFor("web", "api")}
	st := &fakeStatuses{}
	h := NewPushDeployments(q, st, "https://cloud.test")
	if err := h.HandlePush(t.Context(), thePush); err != nil {
		t.Fatal(err)
	}
	h.Wait()

	if len(q.inputs) != 1 || q.inputs[0] != (project.PushInput{InstallationID: 7, RepoID: 9, Ref: "refs/heads/main", CommitSHA: thePush.CommitSHA}) {
		t.Errorf("queue input: %+v", q.inputs)
	}
	if len(st.posts) != 2 {
		t.Fatalf("%d statuses, want one for each project", len(st.posts))
	}
	for i, p := range st.posts {
		slug := q.out[i].Project.Slug
		if p.installation != 7 || p.repo != "octo/web" || p.sha != thePush.CommitSHA {
			t.Errorf("posted to %+v", p)
		}
		// Each project has its own context, so two of them on one commit do not overwrite each other.
		if p.status.State != "pending" || p.status.Context != "vora/"+slug || p.status.TargetURL != "https://cloud.test/projects/"+q.out[i].Project.ID.String() {
			t.Errorf("status %+v", p.status)
		}
		if !strings.Contains(p.status.Description, "Queued") || len(p.status.Description) > maxDescription {
			t.Errorf("description %q", p.status.Description)
		}
	}
}

// GitHub gives a delivery ten seconds: the answer must not wait for the report.
func TestTheWebhookDoesNotWaitForTheStatus(t *testing.T) {
	st := &fakeStatuses{release: make(chan struct{})}
	h := NewPushDeployments(&fakeQueue{out: queuedFor("web")}, st, "")
	done := make(chan error, 1)
	go func() { done <- h.HandlePush(t.Context(), thePush) }()
	select {
	case err := <-done: // returns although PostStatus is blocked
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		close(st.release)
		t.Fatal("HandlePush waited for the status to be posted")
	}
	if len(st.posts) != 0 {
		t.Fatal("the status was posted before the handler returned")
	}
	close(st.release)
	h.Wait()
	if len(st.posts) != 1 {
		t.Errorf("%d statuses after the release", len(st.posts))
	}
	if st.posts[0].status.TargetURL != "" {
		t.Errorf("a link was set without a public URL: %q", st.posts[0].status.TargetURL)
	}
}

// The deployments exist; failing the delivery would only make GitHub queue them
// again, so a failure to report is logged and the push counts as handled.
func TestAFailedStatusDoesNotFailThePush(t *testing.T) {
	st := &fakeStatuses{err: errors.New("GitHub is down")}
	h := NewPushDeployments(&fakeQueue{out: queuedFor("web")}, st, "")
	if err := h.HandlePush(t.Context(), thePush); err != nil {
		t.Errorf("HandlePush = %v, want nil", err)
	}
	h.Wait()
	if len(st.posts) != 1 {
		t.Errorf("%d attempts", len(st.posts))
	}
}

// If queueing fails, nothing is reported and the delivery can be redelivered.
func TestAFailedQueueFailsThePushAndReportsNothing(t *testing.T) {
	st := &fakeStatuses{}
	h := NewPushDeployments(&fakeQueue{out: queuedFor("web"), err: errors.New("database down")}, st, "")
	if err := h.HandlePush(t.Context(), thePush); err == nil {
		t.Error("HandlePush hid a queue failure")
	}
	h.Wait()
	if len(st.posts) != 0 {
		t.Errorf("reported %+v", st.posts)
	}
}

func TestNoLinkedProjectMeansNoStatus(t *testing.T) {
	st := &fakeStatuses{}
	h := NewPushDeployments(&fakeQueue{}, st, "")
	if err := h.HandlePush(t.Context(), thePush); err != nil {
		t.Fatal(err)
	}
	h.Wait()
	if len(st.posts) != 0 {
		t.Errorf("a push to a repository with no project was reported: %+v", st.posts)
	}
}

// Without the App's credentials the deployments are still queued.
func TestWithoutAStatusPosterTheDeploymentIsStillQueued(t *testing.T) {
	q := &fakeQueue{out: queuedFor("web")}
	h := NewPushDeployments(q, nil, "")
	if err := h.HandlePush(t.Context(), thePush); err != nil {
		t.Fatal(err)
	}
	h.Wait()
	if len(q.inputs) != 1 {
		t.Error("the push was not queued")
	}
}

func TestPostStatusSendsTheDocumentedRequest(t *testing.T) {
	e := newConnectEnv(t)
	sha := strings.Repeat("b", 40)
	err := e.c.app.PostStatus(t.Context(), 42, "octo/web", sha, CommitStatus{
		State: "pending", Context: "vora/web", Description: strings.Repeat("d", 300), TargetURL: "https://cloud.test/projects/1",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := e.gh.statuses
	if len(got) != 1 {
		t.Fatalf("%d status requests", len(got))
	}
	r := got[0]
	if r.path != "/repos/octo/web/statuses/"+sha || r.bearer != "ghs_install" {
		t.Errorf("request %s with token %q", r.path, r.bearer)
	}
	if r.body["state"] != "pending" || r.body["context"] != "vora/web" || r.body["target_url"] != "https://cloud.test/projects/1" {
		t.Errorf("body %v", r.body)
	}
	if d, _ := r.body["description"].(string); len(d) != maxDescription {
		t.Errorf("description is %d characters, want it cut to GitHub's %d", len(d), maxDescription)
	}
}

// These end up in a URL path, so they are checked again here whatever the
// webhook did.
func TestPostStatusRefusesWhatCouldBeAPath(t *testing.T) {
	e := newConnectEnv(t)
	sha := strings.Repeat("c", 40)
	ok := CommitStatus{State: "pending", Context: "vora/web"}
	for name, tc := range map[string]struct {
		repo, sha string
		s         CommitStatus
	}{
		"a path as the repository": {"octo/../admin", sha, ok},
		"no owner":                 {"web", sha, ok},
		"a path as the commit":     {"octo/web", "../../x", ok},
		"a short commit":           {"octo/web", "abc", ok},
		"an unknown state":         {"octo/web", sha, CommitStatus{State: "done"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := e.c.app.PostStatus(t.Context(), 42, tc.repo, tc.sha, tc.s); err == nil {
				t.Error("accepted")
			}
		})
	}
	if len(e.gh.statuses) != 0 || e.gh.installTokens != 0 {
		t.Error("GitHub was contacted for a refused status")
	}
}

func TestPostStatusErrorHasNoToken(t *testing.T) {
	e := newConnectEnv(t)
	e.gh.statusCode = 422
	err := e.c.app.PostStatus(t.Context(), 42, "octo/web", strings.Repeat("d", 40), CommitStatus{State: "pending", Context: "vora/web"})
	if err == nil || strings.Contains(err.Error(), "ghs_install") {
		t.Errorf("err = %v", err)
	}
}
