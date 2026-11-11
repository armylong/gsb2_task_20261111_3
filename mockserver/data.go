package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	bookCount = 3000     // 初始的书的数量
	genSeed   = 20261111 // 固定种子，保证每次生成的数据一模一样
)

// Book 是一本书。delisted 的书不出现在列表里，详情也返回 404
type Book struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	Author      string `json:"author"`
	Category    string `json:"category"`
	Price       string `json:"price"` // 字符串，可能是 "45.50" 也可能是 " 45.5 " 这种
	Stock       int    `json:"stock"`
	Condition   string `json:"condition"`
	ISBN        string `json:"isbn"`
	ListedAt    string `json:"listed_at"`
	Description string `json:"description"`
	Delisted    bool   `json:"delisted"`
}

type store struct {
	Books []*Book // 按 ID 升序
}

var titleFronts = []string{"图解", "深入浅出", "手把手教你", "零基础学", "细说", "读懂", "看漫画学", "大话", "重学", "一本书搞懂", "轻松掌握", "从零开始"}

var titleMiddles = []string{
	"TCP/IP", "MySQL", "Python", "Linux", "算法", "数据结构", "Excel", "摄影", "烘焙", "咖啡",
	"围棋", "钓鱼", "养猫", "养多肉", "收纳", "马拉松", "瑜伽", "红酒", "唐诗", "宋词",
	"红楼梦", "三国", "明清家具", "书法", "国画", "天文", "昆虫", "植物", "中医", "经济学",
}

var titleEnds = []string{"", "（第2版）", "（修订本）", "实战", "入门", "笔记", "指南", "问答集", "案例精讲", "从入门到精通", "学习手册"}

var surnames = []string{"王", "李", "张", "刘", "陈", "杨", "赵", "黄", "周", "吴", "徐", "孙", "马", "朱", "胡", "郭", "何", "林"}

var givens = []string{"伟", "芳", "娜", "敏", "静", "磊", "军", "洋", "勇", "艳", "杰", "涛", "明", "超", "霞", "平", "刚", "秀英", "文轩", "子豪", "雨桐", "思远", "晓峰", "建国", "国庆", "志强", "海燕", "桂英"}

var categories = []string{"计算机", "文学", "小说", "少儿", "历史", "经管", "生活", "艺术", "教育", "科技"}

var conditions = []string{"全新", "九五新", "九成新", "八成新", "七成新", "有明显使用痕迹"}

// 前后塞的空白，全角空格也混了一个进去
var dirtyPads = []string{" ", "  ", "\t", "　"}

var descPlain = []string{
	"自己看过一遍，保存得不错，介意品相的慎拍",
	"家里收拾出来的一批书，这本基本没翻过",
	"有少量笔记，不影响阅读，急出",
	"品相看图，按图发货",
	"囤多了出一本，全新没拆封",
	"搬家出书，都是自己的藏书，无划线",
}

var descHTML = []string{
	"九成新，无笔记无划线",
	"书脊有轻微磨损",
	"书角有些磕碰&nbsp;不影响阅读",
	"内页干净，无涂写",
	"封面有点旧了，内页很新",
	"正版，纸张微微发黄",
}

// generate 用固定种子现场生成一份初始数据
func generate() *store {
	rng := rand.New(rand.NewSource(genSeed))

	// 书名用 前缀x中间x后缀 组合出来，先洗牌再取前 3000 本，保证不重名
	type combo struct{ f, m, e int }
	combos := make([]combo, 0, len(titleFronts)*len(titleMiddles)*len(titleEnds))
	for f := range titleFronts {
		for m := range titleMiddles {
			for e := range titleEnds {
				combos = append(combos, combo{f, m, e})
			}
		}
	}
	rng.Shuffle(len(combos), func(i, j int) { combos[i], combos[j] = combos[j], combos[i] })

	// 上架日期以一个固定日期为基准往回数，这样种子一样日期也一样
	anchor := time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local)

	books := make([]*Book, 0, bookCount)
	for i := 0; i < bookCount; i++ {
		c := combos[i]
		b := &Book{ID: i + 1}
		b.Title = maybeDirty(rng, titleFronts[c.f]+titleMiddles[c.m]+titleEnds[c.e])
		b.Author = surnames[rng.Intn(len(surnames))] + givens[rng.Intn(len(givens))]
		b.Category = maybeDirty(rng, categories[rng.Intn(len(categories))])
		b.Price = makePrice(rng, 5, 120)
		b.Stock = rng.Intn(21)
		b.Condition = conditions[rng.Intn(len(conditions))]
		b.ISBN = fmt.Sprintf("978%010d", rng.Int63n(10000000000))
		b.ListedAt = anchor.AddDate(0, 0, -rng.Intn(730)).Format("2006-01-02")
		b.Description = makeDesc(rng)
		books = append(books, b)
	}
	return &store{Books: books}
}

// mutate 模拟"重新生成数据集"：在架的书价格库存会变，偶尔下架几本
func mutate(st *store, rng *rand.Rand) {
	for _, b := range st.Books {
		if b.Delisted {
			continue
		}
		if rng.Intn(100) < 18 {
			// 价格在原价基础上浮动一点
			old, _ := strconv.ParseFloat(strings.TrimSpace(b.Price), 64)
			p := old * (0.75 + rng.Float64()*0.5) // 0.75 ~ 1.25 倍
			if p < 2 {
				p = 2
			}
			if p > 300 {
				p = 300
			}
			b.Price = formatPrice(rng, p)
			// 库存也跟着动一动
			b.Stock = rng.Intn(21)
		}
	}

	// 偶尔下架几本
	n := rng.Intn(4) // 0~3 本
	if n > 0 {
		listed := make([]*Book, 0, len(st.Books))
		for _, b := range st.Books {
			if !b.Delisted {
				listed = append(listed, b)
			}
		}
		rng.Shuffle(len(listed), func(i, j int) { listed[i], listed[j] = listed[j], listed[i] })
		for i := 0; i < n && i < len(listed); i++ {
			listed[i].Delisted = true
		}
	}
}

// loadStore 从数据文件读数据，文件不存在就现场生成一份
func loadStore(path string) (*store, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return generate(), "内置生成（固定种子）", nil
		}
		return nil, "", err
	}
	var books []*Book
	if err := json.Unmarshal(data, &books); err != nil {
		return nil, "", fmt.Errorf("%s 坏了（%w），删掉它再启动会重新生成", path, err)
	}
	return &store{Books: books}, path, nil
}

// saveStore 把数据写回数据文件
func saveStore(st *store, path string) error {
	data, err := json.MarshalIndent(st.Books, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// countListed 数一下在架和总数
func countListed(st *store) (listed, total int) {
	for _, b := range st.Books {
		total++
		if !b.Delisted {
			listed++
		}
	}
	return
}

// maybeDirty 有两成概率在文本前后塞点空白
func maybeDirty(rng *rand.Rand, s string) string {
	if rng.Intn(100) >= 18 {
		return s
	}
	return dirtyPads[rng.Intn(len(dirtyPads))] + s + dirtyPads[rng.Intn(len(dirtyPads))]
}

// makePrice 生成 [lo, hi] 区间里的价格字符串，格式故意不统一
func makePrice(rng *rand.Rand, lo, hi float64) string {
	return formatPrice(rng, lo+rng.Float64()*(hi-lo))
}

func formatPrice(rng *rand.Rand, p float64) string {
	var s string
	if rng.Intn(100) < 15 {
		s = strconv.FormatFloat(p, 'f', 1, 64)
	} else {
		s = strconv.FormatFloat(p, 'f', 2, 64)
	}
	if rng.Intn(100) < 10 {
		s = " " + s + " "
	}
	return s
}

// makeDesc 一半概率是纯文本，一半概率是混着 html 标签的
func makeDesc(rng *rand.Rand) string {
	if rng.Intn(100) < 40 {
		return descPlain[rng.Intn(len(descPlain))]
	}
	n := 2 + rng.Intn(2) // 2~3 段
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		parts = append(parts, descHTML[rng.Intn(len(descHTML))])
	}
	s := "<p>" + strings.Join(parts, "</p><p>") + "</p>"
	if rng.Intn(100) < 30 {
		s += "<br>拍照为准，售出不退"
	}
	return s
}
