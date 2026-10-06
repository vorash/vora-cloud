-- The GitHub repository a project was created from, by GitHub's ids: the
-- installation that can read it and the repository itself. The ids are the
-- only link. A name can be renamed, transferred or taken by someone else, so
-- repo (a label the API lets anyone set) is never used to route a push.
-- Both are set or neither is, and only the dashboard sets them, after checking
-- the repository is one of the user's own installation's.
ALTER TABLE projects
    ADD COLUMN github_installation_id BIGINT,
    ADD COLUMN github_repo_id BIGINT,
    ADD CONSTRAINT projects_github_link CHECK ((github_installation_id IS NULL) = (github_repo_id IS NULL));

-- A push names the installation and the repository; this is how it finds projects.
CREATE INDEX idx_projects_github ON projects(github_installation_id, github_repo_id)
    WHERE github_repo_id IS NOT NULL;
