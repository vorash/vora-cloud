//go:build integration

package auth

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vorash/vora-cloud/pkg/database"
)

// Runs against a migrated PostgreSQL (make test-database).
func TestAuthenticateUser(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, err := database.New(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	ctx := context.Background()

	repo := database.NewUserRepository(db)
	svc := NewUserService(repo, database.NewRefreshTokenRepository(db), NewJWTService("a-secret-of-at-least-32-characters!!", 0, 0))
	email := uuid.NewString() + "@example.com"
	user, err := svc.RegisterUser(ctx, "Test", email, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(ctx, `DELETE FROM users WHERE id = $1`, user.ID); err != nil {
			t.Error(err)
		}
	})

	if got, err := svc.AuthenticateUser(ctx, email, "correct horse"); err != nil || got.ID != user.ID {
		t.Errorf("right password: %v, %v", got, err)
	}
	for name, c := range map[string][2]string{
		"wrong password": {email, "wrong"},
		"unknown email":  {"nobody-" + email, "correct horse"},
	} {
		if _, err := svc.AuthenticateUser(ctx, c[0], c[1]); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("%s: %v, want ErrInvalidCredentials", name, err)
		}
	}

	// An inactive account gets the same answer, so sign-in does not reveal it.
	if _, err := db.Exec(ctx, `UPDATE users SET is_active = false WHERE id = $1`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AuthenticateUser(ctx, email, "correct horse"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("inactive account: %v, want ErrInvalidCredentials", err)
	}
}
