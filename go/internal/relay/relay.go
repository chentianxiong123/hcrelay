// Package relay 编排层：接住 B站 投屏 → 取流 → 本地重封装 → 推给电视。
package relay

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"hcrelay/internal/bili"
	"hcrelay/internal/media"
	"hcrelay/internal/renderer"
	"hcrelay/internal/ssdp"
	"hcrelay/internal/tv"
)

// Relay 编排器
type Relay struct {
	renderer *renderer.Renderer
	media    *media.Server
	bili     *bili.Client
	BaseURL  string

	mu      sync.Mutex
	mode    media.Mode
	tv      *ssdp.Device // 选中的电视
	ctrl    *tv.Controller
	ses     *media.Session
	curURL  string // 上游原始 URL（B站给的）
	lastErr string
	// startedAt 最近一次成功建立会话的时间。
	//
	// B站 切换视频时会先发 Stop 再发 Play；但 Stop 可能乱序晚到，
	// 在新会话刚建立几秒内到达，误杀正在播的新视频。
	// 所以 App 发来的 Stop 若在会话建立后 6 秒内到达，视为迟到的
	// 重复命令，忽略。UI 手动停止不受此限制。
	startedAt time.Time
	// 事件日志
	Logs []string
}

// New 创建编排器
func New(r *renderer.Renderer, m *media.Server, b *bili.Client, baseURL string) *Relay {
	// 面向普通用户：只有一个「自动」模式，由后端根据上游流自动决定处理方式
	rl := &Relay{renderer: r, media: m, bili: b, BaseURL: baseURL, mode: media.ModeAuto}
	// 订阅渲染端事件
	r.SetEventFn(rl.onRendererEvent)
	return rl
}

// Logf 记一条日志（供 server 层调用）
func (rl *Relay) Logf(format string, args ...any) {
	rl.logf(format, args...)
}

func (rl *Relay) logf(format string, args ...any) {
	line := time.Now().Format("15:04:05") + " " + sprintf(format, args...)
	rl.mu.Lock()
	rl.Logs = append(rl.Logs, line)
	if len(rl.Logs) > 200 {
		rl.Logs = rl.Logs[len(rl.Logs)-200:]
	}
	rl.mu.Unlock()
	log.Println(line)
}

func (rl *Relay) setErr(msg string) {
	rl.mu.Lock()
	rl.lastErr = msg
	rl.mu.Unlock()
}

// SetMode 设置播放模式。
//
// 面向普通用户：只有「自动」一种。保留参数是为了将来内部调试，
// 但 remux 依赖 ffmpeg（手机上没有），所以只接受 auto/proxy/hls，
// remux 会被忽略。
func (rl *Relay) SetMode(m media.Mode) {
	switch m {
	case media.ModeAuto, media.ModeProxy, media.ModeHLS:
	default:
		return
	}
	rl.mu.Lock()
	rl.mode = m
	rl.mu.Unlock()
	rl.logf("播放模式 -> %s", m)
}

// GetMode 当前模式
func (rl *Relay) GetMode() media.Mode {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.mode
}

// resolveMode 解析实际处理方式。
//
// 面向普通用户：不需要选模式，统一用 proxy —— media 服务器会在收到
// 上游响应后按 Content-Type（application/vnd.apple.mpegurl 等）自动
// 判断是不是 HLS，是就自动重写播放列表并代理分片。
// 这样抖音那种 URL 不带 .m3u8 后缀的流也能正确处理。
func (rl *Relay) resolveMode(upstreamURL string) media.Mode {
	rl.mu.Lock()
	mode := rl.mode
	rl.mu.Unlock()
	if mode != media.ModeAuto && mode != "" {
		return mode
	}
	return media.ModeProxy
}

// isHLSURL 判断 URL 是否看起来是 HLS 播放列表
func isHLSURL(u string) bool {
	l := strings.ToLower(u)
	return strings.Contains(l, ".m3u8") || strings.Contains(l, "mpegurl")
}

// probeMime 探测上游真实 Content-Type（只读前几 KB，不下载整个流）。
//
// 抖音等来源的 HLS URL 不带 .m3u8 后缀，只能靠响应头判断；
// 探到播放列表就把 mime 改成 mpegurl，让推给电视的 DIDL 正确。
//
// 探测同时也是「能不能拿到流」的一次真实尝试：如果防盗链导致 403，
// 这里就会拿到失败信息，交给调用方换 Referer 策略重试。
func probeMime(ctx context.Context, rawURL, referer, ua string) string {
	ct, _ := probeUpstream(ctx, rawURL, referer, ua)
	return ct
}

// probeUpstream 试拉上游，返回 Content-Type 和 HTTP 状态码
func probeUpstream(ctx context.Context, rawURL, referer, ua string) (string, int) {
	if isHLSURL(rawURL) {
		// URL 明确是 m3u8，但要确认能不能拿到（防盗链）
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", 0
	}
	req.Header.Set("User-Agent", ua)
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	// 只读头部，不下载 body
	req.Header.Set("Range", "bytes=0-1023")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	// 某些 CDN 对 Range 返回 200 + video/mp2t，但实际是 m3u8 文本；
	// 读一点 body 看看开头是不是 #EXTM3U
	if ct != "" && !strings.Contains(strings.ToLower(ct), "mpegurl") {
		buf := make([]byte, 16)
		n, _ := resp.Body.Read(buf)
		if n > 0 && strings.HasPrefix(strings.TrimSpace(string(buf[:n])), "#EXTM3U") {
			return "application/vnd.apple.mpegurl", resp.StatusCode
		}
	}
	return ct, resp.StatusCode
}

// upstreamErrorMsg 把上游 HTTP 状态码翻译成人能看懂的话（给妈妈看）
func upstreamErrorMsg(code int) string {
	switch code {
	case 401:
		return "链接需要登录或已过期"
	case 403:
		return "网站防盗链拒绝访问（链接可能有时效）"
	case 404:
		return "链接已失效"
	case 410:
		return "链接已失效"
	case 429:
		return "网站限流了，等一会儿再试"
	case 500, 502, 503, 504:
		return "网站服务器出错，稍后再试"
	}
	return fmt.Sprintf("拉取失败（HTTP %d）", code)
}

// refererCandidates 给定 URL 和 App 自带 Referer，列出可以尝试的
// Referer 候选（按优先级）。
//
// 不知名站点 / 盗版站的防盗链要求 Referer 是站点自己的域名，
// 这个域名只能从 App 发来的请求头里拿到；拿不到时依次降级。
func refererCandidates(upstreamURL, appReferer string) []string {
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		// 过滤本地地址 / 无意义的 referer
		if strings.Contains(s, "127.0.0.1") || strings.Contains(s, "192.168.") ||
			strings.Contains(s, "localhost") {
			return
		}
		for _, e := range out {
			if e == s {
				return
			}
		}
		out = append(out, s)
	}
	// 1. App 自带（最准）
	add(appReferer)
	// 2. 按域名猜测
	guess, _ := guessHeaders(upstreamURL)
	add(guess)
	// 3. 同源（去掉 path，保留 origin）—— 盗版站常见
	if u, err := url.Parse(upstreamURL); err == nil && u.Host != "" {
		add(u.Scheme + "://" + u.Host + "/")
	}
	return out
}

// onRendererEvent 渲染端事件回调
func (rl *Relay) onRendererEvent(e renderer.Event) {
	switch e.Type {
	case "uri":
		rl.logf("收到 B站 投屏 URL: %s", truncate(e.URI, 80))
		rl.mu.Lock()
		rl.curURL = e.URI
		rl.mu.Unlock()
		rl.startRelay(e.URI, e.Meta, 0, e.Referer, e.UA)
	case "play":
		rl.logf("B站 下发 Play")
		rl.controlTV("play", "", 0)
	case "pause":
		rl.logf("B站 下发 Pause")
		rl.controlTV("pause", "", 0)
	case "stop":
		rl.logf("B站 下发 Stop")
		rl.stopRelay(true)
	case "seek":
		rl.logf("B站 Seek -> %s", e.Pos)
		rl.controlTV("seek", e.Pos, 0)
	case "volume":
		rl.logf("B站 SetVolume -> %d", e.Vol)
		rl.controlTV("volume", "", e.Vol)
	}
}

// HandleNVACmd NVA 协议命令回调（B站 极速/哔哩必连 模式）
func (rl *Relay) HandleNVACmd(name string, args map[string]any) (map[string]any, error) {
	rl.logf("NVA 命令: %s", name)
	switch name {
	case "Play":
		aid, _ := args["aid"].(string)
		cid, _ := args["cid"].(string)
		rl.logf("  Play aid=%s cid=%s", aid, cid)
		rl.startRelayByCID(aid, cid)
	case "PlayUrl":
		u, _ := args["url"].(string)
		rl.logf("  PlayUrl %s", truncate(u, 80))
		rl.startRelay(u, "", 0, "", "")
	case "Pause":
		rl.controlTV("pause", "", 0)
	case "Resume":
		rl.controlTV("play", "", 0)
	case "Stop":
		rl.stopRelay(true)
	case "Seek":
		if v, ok := args["seekTs"].(float64); ok {
			rl.controlTV("seek", fmtFloat(v), 0)
		}
	case "SwitchQn":
		rl.logf("  切清晰度（暂不处理）")
	case "SwitchSpeed":
		rl.logf("  倍速（暂不处理）")
	case "GetVolume":
		return map[string]any{"volume": 50}, nil
	case "SetVolume":
		return nil, nil
	}
	return nil, nil
}

func (rl *Relay) controlTV(action, target string, volume int) {
	rl.mu.Lock()
	ctrl := rl.ctrl
	rl.mu.Unlock()
	if ctrl == nil {
		rl.setErr("尚未选择电视")
		return
	}
	if err := ctrl.Control(action, target, volume); err != nil {
		rl.setErr("电视控制失败: " + err.Error())
		rl.logf("电视控制失败: %v", err)
	}
}

// startRelay 从标准 DLNA 拿到的 URL 开始中继
//
// fromReferer / fromUA 是发起投屏那个请求的头。
// 盗版站、不知名站点的 CDN 普遍校验 Referer（必须是自己站点的域名），
// 这种东西猜不出来，只能把 App 发过来时自带的 Referer 原样透传。
func (rl *Relay) startRelay(upstreamURL, meta string, _ int, fromReferer, fromUA string) {
	if upstreamURL == "" {
		rl.setErr("空 URL")
		return
	}

	title := extractTitle(meta)
	referer, ua := guessHeaders(upstreamURL)
	// App 自带的 Referer 优先（它才是真实来源站点的域名）
	if fromReferer != "" {
		referer = fromReferer
	}
	if fromUA != "" {
		ua = fromUA
	}

	// 探测上游：依次尝试候选 Referer（App 自带 → 域名猜测 → 同源 origin）。
	// 盗版站等不知名站点的防盗链靠 Referer，猜不到就试不出真相。
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 15*time.Second)
	mime := ""
	status := 0
	tried := refererCandidates(upstreamURL, fromReferer)
	for _, cand := range tried {
		ct, code := probeUpstream(probeCtx, upstreamURL, cand, ua)
		status = code
		if code == 200 || code == 206 {
			mime = ct
			referer = cand
			break
		}
	}
	probeCancel()

	// 探测失败时的处理：
	//   - 404/410：链接确定没了 → 直接报错
	//   - 403/401：防盗链或需登录 → 先试一次「不带 Referer」和「同源 origin」，
	//     还不行就报错（电视拿不到也是白转圈）
	//   - 其它（网络抖动/5xx）：先推给电视试，电视侧还会再拉一次
	if status != 200 && status != 206 && status != 0 {
		if status == 404 || status == 410 {
			rl.setErr(upstreamErrorMsg(status))
			rl.logf("上游 %d，链接已失效: %s", status, truncate(upstreamURL, 60))
			return
		}
		if status == 403 || status == 401 {
			// 最后再试一次：不带 Referer（有些站点反而放行）
			probeCtx2, cancel2 := context.WithTimeout(context.Background(), 8*time.Second)
			ct2, code2 := probeUpstream(probeCtx2, upstreamURL, "", ua)
			cancel2()
			if code2 == 200 || code2 == 206 {
				mime = ct2
				referer = ""
				status = code2
			} else {
				rl.setErr(upstreamErrorMsg(status))
				rl.logf("上游 %d，疑似防盗链（已试 %d 种 Referer + 无 Referer）: %s",
					status, len(tried), truncate(upstreamURL, 60))
				return
			}
		}
	}
	if mime == "" {
		mime = guessMime(upstreamURL)
	}
	if strings.Contains(strings.ToLower(mime), "mpegurl") {
		mime = "application/vnd.apple.mpegurl"
	}
	// DASH 明确不支持，提前报错，不要把 mpd 推给电视让它干等
	if strings.Contains(strings.ToLower(mime), "dash+xml") ||
		strings.Contains(strings.ToLower(upstreamURL), ".mpd") {
		rl.setErr("暂不支持 DASH (.mpd) 格式")
		rl.logf("暂不支持 DASH 格式: %s", truncate(upstreamURL, 80))
		return
	}
	src := media.Source{
		URL:      upstreamURL,
		MimeType: mime,
		Referer:  referer,
		UA:       ua,
		Title:    title,
	}
	ses, err := rl.media.Start(src, rl.resolveMode(upstreamURL))
	if err != nil {
		rl.setErr("媒体会话启动失败: " + err.Error())
		rl.logf("媒体会话启动失败: %v", err)
		return
	}
	rl.mu.Lock()
	rl.ses = ses
	rl.startedAt = time.Now()
	rl.mu.Unlock()
	rl.logf("媒体会话就绪: %s (%s)", ses.URL, ses.Type)

	// 推给电视
	rl.pushToTV(ses)
}

// startRelayByCID 用 aid/cid 重新取流（NVA 模式，拿新鲜流）
func (rl *Relay) startRelayByCID(aid, cid string) {
	if aid == "" || cid == "" {
		rl.setErr("NVA Play 缺 aid/cid")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = ctx
	st, err := rl.bili.PlayURL(aid, cid, 80)
	if err != nil {
		rl.setErr("playurl 失败: " + err.Error())
		rl.logf("playurl 失败: %v", err)
		return
	}
	rl.logf("playurl 取到流: %s", truncate(st.URL, 60))
	rl.mu.Lock()
	mode := rl.resolveMode(st.URL)
	rl.mu.Unlock()
	ses, err := rl.media.Start(media.Source{
		URL:      st.URL,
		MimeType: st.MimeType,
		Referer:  "https://www.bilibili.com/",
		UA:       rl.bili.UA,
		Title:    "Bilibili",
	}, mode)
	if err != nil {
		rl.setErr("媒体会话失败: " + err.Error())
		return
	}
	rl.mu.Lock()
	rl.ses = ses
	rl.startedAt = time.Now()
	rl.mu.Unlock()
	rl.logf("媒体会话就绪: %s", ses.URL)
	rl.pushToTV(ses)
}

func (rl *Relay) pushToTV(ses *media.Session) {
	rl.mu.Lock()
	dev := rl.tv
	rl.mu.Unlock()
	if dev == nil {
		rl.setErr("没有选择电视，先选一台")
		rl.logf("未选择电视，先选一台再投")
		return
	}
	// 建立/更新控制句柄
	ctrl, err := tv.NewController(dev)
	if err != nil {
		rl.setErr("连接电视失败: " + err.Error())
		rl.logf("连接电视失败: %v", err)
		return
	}
	rl.mu.Lock()
	rl.ctrl = ctrl
	rl.mu.Unlock()
	if err := ctrl.Push(ses.URL, ses.Src.Title, ses.Type); err != nil {
		rl.setErr("投送电视失败: " + err.Error())
		rl.logf("投送电视失败: %v", err)
		return
	}
	rl.logf("已推送到电视 %s：%s", dev.Name, ses.URL)
	rl.setErr("")
}

// stopRelay 停止当前会话。
//
// fromApp 表示这个 Stop 来自 App（NVA 事件/命令），不是用户手动点 UI。
// App 切换视频时会先发 Stop 再发 Play，但 Stop 可能乱序晚到，在新会话
// 刚建立几秒内到达，误杀正在播的新视频。所以 App 的 Stop 若在会话建立
// 后 stopGuardWindow 内到达，视为迟到的重复命令，忽略。
const stopGuardWindow = 6 * time.Second

func (rl *Relay) stopRelay(fromApp bool) {
	rl.mu.Lock()
	ses := rl.ses
	if ses == nil {
		rl.mu.Unlock()
		return
	}
	// App 发来的 Stop，且会话刚建立（切换序列的迟到 Stop）→ 忽略
	if fromApp && time.Since(rl.startedAt) < stopGuardWindow {
		rl.mu.Unlock()
		rl.logf("忽略迟到的 App Stop（会话建立 %d ms，疑为切换重复命令）",
			time.Since(rl.startedAt).Milliseconds())
		return
	}
	rl.ses = nil
	rl.mu.Unlock()
	rl.media.Stop(ses.ID)
	rl.logf("停止媒体会话 %s", ses.ID)
	rl.controlTV("stop", "", 0)
}

// Control 控制当前会话（给 UI 用）
// action: play | pause | stop | seek | volume | reconnect
// arg: seek 传秒数字符串（如 "120"），volume 传 0-100
func (rl *Relay) Control(action, arg string) error {
	rl.mu.Lock()
	ctrl := rl.ctrl
	hasTV := rl.tv != nil
	rl.mu.Unlock()

	if action == "stop" {
		rl.stopRelay(false)
		return nil
	}
	if action == "reconnect" {
		// 重新推一次当前会话给电视
		rl.mu.Lock()
		ses := rl.ses
		rl.mu.Unlock()
		if ses == nil {
			return fmt.Errorf("没有正在播放的会话")
		}
		rl.pushToTV(ses)
		return nil
	}
	if !hasTV || ctrl == nil {
		return fmt.Errorf("尚未选择电视")
	}
	switch action {
	case "play":
		return ctrl.Control("play", "", 0)
	case "pause":
		return ctrl.Control("pause", "", 0)
	case "seek":
		// arg 秒 → REL_TIME 格式 0:02:00
		sec, err := strconv.Atoi(strings.TrimSpace(arg))
		if err != nil {
			return fmt.Errorf("非法 seek 位置: %q", arg)
		}
		return ctrl.Control("seek", fmt.Sprintf("0:00:%02d", sec), 0)
	case "volume":
		v, err := strconv.Atoi(strings.TrimSpace(arg))
		if err != nil {
			return fmt.Errorf("非法音量: %q", arg)
		}
		if v < 0 {
			v = 0
		}
		if v > 100 {
			v = 100
		}
		return ctrl.Control("volume", "", v)
	}
	return fmt.Errorf("未知操作: %s", action)
}

// PlayState 播放状态（给 UI）
type PlayState struct {
	State    string  `json:"state"`    // PLAYING / PAUSED_PLAYBACK / STOPPED / 空
	Status   string  `json:"status"`   // 电视返回的原始状态
	Title    string  `json:"title"`    // 当前播放标题
	Duration string  `json:"duration"` // 电视侧总时长
	Position string  `json:"position"` // 电视侧当前位置
	URI      string  `json:"uri"`      // 电视正在播的地址
	Volume   string  `json:"volume"`
	Elapsed  float64 `json:"elapsed"`  // 本地估算已播秒
	Total    float64 `json:"total"`    // 本地估算总秒
	Percent  float64 `json:"percent"`  // 本地估算百分比
	PosBytes int64   `json:"pos_bytes"`
	TotalBytes int64 `json:"total_bytes"`
	Bytes    int64   `json:"bytes"`
	Speed    float64 `json:"speed"` // KB/s
}

// PlayStatus 查播放状态（优先电视侧，其次本地估算）
func (rl *Relay) PlayStatus() PlayState {
	var out PlayState
	rl.mu.Lock()
	ctrl := rl.ctrl
	ses := rl.ses
	rl.mu.Unlock()

	// 本地进度
	if ses != nil {
		p := ses.SessionProgress()
		out.Elapsed = p.Elapsed
		out.Total = p.Duration
		out.Percent = p.Percent
		out.Bytes = p.Bytes
		out.PosBytes = p.Pos
		out.TotalBytes = p.Total
		out.Speed = p.Speed
	}
	// 电视侧
	if ctrl != nil {
		st := ctrl.Status()
		out.State = st["state"]
		out.Status = st["status"]
		out.Duration = st["duration"]
		out.Position = st["position"]
		out.URI = st["uri"]
		out.Volume = st["volume"]
	}
	// 标题
	rl.mu.Lock()
	if rl.ses != nil {
		out.Title = rl.ses.Src.Title
	}
	rl.mu.Unlock()
	return out
}

// SelectTV 选择电视
func (rl *Relay) SelectTV(dev *ssdp.Device) {
	rl.mu.Lock()
	rl.tv = dev
	rl.mu.Unlock()
	rl.logf("已选电视: %s", dev.Name)
}

// Status 状态（给 UI）
func (rl *Relay) Status() map[string]any {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	ses := rl.ses
	out := map[string]any{
		"mode":     "auto",
		"tv":       nil,
		"cur_url":  truncate(rl.curURL, 120),
		"last_err": rl.lastErr,
		"ses_title": "",
		"ses_bytes": int64(0),
	}
	if rl.tv != nil {
		out["tv"] = rl.tv.Name
	}
	if ses != nil {
		rl.mu.Unlock()
		p := ses.SessionProgress()
		out["ses_title"] = ses.Src.Title
		out["ses_bytes"] = p.Bytes
		out["ses_pos"] = p.Pos
		out["ses_total"] = p.Total
		out["ses_elapsed"] = p.Elapsed
		out["ses_duration"] = p.Duration
		out["ses_percent"] = p.Percent
		out["ses_speed"] = p.Speed
		out["ses_url"] = ses.URL
		rl.mu.Lock()
	}
	return out
}

// Logs 日志
func (rl *Relay) GetLogs() []string {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	out := make([]string, len(rl.Logs))
	copy(out, rl.Logs)
	return out
}

// ---- 工具 ----

func extractTitle(meta string) string {
	if meta == "" {
		return "Bilibili"
	}
	if i := strings.Index(meta, "<dc:title>"); i > 0 {
		rest := meta[i+len("<dc:title>"):]
		if j := strings.Index(rest, "</dc:title>"); j > 0 {
			return rest[:j]
		}
	}
	return "Bilibili"
}

// guessMime 猜测上游 MIME。HLS 必须用 Content-Type 判断（抖音 URL
// 可能不带 .m3u8 后缀），所以这里只做 URL 层面的粗判，真正的判定
// 在 media 服务器（按上游 Content-Type + URL 后缀双重判断）。
func guessMime(u string) string {
	l := strings.ToLower(u)
	switch {
	case strings.Contains(l, "mpegurl"), strings.Contains(l, ".m3u8"):
		return "application/vnd.apple.mpegurl"
	case strings.Contains(l, ".mpd"):
		return "application/dash+xml"
	case strings.Contains(l, ".flv"):
		return "video/x-flv"
	case strings.Contains(l, ".webm"):
		return "video/webm"
	case strings.Contains(l, ".mp4"):
		return "video/mp4"
	case strings.Contains(l, ".ts"):
		return "video/mp2t"
	}
	return "video/mp4"
}

// guessHeaders 为上游请求补 Referer / UA。
//
// B站 CDN 有防盗链（校验 Referer/UA），无条件带上最保险。
// 但抖音/爱奇艺等来源如果带上 bilibili 的 Referer 反而会被拒，
// 所以按域名分流：认得的就用对应的，认不出的用不带 Referer 的
// 移动端 UA（大多数 CDN 对无 Referer 是放行的）。
func guessHeaders(u string) (referer, ua string) {
	ua = "Mozilla/5.0 (Linux; Android 13; SM-S9180) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Mobile Safari/537.36"
	l := strings.ToLower(u)
	switch {
	case strings.Contains(l, "bilivideo.com"), strings.Contains(l, "bilibili.com"),
		strings.Contains(l, "bfllvip.com"), strings.Contains(l, "biliapi.net"):
		return "https://www.bilibili.com/", ua
	case strings.Contains(l, "douyinvod.com"), strings.Contains(l, "douyin.com"),
		strings.Contains(l, "bytecdn.cn"), strings.Contains(l, "zjcdn.com"),
		strings.Contains(l, "ixigua.com"), strings.Contains(l, "pstatp.com"):
		return "https://www.douyin.com/", ua
	case strings.Contains(l, "baidu.com"), strings.Contains(l, "bdstatic.com"),
		strings.Contains(l, "bcebos.com"):
		return "https://www.baidu.com/", ua
	case strings.Contains(l, "youku.com"), strings.Contains(l, "tudou.com"),
		strings.Contains(l, "alicdn.com"), strings.Contains(l, "aliyuncs.com"):
		return "https://www.youku.com/", ua
	case strings.Contains(l, "iqiyi.com"), strings.Contains(l, "qiyi.com"),
		strings.Contains(l, "qiyipic.com"):
		return "https://www.iqiyi.com/", ua
	case strings.Contains(l, "qq.com"), strings.Contains(l, "gtimg.com"),
		strings.Contains(l, "v.qq.com"):
		return "https://v.qq.com/", ua
	}
	// 认不出的来源：不带 Referer，只带移动端 UA
	return "", ua
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func fmtFloat(v float64) string {
	return time.Duration(v * float64(time.Second)).String()
}

func sprintf(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}
