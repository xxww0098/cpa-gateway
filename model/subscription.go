package model

import "time"

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
	DailyResetAt     time.Time `gorm:"index"` // 下次日配额重置时间（UTC）
	WeeklyUsageUSD   float64   `gorm:"default:0"`
	WeeklyResetAt    time.Time `gorm:"index"` // 下次周配额重置时间（UTC 周一 0:00）
	MonthlyUsageUSD  float64   `gorm:"column:monthly_usage_usd;default:0"`
	MonthlyResetAt   time.Time `gorm:"index"` // 下次月配额重置时间（UTC 1 号 0:00）
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

// NextDailyResetAfter returns the next UTC midnight strictly after t.
func NextDailyResetAfter(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
}

// NextWeeklyResetAfter returns the next UTC Monday 00:00 strictly after t.
// Uses ISO week convention: week starts on Monday.
func NextWeeklyResetAfter(t time.Time) time.Time {
	u := t.UTC()
	midnight := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	// Weekday: Sunday=0..Saturday=6. We want days until next Monday (strictly after t).
	// daysUntilMonday: if today is Monday and it's exactly midnight, that still counts as past,
	// so advance to next Monday (+7 days) to keep strict "after".
	weekday := int(midnight.Weekday()) // Sun=0..Sat=6
	// Convert to ISO weekday where Monday=1..Sunday=7
	isoDay := weekday
	if isoDay == 0 {
		isoDay = 7
	}
	daysUntilNextMonday := 8 - isoDay // Mon(1)->7, Tue(2)->6, ..., Sun(7)->1
	return midnight.AddDate(0, 0, daysUntilNextMonday)
}

// NextMonthlyResetAfter returns the next UTC 1st-of-month 00:00 strictly after t.
func NextMonthlyResetAfter(t time.Time) time.Time {
	u := t.UTC()
	firstOfThisMonth := time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	return firstOfThisMonth.AddDate(0, 1, 0)
}
