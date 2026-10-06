package project

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// BuildSettings say how a project's repository is built. They are read from the
// project's build configuration; the zero configuration builds `Dockerfile` at
// the root of the repository.
type BuildSettings struct {
	// Dockerfile is the path of the Dockerfile, relative to Context.
	Dockerfile string `json:"dockerfile,omitempty"`
	// Context is the folder of the repository the image is built in. Empty is the
	// root of the repository.
	Context string `json:"context,omitempty"`
}

// maxPath bounds a path in the build settings.
const maxPath = 200

var pathRe = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]*$`)

// ValidBuildPath reports whether p is a relative path that stays inside the
// repository: no leading or doubled or trailing slash, and no ".." segment. The
// paths end up in a build's arguments, so they are strict.
func ValidBuildPath(p string) bool {
	if len(p) > maxPath || !pathRe.MatchString(p) || strings.Contains(p, "//") || strings.HasSuffix(p, "/") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "." {
			return false
		}
	}
	return true
}

// ParseBuildSettings reads a project's build configuration, with the defaults
// filled in. An unknown field is an error, so a misspelt one is not silently
// ignored.
func ParseBuildSettings(raw []byte) (BuildSettings, error) {
	var b BuildSettings
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&b); err != nil {
			return BuildSettings{}, fmt.Errorf("the build configuration is not valid: %v", err)
		}
	}
	if b.Dockerfile == "" {
		b.Dockerfile = "Dockerfile"
	}
	if !ValidBuildPath(b.Dockerfile) {
		return BuildSettings{}, fmt.Errorf("the Dockerfile path %q must be a relative path inside the repository", b.Dockerfile)
	}
	if b.Context != "" && !ValidBuildPath(b.Context) {
		return BuildSettings{}, fmt.Errorf("the build context %q must be a folder inside the repository", b.Context)
	}
	return b, nil
}

// Build is a deployment that is waiting for a build, or being built, with its project.
type Build struct {
	Deployment Deployment
	Project    Project
}

// maxBuildLog is how much of a build's log is kept: its tail.
const maxBuildLog = 256 << 10

// Observer hears of every change of a deployment's status, with its project. It
// is called from the goroutine that made the change, so it must not block.
type Observer func(ctx context.Context, d Deployment, p Project)

// Observe sets the observer. Call it before the service is used.
func (s *Service) Observe(o Observer) { s.observer = o }

func (s *Service) notify(ctx context.Context, d *Deployment) {
	if s.observer == nil {
		return
	}
	p, err := s.store.GetProject(ctx, d.ProjectID)
	if err != nil {
		log.Printf("deployment %s: observer: loading its project: %v", d.ID, err)
		return
	}
	s.observer(ctx, *d, *p)
}

// PendingBuilds returns the deployments that are queued for a build, being built
// or being checked after a build, oldest first. Only a deployment with no image
// on a project tied to a repository is a build: one made from an image has
// nothing to build.
func (s *Service) PendingBuilds(ctx context.Context) ([]Build, error) {
	active, err := s.store.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	projects := map[uuid.UUID]*Project{}
	var out []Build
	for _, d := range active {
		if d.Image != "" || (d.Status != StatusQueued && d.Status != StatusBuilding && d.Status != StatusPushing) {
			continue
		}
		p, ok := projects[d.ProjectID]
		if !ok {
			if p, err = s.store.GetProject(ctx, d.ProjectID); err != nil {
				if errors.Is(err, ErrNotFound) {
					continue // the project was deleted under it
				}
				return nil, err
			}
			projects[d.ProjectID] = p
		}
		if p.GitHubRepoID == 0 {
			continue
		}
		out = append(out, Build{Deployment: d, Project: *p})
	}
	return out, nil
}

// buildStep moves a deployment from one status to the next. ErrConflict means
// something else changed it first, and the caller must stop.
func (s *Service) buildStep(ctx context.Context, id uuid.UUID, from, to Status) error {
	d, err := s.store.GetDeployment(ctx, id)
	if err != nil {
		return err
	}
	if d.Status != from {
		return fmt.Errorf("%w: deployment %s is %s, not %s", ErrConflict, id, d.Status, from)
	}
	return s.move(ctx, d, to, "")
}

// StartBuild claims a queued deployment for building.
func (s *Service) StartBuild(ctx context.Context, id uuid.UUID) error {
	return s.buildStep(ctx, id, StatusQueued, StatusBuilding)
}

// BeginPush marks a built image as being checked in the registry.
func (s *Service) BeginPush(ctx context.Context, id uuid.UUID) error {
	return s.buildStep(ctx, id, StatusBuilding, StatusPushing)
}

// FinishBuild deploys the image a build pushed. The deployment must be pushing.
// It goes through the same rollout as a deployment made from an image, so the
// older unfinished deployments are superseded by it, and a failure to apply it
// is recorded on the deployment.
func (s *Service) FinishBuild(ctx context.Context, id uuid.UUID, image string) error {
	d, err := s.store.GetDeployment(ctx, id)
	if err != nil {
		return err
	}
	if d.Status != StatusPushing {
		return fmt.Errorf("%w: deployment %s is %s, not pushing", ErrConflict, id, d.Status)
	}
	p, err := s.store.GetProject(ctx, d.ProjectID)
	if err != nil {
		return err
	}
	deployer, err := s.regions.Deployer(p.Region)
	if err != nil {
		return err
	}
	env, err := s.env(ctx, p.ID)
	if err != nil {
		return err
	}
	spec := s.spec(p, image)
	spec.Env = env
	if err := spec.Validate(); err != nil {
		return s.move(ctx, d, StatusFailed, fmt.Sprintf("the built image cannot be deployed: %v", err))
	}
	url := ""
	if spec.Host != "" {
		url = "https://" + spec.Host
	}
	if err := s.store.SetImage(ctx, d.ID, image, url); err != nil {
		return err
	}
	d.Image, d.URL = image, url
	_, err = s.rollout(ctx, deployer, p, d, spec)
	return err
}

// FailBuild ends a deployment that has not finished with a reason. ErrConflict
// means it was already settled or moved on.
func (s *Service) FailBuild(ctx context.Context, id uuid.UUID, reason string) error {
	d, err := s.store.GetDeployment(ctx, id)
	if err != nil {
		return err
	}
	if d.Status.Final() {
		return fmt.Errorf("%w: deployment %s already finished", ErrConflict, id)
	}
	return s.move(ctx, d, StatusFailed, reason)
}

// RecordBuildLog stores the tail of a build's log. Postgres text holds neither
// invalid UTF-8 nor NUL, and a build can print both.
func (s *Service) RecordBuildLog(ctx context.Context, id uuid.UUID, text string) error {
	if len(text) > maxBuildLog {
		text = text[len(text)-maxBuildLog:]
	}
	text = strings.ToValidUTF8(strings.ReplaceAll(text, "\x00", ""), "�")
	return s.store.SetBuildLog(ctx, id, text)
}

// BuildLog returns the build log of one of the owner's deployments.
func (s *Service) BuildLog(ctx context.Context, owner, id uuid.UUID) (string, error) {
	if _, err := s.GetDeployment(ctx, owner, id); err != nil {
		return "", err
	}
	return s.store.BuildLog(ctx, id)
}
