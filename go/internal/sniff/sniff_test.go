package sniff

import (
	"strings"
	"testing"
)

// 典型苹果CMS/momovod 详情页片段：ul > li > a 集数结构。
const sampleDetail = `<html><head><title>某剧详情</title></head><body>
<div class="hd">某剧 2026</div>
<ul class="episodes">
  <li><a href="/vodplay/12345-1-1.html">第1集</a></li>
  <li><a href="/vodplay/12345-1-2.html">第2集</a></li>
  <li><a href="/vodplay/12345-1-3.html">第3集</a></li>
</ul>
<div class="ad"><a href="/ad.html">立即播放</a></div>
<a href="/play/999.html">第1集</a>
</body></html>`

func TestExtractEpisodes(t *testing.T) {
	e := New(Default())
	eps := e.Extract(sampleDetail, "https://xxxvod.com/vod/12345.html")
	// ul 里 3 个 + body 末尾游离的 /play/999.html（也是合法播放页）
	if len(eps) != 4 {
		t.Fatalf("want 4 episodes, got %d: %+v", len(eps), eps)
	}
	// 相对链接应拼成绝对 URL
	if eps[0].URL != "https://xxxvod.com/vodplay/12345-1-1.html" {
		t.Errorf("ep0 URL=%q", eps[0].URL)
	}
	if eps[0].Title != "第1集" || eps[0].Index != 1 {
		t.Errorf("ep0=%+v", eps[0])
	}
	// body 末尾游离播放链接也被提取（/play/999.html 第4个）
	if eps[3].URL != "https://xxxvod.com/play/999.html" {
		t.Errorf("ep3 URL=%q", eps[3].URL)
	}
}

func TestExtractFiltersNonEpisode(t *testing.T) {
	htmlText := `<ul><li><a href="/vodplay/1.html">第1集</a></li>
<li><a href="/vodplay/2.html">第2集</a></li>
<li><a href="/other.html">第3集</a></li>  <!-- 路径不对，排除 -->
<li><a href="/vodplay/4.html">立即播放</a></li> <!-- 无"集"字，排除 --></ul>`
	e := New(Default())
	eps := e.Extract(htmlText, "http://x.com/")
	if len(eps) != 2 {
		t.Fatalf("want 2, got %d: %+v", len(eps), eps)
	}
}

func TestExtractDedupAndSort(t *testing.T) {
	// 乱序 + 重复，验证去重与按出现顺序
	htmlText := `<a href="/vodplay/3.html">第3集</a>
<a href="/vodplay/1.html">第1集</a>
<a href="/vodplay/2.html">第2集</a>
<a href="/vodplay/1.html">第1集</a>`
	e := New(Default())
	eps := e.Extract(htmlText, "http://x.com/")
	if len(eps) != 3 {
		t.Fatalf("want 3, got %d", len(eps))
	}
	if eps[0].Title != "第3集" || eps[1].Title != "第1集" || eps[2].Title != "第2集" {
		t.Errorf("order unexpected: %+v", eps)
	}
}

func TestIsDetailPlayPage(t *testing.T) {
	cases := []struct {
		url      string
		detail   bool
		play     bool
	}{
		{"https://x.com/vod/123.html", true, false},
		{"https://x.com/vodplay/123-1.html", false, true},
		{"https://x.com/play/99.html", false, true},
		{"https://x.com/detail/88.html", true, false},
		{"https://x.com/movie/1.html", true, false},
	}
	for _, c := range cases {
		if IsDetailPage(c.url) != c.detail {
			t.Errorf("IsDetailPage(%q)=%v want %v", c.url, !c.detail, c.detail)
		}
		if IsPlayPage(c.url) != c.play {
			t.Errorf("IsPlayPage(%q)=%v want %v", c.url, !c.play, c.play)
		}
	}
}

func TestResolveHrefRelative(t *testing.T) {
	e := New(Default())
	htmlText := `<a href="../vodplay/9.html">第9集</a>`
	eps := e.Extract(htmlText, "https://x.com/vod/123.html")
	if len(eps) != 1 {
		t.Fatalf("want 1, got %d", len(eps))
	}
	if !strings.Contains(eps[0].URL, "vodplay/9.html") {
		t.Errorf("URL=%q", eps[0].URL)
	}
}

// 站1（deyunshucai.com）真实结构：/cid/ 路径 + con_playlist 容器 + 下一集/推荐位要排除。
func TestExtractMacCMSWithCIDAndContainer(t *testing.T) {
	htmlText := `<html><body>
<div class="hd">某剧 2026</div>
<div class="user-bt"><a id="next" href="/cid/87964-1-2.html">下一集</a></div>
<div class="details-play-list">
<ul id="con_playlist_1">
  <li><a href="/cid/87964-1-1.html">第1集</a></li>
  <li><a href="/cid/87964-1-2.html">第2集</a></li>
  <li><a href="/cid/87964-1-3.html">第3集</a></li>
</ul>
</div>
<div class="recommend"><a href="/cid/77623.html">0.0全19集</a></div>
</body></html>`
	e := New(Default())
	eps := e.Extract(htmlText, "https://www.deyunshucai.com/cid/87964-1-1.html")
	// 只收容器内 3 集，排除"下一集"和推荐位
	if len(eps) != 3 {
		t.Fatalf("want 3, got %d: %+v", len(eps), eps)
	}
	if eps[0].URL != "https://www.deyunshucai.com/cid/87964-1-1.html" {
		t.Errorf("ep0 URL=%q", eps[0].URL)
	}
	for _, ep := range eps {
		if ep.Title == "下一集" || strings.Contains(ep.Title, "全") {
			t.Errorf("误抓导航/推荐位: %+v", ep)
		}
	}
}

// NeedsBrowser：站2（taijiquanpu.com）守卫页 / 验证码页 → true；正常页 → false。
func TestNeedsBrowser(t *testing.T) {
	guardPage := `<script src="/_guard/auto.js"></script>`
	if !NeedsBrowser(guardPage) {
		t.Errorf("guard page should need browser")
	}
	baiduVerify := `<html><script>location.href='https://wappass.baidu.com/verify'</script></html>`
	if !NeedsBrowser(baiduVerify) {
		t.Errorf("verify page should need browser")
	}
	normal := `<html><body><a href="/cid/1-1-1.html">第1集</a></body></html>`
	if NeedsBrowser(normal) {
		t.Errorf("normal page should NOT need browser")
	}
}

// 站2（taijiquanpu.com 神马影院）真实结构：/smyyplay/ 路径 + playsx 容器 + btn-play 链接。
func TestExtractShenma(t *testing.T) {
	htmlText := `<div class="row gutter-20 sort plays-more" id="playsx">
  <a href="/smyyplay/211339-1-1.html" class="btn-play">第1集</a>
  <a href="/smyyplay/211339-1-2.html" class="btn-play">第2集</a>
  <a href="/smyyplay/211339-1-3.html" class="btn-play">第3集</a>
</div>
<div class="recommend"><a href="/smyydetail/88.html">0.0全20集</a></div>`
	e := New(Default())
	eps := e.Extract(htmlText, "https://www.taijiquanpu.com/smyydetail/211339.html")
	if len(eps) != 3 {
		t.Fatalf("want 3, got %d: %+v", len(eps), eps)
	}
	if eps[0].URL != "https://www.taijiquanpu.com/smyyplay/211339-1-1.html" {
		t.Errorf("ep0 URL=%q", eps[0].URL)
	}
	// 排除推荐位（"0.0全20集"，不在容器，非严格集数）
	for _, ep := range eps {
		if strings.Contains(ep.Title, "全") {
			t.Errorf("误抓推荐位: %+v", ep)
		}
	}
}
