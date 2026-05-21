package security

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestDefaultEgressPolicy(t *testing.T) {
	p := DefaultEgressPolicy()
	if !p.Enabled {
		t.Error("default policy should be enabled")
	}
	if p.DefaultAction != "deny" {
		t.Error("default action should be deny")
	}
	if len(p.BlockedCIDRs) == 0 {
		t.Error("should have blocked CIDRs")
	}
}

func TestEgressValidator_DenyAll(t *testing.T) {
	policy := DefaultEgressPolicy()
	v, err := NewEgressValidator(policy, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}

	// Everything should be denied
	if v.IsAllowed("1.2.3.4", 80) {
		t.Error("should deny public IP")
	}
	if v.IsAllowed("google.com", 443) {
		t.Error("should deny hostname")
	}
	if v.IsAllowed("169.254.169.254", 80) {
		t.Error("should deny metadata endpoint")
	}
}

func TestEgressValidator_AllowedHost(t *testing.T) {
	policy := &EgressPolicy{
		Enabled:       true,
		DefaultAction: "deny",
		AllowedHosts:  []string{"api.example.com:443"},
	}
	v, err := NewEgressValidator(policy, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}

	if !v.IsAllowed("api.example.com", 443) {
		t.Error("should allow whitelisted host:port")
	}
	if v.IsAllowed("api.example.com", 80) {
		t.Error("should deny different port")
	}
	if v.IsAllowed("evil.com", 443) {
		t.Error("should deny non-whitelisted host")
	}
}

func TestEgressValidator_AllowedCIDR(t *testing.T) {
	policy := &EgressPolicy{
		Enabled:       true,
		DefaultAction: "deny",
		AllowedCIDRs:  []string{"10.0.0.0/8"},
		BlockedCIDRs:  []string{"169.254.169.254/32"},
	}
	v, err := NewEgressValidator(policy, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}

	if !v.IsAllowed("10.1.2.3", 5432) {
		t.Error("should allow IP in allowed CIDR")
	}
	if v.IsAllowed("192.168.1.1", 80) {
		t.Error("should deny IP not in allowed CIDR")
	}
}

func TestEgressValidator_BlockedTakesPrecedence(t *testing.T) {
	policy := &EgressPolicy{
		Enabled:       true,
		DefaultAction: "deny",
		AllowedCIDRs:  []string{"169.254.0.0/16"},
		BlockedCIDRs:  []string{"169.254.169.254/32"},
	}
	v, err := NewEgressValidator(policy, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}

	// Allowed range but specifically blocked
	if v.IsAllowed("169.254.169.254", 80) {
		t.Error("blocked CIDR should take precedence over allowed")
	}
	// Other IPs in the allowed range should work
	if !v.IsAllowed("169.254.1.1", 80) {
		t.Error("other IPs in allowed range should be permitted")
	}
}

func TestEgressValidator_DockerNetworkMode(t *testing.T) {
	// Deny-all policy → "none"
	v1, _ := NewEgressValidator(DefaultEgressPolicy(), zap.NewNop())
	if mode := v1.DockerNetworkMode(); mode != "none" {
		t.Errorf("expected 'none', got %q", mode)
	}

	// Policy with allowed hosts → custom network
	v2, _ := NewEgressValidator(&EgressPolicy{
		Enabled:       true,
		DefaultAction: "deny",
		AllowedCIDRs:  []string{"10.0.0.0/8"},
	}, zap.NewNop())
	if mode := v2.DockerNetworkMode(); mode == "none" {
		t.Error("should not be 'none' when allowed CIDRs exist")
	}

	// Disabled policy → "bridge"
	v3, _ := NewEgressValidator(&EgressPolicy{Enabled: false}, zap.NewNop())
	if mode := v3.DockerNetworkMode(); mode != "bridge" {
		t.Errorf("expected 'bridge', got %q", mode)
	}
}

func TestEgressValidator_GenerateIptablesRules(t *testing.T) {
	policy := &EgressPolicy{
		Enabled:       true,
		DefaultAction: "deny",
		AllowedCIDRs:  []string{"10.0.0.0/8"},
		BlockedCIDRs:  []string{"169.254.169.254/32"},
		DNSAllowed:    true,
	}
	v, err := NewEgressValidator(policy, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}

	rules := v.GenerateIptablesRules()
	if len(rules) == 0 {
		t.Fatal("expected iptables rules")
	}

	// Should contain DENY policy, loopback, blocked, allowed, DNS rules
	hasDefault := false
	hasDNS := false
	for _, r := range rules {
		if r == "-P OUTPUT DENY" {
			hasDefault = true
		}
		if r == "-A OUTPUT -p udp --dport 53 -j ACCEPT" {
			hasDNS = true
		}
	}
	if !hasDefault {
		t.Error("missing default DENY policy rule")
	}
	if !hasDNS {
		t.Error("missing DNS allow rule")
	}
}

func TestNewEgressValidator_InvalidCIDR(t *testing.T) {
	policy := &EgressPolicy{
		Enabled:      true,
		AllowedCIDRs: []string{"not-a-cidr"},
	}
	_, err := NewEgressValidator(policy, zap.NewNop())
	if err == nil {
		t.Error("expected error for invalid CIDR")
	}
}

func TestPortFromURL(t *testing.T) {
	cases := map[string]struct {
		raw  string
		want int
		err  bool
	}{
		"explicit":       {"https://example.com:8443/x", 8443, false},
		"https_default":  {"https://example.com/x", 443, false},
		"http_default":   {"http://example.com/x", 80, false},
		"ws_default":     {"ws://example.com/ws", 80, false},
		"wss_default":    {"wss://example.com/ws", 443, false},
		"unknown_scheme": {"ftp://example.com/x", 0, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			u, err := url.Parse(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			got, err := portFromURL(u)
			if tc.err {
				if err == nil {
					t.Errorf("expected error for %q", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestEgressTransport_DeniesDisallowedHost verifies the URL-level block fires
// before any dial or base transport invocation.
func TestEgressTransport_DeniesDisallowedHost(t *testing.T) {
	policy := &EgressPolicy{
		Enabled:       true,
		DefaultAction: "deny",
		AllowedHosts:  []string{"allowed.example"},
	}
	v, err := NewEgressValidator(policy, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}

	delegated := false
	tr := &EgressTransport{
		Validator: v,
		Base: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			delegated = true
			return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
		}),
	}

	req, _ := http.NewRequest("GET", "https://blocked.example/path", nil)
	_, err = tr.RoundTrip(req)
	if !errors.Is(err, ErrEgressDenied) {
		t.Fatalf("expected ErrEgressDenied, got %v", err)
	}
	if delegated {
		t.Error("base transport must not be invoked on denied requests")
	}
}

// TestEgressTransport_AllowsWhitelistedHost verifies the happy path.
func TestEgressTransport_AllowsWhitelistedHost(t *testing.T) {
	policy := &EgressPolicy{
		Enabled:       true,
		DefaultAction: "deny",
		AllowedHosts:  []string{"allowed.example"},
	}
	v, _ := NewEgressValidator(policy, zap.NewNop())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	// Override the URL.Host check by manually inserting the test server's host
	// into the whitelist.
	v.allowedHosts[strings.ToLower(strings.TrimPrefix(srv.URL, "http://"))] = true

	tr := &EgressTransport{Validator: v, Base: http.DefaultTransport}
	req, _ := http.NewRequest("GET", srv.URL, nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("got status %d", resp.StatusCode)
	}
}

// TestEgressTransport_DisabledPolicyPassesThrough verifies that when the
// policy is disabled, the wrapper does not interfere — this is important so
// configuration controls whether enforcement happens.
func TestEgressTransport_DisabledPolicyPassesThrough(t *testing.T) {
	v, _ := NewEgressValidator(&EgressPolicy{Enabled: false}, zap.NewNop())
	tr := &EgressTransport{
		Validator: v,
		Base: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 204, Body: http.NoBody, Header: http.Header{}}, nil
		}),
	}
	req, _ := http.NewRequest("GET", "https://any.example/", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 204 {
		t.Errorf("got %d", resp.StatusCode)
	}
}

// TestNewEgressHTTPClient_BlocksLoopbackUnderDenyPolicy verifies that the
// post-DNS dial-level check rejects the resolved IP even when the URL-level
// hostname allowlist would pass. This is the SSRF-defense path: a hostname
// that resolves to a blocked IP must still be stopped.
func TestNewEgressHTTPClient_BlocksLoopbackUnderDenyPolicy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	// URL-layer: allow hostname "localhost" to pass the request through.
	// Dial-layer: 127.0.0.0/8 is not in AllowedCIDRs and DefaultAction=deny,
	// so the post-resolve dial check rejects the IP the kernel is about to
	// connect to — this is the SSRF defense we want to exercise.
	v, err := NewEgressValidator(&EgressPolicy{
		Enabled:       true,
		DefaultAction: "deny",
		AllowedHosts:  []string{"localhost"},
	}, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}

	client := NewEgressHTTPClient(v, 0)

	// Rewrite the URL so the hostname is "localhost" (allowlisted) — Go will
	// resolve this to 127.0.0.1 at dial time, which the Control func rejects.
	target := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	req, _ := http.NewRequest("GET", target, nil)
	_, err = client.Do(req)
	if err == nil {
		t.Fatal("expected dial-level egress denial, got nil error")
	}
	if !strings.Contains(err.Error(), "egress denied") {
		t.Errorf("expected egress-denied error, got: %v", err)
	}
}

// roundTripperFunc adapts a function to http.RoundTripper for tests.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
