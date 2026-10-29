package media

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGenericHLSFromAnySource 验证「不管来源是谁」的 HLS 都能正确代理。
// 覆盖：URL 不带 .m3u8 后缀（抖音）、带 query 参数、带 fMP4 初始化段、
// AES 加密密钥、master playlist 多码率。
func TestGenericHLSFromAnySource(t *testing.T) {
	cases := []struct {
		name string
		// 上游播放列表内容
		playlist string
		// 上游 Content-Type
		ct string
		// 上游 URL（相对于 server）
		path string
		// 期望在重写结果中出现的片段（相对于上游 origin 的路径+query）
		wantPath []string
		// 期望不出现的片段
		wantNotPath []string
	}{
		{
			name:      "抖音风格-无后缀URL",
			playlist:  "#EXTM3U\n#EXTINF:6,\nseg0.ts\n#EXT-X-ENDLIST\n",
			ct:        "application/vnd.apple.mpegurl",
			path:      "/video/abc/stream",
			wantPath:  []string{"/video/abc/seg0.ts"},
		},
		{
			name:      "带query参数",
			playlist:  "#EXTM3U\n#EXTINF:6,\nseg0.ts?sign=xxx&t=123\n#EXT-X-ENDLIST\n",
			ct:        "application/x-mpegURL",
			path:      "/v/index.m3u8",
			wantPath:  []string{"/v/seg0.ts?sign=xxx&t=123"},
		},
		{
			name:      "fMP4-有EXT-X-MAP",
			playlist:  "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6,\nseg0.m4s\n#EXT-X-ENDLIST\n",
			ct:        "application/vnd.apple.mpegurl",
			path:      "/v/index.m3u8",
			wantPath:  []string{"/v/init.mp4", "/v/seg0.m4s"},
		},
		{
			name:      "AES加密-有EXT-X-KEY",
			playlist:  "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k.bin\"\n#EXTINF:6,\nseg0.ts\n#EXT-X-ENDLIST\n",
			ct:        "application/vnd.apple.mpegurl",
			path:      "/v/index.m3u8",
			wantPath:  []string{"/v/k.bin", "/v/seg0.ts"},
		},
		{
			name:      "master-多码率",
			playlist:  "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\n720/index.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=2000\n1080/index.m3u8\n",
			ct:        "application/vnd.apple.mpegurl",
			path:      "/v/master.m3u8",
			wantPath:  []string{"/v/720/index.m3u8", "/v/1080/index.m3u8"},
		},
		{
			name:      "绝对路径分片-不丢目录",
			playlist:  "#EXTM3U\n#EXTINF:6,\n/other/dir/seg0.ts\n#EXT-X-ENDLIST\n",
			ct:        "application/vnd.apple.mpegurl",
			path:      "/v/index.m3u8",
			wantPath:  []string{"/other/dir/seg0.ts"},
			wantNotPath: []string{"/seg0.ts"},
		},
		{
			name:      "完整绝对URL分片",
			playlist:  "#EXTM3U\n#EXTINF:6,\nhttps://other.cdn/x/seg0.ts\n#EXT-X-ENDLIST\n",
			ct:        "application/vnd.apple.mpegurl",
			path:      "/v/index.m3u8",
			wantPath:  []string{"https://other.cdn/x/seg0.ts"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.ct)
				w.Write([]byte(tc.playlist))
			}))
			defer up.Close()

			s := NewServer("http://127.0.0.1:9999")
			// 关键：URL 不一定带 .m3u8 后缀
			ses, err := s.Start(Source{
				URL:      up.URL + tc.path,
				MimeType: "video/mp4", // 猜不出来，交给内容类型判断
			}, ModeProxy)
			if err != nil {
				t.Fatalf("start: %v", err)
			}

			req := httptest.NewRequest("GET", "/m/"+ses.ID, nil)
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			if rr.Code != 200 {
				t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
			}
			body := rr.Body.String()
			t.Logf("rewritten:\n%s", body)

			// 收集所有被代理的绝对 URL
			got := map[string]bool{}
			for _, ln := range strings.Split(body, "\n") {
				for _, marker := range []string{"/seg/", "/pl/"} {
					i := strings.Index(ln, marker)
					if i < 0 {
						continue
					}
					enc := strings.TrimSpace(ln[i+len(marker):])
					enc = strings.Trim(enc, "\"")
					if dec, err := decodeHlsPath(enc); err == nil {
						got[dec] = true
					}
				}
			}
			// 期望值：相对 path 拼上上游 origin；绝对 URL 直接用
			for _, w := range tc.wantPath {
				full := w
				if !strings.HasPrefix(w, "http") {
					full = up.URL + w
				}
				if !got[full] {
					t.Errorf("缺少代理目标 %s\n实际: %v", full, got)
				}
			}
			for _, w := range tc.wantNotPath {
				full := w
				if !strings.HasPrefix(w, "http") {
					full = up.URL + w
				}
				if got[full] {
					t.Errorf("不该出现的代理目标 %s", full)
				}
			}
		})
	}
}

// TestHLSProxyServesRealBytes 验证代理分片能把上游真实字节传回来
func TestHLSProxyServesRealBytes(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, ".m3u8") {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte("#EXTM3U\n#EXTINF:6,\nseg0.ts\n#EXT-X-ENDLIST\n"))
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write([]byte{0x47, 0x40, 0x11, 0x10, 0xAA, 0xBB})
	}))
	defer up.Close()

	s := NewServer("http://127.0.0.1:9999")
	ses, _ := s.Start(Source{URL: up.URL + "/playlist.m3u8"}, ModeProxy)

	// 播放列表
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/m/"+ses.ID, nil))
	var segURL string
	for _, ln := range strings.Split(rr.Body.String(), "\n") {
		if strings.Contains(ln, "/seg/") {
			segURL = strings.TrimSpace(ln)
			break
		}
	}
	if segURL == "" {
		t.Fatal("没有分片 URL")
	}
	// 分片
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, httptest.NewRequest("GET", segURL, nil))
	if rr2.Code != 200 {
		t.Fatalf("code=%d", rr2.Code)
	}
	if rr2.Body.Len() != 6 {
		t.Fatalf("len=%d", rr2.Body.Len())
	}
	if rr2.Header().Get("Content-Type") != "video/mp2t" {
		t.Fatalf("ct=%s", rr2.Header().Get("Content-Type"))
	}
}

// TestSegmentPathKeepsDirectory 回归测试：分片必须保留完整目录
func TestSegmentPathKeepsDirectory(t *testing.T) {
	s := NewServer("http://127.0.0.1:9999")
	out := s.rewritePlaylist([]byte("#EXTM3U\n#EXTINF:6,\n0000000.ts\n"),
		"https://s3.bfllvip.com/video/jijieyueyu/b2cea4957d6e/index.m3u8", "SID")
	want := "https://s3.bfllvip.com/video/jijieyueyu/b2cea4957d6e/0000000.ts"
	enc := base64.RawURLEncoding.EncodeToString([]byte(want))
	if !strings.Contains(out, enc) {
		t.Fatalf("目录丢失:\n%s", out)
	}
}
