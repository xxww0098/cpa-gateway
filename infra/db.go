package infra

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// InitDB establishes a PostgreSQL connection via GORM using the provided DSN.
// It configures a connection pool and returns the *gorm.DB instance.
func InitDB(dsn string) (*gorm.DB, error) {
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
		&model.User{},
		&model.ApiKey{},
		&model.Group{},
		&model.BalanceLog{},
		&model.UsageLog{},
		&model.SubscriptionPackage{},
		&model.Subscription{},
		&model.Ticket{},
		&model.TicketReply{},
		&model.ModelPrice{},
		&model.ModelCatalogEntry{},
		&model.AmpcodeConfig{},
		&model.OAuthSession{},
		&model.ProviderConfig{},
		&model.AuthRecord{},
	)
}
