// egress.go — 出向流量白名单 / SSRF 防御。
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【威胁场景】
//
//	1. LLM 生成的代码在沙箱里访问云元数据端点（169.254.169.254 → IAM 凭据）；
//	2. LLM tool 拉取 attacker 的 URL 导致 SSRF；
//	3. LLM 工具被诱导成内网扫描器。
//
// 【两层防御（EgressTransport + NewEgressHTTPClient）】
//
//	L1 — URL 层（RoundTrip 前）：检查 req.URL.Host 是否在白名单。最快，不消耗
//	     网络资源。缺点：对手可以用 allow-listed 的**主机名**，让 DNS 解析到
//	     禁止的 IP（DNS Rebinding）。
//
//	L2 — 连接层（Dialer.Control）：DNS 解析后 kernel connect(2) 之前，用
//	     解析出的实际 IP 再查一遍白名单/黑名单。这里是**真正的 SSRF 防御**——
//	     无论 DNS 返回什么，L2 都能挡住通往内网 / 元数据端点的连接。
//
//	NewEgressHTTPClient 同时装配 L1 和 L2。EgressTransport 单独使用也有意义：
//	上层只想做 URL 层粗筛而不想接管整个 dial stack 时。
//
// 【EgressPolicy 的三种用法】
//
//	· 最严：DefaultEgressPolicy()  — default_action=deny，无白名单，黑名单
//	       包含所有云元数据 + 私网 CIDR。沙箱容器必须用这个。
//	· 中性：InternalServiceEgressPolicy() — 允许 10.0.0.0/8 内部服务，仍封
//	       元数据端点。内部 Agent pod 互通用这个。
//	· 自定义：业务构造 EgressPolicy{}，手填 AllowedHosts + AllowedCIDRs +
//	       BlockedCIDRs。
//
// 【fail-open vs fail-close】
//
//	policy.Enabled=false → 完全放行（fail-open 配置）。这是为了兼容开发环境。
//	policy.Enabled=true && IsAllowed 出错 → 拒绝（fail-close）。Validate
//	失败时返回的 false 比误放行危险得多。
//
// 【iptables rule 生成】
//
//	GenerateIptablesRules 把策略翻成 iptables 规则字符串。当前不直接注入到
//	容器，是给运维/Cilium NetworkPolicy 参考的中间产物。未来可以绑定到
//	sandbox 的 network namespace 上做真正的 kernel-level 拦截。
//
// ============================================================================
package security

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// ─── Egress Traffic Control ──────────────────────────────────────────────────
// Docker sandbox containers must have strictly controlled outbound network access.
// In production, use eBPF/Cilium for kernel-level enforcement. This Go-level
// implementation provides application-level validation and Docker network config
// generation for defense-in-depth.

// EgressPolicy defines the network egress rules for sandbox containers.
type EgressPolicy struct {
	// Enabled controls whether egress filtering is active.
	Enabled bool `json:"enabled" mapstructure:"enabled"`

	// DefaultAction is "deny" or "allow" — production MUST use "deny".
	DefaultAction string `json:"default_action" mapstructure:"default_action"`

	// AllowedHosts is the explicit whitelist of allowed outbound destinations.
	// Format: "host:port" or CIDR notation "10.0.0.0/8:5432"
	AllowedHosts []string `json:"allowed_hosts" mapstructure:"allowed_hosts"`

	// AllowedCIDRs are CIDR ranges that containers can connect to.
	AllowedCIDRs []string `json:"allowed_cidrs" mapstructure:"allowed_cidrs"`

	// BlockedCIDRs are explicitly blocked ranges (takes precedence over allowed).
	// Use to block metadata endpoints, internal services, etc.
	BlockedCIDRs []string `json:"blocked_cidrs" mapstructure:"blocked_cidrs"`

	// DNSAllowed controls whether containers can resolve DNS names.
	DNSAllowed bool `json:"dns_allowed" mapstructure:"dns_allowed"`
}

// DefaultEgressPolicy returns a maximally restrictive policy suitable for
// untrusted code execution: no network access at all.
func DefaultEgressPolicy() *EgressPolicy {
	return &EgressPolicy{
		Enabled:       true,
		DefaultAction: "deny",
		AllowedHosts:  nil,
		AllowedCIDRs:  nil,
		BlockedCIDRs: []string{
			"169.254.169.254/32", // AWS/GCP metadata endpoint
			"100.100.100.200/32", // Alibaba Cloud metadata
			"10.0.0.0/8",         // Private network class A
			"172.16.0.0/12",      // Private network class B
			"192.168.0.0/16",     // Private network class C
		},
		DNSAllowed: false,
	}
}

// InternalServiceEgressPolicy returns a policy for trusted Agent containers
// that need to reach internal infrastructure (DB proxy, MCP gateway, etc.)
func InternalServiceEgressPolicy() *EgressPolicy {
	return &EgressPolicy{
		Enabled:       true,
		DefaultAction: "deny",
		AllowedCIDRs: []string{
			"10.0.0.0/8", // Internal services
		},
		BlockedCIDRs: []string{
			"169.254.169.254/32", // Still block cloud metadata
		},
		DNSAllowed: true,
	}
}

// EgressValidator validates outbound connection targets against the policy.
type EgressValidator struct {
	policy       *EgressPolicy
	allowedNets  []*net.IPNet
	blockedNets  []*net.IPNet
	allowedHosts map[string]bool
	logger       *zap.Logger
}

// NewEgressValidator creates a validator from the given policy.
func NewEgressValidator(policy *EgressPolicy, logger *zap.Logger) (*EgressValidator, error) {
	v := &EgressValidator{
		policy:       policy,
		allowedHosts: make(map[string]bool),
		logger:       logger.With(zap.String("component", "egress_validator")),
	}

	// Parse allowed CIDRs
	for _, cidr := range policy.AllowedCIDRs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid allowed CIDR %q: %w", cidr, err)
		}
		v.allowedNets = append(v.allowedNets, ipNet)
	}

	// Parse blocked CIDRs
	for _, cidr := range policy.BlockedCIDRs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid blocked CIDR %q: %w", cidr, err)
		}
		v.blockedNets = append(v.blockedNets, ipNet)
	}

	// Parse allowed hosts
	for _, host := range policy.AllowedHosts {
		v.allowedHosts[strings.ToLower(host)] = true
	}

	return v, nil
}

// IsAllowed checks if a connection to the given host:port is permitted.
func (v *EgressValidator) IsAllowed(host string, port int) bool {
	if !v.policy.Enabled {
		return true
	}

	target := fmt.Sprintf("%s:%d", strings.ToLower(host), port)

	// Check explicit host whitelist
	if v.allowedHosts[target] || v.allowedHosts[strings.ToLower(host)] {
		return true
	}

	// Parse IP
	ip := net.ParseIP(host)
	if ip == nil {
		// It's a hostname - in deny-default mode, reject unless explicitly allowed
		if v.policy.DefaultAction == "deny" {
			v.logger.Warn("egress denied: hostname not in whitelist",
				zap.String("host", host),
				zap.Int("port", port),
			)
			return false
		}
		return true
	}

	// Check blocked CIDRs first (takes precedence)
	for _, blocked := range v.blockedNets {
		if blocked.Contains(ip) {
			v.logger.Warn("egress denied: IP in blocked CIDR",
				zap.String("ip", ip.String()),
				zap.String("cidr", blocked.String()),
			)
			return false
		}
	}

	// Check allowed CIDRs
	for _, allowed := range v.allowedNets {
		if allowed.Contains(ip) {
			return true
		}
	}

	// Default action
	if v.policy.DefaultAction == "deny" {
		v.logger.Warn("egress denied by default policy",
			zap.String("host", host),
			zap.Int("port", port),
		)
		return false
	}

	return true
}

// DockerNetworkMode returns the Docker network mode string based on the policy.
// For maximum isolation, returns "none" if no network access is needed.
func (v *EgressValidator) DockerNetworkMode() string {
	if !v.policy.Enabled {
		return "bridge"
	}

	if v.policy.DefaultAction == "deny" && len(v.policy.AllowedHosts) == 0 &&
		len(v.policy.AllowedCIDRs) == 0 {
		return "none" // Complete network isolation
	}

	// Use a custom network with iptables/eBPF rules
	return "code-agent-sandbox"
}

// GenerateIptablesRules generates iptables rules for the Docker container's
// network namespace. In production, these would be applied via Cilium NetworkPolicy
// or Docker network plugin.
func (v *EgressValidator) GenerateIptablesRules() []string {
	var rules []string

	// Default policy
	rules = append(rules, fmt.Sprintf("-P OUTPUT %s", strings.ToUpper(v.policy.DefaultAction)))

	// Allow loopback
	rules = append(rules, "-A OUTPUT -o lo -j ACCEPT")

	// Block specific CIDRs
	for _, blocked := range v.blockedNets {
		rules = append(rules, fmt.Sprintf("-A OUTPUT -d %s -j DROP", blocked.String()))
	}

	// Allow specific CIDRs
	for _, allowed := range v.allowedNets {
		rules = append(rules, fmt.Sprintf("-A OUTPUT -d %s -j ACCEPT", allowed.String()))
	}

	// DNS
	if v.policy.DNSAllowed {
		rules = append(rules, "-A OUTPUT -p udp --dport 53 -j ACCEPT")
		rules = append(rules, "-A OUTPUT -p tcp --dport 53 -j ACCEPT")
	}

	// Allow established connections (for response traffic)
	rules = append(rules, "-A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT")

	return rules
}

// ─── HTTP Egress Enforcement ─────────────────────────────────────────────────
// The types below turn EgressValidator from documentation into an enforcement
// point by plugging it into Go's net/http stack at two layers:
//
//   1. Pre-request URL check (EgressTransport) — rejects based on URL.Host
//      before any DNS lookup or TCP connect. Fast, but an attacker who controls
//      DNS could still resolve an allow-listed hostname to an internal IP.
//
//   2. Post-DNS IP check (NewEgressHTTPClient's Dialer.Control) — runs after
//      the resolver has produced an IP but before the connect(2) syscall.
//      This is the SSRF defense that catches DNS rebinding / internal IPs.
//
// Both layers call v.IsAllowed; host whitelists apply at layer 1, CIDR rules
// apply at both (layer 1 accepts hostnames as-is in whitelist; layer 2 sees
// only the resolved IP).

// ErrEgressDenied is returned when a connection is blocked by the egress policy.
var ErrEgressDenied = errors.New("egress denied by policy")

// EgressTransport is an http.RoundTripper that enforces the egress policy on
// the URL.Host of outgoing requests before delegating to Base. It provides URL
// -level denial only; wrap with NewEgressHTTPClient to also enforce at the
// resolved-IP layer (recommended for SSRF defense).
type EgressTransport struct {
	Validator *EgressValidator
	Base      http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *EgressTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.Validator != nil && t.Validator.policy != nil && t.Validator.policy.Enabled {
		host := req.URL.Hostname()
		port, err := portFromURL(req.URL)
		if err != nil {
			return nil, fmt.Errorf("egress: %w", err)
		}
		if !t.Validator.IsAllowed(host, port) {
			return nil, fmt.Errorf("%w: %s:%d", ErrEgressDenied, host, port)
		}
	}
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// NewEgressHTTPClient returns an *http.Client whose dial path enforces the
// egress policy against both the URL-level host (pre-resolve) and the resolved
// IP address (post-resolve, pre-connect). If timeout is zero a 30s default is
// applied. The returned client is safe for concurrent use.
func NewEgressHTTPClient(v *EgressValidator, timeout time.Duration) *http.Client {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   newDialerControl(v),
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &EgressTransport{Validator: v, Base: transport},
	}
}

// newDialerControl returns a net.Dialer.Control function that rejects connects
// to addresses the policy disallows. Invoked post-DNS-resolution with the
// literal IP:port the kernel is about to connect to, so DNS rebinding attacks
// that resolve allow-listed hostnames to internal IPs are caught here.
func newDialerControl(v *EgressValidator) func(network, address string, c syscall.RawConn) error {
	if v == nil {
		return nil
	}
	return func(network, address string, _ syscall.RawConn) error {
		host, portStr, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("egress: malformed address %q: %w", address, err)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return fmt.Errorf("egress: invalid port %q: %w", portStr, err)
		}
		if !v.IsAllowed(host, port) {
			return fmt.Errorf("%w: %s:%d", ErrEgressDenied, host, port)
		}
		return nil
	}
}

// portFromURL extracts the numeric port from a *url.URL, falling back to
// scheme defaults (http=80, https=443) when the port is omitted.
func portFromURL(u *url.URL) (int, error) {
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, fmt.Errorf("invalid port %q in URL: %w", p, err)
		}
		return n, nil
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "ws":
		return 80, nil
	case "https", "wss":
		return 443, nil
	default:
		return 0, fmt.Errorf("unknown scheme %q; cannot infer default port", u.Scheme)
	}
}

