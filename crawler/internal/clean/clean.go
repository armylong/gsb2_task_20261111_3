package clean

import (
	"html"
	"strconv"
	"strings"
	"unicode"
)

// trimSpace 去掉字符串首尾空白，全角空格 U+3000 也算空白
func trimSpace(s string) string {
	return strings.TrimFunc(s, unicode.IsSpace)
}

// collapseSpace 把内部连续空白（含全角空格、tab、换行）压成单个半角空格
func collapseSpace(s string) string {
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}

// Text 清洗普通文本字段：去首尾空白，内部连续空白归一化。
// 书名、作者、分类这些字段不允许换行，统一压成单行。
func Text(s string) string {
	return collapseSpace(trimSpace(s))
}

// parseDecimal 按十进制字符串手工解析，避免 float64 的浮点误差。
// 支持整数、小数（如 "45.50"、" 45.5 "），负数也兼容。
// 返回 元*100 的分值，按四舍五入处理超过两位的小数。
func parseDecimal(s string) (int64, error) {
	raw := s
	s = trimSpace(s)
	if s == "" {
		return 0, &PriceError{Input: raw}
	}
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}

	var whole, frac string
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		whole, frac = s[:dot], s[dot+1:]
	} else {
		whole = s
	}
	if whole == "" {
		whole = "0"
	}
	if _, err := strconv.Atoi(whole); err != nil {
		return 0, &PriceError{Input: s}
	}
	for _, c := range frac {
		if c < '0' || c > '9' {
			return 0, &PriceError{Input: s}
		}
	}

	// 补齐到两位，第三位用来四舍五入
	var round int64
	switch {
	case len(frac) >= 3:
		if frac[2] >= '5' {
			round = 1
		}
		frac = frac[:2]
	case len(frac) == 1:
		frac += "0"
	case len(frac) == 0:
		frac = "00"
	}

	wholeN, _ := strconv.ParseInt(whole, 10, 64)
	fracN, _ := strconv.ParseInt(frac, 10, 64)
	cents := wholeN*100 + fracN + round
	if neg {
		cents = -cents
	}
	return cents, nil
}

// PriceError 表示价格字符串无法解析
type PriceError struct {
	Input string
}

func (e *PriceError) Error() string {
	return "无法解析的价格字符串: " + e.Input
}

// Price 把接口给的价格字符串转成以分为单位的整数
func Price(s string) (int64, error) {
	return parseDecimal(s)
}

// blockTags 是渲染时需要断行的标签
var blockTags = map[string]bool{
	"p": true, "div": true, "li": true, "tr": true,
	"h1": true, "h2": true, "h3": true, "h4": true,
}

// stripHTML 把混在描述里的简单 HTML 转成纯文本：
// 块级标签和 <br> 转换行，其余标签直接去掉，HTML 实体（如 &nbsp;）反转义。
func stripHTML(s string) string {
	var b strings.Builder
	var tagBuf strings.Builder
	inTag := false

	flush := func() {
		tag := strings.ToLower(strings.TrimFunc(tagBuf.String(), unicode.IsSpace))
		tagBuf.Reset()
		tag = strings.TrimPrefix(tag, "/")
		tag = strings.Trim(tag, "/")
		// 去掉属性，只留标签名
		if sp := strings.IndexFunc(tag, unicode.IsSpace); sp >= 0 {
			tag = tag[:sp]
		}
		if tag == "br" || blockTags[tag] {
			b.WriteByte('\n')
		}
	}

	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
			tagBuf.Reset()
		case r == '>' && inTag:
			inTag = false
			flush()
		case inTag:
			tagBuf.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	// 标签没闭合（脏数据兜底），标签内容直接丢弃
	return html.UnescapeString(b.String())
}

// HTMLToText 清洗描述字段：去 HTML 标签和实体，每行做空白归一化，去掉空行。
// 返回的是不含任何 HTML 的纯文本。
func HTMLToText(s string) string {
	plain := stripHTML(s)
	lines := strings.Split(plain, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if t := collapseSpace(trimSpace(line)); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, "\n")
}
