// Package relay 编排层：接住 B站 投屏 → 取流 → 本地重封装 → 推给电视。
package relay

import (
	"context"
	"fmt"
	"log"
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
	// 事件日志
	Logs []string
}

// New 创建编排器
func New(r *renderer.Renderer, m *media.Server, b *bili.Client, baseURL string) *Relay {
	rl := &Relay{renderer: r, media: m, bili: b, BaseURL: baseURL, mode: media.ModeProxy}
	// 订阅渲染端事件
	r.SetEventFn(rl.onRendererEvent)
	return rl
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

// Mode 设置播放模式
func (rl *Relay) SetMode(m media.Mode) {
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

// onRendererEvent 渲染端事件回调
func (rl *Relay) onRendererEvent(e renderer.Event) {
	switch e.Type {
	case "uri":
		rl.logf("收到 B站 投屏 URL: %s", truncate(e.URI, 80))
		rl.mu.Lock()
		rl.curURL = e.URI
		rl.mu.Unlock()
		rl.startRelay(e.URI, e.Meta, 0)
	case "play":
		rl.logf("B站 下发 Play")
		rl.controlTV("play", "", 0)
	case "pause":
		rl.logf("B站 下发 Pause")
		rl.controlTV("pause", "", 0)
	case "stop":
		rl.logf("B站 下发 Stop")
		rl.stopRelay()
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
		rl.startRelay(u, "", 0)
	case "Pause":
		rl.controlTV("pause", "", 0)
	case "Resume":
		rl.controlTV("play", "", 0)
	case "Stop":
		rl.stopRelay()
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
func (rl *Relay) startRelay(upstreamURL, meta string, _ int) {
	if upstreamURL == "" {
		rl.setErr("空 URL")
		return
	}
	rl.mu.Lock()
	mode := rl.mode
	rl.mu.Unlock()

	title := extractTitle(meta)
	referer, ua := guessHeaders(upstreamURL)
	src := media.Source{
		URL:      upstreamURL,
		MimeType: guessMime(upstreamURL),
		Referer:  referer,
		UA:       ua,
		Title:    title,
	}
	ses, err := rl.media.Start(src, mode)
	if err != nil {
		rl.setErr("媒体会话启动失败: " + err.Error())
		rl.logf("媒体会话启动失败: %v", err)
		return
	}
	rl.mu.Lock()
	rl.ses = ses
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
	mode := rl.mode
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

func (rl *Relay) stopRelay() {
	rl.mu.Lock()
	ses := rl.ses
	rl.ses = nil
	rl.mu.Unlock()
	if ses != nil {
		rl.media.Stop(ses.ID)
		rl.logf("停止媒体会话 %s", ses.ID)
	}
	rl.controlTV("stop", "", 0)
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
		"mode":     rl.mode,
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
		out["ses_title"] = ses.Src.Title
		out["ses_bytes"] = rl.media.SessionBytes(ses.ID)
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

func guessMime(u string) string {
	l := strings.ToLower(u)
	switch {
	case strings.Contains(l, ".m3u8"):
		return "application/x-mpegurl"
	case strings.Contains(l, ".mpd"):
		return "application/dash+xml"
	case strings.Contains(l, ".ts"):
		return "video/mp2t"
	case strings.Contains(l, ".flv"):
		return "video/x-flv"
	case strings.Contains(l, ".webm"):
		return "video/webm"
	}
	return "video/mp4"
}

// guessHeaders B站 CDN 有防盗链（Referer/UA 校验），无条件带上最保险。
// 不管域名是不是 bilivideo.com，都补上 B站 Referer 和移动端 UA。
func guessHeaders(u string) (referer, ua string) {
	return "https://www.bilibili.com/",
		"Mozilla/5.0 (Linux; Android 13; SM-S9180) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Mobile Safari/537.36"
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
