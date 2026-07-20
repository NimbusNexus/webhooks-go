package redis_test

import (
	"fmt"
	"os"
	"testing"
	"time"

	webhookd "github.com/NimbusNexus/webhookd-go"
	redisstore "github.com/NimbusNexus/webhookd-go/store/redis"
	"github.com/NimbusNexus/webhookd-go/store/storetest"
)

// The Redis contract is the SAME as every other store's; it runs only when WEBHOOKD_TEST_REDIS_URL
// points at a reachable server, and skips cleanly otherwise (so `go test ./...` stays green with no
// infra). Each store gets a unique key prefix so subtests never collide, and Close is registered for
// cleanup.
func TestRedisStoreContract(t *testing.T) {
	url := os.Getenv("WEBHOOKD_TEST_REDIS_URL")
	if url == "" {
		t.Skip("set WEBHOOKD_TEST_REDIS_URL to run the Redis store contract")
	}
	var n int
	storetest.RunContract(t, func(t *testing.T) webhookd.Store {
		n++
		prefix := fmt.Sprintf("webhookd:test:%d:%d", time.Now().UnixNano(), n)
		s, err := redisstore.Open(redisstore.Options{URL: url, KeyPrefix: prefix})
		if err != nil {
			t.Fatalf("redis.Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}
