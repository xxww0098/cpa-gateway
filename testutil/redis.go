// Package testutil provides shared helpers for tests.
//
// This file is intentionally NOT a _test.go file so that helpers can be
// imported from tests across multiple packages. Because the testutil package
// is only imported from _test.go files, miniredis and other test-only
// dependencies will not be linked into the production binary.
package testutil

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// MustMiniRedis starts an in-process miniredis server and returns a connected
// *redis.Client along with the underlying *miniredis.Miniredis instance for
// test assertions. Both the server and the client are automatically closed
// when the test completes via t.Cleanup.
func MustMiniRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()

	srv, err := miniredis.Run()
	if err != nil {
		t.Fatalf("testutil: failed to start miniredis: %v", err)
	}

	client := redis.NewClient(&redis.Options{
		Addr: srv.Addr(),
	})

	t.Cleanup(func() {
		_ = client.Close()
		srv.Close()
	})

	return client, srv
}
