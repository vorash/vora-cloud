-- The GitHub App installations a user has connected. An installation belongs
-- to one user: it is the key, so a second user cannot claim it, and a user
-- may hold several. account_id is the GitHub account (not its login, which can
-- be renamed) the App is installed on.
CREATE TABLE github_installations (
    installation_id BIGINT PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    account_id BIGINT NOT NULL,
    account_login TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_github_installations_user_id ON github_installations(user_id);

ALTER TABLE github_installations ENABLE ROW LEVEL SECURITY;
