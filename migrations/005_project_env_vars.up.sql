-- Environment variables of a project. Values are encrypted by the service
-- (AES-256-GCM, a random nonce in front of the ciphertext) before they reach
-- this table, so a database dump or backup does not expose them.
CREATE TABLE project_env_vars (
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name TEXT NOT NULL CHECK (name ~ '^[A-Za-z_][A-Za-z0-9_]*$'),
    value_encrypted BYTEA NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_id, name)
);

CREATE TRIGGER update_project_env_vars_updated_at BEFORE UPDATE ON project_env_vars
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

ALTER TABLE project_env_vars ENABLE ROW LEVEL SECURITY;
