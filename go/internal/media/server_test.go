package media

import (
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
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "playlist.m3u8") {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nseg0.ts\n#EXTINF:6,\nseg1.ts\n#EXT-X-ENDLIST\n")
			return
		}
		w.Write([]byte("SEGDATA"))
	}))
	defer up.Close()

	s := NewServer("http://127.0.0.1:9999")
	ses, _ := s.Start(Source{URL: up.URL + "/playlist.m3u8", MimeType: "application/x-mpegurl"}, ModeHLS)
	req := httptest.NewRequest("GET", "/m/"+ses.ID+"/playlist.m3u8", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, "/m/"+ses.ID+"/seg/seg0.ts") {
		t.Fatalf("segment not rewritten:\n%s", body)
	}
	t.Logf("rewritten:\n%s", body)
}
