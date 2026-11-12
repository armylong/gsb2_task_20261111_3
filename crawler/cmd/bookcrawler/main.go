package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"gsb2_task_20261111_3/crawler/internal/client"
	"gsb2_task_20261111_3/crawler/internal/crawl"
	"gsb2_task_20261111_3/crawler/internal/storage"
)

func main() {
	fs := flag.NewFlagSet("bookcrawler", flag.ContinueOnError)
	baseURL := fs.String("url", "http://127.0.0.1:18080", "mock 站根地址")
	dbPath := fs.String("db", "books.db", "sqlite 数据库文件路径")
	mode := fs.String("mode", "full", "采集模式：full=全量重采，incr=增量只更新变化")
	concurrency := fs.Int("concurrency", 4, "详情页并发 worker 数")
	rps := fs.Int("rps", 8, "客户端限流：每秒最多请求数（服务端上限 10）")
	maxRetries := fs.Int("max-retries", 8, "单请求最大重试次数（429/5xx/网络错误）")
	fresh := fs.Bool("fresh", false, "忽略未完成的断点，重新开一轮采集（书库数据不清空）")
	verbose := fs.Bool("v", false, "输出每页/每本书的详细日志")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *mode != crawl.ModeFull && *mode != crawl.ModeIncr {
		log.Fatalf("-mode 只支持 full / incr，收到 %q", *mode)
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)
	if !*verbose {
		logger.SetFlags(0)
	}

	// 同时监听 SIGHUP：终端关闭/nohup 失效等场景也走同一条优雅退出路径，
	// 确保在途任务租约和断点都能落库，下次启动自动续采
	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	db, err := storage.Open(ctx, *dbPath)
	if err != nil {
		log.Fatalf("打开数据库 %s 失败: %v", *dbPath, err)
	}
	defer db.Close()

	// 断点续采：找最近一次没跑完的运行。--fresh 时跳过它，新开一轮。
	var resumeRunID int64
	if !*fresh {
		rid, rmode, err := db.LatestRunnableRun(ctx)
		if err != nil {
			log.Fatalf("查询断点失败: %v", err)
		}
		if rid > 0 {
			if rmode != *mode {
				logger.Printf("发现未完成的 %s 模式断点（运行 #%d），与本次 -mode=%s 不一致，新开一轮",
					rmode, rid, *mode)
			} else {
				resumeRunID = rid
				logger.Printf("发现未完成的运行 #%d（%s 模式），从断点继续", rid, rmode)
			}
		}
	}

	cfg := client.DefaultConfig()
	cfg.BaseURL = *baseURL
	cfg.RPS = *rps
	cfg.MaxRetries = *maxRetries
	cli := client.New(cfg)

	c := crawl.New(cli, db, logger)
	res, err := c.Run(ctx, crawl.Config{
		Client:      cfg,
		Mode:        *mode,
		Concurrency: *concurrency,
	}, resumeRunID)

	if res != nil {
		printSummary(logger, db, res, err)
	}

	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			// Ctrl+C：把运行标成 interrupted，进度已在每一步落库，下次自动续
			if res != nil {
				if e := db.InterruptRun(context.Background(), res.RunID); e != nil {
					logger.Printf("标记中断状态失败: %v", e)
				}
			}
			logger.Printf("已中断，断点已保存，重新运行同一条命令即可续采")
			os.Exit(130)
		}
		log.Fatalf("采集失败: %v", err)
	}
	logger.Printf("采集完成")
}

func printSummary(logger *log.Logger, db *storage.DB, res *crawl.Result, runErr error) {
	// 中断场景下运行的 ctx 已取消，统计查询改用独立 context，保证摘要仍能打出来
	stats, err := db.RunStats(context.Background(), res.RunID)
	if err != nil {
		logger.Printf("统计失败: %v", err)
		return
	}
	state := "完成"
	if runErr != nil {
		state = "中断"
	}
	fmt.Fprintln(os.Stderr, "---------- 采集统计 ----------")
	fmt.Fprintf(os.Stderr, "运行 #%d（%s，%s）\n", res.RunID, map[bool]string{true: "续采", false: "新跑"}[res.Resumed], state)
	fmt.Fprintf(os.Stderr, "列表页: %d 页已落库，总页数 %d\n", stats.ListPagesDone, res.TotalPages)
	fmt.Fprintf(os.Stderr, "详情任务: 共 %d，成功 %d，404 下架 %d，失败 %d，待续 %d\n",
		stats.DetailTotal, stats.DetailDone, stats.DetailNotFound, stats.DetailFailed, stats.DetailPending)
	fmt.Fprintf(os.Stderr, "本次写库 %d 本，增量跳过(未变) %d 本，新标记下架 %d 本\n",
		res.DetailsWritten, res.DetailsUnchanged, res.DelistedMarked)
	fmt.Fprintf(os.Stderr, "书库现状: 在架 %d 本，下架 %d 本\n", stats.BooksListed, stats.BooksDelisted)
}
