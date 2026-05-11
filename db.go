package main

import (
	"github.com/xxww0098/cpa-gateway/infra"
	"gorm.io/gorm"
)

// InitDB establishes a PostgreSQL connection via GORM using cfg.Database.
func InitDB(cfg *Config) (*gorm.DB, error) {
	return infra.InitDB(cfg.Database.DSN())
}

// AutoMigrate runs GORM AutoMigrate for all models.
func AutoMigrate(db *gorm.DB) error {
	return infra.AutoMigrate(db)
}
