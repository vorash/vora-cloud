// Package deploy is the seam between vora-cloud and the cluster that runs apps.
// Each region has its own Deployer; a Registry resolves a region id to it.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
)

var (
	ErrUnknownRegion = errors.New("unknown region")
	ErrInvalidSpec   = errors.New("invalid spec")
)

// State is where a deployment is in its rollout.
type State string

const (
	StatePending    State = "pending"
	StateRollingOut State = "rolling_out"
	StateReady      State = "ready"
	StateFailed     State = "failed"
	StateNotFound   State = "not_found"
)

// Spec describes one app: what to run and how to reach it.
type Spec struct {
	// Project owns the app; it maps to one namespace, so its apps are isolated
	// from other projects.
	Project string
	// Name identifies the app inside the project.
	Name  string
	Image string
	// Port is the container port serving HTTP.
	Port     int32
	Replicas int32
	Env      map[string]string
	// CPUMillis and MemoryMiB are both the request and the limit, so an app
	// gets what it asked for and no more.
	CPUMillis int64
	MemoryMiB int64
	// Host is the public hostname. Empty means the app is not exposed.
	Host string
	// TLS serves Host over HTTPS.
	TLS bool
	// HealthPath is an HTTP path that answers 2xx when the app is ready.
	// Empty falls back to a TCP check on Port.
	HealthPath string
}

// Status is the observed state of a deployed app.
type Status struct {
	State   State
	Desired int32
	Updated int32
	Ready   int32
	Message string
}

// Deployer applies apps to one region's cluster. Deploy is idempotent:
// applying the same Spec twice changes nothing.
type Deployer interface {
	Deploy(ctx context.Context, spec Spec) error
	Status(ctx context.Context, project, name string) (Status, error)
	// Delete removes one app and leaves the project's other apps alone.
	Delete(ctx context.Context, project, name string) error
	// DeleteProject removes the project and every app in it.
	DeleteProject(ctx context.Context, project string) error
	// Health returns an error when the region's cluster cannot be reached.
	Health(ctx context.Context) error
}

var (
	labelRe  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	hostRe   = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)+[a-z]{2,}$`)
	regionRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,15}$`)
)

// ValidLabel reports whether s can be used as a project or app name.
func ValidLabel(s string) bool { return labelRe.MatchString(s) }

// Validate rejects a Spec that could not be applied, before any cluster call.
func (s Spec) Validate() error {
	switch {
	case !labelRe.MatchString(s.Project):
		return fmt.Errorf("%w: project %q must be a lowercase DNS label", ErrInvalidSpec, s.Project)
	case !labelRe.MatchString(s.Name):
		return fmt.Errorf("%w: name %q must be a lowercase DNS label", ErrInvalidSpec, s.Name)
	case s.Image == "":
		return fmt.Errorf("%w: image is required", ErrInvalidSpec)
	case s.Port < 1 || s.Port > 65535:
		return fmt.Errorf("%w: port %d out of range", ErrInvalidSpec, s.Port)
	case s.Replicas < 0:
		return fmt.Errorf("%w: replicas %d is negative", ErrInvalidSpec, s.Replicas)
	case s.CPUMillis <= 0 || s.MemoryMiB <= 0:
		return fmt.Errorf("%w: cpu and memory must be positive", ErrInvalidSpec)
	case s.Host != "" && !hostRe.MatchString(s.Host):
		return fmt.Errorf("%w: host %q must be a lowercase hostname without scheme or path", ErrInvalidSpec, s.Host)
	case s.TLS && s.Host == "":
		return fmt.Errorf("%w: tls needs a host", ErrInvalidSpec)
	case s.HealthPath != "" && s.HealthPath[0] != '/':
		return fmt.Errorf("%w: health path %q must start with /", ErrInvalidSpec, s.HealthPath)
	}
	for k := range s.Env {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("%w: env name %q is not a valid variable name", ErrInvalidSpec, k)
		}
	}
	return nil
}

// Registry maps a region id to that region's Deployer.
type Registry struct {
	byRegion map[string]Deployer
}

// NewRegistry validates every region id. An unknown region is always an
// error at lookup time, never a default.
func NewRegistry(regions map[string]Deployer) (*Registry, error) {
	if len(regions) == 0 {
		return nil, errors.New("deploy: at least one region is required")
	}
	byRegion := make(map[string]Deployer, len(regions))
	for id, d := range regions {
		if !regionRe.MatchString(id) {
			return nil, fmt.Errorf("deploy: region id %q must be lowercase letters, digits or dashes", id)
		}
		if d == nil {
			return nil, fmt.Errorf("deploy: region %q has no deployer", id)
		}
		byRegion[id] = d
	}
	return &Registry{byRegion: byRegion}, nil
}

func (r *Registry) Deployer(region string) (Deployer, error) {
	d, ok := r.byRegion[region]
	if !ok {
		return nil, fmt.Errorf("%w %q (have %v)", ErrUnknownRegion, region, r.Regions())
	}
	return d, nil
}

// Regions lists the configured region ids, sorted.
func (r *Registry) Regions() []string {
	ids := make([]string, 0, len(r.byRegion))
	for id := range r.byRegion {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
