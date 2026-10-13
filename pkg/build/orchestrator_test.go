package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vorash/vora-cloud/pkg/deploy"
	"github.com/vorash/vora-cloud/pkg/project"
	"github.com/vorash/vora-cloud/pkg/project/projecttest"
	"github.com/vorash/vora-cloud/pkg/secrets"
)

type recDeployer struct {
	mu       sync.Mutex
	deployed []deploy.Spec
	err      error
}

func (d *recDeployer) Deploy(_ context.Context, s deploy.Spec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return d.err
	}
	d.deployed = append(d.deployed, s)
	return nil
}
func (d *recDeployer) Status(context.Context, string, string) (deploy.Status, error) {
	return deploy.Status{State: deploy.StateReady}, nil
}
func (d *recDeployer) Delete(context.Context, string, string) error { return nil }
func (d *recDeployer) DeleteProject(context.Context, string) error  { return nil }
func (d *recDeployer) Health(context.Context) error                 { return nil }

type fakeRunner struct {
	mu       sync.Mutex
	started  []Spec
	results  map[string]Result
	logs     map[string]string
	cleaned  []string
	startErr error
	swept    []func(string) bool
}

func (f *fakeRunner) Start(_ context.Context, s Spec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.started = append(f.started, s)
	return nil
}
func (f *fakeRunner) Status(_ context.Context, id string) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.results[id]; ok {
		return r, nil
	}
	return Result{State: Running}, nil
}
func (f *fakeRunner) Logs(_ context.Context, id string, _ int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logs[id], nil
}
func (f *fakeRunner) Cleanup(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleaned = append(f.cleaned, id)
	return nil
}
func (f *fakeRunner) Sweep(_ context.Context, keep func(string) bool) error {
	f.swept = append(f.swept, keep)
	return nil
}
func (f *fakeRunner) cleanedCount(id string) int {
	n := 0
	for _, c := range f.cleaned {
		if c == id {
			n++
		}
	}
	return n
}

type fakeTokens struct {
	calls []string
	err   error
}

func (f *fakeTokens) RepoToken(_ context.Context, inst, repo int64) (string, error) {
	f.calls = append(f.calls, fmt.Sprintf("%d/%d", inst, repo))
	if f.err != nil {
		return "", f.err
	}
	return "ghs_TOKENTOKENTOKENTOKENTOKEN", nil
}

type fakeRegistry struct {
	digests map[string]string // "repo:tag"
	err     error
	deleted []string // "repo@digest", in order
	delErr  error
}

func (f *fakeRegistry) Delete(_ context.Context, repo, digest string) error {
	if f.delErr != nil {
		return f.delErr
	}
	f.deleted = append(f.deleted, repo+"@"+digest)
	return nil
}

func (f *fakeRegistry) Digest(_ context.Context, repo, tag string) (string, error) {
	if f.err != nil && tag != cacheTag {
		return "", f.err
	}
	if d, ok := f.digests[repo+":"+tag]; ok {
		return d, nil
	}
	return "", ErrNoImage
}

type harness struct {
	t        *testing.T
	svc      *project.Service
	store    *projecttest.MemStore
	deployer *recDeployer
	runner   *fakeRunner
	tokens   *fakeTokens
	registry *fakeRegistry
	orch     *Orchestrator
	owner    uuid.UUID
	repoSeq  int64
}

const digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func newHarness(t *testing.T) *harness {
	t.Helper()
	dep := &recDeployer{}
	reg, err := deploy.NewRegistry(map[string]deploy.Deployer{"eu": dep})
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := projecttest.New()
	svc := project.NewService(store, reg, box, "cloud.example.com")
	h := &harness{t: t, svc: svc, store: store, deployer: dep, owner: uuid.New(),
		runner: &fakeRunner{results: map[string]Result{}, logs: map[string]string{}}, tokens: &fakeTokens{}, registry: &fakeRegistry{digests: map[string]string{}}}
	h.orch = NewOrchestrator(svc, map[string]Runner{"eu": h.runner}, h.tokens, h.registry,
		Config{PushHost: "registry.registry.svc.cluster.local:5000", PullHost: "localhost:5000"})
	return h
}

func (h *harness) project(slug string, config string) *project.Project {
	h.t.Helper()
	h.repoSeq++
	p, err := h.svc.CreateProject(h.t.Context(), h.owner, project.CreateInput{
		Name: slug, Slug: slug, Region: "eu", Repo: "octo/" + slug, GitHubInstallationID: 7, GitHubRepoID: 100 + h.repoSeq, BuildConfig: []byte(config),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}

// push queues a deployment for the project's repository, as the webhook does.
func (h *harness) push(p *project.Project, sha string) uuid.UUID {
	h.t.Helper()
	got, err := h.svc.QueueFromPush(h.t.Context(), project.PushInput{InstallationID: p.GitHubInstallationID, RepoID: p.GitHubRepoID, Ref: "refs/heads/main", CommitSHA: sha})
	if err != nil || len(got) != 1 {
		h.t.Fatalf("push: %v, %v", got, err)
	}
	return got[0].Deployment.ID
}

func (h *harness) tick() { h.orch.Tick(h.t.Context()) }

func (h *harness) status(id uuid.UUID) project.Deployment {
	h.t.Helper()
	d, err := h.store.GetDeployment(h.t.Context(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return *d
}

func sha(c string) string { return strings.Repeat(c, 40) }

func TestABuildIsStartedAndDeployedByDigest(t *testing.T) {
	h := newHarness(t)
	p := h.project("web", `{"dockerfile":"Dockerfile.backend","context":"app"}`)
	id := h.push(p, sha("a"))

	h.tick()
	if got := h.status(id).Status; got != project.StatusBuilding {
		t.Fatalf("status %s, want building", got)
	}
	if len(h.runner.started) != 1 {
		t.Fatalf("%d builds started", len(h.runner.started))
	}
	s := h.runner.started[0]
	if s.ID != id.String() || s.Repo != "octo/web" || s.CommitSHA != sha("a") || s.Context != "app" || s.Dockerfile != "Dockerfile.backend" ||
		s.PushRef != "registry.registry.svc.cluster.local:5000/web/web:"+sha("a")[:12] || s.Token != "ghs_TOKENTOKENTOKENTOKENTOKEN" {
		t.Errorf("spec %+v", s)
	}
	// The token is for this repository only, and the first build has no cache to import.
	if h.tokens.calls[0] != "7/"+fmt.Sprint(p.GitHubRepoID) || s.ImportCache {
		t.Errorf("token for %v, import cache %v", h.tokens.calls, s.ImportCache)
	}

	// The build runs, logging; then it succeeds and its image is in the registry.
	h.runner.logs[id.String()] = "#1 building\n"
	h.tick()
	if log, _ := h.store.BuildLog(t.Context(), id); log != "#1 building\n" {
		t.Errorf("stored log %q", log)
	}
	h.runner.results[id.String()] = Result{State: Succeeded}
	h.registry.digests["web/web:"+sha("a")[:12]] = digestA
	h.tick()

	d := h.status(id)
	// By digest, under the node's mirror name: another build re-tagging the image cannot change what runs.
	want := "localhost:5000/web/web@" + digestA
	if d.Status != project.StatusDeploying || d.Image != want || d.URL != "https://web.eu.cloud.example.com" {
		t.Errorf("deployment %+v, want deploying with image %s", d, want)
	}
	if len(h.deployer.deployed) != 1 || h.deployer.deployed[0].Image != want {
		t.Errorf("the cluster was asked to run %+v", h.deployer.deployed)
	}
	if h.runner.cleanedCount(id.String()) == 0 {
		t.Error("the build's Job and Secret were not removed")
	}
}

func TestOneBuildAtATime(t *testing.T) {
	h := newHarness(t)
	first, second := h.push(h.project("one", "{}"), sha("a")), h.push(h.project("two", "{}"), sha("b"))

	h.tick()
	h.tick()
	h.tick()
	if len(h.runner.started) != 1 || h.status(first).Status != project.StatusBuilding || h.status(second).Status != project.StatusQueued {
		t.Fatalf("started %d; first %s, second %s", len(h.runner.started), h.status(first).Status, h.status(second).Status)
	}

	// When the first is done, the second starts in the same pass.
	h.runner.results[first.String()] = Result{State: Failed}
	h.tick()
	if h.status(second).Status != project.StatusBuilding || len(h.runner.started) != 2 {
		t.Errorf("the second did not start once the first ended: %s", h.status(second).Status)
	}
}

// A failed build leaves what is serving alone: the deployer is not called.
func TestAFailedBuildNeverReachesTheDeployer(t *testing.T) {
	h := newHarness(t)
	p := h.project("web", "{}")
	serving, err := h.svc.Deploy(t.Context(), h.owner, p.ID, project.DeployInput{Image: "reg/web:1"})
	if err != nil {
		t.Fatal(err)
	}
	before := len(h.deployer.deployed)
	id := h.push(p, sha("a"))

	h.tick()
	h.runner.logs[id.String()] = "#5 RUN npm ci\n#5 ERROR: npm ERR! missing script\nERROR: failed to solve: process \"/bin/sh -c npm ci\" did not complete successfully\n"
	h.runner.results[id.String()] = Result{State: Failed, Reason: "BackoffLimitExceeded: Job has reached the specified backoff limit"}
	h.tick()

	d := h.status(id)
	if d.Status != project.StatusFailed || !strings.Contains(d.Error, "the build failed") || !strings.Contains(d.Error, "did not complete successfully") {
		t.Errorf("deployment %s %q", d.Status, d.Error)
	}
	if len(h.deployer.deployed) != before {
		t.Errorf("the deployer was called by a failed build: %d -> %d", before, len(h.deployer.deployed))
	}
	if h.status(serving.ID).Status == project.StatusFailed {
		t.Error("the deployment that was serving was failed")
	}
	if h.runner.cleanedCount(id.String()) == 0 {
		t.Error("a failed build's Job and Secret were not removed")
	}
	if log, _ := h.store.BuildLog(t.Context(), id); !strings.Contains(log, "npm ERR") {
		t.Errorf("the log of a failed build was not kept: %q", log)
	}
}

// A newer push cancels the build that is running for the project.
func TestANewerPushCancelsARunningBuild(t *testing.T) {
	h := newHarness(t)
	p := h.project("web", "{}")
	old := h.push(p, sha("a"))
	h.tick()
	newer := h.push(p, sha("b"))

	h.tick()
	d := h.status(old)
	if d.Status != project.StatusFailed || !strings.Contains(d.Error, "superseded") {
		t.Errorf("the older build: %s %q", d.Status, d.Error)
	}
	if h.runner.cleanedCount(old.String()) == 0 {
		t.Error("the cancelled build's Job and Secret were not removed")
	}
	if h.status(newer).Status != project.StatusBuilding || len(h.runner.started) != 2 {
		t.Errorf("the newer build: %s, %d started", h.status(newer).Status, len(h.runner.started))
	}
}

// Something else can end a build under the orchestrator: a deployment made by
// hand supersedes every unfinished one, and a deleted project takes its builds.
// Its token must not outlive it.
func TestABuildEndedByAnotherPathHasItsJobAndSecretRemoved(t *testing.T) {
	h := newHarness(t)
	p := h.project("web", "{}")
	id := h.push(p, sha("a"))
	h.tick()
	if h.status(id).Status != project.StatusBuilding {
		t.Fatal("not building")
	}

	if _, err := h.svc.Deploy(t.Context(), h.owner, p.ID, project.DeployInput{Image: "reg/web:2"}); err != nil {
		t.Fatal(err)
	}
	if h.status(id).Status != project.StatusFailed {
		t.Fatalf("the manual deployment did not supersede the build: %s", h.status(id).Status)
	}
	h.tick()
	if h.runner.cleanedCount(id.String()) == 0 {
		t.Error("the Job and Secret of a build that something else ended were left")
	}
}

func TestALostJobFailsTheBuild(t *testing.T) {
	h := newHarness(t)
	id := h.push(h.project("web", "{}"), sha("a"))
	h.tick()
	h.runner.results[id.String()] = Result{State: Missing}
	h.tick()
	if d := h.status(id); d.Status != project.StatusFailed || !strings.Contains(d.Error, "gone") {
		t.Errorf("%s %q", d.Status, d.Error)
	}
	if h.runner.cleanedCount(id.String()) == 0 {
		t.Error("a lost build's Secret may be left, so it was not cleaned up")
	}
}

func TestABuildThatRunsTooLongIsEnded(t *testing.T) {
	h := newHarness(t)
	id := h.push(h.project("web", "{}"), sha("a"))
	h.tick()
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	h.orch.now = func() time.Time { return start.Add(25 * time.Minute) }
	h.runner.results[id.String()] = Result{State: Running, StartedAt: start}
	h.tick()
	if d := h.status(id); d.Status != project.StatusFailed || !strings.Contains(d.Error, "longer than") {
		t.Errorf("%s %q", d.Status, d.Error)
	}
	if h.runner.cleanedCount(id.String()) == 0 {
		t.Error("the Job of a build that took too long was not removed")
	}
}

func TestWithoutAnImageInTheRegistryTheBuildFails(t *testing.T) {
	h := newHarness(t)
	id := h.push(h.project("web", "{}"), sha("a"))
	h.tick()
	h.runner.results[id.String()] = Result{State: Succeeded} // but nothing was pushed
	h.tick()
	if d := h.status(id); d.Status != project.StatusFailed || !strings.Contains(d.Error, "not in the registry") {
		t.Errorf("%s %q", d.Status, d.Error)
	}
	if len(h.deployer.deployed) != 0 {
		t.Error("something was deployed without an image")
	}
}

// A registry that cannot be reached is not a failed build: the build is checked again.
func TestAnUnreachableRegistryIsRetried(t *testing.T) {
	h := newHarness(t)
	id := h.push(h.project("web", "{}"), sha("a"))
	h.tick()
	h.runner.results[id.String()] = Result{State: Succeeded}
	h.registry.err = errors.New("connection refused")
	h.tick()
	h.tick()
	if got := h.status(id).Status; got != project.StatusPushing {
		t.Fatalf("status %s, want pushing while the registry is down", got)
	}
	h.registry.err = nil
	h.registry.digests["web/web:"+sha("a")[:12]] = digestA
	h.tick()
	if got := h.status(id).Status; got != project.StatusDeploying {
		t.Errorf("status %s, want deploying once the registry answers", got)
	}
}

func TestATokenThatCannotBeMintedFailsTheBuildWithoutStartingIt(t *testing.T) {
	h := newHarness(t)
	id := h.push(h.project("web", "{}"), sha("a"))
	h.tokens.err = errors.New("POST .../access_tokens: 404 Not Found")
	h.tick()
	d := h.status(id)
	if d.Status != project.StatusFailed || !strings.Contains(d.Error, "app still installed") || strings.Contains(d.Error, "404") {
		t.Errorf("%s %q (the reason must help the user and not leak the API's answer)", d.Status, d.Error)
	}
	if len(h.runner.started) != 0 {
		t.Error("a build was started without a token")
	}
}

func TestABuildThatCannotBeStartedIsCleanedUpAndFailed(t *testing.T) {
	h := newHarness(t)
	id := h.push(h.project("web", "{}"), sha("a"))
	h.runner.startErr = errors.New("forbidden: exceeded quota")
	h.tick()
	if d := h.status(id); d.Status != project.StatusFailed || strings.Contains(d.Error, "quota") {
		t.Errorf("%s %q", d.Status, d.Error)
	}
	if h.runner.cleanedCount(id.String()) == 0 {
		t.Error("a half-started build was not cleaned up")
	}
}

// badConfig is the project service, but every project has a build configuration
// that got past validation somehow: an old row, or a manual edit.
type badConfig struct{ *project.Service }

func (b badConfig) PendingBuilds(ctx context.Context) ([]project.Build, error) {
	builds, err := b.Service.PendingBuilds(ctx)
	for i := range builds {
		builds[i].Project.BuildConfig = []byte(`{"dockerfile":"../../etc/passwd"}`)
	}
	return builds, err
}

func TestAnInvalidBuildConfigurationFailsTheBuild(t *testing.T) {
	h := newHarness(t)
	h.orch.deps = badConfig{h.svc}
	id := h.push(h.project("web", "{}"), sha("a"))
	h.tick()
	if d := h.status(id); d.Status != project.StatusFailed || !strings.Contains(d.Error, "Dockerfile path") {
		t.Errorf("%s %q", d.Status, d.Error)
	}
	if len(h.runner.started) != 0 || len(h.tokens.calls) != 0 {
		t.Error("a token was minted or a build started for an invalid configuration")
	}
}

func TestTheCacheIsImportedOnlyWhenThereIsOne(t *testing.T) {
	h := newHarness(t)
	h.registry.digests["web/web:buildcache"] = digestA
	h.push(h.project("web", "{}"), sha("a"))
	h.tick()
	s := h.runner.started[0]
	if !s.ImportCache || !strings.HasSuffix(s.CacheRef, "/web/web:buildcache") {
		t.Errorf("spec %+v", s)
	}
}

func TestADeploymentMadeFromAnImageIsNotBuilt(t *testing.T) {
	h := newHarness(t)
	p := h.project("web", "{}")
	if _, err := h.svc.Deploy(t.Context(), h.owner, p.ID, project.DeployInput{Image: "reg/web:1"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if len(h.runner.started) != 0 {
		t.Error("a deployment with an image was built")
	}
}

// What a build printed may include a token; it is stored without it.
func TestATokenInTheLogIsNotStored(t *testing.T) {
	h := newHarness(t)
	id := h.push(h.project("web", "{}"), sha("a"))
	h.tick()
	h.runner.logs[id.String()] = "cloning https://x-access-token:ghs_LEAKEDLEAKEDLEAKEDLEAKED@github.com/o/r.git\nuse ghs_LEAKEDLEAKEDLEAKEDLEAKED\n"
	h.tick()
	log, _ := h.store.BuildLog(t.Context(), id)
	if strings.Contains(log, "LEAKED") || !strings.Contains(log, "cloning") {
		t.Errorf("stored log %q", log)
	}
}

// After a restart the orchestrator follows what is running and removes the rest.
func TestARestartFollowsARunningBuildAndSweepsTheRest(t *testing.T) {
	h := newHarness(t)
	id := h.push(h.project("web", "{}"), sha("a"))
	h.tick()

	restarted := NewOrchestrator(h.svc, map[string]Runner{"eu": h.runner}, h.tokens, h.registry, Config{PushHost: "p:5000", PullHost: "localhost:5000"})
	restarted.Sweep(t.Context())
	if len(h.runner.swept) != 1 {
		t.Fatal("no sweep")
	}
	keep := h.runner.swept[0]
	if !keep(id.String()) || keep(uuid.NewString()) {
		t.Error("the sweep must keep the build that is running and nothing else")
	}

	h.runner.results[id.String()] = Result{State: Succeeded}
	h.registry.digests["web/web:"+sha("a")[:12]] = digestA
	restarted.Tick(t.Context())
	if got := h.status(id).Status; got != project.StatusDeploying {
		t.Errorf("the restarted orchestrator left the finished build at %s", got)
	}
}

func TestARegionWithoutABuilderFailsItsBuilds(t *testing.T) {
	h := newHarness(t)
	h.orch.runners = map[string]Runner{}
	id := h.push(h.project("web", "{}"), sha("a"))
	h.tick()
	if d := h.status(id); d.Status != project.StatusFailed || !strings.Contains(d.Error, "no builder") {
		t.Errorf("%s %q", d.Status, d.Error)
	}
}
