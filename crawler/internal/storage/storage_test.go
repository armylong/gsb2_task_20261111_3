package storage

import (
	"context"
	"path/filepath"
	"testing"

	"gsb2_task_20261111_3/crawler/internal/model"
)

func probeBook(id int64) *model.Book {
	return &model.Book{
		ID: id, Title: "t", Author: "a", Category: "c", PriceCents: 100,
		Stock: 1, Condition: "x", ISBN: "i", ListedAt: "2026-01-01",
		Description: "d", ContentHash: "h", Status: model.StatusListed,
	}
}

func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func statusCount(t *testing.T, db *DB, status string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(),
		`SELECT COUNT(1) FROM books WHERE status=?`, status).Scan(&n); err != nil {
		t.Fatalf("统计 %s 失败: %v", status, err)
	}
	return n
}

// TestMarkDelisted 回归：分批 NOT IN 曾导致在架书被整批误标下架
func TestMarkDelisted(t *testing.T) {
	ctx := context.Background()

	t.Run("全部在架_超过单批上限也不误标", func(t *testing.T) {
		db := newTestDB(t)
		const n = 3000 // 超过旧实现每批 500 的上限，能触发分批 bug
		ids := make([]int64, 0, n)
		for i := int64(1); i <= n; i++ {
			ids = append(ids, i)
			if _, err := db.UpsertBook(ctx, probeBook(i), "full"); err != nil {
				t.Fatal(err)
			}
		}
		marked, err := db.MarkDelisted(ctx, ids)
		if err != nil {
			t.Fatal(err)
		}
		if marked != 0 {
			t.Errorf("全在架却标记了 %d 本下架，想 0", marked)
		}
		if got := statusCount(t, db, model.StatusListed); got != n {
			t.Errorf("在架 %d，想 %d", got, n)
		}
		if got := statusCount(t, db, model.StatusDelisted); got != 0 {
			t.Errorf("下架 %d，想 0", got)
		}
	})

	t.Run("部分缺席_只标记缺席的", func(t *testing.T) {
		db := newTestDB(t)
		for i := int64(1); i <= 10; i++ {
			if _, err := db.UpsertBook(ctx, probeBook(i), "full"); err != nil {
				t.Fatal(err)
			}
		}
		// 本次列表只见到 1..8，9 和 10 缺席应被标记
		listed := []int64{1, 2, 3, 4, 5, 6, 7, 8}
		marked, err := db.MarkDelisted(ctx, listed)
		if err != nil {
			t.Fatal(err)
		}
		if marked != 2 {
			t.Errorf("标记下架 %d 本，想 2", marked)
		}
		if got := statusCount(t, db, model.StatusListed); got != 8 {
			t.Errorf("在架 %d，想 8", got)
		}
		if got := statusCount(t, db, model.StatusDelisted); got != 2 {
			t.Errorf("下架 %d，想 2", got)
		}
	})

	t.Run("空集合_全部下架", func(t *testing.T) {
		db := newTestDB(t)
		for i := int64(1); i <= 3; i++ {
			if _, err := db.UpsertBook(ctx, probeBook(i), "full"); err != nil {
				t.Fatal(err)
			}
		}
		marked, err := db.MarkDelisted(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if marked != 3 {
			t.Errorf("空在架集合应标记 3 本，实际 %d", marked)
		}
	})

	t.Run("已下架的不重复更新", func(t *testing.T) {
		db := newTestDB(t)
		b := probeBook(1)
		b.Status = model.StatusDelisted
		if _, err := db.UpsertBook(ctx, b, "full"); err != nil {
			t.Fatal(err)
		}
		// 列表里没有 id=1，但它已经是 delisted，标记数应为 0
		marked, err := db.MarkDelisted(ctx, []int64{999})
		if err != nil {
			t.Fatal(err)
		}
		if marked != 0 {
			t.Errorf("已下架的书不应重复标记，实际 %d", marked)
		}
	})
}

// TestLeaseRequeue 验证 claimed 租约回收：中断在途任务退回 pending
func TestLeaseRequeue(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	rid, err := db.StartRun(ctx, "full")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnqueueAndMarkPage(ctx, rid, 1, []int64{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	// 领走两个，模拟在途
	if _, ok, err := db.ClaimDetailTask(ctx, rid); err != nil || !ok {
		t.Fatalf("领取 1 失败 ok=%v err=%v", ok, err)
	}
	if _, ok, err := db.ClaimDetailTask(ctx, rid); err != nil || !ok {
		t.Fatalf("领取 2 失败 ok=%v err=%v", ok, err)
	}
	pending, _ := db.PendingDetailCount(ctx, rid)
	if pending != 3 { // pending 1 + claimed 2
		t.Fatalf("领取后未完成任务 %d，想 3", pending)
	}
	if err := db.RequeueClaimed(ctx, rid); err != nil {
		t.Fatal(err)
	}
	pending, _ = db.PendingDetailCount(ctx, rid)
	if pending != 3 {
		t.Errorf("回收后未完成任务 %d，想 3（全部退回 pending）", pending)
	}
	// 再领应该能领到最早的 id=1
	id, ok, err := db.ClaimDetailTask(ctx, rid)
	if err != nil || !ok || id != 1 {
		t.Errorf("回收后领取 id=%d ok=%v err=%v，想 id=1", id, ok, err)
	}
}
