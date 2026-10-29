// Package media 本地媒体服务：把 B站 上游流重封装/代理后，以本地 HTTP 喂给电视。
//
// 这是"中继"真正的价值所在——解决"越来越慢"：
//   - proxy 模式：透传 + 补 Referer/UA + 支持 Range（治"电视拿不到/被鉴权挡"）
//   - remux  模式：ffmpeg -c copy 重封装成 MPEG-TS，重置时间戳（治"PTS 漂移/慢动作"）
//   - hls    模式：把上游 m3u8 的段 URL 改写到本机代理，只暴露最新窗口（治"HLS 被当直播越拖越慢"）
package media

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mode 播放模式
type Mode string

const (
	// ModeAuto 自动：根据上游流类型自动选择（面向普通用户）
	ModeAuto Mode = "auto"
	ModeProxy Mode = "proxy"
	ModeRemux Mode = "remux"
	ModeHLS   Mode = "hls"
)

// Source 一条上游流
type Source struct {
	URL      string
	MimeType string
	Referer  string
	UA       string
	Title    string
}

// Session 一次播放会话
type Session struct {
	ID     string
	Src    Source
	Mode   Mode
	URL    string // 对外可播 URL（http://host/m/<id> 或 /m/<id>/playlist.m3u8）
	Type   string // content-type
	mu     sync.Mutex
	active bool
	pos    int64 // 流位置（上游绝对字节偏移，用于进度条；seek 时按 Range 重置）
	bytes  int64 // 累计传输字节（用于速度）
	total  int64 // 上游总长度（字节），0 = 未知
	start  time.Time
	// 速度采样
	lastBytes int64
	lastTime  time.Time
}

// Progress 播放进度
type Progress struct {
	Bytes    int64   `json:"bytes"`    // 累计传输字节（速度用）
	Pos      int64   `json:"pos"`      // 当前流位置（进度用）
	Total    int64   `json:"total"`    // 总字节（0=未知）
	Percent  float64 `json:"percent"`  // 百分比（0=未知）
	Elapsed  float64 `json:"elapsed"`  // 已播秒数（估算）
	Duration float64 `json:"duration"` // 总秒数（估算）
	Speed    float64 `json:"speed"`    // 实时速度 KB/s
}

// Server 媒体服务器
type Server struct {
	mu       sync.Mutex
	sessions map[string]*Session
	baseURL  string // 本机对外地址，如 http://192.168.1.5:8080
	hc       *http.Client
	ffmpeg   string
}

// NewServer 创建
func NewServer(baseURL string) *Server {
	s := &Server{
		sessions: map[string]*Session{},
		baseURL:  strings.TrimRight(baseURL, "/"),
		hc:       &http.Client{Timeout: 0},
		ffmpeg:   "ffmpeg",
	}
	return s
}

// Start 创建会话并返回可播 URL
func (s *Server) Start(src Source, mode Mode) (*Session, error) {
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	ses := &Session{ID: id, Src: src, Mode: mode, active: true, start: time.Now()}
	s.mu.Lock()
	s.sessions[id] = ses
	s.mu.Unlock()
	switch mode {
	case ModeProxy:
		ses.URL = s.baseURL + "/m/" + id
		ses.Type = src.MimeType
		if isHLSURL(src.URL) || isHLSType(src.MimeType) {
			// HLS：推给电视的 Content-Type 用标准值，电视才会当播放列表处理
			ses.Type = "application/vnd.apple.mpegurl"
		}
	case ModeHLS:
		ses.URL = s.baseURL + "/m/" + id + "/playlist.m3u8"
		ses.Type = "application/vnd.apple.mpegurl"
	default: // remux
		ses.URL = s.baseURL + "/m/" + id + ".ts"
		ses.Type = "video/mp2t"
	}
	log.Printf("[media] session %s mode=%s -> %s", id, mode, ses.URL)
	return ses, nil
}

// Stop 停止会话
func (s *Server) Stop(id string) {
	s.mu.Lock()
	ses, ok := s.sessions[id]
	if ok {
		delete(s.sessions, id)
	}
	s.mu.Unlock()
	if ok {
		ses.mu.Lock()
		ses.active = false
		ses.mu.Unlock()
	}
}

// DebugInfo 调试信息（完整上游 URL + 头 + 上游状态码）
type DebugInfo struct {
	ID           string `json:"id"`
	Mode         Mode   `json:"mode"`
	UpstreamURL  string `json:"upstream_url"`
	Referer      string `json:"referer"`
	UA           string `json:"ua"`
	MimeType     string `json:"mime_type"`
	PublicURL    string `json:"public_url"`
	Bytes        int64  `json:"bytes"`
	UpstreamCode int    `json:"upstream_code"`
	UpstreamErr  string `json:"upstream_err"`
}

// Debug 返回当前所有会话的调试信息
func (s *Server) Debug() []DebugInfo {
	s.mu.Lock()
	out := make([]DebugInfo, 0, len(s.sessions))
	for _, ses := range s.sessions {
		d := DebugInfo{
			ID:        ses.ID,
			Mode:      ses.Mode,
			UpstreamURL: ses.Src.URL,
			Referer:   ses.Src.Referer,
			UA:        ses.Src.UA,
			MimeType:  ses.Src.MimeType,
			PublicURL: ses.URL,
			Bytes:     ses.SessionBytes(),
		}
		// 实测上游
		req, err := http.NewRequest(http.MethodHead, ses.Src.URL, nil)
		if err == nil {
			if ses.Src.Referer != "" {
				req.Header.Set("Referer", ses.Src.Referer)
			}
			if ses.Src.UA != "" {
				req.Header.Set("User-Agent", ses.Src.UA)
			}
			if resp, err := s.hc.Do(req); err == nil {
				d.UpstreamCode = resp.StatusCode
				resp.Body.Close()
			} else {
				d.UpstreamErr = err.Error()
			}
		}
		out = append(out, d)
	}
	s.mu.Unlock()
	return out
}

// SessionBytes 已传输字节数
func (s *Session) SessionBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

// SessionProgress 播放进度（基于流位置 pos + 上游总长估算）
func (s *Session) SessionProgress() Progress {
	s.mu.Lock()
	bytes := s.bytes
	pos := s.pos
	total := s.total
	start := s.start
	// 计算速度（与上次采样对比）
	var speed float64
	if s.lastTime.IsZero() {
		s.lastTime = time.Now()
		s.lastBytes = bytes
	} else {
		if dt := time.Since(s.lastTime).Seconds(); dt > 0.2 {
			speed = float64(bytes-s.lastBytes) / 1024 / dt
			s.lastBytes = bytes
			s.lastTime = time.Now()
		}
	}
	s.mu.Unlock()

	p := Progress{Bytes: bytes, Pos: pos, Total: total, Speed: speed}
	if total > 0 {
		p.Percent = float64(pos) * 100 / float64(total)
	}
	// 估算时长：按平均码率 500KB/s 折算（MP4/TS 通用量级）
	const avgBitrate = 500 * 1024 // 500 KB/s
	if total > 0 {
		p.Duration = float64(total) / avgBitrate
		p.Elapsed = float64(pos) / avgBitrate
	} else if !start.IsZero() {
		p.Elapsed = time.Since(start).Seconds()
	}
	return p
}

// setTotal 记录上游总长度
func (s *Session) setTotal(n int64) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	s.total = n
	s.mu.Unlock()
}

// setPos 设置流位置（seek 时电视会带 Range: bytes=N-，进度跳到 N）
func (s *Session) setPos(n int64) {
	s.mu.Lock()
	s.pos = n
	s.mu.Unlock()
}

// addBytes 传输字节计数
func (s *Session) addBytes(n int64) {
	s.mu.Lock()
	s.bytes += n
	s.pos += n
	s.mu.Unlock()
}

// addUpstreamResponse 记录上游响应头信息（总长等）
func (s *Session) addUpstreamResponse(hdr http.Header) {
	if cl := hdr.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			s.setTotal(n)
		}
	}
	if cr := hdr.Get("Content-Range"); cr != "" {
		if _, total, ok := parseContentRangeTotal(cr); ok {
			s.setTotal(total)
		}
	}
}

// Handler 挂到 http.Server
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/m/", s.handle)
	return mux
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	// /m/<id> 或 /m/<id>.ts 或 /m/<id>/playlist.m3u8 或 /m/<id>/seg/<b64> 或 /m/<id>/pl/<b64>
	rest := strings.TrimPrefix(r.URL.Path, "/m/")
	id := rest
	if i := strings.Index(rest, "."); i > 0 {
		id = rest[:i]
	}
	if i := strings.Index(id, "/"); i > 0 {
		id = id[:i]
	}
	s.mu.Lock()
	ses, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "no session", http.StatusNotFound)
		return
	}
	// HLS 子路径（分片 / 嵌套播放列表）——两种模式通用
	if strings.Contains(rest, "/seg/") || strings.Contains(rest, "/pl/") {
		s.handleHLSSub(w, r, ses)
		return
	}
	switch ses.Mode {
	case ModeHLS:
		s.handleHLS(w, r, ses)
	case ModeRemux:
		s.handleRemux(w, r, ses)
	default:
		// proxy 模式遇到 HLS 上游 → 自动走播放列表重写 + 分片代理
		if isHLSURL(ses.Src.URL) || isHLSType(ses.Src.MimeType) {
			s.handleHLSTop(w, r, ses)
			return
		}
		s.handleProxy(w, r, ses)
	}
}

// isHLSURL 判断 URL 是否是 HLS 播放列表
func isHLSURL(u string) bool {
	l := strings.ToLower(u)
	return strings.Contains(l, ".m3u8") || strings.Contains(l, "mpegurl")
}

// isHLSType 判断 Content-Type 是否是 HLS
func isHLSType(t string) bool {
	return strings.Contains(strings.ToLower(t), "mpegurl")
}

// hlsSegPath 分片代理路径（base64url 编码上游绝对 URL，不会丢目录）
func hlsSegPath(sessionID, absURL string) string {
	return "/m/" + sessionID + "/seg/" + base64.RawURLEncoding.EncodeToString([]byte(absURL))
}

// hlsPlPath 嵌套播放列表代理路径（master playlist 里的 variant）
func hlsPlPath(sessionID, absURL string) string {
	return "/m/" + sessionID + "/pl/" + base64.RawURLEncoding.EncodeToString([]byte(absURL))
}

// decodeHlsPath 解码 base64url 上游绝对 URL
func decodeHlsPath(enc string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// handleHLSTop 顶层播放列表：拉上游 m3u8 并重写分片 URL
func (s *Server) handleHLSTop(w http.ResponseWriter, r *http.Request, ses *Session) {
	body, err := fetchAll(ses.Src.URL, ses.Src.Referer, ses.Src.UA)
	if err != nil {
		http.Error(w, "upstream playlist: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.writePlaylist(w, body, ses.Src.URL, ses.ID)
}

// writePlaylist 重写并输出播放列表
func (s *Server) writePlaylist(w http.ResponseWriter, body []byte, upURL, sessionID string) {
	rewritten := s.rewritePlaylist(body, upURL, sessionID)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(rewritten))
}

// handleHLSSub 处理 /m/<id>/seg/<b64>（二进制分片）和 /m/<id>/pl/<b64>（嵌套播放列表）
func (s *Server) handleHLSSub(w http.ResponseWriter, r *http.Request, ses *Session) {
	path := r.URL.Path
	if i := strings.Index(path, "/pl/"); i > 0 {
		abs, err := decodeHlsPath(path[i+4:])
		if err != nil {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		body, err := fetchAll(abs, ses.Src.Referer, ses.Src.UA)
		if err != nil {
			http.Error(w, "upstream playlist: "+err.Error(), http.StatusBadGateway)
			return
		}
		rewritten := s.rewritePlaylist(body, abs, ses.ID)
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte(rewritten))
		return
	}
	i := strings.Index(path, "/seg/")
	if i <= 0 {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	abs, err := decodeHlsPath(path[i+5:])
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	s.proxyUpstream(w, r, abs, ses)
}

// handleProxy 透明反代 + Range + 补 Referer/UA
//
// 如果上游是 HLS 播放列表（按 Content-Type 判断，兼容抖音 URL 不带
// .m3u8 后缀的情况），自动改写为分片代理模式。
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request, ses *Session) {
	if isHLSURL(ses.Src.URL) || isHLSType(ses.Src.MimeType) {
		s.handleHLSTop(w, r, ses)
		return
	}
	// DASH（.mpd）：本项目只实现了 HLS 与普通文件的代理，
	// 没有 DASH 清单重写能力。透传 mpd 给电视基本都会失败，
	// 所以明确报错，不浪费电视端的时间。
	if isDASHURL(ses.Src.URL) || isDASHType(ses.Src.MimeType) {
		http.Error(w, "暂不支持 DASH (.mpd) 格式，请改用 HLS 或普通视频",
			http.StatusNotImplemented)
		return
	}
	s.proxyUpstream(w, r, ses.Src.URL, ses)
}

// isDASHURL 判断 URL 是否是 DASH 清单
func isDASHURL(u string) bool {
	return strings.Contains(strings.ToLower(u), ".mpd")
}

// isDASHType 判断 Content-Type 是否是 DASH
func isDASHType(t string) bool {
	return strings.Contains(strings.ToLower(t), "dash+xml")
}

// proxyUpstream 反代指定上游 URL（补 Referer/UA，支持 Range）
func (s *Server) proxyUpstream(w http.ResponseWriter, r *http.Request, upstream string, ses *Session) {
	req, err := http.NewRequest(r.Method, upstream, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Range 起始偏移（电视 seek 时发 Range: bytes=N-）→ 设置流位置
	start := int64(0)
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
		if _, s, ok := parseRangeStart(rng); ok {
			start = s
		}
	}
	req.Header.Set("User-Agent", ses.Src.UA)
	if ses.Src.Referer != "" {
		req.Header.Set("Referer", ses.Src.Referer)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		log.Printf("[media] upstream %d mode=%s url=%s", resp.StatusCode, ses.Mode, upstream)
	}
	// 上游实际返回的是 HLS 播放列表（抖音等 URL 不带 .m3u8 后缀，
	// 只能靠响应 Content-Type 判断）→ 丢弃本次响应，改走播放列表重写
	if ses.Mode == ModeProxy && !strings.Contains(r.URL.Path, "/seg/") &&
		isHLSType(resp.Header.Get("Content-Type")) {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, "read playlist: "+err.Error(), http.StatusBadGateway)
			return
		}
		s.writePlaylist(w, body, upstream, ses.ID)
		return
	}
	// 记录总长度 + 起始位置（给进度条用）
	ses.addUpstreamResponse(resp.Header)
	// 只有上游真正返回 206 才认 Range 起点；200 说明上游忽略 Range（不支持 seek），从 0 开始
	if resp.StatusCode == http.StatusPartialContent {
		ses.setPos(start)
	} else {
		ses.setPos(0)
	}
	// 透传响应头
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, &countWriter{w: resp.Body, s: ses})
}

// handleHLS 播放列表模式：入口是 /m/<id>/playlist.m3u8
func (s *Server) handleHLS(w http.ResponseWriter, r *http.Request, ses *Session) {
	s.handleHLSTop(w, r, ses)
}

// handleRemux ffmpeg 重封装成 TS（修时间戳、丢迟到片）
func (s *Server) handleRemux(w http.ResponseWriter, r *http.Request, ses *Session) {
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Accept-Ranges", "none")
	fl, ok := w.(http.Flusher)
	if ok {
		fl.Flush()
	}
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-fflags", "+genpts+discardcorrupt",
		"-user_agent", ses.Src.UA,
	}
	if ses.Src.Referer != "" {
		args = append(args, "-headers", "Referer: "+ses.Src.Referer+"\r\n")
	}
	args = append(args,
		"-i", ses.Src.URL,
		"-map", "0:v:0",
		"-c:v", "copy",
		"-map", "0:a:0",
		"-c:a", "aac",
		"-b:a", "192k",
		"-f", "mpegts",
		"-mpegts_flags", "+resend_headers",
		"-copyts",
		"-muxdelay", "0",
		"-flush_packets", "1",
		"pipe:1",
	)
	cmd := exec.Command(s.ffmpeg, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-r.Context().Done():
			cmd.Process.Kill()
		case <-done:
		}
	}()
	buf := make([]byte, 64*1024)
	var n int
	var rerr error
	for {
		n, rerr = stdout.Read(buf)
		if n > 0 {
			ses.addBytes(int64(n))
			if _, werr := w.Write(buf[:n]); werr != nil {
				cmd.Process.Kill()
				break
			}
			if ok {
				fl.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	close(done)
	_ = cmd.Wait()
}

// parseRangeStart 解析 Range: bytes=100-200 → start=100
func parseRangeStart(rng string) (end, start int64, ok bool) {
	rng = strings.TrimSpace(rng)
	if !strings.HasPrefix(rng, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(rng, "bytes=")
	if i := strings.Index(spec, ","); i >= 0 {
		spec = spec[:i]
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	if parts[0] != "" {
		start, _ = strconv.ParseInt(parts[0], 10, 64)
	}
	if parts[1] != "" {
		end, _ = strconv.ParseInt(parts[1], 10, 64)
	}
	return end, start, true
}

// parseContentRangeTotal 解析 Content-Range: bytes 0-1023/4096 → total=4096
func parseContentRangeTotal(cr string) (start, total int64, ok bool) {
	if i := strings.Index(cr, "/"); i >= 0 {
		totalStr := strings.TrimSpace(cr[i+1:])
		if totalStr != "*" {
			if n, err := strconv.ParseInt(totalStr, 10, 64); err == nil {
				return 0, n, true
			}
		}
	}
	return 0, 0, false
}

func fetchAll(up, referer, ua string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, up, nil)
	if err != nil {
		return nil, err
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	req.Header.Set("User-Agent", ua)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// rewritePlaylist 把 m3u8 里的段 URI 改成走本机代理。
//
// 处理三种 URI：
//   - 普通分片行（0000000.ts）→ /m/<id>/seg/<b64>
//   - 子播放列表（master playlist 里的 variant）→ /m/<id>/pl/<b64>
//   - EXT-X-MAP（fMP4 初始化段）、EXT-X-KEY（加密密钥）→ 同样走代理
//
// 用 base64url 编码上游绝对 URL，避免相对路径丢目录（旧实现把
// https://cdn/x/y/000.ts 错当成 https://cdn/000.ts）。
func (s *Server) rewritePlaylist(body []byte, upURL, sessionID string) string {
	lines := strings.Split(string(body), "\n")
	base, _ := url.Parse(upURL)
	for i, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" {
			continue
		}
		// 带标签的行：只处理 URI 属性（EXT-X-MAP / EXT-X-KEY / EXT-X-MEDIA）
		if strings.HasPrefix(trimmed, "#") {
			lines[i] = s.rewriteTagURIs(trimmed, base, sessionID)
			continue
		}
		// 纯 URI 行：分片 或 子播放列表
		abs := resolveURL(base, trimmed)
		if abs == "" {
			continue
		}
		// 判断是不是子播放列表（以 .m3u8 结尾）
		if isHLSURL(abs) {
			lines[i] = s.baseURL + hlsPlPath(sessionID, abs)
		} else {
			lines[i] = s.baseURL + hlsSegPath(sessionID, abs)
		}
	}
	return strings.Join(lines, "\n")
}

// rewriteTagURIs 重写标签行里的 URI 属性
func (s *Server) rewriteTagURIs(line string, base *url.URL, sessionID string) string {
	// EXT-X-MAP:URI="xxx"
	if i := strings.Index(line, `URI="`); i >= 0 {
		rest := line[i+5:]
		j := strings.Index(rest, `"`)
		if j > 0 {
			abs := resolveURL(base, rest[:j])
			if abs != "" {
				return line[:i+5] + s.baseURL + hlsSegPath(sessionID, abs) + rest[j:]
			}
		}
	}
	// EXT-X-KEY:URI="xxx"  （密钥走代理）
	if strings.HasPrefix(line, "#EXT-X-KEY") {
		if i := strings.Index(line, `URI="`); i >= 0 {
			rest := line[i+5:]
			j := strings.Index(rest, `"`)
			if j > 0 {
				abs := resolveURL(base, rest[:j])
				if abs != "" {
					return line[:i+5] + s.baseURL + hlsSegPath(sessionID, abs) + rest[j:]
				}
			}
		}
	}
	return line
}

// resolveURL 把相对/绝对 URI 解析成绝对 URL
func resolveURL(base *url.URL, ref string) string {
	if base == nil {
		return ref
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	if u.IsAbs() {
		return u.String()
	}
	return base.ResolveReference(u).String()
}

type countWriter struct {
	w io.Reader
	s *Session
}

func (c *countWriter) Read(p []byte) (int, error) {
	n, err := c.w.Read(p)
	if n > 0 && c.s != nil {
		c.s.addBytes(int64(n))
	}
	return n, err
}

// SessionInfo 会话状态（给 UI）
type SessionInfo struct {
	ID     string `json:"id"`
	Mode   Mode   `json:"mode"`
	URL    string `json:"url"`
	Type   string `json:"type"`
	Title  string `json:"title"`
	Bytes  int64  `json:"bytes"`
	Active bool   `json:"active"`
}

// List 会话列表
func (s *Server) List() []SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SessionInfo, 0, len(s.sessions))
	for _, ses := range s.sessions {
		ses.mu.Lock()
		out = append(out, SessionInfo{ID: ses.ID, Mode: ses.Mode, URL: ses.URL, Type: ses.Type, Title: ses.Src.Title, Bytes: ses.bytes, Active: ses.active})
		ses.mu.Unlock()
	}
	return out
}

// Stats 字节计数
func (s *Server) SessionBytes(id string) int64 {
	s.mu.Lock()
	ses, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		return 0
	}
	ses.mu.Lock()
	defer ses.mu.Unlock()
	return ses.bytes
}

var _ = strconv.Itoa
