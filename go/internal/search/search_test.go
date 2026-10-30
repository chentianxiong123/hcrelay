package search

import (
	"encoding/base64"
	"testing"
)

func TestBingRealURL(t *testing.T) {
	// 正常 URL 原样返回
	if got := bingRealURL("https://dayueyu.com/"); got != "https://dayueyu.com/" {
		t.Errorf("直链应原样返回: %s", got)
	}
	// ck/a 跳转链接 base64url 解码
	real := "https://www.kaifangcidian.com/jt/yue/index.html"
	enc := base64.URLEncoding.EncodeToString([]byte(real))
	ck := "https://www.bing.com/ck/a?u=a1" + enc + "&ntb=1"
	if got := bingRealURL(ck); got != real {
		t.Errorf("ck/a 解码错误: got=%q want=%q", got, real)
	}
	// 非 a1 前缀不处理
	if got := bingRealURL("https://www.bing.com/ck/a?u=xyz"); got != "https://www.bing.com/ck/a?u=xyz" {
		t.Errorf("非 a1 应原样返回: %s", got)
	}
}

func TestIsCaptcha(t *testing.T) {
	cases := map[string]bool{
		"<title>百度安全验证</title>":          true,
		"wappass.baidu.com/static/captcha": true,
		"captcha/tuxing_v2.html":            true,
		"<title>搜狗</title><div>结果</div>":   false,
		"normal search results page":        false,
	}
	for body, want := range cases {
		if got := isCaptcha(body); got != want {
			t.Errorf("isCaptcha(%q)=%v want %v", body, got, want)
		}
	}
}

func TestBaiduMu(t *testing.T) {
	// data-log 属性里 JSON 的引号被 HTML 实体转义（&quot;）
	raw := `{"commonLog":{"clkInfo":{"urlsign":"123"}},"mu":"http://www.szkbg.com/p/25702-1-1.html"}`
	mu, _ := baiduMu(raw)
	if mu != "http://www.szkbg.com/p/25702-1-1.html" {
		t.Errorf("mu 解析错误: got=%q", mu)
	}
	// 无 mu 返回空
	if mu, _ := baiduMu(`{"commonLog":{}}`); mu != "" {
		t.Errorf("无 mu 应返回空: %q", mu)
	}
}
