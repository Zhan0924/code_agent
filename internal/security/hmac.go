// Package security implements communication security for the Code Agent,
// including HMAC signature verification for webhooks/MCP callbacks and
// egress traffic control for sandbox containers.
package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// ─── HMAC Signature Verification ─────────────────────────────────────────────
// All incoming webhooks, MCP callbacks, and external event triggers MUST be
// verified via HMAC-SHA256 to prevent man-in-the-middle tampering of execution
// results flowing into the Agent's state machine.

// HMACConfig configures the HMAC verification middleware.
type HMACConfig struct {
	// Secret is the shared HMAC secret key for signature verification.
	Secret string

	// HeaderName is the HTTP header containing the signature (e.g., "X-Hub-Signature-256").
	HeaderName string

	// SignaturePrefix is the prefix before the hex signature (e.g., "sha256=").
	SignaturePrefix string

	// MaxBodySize limits the request body size to prevent DoS via large payloads.
	MaxBodySize int64

	// TimestampHeader is the optional header for replay attack prevention.
	TimestampHeader string

	// MaxTimestampAge is the maximum age of a request timestamp before rejection.
	MaxTimestampAge time.Duration
}

// DefaultHMACConfig returns sensible defaults for webhook HMAC verification.
func DefaultHMACConfig() *HMACConfig {
	return &HMACConfig{
		HeaderName:      "X-Signature-256",
		SignaturePrefix: "sha256=",
		MaxBodySize:     1 << 20, // 1 MB
		TimestampHeader: "X-Timestamp",
		MaxTimestampAge: 5 * time.Minute,
	}
}

// HMACVerifier provides HMAC-SHA256 signature verification.
type HMACVerifier struct {
	cfg    *HMACConfig
	logger *zap.Logger
}

// NewHMACVerifier creates a new HMAC verifier with the given configuration.
func NewHMACVerifier(cfg *HMACConfig, logger *zap.Logger) *HMACVerifier {
	return &HMACVerifier{
		cfg:    cfg,
		logger: logger.With(zap.String("component", "hmac_verifier")),
	}
}

// VerifySignature checks if the given payload matches the provided HMAC-SHA256 signature.
// The timestamp is included in the HMAC computation to prevent replay attacks
// where an attacker modifies the timestamp header without invalidating the signature.
func (v *HMACVerifier) VerifySignature(payload []byte, signature, timestamp string) bool {
	// Strip prefix if present
	sig := strings.TrimPrefix(signature, v.cfg.SignaturePrefix)

	expectedMAC := v.computeHMAC(payload, timestamp)
	expectedSig := hex.EncodeToString(expectedMAC)

	// Use constant-time comparison to prevent timing attacks
	return hmac.Equal([]byte(expectedSig), []byte(sig))
}

// computeHMAC computes the HMAC-SHA256 of timestamp + payload.
func (v *HMACVerifier) computeHMAC(payload []byte, timestamp string) []byte {
	mac := hmac.New(sha256.New, []byte(v.cfg.Secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("\n"))
	mac.Write(payload)
	return mac.Sum(nil)
}

// SignPayload generates an HMAC-SHA256 signature for outgoing requests.
func (v *HMACVerifier) SignPayload(payload []byte, timestamp string) string {
	mac := v.computeHMAC(payload, timestamp)
	return v.cfg.SignaturePrefix + hex.EncodeToString(mac)
}

// ─── Gin Middleware ──────────────────────────────────────────────────────────

// GinMiddleware returns a Gin middleware that enforces HMAC signature verification
// on all incoming requests to the protected route group.
func (v *HMACVerifier) GinMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Step 1: Extract signature header
		signature := c.GetHeader(v.cfg.HeaderName)
		if signature == "" {
			v.logger.Warn("request missing HMAC signature header",
				zap.String("path", c.Request.URL.Path),
				zap.String("ip", c.ClientIP()),
			)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "missing signature header: " + v.cfg.HeaderName,
			})
			return
		}

		// Step 2: Replay attack prevention via timestamp.
		// When TimestampHeader is configured, the header is REQUIRED — a missing
		// timestamp is treated as a protocol violation. Previously the check was
		// silently skipped when the header was absent, which let any attacker
		// bypass replay protection simply by omitting it.
		if v.cfg.TimestampHeader != "" && v.cfg.MaxTimestampAge > 0 {
			tsHeader := c.GetHeader(v.cfg.TimestampHeader)
			if tsHeader == "" {
				v.logger.Warn("request missing required timestamp header",
					zap.String("path", c.Request.URL.Path),
					zap.String("header", v.cfg.TimestampHeader),
					zap.String("ip", c.ClientIP()),
				)
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
					"error": "missing timestamp header: " + v.cfg.TimestampHeader,
				})
				return
			}
			ts, err := time.Parse(time.RFC3339, tsHeader)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
					"error": "invalid timestamp format",
				})
				return
			}
			// Reject both too-old (replay) and too-far-future (clock skew / forgery).
			age := time.Since(ts)
			if age > v.cfg.MaxTimestampAge || age < -v.cfg.MaxTimestampAge {
				v.logger.Warn("request timestamp outside allowed window",
					zap.String("timestamp", tsHeader),
					zap.Duration("age", age),
					zap.Duration("max_age", v.cfg.MaxTimestampAge),
				)
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
					"error": "request timestamp expired or skewed",
				})
				return
			}
		}

		// Step 3: Read and verify request body
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, v.cfg.MaxBodySize))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "failed to read request body",
			})
			return
		}
		c.Request.Body = io.NopCloser(strings.NewReader(string(body)))

		// Step 4: Verify HMAC signature (includes timestamp in computation)
		tsHeader := c.GetHeader(v.cfg.TimestampHeader)
		if !v.VerifySignature(body, signature, tsHeader) {
			v.logger.Warn("HMAC signature verification failed",
				zap.String("path", c.Request.URL.Path),
				zap.String("ip", c.ClientIP()),
				zap.String("provided_sig", signature[:min(len(signature), 16)]+"..."),
			)
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "invalid HMAC signature",
			})
			return
		}

		v.logger.Debug("HMAC signature verified",
			zap.String("path", c.Request.URL.Path),
		)

		c.Next()
	}
}

// ─── HTTP Transport Wrapper ──────────────────────────────────────────────────

// SigningTransport is an http.RoundTripper that automatically signs outgoing
// requests with HMAC-SHA256, used for Agent-to-MCP-Server communication.
type SigningTransport struct {
	Base     http.RoundTripper
	Verifier *HMACVerifier
}

// RoundTrip implements http.RoundTripper, signing the request body before sending.
func (t *SigningTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil && t.Verifier != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read request body for signing: %w", err)
		}
		req.Body = io.NopCloser(strings.NewReader(string(body)))

		timestamp := time.Now().UTC().Format(time.RFC3339)
		signature := t.Verifier.SignPayload(body, timestamp)
		req.Header.Set(t.Verifier.cfg.HeaderName, signature)
		req.Header.Set(t.Verifier.cfg.TimestampHeader, timestamp)
	}

	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}
