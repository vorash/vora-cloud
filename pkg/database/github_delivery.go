package database

import (
	"context"
	"time"
)

// GitHubDeliveryRepository remembers which webhook deliveries were received.
type GitHubDeliveryRepository struct {
	db *DB
}

func NewGitHubDeliveryRepository(db *DB) *GitHubDeliveryRepository {
	return &GitHubDeliveryRepository{db: db}
}

// Record stores a delivery and reports whether it was new. It is one atomic
// insert, so two concurrent copies of a delivery cannot both be told "new".
func (r *GitHubDeliveryRepository) Record(ctx context.Context, id, event string) (bool, error) {
	tag, err := r.db.Exec(ctx, `
		INSERT INTO github_deliveries (delivery_id, event) VALUES ($1, $2)
		ON CONFLICT (delivery_id) DO NOTHING
	`, id, event)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// DeleteOlderThan forgets deliveries received before the cutoff.
func (r *GitHubDeliveryRepository) DeleteOlderThan(ctx context.Context, cutoff time.Time) error {
	_, err := r.db.Exec(ctx, `DELETE FROM github_deliveries WHERE received_at < $1`, cutoff)
	return err
}
