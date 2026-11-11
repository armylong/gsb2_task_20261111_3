package main

import (
	"encoding/json"
	"flag"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const booksPerPage = 20

// 列表只给一部分字段，详情才给全，所以得先采列表再进详情

type listItem struct {
	ID       int    `json:"id"`
	Title    string `json:"title"`
	Author   string `json:"author"`
	Category string `json:"category"`
	Price    string `json:"price"`
}

type detailItem struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	Author      string `json:"author"`
	Category    string `json:"category"`
	Price       string `json:"price"`
	Stock       int    `json:"stock"`
	Condition   string `json:"condition"`
	ISBN        string `json:"isbn"`
	ListedAt    string `json:"listed_at"`
	Description string `json:"description"`
}

// limiter 是个滑动窗口限流器，一秒窗口内最多放 n 个请求
type limiter struct {
	mu  sync.Mutex
	at  []time.Time
	n   int
	win time.Duration
}

func (l *limiter) allow() bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	i := 0
	for ; i < len(l.at) && now.Sub(l.at[i]) >= l.win; i++ {
	}
	l.at = l.at[i:]
	if len(l.at) >= l.n {
		return false
	}
	l.at = append(l.at, now)
	return true
}

type server struct {
	st  *store
	lim *limiter
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/books", s.handleList)
	mux.HandleFunc("/api/books/", s.handleDetail)
	mux.HandleFunc("/", s.handleIndex)
	return s.logReq(mux)
}

// logReq 给每个请求记一行日志，429 也一并记上
func (s *server) logReq(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		if !s.lim.allow() {
			sw.Header().Set("Retry-After", "1")
			writeJSON(sw, http.StatusTooManyRequests, map[string]string{"error": "too many requests"})
		} else {
			next.ServeHTTP(sw, r)
		}
		log.Printf("%s %s %d %s", r.Method, r.URL.RequestURI(), sw.code, time.Since(start).Round(time.Microsecond))
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

const indexHTML = `<!doctype html>
<html>
<head><meta charset="utf-8"><title>旧书小站</title></head>
<body>
<h1>旧书小站</h1>
<p>一个卖二手书的小站，书都在 <a href="/api/books?page=1">这里</a>。</p>
</body>
</html>
`

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(indexHTML))
}

func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	listed := make([]*Book, 0, len(s.st.Books))
	for _, b := range s.st.Books {
		if !b.Delisted {
			listed = append(listed, b)
		}
	}
	total := len(listed)
	totalPages := (total + booksPerPage - 1) / booksPerPage

	start := (page - 1) * booksPerPage
	if start < 0 || start > total {
		start = total
	}
	end := start + booksPerPage
	if end > total {
		end = total
	}

	items := make([]listItem, 0, end-start)
	for _, b := range listed[start:end] {
		items = append(items, listItem{ID: b.ID, Title: b.Title, Author: b.Author, Category: b.Category, Price: b.Price})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":       total,
		"page":        page,
		"page_size":   booksPerPage,
		"total_pages": totalPages,
		"items":       items,
	})
}

func (s *server) handleDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/api/books/")
	id, err := strconv.Atoi(idStr)
	if err != nil || id < 1 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "book not found"})
		return
	}
	for _, b := range s.st.Books {
		if b.ID == id {
			if b.Delisted {
				break
			}
			writeJSON(w, http.StatusOK, detailItem{
				ID:          b.ID,
				Title:       b.Title,
				Author:      b.Author,
				Category:    b.Category,
				Price:       b.Price,
				Stock:       b.Stock,
				Condition:   b.Condition,
				ISBN:        b.ISBN,
				ListedAt:    b.ListedAt,
				Description: b.Description,
			})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "book not found"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func main() {
	addr := flag.String("addr", ":18080", "监听地址")
	dataFile := flag.String("data", "data.json", "数据文件路径，默认当前目录下的 data.json")
	regen := flag.Bool("regenerate", false, "重新生成一份数据集（价格库存会变，偶尔下架几本），写回数据文件后退出")
	flag.Parse()

	st, src, err := loadStore(*dataFile)
	if err != nil {
		log.Fatalf("读数据失败: %v", err)
	}
	listed, total := countListed(st)

	if *regen {
		mutate(st, rand.New(rand.NewSource(time.Now().UnixNano())))
		if err := saveStore(st, *dataFile); err != nil {
			log.Fatalf("写 %s 失败: %v", *dataFile, err)
		}
		listed, _ = countListed(st)
		log.Printf("数据集重新生成完毕，写回 %s：在架 %d 本，下架 %d 本", *dataFile, listed, total-listed)
		return
	}

	log.Printf("在架 %d 本，下架 %d 本（数据来源: %s）", listed, total-listed, src)
	log.Printf("监听 %s，试试 http://127.0.0.1%s", *addr, *addr)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           (&server{st: st, lim: &limiter{n: 10, win: time.Second}}).handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
