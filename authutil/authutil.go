package authutil

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ---------------------------------------------------------------------------
// JWT
// ---------------------------------------------------------------------------

// Claims represents the JWT claims for authenticated users.
type Claims struct {
	UserID uint   `json:"user_id"`
	Email  string `json:"email"`
	jwt.RegisteredClaims
}

// GenerateJWT creates an HS256-signed JWT with a configurable expiry.
// If expiryHours <= 0, it defaults to 24 hours.
// The userID and email are embedded in the claims. The secret must be non-empty.
func GenerateJWT(userID uint, email string, secret string, expiryHours int) (string, error) {
	if secret == "" {
		return "", errors.New("JWT secret not configured")
	}

	if expiryHours <= 0 {
		expiryHours = 24
	}

	now := time.Now()
	claims := Claims{
		UserID: userID,
		Email:  email,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Duration(expiryHours) * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			Issuer:    "cpa-gateway",
			Subject:   fmt.Sprintf("%d", userID),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

// ValidateJWT parses and validates an HS256 JWT.
// Returns the claims if valid, or an error if invalid/expired.
func ValidateJWT(tokenString string, secret string) (*Claims, error) {
	if secret == "" {
		return nil, errors.New("JWT secret not configured")
	}

	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(secret), nil
	})

	if err != nil {
		return nil, fmt.Errorf("invalid JWT: %w", err)
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, errors.New("invalid JWT claims")
	}

	return claims, nil
}

// ---------------------------------------------------------------------------
// API key helpers
// ---------------------------------------------------------------------------

// KeyPrefixLen is the length of the API key prefix stored for display.
const KeyPrefixLen = 12 // "cpa-" (4) + 8 hex chars

// APIKeyPrefix returns the prefix portion of a plaintext API key for storage/display.
func APIKeyPrefix(plaintext string) string {
	if len(plaintext) < KeyPrefixLen {
		return plaintext
	}
	return plaintext[:KeyPrefixLen]
}

// HashAPIKey returns the hex-encoded SHA-256 hash of the plaintext API key.
// This is the canonical at-rest representation used for lookup.
func HashAPIKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// NewAPIKey generates a new plaintext API key with the "cpa-" prefix followed by
// 64 hex characters (32 random bytes). The full key is shown to the user only once;
// callers should persist only HashAPIKey(plaintext) and APIKeyPrefix(plaintext).
func NewAPIKey() (string, error) {
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	return "cpa-" + hex.EncodeToString(randomBytes), nil
}
