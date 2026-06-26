package notification

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

type DeliveryResult struct {
	Success      bool
	Retryable    bool
	StatusCode   int
	ErrorMessage string
	RetryAfter   *time.Time
}

type Delivery interface {
	Send(ctx context.Context, n Notification) DeliveryResult
}

type HTTPDelivery struct {
	client *http.Client
	policy SecurityPolicy
}

func NewHTTPDelivery(client *http.Client) HTTPDelivery {
	return NewHTTPDeliveryWithSecurity(client, DefaultSecurityPolicy())
}

func NewHTTPDeliveryWithSecurity(client *http.Client, policy SecurityPolicy) HTTPDelivery {
	if client == nil {
		panic("http client is required")
	}
	client.Transport = secureTransport(client.Transport, policy)
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return HTTPDelivery{client: client, policy: policy}
}

func secureTransport(roundTripper http.RoundTripper, policy SecurityPolicy) http.RoundTripper {
	if roundTripper == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		transport.DialContext = policy.DialContext
		return transport
	}
	transport, ok := roundTripper.(*http.Transport)
	if !ok {
		panic("http client transport must be *http.Transport or nil")
	}
	clone := transport.Clone()
	clone.Proxy = nil
	clone.DialContext = policy.DialContext
	clone.DialTLS = nil
	clone.DialTLSContext = nil
	return clone
}

func (d HTTPDelivery) Send(ctx context.Context, n Notification) DeliveryResult {
	if err := d.policy.ValidateResolvedTarget(ctx, n.TargetURL); err != nil {
		return DeliveryResult{Retryable: false, ErrorMessage: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, n.Method, n.TargetURL, bytes.NewReader(n.Body))
	if err != nil {
		return DeliveryResult{Retryable: false, ErrorMessage: err.Error()}
	}
	for k, v := range n.Headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Idempotency-Key") == "" && n.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", n.IdempotencyKey)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		if errors.Is(err, ErrBlockedTarget) {
			return DeliveryResult{Retryable: false, ErrorMessage: err.Error()}
		}
		return DeliveryResult{Retryable: true, ErrorMessage: err.Error()}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return DeliveryResult{Success: true, StatusCode: resp.StatusCode}
	}
	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	if isRetryableStatus(resp.StatusCode) {
		return DeliveryResult{
			Retryable:    true,
			StatusCode:   resp.StatusCode,
			ErrorMessage: fmt.Sprintf("vendor returned HTTP %d", resp.StatusCode),
			RetryAfter:   retryAfter,
		}
	}
	return DeliveryResult{
		Retryable:    false,
		StatusCode:   resp.StatusCode,
		ErrorMessage: fmt.Sprintf("vendor returned HTTP %d", resp.StatusCode),
	}
}

func isRetryableStatus(status int) bool {
	if status == http.StatusRequestTimeout || status == http.StatusConflict || status == http.StatusTooManyRequests {
		return true
	}
	return status >= 500 && status <= 599
}

func parseRetryAfter(value string, now time.Time) *time.Time {
	if value == "" {
		return nil
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		t := now.Add(time.Duration(seconds) * time.Second)
		return &t
	}
	t, err := http.ParseTime(value)
	if err != nil {
		return nil
	}
	return &t
}
