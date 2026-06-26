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

var ErrBlockedTarget = errors.New("target resolves to blocked IP")

var blockedTargetPrefixes = mustParsePrefixes([]string{
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"192.168.0.0/16",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"224.0.0.0/4",
	"240.0.0.0/4",
	"255.255.255.255/32",
	"::/128",
	"::1/128",
	"::ffff:0:0/96",
	"64:ff9b::/96",
	"64:ff9b:1::/48",
	"100::/64",
	"2001::/23",
	"2002::/16",
	"2001:db8::/32",
	"fc00::/7",
	"fe80::/10",
	"ff00::/8",
})

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
			return fmt.Errorf("%w: target host %s resolves to blocked IP %s", ErrBlockedTarget, host, addr)
		}
	}
	return nil
}

func (p SecurityPolicy) DialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	dialer := &net.Dialer{}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("dial target address is invalid: %w", err)
	}
	if err := p.validateHostAllowlist(host); err != nil {
		return nil, err
	}
	if p.AllowPrivateIP {
		return dialer.DialContext(ctx, network, address)
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve dial target host %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve dial target host %s: no addresses", host)
	}
	for _, addr := range addrs {
		if isBlockedTargetIP(addr) {
			return nil, fmt.Errorf("%w: dial target host %s resolves to blocked IP %s", ErrBlockedTarget, host, addr)
		}
	}

	var lastErr error
	for _, addr := range addrs {
		if !networkAllowsAddr(network, addr) {
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no %s address found for %s", network, host)
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
	for _, prefix := range blockedTargetPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func networkAllowsAddr(network string, addr netip.Addr) bool {
	switch network {
	case "tcp4":
		return addr.Is4()
	case "tcp6":
		return addr.Is6()
	default:
		return true
	}
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

func mustParsePrefixes(values []string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefixes = append(prefixes, netip.MustParsePrefix(value))
	}
	return prefixes
}
