package infra

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// InitRedis connects to Redis using the provided settings. It pings the
// server to verify connectivity; if the server is unreachable the client
// is closed and nil is returned so callers can continue without Redis.
func InitRedis(addr, password string, db int) *redis.Client {
	if addr == "" {
		slog.Warn("Redis disabled: redis.addr is empty")
		return nil
	}

	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		slog.Warn("Redis unavailable; continuing without Redis-backed holds", "error", err)
		if closeErr := client.Close(); closeErr != nil {
			slog.Warn("failed to close unavailable Redis client", "error", closeErr)
		}
		return nil
	}

	slog.Info("Redis connection established", "addr", addr, "db", db)
	return client
}
