package ratelimit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestBucketRefillAndTake(t *testing.T) {
	t0 := time.Now()
	b := NewBucket(10, 2) // 10 capacity, 2 tokens/sec

	allowed, remaining, wait, _ := b.Take(4, t0)
	if !allowed || remaining != 6 || wait != 0 {
		t.Fatalf("expected allowed=true, remaining=6, wait=0; got %v, %v, %v", allowed, remaining, wait)
	}

	allowed, remaining, wait, _ = b.Take(7, t0)
	if allowed || remaining != 6 || wait != 500*time.Millisecond {
		t.Fatalf("expected allowed=false, remaining=6, wait=500ms; got %v, %v, %v", allowed, remaining, wait)
	}

	t1 := t0.Add(1 * time.Second)
	allowed, remaining, _, _ = b.Take(7, t1)
	if !allowed || remaining != 1 {
		t.Fatalf("expected allowed=true after refill, remaining=1; got %v, %v", allowed, remaining)
	}
}

func TestSlidingWindowTakeAndEviction(t *testing.T) {
	t0 := time.Now()
	sw := NewSlidingWindow(5, 5*time.Second) // 5 requests per 5 seconds

	// Take 3 requests at t0
	allowed, remaining, wait, _ := sw.Take(3, t0)
	if !allowed || remaining != 2 || wait != 0 {
		t.Fatalf("expected allowed=true, remaining=2; got %v, %v", allowed, remaining)
	}

	// Take 2 requests at t0 + 1s -> total 5 in window
	t1 := t0.Add(1 * time.Second)
	allowed, remaining, wait, _ = sw.Take(2, t1)
	if !allowed || remaining != 0 {
		t.Fatalf("expected allowed=true, remaining=0; got %v, %v", allowed, remaining)
	}

	// Take 1 request at t0 + 2s -> should be blocked! Wait time should be until t0 + 5s (3s wait)
	t2 := t0.Add(2 * time.Second)
	allowed, remaining, wait, _ = sw.Take(1, t2)
	if allowed || remaining != 0 || wait != 3*time.Second {
		t.Fatalf("expected allowed=false, remaining=0, wait=3s; got %v, %v, %v", allowed, remaining, wait)
	}

	// Advance time to t0 + 5.1s -> first 3 requests from t0 have expired, only 2 requests from t1 active
	t3 := t0.Add(5100 * time.Millisecond)
	allowed, remaining, wait, _ = sw.Take(2, t3)
	if !allowed || remaining != 1 {
		t.Fatalf("expected allowed=true after sliding window roll, remaining=1; got %v, %v", allowed, remaining)
	}
}

func TestConcurrentSlidingWindow(t *testing.T) {
	sw := NewSlidingWindow(50, 5*time.Second)
	now := time.Now()

	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex

	for i := 0; i < 80; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allowed, _, _, _ := sw.Take(1, now)
			if allowed {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if successCount != 50 {
		t.Fatalf("expected exactly 50 successful takes in sliding window, got %d", successCount)
	}
}

func TestHTTPHandlersBothAlgorithms(t *testing.T) {
	limiter := NewLimiter(4, 2)

	// 1. Test Token Bucket (default)
	reqTB := httptest.NewRequest("GET", "/api/ratelimit?key=tb_test&cost=3", nil)
	wTB := httptest.NewRecorder()
	limiter.HitHandler().ServeHTTP(wTB, reqTB)

	if wTB.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", wTB.Code)
	}
	if wTB.Header().Get("X-RateLimit-Algorithm") != "token_bucket" {
		t.Fatalf("expected algo=token_bucket, got %v", wTB.Header().Get("X-RateLimit-Algorithm"))
	}

	// 2. Test Sliding Window
	reqSW := httptest.NewRequest("GET", "/api/ratelimit?key=sw_test&algo=sliding_window&cost=4", nil)
	wSW := httptest.NewRecorder()
	limiter.HitHandler().ServeHTTP(wSW, reqSW)

	if wSW.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", wSW.Code)
	}
	if wSW.Header().Get("X-RateLimit-Algorithm") != "sliding_window" {
		t.Fatalf("expected algo=sliding_window, got %v", wSW.Header().Get("X-RateLimit-Algorithm"))
	}

	// Immediate next request for SW should be 429
	reqSW2 := httptest.NewRequest("GET", "/api/ratelimit?key=sw_test&algo=sliding_window&cost=1", nil)
	wSW2 := httptest.NewRecorder()
	limiter.HitHandler().ServeHTTP(wSW2, reqSW2)

	if wSW2.Code != http.StatusTooManyRequests {
		t.Fatalf("expected status 429 for exceeded sliding window, got %d", wSW2.Code)
	}
	var resSW Result
	_ = json.NewDecoder(wSW2.Body).Decode(&resSW)
	if resSW.Allowed != false || resSW.Algorithm != "sliding_window" {
		t.Fatalf("expected res.Allowed=false with algo=sliding_window, got %v", resSW)
	}
}
