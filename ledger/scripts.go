package ledger

import (
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Redis key patterns for the billing ledger.
const (
	balanceKeyPrefix = "cpa-gateway:billing:balance:"
	holdsKeyPrefix   = "cpa-gateway:billing:holds:"
	holdsTSKeyPrefix = "cpa-gateway:billing:holds:ts:"
)

// balanceKey returns the Redis key for a user's cached persistent balance.
// Type: String
func balanceKey(userID uint) string {
	return fmt.Sprintf("%s%d", balanceKeyPrefix, userID)
}

// holdsKey returns the Redis key for a user's active holds sorted set.
// Type: Sorted Set (member=requestID, score=holdAmount)
func holdsKey(userID uint) string {
	return fmt.Sprintf("%s%d", holdsKeyPrefix, userID)
}

// holdsTSKey returns the Redis key for a user's hold creation timestamps hash.
// Type: Hash (field=requestID, value=unix_timestamp)
func holdsTSKey(userID uint) string {
	return fmt.Sprintf("%s%d", holdsTSKeyPrefix, userID)
}

// holdScript is an atomic Redis Lua script that performs:
//  1. Reads cached balance from KEYS[1] (returns error if cache miss)
//  2. Cleans expired holds from KEYS[2] and KEYS[3] using ARGV[3] - ARGV[4] as cutoff
//  3. Sums all scores in KEYS[2] (existing active holds)
//  4. Checks if balance - sum_holds >= new_amount
//  5. If yes: ZADD to KEYS[2] and HSET timestamp to KEYS[3], returns "OK"
//  6. If no: returns error string with available balance info
//
// KEYS[1] = cpa-gateway:billing:balance:{userID}   (cached balance string)
// KEYS[2] = cpa-gateway:billing:holds:{userID}     (sorted set)
// KEYS[3] = cpa-gateway:billing:holds:ts:{userID}  (hold timestamps hash)
// ARGV[1] = hold amount (float as string)
// ARGV[2] = request ID
// ARGV[3] = current timestamp (unix seconds)
// ARGV[4] = hold TTL seconds
// ARGV[5] = idempotency key (empty string if none)
//
// Returns: "OK" on success, error string on failure.
var holdScript = redis.NewScript(`
-- Read cached balance
local balance_str = redis.call('GET', KEYS[1])
if not balance_str then
    return redis.error_reply('CACHE_MISS')
end
local balance = tonumber(balance_str)
if not balance then
    return redis.error_reply('INVALID_BALANCE')
end

local amount = tonumber(ARGV[1])
local request_id = ARGV[2]
local now = tonumber(ARGV[3])
local ttl_seconds = tonumber(ARGV[4])

-- Clean expired holds: remove entries whose timestamp is older than (now - ttl_seconds)
local cutoff = now - ttl_seconds
local ts_entries = redis.call('HGETALL', KEYS[3])
for i = 1, #ts_entries, 2 do
    local member = ts_entries[i]
    local ts = tonumber(ts_entries[i + 1])
    if ts and ts < cutoff then
        redis.call('ZREM', KEYS[2], member)
        redis.call('HDEL', KEYS[3], member)
    end
end

-- Check if this request ID already exists (idempotent hold)
local existing = redis.call('ZSCORE', KEYS[2], request_id)
if existing then
    return 'OK'
end

-- Sum all active hold scores
local holds = redis.call('ZRANGE', KEYS[2], 0, -1, 'WITHSCORES')
local sum_holds = 0
for i = 2, #holds, 2 do
    sum_holds = sum_holds + tonumber(holds[i])
end

-- Check available balance
local available = balance - sum_holds
if available < amount then
    return redis.error_reply('INSUFFICIENT_BALANCE:' .. tostring(available))
end

-- Add the new hold
redis.call('ZADD', KEYS[2], amount, request_id)
redis.call('HSET', KEYS[3], request_id, tostring(now))

return 'OK'
`)

// getBalanceScript is an atomic Redis Lua script that:
//  1. Cleans expired holds from KEYS[2] and KEYS[3]
//  2. Returns cached_balance - sum(active holds) as a string
//
// KEYS[1] = cpa-gateway:billing:balance:{userID}   (cached balance string)
// KEYS[2] = cpa-gateway:billing:holds:{userID}     (sorted set)
// KEYS[3] = cpa-gateway:billing:holds:ts:{userID}  (hold timestamps hash)
// ARGV[1] = current timestamp (unix seconds)
// ARGV[2] = hold TTL seconds
//
// Returns: available balance as string, or error on cache miss.
var getBalanceScript = redis.NewScript(`
-- Read cached balance
local balance_str = redis.call('GET', KEYS[1])
if not balance_str then
    return redis.error_reply('CACHE_MISS')
end
local balance = tonumber(balance_str)
if not balance then
    return redis.error_reply('INVALID_BALANCE')
end

local now = tonumber(ARGV[1])
local ttl_seconds = tonumber(ARGV[2])

-- Clean expired holds
local cutoff = now - ttl_seconds
local ts_entries = redis.call('HGETALL', KEYS[3])
for i = 1, #ts_entries, 2 do
    local member = ts_entries[i]
    local ts = tonumber(ts_entries[i + 1])
    if ts and ts < cutoff then
        redis.call('ZREM', KEYS[2], member)
        redis.call('HDEL', KEYS[3], member)
    end
end

-- Sum all active hold scores
local holds = redis.call('ZRANGE', KEYS[2], 0, -1, 'WITHSCORES')
local sum_holds = 0
for i = 2, #holds, 2 do
    sum_holds = sum_holds + tonumber(holds[i])
end

-- Return available balance
local available = balance - sum_holds
return tostring(available)
`)
