package notification

import (
	"context"
	"encoding/json"
	"errors"
	"net"
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
	policy := NewSecurityPolicy([]string{
		"127.0.0.1",
		"169.254.169.254",
		"100.64.0.1",
		"64:ff9b::a9fe:a9fe",
		"2002:a9fe:a9fe::1",
	}, DefaultAllowedHeaders())
	tests := []string{
		"http://127.0.0.1/callback",
		"http://169.254.169.254/latest/meta-data",
		"http://100.64.0.1/internal",
		"http://[64:ff9b::a9fe:a9fe]/nat64-metadata",
		"http://[2002:a9fe:a9fe::1]/six-to-four-metadata",
	}
	for _, target := range tests {
		err := policy.ValidateResolvedTarget(context.Background(), target)
		if !errors.Is(err, ErrBlockedTarget) {
			t.Fatalf("ValidateResolvedTarget(%q) error = %v, want %v", target, err, ErrBlockedTarget)
		}
	}
}

func TestSecurityPolicyDialContextRejectsBlockedIP(t *testing.T) {
	policy := NewSecurityPolicy(nil, DefaultAllowedHeaders())
	for _, address := range []string{
		"169.254.169.254:80",
		"100.64.0.1:80",
		"[64:ff9b::a9fe:a9fe]:80",
		"[::ffff:169.254.169.254]:80",
		"[2002:a9fe:a9fe::1]:80",
	} {
		_, err := policy.DialContext(context.Background(), "tcp", address)
		if !errors.Is(err, ErrBlockedTarget) {
			t.Fatalf("DialContext(%q) error = %v, want %v", address, err, ErrBlockedTarget)
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

func TestHTTPDeliveryOverridesUnsafeClientHooks(t *testing.T) {
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return nil
		},
		Transport: &http.Transport{
			DialTLS: func(_, _ string) (net.Conn, error) {
				t.Fatalf("custom DialTLS should be cleared")
				return nil, nil
			},
			DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
				t.Fatalf("custom DialTLSContext should be cleared")
				return nil, nil
			},
		},
	}
	NewHTTPDeliveryWithSecurity(client, DefaultSecurityPolicy())

	if err := client.CheckRedirect(&http.Request{}, nil); err != http.ErrUseLastResponse {
		t.Fatalf("CheckRedirect error = %v, want %v", err, http.ErrUseLastResponse)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.DialTLS != nil {
		t.Fatalf("DialTLS was not cleared")
	}
	if transport.DialTLSContext != nil {
		t.Fatalf("DialTLSContext was not cleared")
	}
}
