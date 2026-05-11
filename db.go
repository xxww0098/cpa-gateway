package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// InitDB establishes a PostgreSQL connection via GORM using cfg.Database.
// It configures a connection pool and returns the *gorm.DB instance.
func InitDB(cfg *Config) (*gorm.DB, error) {
	dsn := cfg.Database.DSN()

	gcfg := &gorm.Config{
		Logger: logger.Default.LogMode(logger.Info),
	}

	db, err := gorm.Open(postgres.Open(dsn), gcfg)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("getting underlying sql.DB: %w", err)
	}

	// Connection pool settings.
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetMaxOpenConns(100)
	sqlDB.SetConnMaxLifetime(time.Hour)

	slog.Info("database connection established")
	return db, nil
}

// AutoMigrate runs GORM AutoMigrate for all models.
func AutoMigrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&User{},
		&ApiKey{},
		&Group{},
		&BalanceLog{},
		&UsageLog{},
		&SubscriptionPackage{},
		&Subscription{},
		&Ticket{},
		&TicketReply{},
		&ModelPrice{},
		&ModelCatalogEntry{},
		&AmpcodeConfig{},
		&OAuthSession{},
		&ProviderConfig{},
		&AuthRecord{},
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// Model definitions
// ─────────────────────────────────────────────────────────────────────────────

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

// SubscriptionPackage defines self-service subscription products.
type SubscriptionPackage struct {
	ID                   uint    `gorm:"primaryKey"`
	Name                 string  `gorm:"size=128;not null"`
	Description          string  `gorm:"size=512"`
	GroupID              uint    `gorm:"index;not null"`
	RateMultiplier       float64 `gorm:"default:1.0"`
	DefaultValidityDays  int     `gorm:"default:30"`
	DailyLimitUSD        *float64
	WeeklyLimitUSD       *float64
	MonthlyLimitUSD      *float64
	SubscriptionPriceUSD float64   `gorm:"column:subscription_price_usd;default:0"`
	Enabled              bool      `gorm:"default:true"`
	CreatedAt            time.Time `gorm:"autoCreateTime"`
	UpdatedAt            time.Time `gorm:"autoUpdateTime"`
}

// Subscription records user subscription purchases.
type Subscription struct {
	ID               uint      `gorm:"primaryKey"`
	UserID           uint      `gorm:"index;not null"`
	PackageID        uint      `gorm:"index;not null"`
	GroupID          uint      `gorm:"index;not null"`
	GroupName        string    `gorm:"size=128"`
	Status           string    `gorm:"size=32;index;default:'active'"`
	StartsAt         time.Time `gorm:"not null"`
	ExpiresAt        time.Time `gorm:"index;not null"`
	DailyUsageUSD    float64   `gorm:"default:0"`
	WeeklyUsageUSD   float64   `gorm:"default:0"`
	MonthlyUsageUSD  float64   `gorm:"column:monthly_usage_usd;default:0"`
	DailyLimitUSD    *float64
	WeeklyLimitUSD   *float64
	MonthlyLimitUSD  *float64  `gorm:"column:monthly_limit_usd"`
	FundingSource    string    `gorm:"size=64"`
	FundingReference string    `gorm:"size=255"`
	PricePaidUSD     float64   `gorm:"column:price_paid_usd;default:0"`
	Notes            string    `gorm:"size=512"`
	CreatedAt        time.Time `gorm:"autoCreateTime"`
	UpdatedAt        time.Time `gorm:"autoUpdateTime"`
}

type Ticket struct {
	ID         uint      `gorm:"primaryKey"`
	UserID     uint      `gorm:"index;not null"`
	Title      string    `gorm:"size=200;not null"`
	Category   string    `gorm:"size=64;default:'other'"`
	Priority   string    `gorm:"size=32;default:'medium'"`
	Status     string    `gorm:"size=32;index;default:'open'"`
	AssigneeID *uint     `gorm:"index"`
	CreatedAt  time.Time `gorm:"autoCreateTime"`
	UpdatedAt  time.Time `gorm:"autoUpdateTime"`
}

type TicketReply struct {
	ID        uint      `gorm:"primaryKey"`
	TicketID  uint      `gorm:"index;not null"`
	UserID    uint      `gorm:"index;not null"`
	IsAdmin   bool      `gorm:"default:false"`
	Content   string    `gorm:"type:text;not null"`
	CreatedAt time.Time `gorm:"autoCreateTime"`
}

type ModelPrice struct {
	ID                    uint      `gorm:"primaryKey"`
	ModelID               string    `gorm:"uniqueIndex;size=128;not null"`
	InputPricePer1M       float64   `gorm:"default:0"`
	OutputPricePer1M      float64   `gorm:"default:0"`
	CachedInputPricePer1M float64   `gorm:"default:0"`
	ReasoningPricePer1M   float64   `gorm:"default:0"`
	CreatedAt             time.Time `gorm:"autoCreateTime"`
	UpdatedAt             time.Time `gorm:"autoUpdateTime"`
}

type ModelCatalogEntry struct {
	ID         uint      `gorm:"primaryKey"`
	ChannelKey string    `gorm:"uniqueIndex:idx_model_catalog_channel_model;size:128;not null"`
	ModelID    string    `gorm:"uniqueIndex:idx_model_catalog_channel_model;size:128;not null"`
	Visible    bool      `gorm:"default:true"`
	ModelsURL  string    `gorm:"size:512"`
	CreatedAt  time.Time `gorm:"autoCreateTime"`
	UpdatedAt  time.Time `gorm:"autoUpdateTime"`
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
