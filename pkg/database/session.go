package database

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Session is a signed-in browser session together with its user.
type Session struct {
	UserID    uuid.UUID
	UserName  string
	UserEmail string
	ExpiresAt time.Time
}

// SessionRepository stores dashboard sessions by the hash of their token.
type SessionRepository struct {
	db *DB
}

func NewSessionRepository(db *DB) *SessionRepository {
	return &SessionRepository{db: db}
}

func (r *SessionRepository) Create(ctx context.Context, userID uuid.UUID, tokenHash string, expiresAt time.Time) error {
	_, err := r.db.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`, tokenHash, userID, expiresAt)
	return err
}

// Get returns the session for a token hash, or nil when there is none, it has
// expired, or its user is inactive: deactivating a user ends their sessions.
func (r *SessionRepository) Get(ctx context.Context, tokenHash string) (*Session, error) {
	var s Session
	err := r.db.QueryRow(ctx, `
		SELECT u.id, u.name, u.email, s.expires_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > NOW() AND u.is_active
	`, tokenHash).Scan(&s.UserID, &s.UserName, &s.UserEmail, &s.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *SessionRepository) Delete(ctx context.Context, tokenHash string) error {
	_, err := r.db.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash)
	return err
}

func (r *SessionRepository) DeleteExpired(ctx context.Context) error {
	_, err := r.db.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= NOW()`)
	return err
}
