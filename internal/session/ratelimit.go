package session

import (
	"sync"
	"time"
)

// rateLimiter is a fixed-window counter keyed by client IP.
//
// The PRD deliberately removes the participant cap, which leaves a publicly
// shared link open to a bot flood. This is the recommended safeguard: it never
// rejects a legitimate room full of people (they arrive from many addresses)
// but it does blunt a single machine spraying thousands of joins.
type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	buckets map[string]*bucket
	swept   time.Time
}

type bucket struct {
	count int
	start time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, buckets: map[string]*bucket{}, swept: time.Now()}
}

func (r *rateLimiter) allow(key string) bool {
	if key == "" {
		return true
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()

	if now.Sub(r.swept) > r.window {
		for k, b := range r.buckets {
			if now.Sub(b.start) > r.window {
				delete(r.buckets, k)
			}
		}
		r.swept = now
	}

	b, ok := r.buckets[key]
	if !ok || now.Sub(b.start) > r.window {
		r.buckets[key] = &bucket{count: 1, start: now}
		return true
	}
	if b.count >= r.limit {
		return false
	}
	b.count++
	return true
}
