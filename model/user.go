package model

import "time"

// User represents a registered account.
type User struct {
	ID           uint      `gorm:"primaryKey"`
	Email        string    `gorm:"uniqueIndex;size=255;not null"`
	PasswordHash string    `gorm:"size=255;not null"`
	Role         string    `gorm:"size=32;default:'user'"`
	Username     string    `gorm:"size=128"`
	Balance      float64   `gorm:"default:0"`
	Status       string    `gorm:"size=32;default:'active'"`
	Concurrency  int       `gorm:"default:1"`
	CreatedAt    time.Time `gorm:"autoCreateTime"`
	UpdatedAt    time.Time `gorm:"autoUpdateTime"`
}

// ApiKey represents a user's API key.
type ApiKey struct {
	ID         uint       `gorm:"primaryKey"`
	UserID     uint       `gorm:"index;not null"`
	KeyHash    string     `gorm:"uniqueIndex;size=255;not null"`
	KeyPrefix  string     `gorm:"size=16;not null"`
	Name       string     `gorm:"size=128"`
	Status     string     `gorm:"size=32;default:'active'"`
	GroupID    *uint      `gorm:"index"`
	LastUsedAt *time.Time `gorm:"index"`
	CreatedAt  time.Time  `gorm:"autoCreateTime"`
	UpdatedAt  time.Time  `gorm:"autoUpdateTime"`
}

// Group defines a quota bucket with a rate multiplier.
type Group struct {
	ID             uint      `gorm:"primaryKey"`
	Name           string    `gorm:"uniqueIndex;size=128;not null"`
	RateMultiplier float64   `gorm:"default:1.0"`
	QuotaLimit     float64   `gorm:"default:0"`
	CreatedAt      time.Time `gorm:"autoCreateTime"`
	UpdatedAt      time.Time `gorm:"autoUpdateTime"`
}
