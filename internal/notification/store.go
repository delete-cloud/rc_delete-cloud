package notification

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var ErrNotFound = errors.New("notification not found")
var ErrIdempotencyConflict = errors.New("idempotency key conflicts with an existing notification")

type Store interface {
	Create(n Notification) (Notification, bool, error)
	Get(id string) (Notification, error)
	ListByStatus(status Status) ([]Notification, error)
	ClaimDue(now time.Time, limit int, sendingLease time.Duration) ([]Notification, error)
	MarkSuccess(id string, now time.Time) error
	MarkRetry(id string, lastError string, nextRetryAt time.Time, now time.Time) error
	MarkFailed(id string, lastError string, now time.Time) error
	RetryFailed(id string, now time.Time) (Notification, error)
	RecordAttempt(attempt DeliveryAttempt) error
	ListAttempts(notificationID string) ([]DeliveryAttempt, error)
}

func validateAttempt(attempt DeliveryAttempt) error {
	if attempt.ID == "" {
		return errors.New("attempt id is required")
	}
	if attempt.NotificationID == "" {
		return errors.New("attempt notification id is required")
	}
	if attempt.AttemptNo <= 0 {
		return errors.New("attempt number must be positive")
	}
	switch attempt.Status {
	case AttemptSucceeded, AttemptRetrying, AttemptFailed:
	default:
		return fmt.Errorf("unknown attempt status %q", attempt.Status)
	}
	if attempt.StartedAt.IsZero() {
		return errors.New("attempt startedAt is required")
	}
	if attempt.FinishedAt.IsZero() {
		return errors.New("attempt finishedAt is required")
	}
	if attempt.FinishedAt.Before(attempt.StartedAt) {
		return errors.New("attempt finishedAt cannot be before startedAt")
	}
	return nil
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

func sameIdempotentRequest(a Notification, b Notification) bool {
	return a.TargetURL == b.TargetURL &&
		a.Method == b.Method &&
		a.MaxAttempts == b.MaxAttempts &&
		bytes.Equal(a.Body, b.Body) &&
		equalHeaders(a.Headers, b.Headers)
}

func equalHeaders(a map[string]string, b map[string]string) bool {
	normalizedA := normalizeHeadersForCompare(a)
	normalizedB := normalizeHeadersForCompare(b)
	if len(normalizedA) != len(normalizedB) {
		return false
	}
	for k, v := range normalizedA {
		if normalizedB[k] != v {
			return false
		}
	}
	return true
}

func normalizeHeadersForCompare(headers map[string]string) map[string]string {
	normalized := make(map[string]string, len(headers))
	for k, v := range headers {
		key := http.CanonicalHeaderKey(strings.TrimSpace(k))
		normalized[key] = v
	}
	return normalized
}

func cloneAttempt(attempt DeliveryAttempt) DeliveryAttempt {
	if attempt.NextRetryAt != nil {
		next := *attempt.NextRetryAt
		attempt.NextRetryAt = &next
	}
	return attempt
}
