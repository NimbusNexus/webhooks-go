// Package redis provides a durable webhooks.Store backed by Redis via github.com/redis/go-redis/v9.
// Ordering by due-time lives in a sorted set scored on next_attempt_at (epoch ms); the record bodies
// live in a hash. Both keys hang off a caller-set prefix so concurrent users don't collide.
// Importing this package is what pulls the driver in — the core SDK never compiles it.
package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	webhooks "github.com/NimbusNexus/webhooks-go"
	goredis "github.com/redis/go-redis/v9"
)

// Store is a Redis-backed webhooks.Store. Sent records are removed from both keys; a record whose
// retry budget is exhausted is parked with a far-future score, so it is never returned as due.
type Store struct {
	client    *goredis.Client
	keyPrefix string
}

// Options configures Open.
type Options struct {
	// URL is a Redis connection URL, e.g. redis://localhost:6379. Ignored when Client is supplied.
	URL string
	// Client reuses an already-created go-redis client instead of URL.
	Client *goredis.Client
	// KeyPrefix namespaces the two keys this store uses. Default "webhookd:outbox".
	KeyPrefix string
}

// Open builds a Redis-backed store. Supply either opts.URL or opts.Client.
func Open(opts Options) (*Store, error) {
	client := opts.Client
	if client == nil {
		parsed, err := goredis.ParseURL(opts.URL)
		if err != nil {
			return nil, fmt.Errorf("redis: parse url: %w", err)
		}
		client = goredis.NewClient(parsed)
	}
	prefix := opts.KeyPrefix
	if prefix == "" {
		prefix = "webhookd:outbox"
	}
	return &Store{client: client, keyPrefix: prefix}, nil
}

func (s *Store) zsetKey() string { return s.keyPrefix + ":due" }
func (s *Store) hashKey() string { return s.keyPrefix + ":records" }

func (s *Store) Save(ctx context.Context, r webhooks.Record) error {
	body, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("redis: encode record: %w", err)
	}
	pipe := s.client.TxPipeline()
	pipe.HSet(ctx, s.hashKey(), r.ID, body)
	pipe.ZAdd(ctx, s.zsetKey(), goredis.Z{Score: float64(r.NextAttemptAt.UnixMilli()), Member: r.ID})
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis: save: %w", err)
	}
	return nil
}

func (s *Store) ListPending(ctx context.Context, limit int) ([]webhooks.Record, error) {
	now := fmt.Sprintf("%d", time.Now().UnixMilli())
	ids, err := s.client.ZRangeByScore(ctx, s.zsetKey(), &goredis.ZRangeBy{Min: "-inf", Max: now}).Result()
	if err != nil {
		return nil, fmt.Errorf("redis: list_pending: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	bodies, err := s.client.HMGet(ctx, s.hashKey(), ids...).Result()
	if err != nil {
		return nil, fmt.Errorf("redis: load records: %w", err)
	}
	out := make([]webhooks.Record, 0, len(bodies))
	for _, b := range bodies {
		raw, ok := b.(string)
		if !ok {
			continue // id fell out of the hash between the two calls
		}
		var r webhooks.Record
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, fmt.Errorf("redis: decode record: %w", err)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Store) MarkSent(ctx context.Context, id string) error {
	pipe := s.client.TxPipeline()
	pipe.HDel(ctx, s.hashKey(), id)
	pipe.ZRem(ctx, s.zsetKey(), id)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis: mark_sent: %w", err)
	}
	return nil
}

func (s *Store) MarkFailed(ctx context.Context, id string, lastError *string, attempts int, nextAttemptAt time.Time) error {
	raw, err := s.client.HGet(ctx, s.hashKey(), id).Result()
	if err == goredis.Nil {
		return nil // gone — nothing to update
	}
	if err != nil {
		return fmt.Errorf("redis: mark_failed load: %w", err)
	}
	var r webhooks.Record
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return fmt.Errorf("redis: decode record: %w", err)
	}
	r.LastError = lastError
	r.Attempts = attempts
	r.NextAttemptAt = nextAttemptAt
	body, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("redis: encode record: %w", err)
	}
	pipe := s.client.TxPipeline()
	pipe.HSet(ctx, s.hashKey(), id, body)
	pipe.ZAdd(ctx, s.zsetKey(), goredis.Z{Score: float64(nextAttemptAt.UnixMilli()), Member: id})
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis: mark_failed: %w", err)
	}
	return nil
}

func (s *Store) Size(ctx context.Context) (int, error) {
	n, err := s.client.HLen(ctx, s.hashKey()).Result()
	if err != nil {
		return 0, fmt.Errorf("redis: size: %w", err)
	}
	return int(n), nil
}

func (s *Store) Close() error { return s.client.Close() }
