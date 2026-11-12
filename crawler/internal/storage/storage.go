package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"

	"gsb2_task_20261111_3/crawler/internal/model"
)

// DB 封装采集端用到的全部持久化逻辑。
// 进度和去重都在这一个 sqlite 文件里，进程重启后状态不丢。
type DB struct {
	db *sql.DB
}

// Open 打开（或新建）采集数据库，并确保表结构就绪
func Open(ctx context.Context, path string) (*DB, error) {
	// _pragma 走 DSN 配置：忙等 10s、WAL、外键打开
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)", path)
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite 同一时刻只允许一个写事务，多连接并发写会随机 SQLITE_BUSY。
	// 采集器写远多于读，直接把连接池收成 1，所有操作在单连接上排队；
	// 单条 SQL 都是毫秒级，3000 本的量级下吞吐瓶颈在网络而不在数据库。
	sqldb.SetMaxOpenConns(1)
	if err := sqldb.PingContext(ctx); err != nil {
		sqldb.Close()
		return nil, err
	}
	d := &DB{db: sqldb}
	if err := d.migrate(ctx); err != nil {
		sqldb.Close()
		return nil, err
	}
	return d, nil
}

func (d *DB) Close() error { return d.db.Close() }

// QueryRow 暴露只读单行查询，供工具/测试核对库内数据，业务写入仍走封装方法
func (d *DB) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return d.db.QueryRowContext(ctx, query, args...)
}

func (d *DB) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS books (
			id           INTEGER PRIMARY KEY,
			title        TEXT NOT NULL,
			author       TEXT NOT NULL,
			category     TEXT NOT NULL,
			price_cents  INTEGER NOT NULL,
			stock        INTEGER NOT NULL,
			condition     TEXT NOT NULL,
			isbn         TEXT NOT NULL,
			listed_at    TEXT NOT NULL,
			description  TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			status       TEXT NOT NULL DEFAULT 'listed',
			created_at   TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		// 采集运行（一次命令行调用对应一行），用来存断点和最终统计
		`CREATE TABLE IF NOT EXISTS crawl_runs (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			mode        TEXT NOT NULL,
			status      TEXT NOT NULL,          -- running / done / interrupted
			total_pages INTEGER NOT NULL DEFAULT 0,
			started_at  TEXT NOT NULL DEFAULT (datetime('now')),
			finished_at TEXT
		)`,
		// 列表页级断点：哪几页已经采完
		`CREATE TABLE IF NOT EXISTS list_pages (
			run_id   INTEGER NOT NULL,
			page     INTEGER NOT NULL,
			PRIMARY KEY (run_id, page),
			FOREIGN KEY (run_id) REFERENCES crawl_runs(id)
		)`,
		// 详情任务队列 + 去重：同一次运行里每个 ID 只入队一次；
		// 跨进程的"采没采过"由 books 表本身承担（主键即书 ID）。
		// status: pending 待领取 / claimed 已被某个 worker 领走（在途）
		//         / done 成功 / not_found 详情404 / failed 数据或重试耗尽
		`CREATE TABLE IF NOT EXISTS detail_tasks (
			run_id       INTEGER NOT NULL,
			book_id      INTEGER NOT NULL,
			status       TEXT NOT NULL DEFAULT 'pending',
			attempts     INTEGER NOT NULL DEFAULT 0,
			last_error   TEXT,
			updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (run_id, book_id),
			FOREIGN KEY (run_id) REFERENCES crawl_runs(id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_detail_tasks_status ON detail_tasks(run_id, status)`,
	}
	for _, s := range stmts {
		if _, err := d.db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("建表失败: %w", err)
		}
	}
	return nil
}

// StartRun 新建一次采集运行
func (d *DB) StartRun(ctx context.Context, mode string) (int64, error) {
	res, err := d.db.ExecContext(ctx,
		`INSERT INTO crawl_runs(mode, status) VALUES (?, 'running')`, mode)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ResetRun 清空一次运行的断点和任务队列（--fresh 时用，books 不动）
func (d *DB) ResetRun(ctx context.Context, runID int64) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM list_pages WHERE run_id = ?`, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM detail_tasks WHERE run_id = ?`, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE crawl_runs SET status='running', total_pages=0, finished_at=NULL WHERE id=?`, runID); err != nil {
		return err
	}
	return tx.Commit()
}

// LatestRunnableRun 找最近一次没跑完的运行，用于断点续采。
// running（还在跑/进程被杀）和 interrupted（收到信号主动退出）都可续。
// 没有可续的运行时返回 (0, nil)。
func (d *DB) LatestRunnableRun(ctx context.Context) (runID int64, mode string, err error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT id, mode FROM crawl_runs WHERE status IN ('running', 'interrupted')
		 ORDER BY id DESC LIMIT 1`)
	if err := row.Scan(&runID, &mode); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, "", nil
		}
		return 0, "", err
	}
	return runID, mode, nil
}

// RunFinished 判断一次运行是否已经正常跑完
func (d *DB) RunFinished(ctx context.Context, runID int64) (bool, error) {
	var status string
	err := d.db.QueryRowContext(ctx,
		`SELECT status FROM crawl_runs WHERE id=?`, runID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return status == "done", err
}

// SetTotalPages 记录列表总页数
func (d *DB) SetTotalPages(ctx context.Context, runID int64, total int) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE crawl_runs SET total_pages=? WHERE id=?`, total, runID)
	return err
}

// IsListPageDone 判断某列表页是否已完成（断点续采用来跳过）
func (d *DB) IsListPageDone(ctx context.Context, runID int64, page int) (bool, error) {
	var n int
	err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM list_pages WHERE run_id=? AND page=?`, runID, page).Scan(&n)
	return n > 0, err
}

// EnqueueAndMarkPage 在一个事务里完成"详情任务入队 + 列表页标记完成"，
// 保证崩溃时两步要么都生效要么都不生效，重跑该页不会产生状态不一致。
// 已入队过的 ID 靠 INSERT OR IGNORE 自动跳过。
func (d *DB) EnqueueAndMarkPage(ctx context.Context, runID int64, page int, bookIDs []int64) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if len(bookIDs) > 0 {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT OR IGNORE INTO detail_tasks(run_id, book_id, status) VALUES (?, ?, 'pending')`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, id := range bookIDs {
			if _, err := stmt.ExecContext(ctx, runID, id); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO list_pages(run_id, page) VALUES (?, ?)`, runID, page); err != nil {
		return err
	}
	return tx.Commit()
}

// ClaimDetailTask 原子领取一个待办详情任务并标记为 claimed（在途租约），
// 多个 worker 并发调用不会领到同一行。没有任务时返回 ok=false。
// claimed 让任务在 worker 处理期间不会被重复领取；进程崩溃留下的 claimed
// 行在下次运行开始时由 RequeueClaimed 回收。
func (d *DB) ClaimDetailTask(ctx context.Context, runID int64) (bookID int64, ok bool, err error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx,
		`SELECT book_id FROM detail_tasks
		 WHERE run_id=? AND status='pending'
		 ORDER BY book_id LIMIT 1`, runID)
	if err := row.Scan(&bookID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE detail_tasks
		 SET status='claimed', attempts=attempts+1, updated_at=datetime('now')
		 WHERE run_id=? AND book_id=? AND status='pending'`, runID, bookID); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return bookID, true, nil
}

// RequeueClaimed 把上次中断时还在途（claimed）的任务退回 pending，
// 必须在每次运行开始领取详情任务前调用。
func (d *DB) RequeueClaimed(ctx context.Context, runID int64) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE detail_tasks SET status='pending', updated_at=datetime('now')
		 WHERE run_id=? AND status='claimed'`, runID)
	return err
}

// MarkDetailResult 回写详情任务结果（done / not_found / failed / pending 退回）
func (d *DB) MarkDetailResult(ctx context.Context, runID int64, bookID int64, status, lastErr string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE detail_tasks SET status=?, last_error=?, updated_at=datetime('now')
		 WHERE run_id=? AND book_id=?`, status, lastErr, runID, bookID)
	return err
}

// ListTaskIDs 返回本次运行已入队的全部书 ID（即列表阶段见过的完整集合）
func (d *DB) ListTaskIDs(ctx context.Context, runID int64) ([]int64, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT book_id FROM detail_tasks WHERE run_id=? ORDER BY book_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RunTotalPages 读取运行记录里存的列表总页数
func (d *DB) RunTotalPages(ctx context.Context, runID int64) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx,
		`SELECT total_pages FROM crawl_runs WHERE id=?`, runID).Scan(&n)
	return n, err
}

// PendingDetailCount 返回还没处理完的详情任务数（pending 和在途 claimed 都算）
func (d *DB) PendingDetailCount(ctx context.Context, runID int64) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM detail_tasks
		 WHERE run_id=? AND status IN ('pending','claimed')`, runID).Scan(&n)
	return n, err
}

// TaskStatus 查单个详情任务的当前状态，任务不存在返回空串
func (d *DB) TaskStatus(ctx context.Context, runID int64, bookID int64) (string, error) {
	var s string
	err := d.db.QueryRowContext(ctx,
		`SELECT status FROM detail_tasks WHERE run_id=? AND book_id=?`, runID, bookID).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return s, err
}

// UpsertBook 落一本清洗后的书。
// full 模式无条件写；incr 模式只有 content_hash 变化（或新书）才写，没变的书一行不动。
// 返回 true 表示实际发生了写入。
func (d *DB) UpsertBook(ctx context.Context, b *model.Book, mode string) (bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var oldHash sql.NullString
	row := tx.QueryRowContext(ctx,
		`SELECT content_hash FROM books WHERE id=?`, b.ID)
	switch err := row.Scan(&oldHash); {
	case errors.Is(err, sql.ErrNoRows):
		// 新书，落库
	case err != nil:
		return false, err
	default:
		if mode == "incr" && oldHash.Valid && oldHash.String == b.ContentHash {
			return false, nil // 内容没变，增量模式不动它
		}
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO books(id, title, author, category, price_cents, stock, condition,
		                  isbn, listed_at, description, content_hash, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))
		ON CONFLICT(id) DO UPDATE SET
			title=excluded.title, author=excluded.author, category=excluded.category,
			price_cents=excluded.price_cents, stock=excluded.stock, condition=excluded.condition,
			isbn=excluded.isbn, listed_at=excluded.listed_at, description=excluded.description,
			content_hash=excluded.content_hash, status=excluded.status, updated_at=datetime('now')`,
		b.ID, b.Title, b.Author, b.Category, b.PriceCents, b.Stock, b.Condition,
		b.ISBN, b.ListedAt, b.Description, b.ContentHash, b.Status)
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// MarkDelisted 把本次列表里没出现、但库里还标着 listed 的书标记为下架。
// 返回被标记的数量。
//
// 实现要点：不能对 listedIDs 分批拼 NOT IN——每批 UPDATE 会把"不在这一批里"
// 的所有在架书（含其它批）都标记下架，多批互相覆盖会导致全量误删。
// 这里把本次在架 ID 写进事务内临时表，用一条 NOT IN 子查询做集合差，
// 既不受 sqlite 999 变量上限影响，也不会分批互相误伤。
func (d *DB) MarkDelisted(ctx context.Context, listedIDs []int64) (int64, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if len(listedIDs) == 0 {
		res, err := tx.ExecContext(ctx,
			`UPDATE books SET status='delisted', updated_at=datetime('now') WHERE status='listed'`)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		return n, tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_listed(id INTEGER PRIMARY KEY)`); err != nil {
		return 0, err
	}
	// 临时表只在当前连接可见，先清掉上一次可能残留的数据
	if _, err := tx.ExecContext(ctx, `DELETE FROM tmp_listed`); err != nil {
		return 0, err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO tmp_listed(id) VALUES (?)`)
	if err != nil {
		return 0, err
	}
	for _, id := range listedIDs {
		if _, err := stmt.ExecContext(ctx, id); err != nil {
			stmt.Close()
			return 0, err
		}
	}
	stmt.Close()

	res, err := tx.ExecContext(ctx, `
		UPDATE books SET status='delisted', updated_at=datetime('now')
		WHERE status='listed' AND id NOT IN (SELECT id FROM tmp_listed)`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM tmp_listed`); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// FinishRun 把运行标记成正常完成
func (d *DB) FinishRun(ctx context.Context, runID int64) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE crawl_runs SET status='done', finished_at=datetime('now') WHERE id=?`, runID)
	return err
}

// InterruptRun 把没跑完的运行标记为 interrupted（Ctrl+C 时调用）
func (d *DB) InterruptRun(ctx context.Context, runID int64) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE crawl_runs SET status='interrupted', finished_at=datetime('now')
		 WHERE id=? AND status='running'`, runID)
	return err
}

// Stats 是一次运行的统计数字
type Stats struct {
	ListPagesDone  int
	DetailTotal    int
	DetailDone     int
	DetailNotFound int
	DetailFailed   int
	DetailPending  int
	BooksListed    int
	BooksDelisted  int
}

// RunStats 汇总运行进度和书库现状
func (d *DB) RunStats(ctx context.Context, runID int64) (*Stats, error) {
	s := &Stats{}
	err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM list_pages WHERE run_id=?`, runID).Scan(&s.ListPagesDone)
	if err != nil {
		return nil, err
	}
	err = d.db.QueryRowContext(ctx,
		`SELECT
			COUNT(1),
			SUM(CASE WHEN status='done' THEN 1 ELSE 0 END),
			SUM(CASE WHEN status='not_found' THEN 1 ELSE 0 END),
			SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END),
			SUM(CASE WHEN status IN ('pending','claimed') THEN 1 ELSE 0 END)
		 FROM detail_tasks WHERE run_id=?`, runID).
		Scan(&s.DetailTotal, &s.DetailDone, &s.DetailNotFound, &s.DetailFailed, &s.DetailPending)
	if err != nil {
		return nil, err
	}
	if err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM books WHERE status='listed'`).Scan(&s.BooksListed); err != nil {
		return nil, err
	}
	if err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM books WHERE status='delisted'`).Scan(&s.BooksDelisted); err != nil {
		return nil, err
	}
	return s, nil
}
