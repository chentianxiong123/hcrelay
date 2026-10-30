// Package sniff 盗版站/通用网页嗅探：从详情页提取集数链接（纯 Go，无浏览器）。
//
// 逻辑对齐 homecast 历史 backend/app/sniffer/extractor.py 的 _parse_episode_list：
// 遍历 <a> 节点，过滤（路径含 /play/ 或 /vodplay/ + 文字含"集"）→ 集数列表。
// 但用 golang.org/x/net/html 解析，不依赖 Playwright 无头浏览器（手机可跑）。
package sniff

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
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
	UA      string // 请求 User-Agent
	Referer string // 请求 Referer（防盗链）
	Timeout time.Duration
	// 播放页路径关键词（命中任一视为播放页链接）。覆盖 macCMS/苹果CMS 各变体。
	PlayPath []string
	// 播放列表容器 id/class 关键词（链接在该容器内且含集数文字 → 是集数，即使路径不认识）。
	ContainerMarkers []string
	// 集数文本特征（兜底：命中才收，对齐 '集'）
	TextHint string
}

// Default 默认配置（覆盖苹果CMS/momovod 系常见盗版站 + /cid/ 变体）。
func Default() Config {
	return Config{
		UA:      "Mozilla/5.0 (Linux; Android 13; SM-S9180) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Mobile Safari/537.36",
		Referer: "",
		Timeout: 15 * time.Second,
		// 常见播放页路径（macCMS/苹果CMS/神马/自建系各变体）
		PlayPath: []string{
			"/play/", "/vodplay/", "/episode/", "/ep/",
			"/cid/", "/vod/", "/video/", "/show/", "/view/", "/watch/",
			"/dianying/", "/dsj/", "/zongyi/", "/lianzai/", "/tv/",
			"/smyyplay/", "/smyy/", "/smyvod/", "/vodplay2/", "/play2/",
		},
		// 播放列表容器特征（id/class 关键词）
		ContainerMarkers: []string{
			"playlist", "play-list", "play_list", "con_playlist", "episodes", "episode-list", "episode_list", "droplist", "playtab", "playerlist", "list-ep", "ep-list",
			"playsx", "plays-more", "playform", "play_list", "playpage", "player", "play-box", "playbox", "playlistbox",
		},
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
	if len(cfg.ContainerMarkers) == 0 {
		cfg.ContainerMarkers = []string{"playlist", "play-list", "play_list", "con_playlist", "episodes"}
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

// ResolveStream 从播放页 URL 提取实际视频流（m3u8/mp4）。
// macCMS/神马 播放页通常内嵌 player 配置的 url 字段或 video/iframe src。
// 返回流 URL 与 MIME 类型（纯 Go，零浏览器）。
func (e *Extractor) ResolveStream(playURL string) (streamURL, mime string, err error) {
	htmlText, err := e.Fetch(playURL)
	if err != nil {
		return "", "", err
	}
	base, _ := url.Parse(playURL)

	// 1. 绝对 m3u8（最可靠：player 配置内嵌的完整地址）
	if m := regexp.MustCompile(`https?://[^"'<>\\\s]+?\.m3u8[^"'<>\\\s]*`).FindString(htmlText); m != "" {
		return strings.TrimRight(m, ",'\")"), "application/vnd.apple.mpegurl", nil
	}
	// 2. 绝对 mp4
	if m := regexp.MustCompile(`https?://[^"'<>\\\s]+?\.mp4[^"'<>\\\s]*`).FindString(htmlText); m != "" {
		return strings.TrimRight(m, ",'\")"), "video/mp4", nil
	}
	// 3. player 配置相对 url（"url":"/xxx.m3u8" 或 url:'...'）
	if m := regexp.MustCompile(`["']url["']\s*[:=]\s*["']([^"']+\.m3u8[^"']*)["']`).FindStringSubmatch(htmlText); m != nil {
		return resolveRel(base, m[1]), "application/vnd.apple.mpegurl", nil
	}
	// 3b. 混淆流地址（macCMS base64/urldecode 变体：url:"JTY4JTc0..."）→ 解码后找 m3u8/mp4
	reConf := regexp.MustCompile(`["']url["']\s*[:=]\s*["']([^"']+)["']`)
	for _, m := range reConf.FindAllStringSubmatch(htmlText, -1) {
		if d := decodeStreamCandidate(m[1]); isStream(d) {
			return resolveRel(base, d), guessMime(d), nil
		}
	}
	// 4. <video src> / <source src>
	if m := regexp.MustCompile(`<video[^>]+src=["']([^"']+)["']`).FindStringSubmatch(htmlText); m != nil && isStream(m[1]) {
		return resolveRel(base, m[1]), guessMime(m[1]), nil
	}
	// 5. iframe（播放器在子页）→ 请求子页再找
	if m := regexp.MustCompile(`<iframe[^>]+src=["']([^"']+)["']`).FindStringSubmatch(htmlText); m != nil {
		sub := resolveRel(base, m[1])
		if subHTML, e2 := e.Fetch(sub); e2 == nil {
			if sm := regexp.MustCompile(`https?://[^"'<>\\\s]+?\.(m3u8|mp4)[^"'<>\\\s]*`).FindString(subHTML); sm != "" {
				return strings.TrimRight(sm, ",'\")"), guessMime(sm), nil
			}
		}
	}
	return "", "", errors.New("播放页未找到流地址（m3u8/mp4）")
}

func resolveRel(base *url.URL, ref string) string {
	u, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return base.ResolveReference(u).String()
}

// decodeStreamCandidate 解码 macCMS 混淆的流地址（base64 / urldecode 循环变体）。
// 例："JTY4JTc0..." → base64 → "%68%74%74..." → urldecode → "https://.../index.m3u8"
// 返回首个解码后能识别为流的候选（isStream），否则原样返回。
func decodeStreamCandidate(s string) string {
	if s == "" || isStream(s) {
		return s
	}
	cands := []string{s}
	cur := s
	for i := 0; i < 5; i++ {
		if b, err := base64.StdEncoding.DecodeString(cur); err == nil && len(b) > 4 {
			cands = append(cands, string(b))
			cur = string(b)
		}
		if u, err := url.QueryUnescape(cur); err == nil && u != cur {
			cands = append(cands, u)
			cur = u
		}
	}
	for _, c := range cands {
		if isStream(c) {
			return c
		}
	}
	return s
}

func isStream(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, ".m3u8") || strings.Contains(l, ".mp4") || strings.Contains(l, ".flv")
}

func guessMime(s string) string {
	l := strings.ToLower(s)
	if strings.Contains(l, ".m3u8") {
		return "application/vnd.apple.mpegurl"
	}
	return "video/mp4"
}

// Extract 从详情页 HTML 提取集数列表（通用规则 v2）。
// 判定组合（满足任一即收）：
//  1) 路径命中播放关键词 AND 文字含集数特征
//  2) 链接在播放列表容器内 AND 文字含集数特征（不依赖具体路径，兼容 /cid/ 等）
//  3) 链接在播放列表容器内 AND 路径含数字集数形态（macCMS /xxx/<id>-<n>-<ep>.html）
// baseURL 用于把相对 href 拼成绝对 URL。
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
			if isEpisodeLink(n, href, text, e.cfg) {
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

// NeedsBrowser 检测页面是否需要浏览器内核（JS 挑战/指纹反爬）。
// 纯 HTTP 拿不到真实内容，需要 WebView 兜底。
func NeedsBrowser(htmlText string) bool {
	low := strings.ToLower(htmlText)
	// 常见 JS 挑战/指纹守卫特征
	if strings.Contains(low, "/_guard/") || strings.Contains(low, "_guard/auto") {
		return true
	}
	if strings.Contains(low, "wappass.baidu.com") || strings.Contains(low, "verify") {
		return true
	}
	// 页面短小且只含一个 script（挑战页典型形态）
	if len(htmlText) < 500 && strings.Contains(low, "<script") && !strings.Contains(low, "<a ") {
		return true
	}
	return false
}

// isEpisodeLink 判定一个 <a> 是否为集数链接（通用规则 v2）。
// 主判据：在播放列表容器内（con_playlist/playlist 等）→ 集数文字/形态即可。
// 次判据：不在容器内，仅当 严格"第N集" + 路径命中（排除"下一集"/推荐位误抓）。
func isEpisodeLink(a *html.Node, href, text string, cfg Config) bool {
	if href == "" || strings.HasPrefix(href, "javascript:") || strings.HasPrefix(href, "#") {
		return false
	}
	if text == "" {
		return false
	}
	epStrict := reEPStrict.MatchString(text)
	epAny := reEPNum.MatchString(text)
	pathHit := isPlayLink(href, cfg.PlayPath)
	inContainer := inPlaylistContainer(a, cfg.ContainerMarkers)

	// 主判据：播放列表容器内（这里就是集数列表）
	if inContainer {
		if epStrict || epAny || macCMSepShape(href) || strings.Contains(text, cfg.TextHint) {
			return true
		}
	}
	// 次判据：不在容器内，仅严格"第N集" + 路径命中（排除"下一集"/推荐位）
	if !inContainer && epStrict && pathHit {
		return true
	}
	return false
}

// isEpText 集数文字识别：第N集 / N集 / 第N期 / 全集 / 正片 / 含"集"字（兜底）。
func isEpText(text, hint string) bool {
	if text == "" {
		return false
	}
	if reEPNum.MatchString(text) {
		return true
	}
	// 含"集"字（兜底，对齐旧 TextHint）
	if hint != "" && strings.Contains(text, hint) {
		return true
	}
	return false
}

// macCMSepShape 是否形如 macCMS 集数路径：/xxx/<id>-<n>-<ep>.html 或 /xxx/12345-1-1.html。
func macCMSepShape(href string) bool {
	return reEpShape.MatchString(strings.ToLower(href))
}

// inPlaylistContainer 向上找祖先（ul/div/li），id/class 命中播放列表标记。
func inPlaylistContainer(a *html.Node, markers []string) bool {
	for p := a.Parent; p != nil; p = p.Parent {
		if p.Type != html.ElementNode {
			continue
		}
		id := strings.ToLower(getAttr(p, "id"))
		cls := strings.ToLower(getAttr(p, "class"))
		for _, m := range markers {
			if strings.Contains(id, strings.ToLower(m)) || strings.Contains(cls, strings.ToLower(m)) {
				return true
			}
		}
		// 别爬太远（<body> 会误判）
		if p.Data == "body" {
			break
		}
	}
	return false
}

// ErrNeedsBrowser 页面需要浏览器内核（JS 挑战/指纹反爬），纯 HTTP 拿不到。
var ErrNeedsBrowser = errors.New("页面需要浏览器内核（JS 反爬挑战），请用 WebView 打开")

// ExtractURL 一步到位：拉取详情页 → 提取集数。
func (e *Extractor) ExtractURL(rawURL string) ([]Episode, error) {
	htmlText, err := e.Fetch(rawURL)
	if err != nil {
		return nil, err
	}
	// 已知正版平台（B站/优酷/爱奇艺/腾讯/芒果/搜狐/抖音等）不是盗版站，不走 WebView 兜底
	if !isLegitDomain(rawURL) && NeedsBrowser(htmlText) {
		return nil, ErrNeedsBrowser
	}
	eps := e.Extract(htmlText, rawURL)
	if len(eps) == 0 {
		return nil, errors.New("no episodes found")
	}
	return eps, nil
}

// isLegitDomain 已知正版平台域名：这些站的页面可能含 verify/wappass 字样，但走正常提取，不触发 WebView 兜底。
func isLegitDomain(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, d := range []string{
		"bilibili.com", "b23.tv", "youku.com", "iqiyi.com", "qiyi.com",
		"qq.com", "v.qq.com", "mgtv.com", "sohu.com", "douyin.com",
		"miguvideo.com", "1905.com", "cctv.com", "xigua.com",
	} {
		if strings.HasSuffix(host, d) || host == d {
			return true
		}
	}
	return false
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

// 集数文字正则（宽）：第N集 / N集 / 第N期 / 全集 / 正片 / 番外 / 特辑。
var reEPNum = regexp.MustCompile(`第\s*\d+\s*[集期话]|\d+\s*集|全集|正片|番外|特辑`)

// 严格集数文字：第N集 / 第N期 / 第N话（排除"下一集"等导航词）。
var reEPStrict = regexp.MustCompile(`第\s*\d+\s*[集期话]`)

// macCMS 集数路径形态：/xxx/<id>-<n>-<ep>.html 或 /xxx/12345-1-1.html（数字-数字-数字 结尾）。
var reEpShape = regexp.MustCompile(`/[^/]+/\d+-\d+-\d+(?:\.html?)?$`)
