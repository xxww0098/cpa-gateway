package sdk

import (
	"context"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/xxww0098/cpa-gateway/authutil"
	"github.com/xxww0098/cpa-gateway/infra"
	"github.com/xxww0098/cpa-gateway/model"
	"github.com/xxww0098/cpa-gateway/testutil"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// -----------------------------------------------------------------------------
// test helpers
// -----------------------------------------------------------------------------

const testJWTSecret = "test-secret-for-access-provider"

// newTestDB builds a fresh in-memory SQLite GORM database with the
// minimum schema used by the AccessProvider tests. Each test gets its
// own isolated database via a unique DSN name so state does not leak.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:access_test_" + t.Name() + "_?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// Ensure a unique in-memory DB per test by forcing a new connection pool.
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("raw db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	if err := db.AutoMigrate(
		&model.User{},
		&model.ApiKey{},
		&model.Group{},
		&model.Subscription{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// newTestAccessProvider wires up a fresh in-memory DB + miniredis-backed
// Redis + empty APIKeyCache and returns the constructed provider along
// with the handles for further fixture mutation.
func newTestAccessProvider(t *testing.T) (*AccessProvider, *gorm.DB) {
	t.Helper()
	db := newTestDB(t)
	rdb, _ := testutil.MustMiniRedis(t)
	cache := infra.NewAPIKeyCache()
	provider := NewAccessProvider(db, rdb, cache, testJWTSecret)
	return provider, db
}

// seedUserWithAPIKey inserts a user + an api key row and returns the
// plaintext key to present in Authorization headers.
func seedUserWithAPIKey(t *testing.T, db *gorm.DB, email string, groupID *uint, rateMult float64) (uint, string) {
	t.Helper()
	user := &model.User{Email: email, PasswordHash: "x", Role: "user", Status: "active", Balance: 100}
	if err := db.Create(user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	if groupID != nil {
		grp := &model.Group{Name: "test-group-" + email, RateMultiplier: rateMult}
		if err := db.Create(grp).Error; err != nil {
			t.Fatalf("create group: %v", err)
		}
		gid := grp.ID
		groupID = &gid
	}

	plaintext, err := authutil.NewAPIKey()
	if err != nil {
		t.Fatalf("new api key: %v", err)
	}
	ak := &model.ApiKey{
		UserID:    user.ID,
		KeyHash:   authutil.HashAPIKey(plaintext),
		KeyPrefix: authutil.APIKeyPrefix(plaintext),
		Name:      "default",
		Status:    "active",
		GroupID:   groupID,
	}
	if err := db.Create(ak).Error; err != nil {
		t.Fatalf("create api key: %v", err)
	}
	return user.ID, plaintext
}

// -----------------------------------------------------------------------------
// tests
// -----------------------------------------------------------------------------

// TestAuthenticate_APIKey verifies that a valid "cpa-" prefixed Bearer
// token resolves to a Result whose Principal is the decimal user ID.
func TestAuthenticate_APIKey(t *testing.T) {
	provider, db := newTestAccessProvider(t)
	userID, plaintext := seedUserWithAPIKey(t, db, "apikey@example.com", nil, 1.0)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)

	result, authErr := provider.Authenticate(context.Background(), req)
	if authErr != nil {
		t.Fatalf("unexpected auth error: %+v", authErr)
	}
	if result == nil {
		t.Fatalf("expected non-nil result")
	}
	if result.Provider != accessProviderName {
		t.Errorf("Provider = %q, want %q", result.Provider, accessProviderName)
	}
	if got, want := result.Principal, strconv.FormatUint(uint64(userID), 10); got != want {
		t.Errorf("Principal = %q, want %q", got, want)
	}
	if result.Metadata["user_id"] == "" {
		t.Errorf("Metadata[user_id] missing; metadata=%+v", result.Metadata)
	}
	if result.Metadata["api_key_id"] == "" {
		t.Errorf("Metadata[api_key_id] missing; metadata=%+v", result.Metadata)
	}
	if result.Metadata["rate_mult"] == "" {
		t.Errorf("Metadata[rate_mult] missing; metadata=%+v", result.Metadata)
	}
}

// TestAuthenticate_JWT verifies that a non "cpa-" Bearer token is parsed
// as a JWT and resolves to a matching Result.
func TestAuthenticate_JWT(t *testing.T) {
	provider, db := newTestAccessProvider(t)
	// Seed a user matching the JWT claims.
	user := &model.User{Email: "jwt@example.com", PasswordHash: "x", Role: "user", Status: "active", Balance: 50}
	if err := db.Create(user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	token, err := authutil.GenerateJWT(user.ID, user.Email, testJWTSecret, 0)
	if err != nil {
		t.Fatalf("generate jwt: %v", err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	result, authErr := provider.Authenticate(context.Background(), req)
	if authErr != nil {
		t.Fatalf("unexpected auth error: %+v", authErr)
	}
	if result == nil {
		t.Fatalf("expected non-nil result")
	}
	if got, want := result.Principal, strconv.FormatUint(uint64(user.ID), 10); got != want {
		t.Errorf("Principal = %q, want %q", got, want)
	}
	if result.Metadata["user_id"] != want(user.ID) {
		t.Errorf("Metadata[user_id] = %q, want %q", result.Metadata["user_id"], want(user.ID))
	}
}

func want(u uint) string { return strconv.FormatUint(uint64(u), 10) }

// TestAuthenticate_MissingToken verifies that a request with no
// Authorization header yields AuthErrorCodeNoCredentials.
func TestAuthenticate_MissingToken(t *testing.T) {
	provider, _ := newTestAccessProvider(t)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)

	result, authErr := provider.Authenticate(context.Background(), req)
	if result != nil {
		t.Fatalf("expected nil result, got %+v", result)
	}
	if authErr == nil {
		t.Fatalf("expected AuthError, got nil")
	}
	if authErr.Code != sdkaccess.AuthErrorCodeNoCredentials {
		t.Errorf("Code = %q, want %q", authErr.Code, sdkaccess.AuthErrorCodeNoCredentials)
	}
}

// TestAuthenticate_InvalidKey verifies that a "cpa-" prefixed token not
// matching any row yields AuthErrorCodeInvalidCredential.
func TestAuthenticate_InvalidKey(t *testing.T) {
	provider, _ := newTestAccessProvider(t)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer cpa-deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")

	result, authErr := provider.Authenticate(context.Background(), req)
	if result != nil {
		t.Fatalf("expected nil result, got %+v", result)
	}
	if authErr == nil {
		t.Fatalf("expected AuthError, got nil")
	}
	if authErr.Code != sdkaccess.AuthErrorCodeInvalidCredential {
		t.Errorf("Code = %q, want %q", authErr.Code, sdkaccess.AuthErrorCodeInvalidCredential)
	}
}

// TestAuthenticate_SubscriptionLoaded verifies that when the user has
// an active subscription, the Result metadata carries subscription_id
// and the limit/used fields.
func TestAuthenticate_SubscriptionLoaded(t *testing.T) {
	provider, db := newTestAccessProvider(t)
	groupMarker := uint(1) // sentinel; real group id assigned inside helper
	userID, plaintext := seedUserWithAPIKey(t, db, "subbed@example.com", &groupMarker, 1.5)

	daily := 10.0
	weekly := 50.0
	monthly := 200.0
	now := time.Now().UTC()
	sub := &model.Subscription{
		UserID:          userID,
		PackageID:       1,
		GroupID:         1,
		GroupName:       "test-group",
		Status:          "active",
		StartsAt:        now.Add(-24 * time.Hour),
		ExpiresAt:       now.Add(30 * 24 * time.Hour),
		DailyUsageUSD:   1.25,
		DailyResetAt:    now.Add(24 * time.Hour),
		WeeklyUsageUSD:  3.75,
		WeeklyResetAt:   now.Add(7 * 24 * time.Hour),
		MonthlyUsageUSD: 9.5,
		MonthlyResetAt:  now.Add(30 * 24 * time.Hour),
		DailyLimitUSD:   &daily,
		WeeklyLimitUSD:  &weekly,
		MonthlyLimitUSD: &monthly,
	}
	if err := db.Create(sub).Error; err != nil {
		t.Fatalf("create subscription: %v", err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)

	result, authErr := provider.Authenticate(context.Background(), req)
	if authErr != nil {
		t.Fatalf("unexpected auth error: %+v", authErr)
	}
	if result == nil {
		t.Fatalf("expected non-nil result")
	}

	if got := result.Metadata["subscription_id"]; got != strconv.FormatUint(uint64(sub.ID), 10) {
		t.Errorf("Metadata[subscription_id] = %q, want %q", got, strconv.FormatUint(uint64(sub.ID), 10))
	}
	if got := result.Metadata["daily_limit"]; got == "" {
		t.Errorf("Metadata[daily_limit] missing; got metadata=%+v", result.Metadata)
	}
	if got := result.Metadata["daily_used"]; got == "" {
		t.Errorf("Metadata[daily_used] missing; got metadata=%+v", result.Metadata)
	}
	if got := result.Metadata["weekly_limit"]; got == "" {
		t.Errorf("Metadata[weekly_limit] missing; got metadata=%+v", result.Metadata)
	}
	if got := result.Metadata["monthly_limit"]; got == "" {
		t.Errorf("Metadata[monthly_limit] missing; got metadata=%+v", result.Metadata)
	}
}
