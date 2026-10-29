package media

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// upstream 模拟 B站 CDN 源
func upstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", "43152")
			return
		}
		// 支持 Range
		data := []byte(strings.Repeat("ABCDEFGH", 5394)) // 43152 bytes
		if rng := r.Header.Get("Range"); rng != "" {
			var start, end int
			fmt.Sscanf(rng, "bytes=%d-%d", &start, &end)
			if end == 0 || end >= len(data) {
				end = len(data) - 1
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
			w.WriteHeader(206)
			w.Write(data[start : end+1])
			return
		}
		w.Write(data)
	}))
	return srv
}

func TestProxyModeServesWithRange(t *testing.T) {
	up := upstream(t)
	defer up.Close()

	s := NewServer("http://127.0.0.1:9999")
	ses, err := s.Start(Source{URL: up.URL, MimeType: "video/mp4"}, ModeProxy)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/m/"+ses.ID, nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("proxy code=%d", rr.Code)
	}
	if len(rr.Body.Bytes()) != 43152 {
		t.Fatalf("proxy body len=%d", len(rr.Body.Bytes()))
	}

	// Range 请求
	req2 := httptest.NewRequest("GET", "/m/"+ses.ID, nil)
	req2.Header.Set("Range", "bytes=0-99")
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != 206 {
		t.Fatalf("range code=%d", rr2.Code)
	}
	if len(rr2.Body.Bytes()) != 100 {
		t.Fatalf("range body len=%d", len(rr2.Body.Bytes()))
	}
}

func TestHLSPathRewritesSegments(t *testing.T) {
	// 上游在 /video/xxx/ 目录下，分片是相对文件名
	// 旧实现会把分片解析成 /seg0.ts（丢目录）→ 404
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "playlist.m3u8") {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nseg0.ts\n#EXTINF:6,\nseg1.ts\n#EXT-X-ENDLIST\n")
			return
		}
		if strings.Contains(r.URL.Path, "/video/xxx/") {
			w.Write([]byte("SEGDATA"))
			return
		}
		http.Error(w, "segment path lost directory", http.StatusNotFound)
	}))
	defer up.Close()

	s := NewServer("http://127.0.0.1:9999")
	ses, _ := s.Start(Source{URL: up.URL + "/video/xxx/playlist.m3u8", MimeType: "application/x-mpegurl"}, ModeHLS)
	req := httptest.NewRequest("GET", "/m/"+ses.ID+"/playlist.m3u8", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	body := rr.Body.String()
	t.Logf("rewritten:\n%s", body)

	// 分片路径应指向 /seg/<b64>，且 b64 解码后是含目录的完整绝对 URL
	segPath := ""
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "http://127.0.0.1:9999/m/") && strings.Contains(ln, "/seg/") {
			segPath = ln
			break
		}
	}
	if segPath == "" {
		t.Fatalf("segment not rewritten:\n%s", body)
	}
	b64 := segPath[strings.Index(segPath, "/seg/")+len("/seg/"):]
	dec, err := decodeHlsPath(b64)
	if err != nil {
		t.Fatalf("decode b64: %v", err)
	}
	if !strings.Contains(dec, "/video/xxx/seg0.ts") {
		t.Fatalf("segment URL lost directory: %s", dec)
	}
	// 代理该分片应该 200
	req2 := httptest.NewRequest("GET", segPath, nil)
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != 200 {
		t.Fatalf("segment proxy code=%d body=%s", rr2.Code, rr2.Body.String())
	}
	if rr2.Body.String() != "SEGDATA" {
		t.Fatalf("segment body=%q", rr2.Body.String())
	}
}

// TestProxyModeAutoHandlesHLS：proxy 模式下遇到 m3u8 上游应自动重写分片
func TestProxyModeAutoHandlesHLS(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		io.WriteString(w, "#EXTM3U\n#EXTINF:6,\n0000000.ts\n#EXT-X-ENDLIST\n")
	}))
	defer up.Close()

	s := NewServer("http://127.0.0.1:9999")
	ses, _ := s.Start(Source{URL: up.URL, MimeType: "application/vnd.apple.mpegurl"}, ModeProxy)
	req := httptest.NewRequest("GET", "/m/"+ses.ID, nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	body := rr.Body.String()
	t.Logf("auto proxy playlist:\n%s", body)
	if !strings.Contains(body, "/m/"+ses.ID+"/seg/") {
		t.Fatalf("proxy mode did not auto-rewrite HLS:\n%s", body)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Fatalf("content-type=%s", ct)
	}
}

// TestRewritePlaylistHandlesMapAndKey：EXT-X-MAP / EXT-X-KEY 的 URI 也要走代理
func TestRewritePlaylistHandlesMapAndKey(t *testing.T) {
	s := NewServer("http://127.0.0.1:9999")
	pl := "#EXTM3U\n" +
		"#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n" +
		"#EXT-X-MAP:URI=\"init.mp4\"\n" +
		"#EXTINF:6,\nseg0.ts\n"
	out := s.rewritePlaylist([]byte(pl), "https://cdn.example.com/video/abc/playlist.m3u8", "SID")
	t.Logf("out:\n%s", out)
	for _, want := range []string{"key.bin", "init.mp4", "seg0.ts"} {
		if !strings.Contains(out, base64.RawURLEncoding.EncodeToString([]byte("https://cdn.example.com/video/abc/"+want))) {
			t.Fatalf("%s not proxied:\n%s", want, out)
		}
	}
}

// TestRewritePlaylistHandlesMaster：master playlist 的子列表要走 /pl/
func TestRewritePlaylistHandlesMaster(t *testing.T) {
	s := NewServer("http://127.0.0.1:9999")
	pl := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1280000\nlow/index.m3u8\n"
	out := s.rewritePlaylist([]byte(pl), "https://cdn.example.com/v/master.m3u8", "SID")
	t.Logf("out:\n%s", out)
	if !strings.Contains(out, "/pl/") {
		t.Fatalf("master variant not routed to /pl/:\n%s", out)
	}
}
