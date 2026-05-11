package model

import "time"

// BalanceLog records every balance change.
type BalanceLog struct {
	ID        uint      `gorm:"primaryKey"`
	UserID    uint      `gorm:"index;not null"`
	Amount    float64   `gorm:"not null"`
	Type      string    `gorm:"size=32;not null"` // e.g. "precharge", "settle", "refund"
	Reference string    `gorm:"size=255"`         // external reference id
	CreatedAt time.Time `gorm:"autoCreateTime"`
}

// UsageLog records every AI proxy request.
type UsageLog struct {
	ID              uint      `gorm:"primaryKey"`
	UserID          uint      `gorm:"index;not null"`
	ApiKeyID        uint      `gorm:"index;not null"`
	GroupID         *uint     `gorm:"index"`
	RequestID       string    `gorm:"size=128;index"`
	IdempotencyKey  string    `gorm:"size=128;index"`
	EventKey        string    `gorm:"size=128;index"`
	Model           string    `gorm:"size=128;index"`
	Provider        string    `gorm:"size=64;index"`
	AuthID          string    `gorm:"size=128"`
	TokensIn        int       `gorm:"default:0"`
	TokensOut       int       `gorm:"default:0"`
	InputTokens     int       `gorm:"default:0"`
	OutputTokens    int       `gorm:"default:0"`
	ReasoningTokens int       `gorm:"default:0"`
	CachedTokens    int       `gorm:"default:0"`
	InputCost       float64   `gorm:"default:0"`
	OutputCost      float64   `gorm:"default:0"`
	TotalCost       float64   `gorm:"default:0"`
	ActualCost      float64   `gorm:"default:0"`
	Cost            float64   `gorm:"default:0"`
	RateMultiplier  float64   `gorm:"default:1.0"`
	Stream          bool      `gorm:"default:false"`
	DurationMs      int64     `gorm:"default:0"`
	IPAddress       string    `gorm:"size=64"`
	RawMetadata     []byte    `gorm:"type:jsonb"`
	Failed          bool      `gorm:"default:false;index"`
	CreatedAt       time.Time `gorm:"autoCreateTime;index"`
}
