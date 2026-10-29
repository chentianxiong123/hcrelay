// Package server 组装 hcrelay 全部后端 handler（HTTP 服务）。
// 被 cmd/hcrelay（独立进程）和 wails3 壳（安卓/桌面内嵌）复用。
package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"time"

	"hcrelay/internal/bili"
	"hcrelay/internal/media"
	"hcrelay/internal/nva"
	"hcrelay/internal/relay"
	"hcrelay/internal/renderer"
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
	ad      *ssdp.Advertiser
	nvaLn   net.Listener
	nvaSrv  *nva.Server
	stopFn  func()
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
		uuid = "uuid:00000000-0000-0000-0000-00000000relay"
	}
	name := opts.Name
	if name == "" {
		name = "HomeCast Relay"
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
	mux.HandleFunc("/api/mode", func(w http.ResponseWriter, rq *http.Request) {
		rl.SetMode(media.Mode(rq.URL.Query().Get("mode")))
		writeJSON(w, map[string]any{"ok": true})
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

	inst.Handler = mux
	return inst
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
