package notification

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

const (
	defaultMaxHeaders         = 20
	defaultMaxHeaderValueSize = 4096
)

type SecurityPolicy struct {
	AllowedHosts       map[string]struct{}
	AllowedHeaders     map[string]struct{}
	AllowPrivateIP     bool
	MaxHeaders         int
	MaxHeaderValueSize int
}

func DefaultAllowedHeaders() []string {
	return []string{
		"Authorization",
		"Content-Type",
		"Idempotency-Key",
		"X-Request-ID",
	}
}

func NewSecurityPolicy(allowedHosts []string, allowedHeaders []string) SecurityPolicy {
	policy := SecurityPolicy{
		AllowedHosts:       normalizeSet(allowedHosts),
		AllowedHeaders:     normalizeHeaderSet(allowedHeaders),
		MaxHeaders:         defaultMaxHeaders,
		MaxHeaderValueSize: defaultMaxHeaderValueSize,
	}
	return policy
}

func DefaultSecurityPolicy() SecurityPolicy {
	return NewSecurityPolicy(nil, DefaultAllowedHeaders())
}

func (p SecurityPolicy) ValidateEnvelope(req CreateRequest) error {
	parsed, err := url.Parse(req.TargetURL)
	if err != nil {
		return fmt.Errorf("targetUrl is invalid: %w", err)
	}
	if parsed.Hostname() == "" {
		return errors.New("targetUrl host is required")
	}
	if err := p.validateHostAllowlist(parsed.Hostname()); err != nil {
		return err
	}
	if len(req.Headers) > p.maxHeaders() {
		return fmt.Errorf("too many headers: got %d, max %d", len(req.Headers), p.maxHeaders())
	}
	for name, value := range req.Headers {
		if err := p.validateHeader(name, value); err != nil {
			return err
		}
	}
	return nil
}

func (p SecurityPolicy) ValidateResolvedTarget(ctx context.Context, rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("targetUrl is invalid: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("targetUrl must use http or https")
	}
	host := parsed.Hostname()
	if host == "" {
		return errors.New("targetUrl host is required")
	}
	if err := p.validateHostAllowlist(host); err != nil {
		return err
	}
	if p.AllowPrivateIP {
		return nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolve target host %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("resolve target host %s: no addresses", host)
	}
	for _, addr := range addrs {
		if isBlockedTargetIP(addr) {
			return fmt.Errorf("target host %s resolves to blocked IP %s", host, addr)
		}
	}
	return nil
}

func (p SecurityPolicy) validateHostAllowlist(host string) error {
	if len(p.AllowedHosts) == 0 {
		return nil
	}
	normalized := normalizeHost(host)
	if _, ok := p.AllowedHosts[normalized]; !ok {
		return fmt.Errorf("target host %s is not in allowlist", host)
	}
	return nil
}

func (p SecurityPolicy) validateHeader(name string, value string) error {
	canonical := http.CanonicalHeaderKey(strings.TrimSpace(name))
	if canonical == "" {
		return errors.New("header name is required")
	}
	if _, ok := p.allowedHeaders()[canonical]; !ok {
		return fmt.Errorf("header %s is not allowed", canonical)
	}
	if len(value) > p.maxHeaderValueSize() {
		return fmt.Errorf("header %s value is too large", canonical)
	}
	return nil
}

func (p SecurityPolicy) allowedHeaders() map[string]struct{} {
	if len(p.AllowedHeaders) != 0 {
		return p.AllowedHeaders
	}
	return normalizeHeaderSet(DefaultAllowedHeaders())
}

func (p SecurityPolicy) maxHeaders() int {
	if p.MaxHeaders > 0 {
		return p.MaxHeaders
	}
	return defaultMaxHeaders
}

func (p SecurityPolicy) maxHeaderValueSize() int {
	if p.MaxHeaderValueSize > 0 {
		return p.MaxHeaderValueSize
	}
	return defaultMaxHeaderValueSize
}

func isBlockedTargetIP(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsUnspecified() || addr.IsMulticast() {
		return true
	}
	if addr.Is4() && addr == netip.MustParseAddr("169.254.169.254") {
		return true
	}
	return false
}

func normalizeSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		normalized := normalizeHost(value)
		if normalized == "" {
			continue
		}
		result[normalized] = struct{}{}
	}
	return result
}

func normalizeHeaderSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		normalized := http.CanonicalHeaderKey(strings.TrimSpace(value))
		if normalized == "" {
			continue
		}
		result[normalized] = struct{}{}
	}
	return result
}

func normalizeHost(host string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimSuffix(host, ".")))
}
