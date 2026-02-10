package controller

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestMintDesktopRelayJWT_SubjectAndExpiry(t *testing.T) {
	secret := []byte("test-secret-123")
	key := "desktops/jonathan"
	ttl := 1 * time.Hour

	tok, exp, err := mintDesktopRelayJWT(secret, key, ttl)
	if err != nil {
		t.Fatalf("mintDesktopRelayJWT: %v", err)
	}
	if tok == "" {
		t.Fatalf("expected non-empty token")
	}

	// Expiry is derived from wall clock; allow some slack for the test runtime.
	until := time.Until(exp)
	if until < 59*time.Minute || until > 61*time.Minute {
		t.Fatalf("unexpected expiry: until=%s exp=%s", until, exp.UTC().Format(time.RFC3339Nano))
	}

	claims := &jwt.RegisteredClaims{}
	parsed, err := jwt.ParseWithClaims(tok, claims, func(tk *jwt.Token) (any, error) {
		if tk.Method == nil || tk.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			t.Fatalf("unexpected jwt alg: %v", tk.Header["alg"])
		}
		return secret, nil
	})
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	if parsed == nil || !parsed.Valid {
		t.Fatalf("expected token to be valid")
	}
	if claims.Subject != key {
		t.Fatalf("subject mismatch: got=%q want=%q", claims.Subject, key)
	}
	if claims.ExpiresAt == nil || claims.ExpiresAt.Time.IsZero() {
		t.Fatalf("expected exp claim to be set")
	}
}
