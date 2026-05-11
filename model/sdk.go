package model

import (
	"encoding/json"
	"time"
)

// AuthRecord is the PostgreSQL representation of SDK cliproxyauth.Auth.
// Runtime-only fields such as Index, FileName, Storage, Runtime, and counters are intentionally omitted.
type AuthRecord struct {
	ID               string          `gorm:"primaryKey;size:128"`
	Provider         string          `gorm:"size:64;not null;index"`
	Prefix           string          `gorm:"size:128;index"`
	Label            string          `gorm:"size:255"`
	Status           string          `gorm:"size:64;index"`
	StatusMessage    string          `gorm:"size:512"`
	Disabled         bool            `gorm:"not null;default:false"`
	Unavailable      bool            `gorm:"not null;default:false"`
	ProxyURL         string          `gorm:"size:1024"`
	Attributes       json.RawMessage `gorm:"type:jsonb"`
	Metadata         json.RawMessage `gorm:"type:jsonb"`
	Quota            json.RawMessage `gorm:"type:jsonb"`
	ModelStates      json.RawMessage `gorm:"type:jsonb"`
	LastError        json.RawMessage `gorm:"type:jsonb"`
	CreatedAt        time.Time       `gorm:"autoCreateTime"`
	UpdatedAt        time.Time       `gorm:"autoUpdateTime"`
	LastRefreshedAt  time.Time
	NextRefreshAfter time.Time
	NextRetryAfter   time.Time
}

// TableName pins the table name required by the gateway schema.
func (AuthRecord) TableName() string {
	return "auth_records"
}

// ProviderConfig stores JSON configuration blobs keyed by provider name.
type ProviderConfig struct {
	ID         uint            `gorm:"primaryKey"`
	Provider   string          `gorm:"uniqueIndex;size:128;not null"`
	ConfigData json.RawMessage `gorm:"type:jsonb"`
	CreatedAt  time.Time       `gorm:"autoCreateTime"`
	UpdatedAt  time.Time       `gorm:"autoUpdateTime"`
}

// OAuthSession tracks OAuth authorization flows.
type OAuthSession struct {
	ID         uint            `gorm:"primaryKey"`
	Provider   string          `gorm:"size:64;not null"`
	State      string          `gorm:"uniqueIndex;size:255;not null"`
	AuthURL    string          `gorm:"size:1024"`
	Status     string          `gorm:"size:32;default:'pending'"`
	AuthID     *string         `gorm:"size:128"`
	ConfigData json.RawMessage `gorm:"type:jsonb"`
	CreatedAt  time.Time       `gorm:"autoCreateTime"`
	ExpiresAt  time.Time       `gorm:"index;not null"`
}

// AmpcodeConfig stores the ampcode configuration JSON blob.
type AmpcodeConfig struct {
	ID         uint            `gorm:"primaryKey"`
	ConfigData json.RawMessage `gorm:"type:jsonb"`
	CreatedAt  time.Time       `gorm:"autoCreateTime"`
	UpdatedAt  time.Time       `gorm:"autoUpdateTime"`
}
