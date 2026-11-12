package crawl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"gsb2_task_20261111_3/crawler/internal/client"
	"gsb2_task_20261111_3/crawler/internal/model"
	"gsb2_task_20261111_3/crawler/internal/storage"
)

// ---- 假站：实现和 mockserver 一样的协议，方便构造各种场景 ----

type mockBook struct {
	id       int
	price    string
	stock    int
	desc     string
	delisted bool
}

type mockSite struct {
	mu sync.Mutex

	books    []*mockBook // 按 id 升序
	pageSize int

	requestedPages map[int]int // 每页被请求了多少次（验重/验漏）
	detailHits     map[int]int // 每个详情被请求多少次

	fail429       int  // 前 N 个请求一律 429
	retryAfterHdr bool // 429 时带不带 Retry-After 头
	requestCount  int
}

func newMockSite(n int) *mockSite {
	s := &mockSite{
		pageSize:       20,
		requestedPages: map[int]int{},
		detailHits:     map[int]int{},
	}
	for i := 1; i <= n; i++ {
		s.books = append(s.books, &mockBook{
			id:    i,
			price: fmt.Sprintf(" %d.50 ", 10+i), // 故意带空白
			stock: i % 20,
			desc:  "<p>九成新，无笔记无划线</p><p>书角磕碰&nbsp;不影响阅读</p>",
		})
	}
	return s
}

func (s *mockSite) listed() []*mockBook {
	out := make([]*mockBook, 0, len(s.books))
	for _, b := range s.books {
		if !b.delisted {
			out = append(out, b)
		}
	}
	return out
}

func (s *mockSite) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/books", func(w http.ResponseWriter, r *http.Request) {
		if s.takeFail(w, r) {
			return
		}
		page := 1
		if v := r.URL.Query().Get("page"); v != "" {
			fmt.Sscanf(v, "%d", &page)
		}
		if page < 1 {
			page = 1
		}

		s.mu.Lock()
		listed := s.listed()
		total := len(listed)
		totalPages := (total + s.pageSize - 1) / s.pageSize
		s.requestedPages[page]++
		start := (page - 1) * s.pageSize
		if start > total {
			start = total
		}
		end := start + s.pageSize
		if end > total {
			end = total
		}
		items := make([]model.RawListItem, 0, end-start)
		for _, b := range listed[start:end] {
			items = append(items, model.RawListItem{
				ID: b.id, Title: "  书名 ", Author: "作者", Category: "计算机", Price: b.price,
			})
		}
		s.mu.Unlock()

		writeMockJSON(w, http.StatusOK, model.ListResponse{
			Total: total, Page: page, PageSize: s.pageSize,
			TotalPages: totalPages, Items: items,
		})
	})

	mux.HandleFunc("/api/books/", func(w http.ResponseWriter, r *http.Request) {
		if s.takeFail(w, r) {
			return
		}
		var id int
		fmt.Sscanf(r.URL.Path[len("/api/books/"):], "%d", &id)

		s.mu.Lock()
		s.detailHits[id]++
		var found *mockBook
		for _, b := range s.books {
			if b.id == id && !b.delisted {
				found = b
				break
			}
		}
		s.mu.Unlock()

		if found == nil {
			writeMockJSON(w, http.StatusNotFound, map[string]string{"error": "book not found"})
			return
		}
		writeMockJSON(w, http.StatusOK, model.RawDetail{
			ID: found.id, Title: "  书名 ", Author: "作者", Category: "计算机",
			Price: found.price, Stock: found.stock, Condition: "九成新",
			ISBN: "9780000000000", ListedAt: "2026-10-01", Description: found.desc,
		})
	})

	return mux
}

// takeFail 处理前 N 个请求注入 429 的逻辑，返回 true 表示已响应失败
func (s *mockSite) takeFail(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	s.requestCount++
	fail := s.fail429 > 0
	if fail {
		s.fail429--
	}
	withHdr := s.retryAfterHdr
	s.mu.Unlock()

	if fail {
		if withHdr {
			w.Header().Set("Retry-After", "1")
		}
		writeMockJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many requests"})
		return true
	}
	return false
}

func writeMockJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// ---- 测试夹具 ----

type fixture struct {
	t      *testing.T
	srv    *httptest.Server
	site   *mockSite
	db     *storage.DB
	cli    *client.Client
	cliCfg client.Config

	sleepMu sync.Mutex
	sleeps  []time.Duration
}

func newFixture(t *testing.T, n int) *fixture {
	t.Helper()
	site := newMockSite(n)
	srv := httptest.NewServer(site.handler())
	t.Cleanup(srv.Close)

	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "crawl.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := client.DefaultConfig()
	cfg.BaseURL = srv.URL
	cfg.RPS = 100 // 测试里不做真实限流，429 场景单独注入
	cfg.InitialBackoff = 10 * time.Millisecond
	cfg.MaxBackoff = 200 * time.Millisecond
	cfg.MaxRetries = 6
	cli := client.New(cfg)

	f := &fixture{t: t, srv: srv, site: site, db: db, cli: cli, cliCfg: cfg}
	// 退避不真睡，只记录时长
	cli.Sleep = func(ctx context.Context, d time.Duration) error {
		f.sleepMu.Lock()
		f.sleeps = append(f.sleeps, d)
		f.sleepMu.Unlock()
		return nil
	}
	return f
}

func (f *fixture) crawler() *Crawler {
	return New(f.cli, f.db, log.New(io.Discard, "", 0))
}

func (f *fixture) baseCfg(mode string) Config {
	return Config{Mode: mode, Concurrency: 3, Client: f.cliCfg}
}

// countBooks 直接查库统计书行数
func countBooks(t *testing.T, db *storage.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), `SELECT COUNT(1) FROM books`).Scan(&n); err != nil {
		t.Fatalf("查询书数失败: %v", err)
	}
	return n
}

// ---- 集成测试 1：翻页不漏不重 ----

func TestIntegration_PaginationNoMissingNoDuplicate(t *testing.T) {
	const n = 53 // 不是整页：3 页（20+20+13）
	f := newFixture(t, n)

	res, err := f.crawler().Run(context.Background(), f.baseCfg(ModeFull), 0)
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	if res.TotalPages != 3 {
		t.Fatalf("总页数 = %d, 想 3", res.TotalPages)
	}

	// 每页恰好请求一次，且 1..3 全覆盖
	f.site.mu.Lock()
	gotPages := make([]int, 0)
	for p, c := range f.site.requestedPages {
		if c != 1 {
			t.Errorf("第 %d 页被请求了 %d 次，应该恰好 1 次", p, c)
		}
		gotPages = append(gotPages, p)
	}
	sort.Ints(gotPages)
	if len(gotPages) != 3 || gotPages[0] != 1 || gotPages[1] != 2 || gotPages[2] != 3 {
		t.Errorf("实际请求的页 = %v，想 [1 2 3]", gotPages)
	}
	// 每本详情也恰好请求一次
	for id := 1; id <= n; id++ {
		if f.site.detailHits[id] != 1 {
			t.Errorf("书 %d 详情被请求 %d 次，想 1 次", id, f.site.detailHits[id])
		}
	}
	f.site.mu.Unlock()

	if got := countBooks(t, f.db); got != n {
		t.Errorf("落库书数 = %d，想 %d", got, n)
	}

	// 清洗效果抽查：库里不允许出现 HTML 标签/实体
	var bad int
	if err := f.db.QueryRow(context.Background(),
		`SELECT COUNT(1) FROM books WHERE description LIKE '%<%' OR description LIKE '%&%'`).Scan(&bad); err != nil {
		t.Fatalf("查 HTML 残留失败: %v", err)
	}
	if bad != 0 {
		t.Errorf("有 %d 行描述里残留 HTML 痕迹", bad)
	}
	var priceCents int64
	if err := f.db.QueryRow(context.Background(), `SELECT price_cents FROM books WHERE id=1`).Scan(&priceCents); err != nil {
		t.Fatalf("查价格失败: %v", err)
	}
	if priceCents != 1150 { // " 11.50 " -> 1150 分
		t.Errorf("价格清洗 = %d, 想 1150", priceCents)
	}
}

// ---- 集成测试 2：429 退避重试 ----

func TestIntegration_BackoffOn429(t *testing.T) {
	t.Run("带Retry-After头", func(t *testing.T) {
		f := newFixture(t, 25)
		f.site.fail429 = 4
		f.site.retryAfterHdr = true

		res, err := f.crawler().Run(context.Background(), f.baseCfg(ModeFull), 0)
		if err != nil {
			t.Fatalf("有 429 的情况下应该重试成功，实际: %v", err)
		}
		if got := countBooks(t, f.db); got != 25 {
			t.Errorf("落库 %d 本，想 25", got)
		}
		f.sleepMu.Lock()
		sleeps := append([]time.Duration(nil), f.sleeps...)
		f.sleepMu.Unlock()
		if len(sleeps) < 4 {
			t.Fatalf("退避睡眠次数 = %d，至少应有 4 次", len(sleeps))
		}
		// Retry-After: 1 -> 每次都睡 1s（测试里是假睡）
		for _, d := range sleeps[:4] {
			if d != time.Second {
				t.Errorf("带 Retry-After 时退避 = %v，想 1s", d)
			}
		}
		if res.Resumed {
			t.Error("新跑不应被标记为续采")
		}
	})

	t.Run("无Retry-After头走指数退避", func(t *testing.T) {
		f := newFixture(t, 25)
		f.site.fail429 = 3
		f.site.retryAfterHdr = false

		if _, err := f.crawler().Run(context.Background(), f.baseCfg(ModeFull), 0); err != nil {
			t.Fatalf("退避重试后应成功: %v", err)
		}
		f.sleepMu.Lock()
		sleeps := append([]time.Duration(nil), f.sleeps...)
		f.sleepMu.Unlock()
		if len(sleeps) < 3 {
			t.Fatalf("退避次数 = %d，想 >=3", len(sleeps))
		}
		// 指数退避基数 10ms，抖动最多 +50%，验证主体逐次增长（抖动存在，所以只比下界）
		base := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}
		for i := 0; i < 3; i++ {
			if sleeps[i] < base[i] {
				t.Errorf("第 %d 次退避 %v 小于基数 %v", i+1, sleeps[i], base[i])
			}
		}
	})

	t.Run("超过重试上限返回错误且不丢断点", func(t *testing.T) {
		f := newFixture(t, 25)
		f.site.fail429 = 100 // 一直 429

		_, err := f.crawler().Run(context.Background(), f.baseCfg(ModeFull), 0)
		if err == nil {
			t.Fatal("一直 429 应该最终报错")
		}
		// 第一页一个字都没写成，运行记录应仍可续
		rid, mode, rerr := f.db.LatestRunnableRun(context.Background())
		if rerr != nil || rid == 0 || mode != ModeFull {
			t.Fatalf("失败后断点丢失: rid=%d mode=%q err=%v", rid, mode, rerr)
		}
	})
}

// ---- 集成测试 3：断点续采（详情阶段中断 + 列表阶段中断）----

func TestIntegration_CheckpointResumeDuringDetails(t *testing.T) {
	const n = 53
	f := newFixture(t, n)

	// 第一次：处理 10 本详情后模拟 Ctrl+C
	cfg := f.baseCfg(ModeFull)
	cfg.FailAfterDetails = 10
	res1, err := f.crawler().Run(context.Background(), cfg, 0)
	if err != context.Canceled {
		t.Fatalf("想 context.Canceled，实际 %v", err)
	}
	firstRun := res1.RunID
	written1 := countBooks(t, f.db)
	// 达到 10 本阈值即取消；并发下可能有少量在途请求没来得及写库
	if written1 <= 0 || written1 >= n {
		t.Fatalf("中断时落库 %d 本，应落在 (0, %d)", written1, n)
	}
	// 达到 10 本阈值即取消；3 个 worker 并发，在途的可能刚好完成，
	// 允许略超阈值，但不应超过 阈值 + worker 数
	if written1 > 10+f.baseCfg(ModeFull).Concurrency {
		t.Fatalf("中断后写库 %d 本，超出 阈值+worker数 的合理上限", written1)
	}
	// 断点完整性的正确判据：列表见过的 n 个 ID 必须全部持久化进任务队列，
	// 一个不丢。不能用"books 行数 + 待办数"，因为已写库但还没回写 done 的
	// 在途任务会同时计入两边，导致重复计数。
	stats, err := f.db.RunStats(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("查运行统计失败: %v", err)
	}
	if stats.DetailTotal != n {
		t.Errorf("任务队列总数 %d != 列表书数 %d，断点丢任务了", stats.DetailTotal, n)
	}
	// 中断后必然还有未完成（pending/claimed）任务，否则说明没在中途停下来
	if stats.DetailPending == 0 {
		t.Error("中断后不应所有任务都已完成")
	}
	if err := f.db.InterruptRun(context.Background(), firstRun); err != nil {
		t.Fatalf("标记中断失败: %v", err)
	}

	// 记录首轮中断前已确认 done 的书，这些在续采里一次都不许再请求
	doneInFirstRun := map[int]int{}
	f.site.mu.Lock()
	for id := 1; id <= n; id++ {
		st, err := f.db.TaskStatus(context.Background(), firstRun, int64(id))
		if err != nil {
			t.Fatalf("查任务状态失败: %v", err)
		}
		if st == "done" {
			doneInFirstRun[id] = f.site.detailHits[id]
		}
	}
	f.site.mu.Unlock()

	// 重新跑：应自动找到同一个运行并续上
	rid, mode, err := f.db.LatestRunnableRun(context.Background())
	if err != nil || rid != firstRun || mode != ModeFull {
		t.Fatalf("断点没找到: rid=%d mode=%q err=%v", rid, mode, err)
	}
	res2, err := f.crawler().Run(context.Background(), f.baseCfg(ModeFull), rid)
	if err != nil {
		t.Fatalf("续采失败: %v", err)
	}
	if !res2.Resumed || res2.RunID != firstRun {
		t.Fatalf("没有接上旧运行: resumed=%v runID=%d", res2.Resumed, res2.RunID)
	}
	if got := countBooks(t, f.db); got != n {
		t.Errorf("续采后落库 %d 本，想 %d", got, n)
	}

	// 列表在首轮已全部采完，续采不许越过总页数去请求第 totalPages+1 页
	f.site.mu.Lock()
	if f.site.requestedPages[res2.TotalPages+1] != 0 {
		t.Errorf("续采越界请求了第 %d 页（总页数 %d）", res2.TotalPages+1, res2.TotalPages)
	}
	f.site.mu.Unlock()

	// 首轮已 done 的任务，续采后请求次数必须原样不变（一次都没重做）
	f.site.mu.Lock()
	for id, firstHits := range doneInFirstRun {
		if f.site.detailHits[id] != firstHits {
			t.Errorf("书 %d 首轮已 done，续采却又请求了 %d 次（首轮 %d 次）",
				id, f.site.detailHits[id], firstHits)
		}
	}
	// 在途任务允许"取消→重抓一次"，任何书都不应超过 2 次
	for id := 1; id <= n; id++ {
		if f.site.detailHits[id] > 2 {
			t.Errorf("书 %d 详情累计请求 %d 次，超过在途重抓的合理上限", id, f.site.detailHits[id])
		}
	}
	f.site.mu.Unlock()

	// 所有任务最终都收敛到终态，没有遗留 pending/claimed
	remaining, err := f.db.PendingDetailCount(context.Background(), firstRun)
	if err != nil {
		t.Fatalf("查剩余任务失败: %v", err)
	}
	if remaining != 0 {
		t.Errorf("续采后还有 %d 个任务没收敛，应该全部终态", remaining)
	}

	// 再跑一次：旧运行已 done，应新开一轮而不是复用
	res3, err := f.crawler().Run(context.Background(), f.baseCfg(ModeFull), rid)
	if err != nil {
		t.Fatalf("第三轮失败: %v", err)
	}
	if res3.RunID == firstRun || res3.Resumed {
		t.Errorf("已完成的运行不应被复用: runID=%d resumed=%v", res3.RunID, res3.Resumed)
	}
}

func TestIntegration_CheckpointResumeDuringList(t *testing.T) {
	const n = 53
	f := newFixture(t, n)

	// 第一次：翻完第 1 页就在列表阶段中断
	cfg := f.baseCfg(ModeFull)
	cfg.FailAfterListPage = 1
	_, err := f.crawler().Run(context.Background(), cfg, 0)
	if err != context.Canceled {
		t.Fatalf("想 context.Canceled，实际 %v", err)
	}
	rid, _, err := f.db.LatestRunnableRun(context.Background())
	if err != nil || rid == 0 {
		t.Fatalf("列表中断后断点丢失: %v", err)
	}
	// 第一页的 20 个详情任务应已持久化
	pending, err := f.db.PendingDetailCount(context.Background(), rid)
	if err != nil || pending != 20 {
		t.Fatalf("入队任务 = %d (err=%v)，想 20", pending, err)
	}
	if err := f.db.InterruptRun(context.Background(), rid); err != nil {
		t.Fatalf("标记中断失败: %v", err)
	}

	// 续采：跳过第 1 页，从第 2 页继续
	if _, err := f.crawler().Run(context.Background(), f.baseCfg(ModeFull), rid); err != nil {
		t.Fatalf("续采失败: %v", err)
	}
	f.site.mu.Lock()
	defer f.site.mu.Unlock()
	for p := 1; p <= 3; p++ {
		if f.site.requestedPages[p] != 1 {
			t.Errorf("第 %d 页累计请求 %d 次，想恰好 1 次（不能重采）", p, f.site.requestedPages[p])
		}
	}
	if got := countBooks(t, f.db); got != n {
		t.Errorf("续采后落库 %d 本，想 %d", got, n)
	}
}

// ---- 集成测试 4：增量模式只写变化、处理下架 ----

func TestIntegration_IncrementalMode(t *testing.T) {
	const n = 40
	f := newFixture(t, n)

	// 首轮全量
	if _, err := f.crawler().Run(context.Background(), f.baseCfg(ModeFull), 0); err != nil {
		t.Fatalf("全量采集失败: %v", err)
	}
	var updatedBefore string
	if err := f.db.QueryRow(context.Background(), `SELECT updated_at FROM books WHERE id=3`).Scan(&updatedBefore); err != nil {
		t.Fatalf("查 updated_at 失败: %v", err)
	}
	var priceBefore int64
	if err := f.db.QueryRow(context.Background(), `SELECT price_cents FROM books WHERE id=1`).Scan(&priceBefore); err != nil {
		t.Fatalf("查价格失败: %v", err)
	}

	// 数据集变化：书 1 涨价，书 2 下架
	f.site.mu.Lock()
	f.site.books[0].price = "99.99"
	f.site.books[1].delisted = true
	f.site.mu.Unlock()

	// 等一小步确保 updated_at（秒级）能区分
	time.Sleep(1100 * time.Millisecond)

	res, err := f.crawler().Run(context.Background(), f.baseCfg(ModeIncr), 0)
	if err != nil {
		t.Fatalf("增量采集失败: %v", err)
	}
	if res.DetailsWritten != 1 {
		t.Errorf("增量写库 = %d，想 1（只有涨价那本）", res.DetailsWritten)
	}
	if res.DetailsUnchanged != n-2 {
		t.Errorf("跳过未变 = %d，想 %d", res.DetailsUnchanged, n-2)
	}
	// 下架的书已从列表消失：走"列表缺席→标记 delisted"路径，
	// 不会再发它的详情请求（与真实 mockserver 行为一致）
	if res.NotFound != 0 || res.DelistedMarked != 1 {
		t.Errorf("下架处理不对: 404=%d 标记下架=%d，想 404=0 标记=1", res.NotFound, res.DelistedMarked)
	}

	var priceNow int64
	if err := f.db.QueryRow(context.Background(), `SELECT price_cents FROM books WHERE id=1`).Scan(&priceNow); err != nil {
		t.Fatalf("查价格失败: %v", err)
	}
	if priceNow != 9999 {
		t.Errorf("涨价后价格 = %d，想 9999", priceNow)
	}
	var status2 string
	if err := f.db.QueryRow(context.Background(), `SELECT status FROM books WHERE id=2`).Scan(&status2); err != nil {
		t.Fatalf("查状态失败: %v", err)
	}
	if status2 != model.StatusDelisted {
		t.Errorf("书 2 状态 = %q，想 delisted", status2)
	}
	var updatedNow string
	if err := f.db.QueryRow(context.Background(), `SELECT updated_at FROM books WHERE id=3`).Scan(&updatedNow); err != nil {
		t.Fatalf("查 updated_at 失败: %v", err)
	}
	if updatedNow != updatedBefore {
		t.Errorf("没变的书 updated_at 被改动了: %q -> %q", updatedBefore, updatedNow)
	}

	// 再来一轮增量，什么都没变，应该一本都不写
	res2, err := f.crawler().Run(context.Background(), f.baseCfg(ModeIncr), 0)
	if err != nil {
		t.Fatalf("第二次增量失败: %v", err)
	}
	if res2.DetailsWritten != 0 {
		t.Errorf("无变化时写库 = %d，想 0", res2.DetailsWritten)
	}
}
