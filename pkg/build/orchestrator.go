package build

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/vorash/vora-cloud/pkg/project"
)

// Config tunes an Orchestrator.
type Config struct {
	// PushHost is the registry as a build's pod reaches it, and PullHost as the
	// node's container runtime does (a mirror of the same registry).
	PushHost, PullHost string
	// Deadline is how long a build may run before the orchestrator ends it, as a
	// backstop to the Job's own deadline.
	Deadline time.Duration
	// MaxLog is how much of a build's log is kept.
	MaxLog int
}

func (c Config) withDefaults() Config {
	if c.Deadline == 0 {
		c.Deadline = 20 * time.Minute
	}
	if c.MaxLog == 0 {
		c.MaxLog = 256 << 10
	}
	return c
}

// Orchestrator runs the builds of queued deployments, one at a time. All its
// state is in the deployments and in the Jobs, so a restart picks up where it
// was: a build that is running is followed, and one that is gone is failed.
type Orchestrator struct {
	deps     Deployments
	runners  map[string]Runner // by region
	tokens   Tokens
	registry Registry
	cfg      Config
	now      func() time.Time
	logged   map[string]string // the log last stored for a build, to skip an unchanged one
	started  map[string]string // the builds this process started, by id, with their region
}

func NewOrchestrator(deps Deployments, runners map[string]Runner, tokens Tokens, registry Registry, cfg Config) *Orchestrator {
	return &Orchestrator{deps: deps, runners: runners, tokens: tokens, registry: registry, cfg: cfg.withDefaults(), now: time.Now, logged: map[string]string{}, started: map[string]string{}}
}

// Run sweeps what a restart left behind, then settles builds every interval until
// ctx is done.
func (o *Orchestrator) Run(ctx context.Context, interval time.Duration) {
	o.Sweep(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		o.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep removes the Jobs and Secrets of builds that are not running: what a
// build left behind when the service stopped in the middle of it.
func (o *Orchestrator) Sweep(ctx context.Context) {
	builds, err := o.deps.PendingBuilds(ctx)
	if err != nil {
		log.Printf("build: sweep: %v", err)
		return
	}
	running := map[string]bool{}
	for _, b := range builds {
		if b.Deployment.Status == project.StatusBuilding || b.Deployment.Status == project.StatusPushing {
			running[b.Deployment.ID.String()] = true
		}
	}
	for region, r := range o.runners {
		if err := r.Sweep(ctx, func(id string) bool { return running[id] }); err != nil {
			log.Printf("build: sweep of %s: %v", region, err)
		}
	}
}

// Tick makes one pass: it follows the build that is running, and starts the next
// queued one if none is.
func (o *Orchestrator) Tick(ctx context.Context) {
	builds, err := o.deps.PendingBuilds(ctx)
	if err != nil {
		log.Printf("build: %v", err)
		return
	}
	o.releaseSettled(ctx, builds)
	busy := false
	for _, b := range builds {
		if b.Deployment.Status == project.StatusBuilding || b.Deployment.Status == project.StatusPushing {
			if o.follow(ctx, b, builds) {
				busy = true
			}
		}
	}
	if busy {
		return // one build at a time
	}
	for _, b := range builds {
		if b.Deployment.Status != project.StatusQueued {
			continue
		}
		// A queued build whose project has a newer queued one is already failed
		// as superseded when the newer one is queued, so this is the newest.
		o.start(ctx, b)
		return
	}
}

// cacheTag is the tag of a project's build cache in the registry.
const cacheTag = "buildcache"

// releaseSettled removes the Job and Secret of a build this process started and
// that is no longer building or being checked: something else ended it (a manual
// deployment superseded it, or its project was deleted), and its token must not
// outlive it.
func (o *Orchestrator) releaseSettled(ctx context.Context, builds []project.Build) {
	current := map[string]bool{}
	for _, b := range builds {
		if b.Deployment.Status == project.StatusBuilding || b.Deployment.Status == project.StatusPushing {
			current[b.Deployment.ID.String()] = true
		}
	}
	for id, region := range o.started {
		if current[id] {
			continue
		}
		if r, ok := o.runners[region]; ok {
			o.cleanup(ctx, r, id)
		}
		delete(o.started, id)
	}
}

func repoPath(slug string) string { return slug + "/" + slug }

func tagOf(sha string) string { return sha[:12] }

// start claims a queued build, then creates its Secret and Job. The claim comes
// first: it is a compare-and-set on the status, so two passes never start one build.
func (o *Orchestrator) start(ctx context.Context, b project.Build) {
	id := b.Deployment.ID.String()
	r, ok := o.runners[b.Project.Region]
	if !ok {
		o.fail(ctx, b, fmt.Sprintf("no builder is set up for the region %q", b.Project.Region))
		return
	}
	settings, err := project.ParseBuildSettings(b.Project.BuildConfig)
	if err != nil {
		o.fail(ctx, b, err.Error())
		return
	}
	if err := o.deps.StartBuild(ctx, b.Deployment.ID); err != nil {
		if !errors.Is(err, project.ErrConflict) {
			log.Printf("build %s: claiming it: %v", id, err)
		}
		return
	}
	token, err := o.tokens.RepoToken(ctx, b.Project.GitHubInstallationID, b.Project.GitHubRepoID)
	if err != nil {
		log.Printf("build %s: token: %v", id, err)
		o.fail(ctx, b, "GitHub did not give the build access to the repository. Is the app still installed on it?")
		return
	}
	path := repoPath(b.Project.Slug)
	// The cache is imported only if there is one: the first build of a project has none.
	_, cacheErr := o.registry.Digest(ctx, path, cacheTag)
	spec := Spec{
		ID: id, Repo: b.Project.Repo, CommitSHA: b.Deployment.CommitSHA,
		Context: settings.Context, Dockerfile: settings.Dockerfile,
		PushRef:  o.cfg.PushHost + "/" + path + ":" + tagOf(b.Deployment.CommitSHA),
		CacheRef: o.cfg.PushHost + "/" + path + ":" + cacheTag, ImportCache: cacheErr == nil,
		Token: token,
	}
	if err := r.Start(ctx, spec); err != nil {
		log.Printf("build %s: starting it: %v", id, err)
		o.cleanup(ctx, r, id)
		o.fail(ctx, b, "the build could not be started")
		return
	}
	o.started[id] = b.Project.Region
	log.Printf("build %s: started for project %s at %.7s", id, b.Project.Slug, b.Deployment.CommitSHA)
}

// follow settles a build that is running. It reports whether the build still
// holds the builder: false once it has finished, failed or been cancelled.
func (o *Orchestrator) follow(ctx context.Context, b project.Build, all []project.Build) bool {
	id := b.Deployment.ID.String()
	r, ok := o.runners[b.Project.Region]
	if !ok {
		o.fail(ctx, b, fmt.Sprintf("no builder is set up for the region %q", b.Project.Region))
		return false
	}
	if b.Deployment.Status == project.StatusBuilding && superseded(b, all) {
		o.cleanup(ctx, r, id)
		o.fail(ctx, b, "superseded by a newer push")
		return false
	}
	res, err := r.Status(ctx, id)
	if err != nil {
		log.Printf("build %s: %v", id, err)
		return true // the cluster may be briefly unreachable
	}
	switch res.State {
	case Missing:
		o.cleanup(ctx, r, id) // the Secret may be left
		o.fail(ctx, b, "the build's job is gone")
		return false
	case Running:
		o.recordLog(ctx, r, id)
		if !res.StartedAt.IsZero() && o.now().Sub(res.StartedAt) > o.cfg.Deadline {
			o.cleanup(ctx, r, id)
			o.fail(ctx, b, fmt.Sprintf("the build took longer than %s", o.cfg.Deadline))
			return false
		}
		return true
	case Failed:
		text := o.recordLog(ctx, r, id)
		o.cleanup(ctx, r, id)
		o.fail(ctx, b, failureReason(res, text))
		return false
	default: // Succeeded
		return o.finish(ctx, b, r)
	}
}

// finish checks that the build's image is in the registry and deploys it by its
// digest, so that another build re-tagging the image cannot change what runs.
func (o *Orchestrator) finish(ctx context.Context, b project.Build, r Runner) bool {
	id := b.Deployment.ID.String()
	o.recordLog(ctx, r, id)
	if b.Deployment.Status == project.StatusBuilding {
		if err := o.deps.BeginPush(ctx, b.Deployment.ID); err != nil {
			if errors.Is(err, project.ErrConflict) {
				o.cleanup(ctx, r, id)
				return false
			}
			log.Printf("build %s: %v", id, err)
			return true
		}
	}
	path := repoPath(b.Project.Slug)
	digest, err := o.registry.Digest(ctx, path, tagOf(b.Deployment.CommitSHA))
	switch {
	case errors.Is(err, ErrNoImage):
		o.cleanup(ctx, r, id)
		o.fail(ctx, b, "the build finished but its image is not in the registry")
		return false
	case err != nil:
		log.Printf("build %s: %v", id, err)
		return true // the registry may be briefly unreachable; the build stays pushing
	}
	if err := o.deps.FinishBuild(ctx, b.Deployment.ID, o.cfg.PullHost+"/"+path+"@"+digest); err != nil {
		if errors.Is(err, project.ErrConflict) {
			o.cleanup(ctx, r, id)
			return false
		}
		log.Printf("build %s: deploying it: %v", id, err)
		return true
	}
	o.cleanup(ctx, r, id)
	log.Printf("build %s: deployed %s@%.19s", id, path, digest)
	return false
}

// superseded reports whether a newer deployment of the same project is queued.
func superseded(b project.Build, all []project.Build) bool {
	for _, other := range all {
		if other.Project.ID == b.Project.ID && other.Deployment.Status == project.StatusQueued &&
			other.Deployment.CreatedAt.After(b.Deployment.CreatedAt) {
			return true
		}
	}
	return false
}

// recordLog stores a build's log if it changed, and returns it. A token that
// found its way into the log is removed first.
func (o *Orchestrator) recordLog(ctx context.Context, r Runner, id string) string {
	text, err := r.Logs(ctx, id, o.cfg.MaxLog)
	if err != nil {
		log.Printf("build %s: %v", id, err)
		return ""
	}
	text = Redact(text)
	if text == "" || text == o.logged[id] {
		return text
	}
	uid, err := parseID(id)
	if err != nil {
		return text
	}
	if err := o.deps.RecordBuildLog(ctx, uid, text); err != nil {
		log.Printf("build %s: storing its log: %v", id, err)
		return text
	}
	o.logged[id] = text
	return text
}

// cleanup removes a build's Job and Secret. Every way a build can end calls it.
func (o *Orchestrator) cleanup(ctx context.Context, r Runner, id string) {
	if err := r.Cleanup(ctx, id); err != nil {
		log.Printf("build %s: cleaning up: %v", id, err)
	}
	delete(o.logged, id)
	delete(o.started, id)
}

// fail ends a build with a reason. ErrConflict means something else got there
// first (a newer deployment superseded it), which is fine.
func (o *Orchestrator) fail(ctx context.Context, b project.Build, reason string) {
	if len(reason) > 1000 {
		reason = reason[:1000]
	}
	if err := o.deps.FailBuild(ctx, b.Deployment.ID, reason); err != nil && !errors.Is(err, project.ErrConflict) {
		log.Printf("build %s: failing it: %v", b.Deployment.ID, err)
	}
	delete(o.logged, b.Deployment.ID.String())
	delete(o.started, b.Deployment.ID.String())
}

// failureReason says why a build failed: the cluster's reason, and the last lines
// of what BuildKit printed.
func failureReason(res Result, text string) string {
	reason := "the build failed"
	if res.Reason != "" {
		reason += " (" + res.Reason + ")"
	}
	var tail []string
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			tail = append(tail, line)
		}
	}
	if n := len(tail); n > 3 {
		tail = tail[n-3:]
	}
	for i, line := range tail {
		if len(line) > 200 {
			tail[i] = line[:200]
		}
	}
	if len(tail) > 0 {
		reason += ": " + strings.Join(tail, " | ")
	}
	return reason
}
