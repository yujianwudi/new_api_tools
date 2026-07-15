package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/new-api-tools/backend/internal/config"
)

func TestValidateTokenAcceptsOnlyHS256(t *testing.T) {
	t.Setenv("JWT_SECRET_KEY", "unit-test-secret-that-is-long-enough")
	t.Setenv("ADMIN_PASSWORD", "test-password")
	config.Load()

	valid, _, err := GenerateToken("admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateToken(valid); err != nil {
		t.Fatalf("HS256 token rejected: %v", err)
	}

	claims := Claims{RegisteredClaims: jwt.RegisteredClaims{
		Subject:   "admin",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}}
	hs512 := jwt.NewWithClaims(jwt.SigningMethodHS512, claims)
	hs512String, err := hs512.SignedString([]byte(config.Get().JWTSecretKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateToken(hs512String); err == nil {
		t.Fatal("HS512 token was accepted; only HS256 must be valid")
	}
}
