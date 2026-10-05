package database

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vorash/vora-cloud/pkg/githubapp"
)

// GitHubInstallationRepository stores which user each GitHub App installation
// belongs to. It implements githubapp.Links.
type GitHubInstallationRepository struct {
	db *DB
}

func NewGitHubInstallationRepository(db *DB) *GitHubInstallationRepository {
	return &GitHubInstallationRepository{db: db}
}

// Link connects an installation to a user. The installation id is the key and
// the row is only updated for the same user, so a second user cannot take an
// installation over; that is githubapp.ErrInstallationTaken.
func (r *GitHubInstallationRepository) Link(ctx context.Context, i githubapp.Installation) error {
	tag, err := r.db.Exec(ctx, `
		INSERT INTO github_installations (installation_id, user_id, account_id, account_login)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (installation_id) DO UPDATE SET account_login = EXCLUDED.account_login
		WHERE github_installations.user_id = EXCLUDED.user_id
	`, i.ID, i.UserID, i.AccountID, i.AccountLogin)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return githubapp.ErrInstallationTaken
	}
	return nil
}

func (r *GitHubInstallationRepository) List(ctx context.Context, userID uuid.UUID) ([]githubapp.Installation, error) {
	rows, err := r.db.Query(ctx, `
		SELECT installation_id, user_id, account_id, account_login
		FROM github_installations WHERE user_id = $1 ORDER BY created_at, installation_id
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []githubapp.Installation
	for rows.Next() {
		var i githubapp.Installation
		if err := rows.Scan(&i.ID, &i.UserID, &i.AccountID, &i.AccountLogin); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// Get returns githubapp.ErrNotFound for an installation that is another
// user's, the same as one that is not there.
func (r *GitHubInstallationRepository) Get(ctx context.Context, userID uuid.UUID, installationID int64) (*githubapp.Installation, error) {
	var i githubapp.Installation
	err := r.db.QueryRow(ctx, `
		SELECT installation_id, user_id, account_id, account_login
		FROM github_installations WHERE installation_id = $1 AND user_id = $2
	`, installationID, userID).Scan(&i.ID, &i.UserID, &i.AccountID, &i.AccountLogin)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, githubapp.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &i, nil
}

var _ githubapp.Links = (*GitHubInstallationRepository)(nil)
