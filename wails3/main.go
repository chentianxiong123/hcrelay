// HomeCast Relay 统一壳（wails3 v3）：一个入口，三端构建
//
//	🐧 Linux / 🪟 Windows / 🤖 安卓 共用：
//	  - 内嵌 hcrelay 后端（server.New() handler，模板/静态/API 全在内）
//	  - wails WebView 窗口直载 htmx 界面：URL 指向后端真实 HTTP 端口（同源，非 wails asset 桥）
//	  - 桌面端：后端监听 0.0.0.0（局域网 SSDP/DLNA 可见）
//	  - 安卓端：后端监听 127.0.0.1（本机 WebView 访问）+ 挂件 no-op
//	平台差异全部走同目录 build tag 文件：
//	  ensurehome_android.go / ensurehome_other.go   —— 数据目录（安卓 filesDir vs 桌面 ~/.config）
//	  http_desktop.go / http_android.go             —— 后端监听地址（0.0.0.0 vs 127.0.0.1）
//	  widget_desktop.go / widget_android.go         —— 挂件启动（no-op）
package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"hcrelay/internal/server"
)

// httpLog 把每个请求的状态/panic 写进文件（安卓 stderr 不可见，文件可取证）
var httpLog string

var app *application.App

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func logRequest(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: 200}
		start := time.Now()
		defer func() {
			var msg string
			if p := recover(); p != nil {
				msg = fmt.Sprintf("PANIC %s %s -> %v\n%s\n", r.Method, r.URL.Path, p, debug.Stack())
				sw.WriteHeader(500)
			} else {
				msg = fmt.Sprintf("%s %s -> %d (%s)\n", r.Method, r.URL.Path, sw.status, time.Since(start))
			}
			f, err := os.OpenFile(httpLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if err == nil {
				f.WriteString(msg)
				f.Close()
			}
		}()
		h.ServeHTTP(sw, r)
	})
}

// backendPort 独立 HTTP 端口（WebView 访问）；HC_PORT 覆盖
func backendPort() string {
	if p := os.Getenv("HC_PORT"); p != "" {
		return p
	}
	return server.Port
}

func main() {
	ensureHome() // 必须在 main()（nativeInit 之后，JNI bridge 可用时）调用
	httpLog = filepath.Join(hcDataPath(), "hcrelay-http.log")
	os.WriteFile(httpLog, []byte("HOME="+os.Getenv("HOME")+"\n"), 0644)

	inst := server.New(server.Options{
		Port: backendPort(),
		// 安卓：对外 IP 用本机（电视要访问需走 127.0.0.1 不通，需用户在 UI 里看到真实 IP）
		// 桌面：自动取局域网 IP
	})

	// 桌面端：0.0.0.0 让局域网 SSDP/DLNA 可见；安卓 no-op（127.0.0.1 本机）
	go listenBackend(inst.Handler)

	app = application.New(application.Options{
		Name:        "HomeCast Relay",
		Description: "B站投屏中继（手机当中间件）",
	})

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "HomeCast 投屏中继",
		Width:            900,
		Height:           640,
		MinWidth:         380,
		MinHeight:        520,
		BackgroundColour: application.NewRGB(15, 17, 21),
		URL:              "http://127.0.0.1:" + backendPort() + "/ui",
	})

	if err := app.Run(); err != nil {
		os.Exit(1)
	}
}
