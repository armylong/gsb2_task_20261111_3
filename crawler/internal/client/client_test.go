package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gsb2_task_20261111_3/crawler/internal/model"
)

func newTestClient(srvURL string, sleeps *[]time.Duration, mu *sync.Mutex) *Client {
	cfg := DefaultConfig()
	cfg.BaseURL = srvURL
	cfg.RPS = 100
	cfg.InitialBackoff = 10 * time.Millisecond
	cfg.MaxBackoff = 200 * time.Millisecond
	cfg.MaxRetries = 5
	c := New(cfg)
	c.Sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		*sleeps = append(*sleeps, d)
		mu.Unlock()
		return nil
	}
	return c
}

// TestRetryAfterHeader 优先采用服务端 Retry-After
func TestRetryAfterHeader(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":"too many requests"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(model.ListResponse{TotalPages: 1, Items: []model.RawListItem{{ID: 1}}})
	}))
	defer srv.Close()

	var sleeps []time.Duration
	var mu sync.Mutex
	c := newTestClient(srv.URL, &sleeps, &mu)

	if _, err := c.ListPage(context.Background(), 1); err != nil {
		t.Fatalf("重试后应成功: %v", err)
	}
	if calls != 3 {
		t.Errorf("总请求 %d 次，想 3", calls)
	}
	if len(sleeps) != 2 {
		t.Fatalf("退避 %d 次，想 2", len(sleeps))
	}
	for _, d := range sleeps {
		if d != time.Second {
			t.Errorf("Retry-After=1 应退 1s，实际 %v", d)
		}
	}
}

// TestExponentialBackoffNoHeader 无 Retry-After 时走指数退避
func TestExponentialBackoffNoHeader(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(model.ListResponse{TotalPages: 1})
	}))
	defer srv.Close()

	var sleeps []time.Duration
	var mu sync.Mutex
	c := newTestClient(srv.URL, &sleeps, &mu)

	if _, err := c.ListPage(context.Background(), 1); err != nil {
		t.Fatalf("重试后应成功: %v", err)
	}
	if len(sleeps) < 3 {
		t.Fatalf("退避 %d 次，想 >=3", len(sleeps))
	}
	// 基数 10ms 翻倍，抖动只增不减（0 ~ +50%），验证下界逐次翻倍
	base := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}
	for i := 0; i < 3; i++ {
		if sleeps[i] < base[i] {
			t.Errorf("第 %d 次退避 %v 小于指数基数 %v", i+1, sleeps[i], base[i])
		}
	}
}

// TestRetryExhausted 持续 429 超过上限应报错
func TestRetryExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	var sleeps []time.Duration
	var mu sync.Mutex
	c := newTestClient(srv.URL, &sleeps, &mu)

	_, err := c.ListPage(context.Background(), 1)
	if err == nil {
		t.Fatal("持续 429 应在重试耗尽后报错")
	}
	// MaxRetries=5 -> 共 6 次尝试，每次失败都退避，故 6 次睡眠
	if len(sleeps) != 6 {
		t.Errorf("退避次数 %d，想 6", len(sleeps))
	}
}

// TestNotFoundNoRetry 404 直接返回 ErrNotFound，不重试
func TestNotFoundNoRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	var sleeps []time.Duration
	var mu sync.Mutex
	c := newTestClient(srv.URL, &sleeps, &mu)

	_, err := c.Detail(context.Background(), 42)
	if err != ErrNotFound {
		t.Fatalf("想 ErrNotFound，实际 %v", err)
	}
	if calls != 1 {
		t.Errorf("404 不该重试，却请求了 %d 次", calls)
	}
	if len(sleeps) != 0 {
		t.Errorf("404 不该退避，却睡了 %d 次", len(sleeps))
	}
}

// TestServerErrorRetry 5xx 也走退避重试
func TestServerErrorRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(model.RawDetail{ID: 7})
	}))
	defer srv.Close()

	var sleeps []time.Duration
	var mu sync.Mutex
	c := newTestClient(srv.URL, &sleeps, &mu)

	d, err := c.Detail(context.Background(), 7)
	if err != nil {
		t.Fatalf("5xx 重试后应成功: %v", err)
	}
	if d.ID != 7 || calls != 2 {
		t.Errorf("结果不对 id=%d calls=%d", d.ID, calls)
	}
	if len(sleeps) != 1 {
		t.Errorf("应退避 1 次，实际 %d", len(sleeps))
	}
}
