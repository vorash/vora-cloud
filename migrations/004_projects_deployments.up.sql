-- Projects and their deployments (M3).

-- The slug names the project's namespace ("p-" + slug) and its public host, so
-- it is unique across all owners: two users sharing a slug would share a
-- namespace. It is capped at 61 characters so "p-<slug>" stays a DNS label.
CREATE TABLE projects (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    slug VARCHAR(61) NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$'),
    repo TEXT NOT NULL DEFAULT '',
    default_branch TEXT NOT NULL DEFAULT 'main',
    region VARCHAR(16) NOT NULL,
    build_config JSONB NOT NULL DEFAULT '{}',
    -- How the app runs once it has an image.
    port INTEGER NOT NULL DEFAULT 80 CHECK (port BETWEEN 1 AND 65535),
    replicas INTEGER NOT NULL DEFAULT 1 CHECK (replicas >= 0),
    cpu_millis INTEGER NOT NULL DEFAULT 250 CHECK (cpu_millis > 0),
    memory_mib INTEGER NOT NULL DEFAULT 256 CHECK (memory_mib > 0),
    health_path TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_projects_owner_id ON projects(owner_id);

CREATE TABLE deployments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    commit_sha TEXT NOT NULL DEFAULT '',
    ref TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'building', 'pushing', 'deploying', 'ready', 'failed')),
    image TEXT NOT NULL,
    region VARCHAR(16) NOT NULL,
    url TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    started_at TIMESTAMP WITH TIME ZONE,
    finished_at TIMESTAMP WITH TIME ZONE,
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_deployments_project_id ON deployments(project_id, created_at DESC);
-- The reconciler looks only at deployments that are still moving.
CREATE INDEX idx_deployments_active ON deployments(status) WHERE status NOT IN ('ready', 'failed');

CREATE TRIGGER update_projects_updated_at BEFORE UPDATE ON projects
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

CREATE TRIGGER update_deployments_updated_at BEFORE UPDATE ON deployments
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

-- Supabase serves the public schema to anon and authenticated; the service
-- connects as the table owner, which bypasses row level security.
ALTER TABLE projects ENABLE ROW LEVEL SECURITY;
ALTER TABLE deployments ENABLE ROW LEVEL SECURITY;
