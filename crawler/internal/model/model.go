package model

// RawListItem 是列表接口返回的单条数据（字段不全，详情才给全）
type RawListItem struct {
	ID       int    `json:"id"`
	Title    string `json:"title"`
	Author   string `json:"author"`
	Category string `json:"category"`
	Price    string `json:"price"`
}

// ListResponse 是 /api/books 的响应
type ListResponse struct {
	Total      int           `json:"total"`
	Page       int           `json:"page"`
	PageSize   int           `json:"page_size"`
	TotalPages int           `json:"total_pages"`
	Items      []RawListItem `json:"items"`
}

// RawDetail 是 /api/books/{id} 的详情响应
type RawDetail struct {
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

// Book 是清洗后落库的结构，不包含任何原始 HTML
type Book struct {
	ID          int64
	Title       string
	Author      string
	Category    string
	PriceCents  int64 // 价格统一按分存整数
	Stock       int
	Condition   string
	ISBN        string
	ListedAt    string
	Description string // 已去掉 HTML 标签、转义实体后的纯文本
	ContentHash string // 清洗后字段的哈希，增量模式靠它判断"变没变"
	Status      string // listed / delisted
}

// 书的在架状态
const (
	StatusListed   = "listed"
	StatusDelisted = "delisted"
)
