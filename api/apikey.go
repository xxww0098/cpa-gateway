package api

import (
	"errors"
	"fmt"

	"github.com/xxww0098/cpa-gateway/authutil"
	"github.com/xxww0098/cpa-gateway/infra"
	"github.com/xxww0098/cpa-gateway/model"
)

// CachedKey is an alias for infra.CachedKey used by panel handlers.
type CachedKey = infra.CachedKey

// GenerateAPIKey creates a new API key for a user and persists it.
// The plaintext is returned to the caller (shown once); only the SHA-256
// hash and prefix are stored.
func (pr *PanelRouter) GenerateAPIKey(userID uint, name string, groupID *uint) (string, *model.ApiKey, error) {
	if pr == nil || pr.DB == nil {
		return "", nil, errors.New("database not initialized")
	}

	plaintext, err := authutil.NewAPIKey()
	if err != nil {
		return "", nil, err
	}

	apiKey := model.ApiKey{
		UserID:     userID,
		KeyHash:    authutil.HashAPIKey(plaintext),
		KeyPrefix:  authutil.APIKeyPrefix(plaintext),
		Name:       name,
		Status:     "active",
		GroupID:    groupID,
		LastUsedAt: nil,
	}

	if err := pr.DB.Create(&apiKey).Error; err != nil {
		return "", nil, fmt.Errorf("failed to create API key: %w", err)
	}

	return plaintext, &apiKey, nil
}
