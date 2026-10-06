DROP INDEX IF EXISTS idx_projects_github;
ALTER TABLE projects
    DROP CONSTRAINT IF EXISTS projects_github_link,
    DROP COLUMN IF EXISTS github_repo_id,
    DROP COLUMN IF EXISTS github_installation_id;
