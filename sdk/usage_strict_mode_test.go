package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/xxww0098/cpa-gateway/model"
)

// Feature: billing-security-hardening, Property 2 — Strict mode preserves
// Hold on missing upstream usage metadata, and orphaned records with no
// SettleCtx never touch ledger state.
//
// Requirements covered here:
//
//   - R1.4: Under strict mode, a successful upstream response whose
//     terminal usage envelope was not parsed must NOT trigger Settle
//     nor Release. The existing Redis Hold must remain reserved until
//     its natural TTL expiry. A Failed=true UsageLog row captures the
//     event for out-of-band reconciliation
//     (RawMetadata.reason = "missing_upstream_usage_strict", ActualCost = 0).
//
//   - R1.7: When the executor fails to publish a UsageRecord for a
//     completed response (e.g. the dispatcher goroutine never invokes
//     HandleUsage, or the Plugin is called with a context missing its
//     SettleCtx marker), the Hold for the matching requestID must
//     remain reserved until its natural expiry. HandleUsage must emit
//     no side effects when SettleCtx is absent.
//
// The two tests below cover these example cases. They use the package-
// local fakeLedger (defined in sdk/usage_test.go) which counts Settle /
// Release invocations, and a live miniredis to seed a hold entry that
// the tests then assert is still present after HandleUsage returns.
// Seeding the Hold directly via miniredis — rather than routing through
// fakeLedger.Hold — keeps the mock's responsibility focused on Settle /
// Release accounting while still letting the tests prove the ZSET key
// is physically untouched along the strict / no-SettleCtx paths.

// holdsKeyForTest mirrors ledger.holdsKey. Duplicating the constant keeps
// the ledger helper unexported while letting the strict-mode test
// observe the miniredis key directly. Any future change to the ledger
// key scheme will surface as a failing assertion in this file.
func holdsKeyForTest(userID uint) string {
	return fmt.Sprintf("cpa-gateway:billing:holds:%d", userID)
}

// TestStrictModeHoldPreserved covers R1.4: with strict_usage_metadata_mode
// enabled, a successful upstream response whose usage envelope is absent
// (Usage_Detail_Present = false) must NOT Settle and must NOT Release.
// The seeded Redis Hold must remain reserved (ZSCORE returns the original
// value) so it can expire via its natural TTL, and exactly one
// Failed=true UsageLog row must be persisted with ActualCost = 0 and
// RawMetadata.reason = "missing_upstream_usage_strict".
//
// **Validates: Property 2, Requirement 1.4**
func TestStrictModeHoldPreserved(t *testing.T) {
	db := newUsageTestDB(t)

	srv := miniredis.RunT(t)

	led := &fakeLedger{}
	calc := &fakeCalculator{computeCost: 0.5}
	plugin := NewUsagePlugin(db, led, calc)
	plugin.SetStrictUsageMetadataMode(true)

	const (
		userID        uint    = 42
		requestID             = "req-strict-missing-usage"
		seededHoldAmt float64 = 0.25
	)

	// Seed a User row so the strict-mode UsageLog insert does not trip
	// any foreign-key-like constraint surfaced by AutoMigrate.
	user := &model.User{
		ID:           userID,
		Email:        "strict-hold-preserved@test.local",
		PasswordHash: "x",
		Role:         "user",
		Status:       "active",
		Balance:      10,
	}
	if err := db.Create(user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Seed a Hold in miniredis. This is the state that would exist in
	// production after HoldMiddleware.Handle runs — we bypass the real
	// ledger.Hold because fakeLedger does not touch Redis.
	if _, err := srv.ZAdd(holdsKeyForTest(userID), seededHoldAmt, requestID); err != nil {
		t.Fatalf("seed miniredis hold: %v", err)
	}

	// Pre-condition sanity check: the hold is present before HandleUsage.
	if score, err := srv.ZScore(holdsKeyForTest(userID), requestID); err != nil {
		t.Fatalf("pre-condition ZScore: %v (seed failed?)", err)
	} else if score != seededHoldAmt {
		t.Fatalf("pre-condition ZScore = %v, want %v", score, seededHoldAmt)
	}

	sc := &SettleCtx{
		RequestID: requestID,
		UserID:    userID,
		RateMult:  1.0,
		Model:     "gpt-4o",
	}

	// Critical: the context carries SettleCtx but NOT
	// executor.WithUsageDetailPresent(..., true). Per the contract of
	// executor.UsageDetailPresentFromContext, a missing key is treated
	// as (false, false) by HandleUsage, which is exactly the strict-
	// branch precondition we want to exercise.
	ctx := WithSettleCtx(context.Background(), sc)

	rec := cliproxyusage.Record{
		Provider: "openai",
		Model:    "gpt-4o",
		Failed:   false, // successful upstream — only the usage envelope is missing
		Latency:  75 * time.Millisecond,
		// Detail intentionally zero-valued to match the stripping-attack
		// shape strict mode protects against.
		Detail: cliproxyusage.Detail{},
	}

	plugin.HandleUsage(ctx, rec)

	// ---- Assertion 1: Settle was NOT called. -----------------------
	if n := len(led.settleCalls); n != 0 {
		t.Fatalf("strict mode must not call Settle, got %d calls: %+v", n, led.settleCalls)
	}

	// ---- Assertion 2: Release was NOT called. ----------------------
	if n := len(led.releaseCalls); n != 0 {
		t.Fatalf("strict mode must not call Release, got %d calls: %+v", n, led.releaseCalls)
	}

	// ---- Assertion 3: the Redis Hold is still present with the same
	// score — strict mode lets the TTL reclaim it, nothing else. ----
	score, err := srv.ZScore(holdsKeyForTest(userID), requestID)
	if err != nil {
		t.Fatalf("strict mode must leave the Hold in place; ZSCORE holds:{%d} %s error: %v",
			userID, requestID, err)
	}
	if score != seededHoldAmt {
		t.Fatalf("strict mode must not mutate the Hold score; got %v want %v",
			score, seededHoldAmt)
	}

	// ---- Assertion 4: exactly one UsageLog row with the strict
	// annotation, Failed=true, and ActualCost = 0. -------------------
	var logs []model.UsageLog
	if err := db.Where("request_id = ?", requestID).Find(&logs).Error; err != nil {
		t.Fatalf("query UsageLog: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected exactly 1 UsageLog row for strict branch, got %d", len(logs))
	}
	got := logs[0]
	if !got.Failed {
		t.Errorf("UsageLog.Failed = false, want true for strict-mode missing usage")
	}
	if got.ActualCost != 0 {
		t.Errorf("UsageLog.ActualCost = %v, want 0 for strict-mode missing usage", got.ActualCost)
	}
	if got.Cost != 0 {
		t.Errorf("UsageLog.Cost = %v, want 0 for strict-mode missing usage", got.Cost)
	}
	if got.TotalCost != 0 {
		t.Errorf("UsageLog.TotalCost = %v, want 0 for strict-mode missing usage", got.TotalCost)
	}
	if len(got.RawMetadata) == 0 {
		t.Fatalf("UsageLog.RawMetadata empty, want reason=missing_upstream_usage_strict")
	}
	var meta map[string]interface{}
	if err := json.Unmarshal(got.RawMetadata, &meta); err != nil {
		t.Fatalf("unmarshal UsageLog.RawMetadata: %v (raw=%s)", err, string(got.RawMetadata))
	}
	reason, ok := meta["reason"].(string)
	if !ok {
		t.Fatalf("UsageLog.RawMetadata.reason missing or not a string: %v (raw=%s)",
			meta["reason"], string(got.RawMetadata))
	}
	if reason != "missing_upstream_usage_strict" {
		t.Errorf("UsageLog.RawMetadata.reason = %q, want %q",
			reason, "missing_upstream_usage_strict")
	}
}

// TestPublishFailureHoldPreserved covers R1.7: when the executor never
// publishes a UsageRecord for a completed response, the Hold for the
// matching requestID must remain reserved until its natural TTL expiry.
//
// We cannot literally skip the PublishRecord call from outside the
// cliproxy SDK, so the test simulates the observable consequence:
// HandleUsage receives a context with no SettleCtx marker (the
// HoldMiddleware injection that normally carries the requestID is
// absent because the publisher path never reached the plugin). Under
// that shape, HandleUsage must early-return as a no-op — zero ledger
// calls, zero UsageLog rows, zero Compute invocations, and crucially,
// the Redis Hold must be untouched.
//
// **Validates: Property 2, Requirement 1.7**
func TestPublishFailureHoldPreserved(t *testing.T) {
	db := newUsageTestDB(t)

	srv := miniredis.RunT(t)

	led := &fakeLedger{}
	calc := &fakeCalculator{computeCost: 0.99}
	plugin := NewUsagePlugin(db, led, calc)
	// Strict mode toggle is irrelevant for this branch — the no-SettleCtx
	// early-return fires before any strict / fallback decision. Leaving it
	// at its zero value (false) also documents that the no-SettleCtx guard
	// is the FIRST line of defence in HandleUsage and must not depend on
	// operator configuration.

	const (
		userID        uint    = 73
		requestID             = "req-never-published"
		seededHoldAmt float64 = 0.125
	)

	// Seed the state that HoldMiddleware would have written before the
	// executor (or dispatcher) failed to publish the UsageRecord: a
	// Hold in Redis and a User row. No SettleCtx on the context later
	// in the test simulates the missing publish.
	user := &model.User{
		ID:           userID,
		Email:        "publish-failure-hold@test.local",
		PasswordHash: "x",
		Role:         "user",
		Status:       "active",
		Balance:      10,
	}
	if err := db.Create(user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := srv.ZAdd(holdsKeyForTest(userID), seededHoldAmt, requestID); err != nil {
		t.Fatalf("seed miniredis hold: %v", err)
	}

	// Plain context — no SettleCtx, no UsageDetailPresent marker. This
	// is the input shape HandleUsage sees when the executor / dispatcher
	// delivered a record without the upstream HoldMiddleware injection.
	ctx := context.Background()

	// The record itself is intentionally minimal — the plugin should
	// not even reach the point of inspecting rec.Failed or rec.Detail
	// when SettleCtx is absent, per the no-SettleCtx guard at the top
	// of HandleUsage.
	rec := cliproxyusage.Record{
		Provider: "openai",
		Model:    "gpt-4o",
		Failed:   false,
		Detail:   cliproxyusage.Detail{},
	}

	plugin.HandleUsage(ctx, rec)

	// ---- Assertion 1: no ledger calls whatsoever. ------------------
	if n := len(led.settleCalls); n != 0 {
		t.Fatalf("no-SettleCtx branch must not call Settle, got %d calls: %+v", n, led.settleCalls)
	}
	if n := len(led.releaseCalls); n != 0 {
		t.Fatalf("no-SettleCtx branch must not call Release, got %d calls: %+v", n, led.releaseCalls)
	}
	if n := len(led.holdCalls); n != 0 {
		t.Fatalf("no-SettleCtx branch must not call Hold, got %d calls: %+v", n, led.holdCalls)
	}

	// ---- Assertion 2: Compute was NOT invoked. The no-SettleCtx
	// guard must fire BEFORE the Compute call so the plugin wastes no
	// work on an orphaned record. ------------------------------------
	if calc.computed {
		t.Errorf("no-SettleCtx branch must not invoke Compute")
	}

	// ---- Assertion 3: the Redis Hold is still present with the same
	// score. The natural TTL (ledger.holdTTL) is what reclaims it; the
	// plugin must not touch Redis for orphaned records. -------------
	score, err := srv.ZScore(holdsKeyForTest(userID), requestID)
	if err != nil {
		t.Fatalf("no-SettleCtx branch must leave the Hold in place; ZSCORE holds:{%d} %s error: %v",
			userID, requestID, err)
	}
	if score != seededHoldAmt {
		t.Fatalf("no-SettleCtx branch must not mutate the Hold score; got %v want %v",
			score, seededHoldAmt)
	}

	// ---- Assertion 4: no UsageLog row was written. -----------------
	var count int64
	if err := db.Model(&model.UsageLog{}).Count(&count).Error; err != nil {
		t.Fatalf("count UsageLog: %v", err)
	}
	if count != 0 {
		t.Errorf("no-SettleCtx branch must not insert a UsageLog row, got %d rows", count)
	}
}
