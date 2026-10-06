//go:build integration

package database

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vorash/vora-cloud/pkg/project"
)

// Runs against a migrated PostgreSQL (make test-database).
func TestProjectGitHubLinkRoundTrips(t *testing.T) {
	repo, owner := newTestRepo(t)
	ctx := context.Background()

	linked := newProject(owner, "gh-linked")
	linked.Repo, linked.GitHubInstallationID, linked.GitHubRepoID = "octo/web", 168273140, 99
	if err := repo.CreateProject(ctx, linked); err != nil {
		t.Fatal(err)
	}
	plain := newProject(owner, "gh-plain")
	plain.Repo = "someone/elses-repo" // a label, as the API allows
	if err := repo.CreateProject(ctx, plain); err != nil {
		t.Fatal(err)
	}

	got, err := repo.GetProject(ctx, linked.ID)
	if err != nil || got.GitHubInstallationID != 168273140 || got.GitHubRepoID != 99 || got.Repo != "octo/web" {
		t.Errorf("linked project: %+v, %v", got, err)
	}
	// A project with no link reads back as zero, and is NULL in the table, so the
	// index and the both-or-neither rule see "none" rather than a repository 0.
	got, err = repo.GetProject(ctx, plain.ID)
	if err != nil || got.GitHubInstallationID != 0 || got.GitHubRepoID != 0 {
		t.Errorf("plain project: %+v, %v", got, err)
	}
	var nulls int
	if err := repo.db.QueryRow(ctx, `SELECT count(*) FROM projects WHERE id = $1 AND github_installation_id IS NULL AND github_repo_id IS NULL`, plain.ID).Scan(&nulls); err != nil || nulls != 1 {
		t.Errorf("an unlinked project is not NULL in the table: %d, %v", nulls, err)
	}
	list, err := repo.ListProjects(ctx, owner)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v, %v", list, err)
	}
	for _, p := range list {
		if (p.Slug == "gh-linked") != (p.GitHubRepoID != 0) {
			t.Errorf("list scanned %s wrongly: %+v", p.Slug, p)
		}
	}
}

// Half a link is refused by the database itself, not only by the service.
func TestProjectGitHubLinkIsBothOrNeither(t *testing.T) {
	repo, owner := newTestRepo(t)
	ctx := context.Background()
	p := newProject(owner, "gh-half")
	if err := repo.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	for name, sql := range map[string]string{
		"installation only": `UPDATE projects SET github_installation_id = 5 WHERE id = $1`,
		"repository only":   `UPDATE projects SET github_repo_id = 9 WHERE id = $1`,
	} {
		_, err := repo.db.Exec(ctx, sql, p.ID)
		pgErr, ok := err.(*pgconn.PgError)
		if !ok || pgErr.ConstraintName != "projects_github_link" {
			t.Errorf("%s: err = %v, want a projects_github_link violation", name, err)
		}
	}
}

func TestProjectsAreFoundByGitHubIDsOnly(t *testing.T) {
	repo, owner := newTestRepo(t)
	ctx := context.Background()
	inst, rid := int64(900000001), int64(900000002)

	linked := newProject(owner, "gh-push-linked")
	linked.Repo, linked.GitHubInstallationID, linked.GitHubRepoID = "octo/web", inst, rid
	other := newProject(owner, "gh-push-otherinst")
	other.GitHubInstallationID, other.GitHubRepoID = inst+1, rid
	label := newProject(owner, "gh-push-label")
	label.Repo = "octo/web" // the label alone never matches
	for _, p := range []*project.Project{linked, other, label} {
		if err := repo.CreateProject(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	got, err := repo.ListProjectsByGitHub(ctx, inst, rid)
	if err != nil || len(got) != 1 || got[0].ID != linked.ID {
		t.Fatalf("found %+v, %v; want only the project with both ids", got, err)
	}
	if got, _ := repo.ListProjectsByGitHub(ctx, 0, 0); len(got) != 0 {
		t.Errorf("zero ids matched %d projects (NULL must never match)", len(got))
	}
}

func TestAForgottenGitHubDeliveryIsRecordedAgain(t *testing.T) {
	deliveries, db := newTestDeliveries(t)
	ctx := context.Background()
	id := uuid.NewString()
	t.Cleanup(func() { _, _ = db.Exec(ctx, `DELETE FROM github_deliveries WHERE delivery_id = $1`, id) })

	if fresh, err := deliveries.Record(ctx, id, "push"); err != nil || !fresh {
		t.Fatalf("Record = %v, %v", fresh, err)
	}
	if err := deliveries.Forget(ctx, id); err != nil {
		t.Fatal(err)
	}
	if fresh, err := deliveries.Record(ctx, id, "push"); err != nil || !fresh {
		t.Errorf("after Forget: Record = %v, %v; want it handled as new", fresh, err)
	}
	if err := deliveries.Forget(ctx, uuid.NewString()); err != nil {
		t.Errorf("forgetting an unknown delivery: %v", err)
	}
}
