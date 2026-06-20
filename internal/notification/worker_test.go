package notification

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type fakeDelivery struct {
	result DeliveryResult
	seen   []Notification
}

func (f *fakeDelivery) Send(_ context.Context, n Notification) DeliveryResult {
	f.seen = append(f.seen, n)
	return f.result
}

func TestWorkerMarksNotificationSucceeded(t *testing.T) {
	store := newTestStore(t)
	delivery := &fakeDelivery{result: DeliveryResult{Success: true}}
	created := createTestNotification(t, store, DefaultMaxAttempts)

	worker := NewWorker(store, delivery, WorkerConfig{BatchSize: 10})
	worker.RunOnce(context.Background())

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("get notification: %v", err)
	}
	if got.Status != StatusSucceeded {
		t.Fatalf("status = %s, want %s", got.Status, StatusSucceeded)
	}
	if got.AttemptCount != 1 {
		t.Fatalf("attempt count = %d, want 1", got.AttemptCount)
	}
	if len(delivery.seen) != 1 {
		t.Fatalf("delivery calls = %d, want 1", len(delivery.seen))
	}
}

func TestWorkerRetriesTransientFailure(t *testing.T) {
	store := newTestStore(t)
	delivery := &fakeDelivery{result: DeliveryResult{Retryable: true, ErrorMessage: "vendor returned HTTP 503"}}
	created := createTestNotification(t, store, DefaultMaxAttempts)

	worker := NewWorker(store, delivery, WorkerConfig{BatchSize: 10})
	before := time.Now()
	worker.RunOnce(context.Background())
	after := time.Now().Add(NextBackoff(1) + time.Second)

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("get notification: %v", err)
	}
	if got.Status != StatusRetrying {
		t.Fatalf("status = %s, want %s", got.Status, StatusRetrying)
	}
	if got.AttemptCount != 1 {
		t.Fatalf("attempt count = %d, want 1", got.AttemptCount)
	}
	if got.NextRetryAt == nil {
		t.Fatalf("next retry time is nil")
	}
	minNextRetry := before.Add(NextBackoff(1))
	if got.NextRetryAt.Before(minNextRetry) || got.NextRetryAt.After(after) {
		t.Fatalf("next retry = %s, want between %s and %s", got.NextRetryAt, minNextRetry, after)
	}
	if !strings.Contains(got.LastError, "503") {
		t.Fatalf("last error = %q, want HTTP status detail", got.LastError)
	}
}

func TestWorkerFailsPermanentFailureWithoutRetry(t *testing.T) {
	store := newTestStore(t)
	delivery := &fakeDelivery{result: DeliveryResult{Retryable: false, ErrorMessage: "vendor returned HTTP 400"}}
	created := createTestNotification(t, store, DefaultMaxAttempts)

	worker := NewWorker(store, delivery, WorkerConfig{BatchSize: 10})
	worker.RunOnce(context.Background())

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("get notification: %v", err)
	}
	if got.Status != StatusFailed {
		t.Fatalf("status = %s, want %s", got.Status, StatusFailed)
	}
	if got.NextRetryAt != nil {
		t.Fatalf("next retry = %s, want nil", got.NextRetryAt)
	}
}

func TestWorkerFailsAfterMaxAttempts(t *testing.T) {
	store := newTestStore(t)
	delivery := &fakeDelivery{result: DeliveryResult{Retryable: true, ErrorMessage: "timeout"}}
	created := createTestNotification(t, store, 1)

	worker := NewWorker(store, delivery, WorkerConfig{BatchSize: 10})
	worker.RunOnce(context.Background())

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("get notification: %v", err)
	}
	if got.Status != StatusFailed {
		t.Fatalf("status = %s, want %s", got.Status, StatusFailed)
	}
	if got.AttemptCount != 1 {
		t.Fatalf("attempt count = %d, want 1", got.AttemptCount)
	}
}

func createTestNotification(t *testing.T, store Store, maxAttempts int) Notification {
	t.Helper()
	req := CreateRequest{
		TargetURL:   "https://vendor.example.test/callback",
		Method:      "POST",
		Headers:     map[string]string{"Content-Type": "application/json"},
		Body:        json.RawMessage(`{"event":"registered"}`),
		MaxAttempts: maxAttempts,
	}
	created, duplicate, err := store.Create(NewNotification(req, time.Now()))
	if err != nil {
		t.Fatalf("create notification: %v", err)
	}
	if duplicate {
		t.Fatalf("new notification returned duplicate=true")
	}
	return created
}
