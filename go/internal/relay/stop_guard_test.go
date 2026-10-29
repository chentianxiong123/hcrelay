package relay

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"hcrelay/internal/media"
	"hcrelay/internal/renderer"
	"hcrelay/internal/ssdp"
)

// TestLateStopDoesNotKillNewSession 验证：B站 切换视频时迟到的 Stop
// 不会误杀刚建立、正在播放的新会话。
//
// 复现：B站 切换时先发 Stop 再发 Play，但 Stop 乱序晚到，在新会话
// 建立几秒内到达 → 旧代码 stopRelay() 无条件杀当前会话 → 电视拉流 404。
func TestLateStopDoesNotKillNewSession(t *testing.T) {
	// 上游 mock（返回 MP4 字节，让探测通过）
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Write([]byte{0, 0, 0, 0})
	}))
	defer upSrv.Close()

	fake := &fakeTV{}
	tvSrv := httptest.NewServer(fake.handler())
	defer tvSrv.Close()

	rndr := renderer.New("http://127.0.0.1:9999", "uuid:test", "Relay",
		renderer.Services{AVTransport: true, RenderingCtrl: true, ConnMgr: true})
	ms := media.NewServer("http://127.0.0.1:9999")
	rl := New(rndr, ms, nil, "http://127.0.0.1:9999")
	rl.SelectTV(&ssdp.Device{Name: "FakeTV", Location: tvSrv.URL + "/description.xml"})

	// 建立新会话
	rl.startRelay(upSrv.URL+"/video.mp4", "", 0, "", "")
	if rl.ses == nil {
		t.Fatal("会话没建立")
	}
	sessID := rl.ses.ID

	// 模拟 B站 迟到的 Stop（App 发来，会话刚建立 0ms）→ 应被忽略
	rl.stopRelay(true)

	rl.mu.Lock()
	s := rl.ses
	rl.mu.Unlock()
	if s == nil {
		t.Fatal("迟到的 App Stop 误杀了新会话！会话应保留")
	}
	if s.ID != sessID {
		t.Fatalf("会话被替换了: %s != %s", s.ID, sessID)
	}
}

// TestUIManualStopAlwaysWorks 验证：用户手动点 UI 停止（fromApp=false）
// 即使会话刚建立也应生效。
func TestUIManualStopAlwaysWorks(t *testing.T) {
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Write([]byte{0, 0, 0, 0})
	}))
	defer upSrv.Close()

	fake := &fakeTV{}
	tvSrv := httptest.NewServer(fake.handler())
	defer tvSrv.Close()

	rndr := renderer.New("http://127.0.0.1:9999", "uuid:test", "Relay",
		renderer.Services{AVTransport: true, RenderingCtrl: true, ConnMgr: true})
	ms := media.NewServer("http://127.0.0.1:9999")
	rl := New(rndr, ms, nil, "http://127.0.0.1:9999")
	rl.SelectTV(&ssdp.Device{Name: "FakeTV", Location: tvSrv.URL + "/description.xml"})

	rl.startRelay(upSrv.URL+"/video.mp4", "", 0, "", "")
	if rl.ses == nil {
		t.Fatal("会话没建立")
	}

	// UI 手动停止 → 应立即生效
	rl.stopRelay(false)
	rl.mu.Lock()
	s := rl.ses
	rl.mu.Unlock()
	if s != nil {
		t.Fatal("UI 手动停止没生效，会话还在")
	}
}

// TestStopGuardWindowElapsed 验证：会话建立超过保护窗口后，
// App 的 Stop 应正常生效（不是永久忽略）。
func TestStopGuardWindowElapsed(t *testing.T) {
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Write([]byte{0, 0, 0, 0})
	}))
	defer upSrv.Close()

	rndr := renderer.New("http://127.0.0.1:9999", "uuid:test", "Relay",
		renderer.Services{AVTransport: true, RenderingCtrl: true, ConnMgr: true})
	ms := media.NewServer("http://127.0.0.1:9999")
	rl := New(rndr, ms, nil, "http://127.0.0.1:9999")

	rl.startRelay(upSrv.URL+"/video.mp4", "", 0, "", "")
	if rl.ses == nil {
		t.Fatal("会话没建立")
	}

	// 假装会话已建立超过保护窗口
	rl.mu.Lock()
	rl.startedAt = time.Now().Add(-stopGuardWindow - time.Second)
	rl.mu.Unlock()

	// App Stop 超过窗口 → 应生效
	rl.stopRelay(true)
	rl.mu.Lock()
	s := rl.ses
	rl.mu.Unlock()
	if s != nil {
		t.Fatal("超过保护窗口的 App Stop 没生效")
	}
}
