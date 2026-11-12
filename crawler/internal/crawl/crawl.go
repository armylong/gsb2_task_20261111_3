package crawl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"sync"
	"sync/atomic"

	"gsb2_task_20261111_3/crawler/internal/clean"
	"gsb2_task_20261111_3/crawler/internal/client"
	"gsb2_task_20261111_3/crawler/internal/model"
	"gsb2_task_20261111_3/crawler/internal/storage"
)

// ModeFull 全量：所有在架书都重新落库
// ModeIncr 增量：只有内容变化或新上架的书才写，下架的标记 delisted
const (
	ModeFull = "full"
	ModeIncr = "incr"
)

// Config 编排层参数
type Config struct {
	Client      client.Config
	Mode        string
	Concurrency int // 详情 worker 数

	// 测试钩子：每处理完这么多本详情就触发一次 ctx 取消，模拟中途 Ctrl+C。
	// 0 表示不注入。
	FailAfterDetails int
	// 测试钩子：在第 N 页列表处理完后取消，0 表示不注入
	FailAfterListPage int
}

// Result 是一次运行的结果
type Result struct {
	RunID            int64
	Resumed          bool
	TotalPages       int
	DetailsWritten   int // 实际写库的书数（增量模式下跳过的不计）
	DetailsUnchanged int // 增量模式下内容没变、跳过的书数
	NotFound         int // 详情 404 的书数
	DelistedMarked   int64
}

// Crawler 把 client、storage 串成完整采集流程
type Crawler struct {
	cli *client.Client
	db  *storage.DB
	log *log.Logger

	// 测试钩子：非 nil 时达到中断条件就调用它取消整个运行，
	// 和真实 Ctrl+C 走同一条 context 取消路径
	hookCancel context.CancelFunc
}

func New(cli *client.Client, db *storage.DB, logger *log.Logger) *Crawler {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Crawler{cli: cli, db: db, log: logger}
}

// hashBook 对清洗后的业务字段算哈希，作为增量模式的变更依据
func hashBook(b *model.Book) string {
	h := sha256.New()
	join := func(s string) { h.Write([]byte(s)); h.Write([]byte{0}) }
	join(strconv.FormatInt(b.ID, 10))
	join(b.Title)
	join(b.Author)
	join(b.Category)
	join(strconv.FormatInt(b.PriceCents, 10))
	join(strconv.Itoa(b.Stock))
	join(b.Condition)
	join(b.ISBN)
	join(b.ListedAt)
	join(b.Description)
	join(b.Status)
	return hex.EncodeToString(h.Sum(nil))
}

// toBook 把原始详情清洗成落库结构（绝不保留原始 HTML）
func toBook(d *model.RawDetail) (*model.Book, error) {
	priceCents, err := clean.Price(d.Price)
	if err != nil {
		return nil, fmt.Errorf("书 %d 价格解析失败: %w", d.ID, err)
	}
	b := &model.Book{
		ID:          int64(d.ID),
		Title:       clean.Text(d.Title),
		Author:      clean.Text(d.Author),
		Category:    clean.Text(d.Category),
		PriceCents:  priceCents,
		Stock:       d.Stock,
		Condition:   clean.Text(d.Condition),
		ISBN:        clean.Text(d.ISBN),
		ListedAt:    clean.Text(d.ListedAt),
		Description: clean.HTMLToText(d.Description),
		Status:      model.StatusListed,
	}
	b.ContentHash = hashBook(b)
	return b, nil
}

// Run 执行一次采集。resumeRunID > 0 且对应运行未完成时，接着它的断点走。
// 中断时返回 context.Canceled，Result 里仍带已完成的进度。
func (c *Crawler) Run(ctx context.Context, cfg Config, resumeRunID int64) (*Result, error) {
	if cfg.Mode != ModeFull && cfg.Mode != ModeIncr {
		return nil, fmt.Errorf("未知模式 %q，只支持 full / incr", cfg.Mode)
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if cfg.FailAfterDetails > 0 || cfg.FailAfterListPage > 0 {
		c.hookCancel = cancel
	} else {
		c.hookCancel = nil
	}

	runID, resumed, err := c.prepareRun(ctx, cfg.Mode, resumeRunID)
	if err != nil {
		return nil, err
	}
	// 回收上次中断时还在途的任务租约，退回待领取队列
	if err := c.db.RequeueClaimed(ctx, runID); err != nil {
		return nil, fmt.Errorf("回收在途任务失败: %w", err)
	}
	res := &Result{RunID: runID, Resumed: resumed}

	// 阶段一：顺序翻列表，落断点、入详情队列
	totalPages, listedIDs, err := c.crawlList(ctx, cfg, runID)
	if err != nil {
		return res, err
	}
	res.TotalPages = totalPages

	// 阶段二：并发抓详情，worker 从 sqlite 队列里原子领任务
	if err := c.crawlDetails(ctx, cfg, runID, res); err != nil {
		return res, err
	}

	// 阶段三：本次列表里没有的书，库里标记下架
	marked, err := c.db.MarkDelisted(ctx, listedIDs)
	if err != nil {
		return res, fmt.Errorf("标记下架失败: %w", err)
	}
	res.DelistedMarked = marked

	if err := c.db.FinishRun(ctx, runID); err != nil {
		return res, err
	}
	return res, nil
}

// prepareRun 决定续用旧运行还是新开一个
func (c *Crawler) prepareRun(ctx context.Context, mode string, resumeRunID int64) (int64, bool, error) {
	if resumeRunID > 0 {
		finished, err := c.db.RunFinished(ctx, resumeRunID)
		if err != nil {
			return 0, false, err
		}
		if !finished {
			return resumeRunID, true, nil
		}
		c.log.Printf("运行 #%d 已完成，重新开一轮", resumeRunID)
	}
	runID, err := c.db.StartRun(ctx, mode)
	if err != nil {
		return 0, false, err
	}
	return runID, false, nil
}

// crawlList 顺序翻完所有列表页。返回总页数和本次列表里出现过的全部书 ID。
// 列表页必须串行：总页数从第一页响应里拿，串行天然不会漏页/重页。
func (c *Crawler) crawlList(ctx context.Context, cfg Config, runID int64) (int, []int64, error) {
	// 续采时先取回已入队的 ID，保证最终下架判断用的是完整集合
	listedIDs, err := c.db.ListTaskIDs(ctx, runID)
	if err != nil {
		return 0, nil, err
	}
	seen := make(map[int64]struct{}, len(listedIDs))
	for _, id := range listedIDs {
		seen[id] = struct{}{}
	}

	// 续采时第一页可能已完成，总页数从运行记录里恢复
	totalPages, err := c.db.RunTotalPages(ctx, runID)
	if err != nil {
		return 0, listedIDs, err
	}

	page := 1
	for {
		if err := ctx.Err(); err != nil {
			return totalPages, listedIDs, err
		}
		// 护栏：续采时所有页可能都已完成，不能越过总页数再去请求一页空数据、
		// 还往断点表写一条越界记录
		if totalPages > 0 && page > totalPages {
			break
		}
		done, err := c.db.IsListPageDone(ctx, runID, page)
		if err != nil {
			return totalPages, listedIDs, err
		}
		if done {
			page++
			continue
		}

		resp, err := c.cli.ListPage(ctx, page)
		if err != nil {
			return totalPages, listedIDs, fmt.Errorf("列表第 %d 页抓取失败: %w", page, err)
		}
		if page == 1 {
			totalPages = resp.TotalPages
			if err := c.db.SetTotalPages(ctx, runID, totalPages); err != nil {
				return totalPages, listedIDs, err
			}
		}

		ids := make([]int64, 0, len(resp.Items))
		for _, it := range resp.Items {
			id := int64(it.ID)
			if _, ok := seen[id]; ok {
				continue // 防御性去重：同一 ID 不重复入队
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
			listedIDs = append(listedIDs, id)
		}
		// 入队和页断点在同一事务里提交，崩溃重跑不会出现半完成状态
		if err := c.db.EnqueueAndMarkPage(ctx, runID, page, ids); err != nil {
			return totalPages, listedIDs, err
		}
		c.log.Printf("列表第 %d/%d 页完成，本页 %d 本，累计在架 %d 本",
			page, resp.TotalPages, len(resp.Items), len(listedIDs))

		if cfg.FailAfterListPage > 0 && page >= cfg.FailAfterListPage && c.hookCancel != nil {
			c.hookCancel()
			return totalPages, listedIDs, context.Canceled
		}

		// 服务端 total_pages 是权威的翻完判据；
		// 兜底：某页空了也停，避免接口异常时死循环
		if (totalPages > 0 && page >= totalPages) || len(resp.Items) == 0 {
			break
		}
		page++
	}
	return totalPages, listedIDs, nil
}

// crawlDetails 起 worker 池消费详情队列；任务领取是 sqlite 原子操作，重启后继续领。
// 抓取失败（重试耗尽）的任务保持 pending，不会标 done，下轮运行重新领取。
func (c *Crawler) crawlDetails(ctx context.Context, cfg Config, runID int64, res *Result) error {
	workers := cfg.Concurrency
	taskCh := make(chan int64, workers)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	setErr := func(e error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = e
		}
		mu.Unlock()
	}
	var processed int64 // 测试钩子计数

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for id := range taskCh {
				if ctx.Err() != nil {
					return
				}
				// 本地重试：把 client 层偶发失败在 worker 内消化掉。
				// 耗尽仍失败就落 failed，避免任务一直 pending 被重复领取；
				// 只有 ctx 取消时才保留 pending，等下次运行续采。
				const maxLocalAttempts = 2
				var lastErr error
				for attempt := 1; attempt <= maxLocalAttempts; attempt++ {
					retry := c.handleOneDetail(ctx, cfg, runID, id, res, &processed, &mu, setErr)
					if !retry || ctx.Err() != nil {
						lastErr = nil
						break
					}
					lastErr = fmt.Errorf("书 %d 第 %d 次尝试失败", id, attempt)
				}
				if lastErr != nil && ctx.Err() == nil {
					if e := c.db.MarkDetailResult(ctx, runID, id, "failed", lastErr.Error()); e != nil {
						setErr(e)
					} else {
						c.log.Printf("书 %d 重试 %d 次仍失败，标记 failed: %v", id, maxLocalAttempts, lastErr)
					}
				}
			}
		}()
	}

dispatch:
	for {
		select {
		case <-ctx.Done():
			break dispatch
		default:
		}
		id, ok, err := c.db.ClaimDetailTask(ctx, runID)
		if err != nil {
			setErr(err)
			break dispatch
		}
		if !ok {
			// 列表阶段已全部入队，队列空了就真的做完了
			break dispatch
		}
		select {
		case taskCh <- id:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(taskCh)
	wg.Wait()

	// 取消优先：任何分支收到取消都统一返回 context 的错误
	if err := ctx.Err(); err != nil {
		return err
	}
	if firstErr != nil {
		return firstErr
	}
	return nil
}

// handleOneDetail 处理单本详情：抓取→清洗→落库→回写任务状态。
// 返回 true 表示可重试的抓取失败（调用方决定重试或落 failed），false 表示已终态处理。
func (c *Crawler) handleOneDetail(ctx context.Context, cfg Config, runID int64,
	id int64, res *Result, processed *int64, mu *sync.Mutex, setErr func(error)) bool {

	d, err := c.cli.Detail(ctx, id)
	if errors.Is(err, client.ErrNotFound) {
		// 列表里有、详情 404：列表翻页和下架之间的窗口期，按下架处理
		if e := c.db.MarkDetailResult(ctx, runID, id, "not_found", ""); e != nil {
			setErr(e)
			return false
		}
		mu.Lock()
		res.NotFound++
		mu.Unlock()
		c.log.Printf("书 %d 详情 404，按下架处理", id)
		c.afterDetail(cfg, processed)
		return false
	}
	if err != nil {
		// 网络/重试耗尽：交给上层 worker 决定本地重试；ctx 取消时保留 pending
		if ctx.Err() == nil {
			c.log.Printf("书 %d 详情抓取失败: %v", id, err)
		}
		return true
	}

	book, err := toBook(d)
	if err != nil {
		// 数据本身有问题（如价格解析不了）：记 failed，不再卡住整轮
		if e := c.db.MarkDetailResult(ctx, runID, id, "failed", err.Error()); e != nil {
			setErr(e)
			return false
		}
		c.log.Printf("书 %d 清洗失败: %v", id, err)
		c.afterDetail(cfg, processed)
		return false
	}

	written, err := c.db.UpsertBook(ctx, book, cfg.Mode)
	if err != nil {
		setErr(err)
		return false
	}
	if err := c.db.MarkDetailResult(ctx, runID, id, "done", ""); err != nil {
		setErr(err)
		return false
	}

	mu.Lock()
	if written {
		res.DetailsWritten++
	} else {
		res.DetailsUnchanged++
	}
	mu.Unlock()
	c.afterDetail(cfg, processed)
	return false
}

// afterDetail 处理测试用的中断注入；真实运行时 hookCancel 为 nil
func (c *Crawler) afterDetail(cfg Config, processed *int64) {
	if cfg.FailAfterDetails <= 0 || c.hookCancel == nil {
		return
	}
	n := atomic.AddInt64(processed, 1)
	if n >= int64(cfg.FailAfterDetails) {
		c.log.Printf("测试钩子：已处理 %d 本详情，注入中断", n)
		c.hookCancel()
	}
}
