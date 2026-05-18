package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	balanceLogTypeCredit       = "credit"
	balanceLogTypeDebit        = "debit"
	balanceLogTypeHold         = "hold"
	balanceLogTypeSettle       = "settle"
	balanceLogTypeRelease      = "release"
	balanceLogTypeSettleFailed = "settle_failed"

	defaultBalanceTTL = 30 * time.Second
	defaultHoldTTL    = 5 * time.Minute
)

// buildAuditMetadata constructs a JSON metadata payload for BalanceLog entries.
// Extra key-value pairs can be provided via the extras map.
func buildAuditMetadata(userID uint, extras map[string]interface{}) []byte {
	m := map[string]interface{}{
		"user_id":   userID,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}
	for k, v := range extras {
		m[k] = v
	}
	data, err := json.Marshal(m)
	if err != nil {
		log.Printf("ledger: failed to marshal audit metadata: %v", err)
		return nil
	}
	return data
}

// ErrInsufficientBalance indicates the user does not have enough available balance.
var ErrInsufficientBalance = errors.New("insufficient balance")

// ErrUserNotFound indicates the requested user does not exist.
var ErrUserNotFound = errors.New("user not found")

// Ledger coordinates persistent balance updates and Redis pre-charge holds.
type Ledger struct {
	db         *gorm.DB
	redis      *redis.Client
	holdScript *redis.Script // atomic Hold Lua script
	balanceTTL time.Duration // balance cache TTL (default: 30s)
	holdTTL    time.Duration // Hold max lifetime (default: 5min)
}

// New constructs a billing ledger backed by GORM and optional Redis.
func New(db *gorm.DB, redisClient *redis.Client) *Ledger {
	return &Ledger{
		db:         db,
		redis:      redisClient,
		holdScript: holdScript,
		balanceTTL: defaultBalanceTTL,
		holdTTL:    defaultHoldTTL,
	}
}

// NewWithConfig constructs a billing ledger with configurable TTLs.
func NewWithConfig(db *gorm.DB, redisClient *redis.Client, balanceTTL, holdTTL time.Duration) *Ledger {
	if balanceTTL <= 0 {
		balanceTTL = defaultBalanceTTL
	}
	if holdTTL <= 0 {
		holdTTL = defaultHoldTTL
	}
	return &Ledger{
		db:         db,
		redis:      redisClient,
		holdScript: holdScript,
		balanceTTL: balanceTTL,
		holdTTL:    holdTTL,
	}
}

// Credit adds amount to a user's balance and writes a balance log in one DB transaction.
// After commit, the Redis balance cache is invalidated.
func (l *Ledger) Credit(ctx context.Context, userID uint, amount float64, ref string) error {
	if amount <= 0 {
		return fmt.Errorf("credit amount must be positive")
	}

	err := l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user model.User
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

		return tx.Create(&model.BalanceLog{
			UserID:    userID,
			Amount:    amount,
			Type:      balanceLogTypeCredit,
			Reference: ref,
		}).Error
	})
	if err != nil {
		return err
	}

	// Invalidate Redis balance cache after successful PG commit.
	l.invalidateBalanceCache(ctx, userID)
	return nil
}

// Debit subtracts amount from a user's balance and writes a balance log in one DB transaction.
// After commit, the Redis balance cache is invalidated.
func (l *Ledger) Debit(ctx context.Context, userID uint, amount float64, ref string) error {
	if amount <= 0 {
		return fmt.Errorf("debit amount must be positive")
	}

	err := l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user model.User
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

		return tx.Create(&model.BalanceLog{
			UserID:    userID,
			Amount:    -amount,
			Type:      balanceLogTypeDebit,
			Reference: ref,
		}).Error
	})
	if err != nil {
		return err
	}

	// Invalidate Redis balance cache after successful PG commit.
	l.invalidateBalanceCache(ctx, userID)
	return nil
}

// Hold reserves amount for a request using the atomic Lua script.
// If the balance cache is missing, it loads from PG, caches, and retries.
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

	keys := []string{
		balanceKey(userID),
		holdsKey(userID),
		holdsTSKey(userID),
	}
	now := time.Now().Unix()
	holdTTLSec := int64(l.holdTTL.Seconds())

	args := []interface{}{
		strconv.FormatFloat(amount, 'f', -1, 64),
		requestID,
		now,
		holdTTLSec,
		"", // idempotency key (empty for now)
	}

	err := l.runHoldScript(ctx, keys, args)
	if err != nil {
		if isCacheMiss(err) {
			// Load balance from PG, cache it, then retry.
			if cacheErr := l.refreshBalanceCache(ctx, userID); cacheErr != nil {
				return cacheErr
			}
			err = l.runHoldScript(ctx, keys, args)
		}
	}

	if err != nil {
		if isInsufficientBalance(err) {
			return ErrInsufficientBalance
		}
		return err
	}

	// Set TTL on the sorted set and timestamp hash to auto-expire idle keys.
	keyTTL := l.holdTTL + 60*time.Second
	l.redis.Expire(ctx, holdsKey(userID), keyTTL)
	l.redis.Expire(ctx, holdsTSKey(userID), keyTTL)

	// Best-effort audit log for the hold operation.
	l.writeAuditLog(ctx, userID, amount, balanceLogTypeHold, requestID, nil)

	return nil
}

// Settle debits the actual amount from persistent balance and clears the
// Redis hold for requestID.
//
// Semantics (partial-debit mode):
//   - actualAmount <= 0 fast path: no DB transaction. The Redis hold is cleared
//     immediately and a zero-amount settle audit row is written best-effort,
//     preserving the prior behavior for callers that only need to release the
//     reservation (e.g. upstream returned with zero cost attribution).
//   - actualAmount > 0 path: one DB transaction with SELECT ... FOR UPDATE on
//     the user row. The ledger debits min(balance, actualAmount), records one
//     settle BalanceLog for the debited portion, and — when actualAmount
//     exceeds the balance — records a second zero-amount settle BalanceLog
//     whose Metadata carries the shortfall_usd so operators can reconcile the
//     outstanding debt. The Redis hold is cleared ONLY after the transaction
//     commits successfully. A transaction or commit failure returns an error
//     and leaves the hold in place; a post-commit Redis failure also returns
//     an error, with the hold still in place (acceptable degraded state).
//
// Settle never returns ErrInsufficientBalance for a shortfall. Callers that
// need to observe the shortfall read the BalanceLog rows keyed by
// Reference == requestID.
func (l *Ledger) Settle(ctx context.Context, userID uint, requestID string, actualAmount float64) error {
	if requestID == "" {
		return fmt.Errorf("requestID is required")
	}

	// Fast path: no actual debit required. Preserve the existing "clear hold
	// without a DB transaction" behavior so zero-cost settlements stay cheap.
	if actualAmount <= 0 {
		if l.redis != nil {
			l.redis.ZRem(ctx, holdsKey(userID), requestID)
			l.redis.HDel(ctx, holdsTSKey(userID), requestID)
		}
		l.invalidateBalanceCache(ctx, userID)
		l.writeAuditLog(ctx, userID, 0, balanceLogTypeSettle, requestID, nil)
		return nil
	}

	// Partial-debit path: persist debit + optional shortfall inside one tx,
	// then clear the Redis hold only after the tx commits.
	err := l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, userID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrUserNotFound
			}
			return err
		}

		// debited is floored at 0 to guard against a pathological negative
		// balance silently crediting the user through Settle.
		debited := min(user.Balance, actualAmount)
		if debited < 0 {
			debited = 0
		}
		shortfall := actualAmount - debited

		user.Balance -= debited
		if err := tx.Model(&user).Update("balance", user.Balance).Error; err != nil {
			return err
		}

		// Settle row for the debited portion. Amount is -debited (which may be
		// zero when the user had no balance at all to debit).
		settleMetadata := buildAuditMetadata(userID, map[string]interface{}{
			"actual_cost": debited,
		})
		if err := tx.Create(&model.BalanceLog{
			UserID:    userID,
			Amount:    -debited,
			Type:      balanceLogTypeSettle,
			Reference: requestID,
			Metadata:  settleMetadata,
		}).Error; err != nil {
			return err
		}

		// Shortfall row: zero-amount marker recording the unpaid portion so
		// downstream reconciliation (HasUnresolvedShortfall, compensating
		// Credit) can pair the debt with this request.
		if shortfall > 0 {
			shortfallMetadata := buildAuditMetadata(userID, map[string]interface{}{
				"shortfall_usd": shortfall,
				"actual_cost":   actualAmount,
			})
			if err := tx.Create(&model.BalanceLog{
				UserID:    userID,
				Amount:    0,
				Type:      balanceLogTypeSettle,
				Reference: requestID,
				Metadata:  shortfallMetadata,
			}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// Transaction failed. The Redis hold MUST remain so the request can
		// be reconciled or retried. Best-effort audit log outside the failed
		// tx; do not clear the hold.
		l.writeAuditLog(ctx, userID, 0, balanceLogTypeSettleFailed, requestID, map[string]interface{}{
			"reason": err.Error(),
		})
		return err
	}

	// Persistent writes committed. Clear the Redis hold. If Redis fails here,
	// the caller receives an error; the hold is allowed to persist until the
	// next reconcile or TTL expiry.
	if l.redis != nil {
		if err := l.redis.ZRem(ctx, holdsKey(userID), requestID).Err(); err != nil {
			return err
		}
		if err := l.redis.HDel(ctx, holdsTSKey(userID), requestID).Err(); err != nil {
			return err
		}
	}
	l.invalidateBalanceCache(ctx, userID)
	return nil
}

// HasUnresolvedShortfall reports whether the user owns at least one settle
// BalanceLog row whose Metadata.shortfall_usd > 0 and has NOT been paired
// with a compensating credit.
//
// Resolution convention (Requirement 2.5, 2.6 — see design.md "Data Model
// Changes" / "Unresolved shortfall query predicate"): a compensating credit
// is a BalanceLog row where
//
//	type = 'credit'
//	reference = 'shortfall_resolve:' || <debit.reference> || ':' || <debit.id>
//
// The predicate therefore looks for any settle-row D with a positive
// shortfall_usd that has no matching credit-row C pinned to its (reference,
// id) pair. A single unresolved row is enough for the caller (HoldMiddleware
// preflight, PurchaseSubscriptionHandler preflight) to block downstream
// billable work with HTTP 402 outstanding_debt until ops (or the user via
// top-up + manual credit) resolves the shortfall.
//
// Dialect handling: SQLite uses json_extract on the raw metadata blob;
// PostgreSQL casts the bytea to jsonb and uses the ->> accessor. Both
// branches coerce missing keys / NULL to zero so a settle row without any
// shortfall key never contributes to the result. This keeps the predicate
// safe against a partially-populated metadata blob (e.g. older rows that
// predate this feature).
//
// The method is purely additive — it does not mutate any state and is
// independent of Hold / Settle / Release. Errors from the underlying driver
// propagate to the caller; the caller MUST treat an error as "unknown" and
// fail open or closed per its policy (current callers fail closed with a
// 500, matching the project's default-deny posture).
func (l *Ledger) HasUnresolvedShortfall(ctx context.Context, userID uint) (bool, error) {
	// Build the JSON-extraction and id-cast expressions based on the
	// dialect. Keeping this local avoids a package-level init that would
	// couple ledger construction to driver discovery.
	var shortfallExpr, idExpr string
	switch l.db.Dialector.Name() {
	case "sqlite":
		// json_extract returns NULL for missing keys; CAST(NULL AS REAL)
		// is NULL and "NULL > 0" evaluates to false, so rows without a
		// shortfall_usd key drop out of the result set naturally.
		shortfallExpr = "CAST(json_extract(d.metadata, '$.shortfall_usd') AS REAL) > 0"
		// SQLite performs implicit text conversion on || with integers,
		// so the bare column reference is fine.
		idExpr = "d.id"
	default:
		// Postgres (and any jsonb-aware dialect). COALESCE guards against
		// NULL from ->> when the key is absent so the comparison never
		// short-circuits on a type error.
		shortfallExpr = "COALESCE((d.metadata::jsonb ->> 'shortfall_usd')::float, 0) > 0"
		// Integer column needs an explicit text cast before the ||
		// concatenation in Postgres.
		idExpr = "d.id::text"
	}

	// COUNT(*) with LIMIT-1 semantics via an EXISTS-like predicate is
	// expressed as a plain count; the indexed balance_logs.reference
	// column plus the user_id index make this cheap for any realistic
	// shortfall volume. We do not switch to SELECT EXISTS because GORM's
	// Scan(&int64) is friendlier to portable bool materialization than
	// SELECT EXISTS(...) which returns a driver-specific boolean type.
	sql := fmt.Sprintf(`
SELECT COUNT(*) FROM balance_logs d
WHERE d.user_id = ?
  AND d.type = ?
  AND %s
  AND NOT EXISTS (
    SELECT 1 FROM balance_logs c
    WHERE c.user_id = d.user_id
      AND c.type = ?
      AND c.reference = 'shortfall_resolve:' || d.reference || ':' || %s
  )`, shortfallExpr, idExpr)

	var count int64
	if err := l.db.WithContext(ctx).
		Raw(sql, userID, balanceLogTypeSettle, balanceLogTypeCredit).
		Scan(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// ActiveHoldAmount returns the reserved amount for (userID, requestID) from Redis.
//
// The method reads ZSCORE holds:{userID} {requestID}:
//   - If no hold exists for that key (member missing), it returns (0, false, nil).
//   - If the hold exists, it returns (amount, true, nil).
//   - Any other Redis error is propagated to the caller.
//
// This is purely additive and does not mutate any state; callers that need the
// fallback settlement lower bound (see Requirement 1.3, 1.5) can use it to read
// the Active_Hold_Amount without going through Hold/Settle/Release.
func (l *Ledger) ActiveHoldAmount(ctx context.Context, userID uint, requestID string) (float64, bool, error) {
	if requestID == "" {
		return 0, false, fmt.Errorf("requestID is required")
	}
	if l.redis == nil {
		return 0, false, fmt.Errorf("redis client not configured")
	}

	score, err := l.redis.ZScore(ctx, holdsKey(userID), requestID).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			// Member is absent from the sorted set: no active hold.
			return 0, false, nil
		}
		return 0, false, err
	}
	return score, true, nil
}

// Release removes a hold from the sorted set without changing persistent balance.
// After removal, the Redis balance cache is invalidated.
func (l *Ledger) Release(ctx context.Context, userID uint, requestID string) error {
	if requestID == "" {
		return fmt.Errorf("requestID is required")
	}
	if l.redis == nil {
		return nil
	}

	// Remove hold from sorted set and timestamps hash.
	l.redis.ZRem(ctx, holdsKey(userID), requestID)
	l.redis.HDel(ctx, holdsTSKey(userID), requestID)

	// Invalidate balance cache so next GetBalance reflects the released hold.
	l.invalidateBalanceCache(ctx, userID)

	// Best-effort audit log for the release operation.
	l.writeAuditLog(ctx, userID, 0, balanceLogTypeRelease, requestID, nil)

	return nil
}

// GetBalance returns the available balance (cached balance minus active holds).
// Uses the Lua getBalanceScript for atomic computation.
// If Redis is nil, only DB balance is returned.
func (l *Ledger) GetBalance(ctx context.Context, userID uint) (float64, error) {
	if l.redis == nil {
		return l.getDBBalance(ctx, userID)
	}

	keys := []string{
		balanceKey(userID),
		holdsKey(userID),
		holdsTSKey(userID),
	}
	now := time.Now().Unix()
	holdTTLSec := int64(l.holdTTL.Seconds())

	result, err := getBalanceScript.Run(ctx, l.redis, keys, now, holdTTLSec).Result()
	if err != nil {
		if isCacheMiss(err) {
			// Load balance from PG, cache it, then retry.
			if cacheErr := l.refreshBalanceCache(ctx, userID); cacheErr != nil {
				return 0, cacheErr
			}
			result, err = getBalanceScript.Run(ctx, l.redis, keys, now, holdTTLSec).Result()
			if err != nil {
				return 0, err
			}
		} else {
			return 0, err
		}
	}

	available, parseErr := strconv.ParseFloat(result.(string), 64)
	if parseErr != nil {
		return 0, fmt.Errorf("parsing available balance: %w", parseErr)
	}
	return available, nil
}

// runHoldScript executes the hold Lua script and returns any error.
func (l *Ledger) runHoldScript(ctx context.Context, keys []string, args []interface{}) error {
	result, err := l.holdScript.Run(ctx, l.redis, keys, args...).Result()
	if err != nil {
		return err
	}
	// The script returns "OK" on success.
	if s, ok := result.(string); ok && s == "OK" {
		return nil
	}
	return fmt.Errorf("unexpected hold script result: %v", result)
}

// refreshBalanceCache loads the user's balance from PG and caches it in Redis.
func (l *Ledger) refreshBalanceCache(ctx context.Context, userID uint) error {
	balance, err := l.getDBBalance(ctx, userID)
	if err != nil {
		return err
	}
	return l.redis.Set(ctx, balanceKey(userID), strconv.FormatFloat(balance, 'f', -1, 64), l.balanceTTL).Err()
}

// getDBBalance reads the user's persistent balance from PostgreSQL.
func (l *Ledger) getDBBalance(ctx context.Context, userID uint) (float64, error) {
	var user model.User
	if err := l.db.WithContext(ctx).First(&user, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, ErrUserNotFound
		}
		return 0, err
	}
	return user.Balance, nil
}

// invalidateBalanceCache deletes the cached balance key in Redis.
func (l *Ledger) invalidateBalanceCache(ctx context.Context, userID uint) {
	if l.redis == nil {
		return
	}
	l.redis.Del(ctx, balanceKey(userID))
}

// isCacheMiss checks if the error is a CACHE_MISS from the Lua script.
func isCacheMiss(err error) bool {
	return err != nil && strings.Contains(err.Error(), "CACHE_MISS")
}

// isInsufficientBalance checks if the error is an INSUFFICIENT_BALANCE from the Lua script.
func isInsufficientBalance(err error) bool {
	return err != nil && strings.Contains(err.Error(), "INSUFFICIENT_BALANCE")
}

// writeAuditLog writes a BalanceLog record as a best-effort operation.
// Errors are logged but do not propagate to the caller.
func (l *Ledger) writeAuditLog(ctx context.Context, userID uint, amount float64, logType string, reference string, extras map[string]interface{}) {
	metadata := buildAuditMetadata(userID, extras)
	entry := &model.BalanceLog{
		UserID:    userID,
		Amount:    amount,
		Type:      logType,
		Reference: reference,
		Metadata:  metadata,
	}
	if err := l.db.WithContext(ctx).Create(entry).Error; err != nil {
		log.Printf("ledger: failed to write audit log type=%s user=%d ref=%s: %v", logType, userID, reference, err)
	}
}
