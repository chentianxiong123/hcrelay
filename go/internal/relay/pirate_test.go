package relay

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hcrelay/internal/media"
	"hcrelay/internal/renderer"
)

// TestPirateSiteRefererPassthrough 验证盗版站防盗链场景：
//
// 盗版站 CDN 会校验 Referer 必须是自己站点的域名。
// 这个域名只能从 App 投屏请求的 Referer 头里拿到，猜不出来。
// 本测试模拟完整链路：
//
//	模拟 App 带着 Referer: https://mysite.xyz/watch/xxx 发 SetAVTransportURI
//	  → renderer 捕获
//	  → relay 透传
//	  → 代理层用这个 Referer 拉上游
//	  → 上游 200
func TestPirateSiteRefererPassthrough(t *testing.T) {
	var gotRef, gotUA string

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRef = r.Header.Get("Referer")
		gotUA = r.Header.Get("User-Agent")
		// 盗版站防盗链：必须带自己站点的 Referer
		if !strings.Contains(gotRef, "mysite.xyz") {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte("403 Forbidden"))
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write([]byte("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k.bin\"\n#EXTINF:6,\nseg0.ts\n#EXT-X-ENDLIST\n"))
	}))
	defer up.Close()

	// URL 完全没有 m3u8 特征（盗版站常见）
	upstream := up.URL + "/api/v2/stream?id=998877&token=abc"

	// 渲染端（接收 App 投屏）
	rndr := renderer.New("http://127.0.0.1:9999", "uuid:test", "Relay",
		renderer.Services{AVTransport: true, RenderingCtrl: true, ConnMgr: true})
	ms := media.NewServer("http://127.0.0.1:9999")
	rl := New(rndr, ms, nil, "http://127.0.0.1:9999")

	// 模拟 App 发 SetAVTransportURI，带 Referer
	soap := `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">
<s:Body><u:SetAVTransportURI xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">
<InstanceID>0</InstanceID>
<CurrentURI>` + upstream + `</CurrentURI>
<CurrentURIMetaData></CurrentURIMetaData>
</u:SetAVTransportURI></s:Body></s:Envelope>`

	req := httptest.NewRequest("POST", "/ctl/AVTransport", strings.NewReader(soap))
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:AVTransport:1#SetAVTransportURI"`)
	req.Header.Set("Referer", "https://mysite.xyz/watch/12345.html")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 13) Chrome/120 Mobile")

	rr := httptest.NewRecorder()
	rndr.Handler().ServeHTTP(rr, req)
	fmt.Printf("[1] App 投屏请求 status=%d\n", rr.Code)

	// 等 relay 异步建会话
	var sess *media.Session
	for i := 0; i < 100; i++ {
		rl.mu.Lock()
		sess = rl.ses
		rl.mu.Unlock()
		if sess != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if sess == nil {
		t.Fatal("没有建立媒体会话")
	}
	fmt.Printf("[2] 媒体会话 Referer=%q\n", sess.Src.Referer)
	fmt.Printf("    媒体会话 UA=%.40s...\n", sess.Src.UA)
	fmt.Printf("    媒体会话 MimeType=%s\n", sess.Src.MimeType)

	if !strings.Contains(sess.Src.Referer, "mysite.xyz") {
		t.Errorf("Referer 没透传！实际=%q", sess.Src.Referer)
	}

	// 电视来拉播放列表 → 上游应该 200（用真正的 media handler，不是 9999 配置）
	msSrv := httptest.NewServer(ms.Handler())
	defer msSrv.Close()
	segURL := msSrv.URL + "/m/" + sess.ID
	resp, err := http.Get(segURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	fmt.Printf("[3] 电视拉播放列表 status=%d\n", resp.StatusCode)
	fmt.Printf("    上游实际收到 Referer=%q\n", gotRef)
	fmt.Printf("    上游实际收到 UA=%.40s...\n", gotUA)

	if resp.StatusCode != 200 {
		t.Errorf("防盗链没过，status=%d", resp.StatusCode)
	}
	// 确认播放列表重写了分片
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	if !strings.Contains(string(body[:n]), "/seg/") {
		t.Errorf("播放列表没重写分片:\n%s", body[:n])
	}
}
