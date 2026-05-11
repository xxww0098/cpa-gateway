package model

import "time"

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
