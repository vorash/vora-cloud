//go:build integration

package database

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vorash/vora-cloud/pkg/project"
)

// Runs the project store against a migrated PostgreSQL (make test-database).
func newTestRepo(t *testing.T) (*ProjectRepository, uuid.UUID) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, err := New(url, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)

	owner := uuid.New()
	if _, err := db.Exec(context.Background(),
		`INSERT INTO users (id, name, email, password_hash) VALUES ($1, 'test', $2, 'x')`, owner, owner.String()+"@example.com"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, owner); err != nil {
			t.Error(err)
		}
	})
	return NewProjectRepository(db), owner
}

func newProject(owner uuid.UUID, slug string) *project.Project {
	return &project.Project{
		OwnerID: owner, Name: slug, Slug: slug, Region: "eu", DefaultBranch: "main", BuildConfig: []byte("{}"),
		Port: 80, Replicas: 1, CPUMillis: 250, MemoryMiB: 256,
	}
}

func TestProjectRepository(t *testing.T) {
	ctx := context.Background()
	repo, owner := newTestRepo(t)
	slug := "it-" + uuid.NewString()[:8]

	p := newProject(owner, slug)
	if err := repo.CreateProject(ctx, p); err != nil || p.ID == uuid.Nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.CreateProject(ctx, newProject(owner, slug)); !errors.Is(err, project.ErrSlugTaken) {
		t.Errorf("duplicate slug: %v, want ErrSlugTaken", err)
	}
	got, err := repo.GetProject(ctx, p.ID)
	if err != nil || got.Slug != slug || got.Port != 80 || string(got.BuildConfig) != "{}" {
		t.Errorf("get: %+v, %v", got, err)
	}
	if _, err := repo.GetProject(ctx, uuid.New()); !errors.Is(err, project.ErrNotFound) {
		t.Errorf("missing project: %v", err)
	}
	if list, err := repo.ListProjects(ctx, owner); err != nil || len(list) != 1 {
		t.Errorf("list: %d, %v", len(list), err)
	}

	// Deployments: newest first, and a move only from the expected status.
	d1 := &project.Deployment{ProjectID: p.ID, Status: project.StatusQueued, Image: "i:1", Region: "eu"}
	d2 := &project.Deployment{ProjectID: p.ID, Status: project.StatusQueued, Image: "i:2", Region: "eu"}
	for _, d := range []*project.Deployment{d1, d2} {
		if err := repo.CreateDeployment(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	deps, _ := repo.ListDeployments(ctx, p.ID)
	if len(deps) != 2 || deps[0].ID != d2.ID {
		t.Errorf("list order: %+v", deps)
	}
	if err := repo.Move(ctx, d1.ID, project.StatusQueued, project.StatusDeploying, ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.Move(ctx, d1.ID, project.StatusQueued, project.StatusFailed, "x"); !errors.Is(err, project.ErrConflict) {
		t.Errorf("stale move: %v, want ErrConflict", err)
	}
	if err := repo.Move(ctx, d1.ID, project.StatusDeploying, project.StatusFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	got1, _ := repo.GetDeployment(ctx, d1.ID)
	if got1.Status != project.StatusFailed || got1.Error != "boom" || got1.StartedAt == nil || got1.FinishedAt == nil {
		t.Errorf("after moves: %+v", got1)
	}
	if active, _ := repo.ListActive(ctx); len(active) < 1 {
		t.Error("d2 is still queued and must be active")
	}

	// Env values are stored as given (sealed by the service) and upserted.
	if err := repo.SetEnv(ctx, p.ID, "A", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetEnv(ctx, p.ID, "A", []byte("two")); err != nil {
		t.Fatal(err)
	}
	if env, _ := repo.ListEnv(ctx, p.ID); len(env) != 1 || string(env["A"]) != "two" {
		t.Errorf("env: %v", env)
	}
	if err := repo.DeleteEnv(ctx, p.ID, "A"); err != nil {
		t.Fatal(err)
	}
	if env, _ := repo.ListEnv(ctx, p.ID); len(env) != 0 {
		t.Errorf("env after delete: %v", env)
	}

	if err := repo.DeleteProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetDeployment(ctx, d2.ID); !errors.Is(err, project.ErrNotFound) {
		t.Errorf("deployments must go with the project: %v", err)
	}
	if err := repo.DeleteProject(ctx, p.ID); !errors.Is(err, project.ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
}

// A session is found by its token hash while it is valid, and not after it
// expires, is deleted, or its user is deactivated.
func TestSessionRepository(t *testing.T) {
	ctx := context.Background()
	repo, owner := newTestRepo(t)
	sessions := NewSessionRepository(repo.db)
	hash := "hash-" + uuid.NewString()

	if err := sessions.Create(ctx, owner, hash, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := sessions.Get(ctx, hash)
	if err != nil || got == nil || got.UserID != owner || got.UserName != "test" {
		t.Fatalf("get: %+v, %v", got, err)
	}
	if got, _ := sessions.Get(ctx, "unknown"); got != nil {
		t.Error("an unknown hash found a session")
	}

	if _, err := repo.db.Exec(ctx, `UPDATE users SET is_active = false WHERE id = $1`, owner); err != nil {
		t.Fatal(err)
	}
	if got, _ := sessions.Get(ctx, hash); got != nil {
		t.Error("deactivating the user did not end their session")
	}
	if _, err := repo.db.Exec(ctx, `UPDATE users SET is_active = true WHERE id = $1`, owner); err != nil {
		t.Fatal(err)
	}

	expired := "hash-" + uuid.NewString()
	if err := sessions.Create(ctx, owner, expired, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, _ := sessions.Get(ctx, expired); got != nil {
		t.Error("an expired session was found")
	}
	if err := sessions.DeleteExpired(ctx); err != nil {
		t.Fatal(err)
	}

	if err := sessions.Delete(ctx, hash); err != nil {
		t.Fatal(err)
	}
	if got, _ := sessions.Get(ctx, hash); got != nil {
		t.Error("a deleted session was found")
	}
}
