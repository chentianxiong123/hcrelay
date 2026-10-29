package media

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// TestRemuxProducesTS 用真实 mp4 验证 remux 模式产出可被 TV 播放的 TS 流
func TestRemuxProducesTS(t *testing.T) {
	// 需要一个真实 mp4
	if _, err := os.Stat("/tmp/testreal.mp4"); err != nil {
		t.Skip("no /tmp/testreal.mp4, run ffmpeg first")
	}
	mp4, _ := os.ReadFile("/tmp/testreal.mp4")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(mp4)
	}))
	defer up.Close()

	s := NewServer("http://127.0.0.1:9999")
	ses, err := s.Start(Source{URL: up.URL, MimeType: "video/mp4", Referer: "https://www.bilibili.com/", UA: "test"}, ModeRemux)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/m/"+ses.ID+".ts", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("remux code=%d", rr.Code)
	}
	body := rr.Body.Bytes()
	if len(body) < 1000 {
		t.Fatalf("remux produced too little data: %d", len(body))
	}
	// TS 必须以 0x47 开头（sync byte）
	if body[0] != 0x47 {
		t.Fatalf("not MPEG-TS, first byte=0x%02x", body[0])
	}
	t.Logf("remux produced %d bytes of valid MPEG-TS", len(body))
}

var _ = io.Discard
