package security

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func TestHMACVerifier_SignAndVerify(t *testing.T) {
	cfg := DefaultHMACConfig()
	cfg.Secret = "test-secret-key"
	v := NewHMACVerifier(cfg, zap.NewNop())

	payload := []byte(`{"event":"push","ref":"refs/heads/main"}`)

	// Sign
	sig := v.SignPayload(payload)
	if sig == "" {
		t.Fatal("expected non-empty signature")
	}

	// Verify with correct signature
	ok := v.VerifySignature(payload, sig)
	if !ok {
		t.Error("expected valid signature to pass verification")
	}
}

func TestHMACVerifier_InvalidSignature(t *testing.T) {
	cfg := DefaultHMACConfig()
	cfg.Secret = "test-secret-key"
	v := NewHMACVerifier(cfg, zap.NewNop())

	payload := []byte(`{"event":"push"}`)

	ok := v.VerifySignature(payload, "sha256=deadbeef")
	if ok {
		t.Error("expected invalid signature to fail verification")
	}
}

func TestHMACVerifier_EmptySignature(t *testing.T) {
	cfg := DefaultHMACConfig()
	cfg.Secret = "test-secret-key"
	v := NewHMACVerifier(cfg, zap.NewNop())

	payload := []byte(`test`)

	ok := v.VerifySignature(payload, "")
	if ok {
		t.Error("expected empty signature to fail verification")
	}
}

func TestHMACVerifier_DifferentPayloads(t *testing.T) {
	cfg := DefaultHMACConfig()
	cfg.Secret = "my-secret"
	v := NewHMACVerifier(cfg, zap.NewNop())

	payload1 := []byte("payload1")
	payload2 := []byte("payload2")

	sig1 := v.SignPayload(payload1)
	sig2 := v.SignPayload(payload2)

	if sig1 == sig2 {
		t.Error("different payloads should produce different signatures")
	}

	// Cross-verify should fail
	if v.VerifySignature(payload1, sig2) {
		t.Error("cross-verification should fail")
	}
}

func TestHMACVerifier_SignatureWithPrefix(t *testing.T) {
	cfg := DefaultHMACConfig()
	cfg.Secret = "secret"
	v := NewHMACVerifier(cfg, zap.NewNop())

	payload := []byte("data")
	sig := v.SignPayload(payload)

	// SignPayload should include "sha256=" prefix
	if len(sig) < 7 {
		t.Fatal("signature too short")
	}
	// VerifySignature should handle the prefix
	ok := v.VerifySignature(payload, sig)
	if !ok {
		t.Error("prefixed signature should be verifiable")
	}
}

// TestHMACMiddleware_TimestampRequired verifies that when TimestampHeader is
// configured, requests without the header are rejected — the previous behaviour
// silently skipped the check, allowing replay attacks via simple header omission.
func TestHMACMiddleware_TimestampRequired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := DefaultHMACConfig()
	cfg.Secret = "test-secret"
	v := NewHMACVerifier(cfg, zap.NewNop())

	r := gin.New()
	r.Use(v.GinMiddleware())
	r.POST("/hook", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	payload := []byte(`{"event":"ping"}`)
	sig := v.SignPayload(payload)

	tests := []struct {
		name       string
		timestamp  string
		wantStatus int
	}{
		{"missing_timestamp", "", http.StatusUnauthorized},
		{"valid_timestamp", time.Now().UTC().Format(time.RFC3339), http.StatusOK},
		{"too_old", time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339), http.StatusUnauthorized},
		{"future_skew", time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339), http.StatusUnauthorized},
		{"malformed", "not-a-timestamp", http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", "/hook", bytes.NewReader(payload))
			req.Header.Set(cfg.HeaderName, sig)
			if tc.timestamp != "" {
				req.Header.Set(cfg.TimestampHeader, tc.timestamp)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tc.wantStatus {
				t.Errorf("%s: expected status %d, got %d (body=%s)",
					tc.name, tc.wantStatus, w.Code, w.Body.String())
			}
		})
	}
}
