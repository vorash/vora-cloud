package project_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vorash/vora-cloud/pkg/deploy"
	. "github.com/vorash/vora-cloud/pkg/project"
	"github.com/vorash/vora-cloud/pkg/project/projecttest"
	"github.com/vorash/vora-cloud/pkg/secrets"
)

// fakeDeployer records what the cluster was asked to do.
type fakeDeployer struct {
	deployed        []deploy.Spec
	deletedProjects []string
	deployErr       error
	onDeploy        func() // runs inside Deploy, before it returns
	deleteErr       error
	status          deploy.Status
}

func (f *fakeDeployer) Deploy(_ context.Context, s deploy.Spec) error {
	f.deployed = append(f.deployed, s)
	if f.onDeploy != nil {
		f.onDeploy()
	}
	return f.deployErr
}
func (f *fakeDeployer) Status(context.Context, string, string) (deploy.Status, error) {
	return f.status, nil
}
func (f *fakeDeployer) Delete(context.Context, string, string) error { return nil }
func (f *fakeDeployer) DeleteProject(_ context.Context, p string) error {
	f.deletedProjects = append(f.deletedProjects, p)
	return f.deleteErr
}
func (f *fakeDeployer) Health(context.Context) error { return nil }

type env struct {
	svc   *Service
	store *projecttest.MemStore
	eu    *fakeDeployer
	alice uuid.UUID
	bob   uuid.UUID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	eu := &fakeDeployer{}
	reg, err := deploy.NewRegistry(map[string]deploy.Deployer{"eu": eu})
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	st := projecttest.New()
	svc := NewService(st, reg, box, "cloud.example.com")
	return &env{svc: svc, store: st, eu: eu, alice: uuid.New(), bob: uuid.New()}
}

var ctx = context.Background()

func (e *env) project(t *testing.T, owner uuid.UUID, slug string) *Project {
	t.Helper()
	p, err := e.svc.CreateProject(ctx, owner, CreateInput{Name: slug, Slug: slug, Region: "eu"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// deploy deploys an image as alice and fails the test if the call errors.
func (e *env) deploy(t *testing.T, p *Project, image string) *Deployment {
	t.Helper()
	d, err := e.svc.Deploy(ctx, e.alice, p.ID, DeployInput{Image: image})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCreateProjectAppliesDefaults(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	if p.Port != 80 || p.Replicas != 1 || p.CPUMillis != 250 || p.MemoryMiB != 256 || p.DefaultBranch != "main" {
		t.Errorf("defaults not applied: %+v", p)
	}
}

func TestCreateProjectRejects(t *testing.T) {
	e := newEnv(t)
	e.project(t, e.alice, "taken")
	cases := map[string]struct {
		in   CreateInput
		want error
	}{
		"no name":         {CreateInput{Slug: "a", Region: "eu"}, ErrInvalid},
		"uppercase slug":  {CreateInput{Name: "x", Slug: "Web", Region: "eu"}, ErrInvalid},
		"slug too long":   {CreateInput{Name: "x", Slug: strings.Repeat("a", 62), Region: "eu"}, ErrInvalid},
		"unknown region":  {CreateInput{Name: "x", Slug: "a", Region: "mars"}, ErrInvalid},
		"bad port":        {CreateInput{Name: "x", Slug: "a", Region: "eu", Port: 70000}, ErrInvalid},
		"slug of another": {CreateInput{Name: "x", Slug: "taken", Region: "eu"}, ErrSlugTaken},
	}
	for name, c := range cases {
		if _, err := e.svc.CreateProject(ctx, e.bob, c.in); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
}

// The rule the API stands on: a user must not be able to see or change
// anything of another user's project. Each call returns "not found" rather
// than "forbidden", so ids cannot be probed. If the ownership check in
// Service.owned goes away, every line here fails.
func TestAnotherUserSeesNothingOfAProject(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	d, err := e.svc.Deploy(ctx, e.alice, p.ID, DeployInput{Image: "reg/web:1"})
	if err != nil {
		t.Fatal(err)
	}

	calls := map[string]func() error{
		"get project":      func() error { _, err := e.svc.GetProject(ctx, e.bob, p.ID); return err },
		"delete project":   func() error { return e.svc.DeleteProject(ctx, e.bob, p.ID) },
		"list deployments": func() error { _, err := e.svc.ListDeployments(ctx, e.bob, p.ID, Page{}); return err },
		"get deployment":   func() error { _, err := e.svc.GetDeployment(ctx, e.bob, d.ID); return err },
		"deploy":           func() error { _, err := e.svc.Deploy(ctx, e.bob, p.ID, DeployInput{Image: "evil:1"}); return err },
		"redeploy":         func() error { _, err := e.svc.Redeploy(ctx, e.bob, p.ID); return err },
		"rollback":         func() error { _, err := e.svc.Rollback(ctx, e.bob, p.ID, d.ID); return err },
		"set env":          func() error { return e.svc.SetEnv(ctx, e.bob, p.ID, "K", "v") },
		"list env":         func() error { _, err := e.svc.EnvNames(ctx, e.bob, p.ID); return err },
		"delete env":       func() error { return e.svc.DeleteEnv(ctx, e.bob, p.ID, "K") },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s as another user: got %v, want ErrNotFound", name, err)
		}
	}
	if len(e.eu.deployed) != 1 || len(e.eu.deletedProjects) != 0 {
		t.Errorf("another user reached the cluster: deployed %d, deleted %v", len(e.eu.deployed), e.eu.deletedProjects)
	}
	if list, _ := e.svc.ListProjects(ctx, e.bob); len(list) != 0 {
		t.Errorf("another user lists %d projects", len(list))
	}
}

// The projects page shows each project's newest deployment. A project with no
// deployment shows none, and nobody's list carries another user's deployment.
func TestSummariesCarryTheNewestDeploymentOfTheOwnersProjects(t *testing.T) {
	e := newEnv(t)
	web := e.project(t, e.alice, "web")
	e.project(t, e.alice, "idle")
	e.deploy(t, web, "reg/web:1")
	newest := e.deploy(t, web, "reg/web:2")
	theirs := e.project(t, e.bob, "theirs")
	if _, err := e.svc.Deploy(ctx, e.bob, theirs.ID, DeployInput{Image: "reg/theirs:1"}); err != nil {
		t.Fatal(err)
	}

	bySlug := func(owner uuid.UUID) map[string]Summary {
		t.Helper()
		list, err := e.svc.ListSummaries(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]Summary{}
		for _, s := range list {
			out[s.Slug] = s
		}
		return out
	}

	alice := bySlug(e.alice)
	if len(alice) != 2 || alice["web"].Latest == nil || alice["web"].Latest.ID != newest.ID {
		t.Errorf("alice's web must carry its newest deployment %s: %+v", newest.ID, alice["web"].Latest)
	}
	if alice["idle"].Latest != nil {
		t.Errorf("a project without deployments has none: %+v", alice["idle"].Latest)
	}
	bob := bySlug(e.bob)
	if len(bob) != 1 || bob["theirs"].Latest == nil || bob["theirs"].Latest.Image != "reg/theirs:1" {
		t.Errorf("bob sees only his own project and deployment: %+v", bob)
	}
}

func TestRollbackRejectsADeploymentOfAnotherProject(t *testing.T) {
	e := newEnv(t)
	mine := e.project(t, e.alice, "mine")
	other := e.project(t, e.alice, "other") // the same owner, so only the project check can stop it
	od, _ := e.svc.Deploy(ctx, e.alice, other.ID, DeployInput{Image: "reg/other:1"})
	e.eu.status = deploy.Status{State: deploy.StateReady}
	e.svc.Reconcile(ctx)

	if _, err := e.svc.Rollback(ctx, e.alice, mine.ID, od.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// Done when: deploying an existing image creates a deployment that reaches ready.
func TestDeployReachesReadyWhenTheRolloutFinishes(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")

	d, err := e.svc.Deploy(ctx, e.alice, p.ID, DeployInput{Image: "reg/web:1", CommitSHA: "abc", Ref: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != StatusDeploying || d.URL != "https://web.eu.cloud.example.com" {
		t.Fatalf("after deploy: %+v", d)
	}
	spec := e.eu.deployed[0]
	if spec.Project != "web" || spec.Image != "reg/web:1" || spec.Host != "web.eu.cloud.example.com" || !spec.TLS {
		t.Errorf("cluster got %+v", spec)
	}

	e.eu.status = deploy.Status{State: deploy.StateRollingOut}
	e.svc.Reconcile(ctx)
	if got, _ := e.svc.GetDeployment(ctx, e.alice, d.ID); got.Status != StatusDeploying {
		t.Fatalf("mid rollout: %s", got.Status)
	}

	e.eu.status = deploy.Status{State: deploy.StateReady}
	e.svc.Reconcile(ctx)
	if got, _ := e.svc.GetDeployment(ctx, e.alice, d.ID); got.Status != StatusReady {
		t.Errorf("after rollout: %s, want ready", got.Status)
	}
}

// internal is what a cluster's own message can carry and a user must never see.
const internal = `Patch "https://10.42.0.7:6443/api/v1/namespaces/p-web": dial tcp 10.42.0.7:6443`

// captureLog returns what the standard logger writes until the test ends.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func TestFailedRolloutShowsAReasonAndLogsTheClusterText(t *testing.T) {
	cases := []struct {
		name   string
		status deploy.Status
		want   string // a part of the reason shown to the user
	}{
		{"deadline", deploy.Status{Reason: "ProgressDeadlineExceeded"}, "did not become ready in time"},
		{"pull backoff", deploy.Status{Reason: "ImagePullBackOff"}, `image "reg/web:1" could not be pulled`},
		{"pull error", deploy.Status{Reason: "ErrImagePull"}, `image "reg/web:1" could not be pulled`},
		{"unknown reason", deploy.Status{Reason: "Evicted"}, "Quote deployment "},
		{"no reason", deploy.Status{}, "Quote deployment "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logs := captureLog(t)
			e := newEnv(t)
			d := e.deploy(t, e.project(t, e.alice, "web"), "reg/web:1")

			c.status.State, c.status.Message = deploy.StateFailed, internal
			e.eu.status = c.status
			e.svc.Reconcile(ctx)
			got, _ := e.svc.GetDeployment(ctx, e.alice, d.ID)
			if got.Status != StatusFailed || !strings.Contains(got.Error, c.want) {
				t.Errorf("got %s %q, want a reason with %q", got.Status, got.Error, c.want)
			}
			if strings.Contains(got.Error, "10.42.0.7") {
				t.Errorf("the cluster's address reached the user: %q", got.Error)
			}
			if !strings.Contains(logs.String(), d.ID.String()) || !strings.Contains(logs.String(), "10.42.0.7") {
				t.Errorf("the operator's log lacks the deployment id or the raw message: %q", logs)
			}
		})
	}
}

func TestStuckRolloutTimesOut(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	e.store.Clock = time.Now().Add(-time.Hour) // the deployment started long ago
	d := e.deploy(t, p, "reg/web:1")

	e.eu.status = deploy.Status{State: deploy.StatePending}
	e.svc.Reconcile(ctx)
	got, _ := e.svc.GetDeployment(ctx, e.alice, d.ID)
	if got.Status != StatusFailed || !strings.Contains(got.Error, "did not finish") {
		t.Errorf("got %s %q", got.Status, got.Error)
	}
}

func TestClusterErrorLeavesAFailedRecordWithAReason(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"unreachable", fmt.Errorf("apply namespace p-web: %w: %w", deploy.ErrUnreachable, errors.New(internal)), "could not be reached"},
		{"refused", fmt.Errorf("apply deployment: %w: %w", deploy.ErrRefused, errors.New(internal)), "refused the deployment"},
		{"invalid spec", fmt.Errorf("%w: host %q", deploy.ErrInvalidSpec, internal), "refused the deployment"},
		{"anything else", errors.New(internal), "Quote deployment "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logs := captureLog(t)
			e := newEnv(t)
			p := e.project(t, e.alice, "web")
			e.eu.deployErr = c.err

			d, err := e.svc.Deploy(ctx, e.alice, p.ID, DeployInput{Image: "reg/web:1"})
			if err != nil || d.Status != StatusFailed || !strings.Contains(d.Error, c.want) {
				t.Fatalf("got %+v, %v, want a failed record with %q", d, err, c.want)
			}
			if strings.Contains(d.Error, "10.42.0.7") {
				t.Errorf("the cluster's address reached the user: %q", d.Error)
			}
			if got, _ := e.svc.ListDeployments(ctx, e.alice, p.ID, Page{}); len(got.Deployments) != 1 || got.Deployments[0].Error != d.Error {
				t.Errorf("stored: %+v, want the same reason as the returned record", got.Deployments)
			}
			if !strings.Contains(logs.String(), d.ID.String()) || !strings.Contains(logs.String(), "10.42.0.7") {
				t.Errorf("the operator's log lacks the deployment id or the raw message: %q", logs)
			}
		})
	}
}

// The cluster runs only the newest spec, so an older unfinished deployment
// must not be reported ready for a rollout that never carried its image.
func TestANewDeploymentSupersedesTheUnfinishedOne(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	first := e.deploy(t, p, "reg/web:1")
	second := e.deploy(t, p, "reg/web:2")

	got, _ := e.svc.GetDeployment(ctx, e.alice, first.ID)
	if got.Status != StatusFailed || !strings.Contains(got.Error, "superseded") {
		t.Errorf("first: %s %q", got.Status, got.Error)
	}
	if second.Status != StatusDeploying {
		t.Errorf("second: %s", second.Status)
	}
}

func TestRedeployRunsTheLatestImageAgain(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	if _, err := e.svc.Redeploy(ctx, e.alice, p.ID); !errors.Is(err, ErrInvalid) {
		t.Errorf("redeploy of a project that never deployed: %v", err)
	}
	e.deploy(t, p, "reg/web:1")
	e.deploy(t, p, "reg/web:2")
	if _, err := e.svc.Redeploy(ctx, e.alice, p.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.eu.deployed[len(e.eu.deployed)-1].Image; got != "reg/web:2" {
		t.Errorf("redeployed %s, want reg/web:2", got)
	}
}

func TestRollbackDeploysAnEarlierReadyImage(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	e.eu.status = deploy.Status{State: deploy.StateReady}
	v1, _ := e.svc.Deploy(ctx, e.alice, p.ID, DeployInput{Image: "reg/web:1", CommitSHA: "aaa"})
	e.svc.Reconcile(ctx)
	e.deploy(t, p, "reg/web:2")
	e.svc.Reconcile(ctx)

	d, err := e.svc.Rollback(ctx, e.alice, p.ID, v1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID == v1.ID || d.Image != "reg/web:1" || d.CommitSHA != "aaa" {
		t.Errorf("rollback deployment: %+v", d)
	}
	if got := e.eu.deployed[len(e.eu.deployed)-1].Image; got != "reg/web:1" {
		t.Errorf("cluster got %s", got)
	}
}

func TestRollbackToAnImageThatNeverRanIsRefused(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	e.eu.deployErr = errors.New("boom")
	bad := e.deploy(t, p, "reg/web:bad")
	if _, err := e.svc.Rollback(ctx, e.alice, p.ID, bad.ID); !errors.Is(err, ErrInvalid) {
		t.Errorf("got %v, want ErrInvalid", err)
	}
}

func TestDeleteProjectTearsDownTheNamespaceFirst(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")

	e.eu.deleteErr = errors.New("cluster unreachable")
	if err := e.svc.DeleteProject(ctx, e.alice, p.ID); err == nil {
		t.Fatal("delete succeeded while the cluster failed")
	}
	if _, err := e.svc.GetProject(ctx, e.alice, p.ID); err != nil {
		t.Errorf("the record must stay so the delete can be retried: %v", err)
	}

	e.eu.deleteErr = nil
	if err := e.svc.DeleteProject(ctx, e.alice, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetProject(ctx, e.alice, p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
	if len(e.eu.deletedProjects) != 2 || e.eu.deletedProjects[1] != "web" {
		t.Errorf("namespace teardown calls: %v", e.eu.deletedProjects)
	}
}

func TestEnvIsStoredEncryptedAndReachesTheClusterInTheClear(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	const secret = "postgres://u:hunter2@db/app"
	if err := e.svc.SetEnv(ctx, e.alice, p.ID, "DATABASE_URL", secret); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetEnv(ctx, e.alice, p.ID, "bad-name", "x"); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid name: %v", err)
	}

	if stored := e.store.Env[p.ID]["DATABASE_URL"]; len(stored) == 0 || bytes.Contains(stored, []byte("hunter2")) {
		t.Fatalf("stored value is not encrypted: %q", stored)
	}
	names, _ := e.svc.EnvNames(ctx, e.alice, p.ID)
	sort.Strings(names)
	if len(names) != 1 || names[0] != "DATABASE_URL" {
		t.Errorf("names: %v", names)
	}

	e.deploy(t, p, "reg/web:1")
	if got := e.eu.deployed[0].Env["DATABASE_URL"]; got != secret {
		t.Errorf("cluster got %q", got)
	}
}

// Until the new spec is applied, the cluster reports the old rollout, which
// may look ready. A deployment whose apply fails must never be marked ready
// by a reconcile that lands in that window, and must not take the older
// deployment, which is still the one running, down with it.
func TestFailedApplyIsNeverReportedReady(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	first := e.deploy(t, p, "reg/web:1")

	e.eu.deployErr = errors.New("admission webhook denied the request")
	e.eu.onDeploy = func() {
		e.eu.status = deploy.Status{State: deploy.StateReady} // the old rollout
		e.svc.Reconcile(ctx)
	}
	second, err := e.svc.Deploy(ctx, e.alice, p.ID, DeployInput{Image: "reg/web:2"})
	if err != nil {
		t.Fatal(err)
	}

	if second.Status != StatusFailed {
		t.Errorf("second: %s, want failed", second.Status)
	}
	if got, _ := e.svc.GetDeployment(ctx, e.alice, second.ID); got.Status != StatusFailed || got.Error == "" {
		t.Errorf("stored second: %s %q", got.Status, got.Error)
	}
	// The reconcile inside Deploy settled the first one for the old rollout; a
	// failed apply must not have failed it as "superseded".
	if got, _ := e.svc.GetDeployment(ctx, e.alice, first.ID); strings.Contains(got.Error, "superseded") {
		t.Errorf("first was superseded by a deployment that never applied: %s %q", got.Status, got.Error)
	}
}

func TestDeploymentsArePagedNewestFirstWithoutGapsOrRepeats(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	var want []uuid.UUID // newest first
	for i := 0; i < 5; i++ {
		want = append([]uuid.UUID{e.deploy(t, p, "reg/web:"+string(rune('a'+i))).ID}, want...)
	}

	var got []uuid.UUID
	before := ""
	for pages := 0; ; pages++ {
		if pages > len(want) {
			t.Fatal("paging does not end")
		}
		page, err := e.svc.ListDeployments(ctx, e.alice, p.ID, Page{Limit: 2, Before: before})
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range page.Deployments {
			got = append(got, d.ID)
		}
		if page.Next == "" {
			if len(page.Deployments) == 0 {
				t.Error("the last page must not be empty when older ones were promised")
			}
			break
		}
		if len(page.Deployments) != 2 {
			t.Errorf("a page with a next cursor holds %d, want the limit", len(page.Deployments))
		}
		before = page.Next
		// A deployment made between two requests must not shift the next page.
		if pages == 0 {
			e.deploy(t, p, "reg/web:new")
		}
	}
	if len(got) != len(want) {
		t.Fatalf("got %d deployments, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: got %s, want %s", i, got[i], want[i])
		}
	}
}

func TestPageSizeDefaultsAndIsCapped(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	for i := 0; i < MaxPageSize+5; i++ {
		e.deploy(t, p, "reg/web:1")
	}
	for name, c := range map[string]struct{ limit, want int }{
		"default":  {0, DefaultPageSize},
		"asked":    {7, 7},
		"too many": {MaxPageSize * 10, MaxPageSize},
	} {
		page, err := e.svc.ListDeployments(ctx, e.alice, p.ID, Page{Limit: c.limit})
		if err != nil || len(page.Deployments) != c.want || page.Next == "" {
			t.Errorf("%s: %d deployments, next %q, %v; want %d with an older page", name, len(page.Deployments), page.Next, err, c.want)
		}
	}
}

func TestBadPagingInputIsInvalidNotAnInternalError(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	for name, page := range map[string]Page{
		"negative limit": {Limit: -1},
		"not base64":     {Before: "%%%"},
		"no separator":   {Before: "MTIz"},
		"bad id":         {Before: "MTIzLm5vcGU"},
	} {
		if _, err := e.svc.ListDeployments(ctx, e.alice, p.ID, page); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
}

// A cursor carries a position, not a right: another user gets the same
// "not found" for a project they do not own, with or without it.
func TestACursorDoesNotOpenAnotherUsersProject(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	for i := 0; i < 3; i++ {
		e.deploy(t, p, "reg/web:1")
	}
	page, _ := e.svc.ListDeployments(ctx, e.alice, p.ID, Page{Limit: 1})
	if page.Next == "" {
		t.Fatal("expected an older page")
	}
	if _, err := e.svc.ListDeployments(ctx, e.bob, p.ID, Page{Before: page.Next}); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// Redeploy must run the newest image however long the history is.
func TestRedeployUsesTheNewestDeploymentOfALongHistory(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	for i := 0; i < MaxPageSize+3; i++ {
		e.deploy(t, p, "reg/web:old")
	}
	e.deploy(t, p, "reg/web:newest")
	d, err := e.svc.Redeploy(ctx, e.alice, p.ID)
	if err != nil || d.Image != "reg/web:newest" {
		t.Errorf("redeployed %v, %v; want reg/web:newest", d, err)
	}
}

// Superseding reads the unfinished deployments of every project, so it must
// fail only the deploying project's own older ones.
func TestSupersedeLeavesOtherProjectsAlone(t *testing.T) {
	e := newEnv(t)
	web := e.project(t, e.alice, "web")
	other := e.project(t, e.bob, "other")
	elsewhere, err := e.svc.Deploy(ctx, e.bob, other.ID, DeployInput{Image: "reg/other:1"})
	if err != nil {
		t.Fatal(err)
	}
	first := e.deploy(t, web, "reg/web:1")
	e.deploy(t, web, "reg/web:2")

	if got, _ := e.svc.GetDeployment(ctx, e.alice, first.ID); got.Status != StatusFailed {
		t.Errorf("the project's older deployment: %s, want failed", got.Status)
	}
	if got, _ := e.svc.GetDeployment(ctx, e.bob, elsewhere.ID); got.Status != StatusDeploying {
		t.Errorf("another project's deployment: %s, want deploying", got.Status)
	}
}
