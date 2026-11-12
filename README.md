# 二手书采集器（bookcrawler）

采集本地 mock 二手书站的 Go 程序：纯 HTTP 请求（无浏览器），先翻列表再进详情，
数据清洗后落 SQLite，支持并发、429 退避、断点续采、全量/增量两种模式。

目录结构：

```
crawler/
  cmd/bookcrawler/        # 命令行入口
  internal/
    model/                # 接口与落库的数据结构
    clean/                # 清洗：价格转分、文本去空白、HTML 转纯文本
    client/               # HTTP 客户端：令牌桶限流 + 429/5xx/网络错误退避重试
    storage/              # SQLite：书库、任务队列、断点进度（全部持久化）
    crawl/                # 采集编排 + 集成测试
mockserver/               # 被测的本地二手书站（go run 即可起）
```

## 快速开始

先起 mock 站（在另一个终端）：

```bash
cd mockserver
go run .                       # 默认监听 :18080，首次自动生成 3000 本书
# 模拟数据集变化（18% 的书价格库存变动，随机下架 0~3 本）：
go run . -regenerate
```

再跑采集器：

```bash
# 全量采集
go run ./crawler/cmd/bookcrawler -mode full

# 增量采集：只写内容变化/新上架的书，列表里消失的书标记为下架
go run ./crawler/cmd/bookcrawler -mode incr
```

跑完用 sqlite3 看结果：

```bash
sqlite3 books.db "SELECT id, title, category, price_cents, stock, status FROM books LIMIT 5;"
sqlite3 books.db "SELECT status, COUNT(*) FROM books GROUP BY status;"
```

## 命令行参数

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-url` | `http://127.0.0.1:18080` | mock 站根地址 |
| `-db` | `books.db` | SQLite 数据库文件路径 |
| `-mode` | `full` | `full`=全量重采；`incr`=增量，只更新变化的书 |
| `-concurrency` | `4` | 详情页并发 worker 数 |
| `-rps` | `8` | 客户端每秒最多请求数（服务端限流上限 10，留余量） |
| `-max-retries` | `8` | 单个请求对 429/5xx/网络错误的最大重试次数 |
| `-fresh` | `false` | 忽略未完成的断点，重新开一轮（不清空已采的书） |
| `-v` | `false` | 输出每页/每本书的详细日志 |

## 工作流程

1. **列表阶段（串行）**：从第 1 页翻到响应里的 `total_pages`，串行翻页保证
   不漏页、不重页。每页的书 ID 去重后写入 SQLite 任务队列，翻完一页立即落断点。
2. **详情阶段（并发）**：worker 池从 SQLite 队列原子领取任务（`pending → claimed`
   租约 → `done/not_found/failed`），抓取详情、清洗、落库，再回写任务状态。
3. **收尾**：本次列表里没出现、但库里还标着在架的书，统一标记为 `delisted`。

### 清洗规则

- **价格**：字符串按十进制手工解析成**以分为单位的整数**（`price_cents`），
  不经过 float，杜绝浮点误差；支持 `"45.50"`、`" 45.5 "`、一位/两位小数，
  超过两位按四舍五入。
- **文本**：书名、作者、分类等去掉首尾空白（含全角空格 `U+3000`、tab），
  内部连续空白归一化为单个空格。
- **描述**：剥离 `<p>`、`<br>` 等 HTML 标签（块级标签转换行），反转义
  `&nbsp;` 等实体，库里只存纯文本，不存任何原始 HTML。

### 限流与退避

- 客户端令牌桶自我限流（默认 8 req/s，低于服务端的 10 req/s）。
- 收到 429 时优先按响应头 `Retry-After` 等待；没有该头则指数退避
  （0.5s 起，翻倍，加随机抖动，上限 30s）。5xx 和连接错误同样退避重试。
- 404 不重试（书已下架）；其他 4xx 直接报错。

### 断点续采

所有进度都在 SQLite 里，进程重启不丢：

- 列表页断点（`list_pages`）、详情任务队列（`detail_tasks`）、运行记录
  （`crawl_runs`）全部持久化。
- 任务领取是带 `claimed` 租约的原子操作：worker 处理期间不会被重复领取；
- Ctrl+C / 进程崩溃留下的在途任务，下次启动自动回收为 `pending` 重抓，
  已 `done` 的任务不会重做。
- 直接重跑同一条命令即可接着上次的进度走；加 `-fresh` 可放弃断点重开一轮。

### 全量 vs 增量

每本书清洗后对业务字段算 SHA-256（`content_hash`）：

- `full`：所有在架书无条件重新落库。
- `incr`：哈希没变的书跳过（`updated_at` 也不动），只写变化和新上架的书；
  列表里消失的书标记 `delisted`。详情接口 404 的书同样按下架处理。

### 去重的持久化

- 书的主键就是站点书 ID，`books` 表本身保证跨进程不重复入库。
- 单次运行内，任务队列表对 `(run_id, book_id)` 建主键，列表阶段重复见到
  同一个 ID 会被 `INSERT OR IGNORE` 丢掉，重启后依然知道哪些采过。

## 测试

```bash
go test ./...
go test -race ./...          # 带竞态检测
go test -v ./crawler/internal/crawl/   # 看集成测试明细
```

集成测试用 `httptest` 起实现真实协议的假站，不依赖正在运行的 mockserver，覆盖：

- **翻页**：非整数页数时每页恰好请求一次、全覆盖无遗漏，每本详情只抓一次；
- **退避**：注入连续 429，验证带/不带 `Retry-After` 两种退避路径、重试后成功，
  以及超过重试上限时报错且断点不丢；
- **断点**：详情阶段和列表阶段分别注入中断，验证重启后续采、已完成任务不重做、
  在途任务正确回收、最终全部收敛；
- **增量**：改价/下架后只写变化的书、未变的书不动、下架书正确标记。

## 数据表说明

- `books`：清洗后的书库，`price_cents` 为分单位整数，`status` 为 listed/delisted。
- `crawl_runs`：每次命令行调用一行，记录模式、状态（running/interrupted/done）、总页数。
- `list_pages`：已翻完的列表页断点。
- `detail_tasks`：详情任务队列与去重，状态机 pending → claimed → done/not_found/failed。
