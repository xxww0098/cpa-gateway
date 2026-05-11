package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xxww0098/cpa-gateway/authutil"
	"github.com/xxww0098/cpa-gateway/infra"
	"github.com/xxww0098/cpa-gateway/model"
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
// API Key types and functions
// ---------------------------------------------------------------------------

// CachedKey is an alias for infra.CachedKey kept for transitional
// compatibility with existing callers in the main package. Task 6c will
// complete the migration to the api package.
type CachedKey = infra.CachedKey

// apiKeyCache is the L1 in-memory cache for validated API keys.
var apiKeyCache = infra.NewAPIKeyCache()

// GenerateAPIKey generates a new API key for a user.
// Returns the plaintext key (shown only once) and an error if generation fails.
// The key format is: cpa- + 64 hex characters (total 68 chars).
// The SHA-256 hash of the plaintext is stored in the database.
func GenerateAPIKey(userID uint, name string, groupID *uint) (string, *model.ApiKey, error) {
	if GlobalDB == nil {
		return "", nil, errors.New("database not initialized")
	}

	plaintext, err := authutil.NewAPIKey()
	if err != nil {
		return "", nil, err
	}

	// Create the API key record
	apiKey := model.ApiKey{
		UserID:     userID,
		KeyHash:    authutil.HashAPIKey(plaintext),
		KeyPrefix:  authutil.APIKeyPrefix(plaintext),
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

// ValidateAPIKey validates a plaintext API key using an L1 cache with TTL.
// On cache miss, it looks up the database, caches the result, and returns it.
// Returns a CachedKey or an error if the key is invalid.
func ValidateAPIKey(plaintext string) (*CachedKey, error) {
	keyHash := authutil.HashAPIKey(plaintext)

	// Check L1 cache first.
	if ck, found := apiKeyCache.Get(keyHash); found {
		return ck, nil
	}

	// Cache miss - look up in database.
	if GlobalDB == nil {
		return nil, errors.New("database not initialized")
	}

	var apiKey model.ApiKey
	if err := GlobalDB.Where("key_hash = ? AND status = ?", keyHash, "active").First(&apiKey).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("invalid API key")
		}
		return nil, fmt.Errorf("database lookup failed: %w", err)
	}

	// Get rate multiplier from group if set.
	rateMult := 1.0
	if apiKey.GroupID != nil {
		var group model.Group
		if err := GlobalDB.First(&group, *apiKey.GroupID).Error; err == nil {
			rateMult = group.RateMultiplier
		}
	}

	// Build CachedKey with TTL (5 minutes).
	cachedKey := &CachedKey{
		UserID:    apiKey.UserID,
		ApiKeyID:  apiKey.ID,
		GroupID:   apiKey.GroupID,
		RateMult:  rateMult,
		Status:    apiKey.Status,
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}

	// Store in L1 cache.
	apiKeyCache.Set(keyHash, cachedKey)

	// Update LastUsedAt asynchronously (fire-and-forget).
	go func() {
		if db := GlobalDB; db != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			db.WithContext(ctx).Model(&model.ApiKey{}).Where("id = ?", apiKey.ID).Update("last_used_at", time.Now())
		}
	}()

	return cachedKey, nil
}

// startCacheCleanup runs the APIKeyCache background sweeper.
func startCacheCleanup(ctx context.Context) {
	apiKeyCache.Start(ctx)
}
