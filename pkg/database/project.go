package database

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vorash/vora-cloud/pkg/project"
)

// ProjectRepository is the PostgreSQL implementation of project.Store.
type ProjectRepository struct {
	db *DB
}

var _ project.Store = (*ProjectRepository)(nil)

func NewProjectRepository(db *DB) *ProjectRepository {
	return &ProjectRepository{db: db}
}

const projectColumns = `id, owner_id, name, slug, repo, default_branch, region, build_config,
	port, replicas, cpu_millis, memory_mib, health_path, created_at, updated_at,
	COALESCE(github_installation_id, 0), COALESCE(github_repo_id, 0)`

func scanProject(row pgx.Row) (*project.Project, error) {
	var p project.Project
	err := row.Scan(&p.ID, &p.OwnerID, &p.Name, &p.Slug, &p.Repo, &p.DefaultBranch, &p.Region, &p.BuildConfig,
		&p.Port, &p.Replicas, &p.CPUMillis, &p.MemoryMiB, &p.HealthPath, &p.CreatedAt, &p.UpdatedAt,
		&p.GitHubInstallationID, &p.GitHubRepoID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, project.ErrNotFound
	}
	return &p, err
}

const uniqueViolation = "23505"

func (r *ProjectRepository) CreateProject(ctx context.Context, p *project.Project) error {
	err := r.db.QueryRow(ctx, `
		INSERT INTO projects (owner_id, name, slug, repo, default_branch, region, build_config,
			port, replicas, cpu_millis, memory_mib, health_path, github_installation_id, github_repo_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NULLIF($13::bigint, 0), NULLIF($14::bigint, 0))
		RETURNING id, created_at, updated_at
	`, p.OwnerID, p.Name, p.Slug, p.Repo, p.DefaultBranch, p.Region, p.BuildConfig,
		p.Port, p.Replicas, p.CPUMillis, p.MemoryMiB, p.HealthPath, p.GitHubInstallationID, p.GitHubRepoID,
	).Scan(&p.ID, &p.CreatedAt, &p.UpdatedAt)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == "projects_slug_key" {
		return project.ErrSlugTaken
	}
	return err
}

func (r *ProjectRepository) GetProject(ctx context.Context, id uuid.UUID) (*project.Project, error) {
	return scanProject(r.db.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = $1`, id))
}

func (r *ProjectRepository) ListProjects(ctx context.Context, owner uuid.UUID) ([]project.Project, error) {
	rows, err := r.db.Query(ctx, `SELECT `+projectColumns+` FROM projects WHERE owner_id = $1 ORDER BY created_at DESC`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []project.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// ListProjectsByGitHub finds the projects tied to a repository by GitHub's ids.
// A project with no link has NULL ids and never matches.
func (r *ProjectRepository) ListProjectsByGitHub(ctx context.Context, installationID, repoID int64) ([]project.Project, error) {
	rows, err := r.db.Query(ctx, `SELECT `+projectColumns+` FROM projects
		WHERE github_installation_id = $1 AND github_repo_id = $2 ORDER BY created_at`, installationID, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []project.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (r *ProjectRepository) DeleteProject(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM projects WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return project.ErrNotFound
	}
	return nil
}

const deploymentColumns = `id, project_id, commit_sha, ref, status, image, region, url, error,
	created_at, started_at, finished_at, updated_at, image_pruned_at`

func scanDeployment(row pgx.Row) (*project.Deployment, error) {
	var d project.Deployment
	err := row.Scan(&d.ID, &d.ProjectID, &d.CommitSHA, &d.Ref, &d.Status, &d.Image, &d.Region, &d.URL, &d.Error,
		&d.CreatedAt, &d.StartedAt, &d.FinishedAt, &d.UpdatedAt, &d.ImagePrunedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, project.ErrNotFound
	}
	return &d, err
}

func (r *ProjectRepository) CreateDeployment(ctx context.Context, d *project.Deployment) error {
	return r.db.QueryRow(ctx, `
		INSERT INTO deployments (project_id, commit_sha, ref, status, image, region, url)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at, updated_at
	`, d.ProjectID, d.CommitSHA, d.Ref, d.Status, d.Image, d.Region, d.URL,
	).Scan(&d.ID, &d.CreatedAt, &d.UpdatedAt)
}

func (r *ProjectRepository) GetDeployment(ctx context.Context, id uuid.UUID) (*project.Deployment, error) {
	return scanDeployment(r.db.QueryRow(ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE id = $1`, id))
}

func (r *ProjectRepository) listDeployments(ctx context.Context, query string, args ...any) ([]project.Deployment, error) {
	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []project.Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// ListDeployments pages by keyset on (created_at DESC, id), which
// idx_deployments_project_id serves. The id breaks ties in ascending order, so
// the condition is spelled out: a row comparison cannot mix directions.
func (r *ProjectRepository) ListDeployments(ctx context.Context, projectID uuid.UUID, after *project.Cursor, limit int) ([]project.Deployment, error) {
	if after == nil {
		return r.listDeployments(ctx, `SELECT `+deploymentColumns+` FROM deployments
			WHERE project_id = $1 ORDER BY created_at DESC, id LIMIT $2`, projectID, limit)
	}
	return r.listDeployments(ctx, `SELECT `+deploymentColumns+` FROM deployments
		WHERE project_id = $1 AND (created_at < $2 OR (created_at = $2 AND id > $3))
		ORDER BY created_at DESC, id LIMIT $4`, projectID, after.CreatedAt, after.ID, limit)
}

// LatestDeployments picks each project's newest deployment with the same
// order as ListDeployments; idx_deployments_project_id serves the inner query.
func (r *ProjectRepository) LatestDeployments(ctx context.Context, owner uuid.UUID) (map[uuid.UUID]project.Deployment, error) {
	deps, err := r.listDeployments(ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE id IN (
		SELECT DISTINCT ON (project_id) id FROM deployments
		WHERE project_id IN (SELECT id FROM projects WHERE owner_id = $1)
		ORDER BY project_id, created_at DESC, id)`, owner)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]project.Deployment, len(deps))
	for _, d := range deps {
		out[d.ProjectID] = d
	}
	return out, nil
}

func (r *ProjectRepository) ListActive(ctx context.Context) ([]project.Deployment, error) {
	return r.listDeployments(ctx, `SELECT `+deploymentColumns+` FROM deployments
		WHERE status NOT IN ('ready', 'failed') ORDER BY created_at`)
}

func (r *ProjectRepository) Move(ctx context.Context, id uuid.UUID, from, to project.Status, errText string) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE deployments
		SET status = $3, error = $4,
			started_at = CASE WHEN $3 = 'deploying' THEN NOW() ELSE started_at END,
			finished_at = CASE WHEN $3 IN ('ready', 'failed') THEN NOW() ELSE finished_at END
		WHERE id = $1 AND status = $2
	`, id, from, to, errText)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return project.ErrConflict
	}
	return nil
}

// SetImage records the image of a built deployment and its address.
func (r *ProjectRepository) SetImage(ctx context.Context, id uuid.UUID, image, url string) error {
	tag, err := r.db.Exec(ctx, `UPDATE deployments SET image = $2, url = $3 WHERE id = $1`, id, image, url)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return project.ErrNotFound
	}
	return nil
}

// ListImages returns the deployments whose image the registry may still hold.
func (r *ProjectRepository) ListImages(ctx context.Context) ([]project.ImageUse, error) {
	rows, err := r.db.Query(ctx, `SELECT d.id, d.project_id, p.slug, d.image, d.status, d.created_at
		FROM deployments d JOIN projects p ON p.id = d.project_id
		WHERE d.image <> '' AND d.image_pruned_at IS NULL
		ORDER BY d.project_id, d.created_at DESC, d.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []project.ImageUse
	for rows.Next() {
		var u project.ImageUse
		if err := rows.Scan(&u.DeploymentID, &u.ProjectID, &u.Slug, &u.Image, &u.Status, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// MarkImagePruned records that an image is gone, on each deployment that ran it.
func (r *ProjectRepository) MarkImagePruned(ctx context.Context, projectID uuid.UUID, image string) error {
	_, err := r.db.Exec(ctx, `UPDATE deployments SET image_pruned_at = now()
		WHERE project_id = $1 AND image = $2 AND image_pruned_at IS NULL`, projectID, image)
	return err
}

// BuildLog is a column of its own, outside deploymentColumns, so that listing
// deployments never reads it.
func (r *ProjectRepository) BuildLog(ctx context.Context, id uuid.UUID) (string, error) {
	var log string
	err := r.db.QueryRow(ctx, `SELECT build_log FROM deployments WHERE id = $1`, id).Scan(&log)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", project.ErrNotFound
	}
	return log, err
}

func (r *ProjectRepository) SetBuildLog(ctx context.Context, id uuid.UUID, log string) error {
	tag, err := r.db.Exec(ctx, `UPDATE deployments SET build_log = $2 WHERE id = $1`, id, log)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return project.ErrNotFound
	}
	return nil
}

func (r *ProjectRepository) SetEnv(ctx context.Context, projectID uuid.UUID, name string, sealed []byte) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO project_env_vars (project_id, name, value_encrypted) VALUES ($1, $2, $3)
		ON CONFLICT (project_id, name) DO UPDATE SET value_encrypted = EXCLUDED.value_encrypted
	`, projectID, name, sealed)
	return err
}

func (r *ProjectRepository) ListEnv(ctx context.Context, projectID uuid.UUID) (map[string][]byte, error) {
	rows, err := r.db.Query(ctx, `SELECT name, value_encrypted FROM project_env_vars WHERE project_id = $1`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string][]byte{}
	for rows.Next() {
		var name string
		var sealed []byte
		if err := rows.Scan(&name, &sealed); err != nil {
			return nil, err
		}
		out[name] = sealed
	}
	return out, rows.Err()
}

func (r *ProjectRepository) DeleteEnv(ctx context.Context, projectID uuid.UUID, name string) error {
	_, err := r.db.Exec(ctx, `DELETE FROM project_env_vars WHERE project_id = $1 AND name = $2`, projectID, name)
	return err
}
