// Package ratelimit implements rate limiting algorithms for the playground:
// 1. Token Bucket (continuous refill, allows controlled bursts)
// 2. Sliding Window (rolling time window, strict rate enforcement)
package ratelimit

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Default settings for playground limiters.
const (
	DefaultCapacity      = 10.0 // maximum tokens or requests
	DefaultRefillRate    = 2.0  // tokens refilled per second (token bucket)
	DefaultWindowSeconds = 5.0  // sliding window duration in seconds
	DefaultTTL           = 10 * time.Minute
)

// Result is the outcome of a rate-limit attempt.
type Result struct {
	Algorithm       string    `json:"algorithm"`
	Allowed         bool      `json:"allowed"`
	TokensRemaining float64   `json:"tokens_remaining"`
	Capacity        float64   `json:"capacity"`
	RefillRate      float64   `json:"refill_rate_per_sec,omitempty"`
	WindowSec       float64   `json:"window_sec,omitempty"`
	Cost            float64   `json:"cost"`
	Key             string    `json:"key"`
	RetryAfterSec   int       `json:"retry_after_sec"`
	WaitMs          int64     `json:"wait_ms"`
	ResetSec        float64   `json:"reset_sec"`
	Timestamp       time.Time `json:"timestamp"`
	Error           string    `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// 1. Token Bucket Algorithm
// ---------------------------------------------------------------------------

// Bucket is a single token bucket.
type Bucket struct {
	mu         sync.Mutex
	capacity   float64
	refillRate float64 // tokens per second
	tokens     float64
	lastRefill time.Time
}

// NewBucket constructs a token bucket with full capacity.
func NewBucket(capacity, refillRate float64) *Bucket {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	if refillRate <= 0 {
		refillRate = DefaultRefillRate
	}
	return &Bucket{
		capacity:   capacity,
		refillRate: refillRate,
		tokens:     capacity,
		lastRefill: time.Now(),
	}
}

// Take attempts to consume `cost` tokens from the bucket at time `now`.
func (b *Bucket) Take(cost float64, now time.Time) (allowed bool, remaining float64, wait time.Duration, reset time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.refill(now)

	tokensNeededForFull := math.Max(0, b.capacity-b.tokens)
	reset = time.Duration((tokensNeededForFull / b.refillRate) * float64(time.Second))

	if b.tokens >= cost {
		b.tokens -= cost
		remaining = math.Round(b.tokens*100) / 100
		tokensNeededForFull = math.Max(0, b.capacity-b.tokens)
		reset = time.Duration((tokensNeededForFull / b.refillRate) * float64(time.Second))
		return true, remaining, 0, reset
	}

	deficit := cost - b.tokens
	wait = time.Duration((deficit / b.refillRate) * float64(time.Second))
	remaining = math.Round(b.tokens*100) / 100
	return false, remaining, wait, reset
}

// Status inspects the bucket without consuming tokens.
func (b *Bucket) Status(now time.Time) (tokens float64, capacity float64, refillRate float64, reset time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.refill(now)
	tokens = math.Round(b.tokens*100) / 100
	tokensNeededForFull := math.Max(0, b.capacity-b.tokens)
	reset = time.Duration((tokensNeededForFull / b.refillRate) * float64(time.Second))
	return tokens, b.capacity, b.refillRate, reset
}

// Reset refills the bucket to full capacity.
func (b *Bucket) Reset(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens = b.capacity
	b.lastRefill = now
}

func (b *Bucket) refill(now time.Time) {
	if b.lastRefill.IsZero() {
		b.lastRefill = now
		return
	}
	delta := now.Sub(b.lastRefill).Seconds()
	if delta > 0 {
		b.tokens = math.Min(b.capacity, b.tokens+(delta*b.refillRate))
		b.lastRefill = now
	}
}

// ---------------------------------------------------------------------------
// 2. Sliding Window Algorithm (Rolling Log)
// ---------------------------------------------------------------------------

// SlidingWindow tracks request timestamps across a rolling time window.
type SlidingWindow struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	history []time.Time
}

// NewSlidingWindow creates a sliding window rate limiter.
func NewSlidingWindow(limit int, window time.Duration) *SlidingWindow {
	if limit <= 0 {
		limit = int(DefaultCapacity)
	}
	if window <= 0 {
		window = time.Duration(DefaultWindowSeconds * float64(time.Second))
	}
	return &SlidingWindow{
		limit:   limit,
		window:  window,
		history: make([]time.Time, 0, limit*2),
	}
}

// Take records requests if capacity permits in the rolling window.
func (sw *SlidingWindow) Take(cost int, now time.Time) (allowed bool, remaining float64, wait time.Duration, reset time.Duration) {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	sw.evictOld(now)
	active := len(sw.history)

	var oldest time.Time
	if active > 0 {
		oldest = sw.history[0]
		reset = oldest.Add(sw.window).Sub(now)
		if reset < 0 {
			reset = 0
		}
	}

	if active+cost <= sw.limit {
		for i := 0; i < cost; i++ {
			sw.history = append(sw.history, now)
		}
		rem := float64(sw.limit - (active + cost))
		oldest = sw.history[0]
		reset = oldest.Add(sw.window).Sub(now)
		if reset < 0 {
			reset = 0
		}
		return true, rem, 0, reset
	}

	deficit := (active + cost) - sw.limit
	idx := deficit - 1
	if idx >= len(sw.history) {
		idx = len(sw.history) - 1
	}
	if idx >= 0 {
		wait = sw.history[idx].Add(sw.window).Sub(now)
		if wait < 0 {
			wait = 0
		}
	}

	rem := math.Max(0, float64(sw.limit-active))
	return false, rem, wait, reset
}

// Status returns the remaining requests without recording a new request.
func (sw *SlidingWindow) Status(now time.Time) (remaining float64, limit float64, windowSec float64, reset time.Duration) {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	sw.evictOld(now)
	active := len(sw.history)
	rem := math.Max(0, float64(sw.limit-active))

	if active > 0 {
		reset = sw.history[0].Add(sw.window).Sub(now)
		if reset < 0 {
			reset = 0
		}
	}
	return rem, float64(sw.limit), sw.window.Seconds(), reset
}

// Reset clears all request timestamps in the sliding window.
func (sw *SlidingWindow) Reset() {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.history = sw.history[:0]
}

func (sw *SlidingWindow) evictOld(now time.Time) {
	cutoff := now.Add(-sw.window)
	i := 0
	for i < len(sw.history) && (sw.history[i].Before(cutoff) || sw.history[i].Equal(cutoff)) {
		i++
	}
	if i > 0 {
		sw.history = append(sw.history[:0], sw.history[i:]...)
	}
}

// ---------------------------------------------------------------------------
// Multi-Tenant Limiter Manager
// ---------------------------------------------------------------------------

type clientLimiters struct {
	bucket        *Bucket
	slidingWindow *SlidingWindow
	lastSeen      time.Time
}

// Limiter manages per-client buckets and sliding windows.
type Limiter struct {
	mu            sync.RWMutex
	clients       map[string]*clientLimiters
	capacity      float64
	rate          float64
	windowSeconds float64
	ttl           time.Duration
}

// NewLimiter creates a unified limiter.
func NewLimiter(capacity, rate float64) *Limiter {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	if rate <= 0 {
		rate = DefaultRefillRate
	}

	l := &Limiter{
		clients:       make(map[string]*clientLimiters),
		capacity:      capacity,
		rate:          rate,
		windowSeconds: DefaultWindowSeconds,
		ttl:           DefaultTTL,
	}

	go l.cleanupLoop(1 * time.Minute)
	return l
}

func (l *Limiter) getClient(key string) *clientLimiters {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	if c, exists := l.clients[key]; exists {
		c.lastSeen = now
		return c
	}

	c := &clientLimiters{
		bucket:        NewBucket(l.capacity, l.rate),
		slidingWindow: NewSlidingWindow(int(l.capacity), time.Duration(l.windowSeconds*float64(time.Second))),
		lastSeen:      now,
	}
	l.clients[key] = c
	return c
}

func (l *Limiter) cleanupLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	for range ticker.C {
		l.evict()
	}
}

func (l *Limiter) evict() {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := time.Now().Add(-l.ttl)
	for k, c := range l.clients {
		if c.lastSeen.Before(cutoff) {
			delete(l.clients, k)
		}
	}
}

// ResolveKey extracts the client identifier.
func ResolveKey(r *http.Request) string {
	if k := r.URL.Query().Get("key"); k != "" {
		if len(k) > 64 {
			k = k[:64]
		}
		return k
	}
	if k := r.Header.Get("X-Client-ID"); k != "" {
		if len(k) > 64 {
			k = k[:64]
		}
		return k
	}
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		if i := strings.IndexByte(ip, ','); i != -1 {
			return strings.TrimSpace(ip[:i])
		}
		return strings.TrimSpace(ip)
	}
	if r.RemoteAddr != "" {
		if host, _, err := splitHost(r.RemoteAddr); err == nil && host != "" {
			return host
		}
		return r.RemoteAddr
	}
	return "anonymous"
}

func splitHost(addr string) (string, string, error) {
	if i := strings.LastIndexByte(addr, ':'); i != -1 {
		return addr[:i], addr[i+1:], nil
	}
	return addr, "", nil
}

// ParseAlgorithm resolves "token_bucket" or "sliding_window" from the request.
func ParseAlgorithm(r *http.Request) string {
	algo := strings.ToLower(r.URL.Query().Get("algo"))
	if algo == "sliding_window" || algo == "sw" || algo == "sliding" {
		return "sliding_window"
	}
	return "token_bucket"
}

// HitHandler processes rate-limit requests.
func (l *Limiter) HitHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t0 := time.Now()
		key := ResolveKey(r)
		algo := ParseAlgorithm(r)

		cost := 1.0
		if cStr := r.URL.Query().Get("cost"); cStr != "" {
			if c, err := strconv.ParseFloat(cStr, 64); err == nil && c > 0 && c <= 100 {
				cost = c
			}
		}

		client := l.getClient(key)

		var allowed bool
		var remaining float64
		var wait, reset time.Duration
		var capVal, refillRate, windowSec float64

		if algo == "sliding_window" {
			allowed, remaining, wait, reset = client.slidingWindow.Take(int(cost), t0)
			capVal = float64(client.slidingWindow.limit)
			windowSec = client.slidingWindow.window.Seconds()
		} else {
			allowed, remaining, wait, reset = client.bucket.Take(cost, t0)
			capVal = client.bucket.capacity
			refillRate = client.bucket.refillRate
		}

		evalDurMs := float64(time.Since(t0).Microseconds()) / 1000.0

		// Rate limit headers
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-RateLimit-Algorithm", algo)
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(int(capVal)))
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%.2f", remaining))
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%.2f", reset.Seconds()))
		w.Header().Set("Server-Timing", fmt.Sprintf("limiter;dur=%.3f", evalDurMs))

		res := Result{
			Algorithm:       algo,
			Allowed:         allowed,
			TokensRemaining: remaining,
			Capacity:        capVal,
			RefillRate:      refillRate,
			WindowSec:       windowSec,
			Cost:            cost,
			Key:             key,
			WaitMs:          wait.Milliseconds(),
			ResetSec:        math.Round(reset.Seconds()*100) / 100,
			Timestamp:       t0.UTC(),
		}

		if !allowed {
			retryAfter := int(math.Ceil(wait.Seconds()))
			if retryAfter < 1 {
				retryAfter = 1
			}
			res.RetryAfterSec = retryAfter
			res.Error = "rate_limit_exceeded"
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			w.WriteHeader(http.StatusTooManyRequests)
		} else {
			w.WriteHeader(http.StatusOK)
		}

		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	}
}

// StatusHandler returns the current token state without consuming any tokens.
func (l *Limiter) StatusHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t0 := time.Now()
		key := ResolveKey(r)
		algo := ParseAlgorithm(r)

		client := l.getClient(key)

		var remaining, capVal, refillRate, windowSec float64
		var reset time.Duration

		if algo == "sliding_window" {
			remaining, capVal, windowSec, reset = client.slidingWindow.Status(t0)
		} else {
			remaining, capVal, refillRate, reset = client.bucket.Status(t0)
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-RateLimit-Algorithm", algo)
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(int(capVal)))
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%.2f", remaining))
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%.2f", reset.Seconds()))

		res := Result{
			Algorithm:       algo,
			Allowed:         remaining >= 1.0,
			TokensRemaining: remaining,
			Capacity:        capVal,
			RefillRate:      refillRate,
			WindowSec:       windowSec,
			Cost:            0,
			Key:             key,
			ResetSec:        math.Round(reset.Seconds()*100) / 100,
			Timestamp:       t0.UTC(),
		}

		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	}
}

// ResetHandler resets the client's rate limits.
func (l *Limiter) ResetHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t0 := time.Now()
		key := ResolveKey(r)
		algo := ParseAlgorithm(r)

		client := l.getClient(key)
		client.bucket.Reset(t0)
		client.slidingWindow.Reset()

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")

		res := Result{
			Algorithm:       algo,
			Allowed:         true,
			TokensRemaining: l.capacity,
			Capacity:        l.capacity,
			RefillRate:      l.rate,
			WindowSec:       l.windowSeconds,
			Key:             key,
			Timestamp:       t0.UTC(),
		}

		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	}
}
