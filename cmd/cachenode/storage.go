package main

// storage.go — Redis-backed cache store with per-entry owner
// tagging + size accounting. Sits behind the cache HTTP server
// in server.go.
//
// Each cache entry is two Redis keys:
//   cache:<key>          → the cached value
//   cache-owner:<key>    → the owning workspace ID
// Two keys keep the hot Get path simple (one MGET would also
// work but adds round-trip parsing). They share the same TTL.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// cachePrefix segregates cache-node entries from any other
	// data in the same Redis instance — operators sometimes
	// share Redis between services.
	cachePrefix = "cn:cache:"
	ownerPrefix = "cn:owner:"

	// sampleSize is how many keys we look at when estimating
	// total cache size via SCAN + STRLEN sampling. ~50 keys
	// keeps the call cheap while staying statistically useful.
	sampleSize = 50
)

// CacheStats is what Storage.Stats returns + the HTTP /stats
// route serves.
type CacheStats struct {
	TotalEntries int     `json:"total_entries"`
	SizeMB       float64 `json:"size_mb"`
	MaxSizeMB    float64 `json:"max_size_mb"`
	HitRate      float64 `json:"hit_rate"`
	HitsTotal    int64   `json:"hits_total"`
	MissTotal    int64   `json:"miss_total"`
}

// CacheStorage is the persistence layer the cache node hands to
// the HTTP server. Exposes Get / Set / Delete / Stats and a
// small atomic hit/miss counter so /stats can compute hit rate
// without an extra Redis call.
type CacheStorage struct {
	rdb       *redis.Client
	maxSizeMB float64

	// Counters incremented inline by Get — atomic so the HTTP
	// server can read them under load without locking.
	hits   int64
	misses int64
}

// NewCacheStorage builds a CacheStorage backed by an existing
// *redis.Client.
func NewCacheStorage(rdb *redis.Client, maxSizeGB float64) *CacheStorage {
	return &CacheStorage{
		rdb:       rdb,
		maxSizeMB: maxSizeGB * 1024,
	}
}

// ─── Get / Set / Delete ──────────────────────────

// Get returns the cached value + the owning workspace ID. A
// miss returns ("", "", nil) — error is reserved for actual
// Redis failures.
func (s *CacheStorage) Get(ctx context.Context, key string) (string, string, error) {
	val, err := s.rdb.Get(ctx, cachePrefix+key).Result()
	if errors.Is(err, redis.Nil) {
		atomic.AddInt64(&s.misses, 1)
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("cachenode: get: %w", err)
	}
	owner, err := s.rdb.Get(ctx, ownerPrefix+key).Result()
	if errors.Is(err, redis.Nil) {
		// Owner missing is unusual — best-effort, treat as a
		// no-owner entry (older row written before owner tracking
		// landed).
		owner = ""
	} else if err != nil {
		return "", "", fmt.Errorf("cachenode: get owner: %w", err)
	}
	atomic.AddInt64(&s.hits, 1)
	return val, owner, nil
}

// Owner returns the workspace that owns key ("" when the key or its owner tag is absent).
// Unlike Get it touches no hit/miss counter.
func (s *CacheStorage) Owner(ctx context.Context, key string) (string, error) {
	owner, err := s.rdb.Get(ctx, ownerPrefix+key).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("cachenode: get owner: %w", err)
	}
	return owner, nil
}

// Set stores `value` under `key` with the owner tag + TTL. Rejects
// the write when the projected total cache size would exceed the
// configured ceiling. Size accounting is sample-based (see Stats)
// so this is a soft enforcement — it can over-shoot by ~1 sample
// window under heavy concurrent writes.
func (s *CacheStorage) Set(ctx context.Context, key, value, ownerWorkspace string, ttl time.Duration) error {
	stats, err := s.Stats(ctx)
	if err == nil {
		incoming := float64(len(value)) / (1024 * 1024)
		if stats.SizeMB+incoming > s.maxSizeMB {
			return fmt.Errorf("cachenode: cache full (%.2f MB used / %.2f MB max)",
				stats.SizeMB, s.maxSizeMB)
		}
	}
	pipe := s.rdb.Pipeline()
	pipe.Set(ctx, cachePrefix+key, value, ttl)
	if ownerWorkspace != "" {
		pipe.Set(ctx, ownerPrefix+key, ownerWorkspace, ttl)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("cachenode: set: %w", err)
	}
	return nil
}

// Delete removes both the value and the owner key. Idempotent.
func (s *CacheStorage) Delete(ctx context.Context, key string) error {
	if _, err := s.rdb.Del(ctx, cachePrefix+key, ownerPrefix+key).Result(); err != nil {
		return fmt.Errorf("cachenode: delete: %w", err)
	}
	return nil
}

// Flush wipes every key under the cachePrefix. The "talyvor-
// cachenode flush" admin command uses this. We SCAN +
// pipeline-DEL so a large cache doesn't OOM the Redis client.
func (s *CacheStorage) Flush(ctx context.Context) (int, error) {
	var (
		cursor uint64
		count  int
	)
	for {
		keys, next, err := s.rdb.Scan(ctx, cursor, cachePrefix+"*", 500).Result()
		if err != nil {
			return count, fmt.Errorf("cachenode: scan: %w", err)
		}
		if len(keys) > 0 {
			ownerKeys := make([]string, len(keys))
			for i, k := range keys {
				// Reconstruct the matching owner key.
				ownerKeys[i] = ownerPrefix + strings.TrimPrefix(k, cachePrefix)
			}
			pipe := s.rdb.Pipeline()
			pipe.Del(ctx, keys...)
			pipe.Del(ctx, ownerKeys...)
			if _, err := pipe.Exec(ctx); err != nil {
				return count, fmt.Errorf("cachenode: flush del: %w", err)
			}
			count += len(keys)
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return count, nil
}

// ─── Stats ───────────────────────────────────────

// Stats returns a fresh snapshot. TotalEntries comes from a
// SCAN that counts cache-prefix keys; SizeMB is estimated by
// STRLEN'ing a sample and extrapolating to the full key set.
// Cheap enough to call on every /stats hit.
func (s *CacheStorage) Stats(ctx context.Context) (CacheStats, error) {
	stats := CacheStats{MaxSizeMB: s.maxSizeMB}
	var (
		cursor       uint64
		allKeys      []string
		sampledBytes int64
	)
	for {
		keys, next, err := s.rdb.Scan(ctx, cursor, cachePrefix+"*", 500).Result()
		if err != nil {
			return stats, fmt.Errorf("cachenode: scan: %w", err)
		}
		allKeys = append(allKeys, keys...)
		if next == 0 {
			break
		}
		cursor = next
		// Cap the full scan so a million-key Redis doesn't make
		// Stats take seconds. We still extrapolate from the
		// sampled bytes regardless of total count.
		if len(allKeys) > 10_000 {
			break
		}
	}
	stats.TotalEntries = len(allKeys)

	// Sample up to `sampleSize` keys for STRLEN.
	limit := sampleSize
	if limit > len(allKeys) {
		limit = len(allKeys)
	}
	for i := 0; i < limit; i++ {
		n, err := s.rdb.StrLen(ctx, allKeys[i]).Result()
		if err != nil {
			continue
		}
		sampledBytes += n
	}
	if limit > 0 {
		avg := float64(sampledBytes) / float64(limit)
		stats.SizeMB = avg * float64(len(allKeys)) / (1024 * 1024)
	}

	hits := atomic.LoadInt64(&s.hits)
	misses := atomic.LoadInt64(&s.misses)
	total := hits + misses
	if total > 0 {
		stats.HitRate = float64(hits) / float64(total)
	}
	stats.HitsTotal = hits
	stats.MissTotal = misses
	return stats, nil
}
