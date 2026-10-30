// Package server 组装 hcrelay 全部后端 handler（HTTP 服务）。
// 被 cmd/hcrelay（独立进程）和 wails3 壳（安卓/桌面内嵌）复用。
package server

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"hcrelay/internal/bili"
	"hcrelay/internal/media"
	"hcrelay/internal/nva"
	"hcrelay/internal/relay"
	"hcrelay/internal/renderer"
	"hcrelay/internal/search"
	"hcrelay/internal/sniff"
	"hcrelay/internal/ssdp"
)

//go:embed web/templates/*.html
var tplFS embed.FS

// Port 默认端口
const Port = "8080"

// Instance 一次完整装配（服务端 + 后台任务），可 Stop
type Instance struct {
	Handler http.Handler
	BaseURL string
	Relay   *relay.Relay
	Sniff   *sniff.Extractor
	Search  *search.Aggregator
	ad      *ssdp.Advertiser
	nvaLn   net.Listener
	nvaSrv  *nva.Server
	stopFn  func()

	// WebView 兜底回传结果（JS 拿 outerHTML → POST /api/sniff/render → 这里存）
	sniffMu     sync.Mutex
	sniffResult []sniff.Episode
	sniffURL    string
}

// Options 装配选项
type Options struct {
	Port      string // 监听端口（用于拼 URL）
	IP        string // 对外 IP（电视能访问到的地址；空=自动）
	AccessKey string // B站 access_key（可选）
	UUID      string
	Name      string
}

// New 组装全部 handler 并启动后台服务（SSDP 广播 + NVA TCP）。
// 调用方负责把返回的 Handler 挂到 http.Server 上。
func New(opts Options) *Instance {
	port := opts.Port
	if port == "" {
		port = Port
	}
	ip := opts.IP
	if ip == "" {
		ip = ssdp.LocalIP()
	}
	uuid := opts.UUID
	if uuid == "" {
		uuid = newUUID()
	}
	name := opts.Name
	if name == "" {
		name = "MyCast"
	}

	baseURL := fmt.Sprintf("http://%s:%s", ip, port)

	// 渲染端（接收 B站 投屏）
	r := renderer.New(baseURL, uuid, name, renderer.Services{
		AVTransport:   true,
		RenderingCtrl: true,
		ConnMgr:       true,
		BiliNirvana:   true,
	})

	// 媒体服务器（本地重封装喂电视）
	ms := media.NewServer(baseURL)

	// B站 playurl 客户端
	b := bili.New(opts.AccessKey)

	// 编排器
	rl := relay.New(r, ms, b, baseURL)

	// SSDP 广播自己（让 B站 App 搜到）
	ad := ssdp.NewAdvertiser(uuid, name, baseURL+"/description.xml",
		"urn:schemas-upnp-org:service:NirvanaControl:3")
	if err := ad.Start(); err != nil {
		log.Printf("[ssdp] advertise failed: %v", err)
	}

	// NVA（哔哩必连）TCP 服务，端口 9958
	nvaSrv := nva.NewServer(func(cmd nva.Command) (map[string]any, error) {
		return rl.HandleNVACmd(cmd.Name, cmd.Args)
	})
	nvaLn, err := net.Listen("tcp", ":9958")
	if err == nil {
		go nvaSrv.Serve(nvaLn)
		log.Printf("[nva] listening on :9958")
	} else {
		log.Printf("[nva] port busy, skip: %v", err)
	}

	inst := &Instance{
		BaseURL: baseURL,
		Relay:   rl,
		Sniff:   sniff.New(sniff.Default()),
		Search:  search.New(search.Default()),
		ad:      ad,
		nvaLn:   nvaLn,
		nvaSrv:  nvaSrv,
	}

	mux := http.NewServeMux()
	mux.Handle("/", r.Handler())
	mux.Handle("/m/", ms.Handler())
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/play", func(w http.ResponseWriter, rq *http.Request) {
		action := rq.URL.Query().Get("action")
		arg := rq.URL.Query().Get("arg")
		if action == "" {
			writeJSON(w, map[string]any{"ok": false, "msg": "缺少 action"})
			return
		}
		if err := rl.Control(action, arg); err != nil {
			writeJSON(w, map[string]any{"ok": false, "msg": err.Error()})
			return
		}
		rl.Logf("控制: %s %s", action, arg)
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("/api/play/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "play": rl.PlayStatus()})
	})
	mux.HandleFunc("/api/debug", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"sessions": ms.Debug()})
	})
	// 盗版站/通用网页嗅探：输入详情页 URL → 集数列表
	mux.HandleFunc("/api/sniff", func(w http.ResponseWriter, rq *http.Request) {
		u := rq.URL.Query().Get("url")
		if u == "" {
			writeJSON(w, map[string]any{"ok": false, "msg": "缺少 url"})
			return
		}
		// 防盗链：从请求头透传 Referer/UA（盗版站不能猜）
		cfg := sniff.Default()
		if ref := rq.Header.Get("Referer"); ref != "" {
			cfg.Referer = ref
		}
		e := sniff.New(cfg)
		eps, err := e.ExtractURL(u)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "msg": err.Error(), "need_browser": errors.Is(err, sniff.ErrNeedsBrowser)})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "url": u, "episodes": eps})
	})

	// WebView 兜底回传：JS 拿 outerHTML（挑战完成后）→ POST 这里 → 解析存结果
	mux.HandleFunc("/api/sniff/render", func(w http.ResponseWriter, rq *http.Request) {
		if rq.Method != http.MethodPost {
			writeJSON(w, map[string]any{"ok": false, "msg": "需要 POST"})
			return
		}
		rq.ParseForm()
		htmlText := rq.PostFormValue("html")
		u := rq.PostFormValue("url")
		if htmlText == "" {
			writeJSON(w, map[string]any{"ok": false, "msg": "缺少 html"})
			return
		}
		eps := inst.Sniff.Extract(htmlText, u)
		inst.sniffMu.Lock()
		inst.sniffResult = eps
		inst.sniffURL = u
		inst.sniffMu.Unlock()
		if len(eps) == 0 {
			writeJSON(w, map[string]any{"ok": true, "episodes": 0, "msg": "no episodes found"})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "episodes": len(eps)})
	})

	// 前端轮询拿 WebView 兜底结果（读后清空）
	mux.HandleFunc("/api/sniff/result", func(w http.ResponseWriter, _ *http.Request) {
		inst.sniffMu.Lock()
		eps := inst.sniffResult
		u := inst.sniffURL
		inst.sniffResult = nil
		inst.sniffURL = ""
		inst.sniffMu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "url": u, "episodes": eps})
	})

	// 搜索盗版站（bing/360/百度 聚合，接 /api/sniff 提取集数）
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, rq *http.Request) {
		q := strings.TrimSpace(rq.URL.Query().Get("q"))
		if q == "" {
			writeJSON(w, map[string]any{"ok": false, "msg": "缺少关键词"})
			return
		}
		rs, err := inst.Search.Search(rq.Context(), q)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "msg": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "query": q, "results": rs})
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"base_url": baseURL,
			"status":   rl.Status(),
			"tvs":      tvList(),
			"logs":     rl.GetLogs(),
		})
	})
	mux.HandleFunc("/api/tv/select", func(w http.ResponseWriter, rq *http.Request) {
		name := rq.URL.Query().Get("name")
		for _, d := range tvList() {
			if d.Name == name {
				rl.SelectTV(d)
				writeJSON(w, map[string]any{"ok": true, "tv": d.Name})
				return
			}
		}
		writeJSON(w, map[string]any{"ok": false, "msg": "设备已离线，重新扫描"})
	})
	// 模式对普通用户不可见。后端只认 auto（自动识别），
	// 忽略其它值，避免误切到 remux（手机上没有 ffmpeg）等内部选项。
	mux.HandleFunc("/api/mode", func(w http.ResponseWriter, rq *http.Request) {
		rl.SetMode(media.ModeAuto)
		writeJSON(w, map[string]any{"ok": true, "mode": "auto"})
	})
	mux.HandleFunc("/api/tv/scan", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "tvs": tvList()})
	})

	tpl := template.Must(template.ParseFS(tplFS, "web/templates/*.html"))
	mux.HandleFunc("/ui", func(w http.ResponseWriter, _ *http.Request) {
		data := map[string]any{
			"BaseURL": baseURL,
			"Status":  rl.Status(),
			"TVs":     tvList(),
			"Logs":    rl.GetLogs(),
		}
		_ = tpl.ExecuteTemplate(w, "index.html", data)
	})

	inst.Handler = logRequests(mux)
	return inst
}

// logRequests 给每个 HTTP 请求打一行访问日志（诊断 DLNA 客户端行为用）
// 跳过 UI 自己的高频轮询，避免刷屏
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
		p := rq.URL.Path
		quiet := p == "/ui" || p == "/favicon.ico" || strings.HasPrefix(p, "/api/")
		if quiet {
			next.ServeHTTP(w, rq)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, rq)
		log.Printf("[http] %s %s from %s ua=%q -> %d",
			rq.Method, rq.URL.RequestURI(), rq.RemoteAddr, rq.UserAgent(), rec.code)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

// newUUID 生成合法的 RFC4122 v4 UUID（部分 DLNA 客户端会校验 USN 里的 UUID 格式）
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 极少概率失败，退化为时间戳派生
		n := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(n >> (uint(i%8) * 8))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC4122
	return fmt.Sprintf("uuid:%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Stop 停后台服务
func (inst *Instance) Stop() {
	if inst.ad != nil {
		inst.ad.Stop()
	}
	if inst.nvaLn != nil {
		inst.nvaLn.Close()
	}
}

// tvList 扫描电视（每 5 秒缓存）
var lastScan time.Time
var cached []*ssdp.Device

func tvList() []*ssdp.Device {
	if time.Since(lastScan) < 5*time.Second && cached != nil {
		return cached
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cached = ssdp.Search(ctx, 5*time.Second)
	lastScan = time.Now()
	return cached
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
