// Package sniff 盗版站/通用网页嗅探：从详情页提取集数链接（纯 Go，无浏览器）。
//
// 逻辑对齐 homecast 历史 backend/app/sniffer/extractor.py 的 _parse_episode_list：
// 遍历 <a> 节点，过滤（路径含 /play/ 或 /vodplay/ + 文字含"集"）→ 集数列表。
// 但用 golang.org/x/net/html 解析，不依赖 Playwright 无头浏览器（手机可跑）。
package sniff

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// Episode 一集：集数 + 标题 + 播放页链接。
type Episode struct {
	Index int    `json:"index"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

// Config 提取配置：页面类型正则/集数过滤特征（对齐 _is_detail_page/_is_play_page + 过滤规则）。
type Config struct {
	UA       string // 请求 User-Agent
	Referer  string // 请求 Referer（防盗链）
	Timeout  time.Duration
	// 集数链接路径特征（命中任一即视为播放页链接，对齐 '/play/'、'/vodplay/'）
	PlayPath []string
	// 集数文本特征（命中才收，对齐 '集'）
	TextHint string
}

// Default 默认配置（覆盖苹果CMS/momovod 系常见盗版站）。
func Default() Config {
	return Config{
		UA:       "Mozilla/5.0 (Linux; Android 13; SM-S9180) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Mobile Safari/537.36",
		Referer:  "",
		Timeout:  15 * time.Second,
		PlayPath: []string{"/play/", "/vodplay/", "/episode/", "/ep/"},
		TextHint: "集",
	}
}

// Extractor 集数提取器。
type Extractor struct {
	cfg Config
	hc  *http.Client
}

// New 构造提取器。
func New(cfg Config) *Extractor {
	if cfg.Timeout == 0 {
		cfg.Timeout = 15 * time.Second
	}
	if len(cfg.PlayPath) == 0 {
		cfg.PlayPath = []string{"/play/", "/vodplay/", "/episode/", "/ep/"}
	}
	if cfg.TextHint == "" {
		cfg.TextHint = "集"
	}
	return &Extractor{cfg: cfg, hc: &http.Client{Timeout: cfg.Timeout}}
}

// IsDetailPage 正则识别详情页（对齐 _is_detail_page）。
func IsDetailPage(rawURL string) bool {
	for _, p := range []string{
		"/vod/", "/voddetail/", "/detail/", "/video/", "/movie/", "/tv/",
	} {
		if strings.Contains(strings.ToLower(rawURL), p) {
			return true
		}
	}
	return false
}

// IsPlayPage 正则识别播放页（对齐 _is_play_page）。
func IsPlayPage(rawURL string) bool {
	for _, p := range []string{
		"/play/", "/vodplay/", "/episode/", "/ep/", "play",
	} {
		if strings.Contains(strings.ToLower(rawURL), p) {
			return true
		}
	}
	return false
}

// Fetch 拉取详情页 HTML（带 UA/Referer，GBK 自动转 UTF-8）。
func (e *Extractor) Fetch(rawURL string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	if e.cfg.UA != "" {
		req.Header.Set("User-Agent", e.cfg.UA)
	}
	if e.cfg.Referer != "" {
		req.Header.Set("Referer", e.cfg.Referer)
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", &HTTPError{Code: resp.StatusCode}
	}
	// 按响应头/内容自动识别编码（GBK 等 → UTF-8）
	body, err := charset.NewReader(resp.Body, resp.Header.Get("Content-Type"))
	if err != nil {
		body = resp.Body
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// HTTPError 上游 HTTP 状态错误。
type HTTPError struct{ Code int }

func (h *HTTPError) Error() string { return "http " + strconv.Itoa(h.Code) }

// Extract 从详情页 HTML 提取集数列表。
// baseURL 用于把相对 href 拼成绝对 URL（取详情页 URL 的 scheme://host）。
func (e *Extractor) Extract(htmlText, baseURL string) []Episode {
	doc, err := html.Parse(strings.NewReader(htmlText))
	if err != nil {
		return nil
	}
	base, _ := url.Parse(baseURL)
	seen := map[string]bool{}
	var eps []Episode
	idx := 0

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			href := getAttr(n, "href")
			text := strings.TrimSpace(nodeText(n))
			if isPlayLink(href, e.cfg.PlayPath) && strings.Contains(text, e.cfg.TextHint) {
				full := resolveHref(base, href)
				if full != "" && !seen[full] {
					seen[full] = true
					idx++
					eps = append(eps, Episode{Index: idx, Title: text, URL: full})
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return eps
}

// ExtractURL 一步到位：拉取详情页 → 提取集数。
func (e *Extractor) ExtractURL(rawURL string) ([]Episode, error) {
	htmlText, err := e.Fetch(rawURL)
	if err != nil {
		return nil, err
	}
	eps := e.Extract(htmlText, rawURL)
	if len(eps) == 0 {
		return nil, errors.New("no episodes found")
	}
	return eps, nil
}

func isPlayLink(href string, paths []string) bool {
	href = strings.ToLower(href)
	for _, p := range paths {
		if strings.Contains(href, p) {
			return true
		}
	}
	return false
}

func resolveHref(base *url.URL, href string) string {
	if href == "" || strings.HasPrefix(href, "javascript:") || strings.HasPrefix(href, "#") {
		return ""
	}
	if base != nil {
		if u, err := base.Parse(href); err == nil {
			return u.String()
		}
	}
	return href
}

func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func nodeText(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return sb.String()
}
