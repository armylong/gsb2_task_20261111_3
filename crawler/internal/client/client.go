package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"gsb2_task_20261111_3/crawler/internal/model"
)

// ErrNotFound 表示详情接口返回 404（书已下架或不存在），不重试
var ErrNotFound = errors.New("book not found")

// Config 是采集客户端的可调参数
type Config struct {
	BaseURL        string        // 如 http://127.0.0.1:18080
	RPS            int           // 客户端自我限流：每秒最多发多少个请求
	MaxRetries     int           // 单个请求最多重试次数（不含首次）
	InitialBackoff time.Duration // 首次退避基数
	MaxBackoff     time.Duration // 退避上限
	HTTPTimeout    time.Duration // 单个请求超时
}

// DefaultConfig 给出一套保守的默认值：服务端限流是 10 req/s，客户端压到 8
func DefaultConfig() Config {
	return Config{
		RPS:            8,
		MaxRetries:     8,
		InitialBackoff: 500 * time.Millisecond,
		MaxBackoff:     30 * time.Second,
		HTTPTimeout:    15 * time.Second,
	}
}

// Client 负责所有 HTTP 交互，内部带客户端限流和退避重试
type Client struct {
	cfg   Config
	http  *http.Client
	lim   *rateLimiter
	rng   *rand.Rand
	rngMu sync.Mutex

	// Sleep 可在测试里替换成假时钟；默认就是 time.Sleep
	Sleep func(ctx context.Context, d time.Duration) error
}

// New 构造客户端
func New(cfg Config) *Client {
	if cfg.RPS <= 0 {
		cfg.RPS = 8
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 8
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = 500 * time.Millisecond
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 30 * time.Second
	}
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = 15 * time.Second
	}
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: cfg.HTTPTimeout},
		lim:  newRateLimiter(float64(cfg.RPS), float64(cfg.RPS)),
		rng:  rand.New(rand.NewSource(time.Now().UnixNano())),
		Sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
	}
}

// rateLimiter 是个简单令牌桶：每秒补 r 个，桶容量 burst，避免突发打爆服务端
type rateLimiter struct {
	mu     sync.Mutex
	tokens float64
	max    float64
	rate   float64 // 每秒补充的令牌数
	last   time.Time
}

func newRateLimiter(rate, burst float64) *rateLimiter {
	return &rateLimiter{tokens: burst, max: burst, rate: rate, last: time.Now()}
}

// wait 阻塞直到拿到一个令牌或 ctx 取消
func (l *rateLimiter) wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		now := time.Now()
		l.tokens += now.Sub(l.last).Seconds() * l.rate
		if l.tokens > l.max {
			l.tokens = l.max
		}
		l.last = now
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		// 还差多久能凑够一个令牌
		need := (1 - l.tokens) / l.rate
		l.mu.Unlock()

		t := time.NewTimer(time.Duration(need * float64(time.Second)))
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// backoff 计算第 attempt 次失败后的退避时长（attempt 从 0 开始）：
// 指数增长 + 最多 50% 随机抖动，封顶 MaxBackoff
func (c *Client) backoff(attempt int) time.Duration {
	d := c.cfg.InitialBackoff << attempt
	if d <= 0 || d > c.cfg.MaxBackoff {
		d = c.cfg.MaxBackoff
	}
	c.rngMu.Lock()
	jitter := time.Duration(c.rng.Int63n(int64(d)/2 + 1))
	c.rngMu.Unlock()
	d += jitter
	if d > c.cfg.MaxBackoff {
		d = c.cfg.MaxBackoff
	}
	return d
}

// retryAfter 解析 429 响应的 Retry-After 头（只支持秒数形式，mock 站给的就是 "1"）
func retryAfter(h http.Header) (time.Duration, bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

// do 发请求并处理重试。retryable 的失败（429/5xx/网络错误）按退避策略重试，
// 404 和 4xx 其他状态码直接返回。
func (c *Client) do(ctx context.Context, path string) ([]byte, int, error) {
	url := strings.TrimRight(c.cfg.BaseURL, "/") + path
	var lastErr error

	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if err := c.lim.wait(ctx); err != nil {
			return nil, 0, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, 0, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			// 连接被拒、超时等网络错误：退避后重试
			lastErr = err
			if !sleepCtx(ctx, c.Sleep, c.backoff(attempt)) {
				return nil, 0, ctx.Err()
			}
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusNotFound:
			return nil, resp.StatusCode, ErrNotFound
		case resp.StatusCode == http.StatusTooManyRequests:
			// 优先按服务端 Retry-After 退，没有就走指数退避
			wait, ok := retryAfter(resp.Header)
			if !ok {
				wait = c.backoff(attempt)
			}
			lastErr = fmt.Errorf("429 too many requests (尝试 %d/%d)", attempt+1, c.cfg.MaxRetries+1)
			if !sleepCtx(ctx, c.Sleep, wait) {
				return nil, resp.StatusCode, ctx.Err()
			}
			continue
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("服务端 %d (尝试 %d/%d): %s", resp.StatusCode, attempt+1, c.cfg.MaxRetries+1, truncate(body))
			if !sleepCtx(ctx, c.Sleep, c.backoff(attempt)) {
				return nil, resp.StatusCode, ctx.Err()
			}
			continue
		case resp.StatusCode != http.StatusOK:
			return nil, resp.StatusCode, fmt.Errorf("意外的状态码 %d: %s", resp.StatusCode, truncate(body))
		default:
			if readErr != nil {
				lastErr = readErr
				if !sleepCtx(ctx, c.Sleep, c.backoff(attempt)) {
					return nil, resp.StatusCode, ctx.Err()
				}
				continue
			}
			return body, resp.StatusCode, nil
		}
	}
	if lastErr == nil {
		lastErr = errors.New("重试次数耗尽")
	}
	return nil, 0, fmt.Errorf("请求 %s 重试 %d 次后仍失败: %w", path, c.cfg.MaxRetries, lastErr)
}

func sleepCtx(ctx context.Context, sleep func(context.Context, time.Duration) error, d time.Duration) bool {
	if sleep == nil {
		return ctx.Err() == nil
	}
	return sleep(ctx, d) == nil
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// ListPage 抓一页列表
func (c *Client) ListPage(ctx context.Context, page int) (*model.ListResponse, error) {
	body, _, err := c.do(ctx, fmt.Sprintf("/api/books?page=%d", page))
	if err != nil {
		return nil, err
	}
	var resp model.ListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("列表第 %d 页响应解析失败: %w", page, err)
	}
	return &resp, nil
}

// Detail 抓单本详情；下架/不存在返回 ErrNotFound
func (c *Client) Detail(ctx context.Context, id int64) (*model.RawDetail, error) {
	body, _, err := c.do(ctx, fmt.Sprintf("/api/books/%d", id))
	if err != nil {
		return nil, err
	}
	var d model.RawDetail
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("书 %d 详情响应解析失败: %w", id, err)
	}
	return &d, nil
}
