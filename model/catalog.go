package model

import "time"

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
