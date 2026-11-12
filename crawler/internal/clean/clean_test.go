package clean

import (
	"strings"
	"testing"
)

func TestPrice(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"45.50", 4550},
		{" 45.5 ", 4550},
		{"100", 10000},
		{"0.01", 1},
		{"　12.34　", 1234}, // 全角空格
		{"9.999", 1000},   // 第三位 9，四舍五入
		{"9.994", 999},    // 第三位 4，舍
		{".5", 50},
		{"12.", 1200},
		{"-3.25", -325},
		{"+7.70", 770},
	}
	for _, tc := range cases {
		got, err := Price(tc.in)
		if err != nil {
			t.Errorf("Price(%q) 意外报错: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Price(%q) = %d, 想 %d", tc.in, got, tc.want)
		}
	}
}

func TestPriceErrors(t *testing.T) {
	for _, in := range []string{"", "abc", "12.3.4", "1a.2"} {
		if _, err := Price(in); err == nil {
			t.Errorf("Price(%q) 应该报错却没有", in)
		}
	}
}

func TestText(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"  深入浅出 MySQL　", "深入浅出 MySQL"},
		{"图解\tTCP/IP\n", "图解 TCP/IP"},
		{"红楼梦   （修订本）", "红楼梦 （修订本）"},
		{"　", ""},
		{"正常书名", "正常书名"},
	}
	for _, tc := range cases {
		if got := Text(tc.in); got != tc.want {
			t.Errorf("Text(%q) = %q, 想 %q", tc.in, got, tc.want)
		}
	}
}

func TestHTMLToText(t *testing.T) {
	in := `<p>九成新，无笔记无划线</p><p>书角有些磕碰&nbsp;不影响阅读</p><br>拍照为准，售出不退`
	got := HTMLToText(in)

	wantLines := []string{
		"九成新，无笔记无划线",
		"书角有些磕碰 不影响阅读",
		"拍照为准，售出不退",
	}
	want := strings.Join(wantLines, "\n")
	if got != want {
		t.Errorf("HTMLToText 结果不对：\n got=%q\nwant=%q", got, want)
	}
	if strings.ContainsAny(got, "<>&") {
		t.Errorf("结果里还残留 HTML 痕迹: %q", got)
	}

	// 没标签的纯文本也要正常工作
	if g := HTMLToText("  普通描述  "); g != "普通描述" {
		t.Errorf("纯文本清洗不对: %q", g)
	}
}
