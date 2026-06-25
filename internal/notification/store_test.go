package notification

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteStoreCreateReturnsExistingNotificationForIdempotencyKey(t *testing.T) {
	store := newTestStore(t)
	now := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)
	req := CreateRequest{
		TargetURL:      "https://vendor.example.test/callback",
		Method:         "POST",
		Headers:        map[string]string{"Content-Type": "application/json"},
		Body:           json.RawMessage(`{"event":"registered"}`),
		IdempotencyKey: "biz-001",
	}

	first, duplicate, err := store.Create(NewNotification(req, now))
	if err != nil {
		t.Fatalf("create first notification: %v", err)
	}
	if duplicate {
		t.Fatalf("first create returned duplicate=true")
	}

	second, duplicate, err := store.Create(NewNotification(req, now.Add(time.Second)))
	if err != nil {
		t.Fatalf("create duplicate notification: %v", err)
	}
	if !duplicate {
		t.Fatalf("second create returned duplicate=false")
	}
	if second.ID != first.ID {
		t.Fatalf("duplicate id = %s, want %s", second.ID, first.ID)
	}
}

func TestSQLiteStoreCreateRejectsConflictingIdempotencyKey(t *testing.T) {
	store := newTestStore(t)
	now := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)
	req := CreateRequest{
		TargetURL:      "https://vendor.example.test/callback",
		Method:         "POST",
		Headers:        map[string]string{"Content-Type": "application/json"},
		Body:           json.RawMessage(`{"event":"registered","userId":"u_1"}`),
		IdempotencyKey: "biz-001",
	}
	if _, _, err := store.Create(NewNotification(req, now)); err != nil {
		t.Fatalf("create first notification: %v", err)
	}

	req.Body = json.RawMessage(`{"event":"registered","userId":"u_2"}`)
	if _, _, err := store.Create(NewNotification(req, now.Add(time.Second))); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("create conflicting notification error = %v, want %v", err, ErrIdempotencyConflict)
	}
}

func TestSQLiteStorePersistsAttemptsAndListsByStatus(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notifications.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	})
	created, duplicate, err := store.Create(NewNotification(CreateRequest{
		TargetURL:   "https://vendor.example.test/callback",
		Method:      "POST",
		Headers:     map[string]string{"Content-Type": "application/json"},
		Body:        json.RawMessage(`{"event":"registered"}`),
		MaxAttempts: 1,
	}, time.Now()))
	if err != nil {
		t.Fatalf("create notification: %v", err)
	}
	if duplicate {
		t.Fatalf("new notification returned duplicate=true")
	}
	now := time.Now()
	if err := store.RecordAttempt(DeliveryAttempt{
		ID:             "atm_test",
		NotificationID: created.ID,
		AttemptNo:      1,
		Status:         AttemptFailed,
		StatusCode:     503,
		Error:          "vendor returned HTTP 503",
		StartedAt:      now,
		FinishedAt:     now.Add(10 * time.Millisecond),
	}); err != nil {
		t.Fatalf("record attempt: %v", err)
	}
	if err := store.MarkFailed(created.ID, "vendor returned HTTP 503", time.Now()); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close store before reopen: %v", err)
	}
	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Fatalf("close reopened store: %v", err)
		}
	})
	attempts, err := reopened.ListAttempts(created.ID)
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempt count = %d, want 1", len(attempts))
	}
	if attempts[0].Status != AttemptFailed {
		t.Fatalf("attempt status = %s, want %s", attempts[0].Status, AttemptFailed)
	}

	failed, err := reopened.ListByStatus(StatusFailed)
	if err != nil {
		t.Fatalf("list failed notifications: %v", err)
	}
	if len(failed) != 1 {
		t.Fatalf("failed notification count = %d, want 1", len(failed))
	}
	if failed[0].ID != created.ID {
		t.Fatalf("failed notification id = %s, want %s", failed[0].ID, created.ID)
	}
}

func TestSQLiteStoreClaimDueMarksSendingAndIncrementsAttemptCount(t *testing.T) {
	store := newTestStore(t)
	created := createTestNotification(t, store, DefaultMaxAttempts)

	claimed, err := store.ClaimDue(time.Now(), 10, time.Minute)
	if err != nil {
		t.Fatalf("claim due notifications: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed count = %d, want 1", len(claimed))
	}
	if claimed[0].ID != created.ID {
		t.Fatalf("claimed id = %s, want %s", claimed[0].ID, created.ID)
	}
	if claimed[0].Status != StatusSending {
		t.Fatalf("claimed status = %s, want %s", claimed[0].Status, StatusSending)
	}
	if claimed[0].AttemptCount != 1 {
		t.Fatalf("claimed attempt count = %d, want 1", claimed[0].AttemptCount)
	}

	claimedAgain, err := store.ClaimDue(time.Now(), 10, time.Minute)
	if err != nil {
		t.Fatalf("claim due notifications again: %v", err)
	}
	if len(claimedAgain) != 0 {
		t.Fatalf("claimed count after lease held = %d, want 0", len(claimedAgain))
	}

	claimedExpired, err := store.ClaimDue(time.Now().Add(2*time.Minute), 10, time.Minute)
	if err != nil {
		t.Fatalf("claim expired sending notification: %v", err)
	}
	if len(claimedExpired) != 1 {
		t.Fatalf("expired claimed count = %d, want 1", len(claimedExpired))
	}
	if claimedExpired[0].AttemptCount != 2 {
		t.Fatalf("expired claimed attempt count = %d, want 2", claimedExpired[0].AttemptCount)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("get claimed notification: %v", err)
	}
	if got.Status != StatusSending {
		t.Fatalf("stored status = %s, want %s", got.Status, StatusSending)
	}
}

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "notifications.db"))
	if err != nil {
		t.Fatalf("create test store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("close test store: %v", err)
		}
	})
	return store
}
