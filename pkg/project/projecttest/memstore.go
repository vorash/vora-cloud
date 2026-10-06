// Package projecttest has test doubles for the project package.
package projecttest

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/vorash/vora-cloud/pkg/project"
)

// MemStore is an in-memory project.Store for tests. It holds no ownership logic
// on purpose: tests must fail when the Service's own check goes away.
type MemStore struct {
	// mu makes Move and ListDeployments safe together: a test settles a rollout
	// while a stream polls it. The rest of the store is for one goroutine.
	mu       sync.Mutex
	projects map[uuid.UUID]*project.Project
	deps     []*project.Deployment // oldest first
	Env      map[uuid.UUID]map[string][]byte
	logs     map[uuid.UUID]string
	// Clock stamps deployments; each write moves it forward by a millisecond.
	Clock time.Time
}

func New() *MemStore {
	return &MemStore{projects: map[uuid.UUID]*project.Project{}, Env: map[uuid.UUID]map[string][]byte{}, logs: map[uuid.UUID]string{}, Clock: time.Now()}
}

// tick advances the clock. Times are whole microseconds, as in the database,
// so a cursor made from one compares equal to it.
func (m *MemStore) tick() time.Time {
	m.Clock = m.Clock.Add(time.Millisecond).Truncate(time.Microsecond)
	return m.Clock
}

func (m *MemStore) CreateProject(_ context.Context, p *project.Project) error {
	for _, q := range m.projects {
		if q.Slug == p.Slug {
			return project.ErrSlugTaken
		}
	}
	p.ID = uuid.New()
	m.projects[p.ID] = p
	return nil
}

func (m *MemStore) ListProjectsByGitHub(_ context.Context, installationID, repoID int64) ([]project.Project, error) {
	var out []project.Project
	for _, p := range m.projects {
		if p.GitHubInstallationID == installationID && p.GitHubRepoID == repoID && repoID != 0 {
			out = append(out, *p)
		}
	}
	return out, nil
}

func (m *MemStore) GetProject(_ context.Context, id uuid.UUID) (*project.Project, error) {
	if p, ok := m.projects[id]; ok {
		c := *p
		return &c, nil
	}
	return nil, project.ErrNotFound
}

func (m *MemStore) ListProjects(_ context.Context, owner uuid.UUID) ([]project.Project, error) {
	var out []project.Project
	for _, p := range m.projects {
		if p.OwnerID == owner {
			out = append(out, *p)
		}
	}
	return out, nil
}

func (m *MemStore) DeleteProject(_ context.Context, id uuid.UUID) error {
	if _, ok := m.projects[id]; !ok {
		return project.ErrNotFound
	}
	delete(m.projects, id)
	return nil
}

func (m *MemStore) CreateDeployment(_ context.Context, d *project.Deployment) error {
	d.ID = uuid.New()
	d.CreatedAt, d.UpdatedAt = m.tick(), m.Clock
	c := *d
	m.deps = append(m.deps, &c)
	return nil
}

func (m *MemStore) find(id uuid.UUID) *project.Deployment {
	for _, d := range m.deps {
		if d.ID == id {
			return d
		}
	}
	return nil
}

func (m *MemStore) GetDeployment(_ context.Context, id uuid.UUID) (*project.Deployment, error) {
	if d := m.find(id); d != nil {
		c := *d
		return &c, nil
	}
	return nil, project.ErrNotFound
}

func (m *MemStore) ListDeployments(_ context.Context, projectID uuid.UUID, after *project.Cursor, limit int) ([]project.Deployment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []project.Deployment
	for i := len(m.deps) - 1; i >= 0 && len(out) < limit; i-- {
		d := m.deps[i]
		if d.ProjectID != projectID {
			continue
		}
		// The same condition as the database: older than the cursor, or the
		// same instant with a larger id.
		if after != nil {
			older := d.CreatedAt.Before(after.CreatedAt) || d.CreatedAt.Equal(after.CreatedAt) && d.ID.String() > after.ID.String()
			if !older {
				continue
			}
		}
		out = append(out, *d)
	}
	return out, nil
}

func (m *MemStore) LatestDeployments(_ context.Context, owner uuid.UUID) (map[uuid.UUID]project.Deployment, error) {
	out := map[uuid.UUID]project.Deployment{}
	for i := len(m.deps) - 1; i >= 0; i-- { // newest first
		d := m.deps[i]
		if p, ok := m.projects[d.ProjectID]; ok && p.OwnerID == owner {
			if _, seen := out[d.ProjectID]; !seen {
				out[d.ProjectID] = *d
			}
		}
	}
	return out, nil
}

func (m *MemStore) ListActive(context.Context) ([]project.Deployment, error) {
	var out []project.Deployment
	for _, d := range m.deps {
		if !d.Status.Final() {
			out = append(out, *d)
		}
	}
	return out, nil
}

func (m *MemStore) Move(_ context.Context, id uuid.UUID, from, to project.Status, errText string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.find(id)
	if d == nil {
		return project.ErrNotFound
	}
	if d.Status != from {
		return project.ErrConflict
	}
	d.Status, d.Error, d.UpdatedAt = to, errText, m.tick()
	return nil
}

func (m *MemStore) SetEnv(_ context.Context, p uuid.UUID, name string, sealed []byte) error {
	if m.Env[p] == nil {
		m.Env[p] = map[string][]byte{}
	}
	m.Env[p][name] = sealed
	return nil
}

func (m *MemStore) ListEnv(_ context.Context, p uuid.UUID) (map[string][]byte, error) {
	out := map[string][]byte{}
	for k, v := range m.Env[p] {
		out[k] = v
	}
	return out, nil
}

func (m *MemStore) DeleteEnv(_ context.Context, p uuid.UUID, name string) error {
	delete(m.Env[p], name)
	return nil
}

func (m *MemStore) SetImage(_ context.Context, id uuid.UUID, image, url string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.find(id)
	if d == nil {
		return project.ErrNotFound
	}
	d.Image, d.URL = image, url
	d.UpdatedAt = m.tick()
	return nil
}

func (m *MemStore) ListImages(context.Context) ([]project.ImageUse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []project.ImageUse
	for _, d := range m.deps {
		if d.Image == "" || d.ImagePrunedAt != nil {
			continue
		}
		p := m.projects[d.ProjectID]
		if p == nil {
			continue
		}
		out = append(out, project.ImageUse{DeploymentID: d.ID, ProjectID: d.ProjectID, Slug: p.Slug, Image: d.Image, Status: d.Status, CreatedAt: d.CreatedAt})
	}
	return out, nil
}

func (m *MemStore) MarkImagePruned(_ context.Context, projectID uuid.UUID, image string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.tick()
	for _, d := range m.deps {
		if d.ProjectID == projectID && d.Image == image && d.ImagePrunedAt == nil {
			d.ImagePrunedAt = &now
		}
	}
	return nil
}

func (m *MemStore) BuildLog(_ context.Context, id uuid.UUID) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.find(id) == nil {
		return "", project.ErrNotFound
	}
	return m.logs[id], nil
}

func (m *MemStore) SetBuildLog(_ context.Context, id uuid.UUID, log string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.find(id) == nil {
		return project.ErrNotFound
	}
	m.logs[id] = log
	return nil
}
