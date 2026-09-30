// Package projecttest has test doubles for the project package.
package projecttest

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/vorash/vora-cloud/pkg/project"
)

// MemStore is an in-memory project.Store for tests. It holds no ownership logic
// on purpose: tests must fail when the Service's own check goes away.
type MemStore struct {
	projects map[uuid.UUID]*project.Project
	deps     []*project.Deployment // oldest first
	Env      map[uuid.UUID]map[string][]byte
	// Clock stamps deployments; each write moves it forward by a millisecond.
	Clock time.Time
}

func New() *MemStore {
	return &MemStore{projects: map[uuid.UUID]*project.Project{}, Env: map[uuid.UUID]map[string][]byte{}, Clock: time.Now()}
}

func (m *MemStore) tick() time.Time { m.Clock = m.Clock.Add(time.Millisecond); return m.Clock }

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

func (m *MemStore) ListDeployments(_ context.Context, projectID uuid.UUID) ([]project.Deployment, error) {
	var out []project.Deployment
	for i := len(m.deps) - 1; i >= 0; i-- {
		if m.deps[i].ProjectID == projectID {
			out = append(out, *m.deps[i])
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
