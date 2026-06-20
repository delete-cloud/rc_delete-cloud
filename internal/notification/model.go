package notification

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Status string

const (
	StatusPending   Status = "PENDING"
	StatusSending   Status = "SENDING"
	StatusRetrying  Status = "RETRYING"
	StatusSucceeded Status = "SUCCEEDED"
	StatusFailed    Status = "FAILED"
)

const DefaultMaxAttempts = 5

type Notification struct {
	ID             string            `json:"id"`
	TargetURL      string            `json:"targetUrl"`
	Method         string            `json:"method"`
	Headers        map[string]string `json:"headers,omitempty"`
	Body           json.RawMessage   `json:"body"`
	IdempotencyKey string            `json:"idempotencyKey,omitempty"`
	Status         Status            `json:"status"`
	AttemptCount   int               `json:"attemptCount"`
	MaxAttempts    int               `json:"maxAttempts"`
	NextRetryAt    *time.Time        `json:"nextRetryAt,omitempty"`
	LastError      string            `json:"lastError,omitempty"`
	CreatedAt      time.Time         `json:"createdAt"`
	UpdatedAt      time.Time         `json:"updatedAt"`
}

type CreateRequest struct {
	TargetURL      string            `json:"targetUrl"`
	Method         string            `json:"method"`
	Headers        map[string]string `json:"headers"`
	Body           json.RawMessage   `json:"body"`
	IdempotencyKey string            `json:"idempotencyKey"`
	MaxAttempts    int               `json:"maxAttempts"`
}

func (r CreateRequest) Validate() error {
	if strings.TrimSpace(r.TargetURL) == "" {
		return errors.New("targetUrl is required")
	}
	parsed, err := url.ParseRequestURI(r.TargetURL)
	if err != nil {
		return fmt.Errorf("targetUrl is invalid: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("targetUrl must use http or https")
	}

	method := strings.ToUpper(strings.TrimSpace(r.Method))
	if method == "" {
		return errors.New("method is required")
	}
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return fmt.Errorf("method %q is not supported", method)
	}

	if len(r.Body) == 0 {
		return errors.New("body is required")
	}
	if !json.Valid(r.Body) {
		return errors.New("body must be valid JSON")
	}
	if r.MaxAttempts < 0 {
		return errors.New("maxAttempts cannot be negative")
	}
	return nil
}

func NewNotification(req CreateRequest, now time.Time) Notification {
	maxAttempts := req.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = DefaultMaxAttempts
	}
	headers := make(map[string]string, len(req.Headers))
	for k, v := range req.Headers {
		headers[k] = v
	}
	return Notification{
		ID:             newID(),
		TargetURL:      strings.TrimSpace(req.TargetURL),
		Method:         strings.ToUpper(strings.TrimSpace(req.Method)),
		Headers:        headers,
		Body:           append(json.RawMessage(nil), req.Body...),
		IdempotencyKey: strings.TrimSpace(req.IdempotencyKey),
		Status:         StatusPending,
		MaxAttempts:    maxAttempts,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}
