package project_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	. "github.com/vorash/vora-cloud/pkg/project"
)

// The most of a build's log that is kept (the service's own limit).
const maxBuildLog = 256 << 10

func TestBuildSettings(t *testing.T) {
	for raw, want := range map[string]BuildSettings{
		``:                                    {Dockerfile: "Dockerfile"},
		`{}`:                                  {Dockerfile: "Dockerfile"},
		`{"dockerfile":"Dockerfile.backend"}`: {Dockerfile: "Dockerfile.backend"},
		`{"context":"frontend"}`:              {Dockerfile: "Dockerfile", Context: "frontend"},
		`{"dockerfile":"docker/app.Dockerfile","context":"apps/web"}`: {Dockerfile: "docker/app.Dockerfile", Context: "apps/web"},
	} {
		got, err := ParseBuildSettings([]byte(raw))
		if err != nil || got != want {
			t.Errorf("ParseBuildSettings(%q) = %+v, %v; want %+v", raw, got, err, want)
		}
	}
}

// The paths end up in a build's arguments: they stay inside the repository and
// carry nothing that could add an option.
func TestBuildSettingsRefuse(t *testing.T) {
	for name, raw := range map[string]string{
		"a dockerfile that leaves":   `{"dockerfile":"../Dockerfile"}`,
		"a dockerfile in the middle": `{"dockerfile":"a/../../b"}`,
		"an absolute dockerfile":     `{"dockerfile":"/etc/passwd"}`,
		"a dot segment":              `{"dockerfile":"./Dockerfile"}`,
		"a double slash":             `{"dockerfile":"a//b"}`,
		"a trailing slash":           `{"context":"frontend/"}`,
		"a comma":                    `{"context":"a,b"}`,
		"a colon":                    `{"context":"a:b"}`,
		"a space":                    `{"dockerfile":"my file"}`,
		"a context that leaves":      `{"context":"../x"}`,
		"a too long path":            `{"context":"` + strings.Repeat("a", 201) + `"}`,
		"an unknown field":           `{"dockerfle":"Dockerfile"}`,
		"not json":                   `{"dockerfile":`,
		"a number":                   `{"dockerfile":3}`,
	} {
		if got, err := ParseBuildSettings([]byte(raw)); err == nil {
			t.Errorf("%s: accepted as %+v", name, got)
		}
	}
}

func TestCreateProjectRefusesABadBuildConfiguration(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.CreateProject(ctx, e.alice, CreateInput{Name: "x", Slug: "bad-config", Region: "eu", BuildConfig: []byte(`{"dockerfile":"../x"}`)})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func queue(t *testing.T, e *env, p *Project, c string) *Deployment {
	t.Helper()
	got, err := e.svc.QueueFromPush(ctx, PushInput{InstallationID: p.GitHubInstallationID, RepoID: p.GitHubRepoID, Ref: "refs/heads/main", CommitSHA: strings.Repeat(c, 40)})
	if err != nil || len(got) != 1 {
		t.Fatalf("queue: %v, %v", got, err)
	}
	return &got[0].Deployment
}

func TestPendingBuildsAreTheQueuedOnesWithNoImageOnLinkedProjects(t *testing.T) {
	e := newEnv(t)
	linked := e.linkedProject(t, e.alice, "linked", 7, 9)
	plain := e.project(t, e.alice, "plain")
	byImage := e.deploy(t, linked, "reg/linked:1") // has an image: not a build
	first := queue(t, e, linked, "a")
	e.deploy(t, plain, "reg/plain:1")

	got, err := e.svc.PendingBuilds(ctx)
	if err != nil || len(got) != 1 || got[0].Deployment.ID != first.ID || got[0].Project.ID != linked.ID {
		t.Fatalf("PendingBuilds = %+v, %v; want only the push's deployment (not %s)", got, err, byImage.ID)
	}

	// Every step of a build is still pending, and a settled one is not.
	if err := e.svc.StartBuild(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.svc.PendingBuilds(ctx); len(got) != 1 || got[0].Deployment.Status != StatusBuilding {
		t.Errorf("a build in progress: %+v", got)
	}
	if err := e.svc.FailBuild(ctx, first.ID, "no"); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.svc.PendingBuilds(ctx); len(got) != 0 {
		t.Errorf("a failed build is still pending: %+v", got)
	}
}

func TestPendingBuildsAreOldestFirstAndSkipADeletedProject(t *testing.T) {
	e := newEnv(t)
	a, b := e.linkedProject(t, e.alice, "aaa", 7, 1), e.linkedProject(t, e.alice, "bbb", 7, 2)
	da, db := queue(t, e, a, "a"), queue(t, e, b, "b")
	got, _ := e.svc.PendingBuilds(ctx)
	if len(got) != 2 || got[0].Deployment.ID != da.ID || got[1].Deployment.ID != db.ID {
		t.Fatalf("order: %+v", got)
	}
	if err := e.svc.DeleteProject(ctx, e.alice, b.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := e.svc.PendingBuilds(ctx); err != nil || len(got) != 1 {
		t.Errorf("after deleting a project: %+v, %v", got, err)
	}
}

// Two passes, or a pass and a manual action, must never start one build twice.
func TestTheBuildStepsAreCompareAndSet(t *testing.T) {
	e := newEnv(t)
	d := queue(t, e, e.linkedProject(t, e.alice, "web", 7, 9), "a")
	if err := e.svc.BeginPush(ctx, d.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("BeginPush on a queued deployment: %v, want ErrConflict", err)
	}
	if err := e.svc.StartBuild(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.StartBuild(ctx, d.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("a second StartBuild: %v, want ErrConflict", err)
	}
	if err := e.svc.FinishBuild(ctx, d.ID, "reg/x@sha256:1"); !errors.Is(err, ErrConflict) {
		t.Errorf("FinishBuild before the push check: %v, want ErrConflict", err)
	}
	if len(e.eu.deployed) != 0 {
		t.Error("the cluster was touched by a refused step")
	}
}

func TestFinishBuildDeploysTheImageThroughTheSameRolloutAsAnImageDeployment(t *testing.T) {
	e := newEnv(t)
	p := e.linkedProject(t, e.alice, "web", 7, 9)
	if err := e.svc.SetEnv(ctx, e.alice, p.ID, "MODE", "prod"); err != nil {
		t.Fatal(err)
	}
	older := e.deploy(t, p, "reg/web:old") // deploying: an older unfinished deployment
	d := queue(t, e, p, "a")
	for _, step := range []func() error{
		func() error { return e.svc.StartBuild(ctx, d.ID) },
		func() error { return e.svc.BeginPush(ctx, d.ID) },
		func() error {
			return e.svc.FinishBuild(ctx, d.ID, "localhost:5000/web/web@sha256:"+strings.Repeat("c", 64))
		},
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}

	got, _ := e.store.GetDeployment(ctx, d.ID)
	if got.Status != StatusDeploying || got.Image != "localhost:5000/web/web@sha256:"+strings.Repeat("c", 64) || got.URL != "https://web.eu.cloud.example.com" {
		t.Errorf("deployment %+v", got)
	}
	spec := e.eu.deployed[len(e.eu.deployed)-1]
	if spec.Image != got.Image || spec.Env["MODE"] != "prod" || spec.Host != "web.eu.cloud.example.com" {
		t.Errorf("the cluster was given %+v", spec)
	}
	// An older unfinished deployment is superseded, as it is for an image deployment.
	if o, _ := e.store.GetDeployment(ctx, older.ID); o.Status != StatusFailed {
		t.Errorf("the older deployment is %s, want failed", o.Status)
	}
}

func TestFinishBuildRecordsAClusterRefusalOnTheDeployment(t *testing.T) {
	e := newEnv(t)
	p := e.linkedProject(t, e.alice, "web", 7, 9)
	d := queue(t, e, p, "a")
	_ = e.svc.StartBuild(ctx, d.ID)
	_ = e.svc.BeginPush(ctx, d.ID)
	e.eu.deployErr = errors.New("namespace is terminating")

	if err := e.svc.FinishBuild(ctx, d.ID, "localhost:5000/web/web@sha256:"+strings.Repeat("c", 64)); err != nil {
		t.Fatalf("FinishBuild = %v: a cluster refusal belongs on the deployment, not in an error", err)
	}
	got, _ := e.store.GetDeployment(ctx, d.ID)
	if got.Status != StatusFailed || got.Error == "" {
		t.Errorf("deployment %s %q", got.Status, got.Error)
	}
}

func TestFailBuild(t *testing.T) {
	e := newEnv(t)
	d := queue(t, e, e.linkedProject(t, e.alice, "web", 7, 9), "a")
	if err := e.svc.FailBuild(ctx, d.ID, "the build failed"); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.store.GetDeployment(ctx, d.ID); got.Status != StatusFailed || got.Error != "the build failed" {
		t.Errorf("%+v", got)
	}
	if err := e.svc.FailBuild(ctx, d.ID, "again"); !errors.Is(err, ErrConflict) {
		t.Errorf("failing a finished deployment: %v, want ErrConflict", err)
	}
}

// Postgres text holds neither NUL nor invalid UTF-8, and a build can print both.
func TestRecordBuildLogMakesItStorableAndKeepsTheTail(t *testing.T) {
	e := newEnv(t)
	d := queue(t, e, e.linkedProject(t, e.alice, "web", 7, 9), "a")

	if err := e.svc.RecordBuildLog(ctx, d.ID, "ok\x00line\xff\xfe end"); err != nil {
		t.Fatal(err)
	}
	got, _ := e.store.BuildLog(ctx, d.ID)
	if strings.ContainsRune(got, 0) || !utf8.ValidString(got) || !strings.HasPrefix(got, "okline") {
		t.Errorf("stored %q", got)
	}

	big := strings.Repeat("x", maxBuildLog) + "THE END"
	if err := e.svc.RecordBuildLog(ctx, d.ID, big); err != nil {
		t.Fatal(err)
	}
	got, _ = e.store.BuildLog(ctx, d.ID)
	if len(got) != maxBuildLog || !strings.HasSuffix(got, "THE END") {
		t.Errorf("kept %d bytes, ending %q", len(got), got[len(got)-7:])
	}
}

func TestBuildLogIsTheOwnersAlone(t *testing.T) {
	e := newEnv(t)
	d := queue(t, e, e.linkedProject(t, e.alice, "web", 7, 9), "a")
	_ = e.svc.RecordBuildLog(ctx, d.ID, "private output")
	if got, err := e.svc.BuildLog(ctx, e.alice, d.ID); err != nil || got != "private output" {
		t.Errorf("owner: %q, %v", got, err)
	}
	if _, err := e.svc.BuildLog(ctx, e.bob, d.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user: %v, want ErrNotFound", err)
	}
}

func TestTheObserverHearsOfEveryChange(t *testing.T) {
	e := newEnv(t)
	var seen []string
	e.svc.Observe(func(_ context.Context, d Deployment, p Project) {
		seen = append(seen, fmt.Sprintf("%s:%s:%s", p.Slug, d.Status, d.CommitSHA[:3]))
	})
	p := e.linkedProject(t, e.alice, "web", 7, 9)
	d := queue(t, e, p, "a")
	_ = e.svc.StartBuild(ctx, d.ID)
	_ = e.svc.BeginPush(ctx, d.ID)
	_ = e.svc.FinishBuild(ctx, d.ID, "localhost:5000/web/web@sha256:"+strings.Repeat("c", 64))
	want := "web:building:aaa web:pushing:aaa web:deploying:aaa"
	if got := strings.Join(seen, " "); got != want {
		t.Errorf("observed %q, want %q", got, want)
	}

	// A refused step changes nothing and says nothing.
	seen = nil
	_ = e.svc.StartBuild(ctx, d.ID)
	if len(seen) != 0 {
		t.Errorf("observed %v for a refused step", seen)
	}
}

// A deployment on a project that is not tied to a repository has nothing to
// build, however it came to be queued without an image.
func TestAQueuedDeploymentOnAnUnlinkedProjectIsNotABuild(t *testing.T) {
	e := newEnv(t)
	plain := e.project(t, e.alice, "plain")
	if err := e.store.CreateDeployment(ctx, &Deployment{ProjectID: plain.ID, Status: StatusQueued, Region: "eu"}); err != nil {
		t.Fatal(err)
	}
	if got, err := e.svc.PendingBuilds(ctx); err != nil || len(got) != 0 {
		t.Errorf("PendingBuilds = %+v, %v; want none: nothing could be fetched for it", got, err)
	}
}
