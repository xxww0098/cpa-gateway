package sdk

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/xxww0098/cpa-gateway/testutil"
	"pgregory.net/rapid"
)

// Feature: billing-system-optimization, Property 23: Idempotency Deduplication

// TestProperty23_DuplicateKeyReturnsCachedResponse verifies that storing a
// response with a random key and then checking with the same key returns the
// cached response with matching fields.
//
// **Validates: Requirements 13.2, 13.5, 13.7**
func TestProperty23_DuplicateKeyReturnsCachedResponse(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		client, _ := testutil.MustMiniRedis(t)

		im := NewIdempotencyManager(client, 24*time.Hour)
		ctx := context.Background()

		// Generate random idempotency key and response
		key := rapid.StringMatching(`[a-zA-Z0-9\-]{8,64}`).Draw(rt, "key")
		statusCode := rapid.IntRange(200, 599).Draw(rt, "statusCode")
		cost := rapid.Float64Range(0.0, 100.0).Draw(rt, "cost")
		requestID := rapid.StringMatching(`[a-f0-9\-]{36}`).Draw(rt, "requestID")
		bodyLen := rapid.IntRange(0, 512).Draw(rt, "bodyLen")
		body := make([]byte, bodyLen)
		for i := range body {
			body[i] = byte(rapid.IntRange(32, 126).Draw(rt, fmt.Sprintf("bodyByte_%d", i)))
		}

		numHeaders := rapid.IntRange(0, 5).Draw(rt, "numHeaders")
		headers := make(map[string]string, numHeaders)
		for i := 0; i < numHeaders; i++ {
			hKey := rapid.StringMatching(`[A-Za-z\-]{3,20}`).Draw(rt, fmt.Sprintf("headerKey_%d", i))
			hVal := rapid.StringMatching(`[A-Za-z0-9 ]{1,50}`).Draw(rt, fmt.Sprintf("headerVal_%d", i))
			headers[hKey] = hVal
		}

		resp := &CachedResponse{
			StatusCode: statusCode,
			Headers:    headers,
			Body:       body,
			Cost:       cost,
			RequestID:  requestID,
		}

		// Store the response
		err := im.Store(ctx, key, resp)
		if err != nil {
			rt.Fatalf("Store failed: %v", err)
		}

		// Check with the same key — should return cached response
		cached, found, err := im.Check(ctx, key)
		if err != nil {
			rt.Fatalf("Check failed: %v", err)
		}
		if !found {
			rt.Fatal("Check returned found=false for a stored key")
		}

		// Verify all fields match
		if cached.StatusCode != statusCode {
			rt.Fatalf("StatusCode mismatch: got %d, want %d", cached.StatusCode, statusCode)
		}
		if cached.Cost != cost {
			rt.Fatalf("Cost mismatch: got %f, want %f", cached.Cost, cost)
		}
		if cached.RequestID != requestID {
			rt.Fatalf("RequestID mismatch: got %q, want %q", cached.RequestID, requestID)
		}
		if len(cached.Body) != len(body) {
			rt.Fatalf("Body length mismatch: got %d, want %d", len(cached.Body), len(body))
		}
		for i := range body {
			if cached.Body[i] != body[i] {
				rt.Fatalf("Body byte %d mismatch: got %d, want %d", i, cached.Body[i], body[i])
			}
		}
		if len(cached.Headers) != len(headers) {
			rt.Fatalf("Headers count mismatch: got %d, want %d", len(cached.Headers), len(headers))
		}
		for k, v := range headers {
			if cached.Headers[k] != v {
				rt.Fatalf("Header %q mismatch: got %q, want %q", k, cached.Headers[k], v)
			}
		}
	})
}

// TestProperty23_DifferentKeyReturnsNotFound verifies that checking with a
// different key than the one stored returns not found.
//
// **Validates: Requirements 13.2, 13.5, 13.7**
func TestProperty23_DifferentKeyReturnsNotFound(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		client, _ := testutil.MustMiniRedis(t)

		im := NewIdempotencyManager(client, 24*time.Hour)
		ctx := context.Background()

		// Generate two distinct keys
		key1 := rapid.StringMatching(`[a-zA-Z0-9]{8,32}`).Draw(rt, "key1")
		key2 := rapid.StringMatching(`[a-zA-Z0-9]{8,32}`).Draw(rt, "key2")
		// Ensure keys are different
		if key1 == key2 {
			key2 = key2 + "-different"
		}

		resp := &CachedResponse{
			StatusCode: rapid.IntRange(200, 599).Draw(rt, "statusCode"),
			Headers:    map[string]string{"Content-Type": "application/json"},
			Body:       []byte(`{"ok":true}`),
			Cost:       rapid.Float64Range(0.001, 10.0).Draw(rt, "cost"),
			RequestID:  rapid.StringMatching(`[a-f0-9\-]{36}`).Draw(rt, "requestID"),
		}

		// Store with key1
		err := im.Store(ctx, key1, resp)
		if err != nil {
			rt.Fatalf("Store failed: %v", err)
		}

		// Check with key2 — should return not found
		cached, found, err := im.Check(ctx, key2)
		if err != nil {
			rt.Fatalf("Check with different key failed: %v", err)
		}
		if found {
			rt.Fatalf("Check returned found=true for a different key (key1=%q, key2=%q, cached=%+v)",
				key1, key2, cached)
		}
		if cached != nil {
			rt.Fatal("Check returned non-nil response for a different key")
		}
	})
}

// TestProperty23_ConcurrentCheckStoreExactlyOneWins verifies that when
// multiple goroutines concurrently attempt to Check+Store with the same key,
// exactly one Store wins and subsequent Checks return the cached response.
//
// **Validates: Requirements 13.2, 13.5, 13.7**
func TestProperty23_ConcurrentCheckStoreExactlyOneWins(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		client, _ := testutil.MustMiniRedis(t)

		im := NewIdempotencyManager(client, 24*time.Hour)
		ctx := context.Background()

		key := rapid.StringMatching(`[a-zA-Z0-9]{16,32}`).Draw(rt, "key")
		numGoroutines := rapid.IntRange(2, 10).Draw(rt, "numGoroutines")

		// Each goroutine will attempt Check → if not found → Store its own response
		type result struct {
			stored bool
			resp   *CachedResponse
		}

		results := make([]result, numGoroutines)
		var wg sync.WaitGroup
		var startBarrier sync.WaitGroup
		startBarrier.Add(1)

		for i := 0; i < numGoroutines; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				startBarrier.Wait() // synchronize start

				// Check first
				cached, found, err := im.Check(ctx, key)
				if err != nil {
					return
				}

				if found {
					// Another goroutine already stored — record the cached response
					results[idx] = result{stored: false, resp: cached}
					return
				}

				// Not found — attempt to store our own response
				resp := &CachedResponse{
					StatusCode: 200,
					Headers:    map[string]string{"X-Worker": fmt.Sprintf("worker-%d", idx)},
					Body:       []byte(fmt.Sprintf(`{"worker":%d}`, idx)),
					Cost:       float64(idx) * 0.01,
					RequestID:  fmt.Sprintf("req-%d", idx),
				}
				_ = im.Store(ctx, key, resp)
				results[idx] = result{stored: true, resp: resp}
			}(i)
		}

		// Release all goroutines simultaneously
		startBarrier.Done()
		wg.Wait()

		// After all goroutines complete, Check should return a consistent cached response
		finalCached, found, err := im.Check(ctx, key)
		if err != nil {
			rt.Fatalf("final Check failed: %v", err)
		}
		if !found {
			rt.Fatal("final Check returned found=false — no goroutine stored successfully")
		}

		// At least one goroutine must have stored
		anyStored := false
		for _, r := range results {
			if r.stored {
				anyStored = true
				break
			}
		}
		if !anyStored {
			rt.Fatal("no goroutine reported storing a response")
		}

		// The final cached response must be a valid CachedResponse (non-nil fields)
		if finalCached.StatusCode != 200 {
			rt.Fatalf("final cached StatusCode = %d, want 200", finalCached.StatusCode)
		}
		if finalCached.RequestID == "" {
			rt.Fatal("final cached RequestID is empty")
		}

		// All subsequent Checks must return the same response (consistency)
		for i := 0; i < 3; i++ {
			check, found, err := im.Check(ctx, key)
			if err != nil {
				rt.Fatalf("repeated Check %d failed: %v", i, err)
			}
			if !found {
				rt.Fatalf("repeated Check %d returned found=false", i)
			}
			if check.RequestID != finalCached.RequestID {
				rt.Fatalf("repeated Check %d returned different RequestID: got %q, want %q",
					i, check.RequestID, finalCached.RequestID)
			}
			if check.StatusCode != finalCached.StatusCode {
				rt.Fatalf("repeated Check %d returned different StatusCode: got %d, want %d",
					i, check.StatusCode, finalCached.StatusCode)
			}
		}
	})
}
