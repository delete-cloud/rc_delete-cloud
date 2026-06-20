package notification

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var ErrNotFound = errors.New("notification not found")

type Store interface {
	Create(n Notification) (Notification, bool, error)
	Get(id string) (Notification, error)
	FetchDue(now time.Time, limit int, sendingLease time.Duration) ([]Notification, error)
	MarkSending(id string, now time.Time) (Notification, error)
	MarkSuccess(id string, now time.Time) error
	MarkRetry(id string, lastError string, nextRetryAt time.Time, now time.Time) error
	MarkFailed(id string, lastError string, now time.Time) error
	RetryFailed(id string, now time.Time) (Notification, error)
}

type FileStore struct {
	mu       sync.Mutex
	path     string
	items    map[string]Notification
	idemKeys map[string]string
}

func NewFileStore(path string) (*FileStore, error) {
	store := &FileStore{
		path:     path,
		items:    map[string]Notification{},
		idemKeys: map[string]string{},
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *FileStore) Create(n Notification) (Notification, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if n.IdempotencyKey != "" {
		if id, ok := s.idemKeys[n.IdempotencyKey]; ok {
			return s.items[id], true, nil
		}
	}
	s.items[n.ID] = cloneNotification(n)
	if n.IdempotencyKey != "" {
		s.idemKeys[n.IdempotencyKey] = n.ID
	}
	if err := s.saveLocked(); err != nil {
		return Notification{}, false, err
	}
	return cloneNotification(n), false, nil
}

func (s *FileStore) Get(id string) (Notification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, ok := s.items[id]
	if !ok {
		return Notification{}, ErrNotFound
	}
	return cloneNotification(n), nil
}

func (s *FileStore) FetchDue(now time.Time, limit int, sendingLease time.Duration) ([]Notification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if limit <= 0 {
		return nil, errors.New("limit must be positive")
	}
	due := make([]Notification, 0, limit)
	keys := make([]string, 0, len(s.items))
	for id := range s.items {
		keys = append(keys, id)
	}
	sort.Strings(keys)

	for _, id := range keys {
		n := s.items[id]
		if !isDue(n, now, sendingLease) {
			continue
		}
		due = append(due, cloneNotification(n))
		if len(due) == limit {
			break
		}
	}
	return due, nil
}

func (s *FileStore) MarkSending(id string, now time.Time) (Notification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, ok := s.items[id]
	if !ok {
		return Notification{}, ErrNotFound
	}
	if n.Status != StatusPending && n.Status != StatusRetrying && n.Status != StatusSending {
		return Notification{}, fmt.Errorf("notification %s is %s and cannot be sent", id, n.Status)
	}
	n.Status = StatusSending
	n.AttemptCount++
	n.LastError = ""
	n.UpdatedAt = now
	s.items[id] = n
	if err := s.saveLocked(); err != nil {
		return Notification{}, err
	}
	return cloneNotification(n), nil
}

func (s *FileStore) MarkSuccess(id string, now time.Time) error {
	return s.update(id, func(n Notification) (Notification, error) {
		n.Status = StatusSucceeded
		n.NextRetryAt = nil
		n.LastError = ""
		n.UpdatedAt = now
		return n, nil
	})
}

func (s *FileStore) MarkRetry(id string, lastError string, nextRetryAt time.Time, now time.Time) error {
	return s.update(id, func(n Notification) (Notification, error) {
		n.Status = StatusRetrying
		n.NextRetryAt = &nextRetryAt
		n.LastError = lastError
		n.UpdatedAt = now
		return n, nil
	})
}

func (s *FileStore) MarkFailed(id string, lastError string, now time.Time) error {
	return s.update(id, func(n Notification) (Notification, error) {
		n.Status = StatusFailed
		n.NextRetryAt = nil
		n.LastError = lastError
		n.UpdatedAt = now
		return n, nil
	})
}

func (s *FileStore) RetryFailed(id string, now time.Time) (Notification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, ok := s.items[id]
	if !ok {
		return Notification{}, ErrNotFound
	}
	if n.Status != StatusFailed {
		return Notification{}, fmt.Errorf("notification %s is %s and cannot be retried manually", id, n.Status)
	}
	n.Status = StatusRetrying
	n.AttemptCount = 0
	n.NextRetryAt = nil
	n.LastError = ""
	n.UpdatedAt = now
	s.items[id] = n
	if err := s.saveLocked(); err != nil {
		return Notification{}, err
	}
	return cloneNotification(n), nil
}

func (s *FileStore) update(id string, fn func(Notification) (Notification, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, ok := s.items[id]
	if !ok {
		return ErrNotFound
	}
	updated, err := fn(n)
	if err != nil {
		return err
	}
	s.items[id] = updated
	return s.saveLocked()
}

func (s *FileStore) load() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read store: %w", err)
	}
	var items []Notification
	if err := json.Unmarshal(data, &items); err != nil {
		return fmt.Errorf("decode store: %w", err)
	}
	for _, n := range items {
		if n.ID == "" {
			return errors.New("store contains notification without id")
		}
		s.items[n.ID] = cloneNotification(n)
		if n.IdempotencyKey != "" {
			s.idemKeys[n.IdempotencyKey] = n.ID
		}
	}
	return nil
}

func (s *FileStore) saveLocked() error {
	items := make([]Notification, 0, len(s.items))
	for _, n := range s.items {
		items = append(items, cloneNotification(n))
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})

	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return fmt.Errorf("encode store: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create store dir: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write temp store: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace store: %w", err)
	}
	return nil
}

func isDue(n Notification, now time.Time, sendingLease time.Duration) bool {
	switch n.Status {
	case StatusPending:
		return true
	case StatusRetrying:
		return n.NextRetryAt == nil || !n.NextRetryAt.After(now)
	case StatusSending:
		return now.Sub(n.UpdatedAt) >= sendingLease
	default:
		return false
	}
}

func cloneNotification(n Notification) Notification {
	n.Body = append(json.RawMessage(nil), n.Body...)
	if n.Headers != nil {
		headers := make(map[string]string, len(n.Headers))
		for k, v := range n.Headers {
			headers[k] = v
		}
		n.Headers = headers
	}
	if n.NextRetryAt != nil {
		next := *n.NextRetryAt
		n.NextRetryAt = &next
	}
	return n
}
