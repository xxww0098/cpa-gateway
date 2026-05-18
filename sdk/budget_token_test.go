package sdk

import (
	"sync"
	"testing"
	"time"
)

func TestNewBudgetTokenStore(t *testing.T) {
	store := NewBudgetTokenStore()
	if store == nil {
		t.Fatal("NewBudgetTokenStore returned nil")
	}
	if store.tokens == nil {
		t.Fatal("tokens map not initialized")
	}
}

func TestAcquire(t *testing.T) {
	store := NewBudgetTokenStore()
	token := store.Acquire(1, "batch-001", 5.0, 60*time.Second)

	if token == nil {
		t.Fatal("Acquire returned nil")
	}
	if token.UserID != 1 {
		t.Errorf("UserID = %d, want 1", token.UserID)
	}
	if token.BatchRequestID != "batch-001" {
		t.Errorf("BatchRequestID = %q, want %q", token.BatchRequestID, "batch-001")
	}
	if token.Remaining != 5.0 {
		t.Errorf("Remaining = %f, want 5.0", token.Remaining)
	}
	if token.ExpiresAt.Before(token.CreatedAt) {
		t.Error("ExpiresAt is before CreatedAt")
	}
}

func TestTryDeduct_Success(t *testing.T) {
	store := NewBudgetTokenStore()
	store.Acquire(1, "batch-001", 5.0, 60*time.Second)

	ok := store.TryDeduct(1, 2.0)
	if !ok {
		t.Error("TryDeduct returned false, want true")
	}

	// Check remaining was reduced
	store.mu.RLock()
	token := store.tokens[1]
	store.mu.RUnlock()

	token.mu.Lock()
	if token.Remaining != 3.0 {
		t.Errorf("Remaining = %f, want 3.0", token.Remaining)
	}
	token.mu.Unlock()
}

func TestTryDeduct_InsufficientBudget(t *testing.T) {
	store := NewBudgetTokenStore()
	store.Acquire(1, "batch-001", 2.0, 60*time.Second)

	ok := store.TryDeduct(1, 3.0)
	if ok {
		t.Error("TryDeduct returned true for insufficient budget")
	}
}

func TestTryDeduct_NoToken(t *testing.T) {
	store := NewBudgetTokenStore()

	ok := store.TryDeduct(999, 1.0)
	if ok {
		t.Error("TryDeduct returned true for non-existent user")
	}
}

func TestTryDeduct_Expired(t *testing.T) {
	store := NewBudgetTokenStore()
	store.Acquire(1, "batch-001", 5.0, 1*time.Millisecond)

	// Wait for token to expire
	time.Sleep(5 * time.Millisecond)

	ok := store.TryDeduct(1, 1.0)
	if ok {
		t.Error("TryDeduct returned true for expired token")
	}
}

func TestRelease(t *testing.T) {
	store := NewBudgetTokenStore()
	store.Acquire(1, "batch-001", 5.0, 60*time.Second)

	// Deduct some
	store.TryDeduct(1, 2.0)

	batchID, remaining := store.Release(1)
	if batchID != "batch-001" {
		t.Errorf("batchID = %q, want %q", batchID, "batch-001")
	}
	if remaining != 3.0 {
		t.Errorf("remaining = %f, want 3.0", remaining)
	}

	// Token should be removed
	store.mu.RLock()
	_, exists := store.tokens[1]
	store.mu.RUnlock()
	if exists {
		t.Error("token still exists after Release")
	}
}

func TestRelease_NoToken(t *testing.T) {
	store := NewBudgetTokenStore()

	batchID, remaining := store.Release(999)
	if batchID != "" {
		t.Errorf("batchID = %q, want empty", batchID)
	}
	if remaining != 0 {
		t.Errorf("remaining = %f, want 0", remaining)
	}
}

func TestDeductSettle(t *testing.T) {
	store := NewBudgetTokenStore()
	store.Acquire(1, "batch-001", 5.0, 60*time.Second)

	store.DeductSettle(1, 2.5)

	store.mu.RLock()
	token := store.tokens[1]
	store.mu.RUnlock()

	token.mu.Lock()
	if token.Remaining != 2.5 {
		t.Errorf("Remaining = %f, want 2.5", token.Remaining)
	}
	token.mu.Unlock()
}

func TestDeductSettle_GoesNegative(t *testing.T) {
	store := NewBudgetTokenStore()
	store.Acquire(1, "batch-001", 1.0, 60*time.Second)

	store.DeductSettle(1, 3.0)

	store.mu.RLock()
	token := store.tokens[1]
	store.mu.RUnlock()

	token.mu.Lock()
	if token.Remaining != -2.0 {
		t.Errorf("Remaining = %f, want -2.0", token.Remaining)
	}
	token.mu.Unlock()
}

func TestDeductSettle_NoToken(t *testing.T) {
	store := NewBudgetTokenStore()
	// Should not panic
	store.DeductSettle(999, 1.0)
}

func TestConcurrentTryDeduct(t *testing.T) {
	store := NewBudgetTokenStore()
	store.Acquire(1, "batch-001", 10.0, 60*time.Second)

	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex

	// 20 goroutines each trying to deduct 1.0 from a budget of 10.0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if store.TryDeduct(1, 1.0) {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if successCount != 10 {
		t.Errorf("successCount = %d, want 10", successCount)
	}
}

func TestConcurrentAcquireRelease(t *testing.T) {
	store := NewBudgetTokenStore()

	var wg sync.WaitGroup
	// Multiple goroutines acquiring and releasing for different users
	for i := uint(1); i <= 50; i++ {
		wg.Add(1)
		go func(uid uint) {
			defer wg.Done()
			store.Acquire(uid, "batch", 5.0, 60*time.Second)
			store.TryDeduct(uid, 1.0)
			store.DeductSettle(uid, 0.5)
			store.Release(uid)
		}(i)
	}
	wg.Wait()

	// All tokens should be released
	store.mu.RLock()
	remaining := len(store.tokens)
	store.mu.RUnlock()
	if remaining != 0 {
		t.Errorf("tokens remaining = %d, want 0", remaining)
	}
}
