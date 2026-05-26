package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func TestJWTManager_GenerateAndValidate(t *testing.T) {
	logger := zap.NewNop()
	cfg := &JWTConfig{
		SecretKey:   "test-secret-key-32-bytes-long-ok",
		Issuer:      "test-agent",
		TokenExpiry: time.Hour,
	}
	mgr := NewJWTManager(cfg, logger)

	token, err := mgr.GenerateToken("user-123", "", RoleDev, "test@example.com")
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}

	claims, err := mgr.ValidateToken(token)
	if err != nil {
		t.Fatalf("ValidateToken failed: %v", err)
	}
	if claims.UserID != "user-123" {
		t.Errorf("expected user-123, got %s", claims.UserID)
	}
	if claims.Role != RoleDev {
		t.Errorf("expected dev role, got %s", claims.Role)
	}
	if claims.Email != "test@example.com" {
		t.Errorf("expected test@example.com, got %s", claims.Email)
	}
}

func TestJWTManager_ExpiredToken(t *testing.T) {
	logger := zap.NewNop()
	cfg := &JWTConfig{
		SecretKey:   "test-secret-key-32-bytes-long-ok",
		Issuer:      "test-agent",
		TokenExpiry: -time.Hour, // Already expired
	}
	mgr := NewJWTManager(cfg, logger)

	token, err := mgr.GenerateToken("user-123", "", RoleDev, "")
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}

	_, err = mgr.ValidateToken(token)
	if err != ErrTokenExpired {
		t.Errorf("expected ErrTokenExpired, got %v", err)
	}
}

func TestJWTManager_InvalidToken(t *testing.T) {
	logger := zap.NewNop()
	cfg := &JWTConfig{
		SecretKey:   "test-secret-key-32-bytes-long-ok",
		Issuer:      "test-agent",
		TokenExpiry: time.Hour,
	}
	mgr := NewJWTManager(cfg, logger)

	_, err := mgr.ValidateToken("invalid.token.string")
	if err != ErrTokenInvalid {
		t.Errorf("expected ErrTokenInvalid, got %v", err)
	}
}

func TestJWTManager_RevokedToken(t *testing.T) {
	logger := zap.NewNop()
	cfg := &JWTConfig{
		SecretKey:   "test-secret-key-32-bytes-long-ok",
		Issuer:      "test-agent",
		TokenExpiry: time.Hour,
	}
	mgr := NewJWTManager(cfg, logger)

	token, _ := mgr.GenerateToken("user-123", "", RoleDev, "")
	claims, _ := mgr.ValidateToken(token)

	mgr.RevokeToken(claims.ID)

	_, err := mgr.ValidateToken(token)
	if err != ErrTokenInvalid {
		t.Errorf("expected ErrTokenInvalid after revocation, got %v", err)
	}
}

func TestAPIKeyStore(t *testing.T) {
	store := NewAPIKeyStore()

	store.Register(&APIKeyEntry{
		Key:    "test-key-123",
		UserID: "svc-account",
		Role:   RoleService,
		Label:  "CI pipeline",
	})

	entry, ok := store.Validate("test-key-123")
	if !ok {
		t.Fatal("expected API key to be valid")
	}
	if entry.UserID != "svc-account" {
		t.Errorf("expected svc-account, got %s", entry.UserID)
	}
	if entry.Key != "" {
		t.Errorf("Validate must not return plaintext key, got %q", entry.Key)
	}

	_, ok = store.Validate("nonexistent")
	if ok {
		t.Error("expected invalid API key to fail")
	}

	_, ok = store.Validate("")
	if ok {
		t.Error("empty key must not validate")
	}
}

// TestAPIKeyStore_NoPlaintextStorage verifies plaintext keys are not retained
// in the store after Register. Prevents regressions on memory-dump exposure.
func TestAPIKeyStore_NoPlaintextStorage(t *testing.T) {
	store := NewAPIKeyStore()
	const secret = "super-secret-plaintext-key"

	store.Register(&APIKeyEntry{Key: secret, UserID: "u1", Role: RoleDev})

	store.mu.RLock()
	defer store.mu.RUnlock()
	for i, rec := range store.entries {
		if rec.entry.Key != "" {
			t.Fatalf("entry[%d].Key leaked plaintext %q", i, rec.entry.Key)
		}
	}
}

func TestAuthMiddleware_NoToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := zap.NewNop()
	cfg := &JWTConfig{SecretKey: "test-secret", Issuer: "test", TokenExpiry: time.Hour}
	mgr := NewJWTManager(cfg, logger)
	keys := NewAPIKeyStore()

	r := gin.New()
	r.Use(AuthMiddleware(mgr, keys, logger))
	r.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAuthMiddleware_ValidBearer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := zap.NewNop()
	cfg := &JWTConfig{SecretKey: "test-secret", Issuer: "test", TokenExpiry: time.Hour}
	mgr := NewJWTManager(cfg, logger)
	keys := NewAPIKeyStore()

	token, _ := mgr.GenerateToken("user-1", "", RoleDev, "")

	r := gin.New()
	r.Use(AuthMiddleware(mgr, keys, logger))
	r.GET("/test", func(c *gin.Context) {
		claims := GetClaims(c)
		c.JSON(200, gin.H{"user_id": claims.UserID})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestAuthMiddleware_ValidAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := zap.NewNop()
	cfg := &JWTConfig{SecretKey: "test-secret", Issuer: "test", TokenExpiry: time.Hour}
	mgr := NewJWTManager(cfg, logger)
	keys := NewAPIKeyStore()
	keys.Register(&APIKeyEntry{Key: "my-api-key", UserID: "svc-1", Role: RoleService})

	r := gin.New()
	r.Use(AuthMiddleware(mgr, keys, logger))
	r.GET("/test", func(c *gin.Context) {
		claims := GetClaims(c)
		c.JSON(200, gin.H{"user_id": claims.UserID})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	req.Header.Set("X-API-Key", "my-api-key")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestRequireRole_Admin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := zap.NewNop()
	cfg := &JWTConfig{SecretKey: "test-secret", Issuer: "test", TokenExpiry: time.Hour}
	mgr := NewJWTManager(cfg, logger)
	keys := NewAPIKeyStore()

	adminToken, _ := mgr.GenerateToken("admin-1", "", RoleAdmin, "")
	readonlyToken, _ := mgr.GenerateToken("reader-1", "", RoleReadOnly, "")

	r := gin.New()
	r.Use(AuthMiddleware(mgr, keys, logger))
	r.GET("/admin", RequireRole(RoleAdmin, RoleDev), func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	// Admin should pass
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/admin", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("admin: expected 200, got %d", w.Code)
	}

	// ReadOnly should be forbidden
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/admin", nil)
	req.Header.Set("Authorization", "Bearer "+readonlyToken)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("readonly: expected 403, got %d", w.Code)
	}
}

func TestPerUserRateLimiter(t *testing.T) {
	rl := NewPerUserRateLimiter(2, 2, time.Second)

	// First 2 requests should pass
	if !rl.Allow("user:test") {
		t.Error("expected first request to pass")
	}
	if !rl.Allow("user:test") {
		t.Error("expected second request to pass")
	}
	// Third should be blocked
	if rl.Allow("user:test") {
		t.Error("expected third request to be rate limited")
	}

	// Different user should still work
	if !rl.Allow("user:other") {
		t.Error("expected different user to pass")
	}
}
