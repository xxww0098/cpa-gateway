package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/xxww0098/cpa-gateway/ledger"
	"github.com/xxww0098/cpa-gateway/model"
	"github.com/xxww0098/cpa-gateway/pricing"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"pgregory.net/rapid"
)

// raiseRapidChecksPreflight raises the -rapid.checks flag to the requested
// minimum for the duration of the current test and restores the original on
// cleanup. Mirrors pricing.raiseRapidChecks so we hit the ≥ 200 iteration
// budget called out by task 6.5 regardless of the caller's default.
func raiseRapidChecksPreflight(t *testing.T, minChecks int) {
	t.Helper()
	fl := flag.Lookup("rapid.checks")
	if fl == nil {
		return
	}
	orig := fl.Value.String()
	cur, err := strconv.Atoi(orig)
	if err != nil || cur >= minChecks {
		return
	}
	if setErr := flag.Set("rapid.checks", strconv.Itoa(minChecks)); setErr != nil {
		t.Fatalf("flag.Set rapid.checks: %v", setErr)
	}
	t.Cleanup(func() { _ = flag.Set("rapid.checks", orig) })
}

// preflightEnv bundles the real infrastructure the HoldMiddleware preflight
// test exercises end-to-end: miniredis (so the Hold Lua script runs and so
// GetBalance + ZSCORE observations are real), sqlite (so GetBalance can fall
// back to the DB balance), and the concrete *ledger.Ledger that satisfies the
// full BillingLedger interface including HasUnresolvedShortfall and
// ActiveHoldAmount.
type preflightEnv struct {
	db     *gorm.DB
	srv    *miniredis.Miniredis
	client *redis.Client
	ldg    *ledger.Ledger
}

// newPreflightEnv spins up a fresh (miniredis, sqlite, ledger) triple seeded
// with a single user whose persistent balance is the supplied balance. The
// balance cache is primed so Hold can resolve without an extra DB round-trip
// — Hold's own cache-miss fallback would also work, but priming keeps the
// test focused on the preflight branch we care about rather than the cache
// warm-up path.
func newPreflightEnv(t testing.TB, userID uint, balance float64) *preflightEnv {
	t.Helper()

	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("raw db: %v", err)
	}
	// A single connection keeps the in-memory database visible across
	// goroutines that share this *gorm.DB (ledger transactions run on a
	// different connection than the request-handling goroutine otherwise).
	sqlDB.SetMaxOpenConns(1)

	if err := db.AutoMigrate(&model.User{}, &model.BalanceLog{}, &model.UsageLog{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}

	u := &model.User{
		ID:           userID,
		Email:        fmt.Sprintf("preflight-%d@test.local", userID),
		PasswordHash: "hash",
		Role:         "user",
		Status:       "active",
		Balance:      balance,
	}
	if err := db.Create(u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	ldg := ledger.NewWithConfig(db, client, 30*time.Second, 5*time.Minute)

	// Prime the Redis balance cache so GetBalance returns immediately.
	// The Lua script requires the cache entry to exist; GetBalance falls
	// back to a DB read + cache prime on its own, but priming here keeps
	// the test free of that extra round-trip.
	if err := client.Set(
		context.Background(),
		fmt.Sprintf("cpa-gateway:billing:balance:%d", userID),
		strconv.FormatFloat(balance, 'f', -1, 64),
		30*time.Second,
	).Err(); err != nil {
		t.Fatalf("prime balance cache: %v", err)
	}

	return &preflightEnv{db: db, srv: srv, client: client, ldg: ldg}
}

// preflightCalcStub is a PricingCalculator whose Estimate and
// EstimateWithMaxTokens return the exact values the test prescribes per
// iteration. Using a stub (rather than a live pricing.Calculator) lets the
// property enumerate {holdEstimate, estimateStream, estimateWithMax}
// independently and assert the middleware's `max(...)` combination directly,
// without having to reverse-engineer the calculator's per-1M formula.
type preflightCalcStub struct {
	estimate             float64
	estimateWithMaxValue float64
}

func (s *preflightCalcStub) Estimate(_ string, _ bool, _ float64) float64 {
	return s.estimate
}

func (s *preflightCalcStub) EstimateWithMaxTokens(_ string, _ int64, _ bool, _ float64) float64 {
	return s.estimateWithMaxValue
}

// Compute is irrelevant to the preflight path (that runs before the handler
// and never calls Compute). Return 0 so any accidental consumer is benign.
func (s *preflightCalcStub) Compute(_ string, _ pricing.UsageTokens, _ float64) float64 {
	return 0
}

// runPreflightHandler executes one HoldMiddleware request against a
// preflightEnv + preflightCalcStub combination and returns the recorder
// plus a boolean indicating whether the downstream handler was reached.
// The request body intentionally includes stream=true and an honoured
// max_tokens field so parseRequestModelStream resolves them on the real
// middleware path.
func runPreflightHandler(
	t testing.TB,
	env *preflightEnv,
	calc *preflightCalcStub,
	userID uint,
	maxTokens int64,
) (*httptest.ResponseRecorder, bool, string) {
	t.Helper()

	mw := NewHoldMiddleware(env.ldg, calc, env.db, 5*time.Minute)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("accessProvider", accessProviderName)
		c.Set("accessMetadata", map[string]string{
			"user_id":   strconv.FormatUint(uint64(userID), 10),
			"rate_mult": "1.0",
		})
		c.Next()
	})
	r.Use(mw.Handle)

	handlerReached := false
	var reqIDObserved string
	r.Any("/*path", func(c *gin.Context) {
		handlerReached = true
		if sc, ok := SettleCtxFromContext(c.Request.Context()); ok {
			reqIDObserved = sc.RequestID
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	body := fmt.Sprintf(`{"model":"gpt-4o","stream":true,"max_tokens":%d}`, maxTokens)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")

	// Pin the trace ID so the test can query ZSCORE without having to
	// capture the UUID the middleware generates on its own.
	reqID := fmt.Sprintf("preflight-req-%d-%d", userID, time.Now().UnixNano())
	req.Header.Set("X-Trace-ID", reqID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if reqIDObserved == "" {
		reqIDObserved = reqID
	}
	return w, handlerReached, reqIDObserved
}

// holdsZSetKeyFor mirrors ledger.holdsKey — duplicating the constant here
// keeps the ledger package's helpers unexported while letting the test
// observe the miniredis key directly. Any change to the ledger key scheme
// would surface as a test failure.
func holdsZSetKeyFor(userID uint) string {
	return fmt.Sprintf("cpa-gateway:billing:holds:%d", userID)
}

// TestPreflightInsufficientBalance property-tests the HoldMiddleware
// upper-bound preflight documented in task 6.4:
//
//	upper_bound = max( holdEstimate, EstimateWithMaxTokens, Estimate(stream=true) )
//	if upper_bound > balance → HTTP 402 insufficient_balance, no Redis hold
//	else                     → 2xx, Redis hold created via ledger.Hold
//
// Because preflightCalcStub returns the same value for Estimate regardless
// of the `stream` argument, the property effectively enumerates two
// independent knobs — Estimate (used both for the reserved holdAmount and
// for the streaming estimate component) and EstimateWithMaxTokens — plus
// the user balance. That is sufficient to cover every ordering of the
// max(...) expression in the middleware.
//
// Shrinking targets a minimal (balance, estimate, estimateWithMax, maxTokens)
// quadruple because rapid minimises toward the lower end of each generator.
//
// **Validates: Property 5, Requirement 2.1**
func TestPreflightInsufficientBalance(t *testing.T) {
	raiseRapidChecksPreflight(t, 200)

	rapid.Check(t, func(rt *rapid.T) {
		// All monetary values stay non-negative — Requirement 6.4 disallows
		// negative per-1M prices, which means Estimate / EstimateWithMaxTokens
		// can only produce non-negative results in production.
		balance := rapid.Float64Range(0, 100).Draw(rt, "balance")
		estimate := rapid.Float64Range(0.0001, 50).Draw(rt, "estimate")
		estimateWithMax := rapid.Float64Range(0, 50).Draw(rt, "estimateWithMax")
		maxTokens := rapid.Int64Range(0, 1_000_000).Draw(rt, "maxTokens")

		// User IDs kept small but strictly positive — GORM rejects zero IDs
		// on primary-key insert, which would make the test env setup fail
		// for reasons unrelated to the property under test.
		userID := uint(rapid.IntRange(1, 1_000_000).Draw(rt, "userID"))

		env := newPreflightEnv(t, userID, balance)
		calc := &preflightCalcStub{
			estimate:             estimate,
			estimateWithMaxValue: estimateWithMax,
		}

		// Compute the same upper_bound the middleware computes. Because
		// preflightCalcStub returns `estimate` for both `Estimate(model, streaming, ...)`
		// and `Estimate(model, true, ...)`, the holdEstimate and estStream
		// components collapse to the same value.
		upperBound := math.Max(estimate, math.Max(estimateWithMax, estimate))

		w, handlerReached, reqID := runPreflightHandler(t, env, calc, userID, maxTokens)

		if upperBound > balance {
			// --- Preflight rejection branch ---
			if w.Code != http.StatusPaymentRequired {
				rt.Fatalf(
					"preflight rejection expected 402, got %d; body=%s "+
						"(balance=%g estimate=%g estimateWithMax=%g upper=%g maxTokens=%d)",
					w.Code, w.Body.String(),
					balance, estimate, estimateWithMax, upperBound, maxTokens,
				)
			}
			// The response body must carry the insufficient_balance error
			// code per Requirement 2.1 and mirror abortInsufficientBalance's
			// structured shape.
			var resp map[string]interface{}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				rt.Fatalf("parse preflight 402 body: %v; body=%s", err, w.Body.String())
			}
			if resp["error"] != "insufficient_balance" {
				rt.Fatalf(
					"preflight 402 error = %v, want insufficient_balance; body=%s",
					resp["error"], w.Body.String(),
				)
			}
			if handlerReached {
				rt.Fatalf("downstream handler must NOT run on preflight rejection")
			}
			// No Redis hold must exist for this reqID. ZScore on a missing
			// member returns redis.Nil; on a missing key it also returns
			// redis.Nil — miniredis implements the same semantics, so a
			// non-nil error is the signal for "member not present".
			if _, err := env.srv.ZScore(holdsZSetKeyFor(userID), reqID); err == nil {
				rt.Fatalf(
					"preflight rejection must not create a Redis hold, but ZSCORE holds:{%d} %s returned no error",
					userID, reqID,
				)
			}
			return
		}

		// --- Admission branch (upper_bound <= balance) ---
		if w.Code != http.StatusOK {
			rt.Fatalf(
				"admission expected 2xx, got %d; body=%s "+
					"(balance=%g estimate=%g estimateWithMax=%g upper=%g maxTokens=%d)",
				w.Code, w.Body.String(),
				balance, estimate, estimateWithMax, upperBound, maxTokens,
			)
		}
		if !handlerReached {
			rt.Fatalf("admission must reach downstream handler (body=%s)", w.Body.String())
		}
		// The hold amount is `estimate` (the middleware reserves
		// holdAmount = estimatedCost; the upper bound is only used for
		// the rejection gate, see holdmw.go comments on task 6.4).
		score, err := env.srv.ZScore(holdsZSetKeyFor(userID), reqID)
		if err != nil {
			rt.Fatalf(
				"admission must create Redis hold but ZSCORE holds:{%d} %s returned err=%v",
				userID, reqID, err,
			)
		}
		if math.Abs(score-estimate) > 1e-9 {
			rt.Fatalf(
				"Redis hold score = %g, want %g (reserved holdAmount)",
				score, estimate,
			)
		}
	})
}
