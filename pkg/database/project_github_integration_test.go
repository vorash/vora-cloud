//go:build integration

package database

import (
	"context"
	"errors"
	"strings"
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

// The build log is a column of its own, and listing deployments never reads it.
func TestBuildLogAndImageOfADeployment(t *testing.T) {
	repo, owner := newTestRepo(t)
	ctx := context.Background()
	p := newProject(owner, "gh-build-log")
	p.GitHubInstallationID, p.GitHubRepoID = 900000003, 900000004
	if err := repo.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	d := &project.Deployment{ProjectID: p.ID, CommitSHA: strings.Repeat("a", 40), Ref: "refs/heads/main", Status: project.StatusQueued, Region: "eu"}
	if err := repo.CreateDeployment(ctx, d); err != nil {
		t.Fatal(err)
	}

	if log, err := repo.BuildLog(ctx, d.ID); err != nil || log != "" {
		t.Fatalf("a new deployment's log: %q, %v", log, err)
	}
	big := strings.Repeat("log line\n", 20000)
	if err := repo.SetBuildLog(ctx, d.ID, big); err != nil {
		t.Fatal(err)
	}
	if log, _ := repo.BuildLog(ctx, d.ID); log != big {
		t.Errorf("the log was not stored: %d bytes", len(log))
	}
	if strings.Contains(deploymentColumns, "build_log") {
		t.Error("deploymentColumns selects the log: every page of deployments would read up to 256 KiB per row")
	}
	if got, err := repo.GetDeployment(ctx, d.ID); err != nil || got.Image != "" {
		t.Errorf("GetDeployment = %+v, %v", got, err)
	}

	if err := repo.SetImage(ctx, d.ID, "localhost:5000/gh/gh@sha256:abc", "https://gh.eu.example"); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.GetDeployment(ctx, d.ID)
	if got.Image != "localhost:5000/gh/gh@sha256:abc" || got.URL != "https://gh.eu.example" || got.Status != project.StatusQueued {
		t.Errorf("after SetImage: %+v (the status must not change)", got)
	}
	for name, err := range map[string]error{
		"SetImage":    repo.SetImage(ctx, uuid.New(), "x", "y"),
		"SetBuildLog": repo.SetBuildLog(ctx, uuid.New(), "x"),
	} {
		if !errors.Is(err, project.ErrNotFound) {
			t.Errorf("%s on an unknown deployment: %v, want ErrNotFound", name, err)
		}
	}
	if _, err := repo.BuildLog(ctx, uuid.New()); !errors.Is(err, project.ErrNotFound) {
		t.Errorf("BuildLog on an unknown deployment: %v", err)
	}
}

// Retention reads every image the registry may still hold, and marks an image
// gone for the one project that ran it.
func TestImagesOfDeploymentsAreListedAndMarkedPruned(t *testing.T) {
	repo, owner := newTestRepo(t)
	ctx := context.Background()
	mk := func(slug string) *project.Project {
		p := newProject(owner, slug)
		if err := repo.CreateProject(ctx, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	run := func(p *project.Project, image string, status project.Status) *project.Deployment {
		d := &project.Deployment{ProjectID: p.ID, Image: image, Status: status, Region: "eu"}
		if err := repo.CreateDeployment(ctx, d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	a, b := mk("img-a"), mk("img-b")
	same := "localhost:5000/img-a/img-a@sha256:" + strings.Repeat("1", 64)
	old := run(a, same, project.StatusReady)
	run(a, "", project.StatusQueued) // no image: not listed
	again := run(a, same, project.StatusReady)
	newest := run(a, "localhost:5000/img-a/img-a@sha256:"+strings.Repeat("2", 64), project.StatusDeploying)
	other := run(b, same, project.StatusReady) // the same text in another project

	rows, err := repo.ListImages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mine []project.ImageUse
	for _, r := range rows {
		if r.ProjectID == a.ID {
			mine = append(mine, r)
		}
	}
	if len(mine) != 3 || mine[0].DeploymentID != newest.ID || mine[1].DeploymentID != again.ID || mine[2].DeploymentID != old.ID {
		t.Fatalf("project a's images, newest first: %+v", mine)
	}
	if mine[0].Slug != "img-a" || mine[0].Status != project.StatusDeploying || mine[0].Image != newest.Image {
		t.Errorf("row %+v", mine[0])
	}

	if err := repo.MarkImagePruned(ctx, a.ID, same); err != nil {
		t.Fatal(err)
	}
	for _, d := range []*project.Deployment{old, again} {
		if got, _ := repo.GetDeployment(ctx, d.ID); got.ImagePrunedAt == nil {
			t.Errorf("deployment %s still has its image", d.ID)
		}
	}
	if got, _ := repo.GetDeployment(ctx, other.ID); got.ImagePrunedAt != nil {
		t.Error("another project's deployment of the same image text was marked")
	}
	if got, _ := repo.GetDeployment(ctx, newest.ID); got.ImagePrunedAt != nil {
		t.Error("a different image was marked")
	}
	rows, _ = repo.ListImages(ctx)
	for _, r := range rows {
		if r.ProjectID == a.ID && r.Image == same {
			t.Errorf("a marked image is still listed: %+v", r)
		}
	}
	// Marking again is harmless.
	if err := repo.MarkImagePruned(ctx, a.ID, same); err != nil {
		t.Error(err)
	}
}
