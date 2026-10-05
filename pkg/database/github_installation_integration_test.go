//go:build integration

package database

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vorash/vora-cloud/pkg/githubapp"
)

// Runs the installation store against a migrated PostgreSQL (make test-database).
func newTestInstallations(t *testing.T) (*GitHubInstallationRepository, *DB) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, err := New(url, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return NewGitHubInstallationRepository(db), db
}

func newTestUser(t *testing.T, db *DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := db.Exec(context.Background(),
		`INSERT INTO users (id, name, email, password_hash) VALUES ($1, 'test', $2, 'x')`, id, id.String()+"@example.com"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id) })
	return id
}

func TestGitHubInstallationLinkListGet(t *testing.T) {
	repo, db := newTestInstallations(t)
	ctx := context.Background()
	alice, bob := newTestUser(t, db), newTestUser(t, db)
	id := rand.Int64N(1 << 40)

	if err := repo.Link(ctx, githubapp.Installation{ID: id, UserID: alice, AccountID: 7, AccountLogin: "octo"}); err != nil {
		t.Fatal(err)
	}
	// The owner linking again changes nothing, except a renamed login.
	if err := repo.Link(ctx, githubapp.Installation{ID: id, UserID: alice, AccountID: 7, AccountLogin: "octo-renamed"}); err != nil {
		t.Fatalf("relinking by the owner: %v", err)
	}
	got, err := repo.Get(ctx, alice, id)
	if err != nil || got.AccountLogin != "octo-renamed" || got.AccountID != 7 {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if list, err := repo.List(ctx, alice); err != nil || len(list) != 1 || list[0].ID != id {
		t.Errorf("List(alice) = %+v, %v", list, err)
	}
	if list, _ := repo.List(ctx, bob); len(list) != 0 {
		t.Errorf("bob sees %+v", list)
	}
}

// An installation belongs to the user who linked it first. The second user is
// refused, and the row is not changed, not even its login.
func TestGitHubInstallationCannotBeTakenOver(t *testing.T) {
	repo, db := newTestInstallations(t)
	ctx := context.Background()
	alice, mallory := newTestUser(t, db), newTestUser(t, db)
	id := rand.Int64N(1 << 40)

	if err := repo.Link(ctx, githubapp.Installation{ID: id, UserID: alice, AccountID: 7, AccountLogin: "octo"}); err != nil {
		t.Fatal(err)
	}
	err := repo.Link(ctx, githubapp.Installation{ID: id, UserID: mallory, AccountID: 7, AccountLogin: "hijacked"})
	if !errors.Is(err, githubapp.ErrInstallationTaken) {
		t.Fatalf("err = %v, want ErrInstallationTaken", err)
	}
	got, err := repo.Get(ctx, alice, id)
	if err != nil || got.UserID != alice || got.AccountLogin != "octo" {
		t.Errorf("the installation changed: %+v, %v", got, err)
	}
	if _, err := repo.Get(ctx, mallory, id); !errors.Is(err, githubapp.ErrNotFound) {
		t.Errorf("mallory Get err = %v, want ErrNotFound", err)
	}
}

// Deleting a user removes their connections, so nothing outlives the account.
func TestGitHubInstallationsAreDeletedWithTheUser(t *testing.T) {
	repo, db := newTestInstallations(t)
	ctx := context.Background()
	user := uuid.New()
	if _, err := db.Exec(ctx, `INSERT INTO users (id, name, email, password_hash) VALUES ($1, 'test', $2, 'x')`, user, user.String()+"@example.com"); err != nil {
		t.Fatal(err)
	}
	id := rand.Int64N(1 << 40)
	if err := repo.Link(ctx, githubapp.Installation{ID: id, UserID: user, AccountID: 7, AccountLogin: "octo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE id = $1`, user); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM github_installations WHERE installation_id = $1`, id).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d rows left, %v", n, err)
	}
}
