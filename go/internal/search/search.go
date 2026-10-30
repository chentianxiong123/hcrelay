// Package search 盗版站搜索：聚合 Bing/360/百度 三个中文搜索引擎（自建服务器专用）。
//
// 独立于 sniff/relay/media：只负责"搜盗版站"（拿详情页 URL），不负责解析集数（那是 sniff）。
// 策略（借鉴 SearXNG 引擎实现 + 实测验证）：
//   - Bing   直连 cn.bing.com/search → li.b_algo → h2/a href（ck/a 链接 base64url 解码）
//   - 360    先预热拿 cookie → www.so.com/s → m.so.com（移动版）→ res-list 的 data-mdurl
//   - 百度   m.baidu.com/s?word= → cosc 卡片 → data-log JSON 的 mu 字段（真实 URL）
//   - 验证码 检测 wappass/tuxing/安全验证 → 换下一引擎（fallback）
//
// 三个引擎就是最优解：搜中文盗版站只需要中文搜索引擎；海外引擎（Brave/Tavily/Kagi）
// 搜不到中文盗版站或要付费 API key，正规 API（百度千帆/博查）又过滤盗版资源。
package search

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	xhtml "golang.org/x/net/html"
)

// Result 一条搜索结果。
type Result struct {
	Engine  string `json:"engine"`           // bing / so / baidu
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content,omitempty"`
}

// Config 搜索配置。
type Config struct {
	UA      string        // 请求 User-Agent（手机 Safari，更接近真实用户）
	Timeout time.Duration // 单请求超时
	Delay   time.Duration // 引擎间退避（验证码/失败时等待）
}

// Default 默认配置：手机 iPhone UA + 8s 超时 + 3s 引擎间退避。
func Default() Config {
	return Config{
		UA:      "Mozilla/5.0 (iPhone; CPU iPhone OS 16_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.0 Mobile/15E148 Safari/604.1",
		Timeout: 8 * time.Second,
		Delay:   3 * time.Second,
	}
}

// Aggregator 聚合搜索器：按序试引擎，遇验证码/空结果自动 fallback。
type Aggregator struct {
	cfg Config
	hc  *http.Client
}

// New 构造聚合搜索器。
func New(cfg Config) *Aggregator {
	if cfg.UA == "" {
		cfg.UA = Default().UA
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = Default().Timeout
	}
	if cfg.Delay == 0 {
		cfg.Delay = Default().Delay
	}
	return &Aggregator{
		cfg: cfg,
		hc:  &http.Client{Timeout: cfg.Timeout},
	}
}

// Search 聚合搜索：bing → so → baidu，去重，返回结果。
// 每个引擎失败（验证码/网络/空结果）自动换下一个；全部失败返回聚合错误。
func (a *Aggregator) Search(ctx context.Context, query string) ([]Result, error) {
	engines := []func(context.Context, string) ([]Result, error){
		a.searchBing, a.search360, a.searchBaidu,
	}
	seen := map[string]bool{}
	var all []Result
	var errs []string
	for i, fn := range engines {
		rs, err := fn(ctx, query)
		if err != nil {
			errs = append(errs, err.Error())
		}
		for _, r := range rs {
			if r.URL == "" || seen[r.URL] {
				continue
			}
			seen[r.URL] = true
			all = append(all, r)
		}
		// 有结果就停（避免撞反爬）；最后一个引擎不等待
		if len(all) > 0 && i < len(engines)-1 {
			break
		}
		if i < len(engines)-1 {
			select {
			case <-ctx.Done():
				return all, ctx.Err()
			case <-time.After(a.cfg.Delay):
			}
		}
	}
	if len(all) == 0 {
		if len(errs) > 0 {
			return nil, errors.New(strings.Join(errs, "; "))
		}
		return nil, errors.New("无结果")
	}
	return all, nil
}

// ── 公共 HTTP 请求 ──

func (a *Aggregator) get(ctx context.Context, rawURL, referer string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", a.cfg.UA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.5")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	return a.hc.Do(req)
}

// isCaptcha 验证码/安全验证检测：命中返回 true（应换引擎）。
func isCaptcha(body string) bool {
	low := strings.ToLower(body)
	for _, k := range []string{
		"wappass.baidu.com", "captcha/tuxing", "百度安全验证", "安全验证",
		"verify", "访问验证", "请输入验证码",
	} {
		if strings.Contains(low, strings.ToLower(k)) {
			return true
		}
	}
	return false
}

// ── Bing（最可行，直连拿直链）──

func (a *Aggregator) searchBing(ctx context.Context, query string) ([]Result, error) {
	u := "https://cn.bing.com/search?q=" + url.QueryEscape(query)
	resp, err := a.get(ctx, u, "https://cn.bing.com/")
	if err != nil {
		return nil, fmt.Errorf("bing: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("bing: HTTP %d", resp.StatusCode)
	}
	doc, err := xhtml.Parse(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("bing: 解析失败: %w", err)
	}
	var rs []Result
	walkNodes(doc, func(n *xhtml.Node) {
		if n.Type != xhtml.ElementNode || n.Data != "li" || !hasClass(n, "b_algo") {
			return
		}
		// h2 > a
		title, href := "", ""
		walkNodes(n, func(m *xhtml.Node) {
			if m.Type == xhtml.ElementNode && m.Data == "a" && href == "" {
				for _, at := range m.Attr {
					if at.Key == "href" {
						href = at.Val
					}
				}
				title = strings.TrimSpace(textOf(m))
			}
		})
		href = bingRealURL(href)
		title = cleanTitle(title)
		if title != "" && href != "" {
			rs = append(rs, Result{Engine: "bing", Title: title, URL: href})
		}
	})
	return rs, nil
}

// cleanTitle 清理标题：去 URL 混入（bing 卡片可能把域名/URL 混进文本）。
func cleanTitle(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "http"); i > 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

// bingRealURL Bing 跳转链接 base64url 解码：/ck/a?u=a1<base64url> → 真实 URL。
func bingRealURL(href string) string {
	if !strings.Contains(href, "/ck/a?") {
		return href
	}
	qs, err := url.Parse(href)
	if err != nil {
		return href
	}
	u := qs.Query().Get("u")
	if !strings.HasPrefix(u, "a1") {
		return href
	}
	enc := u[2:]
	enc += strings.Repeat("=", (-len(enc))%4)
	b, err := base64.URLEncoding.DecodeString(enc)
	if err != nil {
		return href
	}
	return string(b)
}

// ── 360（需 cookie 预热，data-mdurl 提真实 URL）──

func (a *Aggregator) search360(ctx context.Context, query string) ([]Result, error) {
	// 预热：GET so.com 首页拿 set-cookie（跨域 www→m 自动带）
	_, _ = a.get(ctx, "https://www.so.com/", "")
	u := "https://www.so.com/s?pn=1&q=" + url.QueryEscape(query)
	resp, err := a.get(ctx, u, "https://www.so.com/")
	if err != nil {
		return nil, fmt.Errorf("so: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("so: HTTP %d", resp.StatusCode)
	}
	body := readBody(resp)
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("so: 空响应（非中国 IP / 被拦）")
	}
	// 移动版 m.so.com：res-list → h3.res-title/a 的 data-mdurl
	var rs []Result
	for _, m := range reDataMdurl.FindAllStringSubmatch(body, -1) {
		realURL := html.UnescapeString(m[1])
		if realURL == "" || strings.HasPrefix(realURL, "javascript") {
			continue
		}
		rs = append(rs, Result{Engine: "so", URL: realURL})
	}
	if len(rs) == 0 {
		return nil, fmt.Errorf("so: 未提取到结果")
	}
	return rs, nil
}

// ── 百度（m.baidu.com 移动版，data-log JSON 的 mu 字段）──

func (a *Aggregator) searchBaidu(ctx context.Context, query string) ([]Result, error) {
	u := "https://m.baidu.com/s?word=" + url.QueryEscape(query)
	resp, err := a.get(ctx, u, "https://m.baidu.com/")
	if err != nil {
		return nil, fmt.Errorf("baidu: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("baidu: HTTP %d", resp.StatusCode)
	}
	body := readBody(resp)
	if isCaptcha(body) {
		return nil, fmt.Errorf("baidu: 触发安全验证")
	}
	// cosc 卡片：data-log='{...,"mu":"真实URL"}' → mu
	var rs []Result
	for _, m := range reDataLog.FindAllStringSubmatch(body, -1) {
		mu, title := baiduMu(m[1])
		if mu == "" || strings.HasPrefix(mu, "javascript") {
			continue
		}
		rs = append(rs, Result{Engine: "baidu", Title: title, URL: mu})
	}
	if len(rs) == 0 {
		return nil, fmt.Errorf("baidu: 未提取到结果")
	}
	return rs, nil
}

// baiduMu 解析 data-log 属性 JSON，取 mu（真实 URL）与附近标题。
func baiduMu(raw string) (mu, title string) {
	decoded := html.UnescapeString(raw)
	var obj struct {
		Mu string `json:"mu"`
	}
	if err := json.Unmarshal([]byte(decoded), &obj); err != nil {
		return "", ""
	}
	return obj.Mu, ""
}

// ── 工具 ──

var (
	reDataMdurl = regexp.MustCompile(`data-mdurl="([^"]+)"`)
	reDataLog   = regexp.MustCompile(`data-log="([^"]*mu[^"]*)"`)
)

// hasClass 判断节点是否含指定 class。
func hasClass(n *xhtml.Node, cls string) bool {
	for _, at := range n.Attr {
		if at.Key == "class" {
			for _, c := range strings.Fields(at.Val) {
				if c == cls {
					return true
				}
			}
		}
	}
	return false
}

// textOf 提取节点纯文本。
func textOf(n *xhtml.Node) string {
	var b strings.Builder
	var walk func(*xhtml.Node)
	walk = func(m *xhtml.Node) {
		if m.Type == xhtml.TextNode {
			b.WriteString(m.Data)
		}
		for c := m.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}

// walkNodes 深度遍历，回调每个节点（含自身）。
func walkNodes(n *xhtml.Node, fn func(*xhtml.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkNodes(c, fn)
	}
}

// readBody 读响应体（限制 4MB 防超大）。
func readBody(resp *http.Response) string {
	buf := make([]byte, 0, 1<<20)
	tmp := make([]byte, 64<<10)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if len(buf) > 4<<20 {
			break
		}
		if err != nil {
			break
		}
	}
	return string(buf)
}
