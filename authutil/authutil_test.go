package authutil

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"pgregory.net/rapid"
)

// Feature: billing-system-optimization, Property 17: JWT Expiry Configuration
// Validates: Requirements 8.1, 8.2

// TestProperty17_PositiveExpiryHours verifies that for any positive ExpiryHours (1-720),
// the issued JWT's exp claim is exactly ExpiryHours hours after iat.
func TestProperty17_PositiveExpiryHours(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		expiryHours := rapid.IntRange(1, 720).Draw(t, "expiryHours")
		userID := rapid.Uint32Range(1, 100000).Draw(t, "userID")
		email := rapid.StringMatching(`[a-z]{3,10}@[a-z]{3,8}\.[a-z]{2,4}`).Draw(t, "email")
		secret := "test-secret-key-for-property-testing"

		tokenStr, err := GenerateJWT(uint(userID), email, secret, expiryHours)
		if err != nil {
			t.Fatalf("GenerateJWT failed: %v", err)
		}

		// Parse the token without validation to inspect claims
		parser := jwt.NewParser(jwt.WithoutClaimsValidation())
		token, _, err := parser.ParseUnverified(tokenStr, &Claims{})
		if err != nil {
			t.Fatalf("ParseUnverified failed: %v", err)
		}

		claims, ok := token.Claims.(*Claims)
		if !ok {
			t.Fatal("failed to cast claims")
		}

		if claims.ExpiresAt == nil || claims.IssuedAt == nil {
			t.Fatal("exp or iat claim is nil")
		}

		exp := claims.ExpiresAt.Time
		iat := claims.IssuedAt.Time
		expectedDuration := time.Duration(expiryHours) * time.Hour
		actualDuration := exp.Sub(iat)

		if actualDuration != expectedDuration {
			t.Fatalf("expiryHours=%d: expected exp-iat=%v, got %v", expiryHours, expectedDuration, actualDuration)
		}
	})
}

// TestProperty17_ZeroOrNegativeExpiryHours verifies that for zero or negative ExpiryHours,
// the issued JWT defaults to 24 hours expiry.
func TestProperty17_ZeroOrNegativeExpiryHours(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		expiryHours := rapid.IntRange(-720, 0).Draw(t, "expiryHours")
		userID := rapid.Uint32Range(1, 100000).Draw(t, "userID")
		email := rapid.StringMatching(`[a-z]{3,10}@[a-z]{3,8}\.[a-z]{2,4}`).Draw(t, "email")
		secret := "test-secret-key-for-property-testing"

		tokenStr, err := GenerateJWT(uint(userID), email, secret, expiryHours)
		if err != nil {
			t.Fatalf("GenerateJWT failed: %v", err)
		}

		// Parse the token without validation to inspect claims
		parser := jwt.NewParser(jwt.WithoutClaimsValidation())
		token, _, err := parser.ParseUnverified(tokenStr, &Claims{})
		if err != nil {
			t.Fatalf("ParseUnverified failed: %v", err)
		}

		claims, ok := token.Claims.(*Claims)
		if !ok {
			t.Fatal("failed to cast claims")
		}

		if claims.ExpiresAt == nil || claims.IssuedAt == nil {
			t.Fatal("exp or iat claim is nil")
		}

		exp := claims.ExpiresAt.Time
		iat := claims.IssuedAt.Time
		expectedDuration := 24 * time.Hour
		actualDuration := exp.Sub(iat)

		if actualDuration != expectedDuration {
			t.Fatalf("expiryHours=%d: expected default 24h exp-iat=%v, got %v", expiryHours, expectedDuration, actualDuration)
		}
	})
}
