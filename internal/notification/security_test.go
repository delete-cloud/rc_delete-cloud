package notification

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecurityPolicyValidatesAllowedHostsAndHeaders(t *testing.T) {
	policy := NewSecurityPolicy([]string{"vendor.example.test"}, DefaultAllowedHeaders())
	req := CreateRequest{
		TargetURL: "https://vendor.example.test/callback",
		Method:    "POST",
		Headers: map[string]string{
			"Content-Type": "application/json",
			"X-Request-ID": "req-1",
		},
		Body: json.RawMessage(`{"event":"registered"}`),
	}
	if err := policy.ValidateEnvelope(req); err != nil {
		t.Fatalf("validate allowed envelope: %v", err)
	}

	req.TargetURL = "https://evil.example.test/callback"
	if err := policy.ValidateEnvelope(req); err == nil || !strings.Contains(err.Error(), "not in allowlist") {
		t.Fatalf("validate disallowed host error = %v, want allowlist error", err)
	}

	req.TargetURL = "https://vendor.example.test/callback"
	req.Headers = map[string]string{"Host": "metadata.google.internal"}
	if err := policy.ValidateEnvelope(req); err == nil || !strings.Contains(err.Error(), "header Host is not allowed") {
		t.Fatalf("validate disallowed header error = %v, want header allowlist error", err)
	}
}

func TestSecurityPolicyRejectsPrivateAndMetadataTargets(t *testing.T) {
	policy := NewSecurityPolicy([]string{"127.0.0.1", "169.254.169.254"}, DefaultAllowedHeaders())
	tests := []string{
		"http://127.0.0.1/callback",
		"http://169.254.169.254/latest/meta-data",
	}
	for _, target := range tests {
		err := policy.ValidateResolvedTarget(context.Background(), target)
		if err == nil {
			t.Fatalf("ValidateResolvedTarget(%q) succeeded, want private target error", target)
		}
	}
}

func TestHTTPDeliveryDoesNotFollowRedirects(t *testing.T) {
	targetReached := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetReached = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	policy := NewSecurityPolicy(nil, DefaultAllowedHeaders())
	policy.AllowPrivateIP = true
	delivery := NewHTTPDeliveryWithSecurity(&http.Client{}, policy)
	result := delivery.Send(context.Background(), Notification{
		ID:        "ntf_redirect",
		TargetURL: redirector.URL,
		Method:    http.MethodPost,
		Headers:   map[string]string{"Content-Type": "application/json"},
		Body:      json.RawMessage(`{"event":"redirect"}`),
	})
	if result.Success {
		t.Fatalf("redirect delivery succeeded, want non-success result")
	}
	if result.StatusCode != http.StatusFound {
		t.Fatalf("redirect status code = %d, want %d", result.StatusCode, http.StatusFound)
	}
	if targetReached {
		t.Fatalf("redirect target was reached, want redirects disabled")
	}
}
