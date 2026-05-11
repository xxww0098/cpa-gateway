package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// Global references (set by main.go after config loads)
// ---------------------------------------------------------------------------

// GlobalDB is the PostgreSQL database connection. Set by main.go after db.go initializes.
var GlobalDB *gorm.DB

// GlobalConfig is the application configuration. Set by main.go.
var GlobalConfig *Config

// ---------------------------------------------------------------------------
// JWT types and functions
// ---------------------------------------------------------------------------

// Claims represents the JWT claims for authenticated users.
type Claims struct {
	UserID uint   `json:"user_id"`
	Email  string `json:"email"`
	jwt.RegisteredClaims
}

// GenerateJWT creates an HS256-signed JWT with 15-minute expiry.
// The userID and email are embedded in the claims.
func GenerateJWT(userID uint, email string) (string, error) {
	secret := GlobalConfig.Auth.JWT.Secret
	if secret == "" {
		return "", errors.New("JWT secret not configured")
	}

	now := time.Now()
	claims := Claims{
		UserID: userID,
		Email:  email,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(15 * time.Minute)),
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
func ValidateJWT(tokenString string) (*Claims, error) {
	secret := GlobalConfig.Auth.JWT.Secret
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
// API Key types and functions
// ---------------------------------------------------------------------------

// CachedKey holds the cached API key validation result with TTL metadata.
type CachedKey struct {
	UserID    uint
	ApiKeyID  uint
	GroupID   *uint
	RateMult  float64
	Status    string
	ExpiresAt time.Time
}

// String returns a human-readable representation of the cached key.
func (c *CachedKey) String() string {
	return fmt.Sprintf("CachedKey{UserID:%d ApiKeyID:%d GroupID:%v RateMult:%.2f Status:%s ExpiresAt:%s}",
		c.UserID, c.ApiKeyID, c.GroupID, c.RateMult, c.Status, c.ExpiresAt.Format(time.RFC3339))
}

// KeyPrefixLen is the length of the API key prefix stored for display.
const KeyPrefixLen = 12 // "cpa-" (4) + 8 hex chars

// APIKeyPrefix returns the prefix portion of a plaintext API key for storage/display.
func APIKeyPrefix(plaintext string) string {
	if len(plaintext) < KeyPrefixLen {
		return plaintext
	}
	return plaintext[:KeyPrefixLen]
}

// GenerateAPIKey generates a new API key for a user.
// Returns the plaintext key (shown only once) and an error if generation fails.
// The key format is: cpa- + 64 hex characters (total 68 chars).
// The SHA-256 hash of the plaintext is stored in the database.
func GenerateAPIKey(userID uint, name string, groupID *uint) (string, *ApiKey, error) {
	if GlobalDB == nil {
		return "", nil, errors.New("database not initialized")
	}

	// Generate 32 random bytes (64 hex chars)
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", nil, fmt.Errorf("failed to generate random bytes: %w", err)
	}

	plaintext := "cpa-" + hex.EncodeToString(randomBytes)

	// Hash the plaintext for storage
	hash := sha256.Sum256([]byte(plaintext))
	keyHash := hex.EncodeToString(hash[:])

	// Create the API key record
	apiKey := ApiKey{
		UserID:     userID,
		KeyHash:    keyHash,
		KeyPrefix:  APIKeyPrefix(plaintext),
		Name:       name,
		Status:     "active",
		GroupID:    groupID,
		LastUsedAt: nil,
	}

	if err := GlobalDB.Create(&apiKey).Error; err != nil {
		return "", nil, fmt.Errorf("failed to create API key: %w", err)
	}

	return plaintext, &apiKey, nil
}

// ValidateAPIKey validates a plaintext API key using L1 sync.Map cache with TTL.
// On cache miss, it looks up the database, caches the result, and returns it.
// Returns a CachedKey or an error if the key is invalid.
func ValidateAPIKey(plaintext string) (*CachedKey, error) {
	// Compute hash of the plaintext key
	hash := sha256.Sum256([]byte(plaintext))
	keyHash := hex.EncodeToString(hash[:])

	// Check L1 cache first
	if cached, found := apiKeyCache.Load(keyHash); found {
		ck := cached.(*CachedKey)
		if time.Now().Before(ck.ExpiresAt) {
			return ck, nil
		}
		// Expired - remove from cache
		apiKeyCache.Delete(keyHash)
	}

	// Cache miss - look up in database
	if GlobalDB == nil {
		return nil, errors.New("database not initialized")
	}

	var apiKey ApiKey
	if err := GlobalDB.Where("key_hash = ? AND status = ?", keyHash, "active").First(&apiKey).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("invalid API key")
		}
		return nil, fmt.Errorf("database lookup failed: %w", err)
	}

	// Get rate multiplier from group if set
	rateMult := 1.0
	if apiKey.GroupID != nil {
		var group Group
		if err := GlobalDB.First(&group, *apiKey.GroupID).Error; err == nil {
			rateMult = group.RateMultiplier
		}
	}

	// Build CachedKey with TTL (5 minutes)
	cachedKey := &CachedKey{
		UserID:    apiKey.UserID,
		ApiKeyID:  apiKey.ID,
		GroupID:   apiKey.GroupID,
		RateMult:  rateMult,
		Status:    apiKey.Status,
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}

	// Store in L1 cache
	apiKeyCache.Store(keyHash, cachedKey)

	// Update LastUsedAt asynchronously (fire-and-forget)
	go func() {
		if db := GlobalDB; db != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			db.WithContext(ctx).Model(&ApiKey{}).Where("id = ?", apiKey.ID).Update("last_used_at", time.Now())
		}
	}()

	return cachedKey, nil
}

// ---------------------------------------------------------------------------
// L1 Cache with TTL cleanup
// ---------------------------------------------------------------------------

// apiKeyCache is the L1 in-memory cache for validated API keys.
var apiKeyCache sync.Map

// cacheCleanup runs periodically to remove expired entries from the cache.
func startCacheCleanup(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			apiKeyCache.Range(func(key, value any) bool {
				ck := value.(*CachedKey)
				if now.After(ck.ExpiresAt) {
					apiKeyCache.Delete(key)
				}
				return true
			})
		}
	}
}
