package notification

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestFileStoreCreateReturnsExistingNotificationForIdempotencyKey(t *testing.T) {
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

func newTestStore(t *testing.T) *FileStore {
	t.Helper()
	store, err := NewFileStore(filepath.Join(t.TempDir(), "notifications.json"))
	if err != nil {
		t.Fatalf("create test store: %v", err)
	}
	return store
}
