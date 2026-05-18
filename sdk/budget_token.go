package sdk

import (
	"sync"
	"time"
)

// BudgetToken represents a batch pre-deduction token that covers multiple
// requests for a single user, reducing per-request Redis Hold operations.
type BudgetToken struct {
	mu             sync.Mutex
	BatchRequestID string    // corresponds to the Redis Hold request ID
	UserID         uint
	Remaining      float64   // remaining available budget
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

// BudgetTokenStore is a process-local concurrent-safe store for BudgetTokens,
// keyed by user ID.
type BudgetTokenStore struct {
	mu     sync.RWMutex
	tokens map[uint]*BudgetToken
}

// NewBudgetTokenStore creates a new BudgetTokenStore ready for use.
func NewBudgetTokenStore() *BudgetTokenStore {
	return &BudgetTokenStore{
		tokens: make(map[uint]*BudgetToken),
	}
}

// TryDeduct attempts to deduct the given amount from the user's budget token.
// It returns true if the token is valid (not expired) and has sufficient
// remaining budget; false otherwise.
func (s *BudgetTokenStore) TryDeduct(userID uint, amount float64) bool {
	s.mu.RLock()
	token, ok := s.tokens[userID]
	s.mu.RUnlock()
	if !ok {
		return false
	}

	token.mu.Lock()
	defer token.mu.Unlock()

	if time.Now().After(token.ExpiresAt) {
		return false
	}
	if token.Remaining < amount {
		return false
	}
	token.Remaining -= amount
	return true
}

// Acquire creates a new BudgetToken for the user and stores it in the map.
// It returns the newly created token.
func (s *BudgetTokenStore) Acquire(userID uint, batchID string, budget float64, ttl time.Duration) *BudgetToken {
	now := time.Now()
	token := &BudgetToken{
		BatchRequestID: batchID,
		UserID:         userID,
		Remaining:      budget,
		CreatedAt:      now,
		ExpiresAt:      now.Add(ttl),
	}

	s.mu.Lock()
	s.tokens[userID] = token
	s.mu.Unlock()

	return token
}

// Release removes the token for the given user from the store and returns
// the batch request ID and remaining budget so the caller can release the
// unused amount back to Redis.
func (s *BudgetTokenStore) Release(userID uint) (batchID string, remaining float64) {
	s.mu.Lock()
	token, ok := s.tokens[userID]
	if !ok {
		s.mu.Unlock()
		return "", 0
	}
	delete(s.tokens, userID)
	s.mu.Unlock()

	token.mu.Lock()
	batchID = token.BatchRequestID
	remaining = token.Remaining
	token.mu.Unlock()

	return batchID, remaining
}

// DeductSettle deducts the actual cost from the user's budget token after a
// Settle completes. The remaining value can go negative, which signals that
// a new batch acquisition is needed on the next request.
func (s *BudgetTokenStore) DeductSettle(userID uint, actualCost float64) {
	s.mu.RLock()
	token, ok := s.tokens[userID]
	s.mu.RUnlock()
	if !ok {
		return
	}

	token.mu.Lock()
	token.Remaining -= actualCost
	token.mu.Unlock()
}
