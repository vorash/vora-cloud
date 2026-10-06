//go:build integration

package database

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
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
