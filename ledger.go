package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	balanceLogTypeCredit = "credit"
	balanceLogTypeDebit  = "debit"
	holdKeyPattern       = "cpa-gateway:billing:hold:%d:*"
)

// Ledger coordinates persistent balance updates and Redis pre-charge holds.
type Ledger struct {
	db    *gorm.DB
	redis *redis.Client
}

// NewLedger constructs a billing ledger backed by GORM and optional Redis.
func NewLedger(db *gorm.DB, redisClient *redis.Client) *Ledger {
	return &Ledger{db: db, redis: redisClient}
}

// Credit adds amount to a user's balance and writes a balance log in one DB transaction.
func (l *Ledger) Credit(ctx context.Context, userID uint, amount float64, ref string) error {
	if amount <= 0 {
		return fmt.Errorf("credit amount must be positive")
	}

	return l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, userID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrUserNotFound
			}
			return err
		}

		user.Balance += amount
		if err := tx.Model(&user).Update("balance", user.Balance).Error; err != nil {
			return err
		}

		return tx.Create(&BalanceLog{
			UserID:    userID,
			Amount:    amount,
			Type:      balanceLogTypeCredit,
			Reference: ref,
		}).Error
	})
}

// Debit subtracts amount from a user's balance and writes a balance log in one DB transaction.
func (l *Ledger) Debit(ctx context.Context, userID uint, amount float64, ref string) error {
	if amount <= 0 {
		return fmt.Errorf("debit amount must be positive")
	}

	return l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, userID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrUserNotFound
			}
			return err
		}

		if user.Balance < amount {
			return ErrInsufficientBalance
		}

		user.Balance -= amount
		if err := tx.Model(&user).Update("balance", user.Balance).Error; err != nil {
			return err
		}

		return tx.Create(&BalanceLog{
			UserID:    userID,
			Amount:    -amount,
			Type:      balanceLogTypeDebit,
			Reference: ref,
		}).Error
	})
}

// Hold reserves amount for a request using Redis SET NX and the provided TTL.
func (l *Ledger) Hold(ctx context.Context, userID uint, amount float64, requestID string, ttl time.Duration) error {
	if l.redis == nil {
		return fmt.Errorf("redis client not configured")
	}
	if amount <= 0 {
		return fmt.Errorf("hold amount must be positive")
	}
	if requestID == "" {
		return fmt.Errorf("requestID is required")
	}
	if ttl <= 0 {
		return fmt.Errorf("hold ttl must be positive")
	}

	available, err := l.GetBalance(ctx, userID)
	if err != nil {
		return err
	}
	if available < amount {
		return ErrInsufficientBalance
	}

	ok, err := l.redis.SetNX(ctx, holdKey(userID, requestID), strconv.FormatFloat(amount, 'f', -1, 64), ttl).Result()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("hold already exists for request %s", requestID)
	}

	return nil
}

// Settle removes the Redis hold for requestID, then debits the actual amount from the DB balance.
func (l *Ledger) Settle(ctx context.Context, userID uint, requestID string, actualAmount float64) error {
	if requestID == "" {
		return fmt.Errorf("requestID is required")
	}
	if l.redis != nil {
		if err := l.redis.Del(ctx, holdKey(userID, requestID)).Err(); err != nil {
			return err
		}
	}
	if actualAmount <= 0 {
		return nil
	}

	return l.Debit(ctx, userID, actualAmount, requestID)
}

// Release removes a Redis hold without changing persistent balance.
func (l *Ledger) Release(ctx context.Context, userID uint, requestID string) error {
	if requestID == "" {
		return fmt.Errorf("requestID is required")
	}
	if l.redis == nil {
		return nil
	}

	return l.redis.Del(ctx, holdKey(userID, requestID)).Err()
}

// GetBalance returns DB balance minus active Redis holds. If Redis is nil, only DB balance is returned.
func (l *Ledger) GetBalance(ctx context.Context, userID uint) (float64, error) {
	var user User
	if err := l.db.WithContext(ctx).First(&user, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, ErrUserNotFound
		}
		return 0, err
	}

	if l.redis == nil {
		return user.Balance, nil
	}

	held, err := l.activeHolds(ctx, userID)
	if err != nil {
		return 0, err
	}

	return user.Balance - held, nil
}

func (l *Ledger) activeHolds(ctx context.Context, userID uint) (float64, error) {
	var cursor uint64
	var total float64

	for {
		keys, nextCursor, err := l.redis.Scan(ctx, cursor, fmt.Sprintf(holdKeyPattern, userID), 100).Result()
		if err != nil {
			return 0, err
		}

		for _, key := range keys {
			value, err := l.redis.Get(ctx, key).Result()
			if errors.Is(err, redis.Nil) {
				continue
			}
			if err != nil {
				return 0, err
			}

			amount, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return 0, fmt.Errorf("parsing hold amount for %s: %w", key, err)
			}
			total += amount
		}

		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}

	return total, nil
}

func holdKey(userID uint, requestID string) string {
	return fmt.Sprintf("cpa-gateway:billing:hold:%d:%s", userID, requestID)
}
