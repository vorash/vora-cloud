//go:build integration

package database

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Runs the delivery store against a migrated PostgreSQL (make test-database).
func newTestDeliveries(t *testing.T) (*GitHubDeliveryRepository, *DB) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, err := New(url, 16, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return NewGitHubDeliveryRepository(db), db
}

func TestGitHubDeliveryRecordsOnce(t *testing.T) {
	repo, db := newTestDeliveries(t)
	ctx := context.Background()
	id := uuid.NewString()
	t.Cleanup(func() { _, _ = db.Exec(ctx, `DELETE FROM github_deliveries WHERE delivery_id = $1`, id) })

	if fresh, err := repo.Record(ctx, id, "push"); err != nil || !fresh {
		t.Fatalf("first Record = %v, %v; want true", fresh, err)
	}
	if fresh, err := repo.Record(ctx, id, "push"); err != nil || fresh {
		t.Fatalf("second Record = %v, %v; want false", fresh, err)
	}
}

// GitHub can deliver the same event twice at once; exactly one may win, or a
// push would be queued twice.
func TestGitHubDeliveryConcurrentCopiesRecordOnce(t *testing.T) {
	repo, db := newTestDeliveries(t)
	ctx := context.Background()
	id := uuid.NewString()
	t.Cleanup(func() { _, _ = db.Exec(ctx, `DELETE FROM github_deliveries WHERE delivery_id = $1`, id) })

	var fresh atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := repo.Record(ctx, id, "push")
			if err != nil {
				t.Error(err)
			}
			if ok {
				fresh.Add(1)
			}
		}()
	}
	wg.Wait()
	if fresh.Load() != 1 {
		t.Errorf("%d copies were told they were new, want 1", fresh.Load())
	}
}

func TestGitHubDeliveryDeleteOlderThan(t *testing.T) {
	repo, db := newTestDeliveries(t)
	ctx := context.Background()
	old, recent := uuid.NewString(), uuid.NewString()
	t.Cleanup(func() {
		_, _ = db.Exec(ctx, `DELETE FROM github_deliveries WHERE delivery_id IN ($1, $2)`, old, recent)
	})
	for _, id := range []string{old, recent} {
		if _, err := repo.Record(ctx, id, "push"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(ctx, `UPDATE github_deliveries SET received_at = NOW() - INTERVAL '10 days' WHERE delivery_id = $1`, old); err != nil {
		t.Fatal(err)
	}

	if err := repo.DeleteOlderThan(ctx, time.Now().Add(-7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if fresh, _ := repo.Record(ctx, old, "push"); !fresh {
		t.Error("an old delivery was kept")
	}
	if fresh, _ := repo.Record(ctx, recent, "push"); fresh {
		t.Error("a recent delivery was deleted")
	}
}
